# Backup schedulati con cron

`contrib/backimage-backup.sh` è un wrapper attorno a `backimage backup` pensato
per girare da cron senza sorveglianza: prende un lock, scrive un log per ogni
esecuzione, esegue il backup, opzionalmente applica la retention sul registry e
notifica l'esito su Slack o Google Chat via webhook.

Lo script non aggiunge logica di backup: costruisce la riga di comando,
propaga l'exit code di `backimage` e racconta cosa è successo.

## Cosa fa, in ordine

1. legge la configurazione (file + variabili d'ambiente, l'ambiente vince);
2. valida tutto *prima* di toccare i dati: binario, percorsi sorgente, materiale
   crittografico, webhook, regole di retention;
3. prende un lock esclusivo (`flock`), così due esecuzioni non si sovrappongono
   mai — il caso classico del backup notturno che sfora nell'esecuzione
   successiva;
4. esegue l'eventuale `BI_PRE_CMD`;
5. lancia `backimage backup … --json`, con `timeout`/`nice`/`ionice` se
   configurati; stdout (il riepilogo JSON) viene raccolto, stderr finisce nel
   log del run;
6. su successo esegue `backimage repo prune` se la retention è attiva (un prune
   fallito è un warning, non trasforma un backup riuscito in un fallimento);
