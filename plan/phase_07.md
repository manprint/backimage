# Fase 07 — `restore`, `inspect`, `verify`, `ls`, `doctor` — **milestone v0.1.0**

**Obiettivo**: chiudere il ciclo lato host. Al termine di questa fase il prodotto è completo e utilizzabile su una macchina sola; tutto ciò che segue è efficienza e casi d'uso avanzati.

**Riferimento decisioni**: D07 (`restore` deposita il tar per default), D09 (privilegi espliciti).

---

## 07.1 Lettura dei layer da registry senza spacchettare

**Agente: Sonnet**

### File: `pkg/restore/source.go`

```go
// Source gives random access to the blobs of a backup image.
type Source interface {
	// Manifest returns the parsed public manifest.
	Manifest(ctx context.Context) (*index.Manifest, error)
	// ChunkTable returns the parsed chunk table.
	ChunkTable(ctx context.Context) (*index.ChunkTable, error)
	// KeyFile returns the contents of the named key file, or os.ErrNotExist.
	KeyFile(ctx context.Context, name string) ([]byte, error)
	// IndexBlob returns the raw (compressed, possibly encrypted) index.
	IndexBlob(ctx context.Context) ([]byte, error)
	// Blob returns the stored blob of chunk i.
	Blob(ctx context.Context, i int) ([]byte, error)
	// Close releases resources.
	Close() error
}

// FromRegistry builds a Source over a remote image reference.
func FromRegistry(ctx context.Context, ref name.Reference, kc registry.Keychain, opts SourceOptions) (Source, error)

// FromOCILayout builds a Source over a local OCI layout directory.
func FromOCILayout(path string, ref string) (Source, error)

// FromDaemon builds a Source over an image in the local Docker daemon.
func FromDaemon(ctx context.Context, ref name.Reference) (Source, error)
```

### Prescrizioni (determinano l'efficienza del restore parziale)
- **Non scaricare l'immagine intera.** La `Source` da registry deve:
  1. scaricare il manifest (pochi KB);
  2. scaricare **solo** il layer dei metadati (layer 1) per manifest/chunks/index/keys;
  3. scaricare i layer di dati **su richiesta**, uno alla volta, e leggerne il tar in streaming per estrarre i blob richiesti.
- Cache dei layer scaricati in `$XDG_CACHE_HOME/backimage/layers/<digest>` con dimensione massima configurabile (`--cache-size`, default 2 GiB, LRU). Senza cache, un restore parziale che tocca due volte lo stesso layer lo scaricherebbe due volte.
- Costruire in `NewSource` una mappa `chunkIndex → (layerIndex, nomeFileNelTar)` a partire da `chunks.json` e dall'ordine dei layer nel manifest, così `Blob(i)` sa esattamente quale layer aprire.
- `SourceOptions.Platform`: da un index multi-arch scegliere una piattaforma qualsiasi fra quelle disponibili (i layer di dati sono identici); default `linux/amd64`.

### Test richiesti
- `Blob(i)` legge dal layer giusto (registry in-memory con contatore di richieste);
- restore parziale che tocca 2 chunk dello stesso layer → **1** solo download;
- cache: seconda esecuzione → zero download di layer;
- `FromOCILayout` e `FromDaemon` producono gli stessi byte di `FromRegistry`.

---

## 07.2 `restore` — comportamento predefinito

**Agente: Sonnet**

### Sinossi

```
backimage restore <IMAGE> [flags]
backimage restore --repo <IMAGE> [flags]      # forma equivalente
```

`IMAGE` posizionale è la forma principale; `--repo` è accettato come alias (lo spec originale usava entrambe le forme: qui vengono riconciliate).

### Flag

