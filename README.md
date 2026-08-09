# backimage

Archivia, comprime, cifra e pubblica i tuoi file dentro un'immagine OCI
**eseguibile e auto-estraente**: chi fa `docker pull` può recuperare i dati
con un semplice `docker run`, senza installare nulla.

> **Stato: milestone v0.1 in sviluppo.** Backup cifrato, immagini
> auto-estraenti e restore host-side da registry/OCI-layout sono funzionanti.

## Installazione da sorgente

```console
make embed
sudo install -m 0755 bin/backimage /usr/local/bin/
```

Requisiti: Go 1.26+, `golangci-lint` per `make check`.

## Uso rapido

```console
# Credenziali registry (il secret arriva da stdin).
printf '%s\n' "$REGISTRY_PASSWORD" | backimage login ghcr.io \
  --username USER --password-stdin

# Backup cifrato e multiarch.
backimage backup /srv/data --repo ghcr.io/team/dumps --tag daily \
  --passphrase-file /run/secrets/backup

# Disaster recovery senza installare backimage.
docker run --rm ghcr.io/team/dumps:daily
docker run --rm -i -e BACKIMAGE_PASSPHRASE ghcr.io/team/dumps:daily tar \
  > daily.tar

# Ispezione e restore selettivo dal registry.
backimage inspect ghcr.io/team/dumps:daily --layers
backimage ls ghcr.io/team/dumps:daily --include '**/*.pdf' \
  --passphrase-file /run/secrets/backup
backimage restore ghcr.io/team/dumps:daily --extract -C ./restore \
  --include 'documents/**' --no-preserve-owner \
  --passphrase-file /run/secrets/backup
backimage verify ghcr.io/team/dumps:daily --quick
backimage doctor /srv/data
```

Il flag globale `--json` produce output strutturato su stdout; diagnostica e
hint restano su stderr. Vedi [backup](docs/backup.md),
[ripristino](docs/restore.md), [container auto-estraente](docs/selfextract.md),
[backup remoto](docs/remote.md), [deduplica incrementale](docs/dedup.md), [benchmark TCP/QUIC](docs/transport-benchmark.md),
[sicurezza](docs/security.md) e [riferimento CLI](docs/cli.md).

## Verifica della qualità

```console
make check
```

`make check` esegue fmt, vet, lint, build, test, race, deps-check e docs-check.
Un repository pulito deve uscirne con exit code 0.
