# Backup

`backimage backup` archivia i percorsi indicati, comprime e opzionalmente
cifra i chunk, costruisce un'immagine OCI multi-architettura e la pubblica.

## Esempio consigliato

```sh
printf '%s\n' 'una passphrase lunga' > passphrase.txt
chmod 600 passphrase.txt
backimage login ghcr.io --username USER --password-stdin
backimage backup /srv/data \
  --repo ghcr.io/USER/backups \
  --tag daily \
  --passphrase-file passphrase.txt
```

Per verificare il piano senza prompt, rete o scritture:

```sh
backimage backup /srv/data --repo ghcr.io/USER/backups \
  --dry-run --no-encrypt --json
```

## Pipeline e spazio temporaneo

Prima di leggere i dati il comando valida i flag e i privilegi. Per un backup
reale verifica anche credenziali e scope `pull,push` del repository. Il flusso
successivo è archive → chunk → compressione → cifratura → layer OCI → push.

I layer sono appoggiati su disco e letti in streaming durante il push. Il
preflight richiede spazio libero pari a circa:

```
jobs × max-layer-size
```

Con i default (`--jobs 3`, `--max-layer-size 1GiB`) servono quindi almeno
3 GiB nella directory temporanea. Usare `--temp-dir`, ridurre il numero di job
o la dimensione dei layer se il controllo fallisce. La memoria non cresce con
la dimensione complessiva del backup: il test slow impone un limite di 512 MiB.

## Cifratura

La cifratura è attiva per default e richiede almeno uno dei seguenti:

- `--passphrase-file FILE`;
- `--passphrase-stdin`;
- `--password PASSWORD` (semplice, ma visibile nella history e nei processi);
- uno o più `--recipient AGE_PUBLIC_KEY`.

`--no-encrypt` disabilita la cifratura. Non può essere combinato con chiavi o
passphrase. `--password` è una scorciatoia per la passphrase del backup, non è
la password del registry e mostra il segreto nella history e nella lista dei
processi; per automazioni preferire file, stdin o variabile d'ambiente.

## Ripresa

`--resume` è attivo per default. Dopo ogni blob confermato il comando salva un
checkpoint atomico sotto `$XDG_CACHE_HOME/backimage/checkpoints` (o
`~/.cache/backimage/checkpoints`). Il nome dipende da reference, sorgenti,
codec, livello, dimensione layer, cifratura e versione. Un rilancio con gli
stessi parametri salta i blob già confermati; il checkpoint viene eliminato
solo dopo la pubblicazione del manifest finale.

## Output

| Opzione | Destinazione |
|---|---|
| `--output registry` | registry OCI indicato da `--repo` (default) |
| `--local-repo` / `--output daemon` | daemon Docker locale |
| `--output oci-layout --output-path DIR` | OCI image layout |
| `--output tar --output-path FILE` | archivio OCI tar |

`--local-repo` non può essere combinato con `--output`.

## Flag

| Flag | Default | Significato |
|---|---:|---|
| `--repo` | — | repository di destinazione, obbligatorio |
| `--tag` | `latest` | tag dell'immagine |
| `--timestamp` | false | aggiunge un timestamp UTC al tag |
| `--timestamp-format` | `20060102T150405Z` | layout Go del timestamp |
| `--compression` | `zstd` | `zstd`, `gzip`, `xz`, `lz4`, `none` |
| `--compression-level` | codec | livello del codec |
| `--max-layer-size` | `1GiB` | obiettivo per un layer dati |
| `--encrypt` | true | cifra i chunk |
| `--no-encrypt` | false | backup in chiaro |
| `--passphrase-file` | — | legge la passphrase da file |
| `--passphrase-stdin` | false | legge la passphrase da stdin |
| `--password` | — | passphrase diretta; visibile in history e processi |
| `--recipient` | — | destinatario age, ripetibile |
| `--jobs` | 3 | upload paralleli |
| `--platform` | amd64, arm64 | piattaforma, ripetibile |
| `--output` | `registry` | destinazione |
| `--output-path` | — | path per layout/tar |
| `--local-repo` | false | alias per output daemon |
| `--exclude` | — | glob escluso, ripetibile |
| `--one-file-system` | false | non attraversa mount point |
| `--numeric-owner` | false | non risolve nomi utente/gruppo |
| `--allow-degraded` | false | continua sulle feature non disponibili |
| `--no-metadata` | false | omette sorgenti e host dalle label |
| `--dry-run` | false | piano senza rete o scritture |
| `--resume` | true | usa checkpoint |
| `--runnable` | true | include il binario auto-estraente |
| `--temp-dir` | `$TMPDIR` | spool dei layer |
| `--json` | false | risultato JSON su stdout |

I codec `xz` e `lz4` richiedono `--runnable=false` perché i relativi media
type non sono portabili nei runtime OCI comuni.
