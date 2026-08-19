# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.4] - 2026-08-19

### Security

- **Riuso di nonce AES-GCM con `--dedup` (critico)**. In modalità convergente il
  nonce veniva derivato dal digest del chunk *in chiaro* mentre GCM cifrava i
  byte *compressi*. Due backup che condividono la chiave di repository
  sigillavano quindi due stringhe di byte diverse sotto lo stesso nonce ogni
  volta che la forma compressa di un chunk invariato cambiava: bastava un
  `--compression` o un `--compression-level` diverso fra due esecuzioni, oppure
  un aggiornamento del compressore che cambi l'output a parità di livello. Due
  messaggi AES-GCM sotto la stessa chiave e lo stesso nonce espongono lo XOR dei
  rispettivi plaintext e la chiave di autenticazione GHASH, cioè la possibilità
  di forgiare blob autenticati arbitrari con quel DEK.

  Il nonce è ora derivato dai byte che GCM cifra davvero
  (`HMAC-SHA256(NonceKey, label ‖ role ‖ sha256(payload))`): nonce uguale
  implica payload uguale, che è esattamente il caso che serve alla
  deduplicazione, quindi non si perde nulla. La firma di `crypt.Sealer.Seal` non
  accetta più un digest dal chiamante, così l'errore non è più esprimibile.

- **Chiavi legacy non più riusate**. Una chiave che ha sigillato blob convergenti
  con la derivazione precedente alla 0.2.4 viene considerata bruciata: `--dedup`
  genera una chiave nuova invece di riusarla, perché quel DEK può già avere il
  suo GHASH compromesso. Il manifest pubblica `encryption.envelopeVersion` per
  permettere la verifica prima di aprire qualsiasi cosa. Conseguenza operativa:
  il primo backup `--dedup` cifrato dopo l'aggiornamento ricarica tutti i blob
  una volta, poi la deduplica riprende normalmente.

- **Separazione di dominio nell'AAD**. Fino alla 0.2.3 `index.json.zst`,
  `private.json.zst` e il chunk dati 0 venivano sigillati con un AAD identico
  (indice 0), quindi sotto la stessa chiave uno autenticava al posto dell'altro.
  L'envelope versione 2 autentica il ruolo del blob: uno scambio ora fallisce con
  `ErrIntegrity` invece di arrivare al parser JSON.

- **`--no-verify` non disattiva più la verifica del plaintext su un backup
  cifrato**. Da quando i digest del plaintext vivono nel blob privato sigillato
  (0.2.3) quel controllo non è più un test anti-corruzione barattabile con la
  velocità: è ciò che rifiuta un chunk spostato tra due backup che condividono la
  chiave, uno splice che AES-GCM da solo non vede perché la modalità convergente
  lascia deliberatamente la posizione fuori dai dati autenticati. Su un backup in
  chiaro `--no-verify` continua a valere come prima.

- **Avviso su passphrase debole**. `backimage backup` stima il lavoro di
  indovinamento della passphrase e avvisa sotto i 96 bit, indicando
  `backimage genpass`. È solo un avviso: non blocca nulla e non stampa mai la
  passphrase. Chi possiede l'immagine possiede anche il file chiavi e può provare
  le passphrase offline senza limiti di tentativi, quindi la passphrase è
  l'unica difesa che resta.

### Added

- **`backimage genpass`**: genera una passphrase robusta con `crypto/rand`, senza
  bias di modulo (`crypto/rand.Int`, non `%`). Default 32 caratteri su
  minuscole, maiuscole, cifre e simboli (~184 bit), con almeno un carattere per
  classe. I glifi ambigui `l I 1 O 0` sono esclusi per default, perché una chiave
  si rilegge da uno schermo e un `1` letto come `l` perde il backup esattamente
  come una passphrase dimenticata; `--ambiguous` li riammette. Flag: `--length`,
  `--count`, `--no-symbols`, `--ambiguous`, più `--json`. La passphrase esce solo
  su stdout: non viene mai loggata, salvata o inviata a un registry.

- Test che bloccano il trattamento byte-esatto della passphrase su tutte le
  sorgenti (`--password`, `--passphrase-file`, `--passphrase-stdin`,
  `BACKIMAGE_PASSPHRASE`): punteggiatura ASCII completa, spazi interni e finali,
  `\r` incorporato e UTF-8 multibyte passano intatti fino a scrypt, e ogni
  variante a un byte di distanza viene rifiutata. Nessuna normalizzazione,
  nessun trim oltre al singolo newline finale di file e stdin.

