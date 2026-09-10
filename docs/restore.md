# Ripristino dei backup

`backimage restore` legge soltanto il manifest OCI, il layer dei metadati e i
layer dati effettivamente necessari. I layer estratti sono conservati in una
cache LRU (2 GiB di default) sotto `$XDG_CACHE_HOME/backimage/layers`.

```sh
# Produce dumps_daily.tar nella directory corrente.
backimage restore ghcr.io/team/dumps:daily --passphrase-file /run/secrets/backup

# Tar su stdout, senza diagnostica nel flusso.
backimage restore ghcr.io/team/dumps:daily -o - \
  --passphrase-file /run/secrets/backup > daily.tar

# Estrazione completa o selettiva.
sudo backimage restore ghcr.io/team/dumps:daily --extract -C /restore \
  --passphrase-file /run/secrets/backup
backimage restore ghcr.io/team/dumps:daily --extract -C ./restore \
  --include 'home/alice/documents/**' --no-preserve-owner \
  --passphrase-file /run/secrets/backup
```

Durante l'avvio del restore i log su stderr mostrano anche le fasi che possono
richiedere tempo prima dell'accesso ai dati: apertura della sorgente,
caricamento di manifest e tabella dei chunk, apertura di `keys.pass.age`,
derivazione della chiave con scrypt e sblocco delle chiavi del backup. La
derivazione scrypt è volutamente CPU-intensive per proteggere la passphrase;
non è decompressione dei dati e viene eseguita una sola volta per restore.
Ogni riga contiene il timestamp iniziale, quindi un intervallo senza nuovi
byte indica comunque quale fase è in corso.

Una passphrase errata viene rilevata usando il solo layer dei metadati, prima
di scaricare un layer dati. `--no-verify` è una modalità di emergenza e salta
il digest plaintext; autenticazione e decompressione restano obbligatorie.

Da 0.2.4 `--no-verify` **non ha effetto su un backup cifrato**: i digest del
plaintext vivono nel blob privato sigillato e sono ciò che rifiuta un chunk
spostato tra due backup che condividono la chiave, quindi il controllo è sempre
eseguito. Su un backup in chiaro, dove ogni digest è pubblico, il flag continua a
valere come prima.

## Sorgenti disponibili

| Sorgente | Selezione | Requisiti | Uso tipico |
|---|---|---|---|
| Registry OCI (default) | `IMAGE` | rete e credenziali `backimage login`/Docker | server e CI |
| OCI layout | `--oci-layout DIR` | directory layout locale | air-gap e test |
| Docker daemon | `--local-repo` | socket Docker raggiungibile | immagini locali |
| Immagine auto-estraente | `docker run IMAGE` | solo runtime OCI | disaster recovery |

Il default `--platform linux/amd64` sceglie il manifest di bootstrap; i layer
dati sono identici fra le piattaforme. `--cache-size` limita davvero la cache:
i file meno recenti vengono eliminati prima che il limite venga superato.

## `--overwrite` sovrappone, non sostituisce (0.4.1)

`--overwrite` significa «scrivi sopra ciò che trovi», non «sostituisci
l'albero». Fino alla 0.4.0 una directory già esistente sulla destinazione
veniva **cancellata ricorsivamente** prima di essere ricreata, quindi
ripristinare un solo sottoalbero dentro una directory popolata eliminava in
silenzio i file che il backup non conteneva:

```sh
# 0.4.0: /srv/data/docs/note.txt spariva anche se il backup non lo conteneva.
backimage restore IMAGE -x -C /srv/data --include '**/docs/report.pdf' --overwrite
```

Dalla 0.4.1 la semantica è quella di `tar -x`:

| Sulla destinazione | Nell'archivio | Cosa succede |
| --- | --- | --- |
| directory | directory | nessuna cancellazione: i due alberi si sovrappongono e i metadati archiviati vengono applicati alla directory |
| file | file | il file viene troncato e riscritto |
| tipo diverso (file su directory, symlink su file, …) | qualsiasi | l'oggetto esistente viene rimosso e ricreato: non si può scrivere «sopra» un symlink o un device |

Senza `--overwrite` nulla cambia: un nome già presente resta un errore.

Chi contava sulla cancellazione — per esempio per ottenere una destinazione
identica al backup e non un'unione — deve svuotarla esplicitamente prima del
restore.

## Una credenziale dichiara cosa ci si aspetta di leggere (0.4.1)

Fornire `--passphrase-file`, `--passphrase-stdin`, `--password`, `--identity`
oppure `BACKIMAGE_PASSPHRASE` significa dire «questo backup è cifrato». Dalla
0.4.1, se il backup che si sta leggendo **non** lo è, il comando fallisce con
un errore di integrità (exit 5) invece di ignorare la credenziale e riuscire.

