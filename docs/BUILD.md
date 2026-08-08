# Build

## I due stadi

Il binario principale (`cmd/backimage`) incorpora i binari di
auto-estrazione (`cmd/backimage-selfextract`) per `linux/amd64` e
`linux/arm64`, perché l'immagine OCI prodotta li inserisce nei layer senza
rete.

```console
make embed     # = make selfextract && make build
```

`make selfextract` compila i due binari statici e li scrive in
`internal/embedded/backimage-selfextract-linux-<arch>`, sovrascrivendo i
**file placeholder** (17 byte, riga `PLACEHOLDER`) che sono committati nel
repo per far funzionare `go build ./...` da un clone pulito.

## Placeholder embedded

- I placeholder sono committati **di proposito**: senza di essi
  `go:embed` fallirebbe la build da sorgente pulita.
- Dopo una build locale `git diff` mostra i placeholder modificati:
  **non committare i binari veri**. Consigliato:
  `git update-index --skip-worktree internal/embedded/backimage-selfextract-*`.
- Un binario che contiene ancora `PLACEHOLDER` restituisce
  `ErrNotEmbedded` da `embedded.SelfExtract`; un binario di release che
  fallisce così è un difetto bloccante.

## Piattaforme

| Target | Piattaforme |
|---|---|
| `make build` | host corrente |
| `make build-all` | linux/amd64, linux/arm64, linux/arm, linux/riscv64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64 |
| `make selfextract` | linux/amd64, linux/arm64 (sola %64) |

`CGO_ENABLED=0` è forzato da `make check` (binari statici, obbligatorie
per la base image `scratch` dei container).

## Requisiti CI

- Go 1.26+, `golangci-lint` fissato a **v1.64.8** (non `latest`):
  cambia comportamento fra versioni.
- Docker e/o `qemu-user-static` solo per i job `e2e` e `cross-arch`.
- Job CI: lint, build, test (matrice 3 OS), race, test-root, e2e con
  `registry:2`, cross-arch con QEMU.