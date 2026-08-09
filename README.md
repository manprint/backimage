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
go install github.com/fpierri/backimage/cmd/backimage@latest
# oppure, dentro il checkout:
make build
```

### Immagine Docker

```console
docker pull ghcr.io/manprint/backimage:latest
docker run --rm ghcr.io/manprint/backimage:latest version
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
| Server remoto | `--remote HOST:PORT` | Invia i layer a `listen-remote` |

## Compressione e protezione con password

La pipeline è, in ordine, **archivio → chunk → compressione → cifratura →
layer OCI**. La compressione riduce i dati prima della cifratura; cifrare prima
renderebbe la compressione praticamente inefficace. Il default è `zstd` con il
livello predefinito del codec (`--compression-level 0`), una buona scelta
generale per velocità, dimensioni e interoperabilità.

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

### Con la CLI installata

`restore --extract` materializza direttamente i file; senza `--extract`,
`restore` produce invece un tar. Gli include e gli exclude possono essere
ripetuti e sono utili per estrarre solo una parte di un backup:

```console
# Directory completa in una destinazione locale.
mkdir -p ./restore
backimage restore ghcr.io/acme/backup:daily \
  --extract --destination ./restore \
  --passphrase-file ./backup.pass

# Solo PDF sotto documents, escludendo i temporanei.
backimage restore ghcr.io/acme/backup:daily \
  --extract --destination ./documents \
  --include 'documents/**/*.pdf' \
  --exclude 'documents/**/tmp/**' \
  --passphrase-file ./backup.pass

# Tar su disco, da ispezionare o trasferire.
backimage restore ghcr.io/acme/backup:daily \
  --output ./daily.tar --passphrase-file ./backup.pass

# Sorgente locale invece del registry.
backimage restore --oci-layout ./oci-layout \
  --extract --destination ./restore-local \
  --passphrase-stdin < ./backup.pass
```

Per vedere i dati senza estrarli, usare l'indice: `inspect --files` sblocca
l'immagine e `ls`/`find` elencano i path. `inspect --layers` e `verify --quick`
invece leggono solo i metadati pubblici e non richiedono la passphrase;
`verify` completo è preferibile prima di un restore.

```console
backimage inspect ghcr.io/acme/backup:daily --files \
  --passphrase-file ./backup.pass
backimage ls ghcr.io/acme/backup:daily --long --include 'documents/**' \
  --passphrase-file ./backup.pass
backimage verify ghcr.io/acme/backup:daily \
  --passphrase-file ./backup.pass
```

### Senza installare la CLI sul computer di destinazione

Un'immagine runnable contiene il programma auto-estraente. Il computer di
destinazione non deve installare `backimage`: basta Docker o un runtime OCI.
Per scrivere su una directory dell'host, usare un bind mount:

```console
mkdir -p ./restore
docker run --rm \
  -e BACKIMAGE_PASSPHRASE="$BACKUP_PASSPHRASE" \
  -v "$PWD/restore:/restore" \
  ghcr.io/acme/backup:daily extract --out /restore
```

Si può anche estrarre un tar e affidare la materializzazione agli strumenti
standard del sistema. Questo è il caso più fedele su Linux quando si devono
ripristinare ownership, ACL, xattr, device e permessi speciali:

