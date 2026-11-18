# AGENTS.md — guida operativa per backimage

Ultimo consolidamento: 2026-10-21. Questo file si applica all'intero
repository e serve come punto di ingresso per agenti e sviluppatori.

## Prima di iniziare

Leggere, in quest'ordine:

1. questo file;
2. `README.md` per il comportamento utente corrente;
3. il documento tecnico pertinente sotto `docs/`;
4. codice e test del package interessato;
5. `plan/` solo quando servono decisioni architetturali o contesto storico.

La fonte di verità, in caso di divergenza, è:

1. codice e test verdi;
2. README e documenti tecnici aggiornati;
3. workflow e script dei gate;
4. checklist in `plan/`.

`plan/resume.md` e `plan/resoconto_residuo.md` contengono parti storiche non
allineate al checkout: per esempio descrivono ancora come mancanti il
protocollo v2 streaming, il lifecycle generico, `.goreleaser.yaml` e il
workflow release, che oggi esistono. Non rimuovere le decisioni architetturali
chiuse, ma verificare sempre lo stato reale prima di pianificare lavoro.

Preservare le modifiche già presenti nel worktree. Non ripristinare, spostare
o formattare file estranei al task e non committare senza una richiesta
esplicita.

## Esplorazione del codice con TokenSave

Se sono disponibili i tool MCP TokenSave, usarli prima di leggere sorgenti o
fare scansioni estese:

1. iniziare con `tokensave_context` per una domanda naturale sul task;
2. usare `tokensave_search`, `tokensave_callers`, `tokensave_callees`,
   `tokensave_impact`, `tokensave_node`, `tokensave_files` e
   `tokensave_affected` per restringere simboli e test;
3. se i tool non bastano, interrogare `.tokensave/tokensave.db` direttamente.
   Le tabelle principali sono `nodes`, `edges` e `files`;
4. soltanto dopo leggere i frammenti esatti necessari alla modifica.

Se emerge un limite dell'estrattore, dello schema o dei tool TokenSave,
proporre l'apertura di una issue su
<https://github.com/aovestdipaperino/tokensave>. Ricordare sempre di rimuovere
codice, path, nomi e dati sensibili o proprietari dalla descrizione pubblica.

## Stato funzionale attuale

backimage è una CLI Go 1.27 che archivia file in immagini OCI eseguibili,
cifrate e multi-piattaforma. Il modulo è
`github.com/manprint/backimage`.

Le funzionalità presenti includono:

- backup locale con tar PAX deterministico, chunking, compressione,
  cifratura, layer OCI e push verso registry;
- backup incrementale/deduplicato content-defined, opt-in con `--dedup`;
- restore, estrazione selettiva, inspect, ls, find, verify e doctor;
- immagini auto-estraenti per `linux/amd64` e `linux/arm64`;
- login multi-account, fallback ai credential helper Docker e selezione
  esplicita dell'identità;
- lifecycle OCI generico con `repo stats`, `repo tags`, `repo caps`, `repo rm`
  e `repo prune`;
- backup remoto TLS 1.3 su TCP o QUIC;
- protocollo remoto v2 `stream` predefinito e v1 `layers` legacy;
- autenticazione remota tramite token condiviso, mTLS o modalità esplicitamente
  insicura per LAN isolata;
- immagini e release cross-platform tramite GoReleaser e workflow GitHub;
- immagine tool distroless eseguita come UID/GID 65532.

I comandi root registrati in `internal/cli/root.go` sono:

```text
version, login, logout, backup, restore, inspect, ls, find, verify,
doctor, repo, listen-remote
```

Il riferimento completo dei flag è `docs/cli.md`, generato dalla command tree
Cobra corrente.

## Flussi architetturali

### Backup locale

Il flusso invariabile è:

```text
filesystem -> archivio PAX -> chunk -> compressione -> cifratura
           -> layer OCI -> manifest/index -> registry o output locale
```

Le responsabilità principali sono:

- `pkg/archive`: scansione, tar deterministico e fedeltà dei metadati;
- `pkg/chunk`: chunking fixed/content-defined e pianificazione dei confini;
- `pkg/compress`: codec registrati dietro un'interfaccia comune;
- `pkg/crypt`: DEK, envelope AES-256-GCM e wrapping age;
- `pkg/index`: manifest, chunk table, indice file e lookup;
- `pkg/ociimg`: layer, immagini OCI runnable e manifest multi-arch;
- `pkg/registry`: auth, token, push/pull, verifica e lifecycle;
- `pkg/backup`: orchestrazione end-to-end della pipeline.

### Restore e lettura

`pkg/restore` apre sorgenti registry, OCI layout, tar o daemon locale.
`pkg/recovery` verifica, scarica i layer in modo lazy, decifra, decomprime e
ricostruisce il tar. `pkg/archive` applica poi i metadati sul filesystem.

### Backup remoto

