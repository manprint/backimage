# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Metadati riservati cifrati: un backup cifrato scrive `/backup/private.json.zst`,
  sigillato con la chiave del backup, che contiene percorsi sorgente, host,
  totali, impronta e recipient della chiave e, per ogni chunk, digest e byte del
  plaintext. `manifest.json` e `chunks.json` conservano solo ciò che serve a
  scaricare e verificare i blob senza chiave. Dopo lo sblocco i campi vengono
  rifusi in memoria, quindi restore, `ls`, `find`, `verify` e il self-extract si
  comportano come prima.

### Changed

- **Sicurezza**: senza passphrase (o identità age) un'immagine cifrata non
  rivela più nulla del proprio contenuto. In particolare non è più pubblico il
  digest SHA-256 del plaintext di ogni chunk, che permetteva a chi possedeva
  l'immagine di confermare offline la presenza di un file noto senza attaccare
  la crittografia.
- **Sicurezza**: le label/annotazioni OCI `dev.backimage.sources`,
  `dev.backimage.files` e `dev.backimage.bytes-raw` non vengono più pubblicate
  per un backup cifrato: erano leggibili dal registry senza nemmeno scaricare
  l'immagine.
- **Formato**: i metadati di un backup cifrato usano `schemaVersion: 2`; i
  backup non cifrati restano a `schemaVersion: 1`. Questa versione legge
  entrambi, quindi i backup esistenti si restaurano senza modifiche; un
  backimage precedente rifiuta un'immagine nuova con «backup creato da un
  backimage più recente».
- `inspect` mostra sorgenti e totali di un backup cifrato solo quando riceve una
  credenziale (passphrase, `--passphrase-file`, `BACKIMAGE_PASSPHRASE` o
  `--age-identity`); `docker run IMAGE info` fa lo stesso e non chiede mai nulla
  in modo interattivo.

### Fixed

- **Cifratura**: i blob di metadati (`index.json.zst`, `private.json.zst`)
  venivano sigillati con un digest costante, quindi in modalità convergente
  (`--dedup`) due backup che condividono la chiave di repository riusavano lo
  stesso nonce AES-GCM su metadati diversi. Il nonce ora deriva dal contenuto del
  blob, restando deterministico per la deduplica.

## [0.2.1] - 2026-08-11

### Added

- Più account sullo stesso registry: ogni `backimage login --username` è un
  login distinto e non sovrascrive gli altri (tre utenti Docker Hub convivono
  nello stesso file). L'account usato è scelto dal namespace del repository:
  `docker.io/user2/img` usa il login `user2`.
- Flag globale `--registry-user NOME` per scegliere l'account quando il
  namespace non lo identifica (es. `ghcr.io/team/...`); `--registry-user none`
  forza una richiesta anonima.
- `backimage logout --user NOME` e `--all`.

### Changed

- `login --list` stampa provider, account sul provider e utente locale
  proprietario del file di credenziali, più il percorso del file; `--json`
  espone gli stessi campi. Prima elencava solo gli host.
- `logout REGISTRY` con più account si ferma elencandoli invece di rimuoverli
  tutti: serve `--user` o `--all`.
- **Comportamento**: se un registry ha login salvati ma nessuno corrisponde al
  namespace del repository, il comando fallisce indicando i candidati invece di
  usare l'unica credenziale disponibile. Serve `--registry-user NOME` (oppure
  `none` per una richiesta anonima).
- Formato dello store: il primo account di un host resta sotto la chiave host
  (compatibile con Docker e con i file esistenti), gli account aggiuntivi sono
  salvati sotto `host#username`.

## [0.2.0] - 2026-08-11

### Changed

- **Breaking per chi importa il modulo**: il path passa da
  `github.com/fpierri/backimage` a `github.com/manprint/backimage`, coerente
  con il repository. L'immagine pubblicata è `ghcr.io/manprint/backimage`.
- `--tls-self-signed` ora persiste certificato e chiave (in `--tls-cert/--tls-key`
  se indicati, altrimenti in `WORKDIR/tls/`), con validità 10 anni: il PIN
  sopravvive ai riavvii. Resta effimero solo se non c'è dove scrivere, con un
  warning esplicito.
- `listen-remote` stampa il fingerprint SHA-256 anche quando il certificato è
  fornito con `--tls-cert/--tls-key`.
- `repo prune` accetta durate con unità `s/m/h/d/w` (`12h`, `3d`, `2w`) e
  rifiuta i numeri senza unità; l'output umano elenca regole attive, tag da
  eliminare e conteggi invece della vecchia mappa Go.
- `repo tags` e `repo prune` mostrano `-` per i tag senza data di creazione
  invece di `0001-01-01T00:00:00Z`.
- Help della CLI: descrizioni lunghe con esempi per tutti i comandi, unità
  esplicite su dimensioni e durate, exit code 2 per gli errori di flag.

### Added

- `repo prune --delete-older-than DURATION`, formulazione inversa di
  `--keep-within` (indicarle entrambe è un errore d'uso).
- `compose.yml` per avviare `listen-remote` con Docker, e configurazione via
  ambiente: ogni flag di `listen-remote` è impostabile come `BACKIMAGE_<FLAG>`
  (`--bind-address` → `BACKIMAGE_BIND_ADDRESS`). Serve all'immagine distroless,
  che non ha una shell nell'entrypoint.
- README: sezioni «Certificati TLS del server» e «Server in Docker», retention
  con tabella delle regole ed esempi.

### Fixed

- `repo prune` non eliminava mai nulla: il tag punta a un image index
  multi-arch, `desc.Image()` falliva e la data di creazione restava zero, che la
  retention interpreta come «data sconosciuta, non eliminare». La data viene ora
  letta dalle annotazioni dell'index o dal manifest/config di un figlio, e
  `BuildIndex` replica le annotazioni sull'index per i backup futuri.

## [Unreleased]

### Added

- Compressori (gzip/lz4/xz/zstd), chunking fisso con planner a layer (fase 02).
- Crittografia age: passphrase e keyfile, envelope deterministico (fase 03).
- Index di backup, layer deterministici e assemblaggio immagine OCI multi-arch (fase 04).
- Push verso registry con token flow, ripristino di sessione (checkpoint), retry su 429/5xx (fase 05).
- Pipeline `backimage backup`: stima, preflight privilegi, streaming archive→chunk→seal→layer, checkpoint, pubblicazione su registry/daemon/OCI-Layout/tar, output umano/JSON (fase 05.5).
- Comando `backimage login` con store chiavi da registro.
- Test e2e pipeline→registry con registry in-memory (idempotenza, resume, dedup blob).
- Skeleton del comando `backimage version` con output umano e JSON.
- Infrastruttura di errore/exit-code (`Kind` + hint) e stampante umano/JSON.