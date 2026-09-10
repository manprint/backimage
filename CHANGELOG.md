# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.1] - non ancora rilasciata

Release di sicurezza e di gate. Nessun cambio del formato immagine: un backup
prodotto dalla 0.4.1 resta leggibile dalla 0.4.0 e viceversa.

### Nota sulle release già pubblicate (v0.1.0 → v0.4.0)

Le release da v0.1.0 a v0.4.0 **non vengono ritirate**, e i loro asset restano
scaricabili. Vanno però lette per quello che sono:

- sono state costruite **prima** dei fix di questa versione, quindi
  l'estrattore incorporato nelle immagini prodotte con esse è anteriore ai fix;
- sono state costruite con una standard library che presenta **15 advisory
  raggiungibili** dal codice di backimage, fra cui `archive/tar`
  (GO-2026-4869), quattro su `crypto/x509`, tre su `crypto/tls` e tre su
  `net/http`. Tutte sono chiuse da `go1.26.6`, che questa versione pinza in
  `go.mod`.

Gli asset incorporati in quelle release **erano** coetanei del rispettivo tag:
CI e release eseguono `make embed` prima di costruire, quindi non si tratta di
estrattori disallineati rispetto al codice del tag, ma di estrattori anteriori
ai fix.

**Come sapere cosa contiene una propria immagine.** L'estrattore incorporato
dichiara ora la propria identità:

```console
docker run --rm ghcr.io/tuo/backup@sha256:… version
```

Un'immagine prodotta prima della 0.4.1 non ha il sotto-comando `version`: in
quel caso l'immagine è per definizione anteriore a questa release. Il campo
`tool.version` del manifesto pubblico resta leggibile in entrambi i casi con
`backimage inspect`.

**Come rigenerarla.** Rieseguire il backup con la 0.4.1: i layer dati non
cambiano se i dati non sono cambiati, quindi il costo è il solo layer tool.

### Changed

- **L'esempio primario di restore non è più `docker run --privileged`.** In
  entrambi i readme e nell'handbook il primo comando mostrato è ora il profilo
  confinato — `--network none`, `--read-only --tmpfs /tmp`, `--cap-drop ALL`,
  `--security-opt no-new-privileges`, `--user "$(id -u):$(id -g)"`, un solo
  volume, nessun socket del daemon — che è lo stesso profilo che gli
  end-to-end esercitano. Il profilo a fedeltà massima resta documentato più in
  basso, con cosa compra, cosa costa e la raccomandazione di confinarlo in una
  VM: la documentazione non promette più insieme «ripristino illimitato di
  device e capability» e «assenza dei privilegi necessari».
- **Rottura deliberata: `--remove-local-image` non esiste più
  nell'autoestraente.** Il flag richiedeva di montare `/var/run/docker.sock`
  dentro l'ambiente di estrazione; su un daemon rootful quel socket è
  controllo dell'host molto oltre la cancellazione di un'immagine, e il
  processo non era vincolato alla funzione che l'utente intendeva invocare.
  Invocarlo ora produce un errore d'uso (exit 2) che indica l'equivalente
  dell'host — `backimage restore --remove-local-image`, che resta e gira dove
  il socket già c'è — **prima** di estrarre qualsiasi cosa. Il pacchetto
  Docker non è più nemmeno collegato nel binario: `scripts/check-deps.sh` lo
  vieta come già vieta cobra, go-containerregistry, quic-go e protobuf.
- **Rottura deliberata: un backup remoto con un token statico nel docker
  config ora fallisce subito.** Chi usa `AuthConfig.RegistryToken` (tipicamente
  una CI con un PAT) insieme a `--remote` vedeva il token partire verso il
  server con una scadenza inventata. Ora il backup si ferma prima
  dell'upload con codice 3. La via d'uscita è `--forward-static-token`, che
  invia la credenziale dichiarando il cambio di fiducia; l'alternativa
  preferibile è dare al server un proprio account di registry.