`pkg/remote` è il client, `pkg/server` il ricevente, `pkg/protocol` il contratto
wire e `pkg/transport` fornisce TCP/TLS e QUIC.

- `stream`/v2: il client esegue walk e tar; il server esegue chunking,
  compressione, cifratura, layer e push. Il client non crea spool proporzionale
  al backup, ma il server vede plaintext e DEK di sessione.
- `layers`/v1: il client produce layer già cifrati; il server li pubblica. Il
  ricevente non vede plaintext, ma il client richiede spool locale.

In entrambi i modi le credenziali permanenti del registry restano sul client.
Il client delega al server solo token bearer a scope e durata limitati, dentro
la sessione TLS autenticata.

Contratti del ricevente da non rompere:

- la pubblicazione finale (dopo `StreamEnd` in v2, dopo l'ultimo `LayerEnd` in
  v1) gira mentre la sessione continua a leggere il client
  (`pkg/server/publish.go`): il token di cui ha bisogno può essere quello che
  il client manda in quel momento. Si accettano keepalive e `Token`; `Cancel`
  ferma la pubblicazione; altro è un errore d'uso. Test:
  `pkg/server/session_publish_token_test.go`;
- `--allow-repo` confronta prefissi letterali su componenti di path intere;
  glob e voci vuote sono rifiutati prima del bind (`ValidateAllowedRepos`);
- lo scan del tar v2 applica i limiti dell'indice (entry, lunghezza di nomi e
  owner) mentre legge, non dopo;
- un errore di `accept` transitorio si ritenta con backoff; uno fatale ferma il
  server con quell'errore, invece di lasciarlo vivo e sordo.

Contratto del client v1 (`--remote-mode layers`):

- se il digest pubblicato dal ricevente differisce, il client rilegge per
  digest (non per tag) l'indice dichiarato, con le proprie credenziali, e
  accetta soltanto una differenza nel layer 0, il self-extract, e in ciò che ne
  deriva (config, manifest, indice): stesse piattaforme, ogni altro layer stesso
  blob (`onlySelfExtractDiffers`). Poi ripubblica la propria immagine con il
  proprio estrattore, con verifica quick e senza checkpoint, e i contatori
  restano quelli del ricevente (`republishOwnSelfExtract` in
  `pkg/backup/remote.go`);
- non accettare mai il layer 0 del ricevente: è il programma che `docker run`
  esegue e che legge la passphrase, e `layers` esiste per un ricevente non
  fidato;
- ogni altra differenza è exit 5 (`registry.ErrPublishedMismatch`) senza
  ripubblicazione; una ripubblicazione fallita è exit 5 e l'errore dice che il
  tag contiene ancora l'estrattore del ricevente;
- l'avviso della ripubblicazione passa da `Config.Warn`, non da `Progress`, e
  `Result.SelfExtractRepublished` lo registra in `--json`.

Test: `TestRemoteLayersRepublishesOwnSelfExtract`,
`TestRemoteLayersOtherDataIsNotRepaired`,
`TestRemoteLayersFailedRepairNamesTheTag`,
`TestRemoteLayersInvalidDigestIsAnIntegrityFailure` e
`TestOnlySelfExtractDiffers` (`pkg/backup`).

### Immagine auto-estraente

`cmd/backimage-selfextract` è intenzionalmente piccolo e indipendente dalla CLI
Cobra. `internal/embedded` incorpora i due binari Linux; `pkg/ociimg` li mette
nel layer 0 dell'immagine prodotta. Il self-extract deve poter ripristinare
senza rete e senza backimage installato sull'host di destinazione.

## Mappa del repository

| Percorso | Ruolo |
| --- | --- |
| `cmd/backimage` | entrypoint della CLI principale |
| `cmd/backimage-selfextract` | entrypoint minimale inserito nelle immagini |
| `cmd/gendocs` | generatore del riferimento Cobra |
| `internal/cli` | comandi, flag, config, output ed error classification |
| `internal/embedded` | asset self-extract incorporati con `go:embed` |
| `internal/buildinfo` | versione, commit e data impostati via ldflags |
| `pkg/archive` | tar PAX e metadati filesystem |
| `pkg/backup` | pipeline di backup locale/remota |
| `pkg/chunk`, `pkg/compress`, `pkg/crypt` | trasformazioni dei dati |
| `pkg/index`, `pkg/ociimg` | contratti metadata e immagine OCI |
| `pkg/registry` | credenziali, API registry e retention |
| `pkg/restore`, `pkg/recovery` | apertura sorgenti e ricostruzione dati |
| `pkg/protocol`, `pkg/transport` | wire format e trasporti cifrati |
| `pkg/remote`, `pkg/server` | client e server del backup remoto |
| `test/fixtures` | alberi ostili e confronto di fedeltà |
| `test/e2e` | prove per fase con Docker/registry/processi reali |
| `test/bench/transport` | benchmark TCP/QUIC e netem |
| `docs` | documentazione tecnica e riferimento CLI |
| `scripts` | gate su dipendenze, docs e protobuf |
| `plan` | decisioni e storico delle fasi, non sempre stato corrente |

