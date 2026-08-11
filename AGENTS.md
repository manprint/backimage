# AGENTS.md — guida operativa per backimage

Ultimo consolidamento: 2026-08-11. Questo file si applica all'intero
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

backimage è una CLI Go 1.26 che archivia file in immagini OCI eseguibili,
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

- Lo schema immagine corrente è `schemaVersion: 1`; consultare
  `docs/image-format.md` prima di cambiare JSON, label, annotation o path.
- Ordine layer runnable: `/backimage`, metadata `/backup`, poi layer dati.
- I layer dati sono condivisi tra piattaforme; cambia il self-extract.
- Il limite overlayfs è 127 layer, di cui al massimo 118 dati. Aumentare la
  dimensione dei layer invece di superare il limite.
- Tar, layer metadata e ordinamenti devono restare deterministici.
- I codec non standard `xz` e `lz4` non sono ammessi per immagini runnable.

### Cifratura e segreti

- La cifratura è attiva per default; `--no-encrypt` deve restare esplicito.
- La passphrase del backup non è una credenziale del registry.
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

Test di riferimento: `pkg/registry/auth_multi_test.go` e
`internal/cli/login_multi_test.go`.

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
- Dati richiesti su stdout; log, warning e progress su stderr.
- Exit code CLI: 0 ok, 1 generico, 2 uso, 3 privilegi, 4 passphrase,
  5 integrità, 6 rete, 7 interrotto.
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

- Go 1.26 o superiore;
- `golangci-lint` v1.64.8 in `$HOME/go/bin/golangci-lint`;
- `protoc` 27.3 e `protoc-gen-go` v1.34.2 per `proto-check`;
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
deps-check, docs-check e proto-check. Il race completo usa
`CGO_ENABLED=1 go test -race -p 1 ./...` e può richiedere diversi minuti.

Altri target:

```console
make cover PKG=./pkg/registry/...
make build-all
make embed
make e2e PHASE=08
make e2e PHASE=08_stream
make bench-transport
```

I test root-gated e alcuni e2e richiedono privilegi reali, xattr, ACL,
capability, device/FIFO, Docker o rete. Non sostituirli con mock quando il gate
richiede la semantica del kernel.

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

Ogni modifica al wire format deve preservare compatibilità v1/v2, aggiornare
`docs/protocol.md` e aggiungere test di negoziazione/framing.

### Self-extract embedded

I file
`internal/embedded/backimage-selfextract-linux-{amd64,arm64}` sono placeholder
committati in un clone pulito. `make selfextract`/`make embed` li sovrascrive
con binari reali. Non committare i binari di sviluppo.

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
tag release hanno forma `vX.Y.Z`. Non creare tag, release, push o pubblicazioni
senza richiesta esplicita.

Baseline verificata il 2026-08-11: `make check` verde, incluso race completo;
immagine Docker avviata come `nonroot:nonroot` con nuovo named volume, coppia
TLS persistente creata e nessun errore di permessi. Questa è una fotografia,
non sostituisce il gate dopo nuove modifiche.

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
