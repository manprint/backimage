# backimage

`backimage` archivia, comprime e cifra file dentro immagini OCI multi-arch.
L’immagine risultante è anche un programma auto-estraente: per un restore
semplice può essere eseguita con `docker run`, senza dover installare il
programma nel computer di destinazione.

## Installazione

### Binario precompilato

Le release `vX.Y.Z` pubblicano gli archivi per Linux, macOS e Windows e
l’immagine container `ghcr.io/manprint/backimage`. Scaricare l’archivio della
propria piattaforma, installare il binario e verificare la versione:

```console
tar -xzf backimage_*.tar.gz
sudo install -m 0755 backimage /usr/local/bin/backimage
backimage version
```

### Da sorgente

Richiede Go 1.26 o superiore:

```console
go install github.com/manprint/backimage/cmd/backimage@latest
# oppure, dentro il checkout:
make build
```

### Immagine Docker

```console
docker pull ghcr.io/manprint/backimage:latest
docker run --rm ghcr.io/manprint/backimage:latest version
```

È possibile creare un backup anche eseguendo la CLI nell'immagine Docker. In
questo esempio viene prodotto un OCI layout locale:

```console
mkdir -p ./backup-layout
docker run --rm \
  -v "$PWD/data:/data:ro" \
  -v "$PWD/backup-layout:/out" \
  ghcr.io/manprint/backimage:latest \
  backup /data --repo example.invalid/team/backup --tag daily \
  --output oci-layout --output-path /out --password mypassword
```

## Modello operativo

Un backup è un riferimento OCI (`registry/immagine:tag`) composto da manifest,
metadati, layer dati e binario auto-estraente. Di default i contenuti sono
cifrati con una passphrase, compressi con zstd e suddivisi in layer da 1 GiB.
Il restore verifica i digest dei chunk prima di scrivere i dati.

Le modalità di output di `backup` sono:

| Modalità | Flag | Uso |
| --- | --- | --- |
| Registry OCI | `--output registry` (default) | Pubblica su `--repo` |
| Docker daemon | `--local-repo` oppure `--output daemon` | Carica nell’engine locale |
| OCI layout | `--output oci-layout --output-path DIR` | Salva in una directory OCI |
| Tar | `--output tar --output-path FILE` | Scrive un artefatto locale |
| Server remoto | `--remote HOST:PORT` | Delega il backup a `listen-remote` |

### Modalità remota: cosa viene delegato

`--remote-mode` sceglie il protocollo; il default è `stream` (v2).

| Fase | `stream` (default) | `layers` (legacy v1) |
| --- | --- | --- |
| scansione e tar | client | client |
| chunking, compressione, cifratura | **server** | client |
| spool temporaneo e layer OCI | **server** (un layer alla volta in `--work-dir`) | client |
| `HEAD`/upload blob, manifest/index | server | server |
| spazio su disco del client | indipendente dalla dimensione del backup | uno spool per layer concorrente |
| il server vede il plaintext | **sì** | no |

Con `stream` il client invia solo il flusso tar: un backup da 50 GiB gira con
~1 GiB libero perché non esiste né l'archivio completo né un layer locale. Il
server assembla un layer per volta (serve circa `2 × --max-layer-size` di spazio
temporaneo) e lo carica in streaming nel registry.

`--server-side-compress` è ora un alias accettato di `--remote-mode stream`
(già il default) e viene rifiutato con `--remote-mode layers`, dove sarebbe una
promessa falsa.

Nota di sicurezza: in modalità `stream` la passphrase non lascia mai il client
(i file `keys.age`/`keys.pass.age` sono prodotti in locale), ma il server riceve
la DEK e i dati in chiaro perché è lui a cifrarli. Se il ricevente non deve
vedere il plaintext, usare `--remote-mode layers`.

Le credenziali del registry restano sul client. Il client esegue `backimage
login` (o usa il proprio auth file), riceve dal registry token bearer con scope
limitato e li consegna temporaneamente al server attraverso il canale TLS. Il
server non richiede `backimage login` e non conserva password o credenziali
permanenti; deve però poter raggiungere il registry in rete.

## Compressione e protezione con password

La pipeline è, in ordine, **archivio → chunk → compressione → cifratura →
layer OCI**. La compressione riduce i dati prima della cifratura; cifrare prima
renderebbe la compressione praticamente inefficace. Il default è `zstd` con il
livello predefinito del codec (`--compression-level 0`), attualmente zstd l2,
una buona scelta per velocità, dimensioni e interoperabilità. Il valore `0` è
solo un sentinel dell'opzione: `info` riporta il livello effettivamente usato.
Il livello 6 è il default di gzip, non di zstd; zstd supporta i livelli 1..4.

| Dati sorgente | Esempio | Scelta consigliata |
| --- | --- | --- |
| Testo, log, sorgenti, database dump | `--compression zstd` | Default; usare `--compression-level 4` se si privilegia il rapporto |
| Immagini, video, zip, PDF già compressi | `--compression none` oppure `lz4` | Evita di spendere CPU per ottenere poco spazio |
| Massimo supporto nei runtime OCI | `--compression gzip` | Compatibile con i runtime OCI comuni; `--compression-level 6` è il default del codec |
| Archivio per banda molto limitata | `--compression xz --runnable=false` | Buon rapporto su testo, ma più lento e non eseguibile direttamente con `docker run` |

Esempi eterogenei:

```console
# Default: zstd, cifrato, immagine eseguibile.
backimage backup /srv/logs --repo ghcr.io/acme/backup --tag logs-daily

# File già compressi: copia senza ulteriore compressione.
backimage backup ./video.mp4 ./photos.zip \
  --repo ghcr.io/acme/backup --tag media \
  --compression none

# Livello zstd più alto per un dump testuale archiviato a lungo termine.
backimage backup ./database.sql --repo ghcr.io/acme/backup --tag db \
  --compression zstd --compression-level 4

# gzip per un'immagine leggibile da più runtime OCI possibile.
backimage backup ./release.tar --repo ghcr.io/acme/backup --tag release \
  --compression gzip --compression-level 6
```

`xz` e `lz4` usano media type OCI non standard: per questi codec occorre
`--runnable=false` e l'immagine va trattata come artefatto da ripristinare con
la CLI. Con il valore predefinito `--runnable`, preferire `zstd`, `gzip` o
`none` quando si vuole anche `docker run IMAGE`.

La protezione del backup è una passphrase age e **non** coincide con la
password del registry. La cifratura è attiva per default: scegliere una delle
seguenti modalità per fornire il segreto:

```console
# File protetto: non compare nella command line né nella lista dei processi.
umask 077
printf '%s\n' 'una-passphrase-lunga-e-unica' > backup.pass
chmod 600 backup.pass
backimage backup /srv/data --repo ghcr.io/acme/backup --tag daily \
  --passphrase-file ./backup.pass

# Stdin: utile in CI o quando il secret manager alimenta una pipe.
printf '%s\n' "$BACKUP_PASSPHRASE" | \
  backimage backup /srv/data --repo ghcr.io/acme/backup --tag ci \
    --passphrase-stdin

# Diretto: semplice, ma la password resta nella history e nella lista processi.
backimage backup /srv/data --repo ghcr.io/acme/backup --tag quick \
  --password mypassword

# Cifratura a chiave pubblica age; per il restore servirà la chiave privata.
backimage backup /srv/data --repo ghcr.io/acme/backup --tag age \
  --recipient 'age1...'
backimage restore ghcr.io/acme/backup:age --extract -C ./restore \
  --identity ./age-identity.txt
```

Per un backup cifrato, la stessa passphrase o identità va conservata per il
restore. Non esiste un recupero tramite backdoor: se il segreto è perso, il
contenuto cifrato non è recuperabile. `--no-encrypt` è adatto solo a dati già
protetti da un altro livello e non va confuso con la password del login:

```console
# Solo per dati pubblici o cifrati altrove; il contenuto dell'immagine è in chiaro.
backimage backup ./public-manifest.json --repo ghcr.io/acme/public \
  --no-encrypt

# Login al registry: il token non viene esposto negli argomenti.
printf '%s\n' "$REGISTRY_TOKEN" | backimage login ghcr.io \
  --username acme --password-stdin
```

## Autenticazione dei registry

La password o il token del registry servono per leggere o pubblicare
l'immagine OCI. Non sono la passphrase del backup: la prima autentica il
registry, la seconda cifra e decifra i dati.

### Login, elenco e logout

Usare `--password-stdin` per non esporre il segreto nella lista dei processi.
Per Docker Hub è consigliato un Personal Access Token (PAT), soprattutto se
l'account usa la 2FA:

```console
# Login a Docker Hub.
printf '%s\n' "$DOCKERHUB_PAT" | \
  backimage login docker.io --username demoarchiveuser --password-stdin

# Login a un secondo registry: i due login convivono.
printf '%s\n' "$GHCR_PAT" | \
  backimage login ghcr.io --username demoarchiveuser --password-stdin

# Mostra i login configurati (provider, account, utente locale), mai i segreti.
backimage login --list
backimage login --list --json

# Rimuove il login per un registry.
backimage logout ghcr.io
```