Serve a rendere visibile una sostituzione: nessun controllo interno a un backup
cifrato impedisce di scambiare l'**intera** immagine con un backup in chiaro
costruito da qualcun altro. Prima, il lettore vedeva `encryption.enabled:
false`, lasciava cadere la passphrase e ripristinava quei file annunciando
successo.

La regola vale su tutti i comandi di lettura del binario host — `restore`,
`verify`, `tar`, `ls`, `find` — e sull'autoestraente, che è un secondo ingresso
sugli stessi byte.

Chi legge deliberatamente backup misti in automazione usa `--allow-unencrypted`:

```sh
backimage restore IMAGE -x -C ./restore --passphrase-file ./pass --allow-unencrypted
docker run --rm -e BACKIMAGE_PASSPHRASE IMAGE extract --out /restore --allow-unencrypted
```

Senza credenziali il comportamento è quello di sempre: un backup in chiaro si
legge senza dire nulla a nessuno.

## `--expect-digest`: rifiutare l'immagine sbagliata prima della passphrase (0.4.1)

`--expect-digest sha256:…` ancora la lettura a un digest **ottenuto fuori
banda**. Se l'immagine che il riferimento risolve non ha quel digest, il
comando esce con codice 5 prima di leggere `--passphrase-file`, `--identity` o
`BACKIMAGE_PASSPHRASE`: la credenziale non arriva mai a un'immagine diversa da
quella richiesta.

```sh
backimage restore ghcr.io/me/dumps:daily --expect-digest sha256:9f2c… \
    -x -C ./restore --passphrase-file ./pass
```

Vale su `restore`, `verify`, `ls`, `find` e `inspect` del binario host. Non
esiste nell'autoestraente: un programma dentro l'immagine non può autenticare
l'immagine che lo contiene.

Il digest confrontato dipende dalla sorgente — per un registry è quello che il
registry associa al riferimento, per `--oci-layout` è quello dell'indice della
layout (il campo `digest` stampato da `backimage backup`), per `--local-repo`
è quello che l'immagine ha nel daemon, che non coincide con quello del
registry. **Un digest letto dalla stessa sorgente che fornisce l'immagine non
serve a niente**: dettagli e procedura in `docs/security.md`.

## Ispezione

```sh
backimage inspect IMAGE --layers
backimage inspect IMAGE --files --passphrase-file secret
backimage ls IMAGE -l --include '**/*.pdf'
backimage find IMAGE 'home/**/invoice-*'
backimage verify IMAGE --quick
backimage verify IMAGE --continue --passphrase-file secret
backimage doctor /path/to/source
```

`inspect` e `verify --quick` non scaricano layer dati. `ls` usa lo stesso
formato del comando `list` dentro l'immagine. Tutti i comandi supportano il
flag globale `--json`; gli errori restano su stderr e conservano l'exit code.

## Cosa viene preservato

| Percorso | Owner | mode | xattr/ACL | hardlink | device |
|---|---:|---:|---:|---:|---:|
| Tar + `sudo tar xpf --xattrs --acls --numeric-owner` | sì | sì | sì | sì | sì |
| `restore --extract` come root su Linux | sì | sì | sì | sì | sì |
| `--no-preserve-owner` non-root | no | sì | se consentiti | sì | no |
| bind mount Docker Desktop | non garantito | parziale | non garantiti | parziale | no |

### Politica di degradazione (default)

L'estrazione non si interrompe mai per un metadato che la destinazione non può
applicare. Owner/gruppo, permessi, timestamp, ACL, attributi estesi e hardlink
sono **best effort**: quello che il kernel rifiuta viene contato per classe,
segnalato una volta sola e riportato nel riepilogo finale
(`degradazioni: owner=… xattr.trusted=…`). Il contenuto dei file viene sempre
scritto e verificato per digest.

Casi tipici in un dump di host reale:

| Situazione | Effetto |
|---|---|
| `trusted.*` (overlayfs) senza `CAP_SYS_ADMIN` | attributi ignorati |
| file di altri utenti, restore non-root | owner = utente corrente |
| `security.*` su destinazione senza SELinux | attributi ignorati |
| destinazione senza xattr (vfat, NFS, alcuni bind mount) | attributi ignorati |
| hardlink non ricreabile (device diverso, filesystem senza hardlink) | copia indipendente del file già ripristinato |
| hardlink il cui primo nome non fa parte di questo restore | entry saltata e riportata, mai ricostruita leggendo dal disco |
| device node senza `CAP_MKNOD` | oggetto non creato, contato in `Skipped` |

Si fermano invece sempre, perché non sono degradazioni: destinazione piena o in
sola lettura (`ENOSPC`, `EDQUOT`, `EROFS`, `EIO`), archivio troncato,
destinazione già popolata senza `--overwrite`, entry di tipo non supportato.

`--strict` ripristina il comportamento intransigente: la prima operazione
rifiutata ferma l'estrazione e l'errore riporta il rimedio esatto.
`--no-preserve-xattrs` non tenta nemmeno gli attributi estesi;
`--no-preserve-owner` non tenta owner e gruppo.