- Metadati riservati cifrati: un backup cifrato scrive `/backup/private.json.zst`,
  sigillato con la chiave del backup, che contiene percorsi sorgente, host,
  totali, impronta e recipient della chiave e, per ogni chunk, digest e byte del
  plaintext. `manifest.json` e `chunks.json` conservano solo ciò che serve a
  scaricare e verificare i blob senza chiave. Dopo lo sblocco i campi vengono
  rifusi in memoria, quindi restore, `ls`, `find`, `verify` e il self-extract si
  comportano come prima.

- `backimage backup --upload-chunk-size`: spezza ogni upload in chunk HTTP
  della dimensione indicata (es. `32MiB`). Il default `0` invia un blob per
  richiesta ed è la scelta più veloce; serve solo verso registry che rifiutano
  richieste grandi.

### Fixed

- **Deduplica non deterministica sul livello di compressione**. Un chunk si
  deduplica solo se comprime negli stessi byte, e il livello lo decide quanto il
  codec. `--dedup` eredita ora il livello dal backup di riferimento quando non è
  stato chiesto esplicitamente, esattamente come già faceva con i parametri CDC:
  un default che si muove fra due release avrebbe altrimenti ricodificato ogni
  chunk azzerando la dedup, senza nulla nell'output a spiegare il caricamento.
  Un `--compression-level` esplicito vince sempre.

  Se codec o livello effettivi non coincidono con quelli del backup precedente
  il backup lo dice, invece di ricaricare tutto in silenzio. Il codec viene
  segnalato e non adottato: adottarlo potrebbe tirare dentro `xz` o `lz4`, che
  un'immagine eseguibile rifiuta, e passare `--compression` è una scelta
  deliberata che merita una spiegazione, non un override.

  Il numero di worker dello zstd, invece, **non** era una causa: misurato che
  `WithEncoderConcurrency` non altera i byte prodotti. La parallelizzazione resta
  e la proprietà è bloccata da `TestZstdOutputIndependentOfWorkerCount`, così un
  eventuale cambiamento in `klauspost/compress` fa fallire un test invece di
  degradare la dedup in silenzio. `TestCodecOutputIsReproducible` estende la
  verifica di riproducibilità a tutti i codec e livelli.

- L'identità del checkpoint (`checkpointID`) usa ora il livello di compressione
  **risolto** e non lo zero del chiamante: due esecuzioni con livelli effettivi
  diversi non condividono più un checkpoint. Effetto collaterale: i checkpoint
  creati da una versione precedente non vengono più ritrovati e un backup
  interrotto riparte da capo una volta.

### Changed

- **Envelope dei blob alla versione 2**. Il layout dei byte è identico; cambiano
  la derivazione del nonce convergente e i dati autenticati, come descritto
  sopra. La versione 1 continua a essere letta, quindi i backup già in un
  registry si ripristinano senza modifiche. Un `backimage` precedente alla 0.2.4
  non legge un backup nuovo: rifiuta i blob con
  `unsupported blob version 2 (support 1-2)`.

- **Prestazioni**: ogni blob viene caricato in un'unica richiesta HTTP
  streamata invece che in chunk PATCH da 8 MiB. Il chunking costava un round
  trip completo per chunk, che il registry chiude solo dopo aver scritto il
  chunk sul proprio storage: circa 130 attese sincrone per un layer da 1 GiB.
  Il corpo non passa più per la memoria e viene riaperto in caso di 401. Un
  registry che rifiuta corpi grandi (413) fa ricadere il push su chunk da
  32 MiB da solo. In cambio, un layer fallito riparte da zero: il checkpoint
  per-blob resta invariato.

- **Prestazioni**: sul percorso remoto (server che riceve i layer in
  streaming) l'upload tiene due buffer, così il riempimento del chunk
  successivo si sovrappone alla PATCH in volo. Il working set per upload
  concorrente passa da uno a due chunk (64 MiB con il default) e resta
  indipendente dalla dimensione del blob. Un errore di PATCH può ora emergere
  al flush successivo invece che dalla `Write` che ha riempito il buffer:
  resta sticky e `Commit` lo riporta sempre.

- **Prestazioni**: il traffico verso i registry non usa più
  `http.DefaultTransport`. Il nuovo transport dedicato forza HTTP/1.1 (con h2
  ogni upload concorrente veniva multiplexato su una sola connessione TCP e si
  bloccava sul flow control dei reverse proxy davanti ai registry),
  dimensiona il pool di connessioni idle sul numero di job e allarga i buffer
  di scrittura. Proxy, timeout e default TLS restano quelli della libreria
  standard. Analisi e passi successivi in `docs/TROUGHPUT_IMPROVE.md`.

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
- CI/release: i workflow accettano anche tag di prerelease `vX.Y.Z-<suffisso>`
  (es. `v0.2.3-dev.1`), pubblicati come pre-release GitHub; per questi il tag
  GHCR `latest` non viene spostato.
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