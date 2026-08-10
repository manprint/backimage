# Ripristino dall'immagine auto-estraente

Ogni backup `backimage` eseguibile contiene `/backimage` e tutti i dati
necessari sotto `/backup`. Non serve installare `backimage` sull'host:

```sh
docker run --rm registry.example/team/backup:tag
docker run --rm -it registry.example/team/backup:tag list
docker run --rm -i registry.example/team/backup:tag tar > backup.tar
docker run --rm -v "$PWD/restore:/restore" registry.example/team/backup:tag \
  extract --out /restore

# Rimuove l'immagine locale solo dopo un'estrazione riuscita.
docker run --rm \
  -e BACKIMAGE_PASSPHRASE="$BACKUP_PASSPHRASE" \
  -e BACKIMAGE_IMAGE_REF="registry.example/team/backup:tag" \
  -v "$PWD/restore:/restore" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  registry.example/team/backup:tag \
  extract --out /restore --remove-local-image
```

## Comandi

- `info [--json]` legge solo il manifesto pubblico e non richiede segreti.
- `list [-l] [--include GLOB] [--exclude GLOB] [--json]` elenca l'indice.
- `tar [--cpus N] [--no-verify]` scrive esclusivamente il tar in chiaro su stdout.
- `extract --out DIR` ripristina direttamente; supporta `--include`,
  `--exclude`, `--strip-components N`, `--cpus N`, `--overwrite`,
  `--no-preserve-owner`, `--remove-local-image` e `--json`. Quest'ultimo
  richiede `BACKIMAGE_IMAGE_REF` e il mount di `/var/run/docker.sock` e rimuove
  l'immagine solo dopo il successo dell'estrazione.
- `verify [--continue] [--json]` controlla tutti i digest memorizzati. Senza
  credenziali, su un backup cifrato, esegue una verifica parziale esplicita;
  con la chiave controlla anche autenticazione, plaintext e indice.

`--root DIR` è disponibile su ogni comando per leggere un backup estratto
fuori dal container. `tar` rifiuta stdout se è un terminale: reindirizzare
sempre il flusso binario.

## Passphrase e identità age

In ordine, si può usare `--password PASSWORD`, `--passphrase-file FILE`,
`--passphrase-stdin`, la variabile `BACKIMAGE_PASSPHRASE`, oppure il prompt sul
terminale. Una chiave age privata si passa con `--identity FILE`.

Esempio Docker diretto:

```sh
docker run --rm -v "$PWD/restore:/restore" \
  registry.example/team/backup:tag \
  extract --out /restore --password mypassword
```

`--password` è comodo ma lascia il segreto nella history della shell e nella
lista dei processi; per uso operativo preferire `BACKIMAGE_PASSPHRASE` o
`--passphrase-file`.

La variabile d'ambiente è comoda ma può essere visibile nei metadati del
processo o nell'orchestratore. Per automazioni reali è preferibile montare un
secret e usare `--passphrase-file`. La passphrase non viene mai scritta su
stdout.

## Fedeltà del ripristino

| Metodo | Ownership | xattr/ACL | Device | Portabile |
|---|---:|---:|---:|---:|
| `tar` + `sudo tar xpf --xattrs --acls --numeric-owner` su Linux | sì | sì | sì | sì |
| `extract` in container su bind mount Linux | sì | sì, se supportati | sì, con privilegi | Linux |
| `extract` su Docker Desktop macOS/Windows | non garantita | non garantiti | no | sì, fedeltà ridotta |

Per il ripristino più fedele, esportare il tar e materializzarlo come root su
un filesystem Linux. L'immagine è `scratch`: non contiene shell, certificati
CA, `/tmp` o database utenti; il bootstrap non effettua rete né risoluzione
dei nomi utente.

## Backup danneggiato

Eseguire prima `verify --continue` per ottenere tutti gli errori. Un mismatch
normale termina con exit code 5. `tar --no-verify` salta soltanto il digest del
plaintext ed è un'ultima risorsa: autenticazione/decompressione possono ancora
fallire e il tar su stdout può essere parziale fino al chunk corrotto.
