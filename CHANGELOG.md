# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.1] - 2026-08-22

### Added

- **`repo prune --tag-regex`: retention su un sottoinsieme di tag.** Restringe
  il prune ai tag che corrispondono al pattern; tutti gli altri non vengono mai
  toccati e non consumano gli slot di `--keep-last` né i bucket di calendario.
  Serve a un repository che ospita famiglie di backup diverse, dove
  `--keep-last 3` da solo significherebbe "3 in tutto il repository".
- **`repo prune --group-by-regex`: retention indipendente per gruppo.**
  Partiziona i tag sui gruppi di cattura del pattern e applica le regole dentro
  ogni gruppo, così `--keep-last 3` diventa "3 per famiglia" in un solo
  passaggio. Richiede almeno un gruppo di cattura: senza, ogni tag sarebbe un
  gruppo a sé e la regola conserverebbe tutto in silenzio.
- **`repo tags --tag-regex` e `--group-by-regex`: anteprima read-only.** Sono
  gli stessi selettori di `prune`, valutati dallo stesso codice (`Policy.Select`
  condivide con `Policy.PlanFor` il passo di partizionamento), su un comando che
  non può eliminare nulla.
- Il piano del prune riporta l'ambito e il dettaglio per gruppo, in testo e in
  JSON (`scope`, `groupBy`, `groups`), inclusi i gruppi che non perdono nulla.
  Zero corrispondenze viene segnalato come tale, con la spiegazione
  dell'ancoraggio, invece di passare per un successo silenzioso.

### Fixed

- **Il prune poteva fermarsi a metà lasciando il registry in uno stato
  intermedio.** La cancellazione OCI avviene per digest, e due tag possono
  condividere un manifest (due dump identici di sorgenti diverse). Il vecchio
  loop chiamava `DeleteTag` un tag per volta e scopriva il conflitto solo
  arrivandoci: i tag precedenti erano già stati cancellati e il comando usciva
  in errore. Ora l'intero piano viene verificato prima della prima richiesta e,
  in caso di conflitto, il comando rifiuta elencando i tag coinvolti senza
  inviare nessuna DELETE.
- Conseguenza dello stesso cambio: quando *tutti* i tag di un manifest sono
  nell'insieme da eliminare, il manifest viene rimosso senza richiedere
  `--force`, mentre prima `DeleteTag(force=false)` rifiutava anche quel caso
  legittimo.
- Se una DELETE fallisce a metà piano — cosa che il pre-check non può escludere,
  perché una sequenza di richieste HTTP non è atomica — l'errore dice ora fino a
  dove il comando è arrivato (`N manifest su M erano già stati eliminati`).
  Prima riportava solo l'errore di rete, lasciando indeterminato lo stato del
  repository.

### Changed

- Il percorso di cancellazione di `repo prune` passa da una `DeleteTag` per tag
  a una `DeleteManifest` per digest distinto. Su un registry che disabilita la
  DELETE l'errore arriva quindi una volta sola invece di una per tag, e cade
  l'`ListTags` che `DeleteTag` rieseguiva ad ogni chiamata (50 tag da eliminare
  costavano 50 listing). `repo rm` è invariato.

### Fixed — `--exclude` non escludeva i sottoalberi annidati (grave)

- **`backup --exclude` con `**` archiviava i file annidati che il pattern
  diceva di escludere.** Il filtro passava per `filepath.Match`, dove `**` è un
  carattere jolly di *un solo* segmento: `--exclude 'alice/.cache/**'` eliminava
  `alice/.cache/cookies.db` ma lasciava nell'archivio
  `alice/.cache/chromium/Default/Cookies`. Su uno strumento di backup significa
  archiviare dati che l'operatore aveva chiesto di lasciare fuori. Il prefisso
  letterale (`--exclude 'alice/.cache'`) funzionava già; era la forma con glob a
  dare una falsa ricorsione.