- **`go.mod` pinza `toolchain go1.26.6`.** La riga `go 1.26` resta invariata:
  non è un innalzamento del requisito di linguaggio, è un minimo di toolchain.
  La CI risolveva già `go 1.26` alla patch più recente, ma nulla impediva di
  costruire un rilascio con una patch anteriore. Dopo il bump `govulncheck`
  non riporta **alcuna** advisory raggiungibile dal codice.
- **`make check` esegue anche `vuln`** (`govulncheck ./...`) e
  `proto-check`. Le advisory che restano riguardano codice che non viene
  chiamato e sono elencate sotto.
- **`make build` e `make build-all` dipendono da `make selfextract`.** Gli
  asset di auto-estrazione incorporati non possono più essere più vecchi del
  codice che li incorpora. `make embed` resta come alias di `make build`. Un
  nuovo test in `internal/embedded` rilegge il marchio di build dagli asset e
  fallisce se non coincide con la revisione dell'albero; di conseguenza la
  suite di test va eseguita dopo almeno un `make build`.
- **Configurazione di lint sullo schema golangci-lint v2** (binario `v2.1.6`,
  pinzato in `GOLANGCI_VERSION`, verificato da `make lint` prima di eseguire il
  linter). L'insieme dei controlli è invariato: `gosimple` non esiste più in v2
  perché assorbito in `staticcheck`, e le famiglie `ST1*`/`QF1*`, che la
  configurazione v1 non abilitava, restano disattivate.

### Security

- **Il server remoto non sceglie più cosa il client chiede al proprio provider
  di credenziali.** Repository e azioni arrivavano nel `TokenRequest` e
  finivano dritti in `Provider.Get`: un server autenticato poteva far coniare
  `unrelated/repository:delete` e riceverne il token: quanto il registry poi
  concedesse dipendeva dai privilegi dell'account, ma la decisione non era
  nostra. Ora lo scope è derivato **una volta** dal riferimento scelto in
  locale e ogni richiesta viene validata **prima** di chiamare il provider:
  repository diverso, azione fuori da `pull`/`push`, wildcard, azione ripetuta,
  o troppi scope distinti in una sessione. Una richiesta rifiutata non produce
  nessuna chiamata al provider e nessun token sul filo; il backup esce con
  codice 3 e non ritenta.

- **Una credenziale permanente non viene più spacciata per delega limitata.**
  Un bearer statico (`AuthConfig.RegistryToken`, tipicamente un PAT nella
  configurazione docker) veniva restituito così com'era con una scadenza
  **inventata** di 24 ore, e partiva verso il server remoto etichettato come
  delega. Lo stesso valeva, di fatto, per un registry che non emette token e
  vuole HTTP Basic. Ora la scadenza inventata non c'è più e quelle credenziali
  sono marcate non delegabili: il backup si ferma **prima** dell'upload con un
  messaggio che le distingue da un provider difettoso e nomina l'uscita. Vedi
  la rottura dichiarata più sotto.

- **Nessun byte non verificato raggiunge più il destinatario di un restore.**
  `StreamTar` copiava il chunk decompresso direttamente nel tar e confrontava
  dimensione e digest plaintext **dopo**: al momento del rifiuto il consumatore
  aveva già ricevuto l'intero chunk (circa 10 KiB nella misura della review, e
  in generale tutto il chunk). Il caso che conta non è un tag AEAD rotto —
  quello viene respinto prima di decomprimere — ma un blob **validamente sigillato
  e fuori posto**: con nonce convergente l'indice del chunk è
  deliberatamente fuori dai dati autenticati, quindi un blob spostato fra due
  backup che condividono la chiave di dedup si apre senza errori e solo il
  digest plaintext del blob privato lo smaschera. Ora il chunk viene
  decompresso due volte: la prima passata alimenta solo il digest, la seconda
  scrive, e viene raggiunta solo se la prima ha confermato dimensione e digest.
  Costo: una passata di decompressione in più, nessuna memoria in più — il
  compresso è già residente. Con `--no-verify` su un backup non cifrato non c'è
  digest da confrontare e la passata singola resta quella di prima.

