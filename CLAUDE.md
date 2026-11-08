# CLAUDE.md — backimage

Guida completa: `AGENTS.md` (leggerla prima di modificare codice).

## Toolchain e dipendenze

- Go: `toolchain go1.27.3` in `go.mod` (ultima patch stabile verificata 2026-11-08).
- golangci-lint pinzato `v2.15.0` (Makefile `GOLANGCI_VERSION`), govulncheck in `~/go/bin`.
- Aggiornamento moduli: `go get -u all && go mod tidy`, poi `make check`.
  Moduli solo nel grafo (non compilati) possono restare indietro: non è un problema di build.

## Gate

- `make check` (fmt, vet, lint, build, test, race, deps-check, docs-check, proto-check, vuln).
- Test QUIC reali: `make e2e PHASE=09` (richiede docker).
- `make deps-check` richiede `make selfextract` prima (già incluso in `make check`).

## Regole di lavoro

- Non committare né pushare senza richiesta esplicita.
- Non toccare file estranei al task (es. `.serena/project.yml`, file untracked in root).
- Dopo i test Docker, rimuovere solo i container/immagini creati dal test (non quelli di altri progetti).
