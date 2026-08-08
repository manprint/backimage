# backimage — Piano di sviluppo (overview)

> **Documento normativo.** Ogni fase è in `plan/phase_NN.md`. Lo stato di avanzamento è in `plan/resume.md`.
> Chi implementa NON deve prendere decisioni architetturali: sono tutte già prese e scritte qui.

---

## 1. Cos'è backimage

CLI Go all-in-one, singolo binario, che:

1. **archivia** file e cartelle preservando *integralmente* metadati POSIX/NTFS;
2. **comprime**, **cifra** e **spezza** l'archivio in blob;
3. **assembla** un'immagine **OCI multi-arch eseguibile** che contiene i dati *e* un binario di auto-estrazione;
4. **pusha** l'immagine su un registry qualsiasi (Docker Hub, GHCR, Quay, ECR, custom);
5. **ripristina** i dati sia con `backimage restore`, sia — **senza avere backimage** — con un semplice `docker run`;
6. opzionalmente **delega** l'intero lavoro a un server remoto (`listen-remote`) su TCP/TLS o QUIC, senza mai consegnargli credenziali permanenti.

---

## 2. Decisioni architetturali chiuse (NON rinegoziabili)

| ID | Decisione | Conseguenza operativa |
|----|-----------|----------------------|
| D01 | Nessun dipendenza dal daemon Docker per costruire/pushare. Si usa `go-containerregistry` (ggcr). | Il daemon serve solo per l'output `--local-repo`. |
| D02 | I layer sono **tar validi** compressi gzip/zstd. `docker pull` e `docker run` DEVONO funzionare. | Vietati mediaType non-OCI sui layer dell'immagine runnable. |
| D03 | L'immagine è **multi-arch** (`linux/amd64` + `linux/arm64`) via manifest list. I layer di dati sono **condivisi** fra le piattaforme; cambia solo il layer del binario. | I blob dei dati si caricano una volta sola. |
| D04 | L'immagine contiene `/backimage`, binario **statico** di auto-estrazione, base image **`scratch`**. | CGO_ENABLED=0 obbligatorio ovunque. |
| D05 | **Limite 127 layer di overlayfs**: max **118 layer di dati**. Se il backup è grande, si **aumenta la dimensione del layer**, non il numero. | Confermato dall'utente: layer da 1 GB+ accettabili. |
| D06 | Cifratura **attiva di default**, disattivabile con `--no-encrypt`. DEK casuale AES-256-GCM per chunk; DEK incapsulata con **age** (passphrase scrypt e/o destinatari X25519). | Senza passphrase/chiave il backup è irrecuperabile: va scritto in ogni doc. |
| D07 | `restore` di default **deposita il file `.tar`**; `--extract` estrae preservando tutto. | Il tar è il formato canonico di fedeltà. |
| D08 | Preservazione integrale: uid/gid, uname/gname, mode, setuid/setgid/sticky, mtime/atime ns, **xattr** (`SCHILY.xattr.*` PAX), ACL POSIX, capabilities, label SELinux, **hardlink**, symlink, device, FIFO. | Formato tar: **PAX**. |
| D09 | Se servono privilegi, il comando **fallisce con istruzioni esatte** invece di degradare silenziosamente. `--allow-degraded` è opt-in esplicito. | `--strict` è il default. |
| D10 | Windows e macOS supportati come host. Su Windows i metadati passano da `go-winio/backuptar`. Il container di auto-estrazione è **sempre Linux**. | Per backup Windows, la via fedele è `docker run … tar` + restore con backimage su Windows. |
| D11 | Il server (`listen-remote`) **non gestisce mai credenziali permanenti**: riceve solo bearer token effimeri e li rinnova via protocollo. | Messaggio `TokenRefresh` obbligatorio nel protocollo. |
| D12 | Compressione e cifratura avvengono **lato client** anche in modalità remota (server = tubo). `--server-side-compress` è opt-in. | Il server non vede mai plaintext, per default. |
| D13 | Trasporto astratto: `Dialer`/`Listener`. Impl. TCP+TLS1.3 (default) e QUIC (`--udp`). Niente gRPC. | Protocollo binario proprio, frame length-prefixed + protobuf di controllo. |
| D14 | Ogni componente estendibile è dietro un'**interfaccia registrata in una mappa** (`Codec`, `Splitter`, `Encryptor`, `Transport`, `RegistryAdapter`, `Output`). | Aggiungere una feature = aggiungere un file, mai toccare il core. |
| D15 | Dedup content-defined è in **fase 10**, ma i formati dati sono progettati per accoglierlo fin dalla fase 02. | Nessuna migrazione di formato dopo. |