7. esegue `BI_POST_CMD` (sempre, con `BI_STATUS`, `BI_EXIT_CODE`, `BI_REF`,
   `BI_LOG_FILE` nell'ambiente);
8. invia la notifica secondo la policy, ruota i log, esce con l'exit code di
   `backimage`.

## Installazione

```console
sudo install -m 0755 contrib/backimage-backup.sh /usr/local/bin/backimage-backup
sudo install -d -m 0700 /etc/backimage
sudo install -m 0600 contrib/backimage-backup.env.example /etc/backimage/backup.env
sudo install -d -m 0750 /var/log/backimage
```

Poi le credenziali, che vanno preparate una volta sola:

```console
# passphrase del backup (perderla significa perdere i dati)
umask 077 && backimage genpass | sudo tee /etc/backimage/backup.pass >/dev/null
sudo chmod 600 /etc/backimage/backup.pass

# credenziali del registry, nel file che leggerà il cron di root
printf '%s\n' "$REGISTRY_TOKEN" | sudo env BACKIMAGE_AUTH_FILE=/etc/backimage/auth.json \
  backimage login ghcr.io --username me --password-stdin

# URL del webhook, fuori dall'ambiente e da `ps`
printf '%s\n' 'https://hooks.slack.com/services/T000/B000/xxxx' \
  | sudo tee /etc/backimage/webhook.url >/dev/null
sudo chmod 600 /etc/backimage/webhook.url
```

Dipendenze: `bash` 4+, `flock` (util-linux), `curl` (solo per le notifiche),
`timeout` (solo per `BI_TIMEOUT`), `jq` opzionale — senza `jq` il riepilogo JSON
viene letto da un parser di ripiego.

## Configurazione

Tutta la configurazione sono variabili `BI_*`. Il file di default è
`/etc/backimage/backup.env`, sovrascrivibile con `-c FILE` o con `BI_CONFIG`.
Il file viene sourcato da bash; **una variabile già presente nell'ambiente
vince sul file**, il che rende un job facile da variare senza duplicare la
configurazione:

```console
BI_TAG=weekly BI_PRUNE_KEEP_LAST=8 backimage-backup -c /etc/backimage/backup.env
```

`contrib/backimage-backup.env.example` documenta ogni variabile. Le essenziali:

| Variabile | Default | Significato |
|---|---|---|
| `BI_JOB_NAME` | `backup` | id breve: nome del lock, dei log, titolo della notifica |
| `BI_DESCRIPTION` | derivata | testo libero inviato con la notifica |
| `BI_PATHS` | — | sorgenti, una per riga (obbligatoria) |
| `BI_REPO` | — | repository senza tag (obbligatoria) |
| `BI_TAG` / `BI_TIMESTAMP` | `daily` / `true` | tag pubblicato, con stamp UTC |
| `BI_PASSPHRASE_FILE` | — | passphrase del backup; in alternativa `BI_RECIPIENTS` o `BI_NO_ENCRYPT=true` |
| `BI_AUTH_FILE` | — | `BACKIMAGE_AUTH_FILE` per il processo |
| `BI_TEMP_DIR` | `$TMPDIR` | spool: deve contenere l'intero backup compresso |
| `BI_TIMEOUT` | — | limite di durata, es. `6h`; superarlo dà exit 124 |
| `BI_ON_LOCKED` | `fail` | `fail` (exit 75 + notifica) o `skip` (exit 0) |
| `BI_LOG_DIR` / `BI_LOG_KEEP` | `/var/log/backimage` / `30` | log per run e rotazione |
| `BI_NOTIFY` | `on-error` | `always`, `on-error`, `never` |
| `BI_NOTIFY_TARGET` | `auto` | `slack`, `google-chat`, o dedotto dall'URL |
| `BI_WEBHOOK_URL_FILE` | — | file con l'URL del webhook (preferito a `BI_WEBHOOK_URL`) |
| `BI_PRUNE*` | disattiva | retention via `repo prune` dopo un backup riuscito |
| `BI_EXTRA_ARGS` / `BI_EXTRA_ARGV` | — | qualsiasi altro flag di `backup` |

Ogni flag di `backup` non esposto come variabile dedicata si passa con
`BI_EXTRA_ARGS` (split sugli spazi) o, se contiene spazi, con l'array
`BI_EXTRA_ARGV=(--exclude 'my dir/**')` nel file di configurazione.

### Più job sulla stessa macchina

Un file per job, un `BI_JOB_NAME` diverso per ciascuno: lock, log e notifiche
restano separati.

```console
/etc/backimage/data.env      # BI_JOB_NAME="srv-data"
/etc/backimage/postgres.env  # BI_JOB_NAME="postgres", BI_PRE_CMD="pg_dumpall -f /var/backups/pg.sql"
```

## Notifiche

`BI_NOTIFY=always` notifica ogni esecuzione, `on-error` solo i fallimenti
(inclusi lock occupato con `BI_ON_LOCKED=fail`, timeout ed errori di
configurazione), `never` disattiva tutto.

Il messaggio contiene la descrizione, host, repository, riferimento immagine e
digest pubblicati, file e byte (sorgente, memorizzati, caricati, riusati),
durata, esito della retention e percorso del log. In caso di fallimento
aggiunge exit code con la sua interpretazione e le ultime `BI_NOTIFY_TAIL`
righe di log in un blocco di codice.

- **Slack**: incoming webhook (`https://hooks.slack.com/services/...`). Il
  payload è un attachment colorato (verde/rosso/giallo) con un blocco `mrkdwn`.
- **Google Chat**: webhook di uno spazio
  (`https://chat.googleapis.com/v1/spaces/.../messages?key=...`). Il payload è
  un messaggio `text`, con la stessa formattazione `*grassetto*` e ``` ``` ```.

Il flavour si deduce dall'host dell'URL; se usi un proxy o un relay, imposta
`BI_NOTIFY_TARGET` esplicitamente. Gli invii vengono ritentati
(`BI_NOTIFY_RETRIES`, backoff lineare) su 429, 5xx e errori di trasporto; su un
4xx definitivo lo script rinuncia e lo scrive nel log — **una notifica non
consegnata non cambia l'exit code del backup**.

Prova la configurazione senza fare backup:

```console
backimage-backup -c /etc/backimage/backup.env --test-notify
backimage-backup -c /etc/backimage/backup.env --print-config   # URL redatto
backimage-backup -c /etc/backimage/backup.env --dry-run        # nessuna scrittura
```

Per farsi taggare sui fallimenti: `BI_NOTIFY_MENTION='<!channel>'` su Slack,
`BI_NOTIFY_MENTION='<users/all>'` su Google Chat.

## crontab

```cron
# /etc/cron.d/backimage — root
SHELL=/bin/bash
MAILTO=ops@example.com

# ogni notte alle 03:15
15 3 * * *  root  /usr/local/bin/backimage-backup -c /etc/backimage/backup.env

# settimanale, stesso file, tag e retention diversi
30 4 * * 0  root  BI_TAG=weekly BI_PRUNE_KEEP_LAST=8 /usr/local/bin/backimage-backup -c /etc/backimage/backup.env
```

Con `BI_STDERR=auto` (default) cron riceve output solo su warning ed errori:
niente mail quando tutto va bene, mail con exit code e coda del log quando
qualcosa si rompe. `BI_STDERR=never` silenzia anche quelli (utile se le
notifiche webhook bastano), `always` manda tutto.

Il backup gira come l'utente del crontab: per preservare owner, device node,
ACL e xattr `trusted.*` serve `root`.

### systemd, in alternativa

```ini
# /etc/systemd/system/backimage-backup.service
[Unit]
Description=backimage nightly backup
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/backimage-backup -c /etc/backimage/backup.env
Environment=BI_STDERR=always
Nice=10
IOSchedulingClass=idle
```

```ini
# /etc/systemd/system/backimage-backup.timer
[Unit]
Description=backimage nightly backup

[Timer]
OnCalendar=*-*-* 03:15:00
RandomizedDelaySec=15m
Persistent=true

[Install]
WantedBy=timers.target
```

Con systemd conviene `BI_STDERR=always`: il log finisce anche nel journal.

## Exit code

Lo script propaga l'exit code di `backimage backup`
(vedi [`docs/security.md`](security.md)) e ne aggiunge tre suoi:

| Codice | Significato |
|---|---|
| 0 | successo (o esecuzione saltata con `BI_ON_LOCKED=skip`) |
| 1 | errore generico, o `BI_PRE_CMD` fallito (il backup non è partito) |
| 2 | errore d'uso della CLI |
| 3 | privilegi insufficienti |
| 4 | passphrase mancante o sbagliata |
| 5 | fallimento di integrità (digest discordanti, o un blob non autenticato in un backup cifrato) |
| 6 | errore di rete o del registry |
| 7 | interrotto (SIGINT/SIGTERM: il processo figlio viene terminato) |
| 75 | un'altra esecuzione dello stesso job tiene il lock |
| 78 | errore di configurazione del wrapper (nessun backup eseguito) |
| 124 | superato `BI_TIMEOUT` |

## Log

Un file per esecuzione, `${BI_LOG_DIR}/${BI_JOB_NAME}-<UTC>.log`, con le righe
del wrapper, tutto stderr di `backimage` e il riepilogo JSON finale. La
rotazione tiene i `BI_LOG_KEEP` più recenti dello stesso job. Se `BI_LOG_DIR`
non è scrivibile lo script ripiega su `$TMPDIR/backimage-logs` e lo segnala.

## Note operative

- **Spazio temporaneo**: il picco è la dimensione dell'intero backup compresso,
  non un layer. `BI_TEMP_DIR` su disco vero (il default `$TMPDIR` è spesso una
  tmpfs in RAM), oppure `BI_REMOTE` con `BI_REMOTE_MODE=stream`, che sul client
  usa 4 KiB. Vedi [`docs/backup.md`](backup.md).
- **Permessi dei segreti**: passphrase e webhook a `0600`. Lo script logga un
  warning se la passphrase è leggibile da gruppo o altri, e non stampa mai
  l'URL del webhook (`--print-config` lo redige).
- **Retention**: attiva `BI_PRUNE` solo dopo aver verificato le regole con
  `backimage repo prune REPO --keep-last N --dry-run`. Le cancellazioni non
  sono reversibili.
- **Verifica**: `BI_VERIFY_AFTER_PUSH=quick` (default consigliato) rilegge
  digest di blob e manifest dopo il push; `full` riscarica tutto, costa il
  traffico dell'intero backup. Un restore di prova periodico resta l'unica
  prova vera che il backup serva a qualcosa.