## Contratti che non vanno rotti

### Formato e riproducibilità

- Lo schema immagine corrente è `schemaVersion: 1` per i backup non cifrati e
  `schemaVersion: 2` per quelli cifrati (metadati riservati nel blob
  `private.json.zst`). Entrambi devono restare leggibili; consultare
  `docs/image-format.md` prima di cambiare JSON, label, annotation o path.
- Ordine layer runnable: `/backimage`, metadata `/backup`, poi layer dati.
- I layer dati sono condivisi tra piattaforme; cambia il self-extract.
- Il limite overlayfs è 127 layer, di cui al massimo 118 dati. Aumentare la
  dimensione dei layer invece di superare il limite.
- Tar, layer metadata e ordinamenti devono restare deterministici.
- I codec non standard `xz` e `lz4` non sono ammessi per immagini runnable.

### Cifratura e segreti

- La cifratura è attiva per default; `--no-encrypt` deve restare esplicito.
- Un backup cifrato non deve descrivere il proprio contenuto in nessun dato
  leggibile senza chiave: né in `manifest.json`/`chunks.json`, né nelle label o
  annotazioni OCI. Ciò che resta pubblico è solo quanto serve a scaricare e
  verificare i blob cifrati (`p`, `ss`, `sb`, layer, codec, `nonceMode`,
  `envelopeVersion`).
  Aggiungere un campo pubblico che parli del plaintext è una regressione:
  metterlo in `index.Private` (`pkg/index/private.go`). Test di riferimento:
  `pkg/index/private_test.go`, `TestPrivateMetadataHidesContentUntilUnlock`
  (`pkg/recovery`), `TestPipelineEncryptedPublishesNoContentMetadata`
  (`pkg/backup`) e la sezione «encrypted metadata» in `test/e2e/phase_06.sh`.
- Il nonce convergente si deriva **solo** dai byte che AES-GCM cifra
  (`crypt.convergentNonce` lavora sul payload che riceve). `Seal` non accetta un
  digest dal chiamante proprio per rendere l'errore inesprimibile: non
  reintrodurre un parametro del genere. Derivare il nonce da qualsiasi altra
  cosa — il plaintext prima della compressione, una costante, un contatore —
  fa riusare il nonce tra backup che condividono la chiave convergente, e due
  messaggi GCM sotto la stessa coppia (chiave, nonce) espongono lo XOR dei
  plaintext e la chiave GHASH. È il bug corretto nella 0.2.4; regressioni:
  `TestConvergentNonceIsSealedPayloadDerived` (`pkg/crypt`),
  `TestConvergentMetadataNonceIsContentDerived` (`pkg/index`).
- Ogni blob sigillato dichiara il proprio `crypt.Role`, che entra nello AAD:
  aggiungendo un nuovo tipo di blob va aggiunto un ruolo, non riusato uno
  esistente. Lo AAD non può invece legare l'identità del backup, o due chunk
  identici diventerebbero diversi e la dedup fra tag morirebbe: lo splice fra
  backup è respinto dai digest plaintext nel blob privato, che il restore
  verifica sempre su un backup cifrato (`Backup.mustVerify`). Non rendere quel
  controllo opzionale.
- Una chiave con `envelopeVersion` inferiore a `crypt.EnvelopeVersion` è
  bruciata: `--dedup` non la riusa (`legacyEnvelopeKey`).
- La dedup confronta digest di blob, quindi dipende dal fatto che lo stesso chunk
  comprima negli stessi byte: codec e livello sono parte del contratto
  (`resolveLevel` eredita il livello dal backup di riferimento). Non introdurre
  non determinismo nel percorso di compressione — nessun timestamp, nessun seed
  casuale, nessuna dipendenza dal numero di worker: `pkg/compress`
  `TestCodecOutputIsReproducible` e `TestZstdOutputIndependentOfWorkerCount`
  bloccano la proprietà, perché una sua rottura degrada la dedup in silenzio
  invece di fallire. `TestCodecOutputMatchesThePinnedDigests` fissa invece i
  byte fra versioni delle librerie: se un aggiornamento lo fa fallire, i nuovi
  digest si aggiornano solo annunciando nel `CHANGELOG.md` il ricaricamento
  una tantum dei backup `--dedup` con quel codec.
  `TestGoldenPinsMatchDependencyVersion` lega inoltre i pin alla versione
  esatta di `klauspost/compress`: un bump non passa inosservato nemmeno se i
  digest restano uguali. Con v1.20.4 sono cambiati i 18 pin gzip (livelli 1,
  6, 9); zstd, lz4, xz e store sono invariati.