- **Un backup cifrato non poteva più essere convinto a consegnare byte non
  autenticati.** L'header di un envelope dichiara il proprio AEAD, e `aead=none`
  è legittimo perché è la forma di un backup non cifrato. Esisteva però un solo
  lettore per entrambe le forme, che restituiva il payload di un blob
  `aead=none` **anche possedendo la chiave**: riscrivere quell'header non
  richiede alcuna chiave, quindi chi poteva sostituire un blob dentro
  un'immagine poteva farsi consegnare dati, indice dei file o metadati
  riservati che nessuno aveva firmato. Ora i lettori sono due tipi distinti e
  quale usare si decide una volta sola, da `encryption.enabled` nel manifesto,
  mai dall'header del blob che si sta leggendo. Il rifiuto è un errore di
  integrità e precede l'emissione: zero byte di plaintext raggiungono tar,
  stdout o filesystem. `index.ReadIndex` e `index.ReadPrivate` ricevono
  l'aspettativa come parametro invece di dedurla dalla forma del blob, e un
  manifesto incoerente fra cifratura dichiarata e blob privato presente viene
  respinto prima di toccare qualunque blob. Nessuna release ha mai prodotto un
  blob `aead=none` dentro un backup cifrato, quindi nessun backup esistente
  diventa illeggibile. Dettagli in `docs/security.md`.

- **Un blob privato manomesso esce 5, non più 4.** Lo sblocco fallisce per due
  motivi molto diversi — la credenziale è sbagliata, oppure il blob privato non
  è autenticato — e finora entrambi uscivano 4 con «passphrase errata». Chi
  leggeva quel codice cercava un errore di battitura mentre la risposta onesta
  era che l'immagine non è più quella che dichiara di essere. Ora
  `crypt.ErrIntegrity` e `index.ErrBadSchema` si classificano come integrità
  (exit 5) su entrambi gli eseguibili, con il messaggio corrispondente.
  **Cambio di comportamento** per chi discrimina sui codici di uscita.

- **Una passphrase su un backup non cifrato ora è un errore.** L'opener stretto
  impedisce il downgrade *dentro* un backup cifrato; non impedisce che l'intera
  immagine venga sostituita con un backup in chiaro costruito da qualcun altro.
  In quel caso il lettore vedeva `encryption.enabled: false`, lasciava cadere la
  passphrase ricevuta e ripristinava annunciando successo. Chi fornisce
  `--passphrase-file`, `--passphrase-stdin`, `--password`, `--identity` o
  `BACKIMAGE_PASSPHRASE` sta dichiarando cosa si aspetta di leggere, quindi ora
  un backup non cifrato è un errore di integrità (exit 5) su `restore`,
  `verify`, `tar`, `ls`, `find` **e** sull'autoestraente. **Rottura
  deliberata**: chi legge di proposito backup misti in automazione aggiunge
  `--allow-unencrypted`. Senza credenziali nulla cambia.

- **`--overwrite` non cancella più i figli che il backup non contiene.**
  Il flag significa «scrivi sopra ciò che trovi», ma su una directory già
  esistente veniva eseguito un `RemoveAll` prima di ricrearla: ripristinare un
  solo sottoalbero dentro una destinazione popolata eliminava in silenzio i
  file estranei al backup. Ora la semantica è quella di `tar -x`: directory su
  directory si sovrappongono, file su file viene troncato e riscritto, e la
  rimozione avviene solo quando il tipo dell'oggetto esistente **differisce**
  da quello dell'entry — un symlink o un device non si possono sovrascrivere in
  altro modo. **Cambio di comportamento**: chi contava sulla cancellazione per
  ottenere una destinazione identica al backup deve svuotarla prima.

- **L'estrazione è confinata da descrittori, non da stringhe.** Ogni operazione
  di restore passa ora da un unico `os.Root` aperto sulla destinazione: la
  validazione e l'uso sono la stessa syscall, quindi un componente sostituito
  da un symlink fra l'una e l'altro non può più spostare l'operazione fuori
  dalla destinazione. Prima la validazione restituiva un pathname e ogni
  operazione successiva — creazione, chmod, chown, xattr, timestamp, e la fase
  finale sulle directory — lo risolveva di nuovo. La prova non richiede una
  corsa fra processi: un archivio con una directory `pivot` seguita da un
  symlink `pivot` verso l'esterno, con `--overwrite`, portava il modo della
  directory esterna da `0700` a `0777`. Per ciò che `os.Root` non copre (mknod,
  mkfifo, `utimensat` con `AT_SYMLINK_NOFOLLOW`) si usano le forme `*at` con il
  descrittore della directory contenitrice.