Più account sullo stesso provider convivono: vedi
[Login multipli](#login-multipli-più-account-sullo-stesso-registry).

`--token TOKEN` è un'alternativa quando si dispone già di un bearer token.
`--password TOKEN` funziona, ma il segreto è visibile nella lista dei processi
e quindi va evitato.

### Dove vengono salvate le credenziali

Il file usato da `backimage` è scelto in questo ordine:

1. `BACKIMAGE_AUTH_FILE`, se impostata;
2. `$XDG_CONFIG_HOME/backimage/auth.json`, se `XDG_CONFIG_HOME` è impostata;
3. `$HOME/.config/backimage/auth.json`.

Esempio per scegliere esplicitamente il file:

```console
export BACKIMAGE_AUTH_FILE="$HOME/.config/backimage/auth.json"
backimage login --list
```

Il file usa il formato Docker `auths`, viene scritto atomicamente e ha permessi
`0600`; viene rifiutato se è leggibile da gruppo o da altri utenti. Il primo
account di un provider è salvato sotto la chiave host (compatibile con Docker),
gli account nominati aggiuntivi sotto `host#username`. Un bearer token
host-wide usa invece un'identità interna riservata, distinta da ogni account
nominato. È un file locale compatibile con l'autenticazione Docker, non un
password manager cifrato: proteggerlo e non inserirlo in repository, immagini
o backup pubblici.

`backimage` cerca prima il proprio file. Se non trova una credenziale valida,
usa la configurazione Docker e gli eventuali credential helper come fallback.
Per questo `docker login` e `backimage login` possono riferirsi a file diversi;
un login Docker riuscito non sostituisce automaticamente una vecchia
credenziale presente nello store di `backimage`.

### Login multipli: più account sullo stesso registry

Ogni `--username` è un account separato: tre utenti Docker Hub convivono nello
stesso file e nessun login sovrascrive gli altri.

```console
printf '%s\n' "$PAT_1" | backimage login docker.io --username user1 --password-stdin
printf '%s\n' "$PAT_2" | backimage login docker.io --username user2 --password-stdin
printf '%s\n' "$PAT_3" | backimage login docker.io --username user3 --password-stdin
printf '%s\n' "$GHCR_PAT" | backimage login ghcr.io --username manprint --password-stdin

backimage login --list
```

```text
PROVIDER          ACCOUNT    LOGIN COME   FILE
ghcr.io           manprint   fabio        /home/fabio/.config/backimage/auth.json
index.docker.io   user1      fabio        /home/fabio/.config/backimage/auth.json
index.docker.io   user2      fabio        /home/fabio/.config/backimage/auth.json
index.docker.io   user3      fabio        /home/fabio/.config/backimage/auth.json
```

Le colonne sono: **provider** (registry canonico), **account** sul provider, e
**login come**, cioè l'utente locale proprietario del file di credenziali. La
terza colonna spiega perché `sudo backimage login --list` può mostrare un
elenco diverso dallo stesso comando senza `sudo`: sono due file distinti
(`/root/.config/...` contro `/home/UTENTE/.config/...`). `--json` restituisce
gli stessi campi più `authFile`.

#### Bearer token host-wide: account `token`

`backimage login REGISTRY --token TOKEN` salva un bearer token già emesso, che
non porta uno username del registry. La sua identità pubblica è `token`: viene
mostrata da `login --list` e negli errori di selezione, e può essere usata sia
con `--registry-user` sia con `logout --user`.

```console
# Il valore passato a --token è visibile nella command line: preferire un
# token breve e non riusabile quando il provider lo consente.
backimage login ghcr.io --token "$REGISTRY_BEARER_TOKEN"

# Se sullo stesso host esistono anche account nominati, scegliere il token.
backimage backup /srv/data --repo ghcr.io/team/dumps --tag daily \
  --registry-user token

# Rimuove solo il token e conserva gli account nominati dello stesso host.
backimage logout ghcr.io --user token
```

Token e login nominati possono essere inseriti in qualunque ordine: usano
chiavi di storage distinte e non si sovrascrivono. Per compatibilità, il primo
account continua a poter occupare la chiave host usata dalle versioni
precedenti; quando deve coesistere con un altro account, il token riceve una
chiave riservata separata. Anche un vecchio token salvato sulla chiave host
resta leggibile.

Quando il token convive con uno o più username, il comando non sceglie
silenziosamente: richiede `--registry-user token` oppure uno username
esplicito. Analogamente, `logout` senza `--user` si ferma se la scelta è
ambigua; `--user token` elimina soltanto la credenziale bearer.

#### Quale account viene usato

Lo decide il **namespace del repository**: `docker.io/user2/myimage` usa il
login `user2`, `ghcr.io/manprint/dumps` usa `manprint`. Nessuna euristica: se
il namespace non corrisponde ad alcun account salvato, il comando si ferma
invece di pubblicare con l'identità sbagliata.

```console
backimage backup /srv/data --repo docker.io/user2/myimage --tag daily   # usa user2
```

```text
$ backimage backup /srv/data --repo ghcr.io/team/dumps
error: no login for ghcr.io matching "team": stored accounts are manprint;
select one with --registry-user NAME, or --registry-user none for an anonymous request
```

`--registry-user` è un flag globale e vale per tutti i comandi che parlano con
un registry (`backup`, `restore`, `inspect`, `ls`, `find`, `verify`, `repo *`):

```console
# Namespace di un'organizzazione, push con l'account personale.
backimage backup /srv/data --repo ghcr.io/team/dumps --registry-user manprint

# Pull pubblico ignorando i login salvati.
backimage restore docker.io/library/alpine:latest --registry-user none -x -C /tmp/x
```

I nomi `docker.io`, `index.docker.io` e `registry-1.docker.io` sono
equivalenti: per Docker Hub rappresentano lo stesso provider.

#### Rimuovere un login

```console
backimage logout docker.io --user user2   # un account
backimage logout docker.io --all          # tutti gli account del provider
backimage logout ghcr.io                  # account unico: nessun selettore
```

Con più account e senza selettore il comando si ferma elencandoli, per non
cancellare credenziali che servivano:

```text
$ backimage logout docker.io
error: 3 logins for index.docker.io: user1, user2, user3; use --user NAME or --all
```

Il logout riguarda il registry, non il singolo repository:
`backimage logout docker.io/user1/mindhunters` non esiste. `backimage repo rm`
elimina invece un manifest dal registry e non tocca le credenziali.

#### File separati (ancora supportati)

Restano utili per isolare completamente i contesti, ad esempio in CI:

```console
BACKIMAGE_AUTH_FILE="$HOME/.config/backimage/dockerhub-a.json" \
  backimage login docker.io --username account_a --password-stdin < pat_a

BACKIMAGE_AUTH_FILE="$HOME/.config/backimage/dockerhub-a.json" \
  backimage backup ./mindhunters --repo docker.io/account_a/mindhunters --tag daily
```

### Docker Hub: il login non garantisce il permesso di push

Il login verifica che il PAT autentichi l'account. Il backup esegue poi un
secondo controllo sul repository esatto e richiede lo scope `pull,push`:

```console
# Repository corretto: deve contenere namespace e nome.
printf '%s\n' "$DOCKERHUB_PAT" | \
  backimage login index.docker.io \
    --username demoarchiveuser --password-stdin

printf '%s\n' "$BACKUP_PASSPHRASE" | \
  backimage backup ./mindhunters \
    --repo docker.io/demoarchiveuser/mindhunters \
    --tag mindhunters-test --passphrase-stdin --allow-degraded
```

Se il backup restituisce `credentials rejected by index.docker.io (401)`,
`--allow-degraded` non è la soluzione: quel flag riguarda solo i privilegi
locali. Controllare che il repository esista nell'account corretto e che il
PAT abbia permesso di scrittura. Per eliminare una credenziale `backimage`
vecchia e ripetere il login:

```console
backimage login --list
backimage logout index.docker.io
printf '%s\n' "$DOCKERHUB_PAT" | \
  backimage login index.docker.io \
    --username demoarchiveuser --password-stdin
```

## Backup di file, cartelle e percorsi misti

La sintassi è `backimage backup <PATH...>`: ogni percorso dopo `backup` è una
radice indipendente. Si possono quindi combinare un singolo file, più file,
cartelle e percorsi assoluti nello stesso backup. Le virgolette proteggono i
glob destinati a `--exclude` dalla shell.

```console
# 1. Un solo file.
backimage backup ./config/app.yaml \
  --repo ghcr.io/acme/backup --tag app-config

# 2. Una cartella completa, ad esempio con log e sottocartelle.
backimage backup /srv/application \
  --repo ghcr.io/acme/backup --tag application

# 3. Più file non contigui.
backimage backup ./compose.yaml ./nginx.conf ./database.sql \
  --repo ghcr.io/acme/backup --tag deployment-files

# 4. Mix di cartelle e file: una directory applicativa più due file di sistema.
backimage backup /var/lib/myapp /etc/myapp/app.conf ./README.md \
  --repo ghcr.io/acme/backup --tag mixed

# 5. Cartella con esclusioni ripetibili.
backimage backup /home/alice \
  --exclude 'home/alice/.cache/**' \
  --exclude 'home/alice/Downloads/*.iso' \
  --repo ghcr.io/acme/backup --tag home
```

Prima di leggere molti dati è utile controllare il piano senza pubblicare
nulla. `--dry-run` non scrive sul registry; `--one-file-system` impedisce a una
directory che contiene mount point di attraversare altri filesystem:

```console
backimage backup / --one-file-system \
  --exclude 'proc/**' --exclude 'sys/**' --exclude 'dev/**' \
  --repo ghcr.io/acme/backup --tag host \
  --dry-run --json
```

I path passati sono anche la base dei nomi archiviati: usare `backimage ls` o
`backimage find` per controllare l'indice prima di un ripristino selettivo.

## Estrarre i dati dall'immagine

### Esempio completo con Docker Hub

Nell'esempio seguente `./mindhunters` è il percorso **locale** da archiviare,
mentre `docker.io/demoarchiveuser/mindhunters` è il repository **remoto** su
Docker Hub. Sono due cose diverse:

```text
backimage backup ./mindhunters \
  --repo docker.io/demoarchiveuser/mindhunters \
  --tag mindhunters-test
                         ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
                         repository remoto e tag dell'immagine
```

Il repository dell'immagine deve contenere il namespace dell'utente o
dell'organizzazione (`demoarchiveuser/mindhunters`). Il risultato sarà quindi
la reference completa:

```text
docker.io/demoarchiveuser/mindhunters:mindhunters-test
```

Per Docker Hub è consigliato usare un Personal Access Token (PAT), soprattutto
se l'account ha la 2FA. Il login di `backimage` verifica che le credenziali
siano valide, ma il backup deve anche avere il permesso `push` sul repository
esatto indicato da `--repo`:

```console
# Token Docker Hub, non la passphrase del backup.
printf '%s\n' "$DOCKERHUB_PAT" | backimage login index.docker.io \
  --username demoarchiveuser --password-stdin
backimage login --list

# `./mindhunters` è la directory locale sorgente.
# `--allow-degraded` evita il blocco del preflight dei privilegi; non concede
# accesso a file che l'utente non può leggere.
printf '%s\n' "$BACKUP_PASSPHRASE" | \
  backimage backup ./mindhunters \
    --repo docker.io/demoarchiveuser/mindhunters \
    --tag mindhunters-test \
    --passphrase-stdin \
    --allow-degraded
```

`docker login` e `backimage login` possono usare archivi diversi. `backimage`
cerca prima le proprie credenziali in
`$XDG_CONFIG_HOME/backimage/auth.json` (oppure `$HOME/.config/backimage/auth.json`)
e poi usa la configurazione Docker come fallback. Un login riuscito per il
registry non dimostra quindi che l'account possa fare push su ogni repository.
Se l'account ha accesso solo a `demoarchiveuser/mindhunters`, usare quella
reference completa e non `docker.io/demoarchiveuser`.

#### Errore `credentials rejected ... (401)` durante il backup

Questo errore non viene risolto da `--allow-degraded`: quel flag riguarda solo
il preflight dei privilegi locali. Le cause più comuni sono:

1. `--repo` punta al repository sbagliato: usare
   `docker.io/demoarchiveuser/mindhunters`, non soltanto
   `docker.io/demoarchiveuser`;
2. il PAT è valido, ma l'utente non ha permesso di scrittura su quel repository;
3. è stato eseguito `docker login`, ma `backimage` sta usando una vecchia
   credenziale salvata nel proprio store;
4. il login è stato eseguito come un altro utente o con una configurazione
   Docker diversa.

Per rinnovare esplicitamente la credenziale usata da `backimage`:

```console
backimage login --list
backimage logout index.docker.io
printf '%s\n' "$DOCKERHUB_PAT" | backimage login index.docker.io \
  --username demoarchiveuser --password-stdin
```

Il login riuscito prova solo che il PAT autentica l'account; il successivo
preflight di push verifica anche lo scope `pull,push` sul repository indicato.
Su Docker Hub il repository deve esistere nell'account corretto oppure
l'account deve avere un permesso equivalente tramite organizzazione/team.

### Con la CLI installata

`restore --extract` materializza direttamente i file; senza `--extract`,
`restore` produce invece un tar. Gli include e gli exclude possono essere
ripetuti e sono utili per estrarre solo una parte di un backup:

```console
# Download dei layer dal registry e estrazione diretta nella directory locale.
mkdir -p ./restore
printf '%s\n' "$BACKUP_PASSPHRASE" | \
backimage restore docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  --extract --destination ./restore \
  --passphrase-stdin

# Solo PDF sotto documents, escludendo i temporanei.
printf '%s\n' "$BACKUP_PASSPHRASE" | \
backimage restore docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  --extract --destination ./documents \
  --include 'documents/**/*.pdf' \
  --exclude 'documents/**/tmp/**' \
  --no-preserve-owner --passphrase-stdin

# Download dei layer e salvataggio del tar, senza estrarlo.
printf '%s\n' "$BACKUP_PASSPHRASE" | \
backimage restore docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  --output ./mindhunters-test.tar --passphrase-stdin

# Sorgente locale invece del registry.
backimage restore --oci-layout ./oci-layout \
  --extract --destination ./restore-local \
  --passphrase-stdin < ./backup.pass

# Diretto: semplice, ma la password resta nella history e nella lista processi.
backimage restore docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  --extract --destination ./restore --password mypassword
```

Tips:

- aggiungi `--no-preserve-owner` se non vuoi ripristinare ownership e gruppi;
- aggiungi `--cpus N` per limitare la CPU usata dall'estrazione;
- aggiungi `--remove-local-image` per rimuovere l'immagine locale dopo un
  restore riuscito;
- aggiungi `--include GLOB` e/o `--exclude GLOB` per un restore selettivo;
- aggiungi `--overwrite` se la directory di destinazione non è vuota.

`--remove-local-image` rimuove la reference locale dal Docker daemon solo
dopo che il restore è terminato senza errori. Richiede che l'host esponga il
Docker socket (`DOCKER_HOST` o `/var/run/docker.sock`). Se il restore fallisce,
l'immagine non viene rimossa. `--cpus N` limita il budget CPU del restore;
senza il flag il valore predefinito è metà dei processori disponibili, con
minimo uno. Il limite viene applicato anche quando l'archivio usa gzip, lz4 o
un altro algoritmo: per decoder non paralleli non crea parallelismo aggiuntivo,
ma limita comunque il runtime Go dell'operazione.

Durante backup e restore, ogni riga diagnostica/progressiva su stderr inizia
con un timestamp locale nel formato `YYYY-MM-DDTHH:MM:SS.mmm±HH:MM`. Questo
permette di distinguere il tempo passato in una fase anche quando non arrivano
nuovi byte. Le fasi del backup comprendono scansione sorgenti, piano dei
chunk/layer, archiviazione-compressione-cifratura, preparazione OCI, controllo
e upload dei blob, pubblicazione dei manifest e completamento. Nel restore
sono indicati apertura della sorgente, caricamento di manifest e tabella dei
chunk, apertura del file chiavi, derivazione della chiave passphrase con scrypt,
sblocco, lettura-decrittazione-decompressione di ogni chunk, verifica digest,
scrittura filesystem e finalizzazione dei metadati delle directory. La
derivazione scrypt è volutamente CPU-intensive, non è decompressione e viene
eseguita una sola volta per restore; i timestamp rendono visibile anche questa
fase iniziale senza nuovi byte estratti.

`restore --extract` stampa inoltre l'avanzamento in byte e percentuale; anche
l'immagine auto-estraente mostra gli stessi eventi con `docker run`. Gli
aggiornamenti byte sono periodici per non riempire il terminale e il 100% viene
emesso al termine logico del tar. Il riepilogo finale resta su stdout. Con
`--json`, il JSON non viene mescolato ai messaggi di progresso.

Per vedere i dati senza estrarli, usare l'indice: `inspect --files` sblocca
l'immagine e `ls`/`find` elencano i path. `inspect --layers` e `verify --quick`
invece leggono solo i metadati pubblici e non richiedono la passphrase;
`verify` completo è preferibile prima di un restore.

```console
backimage inspect docker.io/demoarchiveuser/mindhunters:mindhunters-test --files \
  --passphrase-file ./backup.pass
backimage ls docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  --long --include 'documents/**' \
  --passphrase-file ./backup.pass
backimage verify docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  --passphrase-file ./backup.pass
```

### Senza installare la CLI sul computer di destinazione

Un'immagine runnable contiene il programma auto-estraente. Il computer di
destinazione non deve installare `backimage`: basta Docker o un runtime OCI.
Per scrivere su una directory dell'host, usare un bind mount:

```console
mkdir -p ./restore
docker pull docker.io/demoarchiveuser/mindhunters:mindhunters-test
docker run --rm \
  -e BACKIMAGE_PASSPHRASE="$BACKUP_PASSPHRASE" \
  -v "$PWD/restore:/restore" \
  docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  extract --out /restore

# Diretto: semplice, ma la password resta nella history e nella lista processi.
docker run --rm -v "$PWD/restore:/restore" \
  docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  extract --out /restore --password mypassword
```

Tips:

- aggiungi `--no-preserve-owner` a `extract` se non vuoi ripristinare ownership
  e gruppi;
- aggiungi `--cpus N` a `extract` per limitare la CPU;
- per `--remove-local-image`, aggiungi a `docker run`:
  `-e BACKIMAGE_IMAGE_REF="docker.io/demoarchiveuser/mindhunters:mindhunters-test"`,
  `-v /var/run/docker.sock:/var/run/docker.sock` e a `extract`
  `--remove-local-image`;
- aggiungi `--include GLOB`, `--exclude GLOB` o `--overwrite` quando servono.

Si può anche estrarre un tar e affidare la materializzazione agli strumenti
standard del sistema. Questo è il caso più fedele su Linux quando si devono
ripristinare ownership, ACL, xattr, device e permessi speciali:

```console
docker run --rm -i \
  -e BACKIMAGE_PASSPHRASE="$BACKUP_PASSPHRASE" \
  docker.io/demoarchiveuser/mindhunters:mindhunters-test tar \
  > mindhunters-test.tar

mkdir -p ./restore
tar -xpf mindhunters-test.tar -C ./restore \
  --no-same-owner --no-same-permissions
```

In modalità diretta Docker scarica l'immagine con `docker pull` oppure
automaticamente al primo `docker run`; non serve installare `backimage` sul
computer di destinazione. Il comando `extract` dell'immagine è il self-
extractor incorporato e supporta anche `--include`, `--exclude`,
`--strip-components` e `--no-preserve-owner`. Per usare
`--remove-local-image` servono anche `BACKIMAGE_IMAGE_REF` e il mount del
Docker socket mostrati nei Tips; il flag forza la rimozione dell'immagine
solo dopo un'estrazione riuscita.

Per ispezionare senza copiare file:

```console
docker run --rm ghcr.io/acme/backup:daily info
docker run --rm \
  -e BACKIMAGE_PASSPHRASE="$BACKUP_PASSPHRASE" \
  ghcr.io/acme/backup:daily list -l --include 'documents/**'
```

Un layer cifrato o compresso con il formato proprietario di `backimage` non è
estraibile con il solo `tar`: occorre `backimage` oppure il self-extractor
incorporato nell'immagine. Dopo aver ottenuto il tar, invece, non serve più la
CLI. Il comando `tar` scrive dati binari su stdout, quindi va sempre
reindirizzato o collegato a un altro comando, mai lasciato su un terminale.

## Permessi dei file sorgenti e uso di `sudo`

Il backup legge i file con i permessi dell'utente che esegue il comando. Per
una directory privata o per file già leggibili è preferibile l'utente normale:
mantiene le credenziali del registry, usa la propria cache/checkpoint e riduce
il rischio di leggere più dati del necessario.

Usare `sudo` (o eseguire il processo come root) quando il backup deve includere
file non leggibili, directory senza permesso di attraversamento, ACL/xattr
protetti o metadati di sistema. Il root può leggere più contenuto, ma non
risolve automaticamente problemi di mount, filesystem remoto o policy MAC
(SELinux/AppArmor). Se alcuni file devono restare esclusi, è meglio usare
`--exclude` invece di elevare l'intero processo.

`sudo` usa normalmente la configurazione e la home di root: il login del
registry e la cache dell'utente corrente non diventano automaticamente
disponibili. Eseguire quindi il login come root, oppure passare esplicitamente
un file credenziali/configurazione accessibile a root (per esempio tramite
`BACKIMAGE_AUTH_FILE` e `--config`) e usare sempre un `--passphrase-file`
leggibile dal processo privilegiato.

| Situazione | Comando/strategia | Risultato |
| --- | --- | --- |
| File dell'utente leggibili | `backimage backup ./project ...` | Nessun `sudo`; scelta consigliata |
| `/etc` o `/var/lib` con dati di sistema | `sudo backimage backup /etc/myapp /var/lib/myapp ...` | Include i file protetti e i relativi metadati |
| Preflight dei privilegi non disponibile | `--allow-degraded` | Continua senza il preflight strict; non concede accesso ai file illeggibili |
| Ripristino in una directory dell'utente | `backimage restore --extract -C ./restore --no-preserve-owner ...` | L'utente conserva i file; ownership originale non applicata |
| Ripristino fedele del sistema Linux | `backimage restore -o system.tar ...` poi `sudo tar -xpf ...` | Preserva ownership numerica, mode, ACL/xattr e device quando supportati |

Esempi:

```console
# Utente normale: progetto e configurazione leggibili dall'utente corrente.
backimage backup "$HOME/project" ./compose.yaml \
  --repo ghcr.io/acme/backup --tag developer

# Root: sorgenti protette; usare un passphrase file leggibile da root e una
# directory temporanea esplicita se la TMPDIR dell'utente non è accessibile.
sudo backimage backup /etc/ssh /var/lib/postgresql \
  --repo ghcr.io/acme/backup --tag system \
  --passphrase-file /root/secrets/backup.pass \
  --temp-dir /var/tmp/backimage

# Variante controllata: non attraversare filesystem montati e saltare cache.
sudo backimage backup /srv \
  --one-file-system --exclude 'srv/cache/**' \
  --repo ghcr.io/acme/backup --tag srv

# Ripristino non privilegiato in una directory posseduta dall'utente.
backimage restore ghcr.io/acme/backup:developer --extract \
  --destination ./restore --no-preserve-owner \
  --passphrase-file ./backup.pass

# Ripristino di sistema: prima ottenere il tar, poi applicarlo come root.
backimage restore ghcr.io/acme/backup:system \
  --output /tmp/system.tar --passphrase-file ./backup.pass
sudo mkdir -p /srv/restore-system
sudo tar -xpf /tmp/system.tar -C /srv/restore-system \
  --xattrs --acls --numeric-owner
```

`--numeric-owner` durante il backup evita di risolvere i nomi utente/gruppo e
conserva UID/GID numerici, utile tra host con database utenti diversi. Durante
il restore, `--no-preserve-owner` è la scelta pratica per un utente non-root;
`--allow-degraded` è invece un'opzione del backup e non disabilita la
preservazione dell'owner nei metadati archiviati;
per ownership, ACL, xattr, setuid/setgid, device e FIFO usare root su un
filesystem che li supporti. Vedere [FIDELITY](docs/FIDELITY.md) per la matrice
completa di fedeltà per sistema operativo e metodo di estrazione.

## Uso rapido

```console
# Login senza esporre il token nella command line.
printf '%s\n' "$REGISTRY_TOKEN" | backimage login ghcr.io \
  --username USER --password-stdin

# Backup cifrato su un registry.
backimage backup /srv/data /etc/app \
  --repo ghcr.io/team/dumps --tag daily \
  --passphrase-file /run/secrets/backup

# Backup incrementale: riusa i chunk già presenti (rivela solo l’uguaglianza
# dei chunk; leggere la sezione Sicurezza prima di abilitarlo).
backimage backup /srv/data --repo ghcr.io/team/dumps --tag daily-2 \
  --dedup --age-identity /run/secrets/age-identity \
  --passphrase-stdin

# Ispezione pubblica, elenco e verifica.
backimage inspect ghcr.io/team/dumps:daily --layers
backimage ls ghcr.io/team/dumps:daily --long \
  --passphrase-file /run/secrets/backup
backimage verify ghcr.io/team/dumps:daily

# Restore come tar o direttamente su disco.
backimage restore ghcr.io/team/dumps:daily \
  --output daily.tar --passphrase-file /run/secrets/backup
backimage restore ghcr.io/team/dumps:daily --extract \
  --destination ./restore --include 'documents/**' \
  --passphrase-file /run/secrets/backup

# Restore senza installare backimage.
docker run --rm ghcr.io/team/dumps:daily
docker run --rm -i -e BACKIMAGE_PASSPHRASE="$BACKUP_PASSPHRASE" \
  ghcr.io/team/dumps:daily tar > daily.tar
```

## Comandi e sottocomandi

Tutti i comandi supportano `--help`. L’output di errore e i progressi vanno su
stderr; `--json` lascia su stdout solo JSON strutturato.

### Flag globali

| Flag | Descrizione |
| --- | --- |
| `--json` | Output strutturato JSON |
| `-q, --quiet` | Nasconde i progressi |
| `-v, --verbose` | Aumenta il log; ripetere (`-v` debug, `-vv` trace) |
| `--no-color` | Disabilita i colori ANSI |
| `--config FILE` | Percorso di configurazione (default `$XDG_CONFIG_HOME/backimage/config.yaml`) |
| `--registry-user NOME` | Account da usare quando un registry ha più login; `none` forza una richiesta anonima. Default: il namespace del repository |

Durante un backup interattivo vengono mostrati su stderr la stima delle
sorgenti, l'avanzamento di archiviazione/compressione/cifratura, la
preparazione delle immagini OCI, le fasi di controllo/upload dei blob e la
pubblicazione dei manifest. Ogni riga contiene il timestamp iniziale. Con
`--quiet` questi messaggi vengono nascosti; con `--json` il risultato
strutturato resta l'unico contenuto su stdout.

### `version`

Mostra versione, commit, data di build, versione Go e piattaforma.

```console
backimage version
backimage --json version
```

### `login` e `logout`

```text
backimage login [REGISTRY] [flags]
backimage logout [REGISTRY]
```

Flag di `login`:

| Flag | Descrizione |
| --- | --- |
| `-u, --username` | Utente del registry |
| `-p, --password` | Password/token (visibile in `ps`, sconsigliato) |
| `--password-stdin` | Legge la password da stdin |
| `--token` | Token già pronto, alternativo a utente/password |
| `--list` | Elenca i registry configurati senza mostrare segreti |

Le credenziali sono salvate in `BACKIMAGE_AUTH_FILE` se impostato, altrimenti
in `$XDG_CONFIG_HOME/backimage/auth.json` (o `$HOME/.config/backimage/auth.json`)
con permessi `0600`.

### `backup`

```text
backimage backup <PATH...> --repo IMAGE [flags]
```

Flag:

| Flag | Default | Descrizione |
| --- | --- | --- |
| `--repo IMAGE` | — | Repository di destinazione (obbligatorio) |
| `--tag TAG` | `latest` | Tag dell’immagine |
| `--timestamp` | `false` | Appende un timestamp UTC al tag |
| `--timestamp-format LAYOUT` | `20060102T150405Z` | Layout Go del timestamp |
| `--compression CODEC` | `zstd` | `zstd`, `gzip`, `xz`, `lz4`, `none` |
| `--compression-level N` | `0` | Livello del codec (0 = default) |
| `--max-layer-size SIZE` | `1GiB` | Dimensione target del layer |
| `--encrypt` / `--no-encrypt` | encrypt | Abilita/disabilita cifratura |
| `--passphrase-file FILE` | — | Passphrase da file |
| `--passphrase-stdin` | `false` | Passphrase da stdin |
| `--password PASSWORD` | — | Passphrase diretta; visibile in history e processi |
| `--recipient KEY` | — | Chiave pubblica age; ripetibile |
| `--age-identity FILE` | — | Identità age per riusare la chiave dedup |
| `--dedup` | `false` | Deduplicazione content-defined (CDC) |
| `--dedup-chunk-min SIZE` | — | Minimo chunk CDC |
| `--dedup-chunk-avg SIZE` | — | Media chunk CDC |
| `--dedup-chunk-max SIZE` | — | Massimo chunk CDC |
| `--dedup-polynomial 0x...` | — | Polinomio Rabin CDC |
| `--local-repo` | `false` | Usa il Docker daemon locale |
| `--output MODE` | `registry` | `registry`, `daemon`, `oci-layout`, `tar` |
| `--output-path PATH` | — | Destinazione per layout OCI/tar |
| `--exclude GLOB` | — | Esclude un glob; ripetibile |
| `--one-file-system` | `false` | Non attraversa mount point |
| `--numeric-owner` | `false` | Non risolve nomi utente/gruppo |
| `--allow-degraded` | `false` | Disattiva il preflight strict delle capability; non concede privilegi |
| `--jobs N` | `3` | Upload paralleli |
| `--platform OS/ARCH` | `linux/amd64,linux/arm64` | Piattaforme auto-estraenti; ripetibile |
| `--no-metadata` | `false` | Omette i path sorgente dalle label |
| `--dry-run` | `false` | Mostra il piano senza scrivere |
| `--resume` | `true` | Riprende da checkpoint |
| `--runnable` | `true` | Richiede codec compatibili con `docker run` |
| `--temp-dir DIR` | `$TMPDIR` | Spool temporaneo dei layer |
| `--created RFC3339` | — | Data fissa per build riproducibili |
| `--remote HOST:PORT` | — | Server `listen-remote` |
| `--remote-mode stream\|layers` | `stream` | `stream`: pipeline sul server; `layers`: pipeline sul client (v1) |
| `--udp` | `false` | QUIC invece di TCP per `--remote` |
| `--tls-pin SHA256` | — | Pin del certificato remoto |
| `--tls-ca FILE` | — | Bundle CA PEM |
| `--tls-cert FILE` / `--tls-key FILE` | — | Certificato e chiave client mTLS |
| `--auth-token TOKEN` / `--auth-token-file FILE` | — | Autenticazione remota |
| `--server-side-compress` | `false` | Alias deprecato di `--remote-mode stream` (già default); errore con `--remote-mode layers` |

`--encrypt` è attivo per default. `--no-encrypt` non può essere combinato con
`--encrypt`; passphrase e recipient richiedono la cifratura. `--age-identity`
richiede `--dedup`.

### `restore`

```text
backimage restore [IMAGE] [flags]
```

Flag sorgente comuni:

| Flag | Descrizione |
| --- | --- |
| `--repo IMAGE` | Alias della reference posizionale |
| `--local-repo` | Legge dal Docker daemon locale |
| `--oci-layout DIR` | Legge da un layout OCI locale |
| `--platform OS/ARCH` | Piattaforma sorgente (default `linux/amd64`) |
| `--cache-size SIZE` | Cache layer LRU (default `2GiB`) |
| `--passphrase-file FILE` / `--passphrase-stdin` | Sblocco cifratura |
| `--password PASSWORD` | Passphrase diretta; visibile in history e processi |
| `--identity FILE` | Chiave privata age |

Flag restore:

| Flag | Default | Descrizione |
| --- | --- | --- |
| `-x, --extract` | `false` | Estrae su directory invece di produrre tar |
| `-C, --destination DIR` | `.` | Directory di destinazione |
| `-o, --output FILE` | — | Tar di output; `-` = stdout |
| `--include GLOB` | — | Include glob; ripetibile |
| `--exclude GLOB` | — | Esclude glob; ripetibile |
| `--strip-components N` | `0` | Rimuove componenti iniziali dei path |
| `--cpus N` | metà dei CPU disponibili | Limita i CPU usati durante il restore |
| `--no-preserve-owner` | `false` | Non preserva ownership |
| `--remove-local-image` | `false` | Rimuove l'immagine Docker locale dopo un restore riuscito |
| `--overwrite` | `false` | Sovrascrive output esistenti |
| `--no-verify` | `false` | Salta verifica digest plaintext |
| `--jobs N` | `3` | Download layer paralleli |

Esempi:

```console
backimage restore docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  -o mindhunters-test.tar
backimage restore docker.io/demoarchiveuser/mindhunters:mindhunters-test \
  --extract -C restore \
  --include 'photos/**' --exclude 'photos/tmp/**'
backimage restore --oci-layout ./layout --extract -C restore
```

### `inspect`, `ls`, `find`, `verify`

```text
backimage inspect IMAGE [--files] [--layers] [source flags]
backimage ls IMAGE [PATH] [-l] [--include GLOB] [--exclude GLOB] [source flags]
backimage find IMAGE PATTERN [-l] [source flags]
backimage verify IMAGE [--quick] [--continue] [source flags]
```

`inspect` mostra metadati pubblici; `--layers` aggiunge digest e intervalli dei
layer; `--files` sblocca l’immagine e include l’indice dei file. `ls` elenca
path (con `-l` dettagli); `find` applica un glob. `verify --quick` controlla
solo i metadati pubblici, mentre la modalità normale verifica anche i layer;
`--continue` raccoglie tutti gli errori di integrità.

### `repo`

```text
backimage repo stats REPO
backimage repo tags REPO
backimage repo caps REGISTRY
backimage repo rm REPO:TAG|REPO@DIGEST --yes [--force]
backimage repo prune REPO [flags]
```

- `stats` mostra tag, blob unici/condivisi e storage effettivo.
- `tags` elenca tag, digest e data di creazione (`-` se l'immagine non ha
  timestamp).
- `caps` mostra le capacità lifecycle dell’adapter del registry.
- `rm` elimina un manifest; è sempre richiesto `--yes`, e `--force` consente
  di eliminare un tag ancora referenziato.
- `prune` applica retention. Flag: `--keep-last N`,
  `--keep-within DURATION`, `--delete-older-than DURATION`,
  `--keep-tag GLOB` (ripetibile), `--dry-run`, `--yes`.

#### Durate: unità accettate

Le durate di `prune` accettano `s`, `m`, `h`, `d` (giorni), `w` (settimane) e le
combinazioni: `90m`, `12h`, `3d`, `2w`, `1d12h`. **L'unità è obbligatoria**: un
numero nudo come `3` viene rifiutato, perché non si capirebbe se sono secondi,
ore o giorni.

Una durata impostata esplicitamente deve inoltre essere **maggiore di zero**:
`--keep-within 0` e `--delete-older-than 0` sono errori d'uso. Lo zero ha invece
un significato valido per `--keep-last 0`, dove disabilita quella regola. Per
non applicare una regola temporale basta omettere il relativo flag. Questa
distinzione evita che `--delete-older-than 0`, apparentemente distruttivo,
venga interpretato come regola disabilitata e conservi tutto in silenzio.

La policy viene validata prima di aprire la connessione al registry: durata
senza unità, negativa o zero, e la combinazione dei due flag temporali vengono
rifiutate senza eseguire `ListTags` o altre operazioni remote.

#### Regole di retention

Un tag è conservato se **almeno una** regola lo seleziona, ed eliminato solo se
nessuna lo seleziona. Senza alcuna regola non viene eliminato nulla. I tag
**senza data di creazione** sono sempre conservati: un tag estraneo (non prodotto
da backimage) non viene rimosso per errore.

| Flag | Significato |
| --- | --- |
| `--keep-last N` | conserva gli N backup più recenti, a prescindere dall'età |
| `--keep-within DURATION` | conserva i backup più recenti di DURATION |
| `--delete-older-than DURATION` | elimina i backup più vecchi di DURATION |
| `--keep-tag GLOB` | conserva i tag il cui nome corrisponde al glob |

`--keep-within` e `--delete-older-than` sono la stessa regola detta al
contrario: `--keep-within 3d` ≡ `--delete-older-than 3d`. Indicarle entrambe è
un errore d'uso, per non lasciare dubbi su quale prevalga.

Esempi:

```console
# 1. Vedere cosa verrebbe eliminato, senza toccare il registry.
backimage repo prune ghcr.io/team/dumps --keep-last 7 --dry-run
```

```text
regole attive: mantieni i 7 più recenti
2 tag da eliminare (dry-run, nessuna modifica al registry), 7 conservati:
  nightly-20260801T031500Z	2026-08-01T03:15:00Z	sha256:7be74df2...
  nightly-20260802T031500Z	2026-08-02T03:15:00Z	sha256:0f9c21bf...
ripetere senza --dry-run e con --yes per applicare.
```

```console
# 2. Eliminare tutto ciò che è più vecchio di 3 giorni.
backimage repo prune ghcr.io/team/dumps --delete-older-than 3d --yes

# 3. Più vecchio di 12 ore, ma tenendo sempre i 2 più recenti e le release.
backimage repo prune ghcr.io/team/dumps --delete-older-than 12h \
  --keep-last 2 --keep-tag 'release-*' --yes

# 4. Output per script: elenco dei tag rimossi e quanti restano.
backimage repo prune ghcr.io/team/dumps --keep-last 7 --dry-run --json
```

Senza `--yes` il comando si rifiuta di eliminare e dice quanti tag sarebbero
coinvolti; `--dry-run` non richiede `--yes`.

### `doctor`

```console
backimage doctor [PATH...]
backimage --json doctor /srv/data
```

Controlla privilegi filesystem, directory temporanea/cache e, se presente,
Docker. I check non disponibili mostrano anche il rimedio suggerito.

### `listen-remote`

Avvia un server TLS 1.3 che riceve il backup e lo pubblica sul registry. Con il
default `--remote-mode stream` il server esegue l'intera pipeline: il client
manda solo il flusso tar.

```console
backimage listen-remote \
  --bind-address 0.0.0.0:7575 \
  --tls-cert /etc/backimage/server.crt \
  --tls-key /etc/backimage/server.key \
  --auth-token-file /run/secrets/remote-token \
  --allow-repo ghcr.io/team/dumps \
  --work-dir /var/lib/backimage/spool \
  --metrics-address 127.0.0.1:9090

backimage backup /srv/data --repo ghcr.io/team/dumps:remote \
  --remote backup.example:7575 \
  --tls-pin 9a55ed72... --auth-token-file /run/secrets/remote-token
```

`--tls-pin` vuole solo l'esadecimale, senza il prefisso `SHA256:` stampato dal
server.

## Backup remoto in streaming (protocollo v2)

Modalità operativa completa per la macchina con poco spazio libero: il client
non crea archivio, chunk, layer né spool. Un backup da 50 GiB gira con ~1 GiB
libero sul client.

### Chi fa cosa

| Fase | Client | Server `listen-remote` |
| --- | --- | --- |
| scansione filesystem e `tar` | ✔ | — |
| chunking, compressione, cifratura | — | ✔ |
| layer OCI e spool temporaneo | — | ✔ (un layer per volta) |
| `HEAD` deduplica, upload blob, manifest/index | — | ✔ |
| credenziali registry (token effimeri) | ✔ genera | ✔ usa, in memoria |
| passphrase e file chiave age | ✔ resta qui | mai inviata |
| DEK e dati in chiaro | — | **visibili** |

Il server è dentro il perimetro di fiducia dei dati. Se questo non è
accettabile, `--remote-mode layers` mantiene tutta la crittografia sul client
(ma richiede spazio locale per i layer).

### Dimensionamento

| Risorsa | Regola |
| --- | --- |
| disco client | indipendente dal backup: solo buffer di rete |
| RAM client | ~20 MiB in streaming; picco transitorio ~280 MiB quando age/scrypt avvolge la passphrase |
| disco server `--work-dir` | `2 × --max-layer-size × --max-sessions` (default layer 1 GiB) |
| RAM server | buffer chunk + 32 MiB di upload per sessione |

Misure reali su backup da 4 GiB incompressibili con `--max-layer-size 512MiB`:
spool client 0 byte, RSS client 19 MiB senza cifratura, picco spool server
1 GiB, directory di lavoro vuota a fine run.

### Certificati TLS del server

Il client autentica il server per **pinning**: confronta l'impronta SHA-256 del
certificato con il valore di `--tls-pin`. Non serve una CA, ma l'impronta deve
restare **stabile**: se cambia, tutti i client si fermano con
`TLS certificate fingerprint mismatch`.

Tre modi di fornire il certificato, dal più semplice al più strutturato.

#### 1. Self-signed persistente (raccomandato in LAN)

`--tls-self-signed` genera la coppia chiave/certificato e la **salva**, così
l'impronta sopravvive ai riavvii. La destinazione è, in ordine:

| Flag presenti | Dove finisce il materiale |
| --- | --- |
| `--tls-self-signed --tls-cert C --tls-key K` | esattamente in `C` e `K` |
| `--tls-self-signed --work-dir D` | `D/tls/self-signed.crt` e `D/tls/self-signed.key` |
| solo `--tls-self-signed` | in memoria: **effimero**, impronta nuova a ogni avvio |

Se i file esistono vengono riusati; se mancano vengono creati (certificato
valido 10 anni a `0644`, chiave a `0600`, directory create con `0700`). La
chiave non viene mai sovrascritta.

##### Pubblicazione e recupero della coppia TLS

Certificato e chiave sono trattati come una coppia indivisibile:

1. entrambi i PEM vengono scritti integralmente in file temporanei nelle
   rispettive directory, con i permessi finali;
2. i file vengono sincronizzati e chiusi prima di essere resi visibili;
3. la pubblicazione usa rename sullo stesso filesystem, quindi un lettore non
   osserva mai un PEM scritto solo in parte;
4. se la pubblicazione del certificato fallisce dopo quella della chiave, la
   chiave appena pubblicata viene rimossa. Il riavvio successivo può quindi
   riprovare invece di restare bloccato su materiale generato a metà.

Due path distinti non possono essere rinominati in un'unica operazione
filesystem. Per questo, all'avvio, `backimage` accetta soltanto **entrambi i
file presenti** oppure **entrambi assenti**. Se un arresto del sistema o un
intervento esterno lascia un solo file, il server fallisce esplicitamente con
`incomplete TLS material` e non genera una nuova chiave: prima di riavviare,
ripristinare il file corrispondente oppure, dopo aver verificato che la coppia
non sia recuperabile, rimuovere l'orfano. In questo modo un errore di I/O non
causa una rotazione silenziosa del PIN fidato dai client.

Percorso esplicito, indipendente dallo spool:

```console
backimage listen-remote \
  --bind-address 0.0.0.0:7575 --udp --also-tcp \
  --tls-self-signed \
  --tls-cert /etc/backimage/server.crt \
  --tls-key /etc/backimage/server.key \
  --insecure-no-auth \
  --work-dir /var/lib/backimage/spool --max-sessions 3
```

Primo avvio:

```text
generated a persistent self-signed certificate in /etc/backimage/server.crt (valid 10 years)
TLS fingerprint SHA256:9a55ed72...26cc
listening on [::]:7575 via quic (TLS 1.3, protocol v2, streaming pipeline, layer spool in /var/lib/backimage/spool)
```

Avvii successivi: stessa impronta, riga diversa.

```text
reusing the self-signed certificate in /etc/backimage/server.crt
TLS fingerprint SHA256:9a55ed72...26cc
```

Variante senza indicare percorsi: basta `--work-dir`.

```console
backimage listen-remote \
  --bind-address 0.0.0.0:7575 --tls-self-signed \
  --insecure-no-auth \
  --work-dir /home/mint/backimage-srv/data
# -> /home/mint/backimage-srv/data/tls/self-signed.{crt,key}
```

Senza `--work-dir` né `--tls-cert/--tls-key` il server avverte esplicitamente
che l'impronta è usa e getta:

```text
warning: ephemeral TLS certificate: the fingerprint changes at every restart; pass --work-dir or --tls-cert/--tls-key to persist it
```

#### 2. Certificato generato a mano con `openssl`

Utile quando la chiave è gestita da un altro processo o va condivisa con altri
servizi. ECDSA P-256, 10 anni, con gli indirizzi che i client compongono nella
SAN:

```console
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout /etc/backimage/server.key -out /etc/backimage/server.crt \
  -days 3650 -subj "/CN=backimage" \
  -addext "subjectAltName=IP:192.168.1.20,DNS:backup.example,DNS:localhost,IP:127.0.0.1"
chmod 600 /etc/backimage/server.key
```

Avvio **senza** `--tls-self-signed`: il server stampa l'impronta anche per un
certificato fornito, quindi non serve calcolarla a parte.

```console
backimage listen-remote \
  --bind-address 0.0.0.0:7575 \
  --tls-cert /etc/backimage/server.crt \
  --tls-key /etc/backimage/server.key \
  --auth-token-file /etc/backimage/remote-token \
  --allow-repo ghcr.io/team/dumps \
  --work-dir /var/lib/backimage/spool
```

```text
TLS fingerprint SHA256:4d1b8f...a07e
listening on 0.0.0.0:7575 via tcp (TLS 1.3, protocol v2, streaming pipeline, layer spool in /var/lib/backimage/spool)
```

Se serve ricavare l'impronta da un certificato senza avviare il server:

```console
openssl x509 -in /etc/backimage/server.crt -outform DER | sha256sum | cut -d' ' -f1
```

#### 3. Certificato firmato da una CA interna

Con una CA il client verifica la catena (`--tls-ca`) e `--tls-pin` non serve;
`--tls-ca` sul server abilita mTLS e autentica i client (Esempio C).

#### Uso del PIN sul client

`--tls-pin` accetta **solo l'esadecimale**: il prefisso `SHA256:` stampato dal
server non va incollato. I due punti come separatore sono tollerati.

```console
# corretto
backimage backup /srv/data --repo ghcr.io/team/dumps --tag daily \
  --remote 192.168.1.20:7575 --tls-pin 9a55ed723fdfed5ba61e261ff5bc875e324e4c8296a08f47018fa798fcde26cc

# errato: "SHA256:" non è esadecimale
backimage backup ... --tls-pin SHA256:9a55ed72...
```

Il PIN è pubblico: si può distribuire per configuration management o nel
comando di backup. La chiave privata (`server.key`) non lascia mai il server.

### Esempio A — senza autenticazione applicativa (LAN isolata)

TLS resta obbligatorio: `--insecure-no-auth` disattiva solo l'autenticazione
del client, non il trasporto cifrato.

Server:

```console
backimage listen-remote \
  --bind-address 0.0.0.0:7575 \
  --tls-self-signed \
  --insecure-no-auth \
  --allow-repo ghcr.io/team/dumps \
  --work-dir /var/lib/backimage/spool \
  --max-sessions 2
```

Il server stampa le righe da annotare:

```text
generated a persistent self-signed certificate in /var/lib/backimage/spool/tls/self-signed.crt (valid 10 years)
TLS fingerprint SHA256:9f2c...e41
listening on 0.0.0.0:7575 via tcp (TLS 1.3, protocol v2, streaming pipeline, layer spool in /var/lib/backimage/spool)
```

Con `--work-dir` il certificato è persistente: l'impronta resta la stessa dopo
un riavvio, quindi il `--tls-pin` dei client non va aggiornato. Vedi
[Certificati TLS del server](#certificati-tls-del-server).

Client (nessun token; il PIN è obbligatorio, un self-signed senza pin viene
rifiutato):

```console
backimage login ghcr.io --username TEAM --password-stdin

backimage backup /srv/data \
  --repo ghcr.io/team/dumps --tag daily --timestamp \
  --remote 192.168.1.20:7575 \
  --tls-pin 9f2c...e41 \
  --no-encrypt \
  --max-layer-size 512MiB \
  --temp-dir /var/tmp/backimage
```

Chiunque raggiunga la porta può tentare un backup verso i repository
consentiti: usare solo su rete isolata e con firewall.

### Esempio B — token condiviso e backup cifrato (raccomandato)

Server:

```console
head -c 32 /dev/urandom | base64 > /etc/backimage/remote-token
chmod 600 /etc/backimage/remote-token

backimage listen-remote \
  --bind-address 0.0.0.0:7575 \
  --tls-cert /etc/backimage/server.crt \
  --tls-key /etc/backimage/server.key \
  --auth-token-file /etc/backimage/remote-token \
  --allow-repo ghcr.io/team/dumps \
  --work-dir /var/lib/backimage/spool \
  --max-sessions 4 --max-bytes 200GiB --rate-limit 80MiB \
  --metrics-address 127.0.0.1:9090 --log-format json
```

Client (stesso token, passphrase da file, mai in `argv`):

```console
backimage login ghcr.io --username TEAM --password-stdin
printf '%s' "$PASSPHRASE" > /run/secrets/backup-passphrase
chmod 600 /run/secrets/backup-passphrase

backimage backup /srv/data /var/lib/postgresql \
  --repo ghcr.io/team/dumps --tag nightly --timestamp \
  --remote backup.example:7575 \
  --tls-ca /etc/ssl/internal-ca.crt \
  --auth-token-file /etc/backimage/remote-token \
  --passphrase-file /run/secrets/backup-passphrase \
  --exclude '**/*.tmp' --one-file-system \
  --max-layer-size 512MiB \
  --temp-dir /var/tmp/backimage \
  --json
```

Output durante il backup (stderr): le fasi del server arrivano al client.

```text
remote: streaming mode; archiving, compression, encryption and push run on the server, ...
backup: streaming remoto in corso (pipeline sul server)
server[receiving]: ricevuti 193.0 MiB, archiviati 193.0 MiB, caricati 193.0 MiB, layer 6 (0 saltati), chunk 193
server[pushing]:   ...
backup: streaming remoto completato
```

Risultato JSON (`--json`): `digest`, `layers`, `chunks`, `bytesRaw`,
`bytesStored`, `uploadedBytes`, `skippedBlobs` sono contatori del server.

### Esempio C — mTLS, senza token condiviso

Server (la CA autentica i client; il token diventa opzionale):

```console
backimage listen-remote \
  --bind-address 0.0.0.0:7575 \
  --tls-cert /etc/backimage/server.crt \
  --tls-key /etc/backimage/server.key \
  --tls-ca /etc/backimage/clients-ca.crt \
  --allow-repo ghcr.io/team/ \
  --work-dir /var/lib/backimage/spool
```

Client:

```console
backimage backup /srv/data --repo ghcr.io/team/dumps --tag daily \
  --remote backup.example:7575 \
  --tls-ca /etc/ssl/internal-ca.crt \
  --tls-cert /etc/backimage/client.crt --tls-key /etc/backimage/client.key \
  --passphrase-file /run/secrets/backup-passphrase
```

### Esempio D — QUIC/UDP

```console
backimage listen-remote --udp --also-tcp \
  --bind-address 0.0.0.0:7575 --tls-self-signed \
  --auth-token-file /etc/backimage/remote-token \
  --allow-repo ghcr.io/team/dumps --work-dir /var/lib/backimage/spool

backimage backup /srv/data --repo ghcr.io/team/dumps --tag daily \
  --remote 192.168.1.20:7575 --udp --tls-pin <PIN> \
  --auth-token-file /etc/backimage/remote-token \
  --passphrase-file /run/secrets/backup-passphrase
```

`--also-tcp` fa convivere client TCP e QUIC sulla stessa porta. Se UDP è
bloccato dalla rete, il client fallisce con un suggerimento esplicito a
ritentare senza `--udp`.

### Ripristino

Il restore non passa dal server remoto: l'immagine è un normale artefatto OCI.

```console
backimage restore ghcr.io/team/dumps:nightly-20260810T031500Z \
  --extract --destination /restore \
  --passphrase-file /run/secrets/backup-passphrase

docker run --rm -e BACKIMAGE_PASSPHRASE="$PASSPHRASE" \
  -v "$PWD/restore:/restore" ghcr.io/team/dumps:nightly-20260810T031500Z \
  extract --out /restore
```

### Limiti dichiarati

- nessun checkpoint a metà stream: un'interruzione di rete fa ripartire il
  flusso dall'inizio rileggendo la sorgente; i layer già pubblicati restano
  saltati dal controllo `HEAD`;
- nessuna parità di digest tra backup locale e remoto in streaming, perché i
  metadati li costruisce il server (usare `--remote-mode layers` se serve);
- `--server-side-compress` è un alias di questa modalità; con
  `--remote-mode layers` è un errore d'uso;
- campagna di validazione eseguita fino a 4 GiB: i 50 GiB dichiarati come
  obiettivo di progetto non sono ancora stati misurati end-to-end.

#### LAN fidata: nessun file di chiavi TLS

TLS è sempre attivo (solo TLS 1.3): non esiste una modalità remota senza
cifratura del trasporto. In una LAN fidata non è però necessario predisporre
una CA, un certificato persistente o file `--tls-cert/--tls-key`. La modalità
semplice è un certificato self-signed effimero sul server e il pinning sul
client:

Sul server remoto:

```console
printf '%s\n' 'un-token-lungo-e-casuale' > /etc/backimage/remote-token
chmod 600 /etc/backimage/remote-token
backimage listen-remote \
  --bind-address 0.0.0.0:7575 \
  --tls-self-signed \
  --auth-token-file /etc/backimage/remote-token \
  --allow-repo ghcr.io/team/dumps
```

Il server stampa una riga come `TLS fingerprint SHA256:<PIN>`. Sul client:

```console
backimage login ghcr.io --username TEAM --password-stdin
backimage backup /srv/data --repo ghcr.io/team/dumps --tag daily \
  --remote 192.168.1.20:7575 \
  --tls-pin <PIN> \
  --auth-token-file ./remote-token \
  --passphrase-file ./backup-passphrase
```

In questo scenario non servono `--tls-cert` né `--tls-key` sul client:
`--tls-self-signed` produce la coppia sul server. Con `--work-dir` (o con
`--tls-cert/--tls-key`) il materiale viene salvato e il PIN resta valido dopo
un riavvio; senza nessuno dei due il certificato vive in memoria, dura 24 ore e
il PIN va ricopiato sul client a ogni riavvio — vedi
[Certificati TLS del server](#certificati-tls-del-server). `--tls-pin` è
obbligatorio per fidarsi di un certificato self-signed: senza pin o CA il
client rifiuta il collegamento.

Se la LAN è completamente isolata e si accetta di non autenticare i client,
si può sostituire `--auth-token-file` con `--insecure-no-auth` sul server e
omettere `--auth-token-file` sul client:

```console
backimage listen-remote --bind-address 0.0.0.0:7575 \
  --tls-self-signed --insecure-no-auth \
  --allow-repo ghcr.io/team/dumps
backimage backup /srv/data --repo ghcr.io/team/dumps --remote 192.168.1.20:7575 \
  --tls-pin <PIN>
```

`--insecure-no-auth` disabilita solo l'autenticazione applicativa dei client,
non TLS e non la verifica del PIN. Chiunque raggiunga la porta può però
tentare un backup verso i repository consentiti: usarlo solo su una rete
isolata e con firewall.

#### Certificati persistenti e mTLS

Per un certificato emesso da una CA interna o pubblica, il server usa
`--tls-cert SERVER.crt --tls-key SERVER.key`; il client usa `--tls-ca CA.crt`
se la CA non è già nel trust store del sistema. `--tls-key` è la chiave privata
del server e deve restare solo sul server.

Per autenticare anche il client con mTLS, il server aggiunge `--tls-ca CA.crt`
e il client usa `--tls-cert CLIENT.crt --tls-key CLIENT.key`. In modalità mTLS
il server considera il certificato client come autenticazione; il token
condiviso è opzionale. La chiave privata client deve restare sul client.

Ruoli distinti:

| Elemento | Serve a | Dove risiede |
| --- | --- | --- |
| certificato/chiave server | cifrare TLS e autenticare il server | server: file indicati, `WORKDIR/tls/` con `--tls-self-signed`, o memoria se non c'è dove scrivere |
| `--tls-pin` o `--tls-ca` | verificare il certificato server | client |
| certificato/chiave client | autenticazione mTLS opzionale | client |
| `--auth-token-file` | autenticazione applicativa alternativa a mTLS | entrambi, stesso token |
| `backimage login` | token per il registry di destinazione | client |

Flag server:

| Flag | Default | Descrizione |
| --- | --- | --- |
| `--bind-address` | `0.0.0.0:7575` | Indirizzo di ascolto |
| `--udp` | `false` | QUIC invece di TCP |
| `--also-tcp` | `false` | Aggiunge TCP quando è attivo QUIC |
| `--tls-cert`, `--tls-key` | — | Certificato/chiave server PEM; il pin viene stampato anche in questo caso |
| `--tls-ca` | — | CA per autenticare client mTLS |
| `--tls-self-signed` | `false` | Genera il certificato e stampa il pin; persistente in `--tls-cert/--tls-key` o in `WORKDIR/tls/`, effimero se manca entrambi |
| `--auth-token`, `--auth-token-file` | — | Token condiviso |
| `--insecure-no-auth` | `false` | Disabilita auth (fortemente sconsigliato) |
| `--allow-repo PREFIX` | — | Prefix repository consentiti; ripetibile |
| `--max-sessions` | `4` | Sessioni concorrenti |
| `--max-bytes SIZE` | `0` | Limite per sessione; 0 illimitato |
| `--rate-limit SIZE` | `0` | Byte/s per sessione; 0 illimitato |
| `--metrics-address ADDR` | — | Endpoint `/healthz` e `/metrics` |
| `--log-format` | `text` | `text` o `json` |

I flag nascosti `--x-quic-streams`, `--x-quic-window`, `--x-quic-gso` e
`--x-quic-cc` sono sperimentali e non fanno parte dell’API stabile.

### Server in Docker con `compose.yml`

Ogni flag di `listen-remote` è configurabile anche da ambiente:
`BACKIMAGE_` + nome del flag in maiuscolo con i `-` sostituiti da `_`
(`--bind-address` → `BACKIMAGE_BIND_ADDRESS`, `--tls-self-signed` →
`BACKIMAGE_TLS_SELF_SIGNED`). Un flag esplicito sulla command line vince
sempre sull'ambiente; una variabile vuota equivale a non impostata. Questo
rende configurabile l'immagine distroless, che non ha una shell nell'entrypoint.

Il `compose.yml` nella radice del repository avvia il server con i default
equivalenti a:

```console
backimage listen-remote --bind-address 0.0.0.0:7575 --udp --also-tcp \
  --tls-self-signed --insecure-no-auth --work-dir /data --max-sessions 3
```

```console
docker compose up -d
docker compose logs -f     # annotare la riga "TLS fingerprint SHA256:..."
```

Estratto minimo, se serve un file proprio:

```yaml
services:
  backimage:
    image: ghcr.io/manprint/backimage:latest
    command: ["listen-remote"]
    restart: unless-stopped
    ports:
      - "7575:7575/tcp"
      - "7575:7575/udp"
    volumes:
      - backimage-data:/data
    environment:
      BACKIMAGE_BIND_ADDRESS: "0.0.0.0:7575"
      BACKIMAGE_UDP: "true"
      BACKIMAGE_ALSO_TCP: "true"
      BACKIMAGE_TLS_SELF_SIGNED: "true"
      BACKIMAGE_INSECURE_NO_AUTH: "true"
      BACKIMAGE_WORK_DIR: "/data"
      BACKIMAGE_MAX_SESSIONS: "3"

volumes:
  backimage-data:
```

Poiché `BACKIMAGE_WORK_DIR` sta su un volume, il certificato self-signed
persiste in `/data/tls/` e il PIN resta lo stesso dopo `docker compose restart`.

Il `compose.yml` del repository elenca commentate tutte le altre variabili:
`BACKIMAGE_TLS_CERT`/`BACKIMAGE_TLS_KEY` (certificato proprio, oppure posizione
in cui persistere quello generato), `BACKIMAGE_TLS_CA` (mTLS),
`BACKIMAGE_AUTH_TOKEN_FILE`, `BACKIMAGE_ALLOW_REPO` (lista separata da virgole),
`BACKIMAGE_MAX_BYTES`, `BACKIMAGE_RATE_LIMIT`, `BACKIMAGE_METRICS_ADDRESS`,
`BACKIMAGE_LOG_FORMAT`, `BACKIMAGE_VERBOSE`, `BACKIMAGE_JSON`,
`BACKIMAGE_QUIET`, `BACKIMAGE_NO_COLOR`, `BACKIMAGE_CONFIG`,
`BACKIMAGE_AUTH_FILE` e le variabili QUIC sperimentali.

#### Precedenza fra ambiente e command line

Il caricamento da ambiente visita sia i flag locali di `listen-remote`, sia i
flag persistenti ereditati dalla root. Sono quindi effettivi anche
`BACKIMAGE_JSON`, `BACKIMAGE_QUIET`, `BACKIMAGE_VERBOSE`,
`BACKIMAGE_NO_COLOR`, `BACKIMAGE_CONFIG` e `BACKIMAGE_REGISTRY_USER`, non solo
flag locali come `BACKIMAGE_WORK_DIR` o `BACKIMAGE_TLS_SELF_SIGNED`.

La precedenza è intenzionalmente semplice:

1. un flag presente sulla command line vince sempre, anche se imposta
   esplicitamente `false`, `0` o una stringa vuota;
2. in assenza del flag, una variabile `BACKIMAGE_*` non vuota diventa il valore
   del flag;
3. una variabile assente o composta solo da spazi viene ignorata e resta il
   default normale.

```console
# JSON e verbosità arrivano dall'ambiente; l'indirizzo è un flag locale.
BACKIMAGE_JSON=true BACKIMAGE_VERBOSE=2 \
  backimage listen-remote --bind-address 127.0.0.1:7575 \
    --tls-self-signed --insecure-no-auth --work-dir /tmp/backimage-server

# Il false esplicito prevale su BACKIMAGE_JSON=true.
BACKIMAGE_JSON=true \
  backimage --json=false listen-remote --bind-address 127.0.0.1:7575 \
    --tls-self-signed --insecure-no-auth --work-dir /tmp/backimage-server
```

I valori errati riportano il nome della variabile, per esempio
`BACKIMAGE_MAX_SESSIONS="many"`; l'errore avviene prima dell'apertura del
listener. Questa conversione automatica riguarda il comando `listen-remote`;
`BACKIMAGE_AUTH_FILE`, `XDG_CONFIG_HOME` e le altre variabili non derivate da
flag mantengono invece la semantica specifica descritta nella tabella sotto.

#### Proprietà e persistenza di `/data`

L'immagine viene eseguita come `nonroot:nonroot` (UID/GID `65532`) e contiene
già `/data` con proprietario `65532:65532` e modo `0700`. Quando Docker crea un
**nuovo volume nominato** vuoto per quel mount point, copia anche questi
metadati: la configurazione Compose predefinita può quindi creare
`/data/tls`, lo spool e gli altri file fin dal primo avvio senza privilegi
root.

Un bind mount segue invece proprietario e permessi della directory host, che
va preparata esplicitamente:

```console
sudo mkdir -p /srv/backimage/data
sudo chown 65532:65532 /srv/backimage/data
sudo chmod 0700 /srv/backimage/data
```

Un volume nominato creato con una vecchia immagine può essere già `root:root`:
aggiornare l'immagine non ne modifica i metadati. Prima fare un backup del
volume e poi correggerne ricorsivamente il proprietario con un container di
manutenzione, oppure ricrearlo consapevolmente. Ricreare il volume elimina
anche il certificato persistente e cambia il PIN TLS, oltre a rimuovere ogni
file rimasto nello spool.

Note operative:

- il volume `/data` è dello spool dei layer *e* del materiale TLS: dimensionarlo
  con `2 × max-layer-size × max-sessions`;
- con un bind mount host la directory va prima assegnata all'utente
  dell'immagine: `sudo chown 65532:65532 /srv/backimage/data`;
- `BACKIMAGE_METRICS_ADDRESS` va legato a `0.0.0.0:9090` dentro al container
  (con `127.0.0.1` sarebbe raggiungibile solo dall'interno) e la porta va
  pubblicata;
- QUIC chiede un buffer UDP grande: senza i `sysctls` commentati nel file,
  quic-go registra un warning sul receive buffer;
- immagine distroless: nessuna shell, quindi gli healthcheck basati su `test`
  non funzionano.

Client verso il container (il PIN è quello stampato nei log):

```console
backimage backup /srv/data --repo ghcr.io/team/dumps --tag daily --timestamp \
  --remote HOST:7575 --udp --tls-pin <PIN>
```

## File, cache e variabili d’ambiente

| Variabile/percorso | Funzione |
| --- | --- |
| `BACKIMAGE_PASSPHRASE` | Passphrase per immagini auto-estraenti e CLI |
| `BACKIMAGE_AUTH_FILE` | File credenziali custom |
| `BACKIMAGE_<FLAG>` | Valore di default per un flag locale o globale ereditato da `listen-remote` (`--bind-address` → `BACKIMAGE_BIND_ADDRESS`, `--json` → `BACKIMAGE_JSON`); la CLI esplicita prevale |
| `XDG_CONFIG_HOME` | Base per `backimage/auth.json` e config |
| `XDG_CACHE_HOME` | Cache layer e checkpoint; cache restore default 2 GiB |
| `TMPDIR` | Spool se non è impostato `--temp-dir` |
| `$HOME/.config/backimage/auth.json` | Fallback credenziali su Unix |
| `$XDG_CACHE_HOME/backimage/checkpoints` | Checkpoint upload riprendibili |
| `$XDG_CACHE_HOME/backimage/layers` | Cache LRU dei layer scaricati |

Non mettere passphrase o token negli argomenti quando è possibile: usare file,
stdin o secret del runtime container. Le immagini con `--dedup` rivelano a chi
può osservare il registry quali chunk sono uguali tra backup.

## Sicurezza e ripristino

- Preferire `--password-stdin`, `--passphrase-file` e `--auth-token-file`.
- Verificare sempre un’immagine con `backimage verify` prima del restore.
- Conservare separatamente passphrase e identità age.
- Limitare `listen-remote` con TLS, token/mTLS e `--allow-repo`.
- Usare `repo rm` e `repo prune` solo dopo un `--dry-run`; sono operazioni
  distruttive lato registry.
- `--no-verify` e `--insecure-no-auth` sono eccezioni operative, non default.

## Sviluppo e qualità

```console
make check       # lint, test, race test e controlli di progetto
make build       # binario locale
make build-all   # target cross-platform
make embed       # helper auto-estraenti embedded
make e2e         # suite end-to-end
```

La documentazione tecnica dettagliata è in [`docs/`](docs/):
[backup](docs/backup.md), [restore](docs/restore.md),
[registries](docs/registries.md), [dedup](docs/dedup.md),
[compression](docs/compression.md), [remote](docs/remote.md),
[formato immagine](docs/image-format.md), [sicurezza](docs/security.md) e
[riferimento generato della CLI](docs/cli.md).

## Licenza

Vedere [`LICENSE`](LICENSE).