- La passphrase del backup non è una credenziale del registry.
- Da file e da stdin la passphrase è la **prima riga**, senza `\n` o `\r\n`
  finale, con la stessa regola per entrambi; le letture delle versioni
  precedenti restano solo come candidati di fallback in lettura
  (`crypt.ReadPassphraseCandidates`), mai in scrittura. Test:
  `pkg/crypt/passphrase_line_test.go`.
- Non loggare password, token, DEK, passphrase o chiavi private.
- Preferire file o stdin ai segreti sulla command line.
- La modalità dedup convergente rivela uguaglianza fra chunk e richiede la
  stessa chiave di repository; non abilitarla implicitamente.
- Il server v2 streaming è dentro il trust boundary dei dati in chiaro.
- TLS 1.3 è obbligatorio anche quando si usa `--insecure-no-auth`.

### Fedeltà filesystem

L'ordine di estrazione è un contratto:

1. creare l'oggetto;
2. scrivere il contenuto;
3. applicare `lchown`;
4. applicare mode;
5. applicare xattr/ACL/capability;
6. applicare i timestamp;
7. ripristinare directory dal livello più profondo dopo tutti i figli.

Chown cancella setuid/setgid e capability, quindi non riordinare questi passi.
Atime è opzionale e ctime non è ripristinabile; sono le sole rilassazioni
previste dai test di fedeltà.

### Lettura della sorgente

Il backup non modifica la sorgente e non legge nulla fuori dalle root, anche
se altri utenti la stanno modificando mentre gira. Per questo i walk su di
essa (archiver, preflight, `archive.Estimate`) passano da `pkg/archive/source.go`:

- ogni figlio è risolto nell'handle `os.Root` della directory che lo ha
  elencato, mai ricostruendo e riaprendo un pathname;
- ogni apertura è `O_NOFOLLOW|O_NONBLOCK` (più `O_NOATIME` su Linux, con
  fallback su `EPERM`), e il descrittore va confrontato con l'`lstat`
  dell'entry (`verifySource`) prima di leggerne byte o attributi: `os.Root`
  segue i symlink interni alla root, quindi il controllo non è opzionale;
- FIFO e device non vengono mai aperti; le entry escluse non vengono lette;
- stima e archivio usano le stesse opzioni (`archiveOptions` in
  `pkg/backup`): non devono mai divergere su cosa contiene il backup.

Non reintrodurre `os.Open`, `os.ReadDir` o `filepath.Walk` sulla sorgente.
Test di riferimento: `pkg/archive/swap_test.go`,
`pkg/archive/noatime_linux_test.go` e la sezione A8.1b di
`test/e2e/phase_A8.sh`.

### Credenziali registry e account multipli

- Lo store è `BACKIMAGE_AUTH_FILE`, poi XDG, poi
  `$HOME/.config/backimage/auth.json`.
- Il file è scritto atomicamente, deve essere `0600` e non è cifrato.
- Docker Hub viene canonicalizzato a `index.docker.io`.
- Il primo account conserva la chiave host per compatibilità; account
  aggiuntivi hanno chiavi distinte.
- Un bearer token host-wide ha identità pubblica `token` e identità interna
  riservata. Deve poter coesistere con account nominati in entrambi gli ordini.
- `GetFor(host, "token")`, `--registry-user token` e
  `logout HOST --user token` devono riferirsi alla stessa credenziale.
- Quando più account convivono, nessun fallback deve scegliere silenziosamente
  l'identità sbagliata.

- Ogni modifica dello store passa da `fileStore.update`, che tiene un lock
  esclusivo su `auth.json.lock` (flock su Unix, `LockFileEx` su Windows)
  attorno a lettura-modifica-scrittura: senza, due `login` concorrenti si
  cancellavano gli account. La lettura resta senza lock (rename atomico).
- Le risposte del registry sono input non fidato: `realm` solo `http`/`https`
  e mai `http` da un registry `https`, challenge letta come da RFC 7235,
  risposta token ≤ 1 MiB, rilettura del manifest ≤ lunghezza pubblicata + 1,
  elenco tag solo tramite `ListTagNames` (limiti di pagine, tag e byte,
  guardia sui cicli di `Link`).

Test di riferimento: `pkg/registry/auth_multi_test.go`,
`internal/cli/login_multi_test.go`, `pkg/registry/auth_lock_test.go`,
`pkg/registry/hardening_test.go` e `pkg/registry/tags_test.go`.

### Ambiente e precedenza CLI

Per `listen-remote`, `BACKIMAGE_<FLAG>` è il default del flag omonimo. Il
caricamento deve visitare sia i flag locali sia i persistent flag ereditati
(`json`, `quiet`, `verbose`, `no-color`, `config`, `registry-user`).

Ordine: flag CLI esplicito > env non vuoto > default. `false` e `0` espliciti
sono valori CLI validi e non devono essere sostituiti. Env vuote o fatte solo
di spazi equivalgono a non impostate. Gli errori devono citare la variabile.

Test di riferimento: `internal/cli/env_test.go`.