### Punti aperti (da confermare prima della fase 00)

- **A01** — Module path Go. Il piano usa `github.com/fpierri/backimage`. Se il repo reale ha un altro path, cambiarlo in `go.mod` e nei soli import (un `gofmt -r` risolve).

---

## 3. Layout dell'immagine prodotta

```
/backimage                      binario statico auto-estraente (ENTRYPOINT)  [layer per-arch]
/backup/manifest.json           metadati PUBBLICI, in chiaro                 [layer condiviso]
/backup/chunks.json             mappa chunk→layer/offset, in chiaro          [layer condiviso]
/backup/keys.age                DEK incapsulata (assente se --no-encrypt)    [layer condiviso]
/backup/index.json.zst[.age]    indice file (CIFRATO se cifratura attiva)    [layer condiviso]
/backup/data/000000.blob …      chunk compressi (+cifrati)                   [layer 2..N]
```

Config OCI:

```json
{
  "config": {
    "Entrypoint": ["/backimage"],
    "Cmd": ["info"],
    "WorkingDir": "/",
    "User": "0:0",
    "Labels": { "dev.backimage.schema-version": "1", "…": "…" }
  }
}
```

### Comandi utente finale, senza backimage installato

```bash
docker run --rm IMG                                   # info pubbliche, nessuna password
docker run --rm -it IMG list                          # elenco file (chiede passphrase)
docker run --rm -i  IMG tar > backup.tar              # tar su stdout — FEDELTÀ TOTALE
docker run --rm -it -v "$PWD:/restore" IMG extract --out /restore
docker run --rm -it IMG verify
```

---

## 4. Formati dati (contratti stabili — schemaVersion 1)

### 4.1 `/backup/manifest.json` (pubblico, piccolo, senza elenco chunk)

```json
{
  "schemaVersion": 1,
  "tool": { "name": "backimage", "version": "0.1.0" },
  "createdAt": "2026-08-08T18:34:12Z",
  "sources": ["/home/fabio/myfiles"],
  "host": { "hostname": "ws01", "os": "linux", "arch": "amd64" },
  "totals": { "files": 0, "dirs": 0, "symlinks": 0, "hardlinks": 0, "devices": 0,
              "bytesRaw": 0, "bytesStored": 0 },
  "archive": { "format": "tar-pax", "compression": "zstd", "compressionLevel": 3 },
  "encryption": { "enabled": true, "kdf": "scrypt", "aead": "aes-256-gcm",
                  "nonceMode": "random", "recipients": ["scrypt"] },
  "chunking": { "strategy": "fixed", "targetChunkBytes": 4194304, "count": 0 },
  "layers": [ { "index": 0, "digest": "sha256:…", "chunkFrom": 0, "chunkTo": 63, "storedBytes": 0 } ],
  "index": { "path": "backup/index.json.zst.age", "storedSha256": "…", "encrypted": true }
}
```

### 4.2 `/backup/chunks.json` (pubblico, può essere grande)

```json
{ "schemaVersion": 1,
  "chunks": [ { "i": 0, "p": "backup/data/000000.blob",
                "ps": "sha256 del plaintext", "ss": "sha256 del blob memorizzato",
                "pb": 4194304, "sb": 1048576 } ] }
```

`pb` = plain bytes, `sb` = stored bytes. La concatenazione dei plaintext dei chunk **in ordine di `i`** è esattamente il flusso `tar` non compresso.

### 4.3 `/backup/index.json.zst[.age]` (cifrato se la cifratura è attiva)