```console
docker run --rm -i \
  -e BACKIMAGE_PASSPHRASE="$BACKUP_PASSPHRASE" \
  ghcr.io/acme/backup:daily tar \
  | sudo tar -xpf - -C /srv/restore \
      --xattrs --acls --numeric-owner
```

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
| File illeggibili non essenziali | `--allow-degraded` | Continua, ma il backup è parziale: registrare e correggere gli errori |
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
| `--allow-degraded` | `false` | Continua con file illeggibili |
| `--jobs N` | `3` | Upload paralleli |
| `--platform OS/ARCH` | `linux/amd64,linux/arm64` | Piattaforme auto-estraenti; ripetibile |
| `--no-metadata` | `false` | Omette i path sorgente dalle label |
| `--dry-run` | `false` | Mostra il piano senza scrivere |
| `--resume` | `true` | Riprende da checkpoint |
| `--runnable` | `true` | Richiede codec compatibili con `docker run` |
| `--temp-dir DIR` | `$TMPDIR` | Spool temporaneo dei layer |
| `--created RFC3339` | — | Data fissa per build riproducibili |
| `--remote HOST:PORT` | — | Server `listen-remote` |
| `--udp` | `false` | QUIC invece di TCP per `--remote` |
| `--tls-pin SHA256` | — | Pin del certificato remoto |
| `--tls-ca FILE` | — | Bundle CA PEM |
| `--tls-cert FILE` / `--tls-key FILE` | — | Certificato e chiave client mTLS |
| `--auth-token TOKEN` / `--auth-token-file FILE` | — | Autenticazione remota |
| `--server-side-compress` | `false` | Compressione richiesta al server (può vedere plaintext) |

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
| `--no-preserve-owner` | `false` | Non preserva ownership |
| `--overwrite` | `false` | Sovrascrive output esistenti |
| `--no-verify` | `false` | Salta verifica digest plaintext |
| `--jobs N` | `3` | Download layer paralleli |

Esempi:

```console
backimage restore ghcr.io/team/dumps:daily -o daily.tar
backimage restore ghcr.io/team/dumps:daily --extract -C restore \
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
- `tags` elenca tag, digest e data di creazione.
- `caps` mostra le capacità lifecycle dell’adapter del registry.
- `rm` elimina un manifest; è sempre richiesto `--yes`, e `--force` consente
  di eliminare un tag ancora referenziato.
- `prune` applica retention. Flag: `--keep-last N`, `--keep-within DURATION`,
  `--keep-tag GLOB` (ripetibile), `--dry-run`, `--yes`.

Esempio sicuro:

```console
backimage repo prune ghcr.io/team/dumps --keep-last 7 --keep-tag 'release-*' \
  --dry-run --json
backimage repo prune ghcr.io/team/dumps --keep-last 7 --keep-tag 'release-*' \
  --yes
```

### `doctor`

```console
backimage doctor [PATH...]
backimage --json doctor /srv/data
```

Controlla privilegi filesystem, directory temporanea/cache e, se presente,
Docker. I check non disponibili mostrano anche il rimedio suggerito.

### `listen-remote`

Avvia un server TLS che riceve stream cifrati e pubblica i layer su un registry.
Il protocollo v1 è diskless; `--spool` è rifiutato.

```console
backimage listen-remote \
  --bind-address 0.0.0.0:7575 \
  --tls-cert /etc/backimage/server.crt \
  --tls-key /etc/backimage/server.key \
  --auth-token-file /run/secrets/remote-token \
  --allow-repo ghcr.io/team/dumps \
  --metrics-address 127.0.0.1:9090

backimage backup /srv/data --repo ghcr.io/team/dumps:remote \
  --remote backup.example:7575 \
  --tls-pin SHA256:... --auth-token-file /run/secrets/remote-token
```

Flag server:

| Flag | Default | Descrizione |
| --- | --- | --- |
| `--bind-address` | `0.0.0.0:7575` | Indirizzo di ascolto |
| `--udp` | `false` | QUIC invece di TCP |
| `--also-tcp` | `false` | Aggiunge TCP quando è attivo QUIC |
| `--tls-cert`, `--tls-key` | — | Certificato/chiave server PEM |
| `--tls-ca` | — | CA per autenticare client mTLS |
| `--tls-self-signed` | `false` | Certificato effimero e pin stampato |
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

## File, cache e variabili d’ambiente

| Variabile/percorso | Funzione |
| --- | --- |
| `BACKIMAGE_PASSPHRASE` | Passphrase per immagini auto-estraenti e CLI |
| `BACKIMAGE_AUTH_FILE` | File credenziali custom |
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