### Persistenza TLS

- Una coppia persistente deve essere presente interamente o assente
  interamente; materiale incompleto è un errore esplicito.
- I PEM vengono prima scritti e sincronizzati su temporanei nello stesso
  filesystem, poi pubblicati con rename.
- Chiave `0600`, certificato `0644`, directory `0700`.
- Se fallisce la pubblicazione del certificato dopo quella della chiave,
  rimuovere la chiave appena pubblicata per non bloccare i riavvii.
- Non rigenerare silenziosamente una coppia esistente: cambierebbe il pin dei
  client.

Test di riferimento: `pkg/transport/tls_persist_test.go` e
`internal/cli/listen_remote_tls_test.go`.

### Retention e cancellazioni

- Nessuna regola significa nessuna eliminazione.
- Un tag senza timestamp viene sempre conservato.
- Le regole sono in OR: basta una regola di keep per conservare un tag.
- `--keep-within` e `--delete-older-than` sono alias semantici e non possono
  essere forniti insieme.
- Una durata esplicitamente impostata deve essere maggiore di zero;
  `--delete-older-than 0` non deve diventare una regola disabilitata.
- `--keep-last 0` disabilita invece quella specifica regola.
- Validare la policy prima di ogni chiamata di rete.
- Ogni cancellazione reale richiede `--yes`; usare `--dry-run` nei test e
  negli esempi operativi.

Test di riferimento: `internal/cli/repo_prune_test.go` e
`pkg/registry/retention_test.go`.

### Container e volume `/data`

- L'immagine finale è distroless e gira come `nonroot:nonroot`, UID/GID 65532.
- `/data` deve esistere nell'immagine con owner `65532:65532` e modo `0700`,
  così un nuovo named volume eredita i permessi corretti.
- Un bind mount host va preparato con owner 65532.
- Non aggiungere healthcheck che richiedono una shell: la distroless non ne ha.
- Il Compose di esempio usa `--insecure-no-auth` soltanto come riferimento per
  LAN isolate; non trasformarlo in un default sicuro per Internet.

## Convenzioni di implementazione

- Identificatori, commenti e messaggi di errore in inglese; documentazione
  utente in italiano.
- Errori minuscoli, senza punto finale e avvolti con contesto tramite `%w`.
- Niente `panic` fuori da `main` e niente errori ignorati.
- Niente stato globale mutabile o `init()` con side effect; sono ammesse solo
  le registrazioni plugin già previste.
- Le funzioni che fanno I/O ricevono `context.Context` come primo parametro
  quando l'API lo consente.
- Dati richiesti su stdout; log, warning e progress su stderr. `--quiet`
  toglie il progress, non i warning: in `pkg/backup` ciò che l'operatore non
  deve perdere passa da `Config.Warn` (`printer.Warnf`), non da `Progress`.
- Exit code CLI: 0 ok, 1 generico, 2 uso, 3 privilegi, 4 passphrase,
  5 integrità, 6 rete, 7 interrotto.