```json
{ "schemaVersion": 1,
  "entries": [ { "path": "myfiles/a.txt", "type": "reg", "size": 123, "mode": "0644",
                 "uid": 1000, "gid": 1000, "uname": "fabio", "gname": "fabio",
                 "mtime": "2026-08-01T10:00:00.123456789Z",
                 "linkTarget": "", "tarOffset": 1536, "sha256": "…" } ] }
```

`tarOffset` = offset **del blocco header tar** nel flusso plaintext. Serve al restore parziale: da `tarOffset` si ricava il chunk di partenza tramite `chunks.json`.

### 4.4 Envelope di un blob (`data/NNNNNN.blob`)

```
offset  len  campo
0       8    magic  "BIMGCHK1"  (ASCII)
8       1    version = 1
9       1    codec   (0=store 1=gzip 2=zstd 3=xz 4=lz4)
10      1    aead    (0=none 1=aes-256-gcm)
11      1    flags   (bit0 = nonce convergente)
12      12   nonce   (assente se aead=0)
24      …    payload (compresso, poi cifrato) + tag GCM 16B
```

Ordine invariabile: **tar → compressione → cifratura**. AAD del GCM = `magic||version||codec||aead||flags||uint32be(chunkIndex)`.

### 4.5 `/backup/keys.age`

File age (armored) che contiene, in JSON: `{"dek":"<base64 32B>","nonceKey":"<base64 32B>","schemaVersion":1}`.
Destinatari: `scrypt` (passphrase) e/o uno o più `age1…` X25519. `nonceKey` serve solo in modalità dedup (fase 10).

---

## 5. Struttura del repository

```
backimage/
├── cmd/
│   ├── backimage/            # CLI principale (cobra)
│   └── backimage-selfextract/# binario embeddato nell'immagine (stdlib flag, NO cobra)
├── internal/
│   ├── cli/                  # wiring flag, output umano/JSON, exit code
│   └── buildinfo/            # versione, commit, data (ldflags)
├── pkg/
│   ├── archive/              # tar PAX + metadati (file _linux/_darwin/_windows)
│   ├── compress/             # Codec + gzip|zstd|xz|lz4
│   ├── chunk/                # Splitter + planner dei layer
│   ├── crypt/                # DEK, age wrap, AEAD per chunk
│   ├── index/                # index/manifest/chunks: modelli + I/O
│   ├── ociimg/               # assemblaggio immagine, layer tar deterministici, manifest list
│   ├── registry/             # authn, token provider, push/pull, adapter vendor
│   ├── transport/            # Dialer/Listener: tcp+tls | quic
│   ├── protocol/             # frame + messaggi protobuf
│   ├── server/               # listen-remote
│   ├── backup/               # orchestratore backup
│   └── restore/              # orchestratore restore
├── test/
│   ├── fixtures/             # generatori di alberi di test
│   └── e2e/                  # script e2e, uno per fase
├── docs/
├── plan/
├── Makefile
└── README.md
```

---

## 6. Dipendenze consentite (elenco chiuso)

Aggiungere una dipendenza fuori da questo elenco **richiede approvazione di Opus** e l'aggiornamento di `docs/DEPENDENCIES.md`.

| Modulo | Uso | Fase |
|---|---|---|
| `github.com/spf13/cobra` | CLI principale | 00 |
| `github.com/google/go-containerregistry` | OCI, registry, daemon | 04 |
| `github.com/klauspost/compress` | zstd, gzip | 02 |
| `github.com/ulikunitz/xz` | xz | 02 |
| `github.com/pierrec/lz4/v4` | lz4 | 02 |
| `filippo.io/age` | incapsulamento DEK | 03 |
| `golang.org/x/sys` | xattr, stat, syscall | 01 |
| `golang.org/x/term` | prompt passphrase | 03 |
| `github.com/Microsoft/go-winio` | metadati Windows | 01 |
| `github.com/quic-go/quic-go` | trasporto QUIC | 09 |
| `google.golang.org/protobuf` | messaggi di controllo | 08 |
| `github.com/restic/chunker` | CDC (dedup) | 10 |
| `github.com/stretchr/testify` | asserzioni nei test | 00 |
| `github.com/dustin/go-humanize` | formattazione dimensioni | 00 |

