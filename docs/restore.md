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