- **Un hardlink può puntare solo a un file di questo restore.** Il nome nel
  campo `Linkname` non passava dai controlli applicati al nome dell'entry:
  `Linkname="../fuori"` dava al restore un secondo nome per un file esterno, e
  la fase dei metadati ne riscriveva owner, permessi e timestamp attraverso
  l'inode condiviso, senza privilegi. Ora il primo nome deve essere un file
  regolare che la corsa ha già scritto, risolto dentro la destinazione.
  **Cambio di fedeltà**: se non lo è — filtrato, tagliato da
  `--strip-components`, o successivo nell'archivio — l'entry viene saltata e
  riportata, mentre prima veniva materializzata come copia leggendo dal disco.

- **Un backslash nel nome di un file non è più un separatore.** `CleanPath`
  sostituiva `\` con `/` su ogni piattaforma, quindi il file Unix `a\b`
  tornava da un restore come la directory `a` contenente `b`. Non era
  un'evasione — dopo la sostituzione `..\..` diventa `../..` e veniva
  rifiutato — era corruzione di dati nel roundtrip. La normalizzazione
  appartiene al percorso Windows, dove quel carattere non è ammesso in un nome.

- **`--continue` non annulla più `--include` e `--exclude`.** Il flag
  sostituiva lo stream selettivo con quello tollerante ai chunk danneggiati,
  che non conosceva alcuna selezione, e nello stesso momento diceva
  all'estrattore che lo stream era già filtrato: `--continue --include
  '**/*.pdf'` estraeva l'intero backup, e con `--overwrite` lo scriveva sopra
  la destinazione. Tolleranza e selezione sono ora indipendenti e si combinano
  (`Backup.StreamSelectedTarPartial`), sia verso il filesystem sia verso il
  tar; `--strip-components` continua ad applicarsi.

- **L'estrattore Windows è stato riscritto.** Erano novanta righe che
  ignoravano del tutto `--include`, `--exclude` e `--strip-components` — un
  restore selettivo produceva l'intero backup, in silenzio, su ogni host
  Windows — e che trasformavano ogni entry di tipo sconosciuto in un file
  regolare vuoto: hardlink, device e fifo arrivavano come zero byte senza
  errore. Non c'era alcun confinamento. Ora le regole di selezione sono le
  stesse del percorso Unix, il traversal è ancorato a `os.Root` (che su Windows
  copre anche le junction), e ciò che Windows non può contenere — device, fifo,
  nomi con `\ : * ? " < > |` o con spazi e punti finali — viene riportato come
  saltato invece di essere inventato.

### Added

- **`--expect-digest sha256:…` sui comandi di lettura del binario host**
  (`restore`, `verify`, `ls`, `find`, `inspect`). Ancora la lettura a un digest
  ottenuto **fuori banda**: se l'immagine che il riferimento risolve non ha
  quel digest, il comando esce con codice 5 **prima** di leggere la passphrase
  o il file di identità, quindi la credenziale non arriva a un'immagine che non
  è quella richiesta. Il valore confrontato è quello che la sorgente dichiara
  per l'oggetto risolto — il descrittore del registry, l'indice della layout
  OCI, l'immagine del daemon — mai un digest ricalcolato sull'oggetto già
  scelto. Il flag **non** esiste nell'autoestraente: un programma dentro
  l'immagine non può autenticare l'immagine che lo contiene, e fingerlo
  sarebbe teatro.
- **`--platform` conta anche con `--oci-layout`.** Era accettato e ignorato: la
  layout veniva letta sempre come `linux/amd64`.
