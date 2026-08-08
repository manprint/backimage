# Fase 06 — Binario auto-estraente

**Obiettivo**: il requisito centrale posto dall'utente — **chi fa `docker pull` deve poter recuperare i dati senza avere backimage**. Il container deve essere autosufficiente.

**Vincoli**: base image `scratch` (nessuna shell, nessuna libc), binario statico, budget **≤ 8 MB**, import limitati (vedi `overview.md` §6).

**Riferimento decisioni**: D02, D03, D04, D06, D07, D10.

---

## 06.1 Scheletro `cmd/backimage-selfextract`

**Agente: Sonnet**

### File: `cmd/backimage-selfextract/main.go`

**Vietato cobra.** Usare `flag` della stdlib con un dispatch manuale sui sottocomandi. Motivazione: budget di dimensione. Struttura prescritta:

```go
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(exitCode(err))
	}
}

func run(args []string) error {
	if len(args) == 0 {
		args = []string{"info"}
	}
	switch args[0] {
	case "info":    return cmdInfo(args[1:])
	case "list":    return cmdList(args[1:])
	case "tar":     return cmdTar(args[1:])
	case "extract": return cmdExtract(args[1:])
	case "verify":  return cmdVerify(args[1:])
	case "help", "-h", "--help": return usage(os.Stdout)
	default: return usageError("unknown command %q", args[0])
	}
}
```

### Costanti di percorso

```go
const (
	rootDir       = "/backup"
	manifestPath  = "/backup/manifest.json"
	chunksPath    = "/backup/chunks.json"
	keysAgePath   = "/backup/keys.age"
	keysPassPath  = "/backup/keys.pass.age"
	dataDir       = "/backup/data"
)
```

Aggiungere il flag globale `--root` (default `/backup`) per poter testare il binario **fuori** dal container, su una directory qualsiasi. Senza questo flag i test unitari sarebbero impossibili: è obbligatorio.

### Codici di uscita
Gli stessi della CLI principale (`overview.md` §9): 0, 1, 2, 3, 4, 5. Il pacchetto `internal/cli` **non** è importabile (porta cobra): duplicare le 7 costanti in `cmd/backimage-selfextract/exit.go`, con un commento che rimanda a `internal/cli/errors.go` e un test che verifica che i due elenchi coincidano (test nel pacchetto principale, che può importare entrambi).

### Testo di `usage`

```
backimage self-extracting backup

Usage:
  docker run --rm IMAGE [command] [flags]

Commands:
  info                 show public backup metadata (default, no passphrase needed)
  list                 list archived files
  tar                  write the plaintext tar archive to stdout
  extract              extract files to a directory
  verify               check the integrity of every blob
  help                 show this text

Common flags:
  --passphrase-stdin   read the passphrase from stdin
  --passphrase-file F  read the passphrase from file F
  --identity F         age private key file
  --root DIR           backup directory inside the image (default /backup)
  --json               machine-readable output

The passphrase is also read from $BACKIMAGE_PASSPHRASE, or prompted on the
terminal when the container is run with -it.

Examples:
  docker run --rm IMAGE
  docker run --rm -it IMAGE list
  docker run --rm -i  IMAGE tar > backup.tar
  docker run --rm -it -v "$PWD:/restore" IMAGE extract --out /restore
```

### Definition of Done
- [ ] `go build ./cmd/backimage-selfextract` compila
- [ ] `--help` mostra il testo prescritto
- [ ] `scripts/check-deps.sh` conferma l'assenza di cobra/ggcr/quic-go/protobuf

---

## 06.2 `info`

**Agente: Sonnet**

Legge **solo** `manifest.json`. **Non chiede la passphrase.**

### Output umano

```
backimage backup
  creato        2026-08-08 18:34:12 UTC
  strumento     backimage 0.1.0
  sorgenti      /home/fabio/myfiles
  host          ws01 (linux/amd64)
  contenuto     12 843 file, 1 204 directory, 47,2 GiB
  archivio      tar-pax, zstd livello 2 → 20,0 GiB
  cifratura     attiva (aes-256-gcm, passphrase)
  chunk         12 032 da 4,0 MiB in 48 layer

Per estrarre:
  docker run --rm -i IMAGE tar > backup.tar
  docker run --rm -it -v "$PWD:/restore" IMAGE extract --out /restore
```