- Tutti i comandi che filtrano path archiviati (`backup --exclude`,
  `restore --include/--exclude`, `ls`, `find`) usano ora **lo stesso** matcher,
  il nuovo `internal/pathglob`: `*` e `?` restano dentro un segmento, `**` ne
  attraversa un numero qualsiasi, zero compreso, e `dir/**` copre `dir` stessa.
  Prima `ls` e `find` erano ricorsivi e `backup` no, con la stessa sintassi.
- Un pattern malformato in `--exclude` è ora un errore d'uso invece di essere
  ignorato in silenzio. `path.Match` risponde «nessuna corrispondenza» a un
  pattern che non riesce a compilare, quindi un typo si leggeva come «niente da
  escludere» e i dati venivano archiviati comunque.
- `TestWriterSkippedSocketAndExcluded` asseriva su nomi archiviati che non
  possono esistere (i path sono radicati sul basename della sorgente): passava a
  vuoto ed è il motivo per cui il difetto non era stato intercettato.

### Changed — documentazione riorganizzata

- `README.md` è ora una guida operativa sintetica in **inglese**: cosa fa lo
  strumento, installazione, primo backup, poi una sezione per comando con
  esempi eseguibili, codici di uscita, variabili d'ambiente e limiti noti. Da
  2028 righe a 385.
- `README.it.md` è la stessa guida in italiano.
- Il vecchio README integrale è conservato come
  [`docs/handbook.it.md`](docs/handbook.it.md): nulla è stato eliminato, le
  ricette lunghe (certificati TLS, `compose.yml`, multi-account, fedeltà
  massima) sono lì.

### Fixed — inesattezze nella documentazione

- I nomi archiviati partono dal **basename** della sorgente
  (`pkg/archive/writer.go`): per `/home/alice` le voci sono `alice/...`. Gli
  esempi di `--exclude` usavano `home/alice/.cache/**`, che non corrisponde a
  nulla. Documentata anche la collisione fra due sorgenti con lo stesso
  basename, che il backup rifiuta.
- `docs/registries.md` affermava che più account sullo stesso registry
  richiedono file separati via `BACKIMAGE_AUTH_FILE`. Non è vero da quando
  esistono le chiavi `host#username` e `--registry-user`.
- `docs/security.md` elencava solo i codici di uscita 0, 4 e 5. La tabella
  riporta ora tutti e otto i valori di `internal/cli/errors.go`.
- La cifra «~184 bit» per `genpass` era il massimo, non il tipico: il valore
  reale oscilla con i caratteri ripetuti (campo `bits` di `genpass --json`).
- L'affermazione «un backup da 50 GiB gira con ~1 GiB libero» non era
  dimostrata: `plan/resume.md` registra la campagna come non eseguita.
  Sostituita con la proprietà architetturale e con la misura reale (picco di
  spool sul client 4 KiB su un backup da 4 GiB).
- L'avviso di passphrase debole è soppresso da `--quiet`: ora è detto.
- `BACKIMAGE_PASSPHRASE` era documentata come «passphrase per la CLI». La
  leggono i comandi di lettura (`restore`, `ls`, `find`, `inspect`, `verify`) e
  l'immagine auto-estraente, ma **non** `backup`, che senza
  `--passphrase-file`/`--passphrase-stdin` esce con «cifratura attiva ma nessuna
  passphrase o destinatario age».
- `BACKIMAGE_<FLAG>` vale solo per `listen-remote` (`applyEnvDefaults` è
  agganciato al suo `PreRunE`), non per tutti i comandi.
- La verifica integrale di un backup cifrato richiede la passphrase: ricalcola il
  digest in chiaro di ogni chunk. Il blocco «uso rapido» mostrava
  `backimage verify IMAGE` senza passphrase, che esce con codice 4.
- L'argomento `PATH` di `ls` e il pattern di `find` sono confrontati con il nome
  archiviato, non con quello sul filesystem: dopo un backup di `/var/log` le voci
  sono `log/...`, quindi `ls IMAGE var/log` non elencava nulla.

### Known issues

