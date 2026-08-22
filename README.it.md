# backimage

I tuoi file, archiviati e cifrati dentro un'immagine OCI multi-arch. L'immagine è
anche un programma auto-estraente, quindi la macchina che ripristina i dati ha
bisogno di `docker` e di nient'altro: nessun tool da installare, nessun agente,
nessun server di backup. Il registry fa da storage.

*This page is also available [in English](README.md).*

```console
backimage backup /srv/data --repo ghcr.io/me/dumps --tag daily --passphrase-file ./pass
docker run --rm --privileged -v "$PWD/restore:/restore" \
  ghcr.io/me/dumps:daily extract --out /restore
```

## Installazione

```console
# archivio di release
tar -xzf backimage_*.tar.gz && sudo install -m 0755 backimage /usr/local/bin/
backimage version

# da sorgente (Go 1.26+)
go install github.com/manprint/backimage/cmd/backimage@latest

# container
docker run --rm ghcr.io/manprint/backimage:latest version
```

## Il primo backup in quattro comandi

```console
# 1. una passphrase che vale qualcosa: 32 caratteri casuali, ~180 bit
umask 077 && backimage genpass > backup.pass

# 2. credenziali del registry (non la passphrase del backup)
printf '%s\n' "$REGISTRY_TOKEN" | backimage login ghcr.io --username me --password-stdin

# 3. backup cifrato, un tag per esecuzione
backimage backup /srv/data --repo ghcr.io/me/dumps --tag daily --timestamp \
  --passphrase-file ./backup.pass

# 4. dimostra che è integro, prima di averne bisogno
backimage verify ghcr.io/me/dumps:daily-20260822T031500Z --passphrase-file ./backup.pass
```

Se perdi la passphrase i dati sono persi. Non esiste una via di recupero, per
scelta progettuale.

---

## Comandi

Ogni comando accetta `--json` per l'output leggibile da script, `-q` per
silenziare l'avanzamento e `-v`/`-vv` per più log. `backimage <comando> --help` è
il riferimento completo; [`docs/cli.md`](docs/cli.md) ne è la versione generata.

### `backup` — archivia, cifra, pubblica

```console
# una o più radici, archiviate insieme
backimage backup /var/lib/myapp /etc/myapp/app.conf --repo ghcr.io/me/dumps --tag app

# guarda il piano senza scrivere nulla
backimage backup /srv/data --repo ghcr.io/me/dumps --dry-run --json

# escludi sottoalberi (per la base dei pattern vedi "Nomi dei path archiviati")
backimage backup /home/alice --repo ghcr.io/me/dumps --tag home \
  --exclude 'alice/.cache/**' --exclude 'alice/Downloads/*.iso'

# un artefatto locale invece del registry
backimage backup /srv/data --repo local/dumps --tag t --output oci-layout --output-path ./layout
backimage backup /srv/data --repo local/dumps --tag t --local-repo   # daemon Docker
```

