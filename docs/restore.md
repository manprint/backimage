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
| hardlink non ricreabile | materializzato come copia indipendente |
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
`Warnings`, `XattrsSkipped`, `Skipped` ed `Errors`.

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