- **Una radice `/` singola non è supportata.** `backimage backup /` produce nomi
  archiviati `//etc`, `//home` — il basename di `/` è `/` — quindi le esclusioni
  non li intercettano e il restore di quell'immagine non riesce, terminando con
  un deadlock del processo. Documentato in entrambi i README e nel manuale:
  elencare i sottoalberi (`backimage backup /etc /var/lib /home`). La
  correzione richiede di normalizzare la radice e di sistemare la propagazione
  dell'errore nella pipeline di restore, ed è rinviata a una fase dedicata.

### Notes

- Un pattern deve corrispondere al **tag intero**: `db_` non seleziona nulla,
  `db_.*` seleziona `db_1`. Con la semantica *unanchored* di Go, `db` avrebbe
  selezionato anche `app_db_1` e `mydb_1`, allargando in silenzio
  un'operazione irreversibile. Sintassi RE2: nessun lookahead né backreference,
  `(?i)` per ignorare le maiuscole.
- Una regex non è mai una regola di cancellazione: `--tag-regex` senza
  `--keep-last`/`--keep-within`/`--keep-tag` non elimina nulla.
- L'output di `prune` e di `repo tags` senza i nuovi flag è invariato, campo per
  campo, rispetto a 0.3.0.

## [0.3.0] - 2026-08-21

### Fixed