**Vincolo di dimensione**: `cmd/backimage-selfextract` può importare **solo** `pkg/archive`, `pkg/compress`, `pkg/crypt`, `pkg/index`, stdlib, `filippo.io/age`, `golang.org/x/term`, `golang.org/x/sys`, e le librerie di compressione. Mai cobra, mai ggcr, mai quic-go. Budget: **≤ 8 MB non compresso**.

---

## 7. Gate di qualità formali

Ogni sotto-fase si chiude solo se **tutti** i gate applicabili passano. Il comando unico è `make check`.

| Gate | Comando | Criterio di superamento |
|---|---|---|
| **G1 fmt** | `gofmt -l . && goimports -l .` | output vuoto |
| **G2 vet** | `go vet ./...` | zero diagnostiche |
| **G3 lint** | `golangci-lint run` | zero issue (config in `.golangci.yml`) |
| **G4 build** | `make build-all` | binari per linux/darwin/windows × amd64/arm64 + linux/arm/riscv64 |
| **G5 test** | `go test ./...` | tutti verdi |
| **G6 race** | `go test -race ./...` | tutti verdi |
| **G7 coverage** | `make cover PKG=<pacchetto della fase>` | **≥ 80 %** di statement sul pacchetto della fase |
| **G8 e2e** | `make e2e PHASE=NN` | exit code 0 |
| **G9 deps** | `make deps-check` | nessun modulo fuori da `docs/DEPENDENCIES.md` |
| **G10 docs** | `make docs-check` | i deliverable doc della fase esistono e i comandi citati corrispondono a `--help` |
| **G11 review** | revisione Opus | approvazione scritta in `resume.md` |

Gate aggiuntivi specifici sono elencati nel file di fase come **GS-NN.x**.

---

## 8. Harness per l'agente implementatore

> **Leggere prima di scrivere una riga di codice. Queste regole prevalgono su qualunque istinto.**

### 8.1 Le dieci regole ferree