| Flag | Cosa fa |
| --- | --- |
| `--repo` | repository di destinazione, senza tag (obbligatorio) |
| `--tag`, `--timestamp` | tag da pubblicare; `--timestamp` aggiunge un marcatore UTC, un tag per esecuzione |
| `--exclude` | glob da saltare, ripetibile |
| `--one-file-system` | non attraversare altri filesystem |
| `--compression`, `--compression-level` | `zstd` (default, livelli 1–4), `gzip` (1–9), `lz4`, `xz`, `none` |
| `--max-layer-size` | dimensione obiettivo del layer, default `1GiB` |
| `--output`, `--output-path` | `registry` (default), `daemon`, `oci-layout`, `tar` |
| `--dedup` | deduplicazione incrementale (vedi l'avvertenza sotto) |
| `--dry-run` | stampa il piano ed esce |
| `--allow-degraded` | prosegue quando alcuni file non sono leggibili per intero |
| `--created` | data di creazione fissa, per immagini riproducibili |

`xz` e `lz4` usano media type OCI non standard e richiedono quindi
`--runnable=false`; l'immagine diventa un artefatto da ripristinare con la CLI,
non qualcosa che `docker run` esegue. `--dedup` rivela a chi può leggere il
registry quali chunk hanno in comune due backup: leggere
[`docs/dedup.md`](docs/dedup.md) prima di abilitarlo.

### `restore` — di nuovo su disco, oppure in un tar

```console
# estrai in una directory
backimage restore ghcr.io/me/dumps:daily -x -C ./restore --passphrase-file ./pass

# solo una parte
backimage restore ghcr.io/me/dumps:daily -x -C ./restore \
  --include '**/*.pdf' --exclude '**/tmp/**' --passphrase-file ./pass

# un file tar, oppure stdout
backimage restore ghcr.io/me/dumps:daily -o ./backup.tar --passphrase-file ./pass
backimage restore ghcr.io/me/dumps:daily -o - --passphrase-file ./pass | tar -tv

# da una sorgente locale invece del registry
backimage restore local/dumps:t --oci-layout ./layout -x -C ./restore
```

Senza `-x` né `-o` il tar va su stdout. Il digest in chiaro di ogni chunk viene
verificato prima di scrivere qualsiasi cosa; `--strict` rifiuta di degradare
anche una sola operazione di metadati, `--continue` salva tutto ciò che verifica
e dichiara ciò che è andato perso. Per ownership, device node, ACL e xattr
`trusted.*` il restore va eseguito come root.

### `restore` senza installare nulla

L'immagine si ripristina da sé. È il senso del formato:

```console
docker run --rm --privileged \
  -e BACKIMAGE_PASSPHRASE="$(cat ./pass)" \
  -v "$PWD/restore:/restore" \
  ghcr.io/me/dumps:daily extract --out /restore
```

`--privileged` è ciò che compra la fedeltà massima (ownership, device, ACL, xattr
overlayfs). Senza, l'estrazione riesce comunque e il riepilogo dichiara quali
classi di metadati sono state degradate. Su Docker Desktop per macOS e Windows è
preferibile prendere il `tar` ed estrarlo sull'host.

### `inspect`, `ls`, `find` — guardare prima di ripristinare

```console
backimage inspect ghcr.io/me/dumps:daily                      # metadati pubblici, senza passphrase
backimage inspect ghcr.io/me/dumps:daily --layers --json
backimage ls   ghcr.io/me/dumps:daily var/log -l --passphrase-file ./pass
backimage find ghcr.io/me/dumps:daily '**/*.conf' --passphrase-file ./pass
```

Layout, compressione e impostazioni di cifratura sono pubblici. I path sorgente,
l'host e l'elenco dei file stanno nei metadati cifrati e richiedono quindi la
passphrase. Leggere l'indice non scarica alcun layer di dati.

### `verify` — prima di fidarsi

```console
backimage verify ghcr.io/me/dumps:daily --quick                 # metadati e digest dei layer
backimage verify ghcr.io/me/dumps:daily --continue --passphrase-file ./pass
```

Il controllo integrale riscarica ogni layer e ricalcola ogni digest di chunk,
quindi costa in traffico quanto il backup. Il codice di uscita 5 significa
integrità fallita.

### `repo` — il ciclo di vita di ciò che hai pubblicato

```console
backimage repo tags  ghcr.io/me/dumps            # tag, digest, data di creazione
backimage repo stats ghcr.io/me/dumps            # blob unici e condivisi, storage reale
backimage repo caps  ghcr.io                     # cosa permette questo registry
backimage repo rm    ghcr.io/me/dumps:old --yes  # un manifest
```

`prune` applica una policy di retention. Un tag sopravvive se **almeno una**
regola lo conserva, ed è eliminato solo quando nessuna lo fa; senza alcuna regola
non viene eliminato nulla, e un tag senza data di creazione non viene mai
toccato.

```console
# conserva i 7 più recenti — guardare sempre prima
backimage repo prune ghcr.io/me/dumps --keep-last 7 --dry-run

# elimina tutto ciò che è più vecchio di 3 giorni, tenendo i 2 più recenti e ogni release-*
backimage repo prune ghcr.io/me/dumps --delete-older-than 3d \
  --keep-last 2 --keep-tag 'release-*' --yes
```

Quando un repository ospita più famiglie di backup (`db_1..db_N` accanto ad
`app_1..app_N`), `--keep-last 3` da solo significherebbe «3 in tutto il
repository». Due selettori risolvono il problema:

```console
# dei backup del database tieni i 3 più recenti; ogni altro tag resta dov'è
backimage repo prune ghcr.io/me/dumps --tag-regex 'db_.*' --keep-last 3 --dry-run

# tieni i 3 più recenti di ogni famiglia in un solo passaggio
backimage repo prune ghcr.io/me/dumps --group-by-regex '([a-z]+)_.*' --keep-last 3 --dry-run

# verifica la selezione con un comando che non può eliminare nulla
backimage repo tags ghcr.io/me/dumps --tag-regex 'db_.*'
```

Tre proprietà da conoscere, perché sono ciò che impedisce una cancellazione
sbagliata:

1. **Una regex non è mai una regola di cancellazione.** Restringe soltanto ciò
   che le regole raggiungono: `--tag-regex 'db_.*'` da solo non elimina nulla.
2. **Il pattern deve corrispondere al tag intero.** `db_` non seleziona niente,
   `db_.*` seleziona `db_1`. La semantica *unanchored* di Go avrebbe fatto
   selezionare a `db` anche `app_db_1`. La sintassi è RE2: nessun lookahead,
   nessuna backreference, `(?i)` per ignorare le maiuscole.
3. **Ciò che il selettore esclude non consuma slot.** I tag fuori ambito sono
   conservati e non contano per nessuna regola.

La cancellazione avviene per digest del manifest, quindi due tag sullo stesso
manifest se ne vanno insieme. `prune` verifica l'intero piano prima della prima
richiesta e rifiuta elencando i tag coinvolti, invece di fermarsi a metà.

### `login`, `logout` — credenziali dei registry

```console
printf '%s\n' "$PAT" | backimage login ghcr.io --username me --password-stdin
backimage login --list                       # account, mai i segreti
backimage logout ghcr.io
```

Più account sullo stesso registry convivono; il namespace del repository ne
sceglie uno e `--registry-user NOME` sovrascrive la scelta (`--registry-user
none` forza una richiesta anonima). `--registry-user` è globale e vale per ogni
comando che parla con un registry.

```console
backimage backup /srv/data --repo ghcr.io/team/dumps --registry-user me
backimage logout docker.io --user user2      # un account
backimage logout docker.io --all             # tutti
```

Le credenziali finiscono in `$XDG_CONFIG_HOME/backimage/auth.json` (oppure
`BACKIMAGE_AUTH_FILE`), nel formato `auths` di Docker, con permessi `0600`, e
sono rifiutate se leggibili da gruppo o da altri. La configurazione Docker è il
fallback. [`docs/registries.md`](docs/registries.md) riporta l'ordine di
risoluzione e le note per ogni vendor.

### `genpass` — una passphrase che vale qualcosa

```console
backimage genpass                       # 32 caratteri, ~180 bit
backimage genpass --length 48
backimage genpass --no-symbols          # per campi che rifiutano la punteggiatura
```

Il file chiavi viaggia dentro l'immagine, quindi chi possiede l'immagine può
provare passphrase offline senza limiti. Una frase inventata di 24 caratteri vale
una trentina di bit e cade in ore.

### `doctor` — controlla prima del backup, non dopo

```console
backimage doctor                                   # solo l'ambiente
sudo backimage doctor /srv/data /var/lib/postgresql
```

Dice se quelle sorgenti sono leggibili per intero (ownership, xattr, ACL, file
sparsi) e indica il rimedio per ogni capacità mancante.

### `listen-remote` — far fare il lavoro a un server

```console
# server
backimage listen-remote --bind-address 0.0.0.0:7575 --tls-self-signed \
  --auth-token-file ./token --allow-repo 'ghcr.io/me/*'

# client: dalla macchina esce solo il flusso tar
backimage backup /srv/data --repo ghcr.io/me/dumps --tag daily \
  --remote backup.example:7575 --tls-pin <PIN> \
  --auth-token-file ./token --passphrase-file ./pass
```

Nella modalità predefinita `stream` il server esegue l'intera pipeline, quindi il
client non tiene mai né l'archivio né un layer e il suo uso di disco non cresce
con la dimensione del backup (misurato: 4 KiB di spool sul client per un backup
da 4 GiB). Il compromesso è dichiarato: **il server vede il plaintext**, perché è
lui a cifrarlo. Usare `--remote-mode layers` quando il ricevente non deve
vederlo.

Le credenziali del registry restano sul client, che consegna al server token
bearer di breve durata attraverso TLS. Configurazione completa, pinning del
certificato, mTLS e QUIC sono in [`docs/remote.md`](docs/remote.md).

### `version`

```console
backimage version --json
```

---

## Cose da sapere prima del primo backup vero

### Nomi dei path archiviati

Ogni sorgente è archiviata sotto il proprio **basename**: `backimage backup
/home/alice` produce voci `alice/...`, non `home/alice/...`. Quella base è ciò
contro cui vengono confrontati i pattern di `--exclude`, `--include` e `find`, ed
è ciò che `--strip-components` conta.

Due conseguenze:

- Due sorgenti con lo stesso basename collidono e il backup si rifiuta di
  partire (`/opt/app/data` insieme a `/srv/app/data`). Rinominare, o dividere in
  due esecuzioni.
- **Non passare `/` come radice unica.** Il suo basename è `/`, quindi le voci
  diventano `//etc`, `//home`, e quell'immagine non si ripristina. Elencare
  invece i sottoalberi: `backimage backup /etc /var/lib /home`.

### Pattern glob

`*` e `?` restano dentro un singolo segmento di path; `**` ne attraversa un
numero qualsiasi, zero compreso. `dir/**` copre `dir` stessa e tutto ciò che
contiene, e il semplice `dir` fa lo stesso. Le stesse regole valgono per
`backup --exclude`, `restore --include/--exclude`, `ls` e `find`. Un pattern
malformato è un errore d'uso, non un silenzioso nulla di fatto.

### Passare i segreti

Preferire `--passphrase-file`, `--passphrase-stdin`, `--password-stdin` e
`--auth-token-file`. `--password` e `--token` funzionano, ma lasciano il segreto
nella history della shell e in `ps`.

### Codici di uscita

| Codice | Significato |
| --- | --- |
| 0 | successo |
| 1 | errore generico |
| 2 | errore d'uso |
| 3 | privilegi insufficienti |
| 4 | passphrase mancante o sbagliata |
| 5 | integrità fallita — i dati non corrispondono ai loro digest |
| 6 | errore di rete o del registry |
| 7 | interrotto |

### Ambiente

| Variabile | Effetto |
| --- | --- |
| `BACKIMAGE_PASSPHRASE` | passphrase per la CLI e per l'immagine auto-estraente |
| `BACKIMAGE_AUTH_FILE` | file credenziali da usare invece del default |
| `BACKIMAGE_<FLAG>` | default di un flag, es. `BACKIMAGE_BIND_ADDRESS`, `BACKIMAGE_JSON`; il flag esplicito prevale |
| `XDG_CONFIG_HOME` | base per `backimage/auth.json` |
| `XDG_CACHE_HOME` | cache dei layer e checkpoint di upload riprendibili |
| `TMPDIR` | spool, se non è dato `--temp-dir` |

### Limiti noti

- `docker run` tollera al massimo 118 layer di dati, quindi backup molto grandi
  producono layer molto grandi. È il prezzo di mantenere l'immagine eseguibile.
- L'estrazione dentro il container su Docker Desktop per macOS e Windows non
  preserva ownership e xattr: prendere il `tar` ed estrarlo sull'host.
- `xz` e `lz4` producono immagini che `docker run` non può eseguire
  (`--runnable=false`).
- La deduplicazione ha grana di layer, non di blocco: risparmia meno di restic o
  kopia a parità di modifiche.
- La cancellazione dei tag dipende dal registry: vedere
  [`docs/registries.md`](docs/registries.md).
- Una radice `/` singola non è supportata (vedi *Nomi dei path archiviati*).
- Perdere la passphrase significa perdere il backup. Non esiste recupero.

---

## Documentazione

Il manuale esteso, in italiano, è
[`docs/handbook.it.md`](docs/handbook.it.md): ricette lunghe, certificati TLS,
configurazioni multi-account, `compose.yml` per il server, procedure di fedeltà
massima.

Riferimento tecnico: [backup](docs/backup.md) · [restore](docs/restore.md) ·
[fedeltà](docs/FIDELITY.md) · [registries](docs/registries.md) ·
[dedup](docs/dedup.md) · [compressione](docs/compression.md) ·
[remoto](docs/remote.md) · [formato immagine](docs/image-format.md) ·
[sicurezza](docs/security.md) · [riferimento CLI](docs/cli.md) ·
[architettura](docs/ARCHITECTURE.md)

## Sviluppo

```console
make check       # lint, test, race test e gate di progetto
make build       # binario locale
make embed       # build a due stadi con gli auto-estrattori embedded
make e2e PHASE=05
```

Vedere [`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md) e
[`docs/BUILD.md`](docs/BUILD.md).

## Licenza

Vedere [`LICENSE`](LICENSE).