Con `--json`: il contenuto di `manifest.json` così com'è.

### Prescrizioni
- Se `manifest.json` manca → errore *"questa immagine non è un backup backimage"*, exit 1.
- Le tre righe finali di suggerimento vanno su **stdout** nell'output umano (fanno parte del risultato) e sono assenti in `--json`.

### Test
- `info --root testdata/backup1` produce l'output atteso (golden file);
- manifest mancante → errore;
- manifest con schema 99 → errore che invita ad aggiornare.

---

## 06.3 `list`

**Agente: Sonnet**

Legge `manifest.json`, risolve la passphrase, apre `keys*.age`, decifra `index.json.zst.age`, elenca.

### Flag
`--long, -l` (mode, owner, size, mtime), `--include`, `--exclude`, `--json`.

### Output

```
drwxr-xr-x  fabio:fabio       -  2026-08-01 10:00  myfiles/
-rw-r--r--  fabio:fabio   1,2 K  2026-08-01 10:00  myfiles/a.txt
lrwxrwxrwx  fabio:fabio       -  2026-08-01 10:00  myfiles/link -> a.txt
```

Senza `-l`, solo i path, uno per riga (adatto a `| grep`).

### Prescrizioni
- **L'elenco va su stdout**, tutto il resto su stderr, incluso il prompt della passphrase (che comunque va su `/dev/tty`).
- Se la cifratura non è attiva, nessuna passphrase viene chiesta.
- Con milioni di voci, stampare in streaming, senza accumulare.

### Test
- backup cifrato: senza passphrase → exit 4; con passphrase corretta → elenco corretto;
- `--include`/`--exclude`;
- `-l` con file setuid, symlink, hardlink, device: la colonna dei permessi è corretta;
- `--json` produce un array di oggetti.

---

## 06.4 `tar` — il comando canonico

**Agente: Sonnet**

Ricostruisce il flusso tar in chiaro e lo scrive su **stdout**.

### Algoritmo
```
1. leggi manifest.json e chunks.json
2. risolvi la passphrase / identità e apri keys*.age  (salvo backup non cifrato)
3. per ogni chunk in ordine di indice:
     a. leggi /backup/data/NNNNNN.blob
     b. Open (decifra, verifica il tag GCM)
     c. decomprimi
     d. verifica lo SHA-256 del plaintext contro chunks.json  ← obbligatorio
     e. scrivi su stdout
4. exit 0
```

### Prescrizioni (vincolanti)
- **Nessun byte di diagnostica su stdout.** Mai. È la ragione per cui `internal/cli` impone la regola: qui un solo `fmt.Println` di troppo corrompe l'archivio dell'utente.
- Il progresso va su stderr solo se stderr è un TTY; altrimenti niente.
- Se stdout è un **terminale**, rifiutare con: *"tar scrive dati binari: reindirizza l'output, es. `docker run --rm -i IMAGE tar > backup.tar`"*, exit 2. Questa guardia evita che l'utente si trovi il terminale pieno di binario.
- Verifica del digest per chunk (passo 3d): un mismatch è `ErrIntegrity`, exit 5, con il numero del chunk nel messaggio.
- Buffer riusati: consumo di memoria costante, indipendente dalla dimensione del backup. Test obbligatorio.
- `--no-verify` per saltare 3d in caso di emergenza (dati parzialmente corrotti da recuperare comunque). Documentato come "ultima risorsa".

### Test
- `tar --root testdata/backup1 > out.tar` seguito da `tar tf out.tar` → elenco corretto;
- confronto byte a byte con il tar originale della fase 01;
- blob corrotto → exit 5, e stdout contiene **solo** i byte validi prodotti prima dell'errore (documentare che l'output parziale è possibile);
- stdout su TTY → exit 2 (test con pty finto o con un flag di iniezione);
- RSS costante su un backup da 1 GiB.