- La classe di un errore si decide con `errors.Is`/`errors.As` sui sentinel,
  non su sottostringhe del messaggio: `registry.ErrPublishedMismatch` → 5,
  `backup.ErrPrivileges` e `fs.ErrPermission` → 3, i sentinel di passphrase →
  4, `context.Canceled` e i segnali (SIGINT/SIGTERM in entrambi i `main`) → 7,
  con 5 s di grazia dopo il primo segnale e uscita immediata al secondo
  (`windDown`: un passo che non guarda il contesto non deve tenere vivo il
  processo), un errore di rete (`net.Error` che non sia un `syscall.Errno`:
  un `ENOENT` o un `ENOSPC` locale soddisfa anche lui l'interfaccia) → 6. Un
  `*cli.Error` già classificato non va riclassificato. Test:
  `internal/cli/exitcode_test.go`, `internal/cli/interrupt_test.go` e i
  `signal_unix_test.go` dei due `main`.
- Un flag che il comando ignorerebbe in silenzio è un errore d'uso (exit 2):
  `-x` con `-o`, i flag di sola estrazione senza `-x`, i parametri
  `--dedup-*` senza `--dedup`, `--remove-local-image` senza `--local-repo`,
  `--verify-after-push full|off` con `--remote`.
- Ogni package deve mantenere una package doc concisa.
- Non aggiungere dipendenze senza autorizzazione e senza aggiornare
  `docs/DEPENDENCIES.md`.
- Evitare refactor fuori scope; aggiungere prima un test che riproduce una
  regressione.

Le “dieci regole ferree” storiche sono in `docs/CONTRIBUTING.md`. La regola
“modifica solo i file della sotto-fase” va interpretata come vincolo di scope:
per manutenzione successiva alle fasi, lo scope autorevole è la richiesta
corrente, non un elenco file obsoleto nel piano.

## Setup e loop di sviluppo

Requisiti base:

- Go 1.27 o superiore, con `toolchain go1.27.4` come minimo pinzato in `go.mod`;
- `golangci-lint` v2.15.0 in `$HOME/go/bin/golangci-lint` (la configurazione è sullo schema v2; un binario della serie 1 non la sa leggere e `make lint` lo rifiuta prima di partire; il controllo accetta la versione con o senza la `v` iniziale). Va compilato con un Go non più vecchio di quello del modulo, come in CI: compilato con la 1.26 non sa leggere la standard library della 1.27 e riempie il rapporto di falsi `typecheck`. Un binario installato altrove si passa con `make lint GOLANGCI=/percorso/golangci-lint`;
- `govulncheck` v1.10.0, pinzato in `.github/workflows/ci.yml` e `.github/workflows/release.yml`; il `Makefile` lo invoca senza pin;
- `protoc` 36.2 e `protoc-gen-go` v1.36.12 per `proto-check`, la stessa versione del runtime `google.golang.org/protobuf` in `go.mod`. Con la v1.34.2 il file generato dipendeva dal Go con cui era compilato il plugin (un commento cambiava fra 1.26 e 1.27): la v1.36.12 produce gli stessi byte con entrambe;
- Docker e QEMU solo per e2e/cross-arch;
- CGO disabilitato per build normali; il race detector lo abilita.

Bootstrap:

```console
go mod download
make build
```

Loop rapido proporzionato alla modifica:

```console
gofmt -w FILE...
go test ./internal/cli ./pkg/registry ./pkg/transport
go test -race ./PACKAGE_MODIFICATO
git diff --check
```

Gate autorevole prima della consegna:

```console
make check
```

`make check` esegue fmt, vet, lint, build, test, race seriale tra package,
deps-check, docs-check, proto-check e vuln. Il race completo usa
`CGO_ENABLED=1 go test -race -p 1 ./...` e può richiedere diversi minuti.

Altri target:

```console
make cover PKG=./pkg/registry/...
make build-all
make embed
make e2e PHASE=08
make e2e PHASE=08_stream
make bench-transport
make vuln
```

`make vuln` esegue `govulncheck ./...`. Fa parte di `check`: `go.mod`
pinza `toolchain go1.27.4`, patch del 2026-11-10 che chiude 12 advisory (11
nel toolchain/cgo, una in `net/http`); `govulncheck` non riporta advisory
raggiungibili da questo codice. Le patch precedenti no: con la 1.26.6 erano
11 e la 1.27.2 aveva ancora GO-2026-6809. La dipendenza indiretta
`golang.org/x/crypto` è alla v0.59.0, che non contiene più il pacchetto
`openpgp` non manutenuto di GO-2026-5932. `govulncheck` giudica la standard
library del Go in uso, non il minimo: un Go locale senza la patch che chiude
un'advisory fa fallire il gate anche se il codice è a posto, e
`GOTOOLCHAIN=go1.27.4 make vuln` riproduce la toolchain della CI. Il minimo va
rialzato quando una nuova advisory raggiungibile viene chiusa da una patch
successiva.

I test root-gated e alcuni e2e richiedono privilegi reali, xattr, ACL,
capability, device/FIFO, Docker o rete. Non sostituirli con mock quando il gate
richiede la semantica del kernel. Senza `sudo` si eseguono compilando il
binario di test e lanciandolo come root in un container usa e getta:

```console
CGO_ENABLED=0 go test -c -tags root -o /tmp/archive.test ./pkg/archive
docker run --rm -v /tmp:/t:ro --tmpfs /work:exec,mode=1777 -e TMPDIR=/work \
  --entrypoint /t/archive.test debian:13 -test.count=1
```

Per `phase_01.sh` da root serve anche `--cap-add LINUX_IMMUTABLE` (il caso
`chattr +i`).

## Scelta dei test per area

| Modifica | Verifica minima |
| --- | --- |
| CLI, flag, output | `go test ./internal/cli` |
| archivio/fedeltà | `go test ./pkg/archive ./test/fixtures` |
| chunk/compressione/crypto | test del package + `pkg/backup` + `pkg/recovery` |
| formato OCI | `go test ./pkg/index ./pkg/ociimg` + e2e 04 |
| auth/registry/retention | `go test ./pkg/registry ./internal/cli` |
| restore | `go test ./pkg/restore ./pkg/recovery ./internal/cli` |
| protocollo/trasporto | `go test ./pkg/protocol ./pkg/transport ./pkg/remote ./pkg/server` |
| remote streaming | test precedenti + e2e 08 e 08_stream |
| QUIC | test transport/remote/server + e2e 09 |
| dedup | `pkg/chunk`, `pkg/crypt`, `pkg/backup`, `pkg/recovery` + e2e 10 |
| Docker/Compose | build immagine e smoke con un named volume nuovo |
| documentazione CLI | rigenerazione `docs/cli.md` + `make docs-check` |
| protobuf | `make proto-check` |

Usare `tokensave_affected` sui file cambiati per scoprire consumatori e test
transitivi prima di restringere la suite.

## File generati e asset speciali

### Riferimento CLI

`docs/cli.md` è generato e committato. Non modificarlo a mano:

```console
bash scripts/generate-cli-docs.sh
make docs-check
```

Ogni modifica a command tree, help o flag Cobra deve aggiornare questo file.

### Protobuf

`pkg/protocol/backimage.proto` è la sorgente;
`pkg/protocol/backimage.pb.go` è generato e committato. `make proto-check`
rigenera in una directory temporanea e fallisce in caso di drift.

Il target ha tre esiti distinti: verde senza drift, rosso con drift, e
`SKIP` con **exit 0** quando `protoc` o `protoc-gen-go` non sono installati,
così `make check` resta eseguibile in locale. La CI imposta
`BACKIMAGE_REQUIRE_PROTOC=1` sullo step del gate, dove l'assenza della
toolchain è un errore. In locale si può puntare a un binario qualsiasi con
`PROTOC=/percorso/protoc make proto-check`.

Ogni modifica al wire format deve preservare compatibilità v1/v2, aggiornare
`docs/protocol.md` e aggiungere test di negoziazione/framing.

### Self-extract embedded

I file
`internal/embedded/backimage-selfextract-linux-{amd64,arm64}` sono placeholder
committati in un clone pulito. `make selfextract` li sovrascrive con binari
reali, e `make build`/`make build-all` ora **dipendono** da quel target: non
esiste più una build che incorpora un estrattore più vecchio del codice. Non
committare i binari di sviluppo.

Gli asset sono marchiati con `LDFLAGS_EMBED` (`Version` e `Commit`, mai `Date`:
la data cambierebbe il digest del layer tool di ogni immagine a codice
identico), e `internal/embedded/coeval_test.go` fallisce se il marchio non
coincide con la revisione dell'albero. Conseguenza operativa: la suite va
eseguita dopo almeno un `make build`; con i placeholder ancora in posto il test
lo dichiara invece di passare in silenzio.

Verificare `docs/BUILD.md` e lo stato skip-worktree prima di toccarli. Un
release build deve eseguire `make embed`; un placeholder in release è un
difetto bloccante. `scripts/check-deps.sh` vieta al self-extract dipendenze
pesanti come Cobra, go-containerregistry, quic-go e protobuf.

### Dipendenze

Ogni modulo diretto in `go.mod` deve essere documentato in
`docs/DEPENDENCIES.md`. Non usare `go get` per comodità: prima dimostrare che
la dipendenza è necessaria, autorizzata e compatibile con il self-extract.

## Ricette di modifica frequenti

### Aggiungere o cambiare un flag CLI

1. modificare il costruttore comando in `internal/cli`;
2. mantenere la validazione prima di I/O o rete;
3. verificare output text e JSON e la classificazione degli errori;
4. se è un flag `listen-remote`, aggiungere il test `BACKIMAGE_<FLAG>`;
5. aggiornare README/Compose se è operativo;
6. rigenerare `docs/cli.md`;
7. eseguire `go test ./internal/cli` e `make docs-check`.

### Cambiare autenticazione o account registry

1. preservare il layout legacy sulla chiave host;
2. testare ordine di inserimento, re-login, selezione, ambiguità e delete;
3. non esporre segreti in `Account`, JSON, errori o log;
4. verificare fallback Docker e canonicalizzazione Docker Hub;
5. eseguire almeno `pkg/registry`, `internal/cli`, `pkg/backup` e restore.

### Cambiare TLS o trasporto

1. mantenere TLS 1.3 e autenticazione pin/CA;
2. testare coppie persistenti, file incompleti, permessi e rollback;
3. testare TCP e QUIC quando il cambiamento è sopra l'astrazione transport;
4. verificare cancellazione, retry, quote e assenza di leak nello spool;
5. aggiornare `docs/remote.md`, `docs/protocol.md` e README.

### Cambiare retention

1. separare parsing/validazione da accesso registry;
2. testare policy pure in `pkg/registry`;
3. testare CLI, dry-run, JSON, `--yes` e capability delete;
4. tenere sempre i tag senza timestamp;
5. documentare esattamente semantica di zero e combinazione delle regole.

### Cambiare container o Compose

1. costruire l'immagine multi-stage con helper embedded reali;
2. confermare `Config.User=nonroot:nonroot`;
3. avviare `listen-remote` con un volume Docker nuovo su `/data`;
4. verificare creazione di `/data/tls`, PIN stampato e assenza di
   `permission denied`;
5. rimuovere container, volume e tag temporanei creati dal test.

## CI e release

`.github/workflows/ci.yml` esegue:

- quality su Ubuntu: toolchain, `make embed`, `make check`, coverage;
- cross-build su otto target;
- matrice e2e per 00, 01, 04, 05, 06, 07, 08, 08_stream, 09 e 10,
  con QEMU arm64.

`.github/workflows/release.yml` e `.goreleaser.yaml` gestiscono verifica,
archivi, checksum SHA-256, firme Cosign keyless e immagine GHCR multi-arch. I
tag release hanno forma `vX.Y.Z`, oppure `vX.Y.Z-<suffisso>` per una prerelease
pubblicata da un branch in prova (per esempio `v0.2.3-dev.1`): GoReleaser la
marca come pre-release e il tag GHCR `latest` non viene spostato. Non creare
tag, release, push o pubblicazioni senza richiesta esplicita.

Baseline verificata il 2026-10-21 con go1.27.3: `make check` verde con
`proto-check` reale (`BACKIMAGE_REQUIRE_PROTOC=1`), incluso race completo;
test root di `pkg/archive` e `test/fixtures` verdi in container; matrice e2e
locale verde tranne la 06, che richiede QEMU arm64; immagine Docker avviata
come `nonroot:nonroot` con nuovo named volume, coppia TLS persistente creata e
riusata dopo il restart, nessun errore di permessi. Questa è una fotografia,
non sostituisce il gate dopo nuove modifiche.

Aggiornamento 2026-11-17: toolchain pinzata a go1.27.4, `govulncheck` v1.10.0,
`klauspost/compress` 1.20.4, `moby/client` 0.6.3, `x/sys` 0.50.0, `x/term`
0.48.0 e `x/crypto` 0.59.0. I 18 pin gzip sono stati ricalcolati e il ricarico
una tantum di `--dedup` è annunciato nel `CHANGELOG.md`.
Verifica locale parziale: `go build ./...`, test dei package e `govulncheck`
verdi, `deps-check`, `docs-check` ed e2e 10 verdi. `make check` completo non
eseguito: `golangci-lint` e la toolchain protobuf non sono installati in questa
sandbox.

## Documentazione autorevole

- `README.md`: guida utente completa ed esempi operativi;
- `docs/ARCHITECTURE.md`: layout e contratti immagine;
- `docs/image-format.md`: schema metadata e label OCI;
- `docs/security.md`: crypto, nonce, AAD e threat model;
- `docs/FIDELITY.md`: metadati e ordine di restore;
- `docs/backup.md`, `docs/restore.md`: pipeline e uso avanzato;
- `docs/registries.md`: auth, compatibilità e push;
- `docs/remote.md`, `docs/protocol.md`: server, v1/v2, retry e wire format;
- `docs/dedup.md`: CDC e compromesso di confidenzialità;
- `docs/BUILD.md`, `docs/DEPENDENCIES.md`: build a due stadi e policy moduli;
- `docs/cli.md`: riferimento generato;
- `docs/CONTRIBUTING.md`: convenzioni e disciplina dei gate.

## Gap noti da non nascondere

- Le checklist di fase non sono state riallineate a tutto il codice corrente;
  aggiornare lo stato solo con evidenze di gate, non per deduzione.
- Restano gap formali root-gated/coverage nella fase fedeltà su alcuni ambienti.
- La dedup richiede ancora la review crittografica indipendente prevista dal
  piano prima di dichiararne chiuso il gate formale.
- Il lifecycle generico OCI esiste, ma adapter vendor dedicati e relative
  capability possono essere incompleti: non promettere delete dove il registry
  non lo supporta.
- La pipeline release firma archivi e checksum, ma non configura ancora la
  generazione di un SBOM.
- `docs/troubleshooting.md`, `docs/FAQ.md` e un acceptance finale unico non
  sono presenti al momento di questo consolidamento.
- Non dichiarare milestone v1.0.0 completata finché i gate formali e una
  release candidate firmata non sono verificati.
- Nella fase dati di una sessione remota un `Token` dietro frame dati viene
  letto solo quando la pipeline accetta quei frame: un passo che tiene il
  ricevente (un chunk da 32 MiB del v1, o di `--upload-chunk-size`) più a
  lungo del 40% della vita del token registry fallisce dopo 30 secondi e il
  client ritenta. Descritto in `docs/protocol.md`; la pubblicazione finale non
  ha più questo limite.

## Definition of Done per ogni modifica

- test di regressione che fallisce senza il fix e passa con il fix;
- validazione prima di side effect quando possibile;
- nessun segreto o dato utente in output, log, fixture o diff;
- errori con contesto e corretta categoria CLI;
- documentazione utente e tecnica aggiornata nello stesso change set;
- file generati rigenerati, mai modificati manualmente;
- test focalizzati verdi e suite allargata in base all'impatto;
- `make check` verde per modifiche consegnabili;
- e2e/root/Compose eseguiti quando la semantica dipende dall'ambiente reale;
- `git diff --check` verde e worktree privo di artefatti temporanei;
- questa guida aggiornata se cambiano architettura, invarianti, toolchain,
  gate, flussi o stato funzionale del repository.