- **`--forward-static-token` su `backup`.** Consenso esplicito a inviare al
  server remoto una credenziale che non è una delega limitata. Il comando lo
  dichiara su stderr quando succede: il server riceve una credenziale
  dell'intero account, non una delega a questo repository, e la tiene per la
  finestra dichiarata (un'ora) invece che per una durata scelta dal registry.
- **Fixture congelate dei formati rilasciati** in `pkg/recovery/testdata/`:
  un backup completo — manifest, chunks, indice, blob private, keyfile age e
  layer dati — per `schema1-plain`, `schema2-encrypted` e `legacy-envelope1`
  (envelope v1, scritto fino alla 0.2.3). `format_compat_test.go` le apre, le
  elenca, le ripristina e le verifica tutte. Servono perché il giorno in cui
  il writer cambia nessuna build sa più produrre i byte vecchi: un test di
  compatibilità che genera il proprio input smette di coprire qualcosa senza
  dirlo. `scripts/make-format-fixtures.sh` le rigenera, e la fixture legacy
  esce da un `git worktree` sul tag che la scriveva.
- **`make e2e PHASE=A5`.** Sostituisce l'entrypoint di un'immagine di backup
  con un programma che si limita a scrivere su file la passphrase ricevuta —
  la perdita che nessun controllo interno all'immagine può impedire — e poi
  verifica che il binario host, ancorato al digest onesto, la rifiuti con
  codice 5. Che il segreto non sia stato letto è provato da una fifo senza
  scrittori: la corsa ancorata esce subito, la corsa di controllo senza
  `--expect-digest` si blocca su quella fifo. Verifica inoltre il restore in
  profilo confinato (nessuna rete, nessuna capability, filesystem in sola
  lettura, utente non privilegiato), che l'ownership degradi in modo
  dichiarato invece di essere finta, e — dove l'ambiente lo consente — la
  fedeltà completa su ownership, xattr `trusted.*` e device node nel profilo
  privilegiato. Nessuno script e2e monta il socket del daemon, e lo script
  stesso lo verifica per tutti.
- **`make e2e PHASE=A4`.** Un server remoto ostile (`greedyremote`) che chiede
  credenziali per un altro repository e per `delete`, e una sessione con un
  bearer statico con e senza il consenso esplicito.
- **`make e2e PHASE=A3`.** Costruisce un backup con nonce convergente i cui
  chunk hanno tutti la stessa dimensione memorizzata, sposta un chunk
  validamente sigillato su un'altra posizione riparando ogni numero pubblico,
  e verifica che il restore esca 5 avendo scritto **esattamente** i byte dei
  chunk precedenti — non uno in più. Misura poi la memoria residente di un
  recupero parziale su una entry da 1 GiB e controlla che nessun layer
  materializzato sopravviva al restore.
- **`restore --extract` dichiara quanto aveva già scritto quando si
  interrompe.** Un errore di integrità a metà stream lasciava sul posto i file
  prodotti dai chunk precedenti senza dirlo: ora una riga di log li conta e
  nomina la destinazione. L'atomicità garantita resta quella del singolo
  chunk; quella del file e quella dell'intero restore no, ed è ora scritto
  invece che deducibile.
- **`restore --extract --json` riporta `skipped` e `skipped_reasons`.** Le
  entry che l'estrattore non ha potuto scrivere finivano solo in una riga di
  attenzione su stderr: un'automazione non poteva accorgersi che il restore era
  incompleto.
- **Job CI su `windows-latest` e `macos-latest`.** I tre job esistenti erano
  tutti `ubuntu-latest`, e un verde su Linux non dice nulla dell'estrattore
  Windows: finché non è esistito questo job, il restore selettivo lì era rotto
  e nessun gate poteva accorgersene.
- **`make e2e PHASE=A2`.** Roundtrip di nomi ostili attraverso un backup reale,
  gruppi di hardlink, e una destinazione che tenta di dirottare il restore
  fuori da sé.
- **`version` nell'autoestraente.** Stampa versione e commit dell'estrattore
  incorporato senza toccare il backup e senza credenziali.
- **`make vuln`.** Esegue `govulncheck ./...` da solo.
- **`make e2e PHASE=A1`.** Costruisce un backup cifrato reale, ne riscrive
  dati, indice e blob privato come envelope in chiaro riparando ogni numero
  pubblico che li descrive, li ripubblica come layout OCI e su registry, e
  verifica il rifiuto sul binario host e sull'autoestraente per ogni comando di
  lettura — più i casi di `--overwrite` e di `--continue` con i filtri.
- **`--allow-unencrypted`** su tutti i comandi di lettura del binario host e
  sull'autoestraente: accetta un backup non cifrato anche quando è stata
  fornita una credenziale.

### Fixed

- **Un backup su Windows non archiviava più niente.** `readMeta` restituiva un
  errore ogni volta che veniva chiesto di preservare gli attributi estesi — che
  è il default — e ogni entry finiva scartata: l'archivio usciva vuoto, con
  `Files: 0`, e il `verify` successivo riportava «archive/tar: invalid tar
  header». Windows non ha attributi estesi POSIX: chiederli non è un errore, non
  c'è nulla da perdere. Ora il writer lo dichiara una volta sola fra i
  `Warnings` e archivia tutto il resto.
- **Su macOS il writer perdeva hardlink, device e i tempi di accesso e di
  cambio.** `fileIdentity`, `unixFileDevice` e `statTimes` avevano una sola
  implementazione reale, quella Linux, e fuori da Linux restituivano «non
  disponibile»: tre hardlink allo stesso inode venivano archiviati come tre
  copie del contenuto, il numero di device andava perso e `atime`/`ctime`
  uscivano azzerati. I campi di `Stat_t` hanno nomi e ampiezze diverse su
  darwin (`Atimespec`, `Dev` con segno, `Nlink` a 16 bit), che è l'unico motivo
  per cui servono file separati.
- **Un attributo esteso illeggibile non fa più sparire il file.** Se
  `llistxattr` falliva su una entry — su macOS succede con un file in modo
  `0000` — l'errore risaliva come errore di metadati e l'intera entry veniva
  scartata. Ora, senza `--strict`, la perdita è contata (`XattrsSkipped`) e la
  entry resta: perdere un attributo è un rapporto di fedeltà, perdere il file è
  perdita di dati.
- **`Lgetxattr` riconosce l'assenza di un attributo anche fuori da Linux.** Il
  controllo era su `ENODATA`; i BSD, macOS compreso, rispondono `ENOATTR`, che
  è un errno diverso, e un attributo elencato ma sparito nel frattempo
  diventava un errore.
- **Il recupero parziale non tiene più in memoria l'entry più grande del
  backup.** `readRange` raccoglieva l'intera entry prima di scriverla, per
  poterla scartare intera se un chunk era danneggiato: una entry da 1 GiB
  costava oltre 2 GiB residenti (misurati). Ora il range viene percorso due
  volte — la prima per provare che tutti i chunk si caricano, la seconda per
  scrivere — e la memoria è quella di un chunk. La garanzia è invariata:
  l'entry si scrive intera o non si scrive.
- **Leggere un chunk non rilegge più il layer dall'inizio.** I chunk di un
  layer sono concatenati in un solo file e raggiungere il chunk *i* significa
  arrivare alla somma delle dimensioni che lo precedono: veniva fatto
  scartando quei byte, quindi un restore completo costava n²/2 letture (circa
  32 GiB per consegnare 1 GiB su un layer da 64 chunk). Ora, quando la
  sorgente è posizionabile — `*os.File`, cioè il backup locale e
  l'autoestraente — è una `Seek`; lo scarto resta come ripiego.
- **Un layer che la cache non può tenere viene ricostruito una volta per
  layer, non una per chunk.** Con `--cache-size 0`, o per un layer più grande
  della cache, il file temporaneo veniva cancellato dopo **un** chunk: ogni
  chunk successivo riscaricava e ridecomprimeva l'intero layer. Un
  `--oci-layout` non tiene mai una cache dei layer, quindi prendeva sempre il
  caso peggiore. Il temporaneo ora dura quanto il layer, con un tetto di layer
  vivi per i percorsi selettivo e parziale che saltano fra entry, e viene
  rimosso alla chiusura della sorgente.
- **`make proto-check` distingue tre esiti.** Toolchain assente →
  `SKIP` con exit 0, così `make check` resta eseguibile su una macchina senza
  `protoc`; generato non aggiornato → rosso, come prima; in CI
  `BACKIMAGE_REQUIRE_PROTOC=1` rende l'assenza un errore, così il controllo non
  può sparire in silenzio. Lo script risolve ora `protoc-gen-go` anche da
  `$(go env GOPATH)/bin`.

### Advisory note raggiungibili

`govulncheck` continua a segnalarle perché i moduli sono nel grafo, ma nessun
percorso di chiamata da backimage le raggiunge:

| Advisory | Modulo | Dove | Corretta in |
| --- | --- | --- | --- |
| GO-2026-5158 | `go.opentelemetry.io/otel` v1.41.0 | package importato | v1.42.0 |
| GO-2026-6355 | `golang.org/x/crypto` v0.54.0 | modulo richiesto | v0.56.0 |
| GO-2026-6354 | `golang.org/x/crypto` v0.54.0 | modulo richiesto | v0.56.0 |
| GO-2026-6303 | `golang.org/x/crypto` v0.54.0 | modulo richiesto | v0.55.0 |
| GO-2026-5932 | `golang.org/x/crypto` v0.54.0 | modulo richiesto | nessuna |

## [0.4.0] - 2026-08-24

### Added

- **`contrib/backimage-backup.sh`.** Wrapper per i backup schedulati da cron o
  da un timer systemd: lock `flock` che impedisce due esecuzioni sovrapposte
  dello stesso job, un log per esecuzione con rotazione, `BI_TIMEOUT`,
  `nice`/`ionice`, hook pre e post, retention opzionale con `repo prune` dopo
  un backup riuscito, e notifica dell'esito su webhook Slack o Google Chat in
  policy `always` o `on-error`. Il messaggio porta la descrizione del job,
  riferimento e digest pubblicati, byte sorgente/memorizzati/caricati, durata,
  e per un fallimento l'exit code interpretato con la coda del log. La
  configurazione sono variabili `BI_*` lette da un file e dall'ambiente, dove
  l'ambiente vince; `contrib/backimage-backup.env.example` le elenca tutte.
  Documentazione in `contrib/README.md` e `docs/cron.md`.

## [0.3.2] - 2026-08-23

### Added

- **`listen-remote --upload-chunk-size`.** Espone la dimensione del chunk
  `PATCH` verso il registry. Il default `0` invia ogni blob che il server ha su
  disco in **una sola richiesta streamata**, come già faceva `backup`; serve
  solo per un registry che rifiuta corpi grandi (un 413 fa comunque ricadere il
  push sul chunking da solo).
- **`listen-remote --push-jobs`.** Numero di blob caricati in parallelo dentro
  un singolo push, prima ricavato da `--max-sessions`.

### Fixed

- **Un backup remoto su TCP moriva dopo 120 secondi.** `IdleTimeout` veniva
  applicato come deadline assoluta al momento della `Dial` e nessuno la
  rinfrescava lato client (il server lo faceva a ogni frame): qualunque backup
  più lungo del timeout falliva con `i/o timeout`, e i cinque retry morivano
  allo stesso punto. Adesso la deadline avanza a ogni byte trasferito, quindi
  significa davvero "inattivo". QUIC non era interessato.
- **Una singola connessione silenziosa bloccava il server.** L'handshake TLS
  (e, per QUIC, l'attesa dello stream di sessione) avveniva dentro il loop di
  accept, senza deadline sulla connessione grezza: un peer che apriva il socket
  e taceva impediva ogni nuova sessione finché voleva. L'handshake ora avviene
  fuori dal loop, con un budget di 10 secondi e al massimo 64 peer in
  handshake insieme.
- **Il token del registry di un client era raggiungibile dalla sessione di un
  altro.** `listen-remote` creava un unico `TokenBroker` condiviso da tutte le
  sessioni, con le chiavi per repository: due client che spingono sullo stesso
  repository si scambiavano le credenziali. Ogni sessione ha ora il proprio
  sink e il proprio broker (`server.Config.NewSink`).
- **Due sessioni streaming concorrenti si corrompevano lo spool a vicenda.** Il
  file era nominato sull'indice del layer (`backimage-stream-000000.blob.tmp`)
  dentro un `--work-dir` condiviso: la seconda sessione troncava il layer che
  la prima stava ancora caricando, e il backup veniva pubblicato corrotto senza
  errori. Il nome è ora unico per spool.
- **Il server smetteva di parlare proprio quando era occupato.** Lo
  `StreamProgress` era emesso all'arrivo di un frame: durante un upload lento
  il client non sentiva più nulla e chiudeva una sessione che stava
  progredendo. Ora è su timer (`ProgressInterval`, minimo 50 ms), e vale da
  keepalive del server.
- **Il preflight dello spazio temporaneo sottostimava di ordini di
  grandezza.** Richiedeva `jobs × max-layer-size`, ma la costruzione di un
  layer libera lo spool e *conserva* il blob OCI prodotto: tutti i layer
  restano in `--temp-dir` fino alla fine del push. Misurato su una sorgente da
  1 GiB incomprimibile con `--jobs 1 --max-layer-size 64MiB`: preflight 64 MiB,
  picco reale 1024 MiB. Una sorgente da 20 GiB su un disco da 5 GiB passava il
  controllo e poi moriva di ENOSPC a metà corsa. Ora il requisito è la
  dimensione del backup compresso e l'errore indica i due rimedi che
  funzionano: `--temp-dir`, oppure `--remote-mode stream` che in locale non
  costruisce nulla (misurato: 4 KiB di spool sul client per lo stesso 1 GiB).
  Ridurre `--max-layer-size` o `--jobs` non abbassa il requisito, e la
  documentazione che lo suggeriva è stata corretta.
- **Una sessione fatta di soli keepalive teneva uno slot per sempre.** La
  deadline di inattività non può accorgersene: un peer che manda un keepalive
  ogni 30 secondi non è mai inattivo. Ora una sessione senza progresso di
  protocollo per 15 minuti viene chiusa con un errore di rete.
- **Il client rifiutava un server che negoziava una versione più bassa.**
  `uploadOnce` pretendeva esattamente la propria versione di protocollo, quindi
  `--remote-mode layers` non parlava con un server v1 nonostante lo scambio
  layer-per-layer sia proprio ciò che un peer v1 capisce.
- **Un backup riuscito poteva essere riportato come fallito.** Il keepalive
  del client scrive per conto suo: se la sua scrittura perdeva la corsa con la
  chiusura della sessione, l'errore finiva in `asyncErr` e il ciclo terminale
  lo consultava *prima* del `BackupEnd` già ricevuto. Ora il `BackupEnd` ha la
  precedenza: quando il server ha pubblicato, un keepalive in ritardo non dice
  niente sull'esito.
- **Il server QUIC non liberava la connessione**, solo lo stream: restava
  appesa fino a `MaxIdleTimeout`. Ora attende che il peer chiuda la sua metà —
  chiuderla subito farebbe scartare il `BackupEnd` non ancora riscontrato — con
  un limite di 5 secondi.

### Changed

- **La ricezione e il push verso il registry si sovrappongono.** Il tail della
  pipeline (digest dello spool, ricostruzione del layer OCI, upload) gira su
  una goroutine separata invece che sul percorso di ricezione, dove fermava il
  client per tutta la durata dell'upload. Il passaggio di consegne non è
  bufferizzato: un solo layer in volo, ed è questa la contropressione che
  impedisce a un client veloce di riempire il disco.
  **`--work-dir` richiede ora `3 × --max-layer-size × --max-sessions`** invece
  di `2 ×`: lo spool in riempimento, quello in upload e il suo blob OCI
  ricostruito.
- Il client streaming usa un doppio buffer (`remote.FrameBuffer`) al posto di
  un `bufio.Writer`: la scansione del filesystem riempie un frame mentre il
  precedente è sul filo, invece di fermarsi a ogni invio.

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