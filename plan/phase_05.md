# Fase 05 — `pkg/registry`, `login`, `backup`

**Obiettivo**: autenticazione ai registry, **token effimeri con refresh** (indispensabile per i backup da 50 GB richiesti dall'utente), push con ripresa, e il comando `backup` completo che mette in fila tutta la pipeline delle fasi 01–04.

**Riferimento decisioni**: D01, D06, D11.

---

## 05.1 Keychain

**Agente: Sonnet**

### File: `pkg/registry/auth.go`

```go
// Keychain resolves credentials for a registry host.
type Keychain interface {
	// Resolve returns an authenticator for the given resource.
	Resolve(res authn.Resource) (authn.Authenticator, error)
}

// NewKeychain builds the layered keychain used by backimage:
//  1. explicit credentials from flags/env
//  2. backimage's own store (~/.config/backimage/auth.json)
//  3. the Docker config (~/.docker/config.json) and its credential helpers
//  4. anonymous
func NewKeychain(explicit *Credentials) Keychain

// Credentials is a username/secret pair for one registry.
type Credentials struct {
	Registry string
	Username string
	Secret   string // password or token
}

// Store persists credentials in backimage's own file, mode 0600.
type Store interface {
	Get(registry string) (*Credentials, error)
	Put(c Credentials) error
	Delete(registry string) error
	List() ([]string, error)  // registry hosts only, never secrets
}

// NewStore opens (creating if needed) the store at path.
func NewStore(path string) (Store, error)
```

### Prescrizioni
- File `auth.json`, permessi **0600** creati con `os.OpenFile(..., 0600)`; se il file esiste con permessi più larghi, **rifiutare** con hint `chmod 600 <path>`. Su Windows saltare il controllo.
- Formato: `{"auths": {"ghcr.io": {"auth": "<base64 user:secret>"}}}`, compatibile con Docker così chi vuole può copiarlo.
- **Il segreto non deve mai comparire** in log, errori, `--json` o `List()`.
- Scrittura atomica: file temporaneo nella stessa directory + `os.Rename`.
- Normalizzazione host: `docker.io` ⇄ `index.docker.io` ⇄ `registry-1.docker.io` sono lo stesso registry; usare `name.Registry.RegistryStr()` come chiave canonica.

### Test richiesti
- put/get/delete/list;
- file con permessi 0644 → errore con hint;
- scrittura atomica: uccidere il processo a metà non lascia un file corrotto (simulare con un writer che fallisce);
- normalizzazione dei tre alias di Docker Hub;
- `List()` non contiene segreti (grep sull'output).

---

## 05.2 Comandi `login` e `logout`

**Agente: Sonnet**

### File: `internal/cli/login.go`

```
backimage login [REGISTRY] [flags]
  --username, -u string     nome utente
  --password, -p string     password o token (SCONSIGLIATO: visibile in ps)
  --password-stdin          legge il segreto da stdin (consigliato)
  --token string            token già pronto (alternativa a username/password)

backimage logout [REGISTRY]
backimage login --list      elenca i registry configurati (mai i segreti)
```

### Prescrizioni
- `REGISTRY` omesso → `index.docker.io`.
- `--password` presente sulla riga di comando → **warning su stderr**: *"la password è visibile nella lista dei processi: preferisci --password-stdin"*. Non è un errore.
- Dopo aver salvato, `login` **verifica** le credenziali con una richiesta di token allo scope `registry:catalog:*` o, se rifiutata, un `GET /v2/`. Fallimento → non salvare, errore `KindNetwork`.
- `login` senza `--username` in TTY → prompt interattivo per utente e segreto (segreto senza eco).

### Test richiesti
- login verso il registry in-memory con auth base attivo;
- credenziali errate → nessuna scrittura su `auth.json`;
- `--password` produce il warning;
- `--list` in JSON produce un array di stringhe.

---

## 05.3 Token effimeri e refresh

**Agente: Sonnet** — *sotto-fase critica per i backup da 50 GB*

### Problema

I bearer token dei registry durano poco (Docker Hub ~300 s). Un upload da 50 GB dura molto di più. Un `401` a metà di un upload da 4 GB, se non gestito, fa ripartire il blob da capo o fa fallire il backup.

### File: `pkg/registry/token.go`

```go
// Scope describes the access needed for one repository.
type Scope struct {
	Repository string // "myuser/dumps"
	Actions    []string // "pull", "push"
}

// Token is a bearer token with its expiry.
type Token struct {
	Value     string
	ExpiresAt time.Time
	Scope     Scope
}

// Valid reports whether the token is still usable with the given safety margin.
func (t *Token) Valid(margin time.Duration) bool

// Provider mints and refreshes bearer tokens for one registry.
type Provider interface {
	// Get returns a valid token for scope, minting or refreshing as needed.
	// It is safe for concurrent use and coalesces concurrent refreshes.
	Get(ctx context.Context, scope Scope) (*Token, error)
	// Invalidate marks the current token for scope as unusable, forcing a
	// refresh on the next Get. Called after a 401.
	Invalidate(scope Scope)
}

// NewProvider builds a provider that performs the Docker registry token flow
// against the realm advertised by the registry's WWW-Authenticate header.
func NewProvider(registry string, auth authn.Authenticator) Provider

// NewStaticProvider wraps a fixed token, used by the server in phase 08.
func NewStaticProvider(get func(ctx context.Context, s Scope) (*Token, error)) Provider
```

### File: `pkg/registry/transport.go`

```go
// NewRoundTripper returns an http.RoundTripper that attaches a bearer token to
// every request and retries exactly once after invalidating the token when the
// server answers 401.
func NewRoundTripper(base http.RoundTripper, p Provider, scope Scope) http.RoundTripper
```

### Prescrizioni (vincolanti)
1. **Margine di sicurezza**: `Valid` usa un margine di **60 s**. `Get` rinnova proattivamente sotto il margine, senza aspettare il 401.
2. **Coalescenza**: richieste concorrenti di refresh producono **una** sola chiamata di rete (`golang.org/x/sync/singleflight` non è nell'elenco dipendenze: implementare con `sync.Mutex` + campo `inFlight chan struct{}`).
3. **Retry su 401**: al massimo **una** ripetizione per richiesta, dopo `Invalidate`. Il corpo della richiesta deve essere ripetibile: usare `req.GetBody` e, se assente e il metodo ha un corpo, **non** ritentare (restituire l'errore) — mai leggere un corpo in memoria per poterlo ripetere.
4. **Upload lunghi**: la sessione di upload di un blob (`POST` → `PATCH`* → `PUT`) è composta da più richieste HTTP; ognuna passa dal RoundTripper e prende un token fresco. Con `PATCH` a pezzi da 32 MiB, nessuna singola richiesta dura più della vita del token. **Prescrizione**: forzare l'upload a pezzi impostando `remote.WithChunkSize(32 << 20)`; senza di essa ggcr fa una `PUT` monolitica che per un layer da 4 GB può superare la scadenza.
5. **Parsing di `expires_in`**: se il registry non lo fornisce, assumere 60 s (conservativo). Se fornisce `issued_at`, usarlo.
6. **Nessun segreto nei log**: loggare solo `scope`, `expiresAt` e l'esito.

### Test richiesti
- server HTTP di test che risponde `401` con `WWW-Authenticate: Bearer realm=…,service=…,scope=…`, poi emette token con `expires_in: 1`;
- una richiesta dopo 1,5 s usa un token **nuovo** (verifica del margine proattivo);
- 50 goroutine che chiamano `Get` insieme producono **1** sola richiesta di token (contatore sul server di test);
- `401` a metà sequenza → esattamente 1 retry, poi successo;
- `401` ripetuto → errore dopo 1 retry, non un ciclo infinito;
- richiesta `PATCH` con `GetBody` nil → nessun retry, errore esplicito;
- token senza `expires_in` → scadenza a 60 s.

### Definition of Done
- [ ] tutti i test sopra verdi
- [ ] copertura `pkg/registry` (file token/transport) ≥ 90 %

---

## 05.4 Push con parallelismo, backoff e checkpoint di ripresa

**Agente: Sonnet**

### File: `pkg/registry/push.go`

```go
// PushOptions tunes the upload.
type PushOptions struct {
	Jobs        int           // parallel blob uploads, default 3
	ChunkSize   int64         // HTTP PATCH chunk size, default 32 MiB
	MaxRetries  int           // per blob, default 5
	Checkpoint  CheckpointStore
	Progress    chan<- ociimg.Progress
}

// CheckpointStore records which blobs are already on the registry, so an
// interrupted backup can resume without re-uploading them.
type CheckpointStore interface {
	// Load returns the checkpoint for a backup id, or nil when absent.
	Load(id string) (*Checkpoint, error)
	Save(c *Checkpoint) error
	Delete(id string) error
}

// Checkpoint is the resumable state of one backup.
type Checkpoint struct {
	ID          string    // deterministic: sha256(sources|tag|codec|chunkSize|version)
	Ref         string
	CreatedAt   time.Time
	DoneBlobs   []string  // digests already confirmed present
	ManifestJSON []byte   // manifest.json bytes, so a resume reuses the same layout
}

// Push uploads idx to ref, skipping blobs already present on the registry.
func Push(ctx context.Context, ref name.Reference, idx v1.ImageIndex, kc Keychain, opts PushOptions) error
```

### Prescrizioni
- **Prima di caricare** ogni blob: `HEAD /v2/<repo>/blobs/<digest>`. Se esiste, marcare `Skipped: true` e non caricare. Questo è già la base della dedup della fase 10.
- Backoff esponenziale con jitter: 1s, 2s, 4s, 8s, 16s; ritentare solo su `5xx`, `429`, e errori di rete. **Mai** su `4xx` diversi da 429 e 401.
- `429`: rispettare `Retry-After` se presente.
- Checkpoint su `$XDG_CACHE_HOME/backimage/checkpoints/<id>.json`, scritto **dopo ogni blob confermato**, cancellato al termine.
- Un backup ripreso deve produrre **lo stesso** manifest: per questo il checkpoint conserva `manifest.json`. Se i sorgenti sono cambiati, l'ID cambia e non si riprende: si ricomincia (comportamento corretto, va documentato).
- Il parallelismo agisce sui blob, non dentro il singolo blob.

### Test richiesti
- push verso registry in-memory: tutti i blob presenti dopo;
- secondo push identico: tutti i blob `Skipped`;
- registry che risponde `500` alle prime 3 richieste di un blob → successo al quarto tentativo;
- registry che risponde `403` → nessun retry, errore immediato;
- interruzione (`context.Cancel`) dopo 2 blob → checkpoint contiene 2 digest; ripresa carica solo i restanti;
- `429` con `Retry-After: 2` → attesa rispettata (con clock iniettabile).

---

## 05.5 Comando `backup`

**Agente: Sonnet**

### File: `internal/cli/backup.go`, `pkg/backup/pipeline.go`

### Sinossi

```
backimage backup <PATH...> --repo <IMAGE> [flags]
```

### Flag (elenco completo e vincolante)

| Flag | Tipo | Default | Note |
|---|---|---|---|
| `--repo` | string | *obbligatorio* | `ghcr.io/utente/dumps` |
| `--tag` | string | `latest` | tag del backup |
| `--timestamp` | bool | false | accoda un timestamp al tag |
| `--timestamp-format` | string | `20060102T150405Z` | layout Go; ordinabile per default |
| `--compression` | string | `zstd` | `zstd\|gzip\|xz\|lz4\|none` |
| `--compression-level` | int | default del codec | |
| `--max-layer-size` | size | `1GiB` | vedi planner 02.4 |
| `--runnable` | bool | true | falso permette codec non standard |
| `--encrypt` | bool | **true** | D06 |
| `--no-encrypt` | bool | false | disattiva la cifratura (esclusivo con `--encrypt`) |
| `--passphrase-stdin` / `--passphrase-file` | | | vedi 03.5 |
| `--recipient` | stringSlice | vuoto | chiavi pubbliche age |
| `--local-repo` | bool | false | output al daemon Docker |
| `--output` | string | `registry` | `registry\|daemon\|oci-layout\|tar` |
| `--output-path` | string | | per `oci-layout` e `tar` |
| `--exclude` | stringSlice | | glob |
| `--one-file-system` | bool | false | |
| `--numeric-owner` | bool | false | |
| `--allow-degraded` | bool | false | disattiva strict (D09) |
| `--jobs` | int | 3 | upload paralleli |
| `--platform` | stringSlice | `linux/amd64,linux/arm64` | architetture del self-extract |
| `--no-metadata` | bool | false | omette i path sorgente dalle label |
| `--dry-run` | bool | false | calcola e stampa il piano, non scrive nulla |
| `--resume` | bool | true | riprende dal checkpoint se presente |

### Pipeline (ordine vincolante)

```
1.  valida i flag (conflitti: --encrypt/--no-encrypt, --local-repo/--output, codec/--runnable)
2.  preflight privilegi (01.7); in strict, esce qui se manca qualcosa
3.  risolve la reference e le credenziali; verifica l'accesso in push con una
    richiesta di token allo scope corretto  ← fallire QUI, non dopo 2 ore di lavoro
4.  stima bytesRaw con un walk leggero -> PlanLayers (02.4)
5.  se --dry-run: stampa il piano ed esce 0
6.  genera KeyMaterial e i keyfile (03.1, 03.2), salvo --no-encrypt
7.  avvia la pipeline in streaming, con goroutine collegate da pipe:
        archive.Writer -> splitter -> [per chunk: compress -> seal] -> accumulo in layer
    Ogni layer completato diventa un v1.Layer e viene messo in coda all'uploader.
8.  costruisce index/manifest/chunkTable a mano a mano
9.  costruisce le immagini per piattaforma e l'index multi-arch (04.4)
10. push con checkpoint (05.4)
11. stampa il risultato (o JSON) e cancella il checkpoint
```

### Prescrizioni sulla memoria
- Il picco di memoria non deve dipendere dalla dimensione del backup. **Vincolo**: un layer viene materializzato su file temporaneo (`$TMPDIR` o `--temp-dir`), non in RAM, perché ggcr deve poterlo rileggere per calcolare il digest e per gli eventuali retry.
  - Spazio temporaneo necessario ≈ `jobs × maxLayerSize`. Va **verificato prima di iniziare** (`statfs` su `$TMPDIR`), con errore chiaro se insufficiente e hint su `--temp-dir` o su `--max-layer-size` più piccolo.
  - Questo è il motivo per cui esiste la modalità remota della fase 08: se non c'è spazio nemmeno per un layer, serve delegare.
- I file temporanei vanno cancellati anche su errore e su `SIGINT` (`defer` + handler di segnale che annulla il contesto).

### Output umano

```
backimage backup ./myfiles --repo ghcr.io/me/dumps --tag daily

  sorgenti      ./myfiles (12 843 file, 47,2 GiB)
  compressione  zstd livello 2
  cifratura     attiva (passphrase)
  piano         48 layer da 1,0 GiB, 12 032 chunk da 4,0 MiB
  upload        [████████░░░░░░░]  22/48 layer  9,8 GiB / 21,3 GiB  118 MiB/s  eta 1m37s
```

Barra di progresso su **stderr**. Con `--quiet` o output non-TTY, una riga per layer completato.

### Output JSON (`--json`)

```json
{"ref":"ghcr.io/me/dumps:daily-20260808T183412Z","digest":"sha256:…",
 "platforms":["linux/amd64","linux/arm64"],
 "files":12843,"bytesRaw":50692097024,"bytesStored":21474836480,
 "layers":48,"chunks":12032,"encrypted":true,"compression":"zstd",
 "durationSeconds":1837,"skippedBlobs":0}
```

### Test richiesti
- unit sui conflitti di flag (6 casi);
- `--dry-run` non contatta la rete e non scrive file (verificabile con un keychain e un filesystem finti);
- pipeline su un albero da 100 MiB verso registry in-memory: manifest coerente, chunk coerenti, index coerente;
- interruzione a metà e ripresa;
- verifica del picco di memoria: backup da 2 GiB con `--max-layer-size 64MiB` e `--jobs 2` resta sotto **512 MiB** di RSS (test `-tags slow`);
- spazio temporaneo insufficiente → errore prima di iniziare, con hint.

---

## 05.6 End-to-end della fase

**Agente: Sonnet**

### File: `test/e2e/phase_05.sh`

1. `registry:2` su `localhost:5000`;
2. albero di prova da **2 GiB** (generato, non committato) con file grandi e piccoli;
3. `backimage login localhost:5000 -u test --password-stdin`;
4. `backimage backup ./tree --repo localhost:5000/e2e/backup --tag t1 --passphrase-file pass.txt`;
5. `docker pull localhost:5000/e2e/backup:t1` → riesce;
6. secondo backup identico → `skippedBlobs > 0` nel JSON;
7. test di ripresa: lanciare il backup, ucciderlo dopo 20 s, rilanciarlo, verificare che riprenda (log `resuming from checkpoint`, e meno byte trasferiti);
8. test del token: registry configurato con token da 10 s di vita (usare un proxy di test in Go incluso in `test/e2e/tools/shorttokenproxy`), backup che dura più di 60 s → deve completare.

### Definition of Done
- [ ] `make e2e PHASE=05` esce 0
- [ ] il punto 8 dimostra il refresh del token sotto carico

---

## Gate di fase 05

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/registry/... ./pkg/backup/...` | ≥ 85 % |
| G8 | `make e2e PHASE=05` | exit 0 |
| **GS-05.1** | 50 refresh concorrenti | 1 sola richiesta di token |
| **GS-05.2** | backup > vita del token | completa senza errori |
| **GS-05.3** | interruzione + ripresa | i blob già caricati non si ricaricano |
| **GS-05.4** | RSS su backup da 2 GiB | < 512 MiB |
| **GS-05.5** | `grep -ri "password\|secret\|token" <log del backup>` | nessun segreto in chiaro |
| **GS-05.6** | permessi di `auth.json` | `0600` |
| **GS-05.7** | `--dry-run` | zero richieste di rete, zero file scritti |
| G10 | `docs/backup.md`, `docs/registries.md` | presenti |
| G11 | revisione Opus | approvazione in `resume.md` |

**Deliverable documentali**
- `docs/backup.md`: tutti i flag con esempi, la pipeline, il fabbisogno di spazio temporaneo, la ripresa.
- `docs/registries.md`: compatibilità verificata per Docker Hub, GHCR, Quay, ECR, Harbor, `registry:2` — per ognuno: login, formato della reference, limiti noti di dimensione dei blob, supporto ai manifest list.

**Rischi noti**
- Il fabbisogno di disco temporaneo (`jobs × maxLayerSize`, cioè 3 GiB con i default) è il vincolo pratico più insidioso: va detto in `docs/backup.md` in modo prominente e verificato in preflight.
- Alcuni registry impongono un limite alla dimensione del blob (spesso 5–10 GiB): con backup enormi il planner può superarlo. Il warning del planner (02.4 punto 6) va mostrato all'utente, non solo registrato.