1. **Implementa solo i file elencati** nella sotto-fase corrente. Nessun altro file va creato o modificato.
2. **Non inventare API.** Le firme esportate sono scritte nel file di fase: copiale alla lettera, incluso l'ordine dei parametri.
3. **Non aggiungere dipendenze.** Se sembra servirne una, è un segnale che stai sbagliando approccio: fermati e segnala.
4. **Non modificare un test per farlo passare.** Se un test sembra sbagliato, fermati e segnala. L'unica eccezione è quando la sotto-fase dice esplicitamente "aggiorna il test".
5. **Non rifattorizzare** codice fuori dalla sotto-fase corrente, nemmeno se è brutto.
6. **Non saltare sotto-fasi.** L'ordine è vincolante: ogni sotto-fase presuppone la precedente completata e verde.
7. **Un commit per sotto-fase**, messaggio `feat(NN.x): <titolo della sotto-fase>` (o `test:`/`docs:`/`chore:` quando appropriato).
8. **Errori sempre avvolti** con `fmt.Errorf("contesto: %w", err)`. Mai `panic` fuori da `main`. Mai ignorare un `err` con `_`.
9. **Niente stato globale mutabile**, niente `init()` con effetti collaterali (le mappe di registrazione dei plugin sono l'unica eccezione ammessa).
10. **Ogni funzione che fa I/O accetta `context.Context` come primo parametro.**

### 8.2 Loop di autocorrezione (obbligatorio)

```
1.  Leggi l'intera sotto-fase, inclusa la sezione "Definition of Done".
2.  Scrivi i test PRIMA dell'implementazione, quando la sotto-fase li specifica.
3.  Implementa.
4.  Esegui:  make check
5.  Se fallisce:
      a. leggi SOLO il primo errore riportato;
      b. correggi SOLO quello;
      c. torna al punto 4.
6.  Massimo 5 iterazioni sullo stesso errore.
7.  Se dopo 5 iterazioni il gate non passa:
      - NON proseguire, NON aggirare il test, NON commentare codice;
      - scrivi plan/BLOCKED.md nel formato indicato in 8.3;
      - fermati e chiedi supervisione.
8.  Se make check passa:
      - spunta le caselle della Definition of Done;
      - aggiorna plan/resume.md (sezione 8.4);
      - committa;
      - passa alla sotto-fase successiva.
```

### 8.3 Formato di `plan/BLOCKED.md`

```markdown
# BLOCKED — fase NN.x

## Comando eseguito
<comando esatto>

## Output (ultime 40 righe)
<incolla testuale, non riassumere>

## Cosa ho già provato
1. …
2. …

## File toccati
- path:riga
```

### 8.4 Aggiornamento di `plan/resume.md`

Dopo ogni sotto-fase verde, cambia `[ ]` in `[x]` sulla riga corrispondente e aggiungi in coda alla tabella "Log": data, sotto-fase, hash del commit, esito dei gate.

### 8.5 Assegnazione degli agenti

| Modello | Ruolo | Cosa fa |
|---|---|---|
| **Opus** | Architetto e supervisore | Possiede scope, architettura, decisioni, decomposizione, i gate di revisione, la lettura finale. Assegna, valida, corregge. Autore del piano. |
| **Sonnet** | Sviluppatore | Implementa feature e refactor; ossessivo su test e qualità del codice. Implementatore predefinito di ogni sotto-fase non banale. |
| **Haiku** | Esploratore | Raccoglie fatti sul codice, scrive documentazione, lavoro meccanico e di volume. Scrive codice solo quando Opus giudica il compito al suo livello (boilerplate: campi di struct, parsing di flag, piccole guardie, ritocchi di doc). |

L'assegnazione puntuale è indicata all'inizio di ogni sotto-fase come `Agente: Sonnet` / `Agente: Haiku` / `Agente: Opus`.

---

## 9. Convenzioni di codice

- Go 1.26, `CGO_ENABLED=0` sempre.
- Nomi identificatori, commenti e messaggi di errore **in inglese**. La documentazione utente in `docs/` e `README.md` **in italiano**, con `docs/en/` come traduzione opzionale (fase 12).
- Messaggi di errore: minuscoli, senza punto finale, con contesto (`"open source path %q: %w"`).
- Codici di uscita: `0` ok, `1` errore generico, `2` uso errato dei flag, `3` privilegi insufficienti, `4` passphrase errata, `5` integrità/verifica fallita, `6` errore di rete/registry, `7` operazione interrotta.
- Ogni pacchetto ha un `doc.go` con 5–15 righe che spiegano contratto e invarianti.
- Nessun output su `stdout` che non sia il dato richiesto: log e progresso vanno su `stderr` (indispensabile per `… tar > file.tar`).

---

## 10. Sequenza delle fasi

| Fase | Titolo | Esito |
|---|---|---|
| 00 | Fondamenta, build a due stadi, harness CI | scheletro verde su 8 piattaforme |
| 01 | `pkg/archive` — fedeltà dei metadati | round-trip identico su fixture ostile |
| 02 | `pkg/compress` + `pkg/chunk` | 4 codec, planner dei layer con guardia 127 |
| 03 | `pkg/crypt` | cifratura age + AES-GCM per chunk |
| 04 | `pkg/index` + `pkg/ociimg` | immagine multi-arch assemblata e ispezionabile |
| 05 | `pkg/registry` + `login` + `backup` | push reale, ripresa, token refresh |
| 06 | binario auto-estraente | `docker run` ripristina senza backimage |
| 07 | `restore`/`inspect`/`verify`/`ls`/`doctor` | **milestone v0.1.0** |
| 08 | trasporto TCP/TLS, protocollo, `listen-remote` | backup a zero disco su client e server |
| 09 | trasporto QUIC `--udp` | numeri di benchmark che giustificano il flag |
| 10 | dedup content-defined | il secondo backup carica solo il delta |
| 11 | `repo` + retention + adapter vendor | ciclo di vita dei tag |
| 12 | documentazione finale, README, release | **v1.0.0** |

Le fasi 00–07 producono un prodotto completo e autosufficiente. Le 08–09 coprono il caso "niente spazio disco". Le 10–12 riguardano efficienza e ciclo di vita.