## Evidenze prodotte dal restore

Ogni estrazione lascia nel log tre righe verificabili:

```text
restore: integrità: 520/520 chunk letti e verificati (dimensione e digest plaintext
         coincidono con quelli registrati nel backup)
restore: esito 1:1 sulle entry ricevute: 13 oggetti ripristinati (4 file, 6 directory,
         1 symlink, 1 hardlink, 0 device, 1 fifo); contenuti, permessi, owner, timestamp
         e attributi estesi applicati integralmente; nessuna differenza
```

Se qualcosa non è stato applicato, la seconda riga diventa un elenco di
differenze con conteggio e un esempio reale per classe:

```text
restore: esito NON 1:1 sulle entry ricevute: 4821 oggetti ripristinati, 3 entry non create,
         15855 differenze di metadati per classe:
restore:   differenza owner: 812 (es. lchown "/restore/db/data.mdb": operation not permitted)
restore:   differenza xattr.trusted: 15043 (es. setxattr "/restore/overlay2/l" trusted.overlay.opaque: operation not permitted)
restore:   3 entry NON estratte: elenco completo in Stats.Errors (--json)
```

Le classi sono `owner`, `mode`, `times`, `xattr.<namespace>`, `hardlink` e
`object`. Con `--json` gli stessi dati sono in `Degraded`, `DegradedExamples`,
`Warnings`, `XattrsSkipped`, `Skipped` ed `Errors`; `restore --extract --json`
riporta inoltre `skipped` e `skipped_reasons`, cioè le entry che l'estrattore
non ha potuto scrivere e il perché. Prima finivano solo in una riga di
attenzione su stderr, e un'automazione non poteva accorgersi che il restore era
incompleto.

## Un hardlink punta solo a un file di questo restore

Il primo nome di un hardlink deve essere **un file regolare che questa stessa
corsa ha già scritto**, risolto dentro la destinazione.

Il nome contenuto nell'header non passava dai controlli applicati al nome
dell'entry: veniva unito alla destinazione e collegato così com'era. Con
`Linkname="../fuori"` il restore otteneva un secondo nome per un file esterno,
e la fase dei metadati ne riscriveva owner, permessi e timestamp attraverso
l'inode condiviso, senza bisogno di privilegi.

**Cambio di fedeltà**: un hardlink il cui primo nome non fa parte di questo
restore viene ora **saltato e riportato**, mentre prima veniva materializzato
come copia leggendo qualunque cosa si trovasse a quel percorso sul disco. Un
restore selettivo lo incontra di rado: chiedere un hardlink chiede anche il
nome a cui punta, quindi il gruppo torna intero.

## Recupero parziale: `--continue`

Senza il flag, un chunk danneggiato ferma tutto: lo stream è sequenziale,
quindi un errore al chunk 393 di 520 perde anche i 127 chunk sani successivi.

Con `--continue` il restore lavora sull'indice dei file invece che sullo
stream: ricostruisce ogni entry i cui byte stanno in chunk che superano la
verifica, salta le altre e le elenca.

```text
restore: ATTENZIONE: recupero parziale: 2 entry ricostruite, 5 NON recuperabili
         perché ricadono nei chunk danneggiati [0]
restore:   causa: blob authentication failed: chunk 0 plaintext digest mismatch
restore:   percorsi perduti: src5, src5/file6.txt, src5/file5.txt, src5/file4.txt, src5/file3.txt
```

L'exit code resta quello di integrità (5) anche quando il recupero ha salvato
qualcosa: i dati mancanti sono un fallimento, per quanto parziale. Una entry è
scritta solo se completa — un record tar troncato romperebbe tutte le entry
successive.

### `--continue` rispetta i filtri (0.4.1)

Fino alla 0.4.0 `--continue` **annullava** `--include` e `--exclude`: il flag
sostituiva lo stream selettivo con quello tollerante, che non conosceva alcuna
selezione, e l'estrattore a valle veniva contemporaneamente informato che lo
stream era «già filtrato». Il risultato era che

```sh
backimage restore IMAGE -x -C ./dest --continue --include '**/*.pdf' --overwrite
```

estraeva **l'intero backup** sopra la destinazione.

Dalla 0.4.1 tolleranza ai chunk danneggiati e selezione sono proprietà
indipendenti e si combinano: `--continue` con dei filtri emette solo le entry
selezionate, e di quelle salta e rendiconta le non recuperabili. Vale sia verso
il filesystem sia verso il tar, e `--strip-components` continua ad applicarsi
come senza `--continue`.

Il recupero parziale mantiene un intervallo **per entry** anche con una
selezione attiva: unire gli intervalli adiacenti, come fa il restore selettivo
non tollerante, farebbe perdere i vicini di un chunk rotto.