| Flag | Default | Comportamento |
|---|---|---|
| *(nessuno)* | — | **deposita `<nome>.tar` nella cwd** (D07) |
| `--extract, -x` | false | estrae invece di depositare il tar |
| `--destination, -C` | `.` | directory di destinazione (per il tar o per l'estrazione) |
| `--output, -o` | | nome del file tar; `-` significa stdout |
| `--include` / `--exclude` | | glob, restore parziale |
| `--strip-components` | 0 | |
| `--no-preserve-owner` | false | |
| `--overwrite` | false | |
| `--passphrase-*` / `--identity` | | come 03.5 |
| `--platform` | `linux/amd64` | quale manifest usare come sorgente dei blob |
| `--cache-size` | 2GiB | cache dei layer |
| `--no-verify` | false | salta la verifica dei digest per chunk |
| `--jobs` | 3 | download paralleli dei layer |
| `--json` | | |

### Prescrizioni
- Nome del tar per default: `<ultimo segmento del repo>_<tag>.tar`, es. `dumps_daily-20260808T183412Z.tar`. Se esiste già → errore, salvo `--overwrite`.
- `-o -` scrive su stdout: valgono le stesse regole di 06.4 (nessuna diagnostica su stdout, rifiuto se stdout è un TTY).
- `--extract` senza privilegi sufficienti: preflight di 01.7; in strict fallisce prima di scrivere, con hint.
- Verifica del digest per chunk sempre attiva, salvo `--no-verify`.
- Al termine, riepilogo su stderr: file scritti, byte, durata, layer scaricati vs presi dalla cache.

### Test richiesti
- default: produce il tar con il nome atteso;
- `--extract` in una directory temporanea → `CompareTrees` zero differenze (test root);
- `-o -` con stdout su file → tar valido;
- file di destinazione esistente → errore, con `--overwrite` procede;
- passphrase errata → exit 4 prima di scaricare i layer di dati (verifica: contatore dei download a zero);
- `--include` su un solo file → scarica un solo layer.

---

## 07.3 Restore parziale

**Agente: Sonnet**

Già coperto dalle API di 04.2 e dalla `Source` di 07.1: qui si tratta di collegarli e di provare l'efficienza.

### Prescrizioni
- Con `--include`, il flusso non è più un tar contiguo: si producono le sole voci selezionate, in un **nuovo** tar valido (con `--extract` si estraggono direttamente).
- Se `--include` non seleziona nulla → errore `KindUsage` con il conteggio delle voci disponibili e il suggerimento di usare `backimage ls`.
- Le voci selezionate che sono hardlink il cui "primo" file non è selezionato: includere anche il primo (altrimenti il tar è invalido). Regola documentata e testata.

### Test
- selezione di 1 file su 10 000 → tar con 1 voce, meno di 3 blob letti;
- selezione di un hardlink secondario → il tar contiene anche il primario;
- selezione vuota → errore con conteggio;
- selezione di una directory → contenuto ricorsivo.

---

## 07.4 `inspect`, `ls`, `find`

**Agente: Sonnet**

### `backimage inspect <IMAGE>`

Mostra i metadati **pubblici**: la stessa informazione di `docker run IMAGE`, ma senza scaricare i dati né avere Docker. Scarica manifest + layer dei metadati, nient'altro.

Con `--files` elenca anche i file (richiede la passphrase). Con `--layers` mostra la tabella dei layer con dimensione e digest — questo copre la richiesta originale dell'utente ("mostra i file tar contenuti nell'immagine con la dimensione associata").

```
backimage inspect ghcr.io/me/dumps:daily

  riferimento   ghcr.io/me/dumps:daily
  digest        sha256:9f2c…
  piattaforme   linux/amd64, linux/arm64
  creato        2026-08-08 18:34:12 UTC
  sorgenti      /home/fabio/myfiles
  contenuto     12 843 file, 47,2 GiB → 20,0 GiB (zstd:2)
  cifratura     attiva (aes-256-gcm, passphrase)
  layer         48 di dati + 1 metadati + 1 binario

  #   digest          dimensione   chunk
  0   sha256:1a2b…       6,1 MiB   /backimage (linux/amd64)
  1   sha256:3c4d…       842 KiB   metadati
  2   sha256:5e6f…       1,0 GiB   0–255
  …
```

### `backimage ls <IMAGE> [PATH]`
Elenca i file dell'indice. Flag `-l`, `--include`, `--exclude`, `--json`. Stesso formato di `list` del self-extract (06.3): riusare la funzione di formattazione, che va quindi collocata in `pkg/index/format.go` per essere condivisa.

### `backimage find <IMAGE> <PATTERN>`
Scorciatoia per `ls --include`.

### Test
- `inspect` scarica **zero** layer di dati (contatore);
- `inspect --layers` mostra un numero di righe pari ai layer;
- `ls` e il `list` del self-extract producono un output identico sullo stesso backup (test di parità, importante per coerenza);
- `--json` valido per tutti e tre i comandi.

---

## 07.5 `verify`

**Agente: Sonnet**

`backimage verify <IMAGE>` — versione lato host di 06.6, con in più:
- `--quick`: verifica solo i digest dei blob memorizzati leggendoli dal registry senza scaricarli tutti (usa `HEAD` sui blob e confronta le dimensioni; il digest è garantito dal registry stesso, essendo content-addressed). Rapidissimo, verifica la presenza e la coerenza.
- default: scarica tutto, decifra, verifica i digest dei plaintext.
- `--continue` per elencare tutti gli errori.

### Test
- `--quick` su backup integro → exit 0 senza scaricare layer di dati;
- blob cancellato dal registry → errore che nomina il digest;
- backup integro completo → exit 0.

---

## 07.6 `doctor`

**Agente: Sonnet**

`backimage doctor [PATH...]` — esegue e stampa i preflight di 01.7 più i controlli d'ambiente.

```
backimage doctor ./myfiles

  ambiente
    ✓ backimage 0.1.0 (linux/amd64)
    ✓ spazio temporaneo   /tmp — 82 GiB liberi (servono ~3 GiB)
    ✗ docker              non raggiungibile (serve solo per --local-repo)

  privilegi
    ✓ lettura di tutti i file   euid 0
    ✓ chown                     CAP_CHOWN
    ✓ mknod                     CAP_MKNOD
    ✓ xattr di sicurezza        CAP_SETFCAP

  sorgenti
    ✓ ./myfiles   12 843 file, 47,2 GiB, tutti leggibili
    ! ./myfiles/socket   socket: verrà saltato (1 occorrenza)

  registry
    ✓ ghcr.io     credenziali presenti, token ottenuto

esito: pronto per il backup
```

### Prescrizioni
- Ogni riga `✗` **deve** avere sotto una riga `→` con il comando esatto da eseguire per risolvere.
- Exit code: 0 se tutto ok o solo avvisi; 3 se manca un privilegio necessario.
- `--json` produce l'elenco delle `Capability` di 01.7 più i controlli d'ambiente.

### Test
- ambiente senza privilegi → exit 3 con rimedi non vuoti;
- ambiente completo → exit 0;
- sorgente inesistente → errore chiaro.

---

## 07.7 Output JSON su tutti i comandi

**Agente: Haiku**

Verificare e completare `--json` per: `version`, `login --list`, `backup`, `restore`, `inspect`, `ls`, `find`, `verify`, `doctor`.

### Prescrizioni
- Un solo oggetto o array JSON su stdout, nient'altro.
- Gli errori in modalità JSON vanno comunque su stderr in forma testuale, **e** l'exit code resta quello previsto. (Un JSON di errore su stdout renderebbe ambiguo il parsing lato script.)
- Aggiungere a `test/e2e/phase_07.sh` un ciclo che, per ogni comando, esegue `| jq -e .` per validare il JSON.

---

## 07.8 Matrice end-to-end completa

**Agente: Sonnet**

### File: `test/e2e/phase_07.sh`

Matrice obbligatoria (ogni cella deve dare **zero differenze** rispetto all'originale):

| Sorgente del backup | Metodo di restore | Ambiente |
|---|---|---|
| registry | `restore` (tar) + `tar xpf` | Linux root |
| registry | `restore --extract` | Linux root |
| registry | `docker run … tar` | Linux root |
| registry | `docker run … extract` su bind mount | Linux root |
| oci-layout | `restore --extract` | Linux root |
| daemon (`--local-repo`) | `restore --extract` | Linux root |
| registry, `--no-encrypt` | `restore --extract` | Linux root |
| registry, `--compression gzip` | `restore --extract` | Linux root |
| registry, `--compression xz --runnable=false` | `restore --extract` | Linux root |
| registry | `restore --extract --no-preserve-owner` | Linux **non** root (differenze attese solo su owner) |
| registry (backup fatto su Windows) | `restore --extract` su Windows | CI windows |

Più i casi negativi: passphrase errata (4), immagine inesistente (6), blob corrotto (5), privilegi insufficienti (3).

### Definition of Done
- [ ] tutte le celle della matrice verdi
- [ ] tutti i casi negativi restituiscono l'exit code atteso

---

## Gate di fase 07 — **MILESTONE v0.1.0**

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/restore/... ./internal/cli/...` | ≥ 85 % |
| G8 | `make e2e PHASE=07` | exit 0 |
| **GS-07.1** | matrice di restore (11 celle) | zero differenze ovunque |
| **GS-07.2** | `inspect` | zero layer di dati scaricati |
| **GS-07.3** | restore parziale di 1 file su 10 000 | < 3 blob letti |
| **GS-07.4** | passphrase errata | exit 4 **prima** di scaricare i dati |
| **GS-07.5** | parità `backimage ls` vs `docker run … list` | output identico |
| **GS-07.6** | tutti i comandi con `--json \| jq -e .` | exit 0 |
| **GS-07.7** | `doctor` senza privilegi | exit 3 con rimedi |
| G10 | `README.md` completo, `docs/restore.md`, `docs/cli.md` | presenti |
| G11 | revisione Opus + **tag `v0.1.0`** | approvazione in `resume.md` |

**Deliverable documentali**
- `README.md` nella forma definitiva per l'utente finale (vedi fase 12 per il contenuto esatto; qui va già completo di tutti i comandi esistenti).
- `docs/restore.md`: le quattro strade di ripristino a confronto, con una tabella "cosa preserva, cosa serve".
- `docs/cli.md`: riferimento completo di ogni comando e ogni flag, generato a partire da cobra (`cobra/doc.GenMarkdownTree`) e committato, con un test in `make docs-check` che verifica che il file rigenerato coincida con quello committato.

**Rischi noti**
- La cache dei layer può riempire il disco: il limite LRU va rispettato davvero, non solo dichiarato. Test dedicato.
- Il restore parziale con hardlink è la fonte più probabile di tar invalidi: la regola di 07.3 va testata esplicitamente.