---

## 06.5 `extract`

**Agente: Sonnet**

Come `tar`, ma il flusso va in `pkg/archive.Extractor` (fase 01).

### Flag
`--out DIR` (obbligatorio), `--include`, `--exclude`, `--no-preserve-owner`, `--overwrite`, `--strip-components N`, `--json`.

### Prescrizioni
- **Restore parziale efficiente**: con `--include`, usare `index.Locator.ChunksFor` per leggere solo i blob necessari. Con backup da 50 GB e un file da recuperare, deve leggere pochi MB.
- Dentro il container si è root: chown, mknod e setxattr funzionano. Su bind mount di **Docker Desktop macOS/Windows** però ownership e xattr non arrivano all'host: rilevarlo (tentare un `Lchown` di prova sul target e verificare con `Lstat`) e stampare **un avviso una sola volta** su stderr:
  > *attenzione: il filesystem di destinazione non conserva ownership/xattr (tipico dei bind mount di Docker Desktop). Per un ripristino fedele usa: `docker run --rm -i IMAGE tar > backup.tar` e poi `sudo tar xpf backup.tar --xattrs --acls --numeric-owner`.*
- `--out` inesistente → creare con mode 0755; `--out` non vuoto senza `--overwrite` → errore.

### Test
- estrazione integrale in una directory temporanea, confronto con `fixtures.CompareTrees` → zero differenze (test Linux con root, dentro il container in e2e);
- `--include` su un solo file di un backup da 100 chunk → meno di 3 blob letti (contatore sull'accesso ai file);
- rilevamento del filesystem che non supporta chown → l'avviso compare una sola volta;
- `--strip-components 1`.

---

## 06.6 `verify`

**Agente: Sonnet**

Verifica **tutti** i blob: header, autenticazione GCM, digest del plaintext, e coerenza fra `chunks.json` e i file presenti.

### Output

```
verifica di 12 032 chunk…
  chunk         12 032/12 032 ok
  dimensioni    corrispondono al manifest
  indice        decifrato e coerente (12 843 voci)
ok: il backup è integro
```

Con `--json`: `{"chunks":12032,"ok":true,"errors":[]}`.

### Prescrizioni
- Senza passphrase può comunque verificare: presenza dei file, dimensioni, digest **del blob memorizzato** (`ss` in `chunks.json`). Il digest del plaintext e l'indice richiedono la passphrase. Comportamento: verifica ciò che può, e dice esplicitamente cosa non ha potuto verificare.
- Exit 5 al primo errore in modalità normale; `--continue` per elencarli tutti.

### Test
- backup integro → exit 0;
- un blob troncato → exit 5 con il numero del chunk;
- un blob con un bit alterato → exit 5;
- `chunks.json` che cita un blob assente → exit 5;
- senza passphrase → exit 0 con avvertenza "verifica parziale".

---

## 06.7 Budget di dimensione e `go:embed` reale

**Agente: Sonnet**

### Prescrizioni
- `make selfextract` produce i binari veri; verificare `ls -l` < **8 MB** per architettura.
- Se il budget è superato, in ordine: (a) `-ldflags "-s -w"` già attivo; (b) verificare con `go tool nm -size -sort size` chi occupa spazio; (c) se il colpevole è un codec inutilizzato, **non** rimuoverlo — il self-extract deve saper leggere qualunque backup, anche prodotto con un altro codec. In caso di sforamento, escalare a Opus.
- Sostituire i placeholder committati: `internal/embedded/embed.go` (fase 00.7) ora restituisce ELF veri.
- Verifica di staticità: `go tool nm` non deve mostrare simboli dinamici; in e2e, `docker run --rm IMAGE` su `scratch` funziona (se non fosse statico, fallirebbe con "no such file or directory").

### Test
- test gated `//go:build embedded`: i due binari sono ELF, > 1 MB, < 8 MB;
- il test di coerenza dei codici di uscita fra `internal/cli` e il self-extract.

---

## 06.8 End-to-end: `docker run` da host pulito

**Agente: Sonnet** — *il test che dimostra il requisito dell'utente*

### File: `test/e2e/phase_06.sh`

```
1.  crea un albero di prova con file, permessi, symlink, hardlink, xattr
2.  backimage backup ./tree --repo localhost:5000/e2e/se:t1 --passphrase-file pass.txt
3.  docker rmi -f localhost:5000/e2e/se:t1   (svuota la cache locale)
4.  docker pull localhost:5000/e2e/se:t1                                  → deve riuscire
5.  docker run --rm localhost:5000/e2e/se:t1                              → info, exit 0
6.  docker run --rm -e BACKIMAGE_PASSPHRASE=... IMAGE list                → elenco corretto
7.  docker run --rm -i -e BACKIMAGE_PASSPHRASE=... IMAGE tar > out.tar
8.  sudo tar xpf out.tar --xattrs --acls --numeric-owner -C restored/
9.  confronto restored/tree con tree                                      → ZERO differenze
10. docker run --rm -e BACKIMAGE_PASSPHRASE=... -v "$PWD/r2:/restore" IMAGE extract --out /restore
11. confronto r2/tree con tree (ownership inclusa: host Linux, bind mount) → ZERO differenze
12. docker run --rm -e BACKIMAGE_PASSPHRASE=wrong IMAGE list              → exit 4
13. docker run --rm IMAGE verify                                          → exit 0 (verifica parziale)
14. docker run --rm -i IMAGE tar (senza passphrase)                       → exit 4
15. ripeti 4–9 sotto `docker run --platform linux/arm64` con qemu         → ZERO differenze
```

### Definition of Done
- [ ] `make e2e PHASE=06` esce 0
- [ ] il passo 9 e il passo 11 hanno **zero** differenze
- [ ] il passo 15 passa con qemu

---

## Gate di fase 06

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./cmd/backimage-selfextract/...` | ≥ 80 % |
| G8 | `make e2e PHASE=06` | exit 0 |
| **GS-06.1** | dimensione dei binari self-extract | < 8 MB ciascuno |
| **GS-06.2** | `go list -deps ./cmd/backimage-selfextract` | nessun modulo vietato |
| **GS-06.3** | `docker run --rm IMAGE` da host senza backimage | exit 0 |
| **GS-06.4** | round-trip via `docker run … tar` + confronto | zero differenze |
| **GS-06.5** | round-trip via `extract` su bind mount Linux | zero differenze |
| **GS-06.6** | stesso test su `linux/arm64` (qemu) | zero differenze |
| **GS-06.7** | passphrase errata | exit 4, nessun dato in output |
| **GS-06.8** | `tar` con stdout su TTY | exit 2 con messaggio guida |
| **GS-06.9** | `tar` su backup da 1 GiB | RSS < 128 MiB |
| **GS-06.10** | `extract --include <un file>` su 100 chunk | < 3 blob letti |
| G10 | `docs/selfextract.md` | presente |
| G11 | revisione Opus | approvazione in `resume.md` |

**Deliverable documentali**: `docs/selfextract.md` — tutti i comandi del container con esempi copiabili, la tabella "quale metodo preserva cosa" (tar+host vs extract su Linux vs extract su Docker Desktop), la gestione della passphrase e l'avvertenza sulla env var, cosa fare se il backup è corrotto (`--no-verify`, `verify --continue`).

**Rischi noti**
- **`scratch` non ha `/tmp`, né `/etc/passwd`, né certificati CA.** Il self-extract non fa rete e non risolve utenti (usa `Uname/Gname` dal tar solo come informazione), quindi va bene. Se in futuro servisse una qualunque risoluzione DNS o TLS, la base image dovrà cambiare: annotarlo.
- L'avviso sul filesystem che non conserva l'ownership è la differenza fra un utente contento e un ripristino silenziosamente sbagliato su macOS. Non va declassato a livello debug.
