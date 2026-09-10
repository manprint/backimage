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

Durante il backup vengono scritti su stderr log timestampati. Le fasi visibili
sono: scansione delle sorgenti, pianificazione dei chunk/layer, archiviazione,
compressione e cifratura, costruzione delle immagini OCI, controllo della
presenza di ogni blob, upload parallelo, pubblicazione dei manifest e risultato
finale. Il numero di upload paralleli è quello di `--jobs` (default 3). Ogni
blob viaggia in un'unica richiesta HTTP streamata: spezzarlo in chunk costa un
round trip ciascuno, che il registry chiude solo dopo aver scritto il chunk sul
proprio storage. `--upload-chunk-size 32MiB` forza di nuovo il chunking, e serve
solo verso registry che rifiutano richieste grandi (un 413 attiva comunque il
fallback da solo). Il
riepilogo di successo resta su stdout e non viene prefissato dal timestamp;
con `--json` stdout contiene solo JSON.

I layer sono appoggiati su disco e letti in streaming durante il push. **Tutti
i layer restano su disco fino alla fine del push**: la costruzione di un layer
libera lo spool ma conserva il file del blob OCI, e la pulizia avviene solo
all'uscita. Lo spazio temporaneo di picco è quindi:

```
dimensione del backup compresso  (+ due layer transitori)
```

Non `jobs × max-layer-size`, come questo documento affermava fino alla 0.3.2:
quella era una finestra che non esiste, e su una sorgente da 1 GiB
incomprimibile con `--jobs 1 --max-layer-size 64MiB` il preflight chiedeva
64 MiB mentre il picco reale era 1024 MiB. Ridurre `--max-layer-size` o
`--jobs` non abbassa il requisito.

Il preflight controlla il caso peggiore, cioè dati incomprimibili: un backup
che si comprime bene ne usa meno. Se fallisce:

- `--temp-dir` per puntare a un filesystem più capiente (attenzione: il default
  è `$TMPDIR`, che su molti sistemi è una tmpfs in RAM);
- `--remote-mode stream` verso un `listen-remote`, che non costruisce nessun
  layer in locale: il client usa **4 KiB** di disco indipendentemente dalla
  dimensione del backup (misurato su 1 GiB).

La memoria non cresce con la dimensione complessiva del backup: il test slow
impone un limite di 512 MiB.

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

### Quanto costa rileggerlo

Il tempo di restore è lineare nella dimensione del backup, non nel numero di
chunk: dalla 0.5.0 la lettura di un chunk si posiziona sul suo offset invece di
rileggere il layer dall'inizio, e un layer che la cache non può tenere viene
materializzato una volta per layer invece di una volta per chunk. Prima di quel
cambio un layer da 1 GiB diviso in 64 chunk poteva costare ~32 GiB di letture
su un backup locale e 64 decompressioni su un `--oci-layout`, che non tiene mai
una cache dei layer. Il dettaglio è in `docs/TROUGHPUT_IMPROVE.md` §11-12.

Nella stessa versione il restore decomprime **due volte** ogni chunk quando ne
verifica il digest: la prima passata verifica, la seconda scrive. È voluto —
serve a non consegnare byte che poi verranno rifiutati — e costa una passata di
decompressione in più, non memoria in più.

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
| `--upload-chunk-size` | 0 | chunk HTTP per blob; 0 = una richiesta per blob |
| `--platform` | amd64, arm64 | piattaforma, ripetibile |
| `--output` | `registry` | destinazione |
| `--output-path` | — | path per layout/tar |
| `--local-repo` | false | alias per output daemon |
| `--exclude` | — | glob escluso, ripetibile |
| `--one-file-system` | false | non attraversa mount point |
| `--numeric-owner` | false | non risolve nomi utente/gruppo |
| `--allow-degraded` | false | continua sulle feature non disponibili |
| `--verify-after-push` | quick | rilettura post-push: `quick` (digest di blob e manifest, nessun download), `full` (riscarica ogni layer e ricalcola i digest memorizzati, in streaming), `off` |
| `--no-metadata` | false | omette sorgenti e host dalle label |
| `--dry-run` | false | piano senza rete o scritture |
| `--resume` | true | usa checkpoint |
| `--runnable` | true | include il binario auto-estraente |
| `--temp-dir` | `$TMPDIR` | spool dei layer |
| `--json` | false | risultato JSON su stdout |

