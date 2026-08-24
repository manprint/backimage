# `backimage-backup.sh` — wrapper per backup schedulati

Wrapper attorno a `backimage backup` pensato per girare da cron o da un timer
systemd senza nessuno che guardi: prende un lock, scrive un log per ogni
esecuzione, esegue il backup, opzionalmente applica la retention sul registry e
notifica l'esito su Slack o Google Chat.

Non aggiunge logica di backup: costruisce la riga di comando, propaga l'exit
code di `backimage` e racconta cosa è successo.

**File in questa cartella**

| File | Cos'è |
|---|---|
| `backimage-backup.sh` | lo script |
| `backimage-backup.env.example` | file di configurazione da copiare |
| `README.md` | questo documento |

Guida operativa più breve, con esempi cron e systemd:
[`../docs/cron.md`](../docs/cron.md).

---

## Indice

1. [Requisiti](#requisiti)
2. [Installazione](#installazione)
3. [Quickstart](#quickstart)
4. [Uso da riga di comando](#uso-da-riga-di-comando)
5. [Il file di environment](#il-file-di-environment)
6. [Riferimento delle variabili](#riferimento-delle-variabili)
   - [Identità del job](#identità-del-job)
   - [Cosa salvare e dove](#cosa-salvare-e-dove)
   - [Chiavi e credenziali](#chiavi-e-credenziali)
   - [Pipeline](#pipeline)
   - [Backup delegato a un server remoto](#backup-delegato-a-un-server-remoto)
   - [Esecuzione](#esecuzione)
   - [Log](#log)
   - [Retention sul registry](#retention-sul-registry)
   - [Notifiche](#notifiche)
7. [Notifiche in pratica](#notifiche-in-pratica)
8. [Schedulazione](#schedulazione)
9. [Exit code](#exit-code)
10. [Anatomia di un'esecuzione](#anatomia-di-unesecuzione)
11. [Troubleshooting](#troubleshooting)

---

## Requisiti

| Strumento | Serve per |
|---|---|
| `bash` ≥ 4 | lo script |
| `flock` (util-linux) | lock anti-sovrapposizione — **obbligatorio** |
| `backimage` | il backup |
| `curl` | notifiche (solo se `BI_NOTIFY` ≠ `never`) |
| `timeout` (coreutils) | solo se usi `BI_TIMEOUT` |
| `jq` | opzionale: senza, il riepilogo JSON viene letto da un parser di ripiego |

Lo script verifica queste dipendenze all'avvio e si ferma con exit 78 se ne
manca una necessaria.

## Installazione

```console
sudo install -m 0755 backimage-backup.sh /usr/local/bin/backimage-backup
sudo install -d -m 0700 /etc/backimage
sudo install -m 0600 backimage-backup.env.example /etc/backimage/backup.env
sudo install -d -m 0750 /var/log/backimage
```

## Quickstart

Cinque minuti, dal nulla a un backup notturno notificato su Slack.

```console
# 1. passphrase del backup: perderla significa perdere i dati
umask 077 && backimage genpass | sudo tee /etc/backimage/backup.pass >/dev/null
sudo chmod 600 /etc/backimage/backup.pass

# 2. credenziali del registry, nel file che leggerà il cron di root
printf '%s\n' "$REGISTRY_TOKEN" | sudo env BACKIMAGE_AUTH_FILE=/etc/backimage/auth.json \
  backimage login ghcr.io --username me --password-stdin

# 3. webhook, fuori dall'ambiente e da `ps`
printf '%s\n' 'https://hooks.slack.com/services/T000/B000/xxxx' \
  | sudo tee /etc/backimage/webhook.url >/dev/null
sudo chmod 600 /etc/backimage/webhook.url
```

Poi `/etc/backimage/backup.env`, versione minima:

```bash
BI_JOB_NAME="prod-data"
BI_DESCRIPTION="Backup notturno di /srv/data (upload applicativi) — team platform"
BI_PATHS="/srv/data"
BI_REPO="ghcr.io/me/dumps"
BI_TAG="daily"
BI_PASSPHRASE_FILE="/etc/backimage/backup.pass"
BI_AUTH_FILE="/etc/backimage/auth.json"
BI_WEBHOOK_URL_FILE="/etc/backimage/webhook.url"
BI_NOTIFY="on-error"
```

Verifica in tre passi, senza scrivere niente:

```console
sudo backimage-backup --print-config     # configurazione risolta, URL redatto
sudo backimage-backup --test-notify      # arriva il messaggio nel canale?
sudo backimage-backup --dry-run          # backimage stampa il piano ed esce
sudo backimage-backup                    # esecuzione vera
```

Infine il crontab:

```cron
15 3 * * *  root  /usr/local/bin/backimage-backup -c /etc/backimage/backup.env
```

## Uso da riga di comando

```
backimage-backup [-c FILE] [--dry-run] [--test-notify] [--print-config]
```

| Opzione | Effetto |
|---|---|
| `-c`, `--config FILE` | file di configurazione da usare. Senza, si usa `$BI_CONFIG`, poi `/etc/backimage/backup.env`. Un file passato con `-c` e mancante è un errore; il default mancante no (si va di sole variabili d'ambiente) |
| `-n`, `--dry-run` | aggiunge `--dry-run` a `backimage backup`: stampa il piano, non scrive niente, non fa prune, non notifica |
| `--test-notify` | invia una notifica di prova (con la tua `BI_DESCRIPTION`) ed esce. Ignora `BI_NOTIFY`: serve proprio a testare il webhook |
| `--print-config` | stampa tutte le `BI_*` risolte, il file usato, il log del run e il flavour di notifica dedotto. `BI_WEBHOOK_URL` viene redatto |
| `-h`, `--help` | riepilogo delle opzioni |
| `--version` | versione del wrapper |

Esempi:

```console
# job diverso, stesso file di configurazione
backimage-backup -c /etc/backimage/postgres.env

# variante settimanale senza duplicare la configurazione
BI_TAG=weekly BI_PRUNE_KEEP_LAST=8 backimage-backup -c /etc/backimage/backup.env

# configurazione interamente da ambiente, nessun file
BI_CONFIG=/dev/null BI_PATHS=/srv/data BI_REPO=ghcr.io/me/dumps \
  BI_PASSPHRASE_FILE=/etc/backimage/backup.pass BI_NOTIFY=never backimage-backup
```

## Il file di environment

**Formato.** È un file sourcato da bash: `NOME="valore"`, commenti con `#`.
Quota sempre i valori. Puoi usare array bash (serve solo per `BI_EXTRA_ARGV`).

**Precedenza.** Una variabile `BI_*` già presente nell'ambiente **vince** su
quella nel file. Ordine effettivo:

```
ambiente  >  file di configurazione  >  default dello script
```

Questo rende un file riutilizzabile per varianti dello stesso job (`BI_TAG`,
`BI_PRUNE_KEEP_LAST`, `BI_NOTIFY`, …) senza copiarlo.

**Liste.** `BI_PATHS`, `BI_EXCLUDES`, `BI_RECIPIENTS` e `BI_PRUNE_KEEP_TAGS`
accettano un elemento per riga. Righe vuote e righe che iniziano con `#`
vengono ignorate; gli spazi in testa e in coda vengono tolti. Se il valore sta
su una riga sola viene diviso sugli spazi, quindi `"/a /b"` e

```bash
BI_PATHS="/a
/b"
```

sono equivalenti. **Un percorso che contiene spazi deve stare su una riga sua.**

**Booleani.** `true`, `yes`, `on`, `1` (case-insensitive) valgono vero;
qualsiasi altra cosa è falso.

**Permessi.** Il file contiene percorsi di segreti (e volendo il webhook):
`0600`, proprietario `root`, dentro una directory `0700`. Lo script gira con
`umask 077`.

**Più job sulla stessa macchina.** Un file per job, un `BI_JOB_NAME` diverso
per ciascuno: lock, log e titolo delle notifiche restano separati.

```console
/etc/backimage/data.env       # BI_JOB_NAME="srv-data"
/etc/backimage/postgres.env   # BI_JOB_NAME="postgres"
```

---

## Riferimento delle variabili

Ogni voce riporta il default fra parentesi. `—` significa "vuota, nessun flag
passato a `backimage`": in quel caso vale il default del comando.

### Identità del job

#### `BI_CONFIG` (`/etc/backimage/backup.env`)
File di configurazione da leggere, quando non passi `-c`. Utile nei timer
systemd, dove è più comodo passare un `Environment=` che un argomento.

```bash
BI_CONFIG=/etc/backimage/postgres.env backimage-backup
```

#### `BI_BIN` (`backimage`)
Eseguibile di backimage: un nome cercato nel `PATH` o un percorso assoluto. Da
impostare quando il binario non è in `/usr/local/bin` o quando ne tieni più
versioni. Lo script normalizza comunque il `PATH` (cron ne dà uno minimo).

```bash
BI_BIN="/opt/backimage/bin/backimage"
```

#### `BI_JOB_NAME` (`backup`)
Identificativo breve del job. Nomina il lock file di default, i file di log e
il titolo delle notifiche. I caratteri fuori da `[A-Za-z0-9._-]` diventano `-`.
Con più job sulla stessa macchina, deve essere diverso per ciascuno: è ciò che
impedisce a due job distinti di bloccarsi a vicenda.

```bash
BI_JOB_NAME="postgres"
```

#### `BI_DESCRIPTION` (derivata dai percorsi e dal repository)
Testo libero inviato con ogni notifica. È il campo che leggi alle 3 di notte
quando arriva l'allarme: scrivi *cosa* protegge questo job e *chi* lo governa,
non "backup".

```bash
BI_DESCRIPTION="Dump PostgreSQL cluster prod (db01) — on-call: #team-data"
```

#### `BI_WORKDIR` (`/`)
Directory di lavoro del processo. Il default evita che il backup tenga occupato
un mount che qualcun altro vuole smontare. Cambiala solo se un hook ha bisogno
di una cwd particolare.

### Cosa salvare e dove

#### `BI_PATHS` (obbligatoria)
Sorgenti da archiviare, una per riga. Ogni percorso deve esistere: se manca, lo
script esce con 78 **prima** di iniziare, invece di produrre un backup
silenziosamente monco.

```bash
BI_PATHS="/srv/data
/etc/myapp
/var/lib/myapp/uploads"
```

#### `BI_REPO` (obbligatoria)
Repository di destinazione, **senza tag**.

```bash
BI_REPO="ghcr.io/me/dumps"
```

#### `BI_TAG` (`daily`)
Tag da pubblicare. Con `BI_TIMESTAMP=true` è il prefisso a cui viene appeso lo
stamp UTC.

#### `BI_TIMESTAMP` (`true`)
Appende uno stamp UTC al tag: `daily` → `daily-20260824T031500Z`. Un tag per
esecuzione, quindi uno storico invece di un'unica copia sovrascritta. Mettila a
`false` solo se vuoi davvero una sola immagine sempre aggiornata (in quel caso
la retention non serve).

#### `BI_TIMESTAMP_FORMAT` (`—`, cioè `20060102T150405Z`)
Layout Go per lo stamp (data di riferimento `2006-01-02 15:04:05`).

```bash
BI_TIMESTAMP_FORMAT="2006-01-02"    # daily-2026-08-24
```

#### `BI_EXCLUDES` (`—`)
Glob da escludere, uno per riga. La base del pattern è il nome archiviato, non
il percorso assoluto: vedi *Archived path names* nel README principale.

```bash
BI_EXCLUDES="**/*.tmp
**/.cache/**
data/Downloads/*.iso"
```

### Chiavi e credenziali

Serve **una** di queste tre strade: `BI_PASSPHRASE_FILE`, `BI_RECIPIENTS`,
oppure `BI_NO_ENCRYPT=true`. Senza nessuna, exit 78.

#### `BI_PASSPHRASE_FILE` (`—`)
File con la passphrase del backup. `backup` **non** legge `BACKIMAGE_PASSPHRASE`
(a differenza dei comandi di lettura): serve un file. Modo `0600`; lo script
logga un warning se è leggibile da gruppo o altri.

```bash
BI_PASSPHRASE_FILE="/etc/backimage/backup.pass"
```

> Perdere la passphrase significa perdere il backup. Non c'è recupero, per
> progetto. Conservane una copia fuori dalla macchina che stai salvando.

#### `BI_RECIPIENTS` (`—`)
Chiavi pubbliche age, una per riga, in alternativa (o in aggiunta) alla
passphrase. Utile quando il backup deve poter essere aperto da più identità.

```bash
BI_RECIPIENTS="age1qy8...ops
age1lk3...dr"
```

#### `BI_NO_ENCRYPT` (`false`)
Disattiva la cifratura. Solo per dati che non hanno bisogno di riservatezza;
lo script logga comunque un warning a ogni esecuzione.

#### `BI_AGE_IDENTITY` (`—`)
File identity age usato per riusare la chiave di deduplicazione fra esecuzioni.
Necessario perché `BI_DEDUP` produca risparmi nel tempo.

#### `BI_AUTH_FILE` (`—`)
Esportato come `BACKIMAGE_AUTH_FILE`: il file di credenziali del registry
scritto da `backimage login`. **Da impostare quasi sempre nei job cron**: il
cron di root non legge la directory XDG dell'utente con cui hai fatto login, e
il sintomo è un exit 6 per autenticazione mancante.

```bash
BI_AUTH_FILE="/etc/backimage/auth.json"
```

#### `BI_REGISTRY_USER` (`—`)
Login da usare quando il registry ospita più account. Passato sia al backup che
al prune.

### Pipeline

#### `BI_COMPRESSION` (`—`, cioè `zstd`)
Codec dei layer: `zstd`, `gzip`, `lz4`, `xz`, `none`. `xz` e `lz4` producono
immagini non eseguibili con `docker run`: richiedono
`BI_EXTRA_ARGS="--runnable=false"`.

#### `BI_COMPRESSION_LEVEL` (`—`)
Livello del codec: zstd 1–4, gzip 1–9. Più alto = più piccolo e più lento.

#### `BI_JOBS` (`—`, cioè `3`)
Upload di blob concorrenti. Alzalo su link veloci con registry lontani,
abbassalo se saturi la banda della macchina in orario di lavoro.

#### `BI_MAX_LAYER_SIZE` (`—`, cioè `1GiB`)
Dimensione obiettivo di ogni layer OCI. **Non** riduce lo spazio temporaneo
richiesto (vedi `BI_TEMP_DIR`); `docker run` tollera al massimo 118 layer di
dati, quindi backup enormi hanno per forza layer grandi.

#### `BI_TEMP_DIR` (`—`, cioè `$TMPDIR`)
Directory di spool. **Il picco è la dimensione dell'intero backup compresso**,
non di un layer: tutti i layer restano su disco fino alla fine del push. Su
molti sistemi `$TMPDIR` è una tmpfs in RAM, quindi vale la pena puntare a disco
vero. In alternativa, `BI_REMOTE` in modalità `stream` usa 4 KiB sul client.

```bash
BI_TEMP_DIR="/var/tmp/backimage"
```

#### `BI_ONE_FILE_SYSTEM` (`false`)
Non attraversa i punti di mount. Con `true`, `/srv/data` non si porta dietro
una NFS montata sotto.

#### `BI_ALLOW_DEGRADED` (`false`)
Continua quando qualche file non è leggibile per intero, invece di fallire.
Utile su sorgenti vive (file cancellati o troncati durante la lettura); il
prezzo è che il backup può essere incompleto, quindi controlla il log.

#### `BI_DEDUP` (`false`)
Deduplicazione incrementale content-defined. **Rivela a chiunque possa leggere
il registry quali chunk due backup hanno in comune**: leggi `docs/dedup.md`
prima di attivarla. Ha senso solo insieme a `BI_AGE_IDENTITY`.

#### `BI_VERIFY_AFTER_PUSH` (`—`, cioè `quick`)
Rilegge ciò che è stato pubblicato: `quick` (HEAD per blob, GET del manifest —
pochi KB), `full` (riscarica tutto e ricalcola ogni digest: costa il traffico
dell'intero backup), `off`. `quick` è il compromesso giusto per un job
notturno; `full` ha senso settimanalmente, se la banda non è un problema.

#### `BI_OUTPUT` (`—`, cioè `registry`) e `BI_OUTPUT_PATH`
Destinazione: `registry`, `daemon`, `oci-layout`, `tar`. Le ultime due
richiedono `BI_OUTPUT_PATH`.

```bash
BI_OUTPUT="oci-layout"
BI_OUTPUT_PATH="/srv/backup-layout"
```

#### `BI_LOCAL_REPO` (`false`)
Scrive verso il daemon Docker locale invece che verso un registry.

#### `BI_VERBOSE` (`0`)
`1` aggiunge `-v` (debug), `2` aggiunge `-vv` (trace). Finisce nel file di log,
non nella mail di cron.

#### `BI_EXTRA_ARGS` (`—`)
Qualsiasi altro flag di `backimage backup`, diviso sugli spazi.

```bash
BI_EXTRA_ARGS="--platform linux/amd64 --no-metadata --numeric-owner"
```

#### `BI_EXTRA_ARGV` (array, non impostabile da ambiente)
Come sopra, ma per valori che contengono spazi. Solo nel file di
configurazione, perché le array non si esportano.

```bash
BI_EXTRA_ARGV=(--exclude 'My Documents/**' --created '2026-01-01T00:00:00Z')
```

### Backup delegato a un server remoto

Da usare quando la macchina non ha spazio temporaneo: in modalità `stream` è il
server a costruire la pipeline, e il client scrive 4 KiB su disco.

#### `BI_REMOTE` (`—`)
`HOST:PORT` di un `backimage listen-remote`.

#### `BI_REMOTE_MODE` (`—`, cioè `stream`)
`stream` (il server esegue tutta la pipeline) o `layers` (pipeline lato client,
modalità legacy).

#### `BI_TLS_PIN` (`—`)
Fingerprint SHA-256 del certificato del server, solo esadecimale (togli il
prefisso `SHA256:` che il server stampa). È il modo più semplice per fidarsi di
un certificato self-signed.

#### `BI_TLS_CA`, `BI_TLS_CERT`, `BI_TLS_KEY` (`—`)
Bundle CA PEM, e coppia certificato/chiave per mTLS.

#### `BI_AUTH_TOKEN_FILE` (`—`)
File con il token pre-condiviso del server remoto.

#### `BI_UDP` (`false`)
QUIC invece di TCP.

```bash
BI_REMOTE="backup.example:7575"
BI_TLS_PIN="9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
BI_AUTH_TOKEN_FILE="/etc/backimage/remote.token"
```

### Esecuzione

#### `BI_TIMEOUT` (`—`)
Durata massima, con unità (`90m`, `6h`). Allo scadere il processo riceve
`TERM`, poi `KILL` dopo 60 s, e lo script esce con **124**. Vuota o `0`
disattiva il limite. Con un job notturno vale la pena metterlo: meglio un
fallimento notificato che un backup che occupa la macchina fino alle 9.

#### `BI_NICE` (`—`)
Priorità CPU, es. `10`.

#### `BI_IONICE_CLASS` / `BI_IONICE_LEVEL` (`—`)
Classe I/O (`1` realtime, `2` best-effort, `3` idle) e livello 0–7 dentro la
classe. Su una macchina di produzione, `2` + `7` (o classe `3`) tiene il backup
fuori dai piedi.

```bash
BI_NICE="10"
BI_IONICE_CLASS="2"
BI_IONICE_LEVEL="7"
```

#### `BI_LOCK_FILE` (`/var/lock/backimage-<job>.lock`)
File di lock. Se `/var/lock` non è scrivibile, il default diventa
`$TMPDIR/backimage-<job>.lock`. Due esecuzioni dello stesso job non si
sovrappongono mai; job con `BI_JOB_NAME` diversi non si disturbano.

#### `BI_LOCK_WAIT` (`0`)
Secondi di attesa per un lock occupato prima di arrendersi. `0` = non aspetta.

#### `BI_ON_LOCKED` (`fail`)
Cosa fare se il lock è occupato:

- `fail`: exit **75** e notifica di fallimento — è il default perché un run che
  sfora nel successivo di solito è un problema da vedere;
- `skip`: exit **0**, con notifica solo se `BI_NOTIFY=always`. Ha senso per job
  molto frequenti dove la sovrapposizione è normale.

#### `BI_PRE_CMD` (`—`)
Comando shell eseguito **prima** del backup. Se fallisce, il backup non parte e
lo script esce con 1, notificando. Serve a mettere i dati in uno stato
consistente.

```bash
BI_PRE_CMD="pg_dumpall --file=/var/backups/pg.sql && systemctl stop myapp"
```

#### `BI_POST_CMD` (`—`)
Comando shell eseguito **sempre** dopo (successo, fallimento, timeout,
interruzione), prima della notifica. Nel suo ambiente trova:

| Variabile | Contenuto |
|---|---|
| `BI_STATUS` | `OK` o `FAILED` |
| `BI_EXIT_CODE` | exit code di `backimage` |
| `BI_REF` | riferimento immagine pubblicato (vuoto se il backup è fallito) |
| `BI_LOG_FILE` | percorso del log di questa esecuzione |

Un fallimento del post-comando non cambia l'esito del backup: viene solo
loggato.

```bash
BI_POST_CMD='systemctl start myapp; logger -t backimage "$BI_STATUS $BI_REF"'
```

### Log

#### `BI_LOG_DIR` (`/var/log/backimage`)
Directory dei log. Un file per esecuzione:
`${BI_LOG_DIR}/${BI_JOB_NAME}-<UTC>.log`, con le righe del wrapper, tutto lo
stderr di `backimage` e il riepilogo JSON finale. Se non è creabile o
scrivibile, si ripiega su `$TMPDIR/backimage-logs` e lo dice su stderr.

#### `BI_LOG_KEEP` (`30`)
Quanti log dello stesso job tenere. `0` = tienili tutti (allora pensa tu alla
rotazione).

#### `BI_STDERR` (`auto`)
Cosa finisce su stderr — cioè, sotto cron, nella mail:

| Valore | Effetto |
|---|---|
| `auto` | solo warning ed errori. Niente mail quando va bene, mail con exit code e coda del log quando si rompe |
| `always` | tutte le righe del wrapper. Consigliato sotto systemd, così il journal ha tutto |
| `never` | silenzio totale, anche sui fallimenti. Solo se ti fidi delle notifiche webhook |

Il riepilogo finale di un fallimento (exit code, significato, percorso del log,
ultime righe) viene stampato con `auto` e `always`.

### Retention sul registry

Eseguita con `backimage repo prune --yes` **solo dopo un backup riuscito**, e
mai in `--dry-run`. Un prune fallito è un warning: non trasforma un backup
riuscito in un fallimento, ma compare nella notifica come
`Retention: FAILED (exit N)`.

> Le cancellazioni non sono reversibili. Prova sempre prima le regole a mano:
> `backimage repo prune REPO --keep-last 14 --dry-run`.

#### `BI_PRUNE` (`false`)
Attiva la retention. Con `true` serve almeno una regola, altrimenti exit 78.

#### `BI_PRUNE_KEEP_LAST` (`—`)
Tiene gli N backup più recenti, indipendentemente dall'età.

#### `BI_PRUNE_KEEP_WITHIN` (`—`)
Tiene i backup più recenti di questa età. Unità: `s`, `m`, `h`, `d`, `w`.

#### `BI_PRUNE_KEEP_TAGS` (`—`)
Glob di tag da tenere sempre, uno per riga.

#### `BI_PRUNE_TAG_REGEX` (`—`)
Restringe l'operazione ai tag che corrispondono (match sull'intero tag): i tag
fuori dal pattern non vengono mai toccati. È la cintura di sicurezza quando nel
repository convivono famiglie di tag diverse.

#### `BI_PRUNE_EXTRA_ARGS` (`—`)
Altri flag di `repo prune`, divisi sugli spazi (es. `--group-by-regex`).

Un tag è tenuto se **almeno una** regola lo seleziona; senza nessuna regola non
si cancella niente. I tag senza timestamp di creazione sono sempre tenuti,
quindi un tag non prodotto da backimage non sparisce per sbaglio.

```bash
BI_PRUNE="true"
BI_PRUNE_KEEP_LAST="14"
BI_PRUNE_KEEP_WITHIN="30d"
BI_PRUNE_KEEP_TAGS="release-*"
BI_PRUNE_TAG_REGEX="daily-.*"
```

### Notifiche

#### `BI_NOTIFY` (`on-error`)

| Valore | Quando notifica |
|---|---|
| `always` | ogni esecuzione: successo, fallimento, run saltato |
| `on-error` | solo fallimenti: backup fallito, timeout (124), lock occupato con `BI_ON_LOCKED=fail` (75), errore di configurazione (78), interruzione (7) |
| `never` | mai, e `curl` non serve |

#### `BI_NOTIFY_TARGET` (`auto`)
`slack`, `google-chat`, oppure `auto`: dedotto dall'host del webhook
(`hooks.slack.com` → Slack, `chat.googleapis.com` → Google Chat). Se passi da
un proxy o da un relay, l'host non è riconoscibile e devi impostarlo a mano —
altrimenti exit 78 in validazione, non a notifica fallita.

#### `BI_WEBHOOK_URL_FILE` (`—`) — **preferito**
File contenente l'URL del webhook (prima riga). Tiene il segreto fuori
dall'ambiente del processo e dai listati di `ps`. Ha precedenza su
`BI_WEBHOOK_URL`. Modo `0600`.

#### `BI_WEBHOOK_URL` (`—`)
L'URL direttamente. Comodo per una prova, meno per la produzione.
`--print-config` lo redige comunque.

#### `BI_NOTIFY_TIMEOUT` (`15`)
Secondi per tentativo HTTP.

#### `BI_NOTIFY_RETRIES` (`3`)
Tentativi totali. Si ritenta su 429, 5xx e errori di trasporto, con backoff
lineare (5 s, 10 s, …); su un 4xx definitivo lo script rinuncia subito.
**Una notifica non consegnata non cambia l'exit code del backup**: viene
scritta nel log come `ERROR`.

#### `BI_NOTIFY_TAIL` (`20`)
Righe finali di log allegate a una notifica di fallimento, in un blocco di
codice. Ogni riga è tagliata a 300 caratteri e il blocco a ~2200, per stare nei
limiti dei due servizi. `0` disattiva l'allegato.

#### `BI_NOTIFY_MENTION` (`—`)
Testo inserito nel titolo delle notifiche **non riuscite**, per farsi taggare.

```bash
BI_NOTIFY_MENTION="<!channel>"     # Slack: anche <@U01ABCDEF>
BI_NOTIFY_MENTION="<users/all>"    # Google Chat
```

#### `BI_HOSTNAME` (nome della macchina)
Nome mostrato nella notifica. Utile quando l'hostname reale è un id inutile
(`ip-10-0-3-14`) o quando il job gira in un container.

```bash
BI_HOSTNAME="db01.prod"
```

---

## Notifiche in pratica

**Slack.** Crea una Slack app → *Incoming Webhooks* → *Add New Webhook to
Workspace*, scegli il canale, copia l'URL `https://hooks.slack.com/services/…`.
Il payload è un attachment colorato (verde successo, rosso fallimento, giallo
run saltato) con un blocco `mrkdwn`.

**Google Chat.** Nello spazio: *Apps & integrations* → *Webhooks* → *Add
webhook*, copia l'URL `https://chat.googleapis.com/v1/spaces/…?key=…`. Il
payload è un messaggio `text` con la stessa formattazione.

Messaggio reale di successo:

```
✅ *BACKUP OK* — prod-data
*Description:* Backup notturno di /srv/data (upload applicativi) — team platform
*Host:* db01.prod    *Started:* 2026-08-24T03:15:00Z
*Repository:* ghcr.io/me/dumps
*Image:* `ghcr.io/me/dumps:daily-20260824T031500Z`
*Digest:* `sha256:d394398559…`
*Files:* 128432    *Source:* 41.7 GiB → *Stored:* 12.3 GiB
*Uploaded:* 3.1 GiB    *Reused:* 9.2 GiB
*Duration:* 00h48m12s
*Retention:* ok, 3 tag(s) removed
*Log:* `/var/log/backimage/prod-data-20260824T031500Z.log`
```

E di fallimento (stessi campi più il motivo e la coda del log):

````
🚨 *BACKUP FAILED* — prod-data <!channel>
…
*Detail:* exit code 6: network or registry error
*Log:* `/var/log/backimage/prod-data-20260824T031500Z.log`
```
2026-08-24T03:15:44Z [ERROR] backup failed with exit code 6
error: push: unauthorized: authentication required
```
````

Prova sempre il canale prima di affidarti al job: `--test-notify`.

## Schedulazione

```cron
# /etc/cron.d/backimage
SHELL=/bin/bash
MAILTO=ops@example.com

15 3 * * *  root  /usr/local/bin/backimage-backup -c /etc/backimage/backup.env
30 4 * * 0  root  BI_TAG=weekly BI_PRUNE_KEEP_LAST=8 /usr/local/bin/backimage-backup -c /etc/backimage/backup.env
```

Il backup gira come l'utente del crontab: per preservare owner, device node,
ACL e xattr `trusted.*` serve `root`.

Con systemd, unit e timer di esempio sono in [`../docs/cron.md`](../docs/cron.md);
lì conviene `Environment=BI_STDERR=always` così il journal contiene tutto.

## Exit code

Lo script propaga l'exit code di `backimage backup` e ne aggiunge tre suoi.

| Codice | Significato | Chi lo produce |
|---|---|---|
| 0 | successo (o run saltato con `BI_ON_LOCKED=skip`) | |
| 1 | errore generico, o `BI_PRE_CMD` fallito (backup non eseguito) | backimage / wrapper |
| 2 | errore d'uso della CLI | backimage |
| 3 | privilegi insufficienti | backimage |
| 4 | passphrase mancante o sbagliata | backimage |
| 5 | fallimento di integrità | backimage |
| 6 | errore di rete o del registry | backimage |
| 7 | interrotto (SIGINT/SIGTERM: il figlio viene terminato) | entrambi |
| 75 | un'altra esecuzione dello stesso job tiene il lock | wrapper |
| 78 | errore di configurazione: nessun backup eseguito | wrapper |
| 124 | superato `BI_TIMEOUT` | wrapper |

## Anatomia di un'esecuzione

1. legge la configurazione (ambiente > file > default);
2. valida **prima** di toccare i dati: binario, `flock`, percorsi sorgente,
   materiale crittografico, webhook e flavour, regole di retention;
3. apre il log del run e prende il lock esclusivo;
4. esegue `BI_PRE_CMD`;
5. lancia `backimage backup … --json` sotto `timeout`/`nice`/`ionice`: stdout
   (il riepilogo JSON) viene raccolto, stderr va nel log;
6. su successo, esegue la retention se attiva;
7. esegue `BI_POST_CMD`;
8. invia la notifica secondo la policy;
9. ruota i log ed esce con l'exit code di `backimage`.

## Troubleshooting

**`exit 78` all'avvio.** Errore di configurazione: la riga `ERROR` nel log (e
su stderr) dice quale. Cause tipiche: percorso sorgente inesistente, nessun
materiale crittografico, webhook assente con `BI_NOTIFY` attivo, `BI_PRUNE`
senza regole, `flock` non installato.

**`exit 6` solo da cron, a mano funziona.** Credenziali del registry: il cron
di root non legge la tua directory XDG. Imposta `BI_AUTH_FILE`.

**`exit 75` ogni notte.** Il run precedente non è finito. Guarda la durata nei
log: o alzi la finestra (cron meno frequente), o metti `BI_TIMEOUT`, o passi a
`BI_ON_LOCKED=skip` se la sovrapposizione è accettabile.

**Il backup fallisce per spazio temporaneo.** Il picco è l'intero backup
compresso, non un layer: `BI_TEMP_DIR` su disco vero, oppure `BI_REMOTE` in
modalità `stream`. Ridurre `BI_MAX_LAYER_SIZE` o `BI_JOBS` non abbassa il
requisito.

**Notifiche mute.** `--test-notify` mostra l'errore esatto. Un 4xx immediato è
un webhook revocato o un URL sbagliato; `unknown notification target` significa
che l'host non è riconoscibile: imposta `BI_NOTIFY_TARGET`.

**Niente mail da cron nemmeno sui fallimenti.** `BI_STDERR=never`, oppure
`MAILTO` non impostato nel crontab.

**Il file di configurazione sembra ignorato.** Una variabile con lo stesso nome
è già nell'ambiente: l'ambiente vince. Verifica con `--print-config`.
