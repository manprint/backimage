# backimage

Archivia, comprime, cifra e pubblica i tuoi file dentro un'immagine OCI
**eseguibile e auto-estraente**: chi fa `docker pull` può recuperare i dati
con un semplice `docker run`, senza installare nulla.

> **Stato: in sviluppo.** La struttura del repository e il progetto sono
> completi; i comandi di backup/restore arrivano con le fasi di sviluppo
> (vedi `plan/`).

## Installazione da sorgente

```console
make embed
sudo install -m 0755 bin/backimage /usr/local/bin/
```

Requisiti: Go 1.26+, `golangci-lint` per `make check`.

## Verifica della qualità

```console
make check
```

`make check` esegue fmt, vet, lint, build, test, race, deps-check e docs-check.
Un repository pulito deve uscirne con exit code 0.