I codec `xz` e `lz4` richiedono `--runnable=false` perché i relativi media
type non sono portabili nei runtime OCI comuni.

## Verifica di ciò che è stato pubblicato

Il registry è obbligato dalla spec OCI a ricalcolare il digest di un blob
quando l'upload viene finalizzato (`PUT .../uploads/<id>?digest=sha256:…`),
quindi una corruzione in transito fa fallire il push. Restano però due casi che
nessuno confermava: un blob saltato perché il registry dichiarava di averlo già,
e un blob saltato perché lo diceva il checkpoint.

`--verify-after-push` chiude entrambi rileggendo ciò che è stato pubblicato:

| Livello | Cosa fa | Rete | Disco |
|---|---|---|---|
| `quick` (default) | una `HEAD` per blob (presenza, dimensione, `Docker-Content-Digest`), una `GET` per manifest con ricalcolo del digest sul body, e la risoluzione del tag | pochi KB | 0 |
| `full` | riscarica ogni data layer in streaming e ricalcola tre digest: quello compresso del layer (manifest OCI), quello del blob (metadati del backup) e quello memorizzato di ogni chunk (tabella dei chunk) | pari al backup | 0 |
| `off` | pubblica senza rileggere nulla | 0 | 0 |

Il livello `full` non scrive nulla su disco: i chunk di un layer sono intervalli
contigui dello stesso blob, quindi la verifica è una sola passata sequenziale
con in memoria un chunk per volta. Non serve la chiave del backup: risponde
alla domanda «il registry serve i byte che ho pubblicato?», mentre
`backimage verify` con la passphrase risponde a «quei byte decifrano nel
plaintext atteso?».

In più, indipendentemente dal livello: un blob che il registry dichiara già
presente ma con una dimensione diversa dalla nostra non viene più creduto,
viene reinviato.

Evidenze nel log:

```text
integrità: registrati 520 chunk con digest in chiaro e digest memorizzato, su 9 layer dati;
           i digest in chiaro sono sigillati nel blob privato (AES-256-GCM), non leggibili senza la chiave
push: verifica rapida superata: 12 blob confermati (presenza, dimensione, digest) e
      3 manifest riletti e ricalcolati; il tag risolve al digest pubblicato
push: verifica completa superata: 9 layer riletti, digest compresso coincidente;
      520/520 chunk con digest memorizzato coincidente; 8233.1 MiB riletti dal registry
```

## Ridondanza e copie multiple (TODO, prossima release)

La verifica dice se il backup pubblicato è integro; non lo rende
sopravvivibile. I modi di guasto che restano scoperti sono quelli che
riguardano il repository nel suo insieme:

| Guasto | Coperto oggi? |
|---|---|
| corruzione in transito | sì: il registry rifiuta il blob, il push fallisce |
| blob conservato male | sì: `--verify-after-push=full` e `verify` lo rilevano |
| chunk danneggiato al ripristino | in parte: `restore --continue` salva il resto e dichiara le perdite |
| blob cancellato da GC/retention/`repo prune` | **no** |
| repository, account o registry perduti | **no** |
| tag sovrascritto | **no** |

### Perché non un recovery record interno (stile RAR)

Aggiungere una percentuale di parità dentro l'immagine protegge solo il caso
più raro — il bit rot dello storage del registry, che gli object store già
mitigano con checksum ed erasure coding — e non protegge i due più probabili:
i blob di parità vivono nello stesso repository e muoiono insieme a esso. Il
costo, invece, è permanente: un nuovo tipo di layer, campi nuovi nel manifest,
un decoder Reed-Solomon dentro il self-extractor, l'interazione con `--dedup` e
con i confini dei chunk, e una versione di formato da mantenere per sempre.

### La direzione scelta

Una **seconda copia indipendente** su un altro registry o un'altra regione
copre ogni riga della tabella qui sopra, ed è la regola 3-2-1 applicata a un
repository OCI. La forma prevista è un flag `--also-repo REGISTRY/REPO` in
push, oppure un comando `repo copy SRC DST` che ricopia i blob **già cifrati**,
senza riesporre il plaintext e senza ricalcolare la pipeline.

Nel frattempo la stessa cosa si ottiene a mano, ripetendo il backup verso il
secondo repository oppure copiando l'immagine con uno strumento OCI generico
(`skopeo copy`, `crane copy`), e verificando la copia con
`backimage verify --continue`.