- **Il restore abortiva a metà estrazione su un metadato non applicabile
  (grave)**. Estraendo un backup che contiene un `/var/lib/docker` annidato, il
  tar porta gli attributi di servizio di overlayfs
  (`trusted.overlay.opaque`, `.redirect`, `.origin`). Scrivere quel namespace
  richiede `CAP_SYS_ADMIN` nell'user namespace iniziale, che un container
  avviato senza `--privileged` non ha: `Lsetxattr` restituiva `EPERM` e, dato
  che il restore girava sempre in modalità strict senza alcun flag per
  degradare, l'estrazione moriva in corsa (`errore: setxattr
  trusted.overlay.opaque: operation not permitted`) lasciando una destinazione
  parziale. I dati archiviati erano integri — ogni chunk aveva superato la
  verifica del digest — ma non erano estraibili senza privilegi.

  Lo stesso abort valeva per ogni altro metadato rifiutato dalla destinazione:
  `lchown` di file di altri utenti in un restore non-root, `chmod`/`utimes` su
  entry non possedute, ACL e `security.*` su filesystem che non li supportano,
  device node senza `CAP_MKNOD`, hardlink non ricreabili (questi ultimi non
  passavano nemmeno dalla gestione dei permessi: qualsiasi errore di
  `os.Link` era fatale).

### Changed

- `--cache-size 0` disabilita davvero la cache dei layer scaricati, come già
  documentava l'help: prima veniva silenziosamente riportata al default di
  2 GiB (solo un valore negativo la disattivava).

- **L'estrazione ora degrada per default invece di abortire.** Owner/gruppo,
  permessi, timestamp, ACL, attributi estesi e hardlink sono best effort: ciò
  che il kernel rifiuta viene contato per classe in `Stats.Degraded`,
  segnalato una volta sola e riepilogato alla fine
  (`degradazioni: owner=… xattr.trusted=…`). Il contenuto dei file viene
  sempre scritto e verificato. Un hardlink non ricreabile diventa una copia
  indipendente invece di un errore. Le entry che non è stato possibile creare
  sono contate in `Stats.Skipped`, elencate in `Stats.Errors` e annunciate con
  un `ATTENZIONE` esplicito nel riepilogo.

  Restano fatali le condizioni che non sono degradazioni: `ENOSPC`, `EDQUOT`,
  `EROFS`, `EIO`, `ENOMEM`, `EMFILE`, `ENFILE`, archivio troncato,
  destinazione già popolata senza `--overwrite`, typeflag non supportato.

- Il preflight non deduce più le capability dall'uid: root in un container ha
  un bounding set ridotto, quindi il set effettivo letto da
  `/proc/self/status` ha la precedenza e l'uid resta solo come fallback.
- Le capability advisory non bloccano più né backup né restore né `doctor`:
  segnalano solo che qualcosa non verrà preservato.

### Added

- **Verifica di ciò che è stato pubblicato (`--verify-after-push`)**. Il registry
  è obbligato dalla spec OCI a ricalcolare il digest di un blob quando l'upload
  viene finalizzato, quindi una corruzione in transito fa già fallire il push.
  Restavano però due casi che nessuno confermava: un blob saltato perché il
  registry dichiarava di averlo già, e un blob saltato perché lo diceva il
  checkpoint. Ora, per default (`quick`), dopo la pubblicazione si rileggono
  una `HEAD` per blob (presenza, dimensione, `Docker-Content-Digest`), una
  `GET` per manifest con ricalcolo locale del digest sul body, e la risoluzione
  del tag: pochi KB, zero disco. Con `full` ogni data layer viene riscaricato
  in streaming e si ricalcolano tre digest indipendenti — quello compresso del
  layer, quello del blob e quello memorizzato di ogni chunk — senza scrivere
  nulla su disco e senza bisogno della chiave. `off` disattiva la rilettura.

- **Un blob remoto di dimensione diversa non viene più creduto**. Se il
  registry dichiara di avere già un blob con quel digest ma con un'altra
  lunghezza, viene reinviato invece di essere saltato.

- **Recupero parziale (`restore --continue`, `extract --continue`)**. Un chunk
  danneggiato fermava tutto: lo stream è sequenziale, quindi un errore al chunk
  393 di 520 perdeva anche i 127 chunk sani successivi. Ora il restore può
  lavorare sull'indice dei file: ricostruisce ogni entry i cui byte stanno in
  chunk che superano la verifica, salta le altre, elenca i percorsi perduti e i
  chunk responsabili, e chiude con l'exit code di integrità. Una entry è scritta
  solo se completa, così un record tar troncato non può rompere quelle
  successive.

- **Evidenze verificabili nei log, per backup e per restore**. Il backup
  dichiara quanti chunk ha registrato con quali digest e cosa ha riletto dal
  registry; il restore dichiara quanti chunk ha verificato e se l'esito è 1:1.
  Quando non lo è, elenca le differenze per classe (`owner`, `mode`, `times`,
  `xattr.<namespace>`, `hardlink`, `object`) con conteggio e un esempio reale
  per ciascuna. Gli stessi dati sono in `--json` (`Degraded`,
  `DegradedExamples`, `Warnings`, `Skipped`, `Errors`).

- `restore --extract` e `extract` dell'immagine auto-estraente accettano
  `--strict` (ripristina l'abort al primo metadato rifiutato) e
  `--no-preserve-xattrs`.
- Gli errori di privilegio in modalità strict riportano il rimedio esatto
  (`--strict`, `--no-preserve-owner`, `--cap-add`), non più solo la syscall
  fallita.
- Il preflight riporta la capability advisory `set-trusted-xattr`: senza
  `CAP_SYS_ADMIN` gli attributi `trusted.*` non sono né leggibili in backup né
  scrivibili in restore.
- Il riepilogo di fine backup elenca anche le verifiche disponibili sul
  ripristino: `backimage verify --continue` prima di estrarre, le righe di
  evidenza da cercare nel log del restore (`integrità: N/N chunk letti e
  verificati`, `esito 1:1`), e `--strict` per pretendere la fedeltà totale.
- Il riepilogo di fine backup stampa i comandi di ripristino nella forma a
  fedeltà massima: `sudo backimage restore …` e `docker run --rm --privileged`
  con `BACKIMAGE_IMAGE_REF` e il socket Docker già inclusi, più la spiegazione
  di quali metadati richiedono privilegi e il suggerimento `--strict` per
  dimostrare che il ripristino è fedele.
- README: nuova sezione «Backup e restore in fedeltà massima» con i parametri
  obbligatori di backup e restore, la prova periodica di ripristino e l'elenco
  di ciò che nessuno strumento può ripristinare.
- `extract` stampa le degradazioni per classe (`degradato owner: 1234`);
  `Stats.Degraded`, `Stats.Warnings` e `Stats.XattrsSkipped` sono esposti anche
  in `--json`.

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