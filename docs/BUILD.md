# Build

## I due stadi

Il binario principale (`cmd/backimage`) incorpora i binari di
auto-estrazione (`cmd/backimage-selfextract`) per `linux/amd64` e
`linux/arm64`, perché l'immagine OCI prodotta li inserisce nei layer senza
rete.

```console
make build     # rigenera gli asset e poi compila la CLI
make embed     # alias storico di `make build`
```

`make selfextract` compila i due binari statici e li scrive in
`internal/embedded/backimage-selfextract-linux-<arch>`, sovrascrivendo i
**file placeholder** (17 byte, riga `PLACEHOLDER`) che sono committati nel
repo per far funzionare `go build ./...` da un clone pulito.

`build` e `build-all` **dipendono** da `selfextract`: gli asset non sono più
qualcosa che si ricorda di rigenerare. Senza quella dipendenza `make build`
incorpora l'estrattore che si trova sul disco, di qualunque età, e i test
locali finiscono per misurare codice diverso da quello che si sta scrivendo —
una correzione in `pkg/archive` può sembrare funzionante perché l'e2e usa un
estrattore che non la contiene, o rotta per lo stesso motivo.

Gli asset sono marchiati con `LDFLAGS_EMBED`, che inietta `Version` e `Commit`
ma **non** `Date`: con la data il binario cambierebbe a ogni build e con esso il
digest del layer tool di ogni immagine prodotta, a codice identico.
`internal/embedded/coeval_test.go` rilegge quel marchio dai byte dell'asset —
senza eseguirlo, perché l'asset arm64 non gira su un host amd64 — e fallisce se
non coincide con la revisione dell'albero. Da qui una conseguenza pratica: la
suite di test va eseguita **dopo** almeno un `make build`; su un clone appena
fatto, con i placeholder ancora al loro posto, il test lo dice e indica il
comando.

Il marchio è leggibile anche a mano:

```console
./internal/embedded/backimage-selfextract-linux-amd64 version
go version -m internal/embedded/backimage-selfextract-linux-amd64 | grep ldflags
```

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
| `make build` | host corrente (rigenera prima gli asset embedded) |
| `make build-all` | (rigenera prima gli asset embedded) linux/amd64, linux/arm64, linux/arm, linux/riscv64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64 |
| `make selfextract` | linux/amd64, linux/arm64 (sola %64) |

`CGO_ENABLED=0` è forzato da `make check` (binari statici, obbligatorie
per la base image `scratch` dei container).

## Requisiti CI

- Go 1.26+, `golangci-lint` fissato a **v2.1.6** (non `latest`): cambia
  comportamento fra versioni. La versione vive in un solo posto,
  `GOLANGCI_VERSION` nel `Makefile`; `make lint` confronta il binario trovato
  con quel valore e si ferma prima di eseguire il linter se non corrisponde.
  Il percorso del modulo della serie 2 contiene `/v2`:
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6`.
- Docker e/o `qemu-user-static` solo per i job `e2e` e `cross-arch`.
- Job CI: lint, build, test (matrice 3 OS), race, test-root, e2e con
  `registry:2`, cross-arch con QEMU.