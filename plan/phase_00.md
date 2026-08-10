# Fase 00 — Fondamenta, build a due stadi, harness CI

**Obiettivo**: avere uno scheletro compilabile su 8 piattaforme, un comando `make check` che è l'unico giudice della qualità, e la catena di build a due stadi già in piedi (anche se con un binario placeholder).

**Perché per prima**: tutte le fasi successive si autovalidano con `make check`. Se questo passo è debole, l'agente implementatore non ha modo di accorgersi dei propri errori.

**Prerequisiti**: Go 1.26+, Docker, `golangci-lint`, `qemu-user-static` (solo CI).

---

## 00.1 Scheletro repo, go.mod, albero directory

**Agente: Haiku**

### File da creare

```
go.mod
.gitignore
.editorconfig
LICENSE                          # Apache-2.0
cmd/backimage/main.go
cmd/backimage-selfextract/main.go
internal/cli/.keep
internal/buildinfo/.keep
pkg/archive/.keep  pkg/compress/.keep  pkg/chunk/.keep  pkg/crypt/.keep
pkg/index/.keep    pkg/ociimg/.keep    pkg/registry/.keep
pkg/transport/.keep pkg/protocol/.keep pkg/server/.keep
pkg/backup/.keep   pkg/restore/.keep
test/fixtures/.keep test/e2e/.keep
docs/.keep
```

### Contenuto vincolato

`go.mod`:
```
module github.com/manprint/backimage

go 1.26
```

`.gitignore` deve contenere almeno: `/dist/`, `/bin/`, `*.test`, `coverage.out`, `plan/BLOCKED.md`, `.backimage-cache/`.

`cmd/backimage/main.go` e `cmd/backimage-selfextract/main.go`: per ora solo `package main` con `func main() { fmt.Println("placeholder") }`. Verranno riscritti in 00.5 e 06.1.

### Definition of Done
- [ ] `go build ./...` esce 0
- [ ] `git status` non mostra file non tracciati oltre a quelli previsti

---

## 00.2 Makefile e script di verifica

**Agente: Sonnet**

### File da creare
- `Makefile`
- `scripts/check-deps.sh`
- `scripts/check-docs.sh`

### Target obbligatori del Makefile

```make
BIN            := backimage
MODULE         := github.com/manprint/backimage
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT         ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE           ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS        := -s -w \
  -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
  -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
  -X $(MODULE)/internal/buildinfo.Date=$(DATE)
export CGO_ENABLED := 0

PLATFORMS := linux/amd64 linux/arm64 linux/arm linux/riscv64 \
             darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: check fmt vet lint build build-all test race cover e2e deps-check docs-check clean selfextract embed

check: fmt vet lint build test race deps-check docs-check   ## gate unico

fmt:            # G1 — fallisce se ci sono file non formattati
	@out="$$(gofmt -l . | grep -v '^vendor/' || true)"; \
	 if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:            # G2
	go vet ./...

lint:           # G3
	golangci-lint run

build:          # G4 (host)
	go build -ldflags '$(LDFLAGS)' -o bin/$(BIN) ./cmd/backimage

build-all:      # G4 (tutte le piattaforme)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = windows ] && ext=".exe"; \
	  echo "building $$os/$$arch"; \
	  GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' \
	    -o dist/$(BIN)_$${os}_$${arch}$$ext ./cmd/backimage || exit 1; \
	done

selfextract:    # binari embeddabili: SOLO linux/amd64 e linux/arm64
	@for arch in amd64 arm64; do \
	  GOOS=linux GOARCH=$$arch go build -ldflags '-s -w' \
	    -o internal/embedded/backimage-selfextract-linux-$$arch \
	    ./cmd/backimage-selfextract || exit 1; \
	done

embed: selfextract build   ## build a due stadi completa

test:           # G5
	go test ./...

race:           # G6
	go test -race ./...

cover:          # G7 — uso: make cover PKG=./pkg/archive/...
	go test -coverprofile=coverage.out $(PKG)
	@go tool cover -func=coverage.out | tail -1

e2e:            # G8 — uso: make e2e PHASE=04
	bash test/e2e/phase_$(PHASE).sh

deps-check:     # G9
	bash scripts/check-deps.sh

docs-check:     # G10
	bash scripts/check-docs.sh

clean:
	rm -rf bin dist coverage.out internal/embedded/backimage-selfextract-*
```

### `scripts/check-deps.sh`

Deve: estrarre i moduli diretti da `go.mod` (`go list -m -f '{{if not .Indirect}}{{.Path}}{{end}}' all` limitato ai `require` diretti), confrontarli con l'elenco in `docs/DEPENDENCIES.md` (una riga per modulo, formato `- <module-path> — <motivo> — fase <NN>`), uscire 1 se c'è un modulo non documentato **o** un modulo documentato ma non usato.

Deve inoltre verificare il **vincolo di importazione del self-extract**:
```bash
go list -deps ./cmd/backimage-selfextract | grep -E 'cobra|go-containerregistry|quic-go|protobuf' && exit 1
```
(la presenza di uno di questi è un errore).

### `scripts/check-docs.sh`

Per la fase corrente (variabile `PHASE`, default: tutte le fasi già spuntate in `resume.md`):
1. verifica l'esistenza dei file elencati come deliverable doc;
2. estrae ogni blocco ` ```console ` di `README.md` che inizia con `backimage ` e verifica che il sottocomando esista in `bin/backimage --help` (semplice grep del nome del sottocomando);
3. esce 1 al primo scostamento.

Finché il binario non ha sottocomandi (fase 00), il punto 2 è un no-op se `bin/backimage` non esiste.

### Definition of Done
- [ ] `make check` esce 0 su repo pulito
- [ ] `make build-all` produce 8 binari in `dist/`
- [ ] `make selfextract` produce 2 binari in `internal/embedded/`
- [ ] `make check` **fallisce** se si introduce di proposito un file mal formattato (verifica manuale, poi si annulla)

---

## 00.3 Configurazione lint

**Agente: Haiku**

### File: `.golangci.yml`

Linter da abilitare (elenco chiuso): `errcheck`, `govet`, `staticcheck`, `unused`, `ineffassign`, `gosimple`, `misspell`, `revive`, `gosec`, `bodyclose`, `errorlint`, `nilerr`, `contextcheck`, `noctx`, `copyloopvar`, `durationcheck`, `makezero`, `prealloc`.

Regole aggiuntive:
- `errcheck`: `check-blank: true` (vieta `_ = f()`);
- `revive`: attiva `exported` (ogni identificatore esportato ha un commento doc);
- esclusioni: nei file `*_test.go` disabilita `gosec` e `errcheck`;
- `gosec`: esclude G304 (path da input utente è il mestiere di questo programma) ma **non** G401/G501 (hash deboli): quelli devono restare errori.

### Definition of Done
- [ ] `golangci-lint run` esce 0
- [ ] rimuovere un commento doc da un simbolo esportato fa fallire il lint (verifica manuale)

---

## 00.4 CI GitHub Actions

**Agente: Sonnet**

### File: `.github/workflows/ci.yml`

Job obbligatori:

| Job | Runner | Cosa esegue |
|---|---|---|
| `lint` | ubuntu-latest | `make fmt vet lint` |
| `build` | ubuntu-latest | `make build-all` + upload artifact `dist/` |
| `test` | matrice `ubuntu-latest`, `macos-latest`, `windows-latest` | `make test` |
| `race` | ubuntu-latest | `make race` |
| `test-root` | ubuntu-latest | `sudo -E env "PATH=$PATH" go test -tags root ./...` |
| `e2e` | ubuntu-latest, servizio `registry:2` sulla porta 5000 | `make e2e PHASE=<max fase completata>` |
| `cross-arch` | ubuntu-latest con `docker/setup-qemu-action` | e2e del self-extract su `linux/arm64` |

Vincoli:
- cache dei moduli Go attiva (`actions/setup-go` con `cache: true`);
- `CGO_ENABLED=0` nell'ambiente globale del workflow;
- ogni job fallisce la pipeline (nessun `continue-on-error`);
- timeout per job: 30 minuti.

### Definition of Done
- [ ] il workflow è sintatticamente valido (`actionlint` o `yq` di validazione)
- [ ] i job `lint`, `build`, `test`, `race` passano
- [ ] i job `test-root`, `e2e`, `cross-arch` esistono ed escono 0 anche se al momento non hanno test da eseguire (skip esplicito, non fallimento)

---

## 00.5 internal/buildinfo e comando root

**Agente: Sonnet**

### File: `internal/buildinfo/buildinfo.go`

```go
// Package buildinfo carries build-time metadata injected via -ldflags.
package buildinfo

// Populated at build time via -ldflags -X.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a single-line human readable build identifier.
func String() string

// UserAgent returns the HTTP User-Agent used for registry requests.
func UserAgent() string   // "backimage/<Version> (<GOOS>/<GOARCH>)"
```

### File: `cmd/backimage/main.go`

```go
func main() {
	if err := cli.Execute(context.Background(), os.Args[1:]); err != nil {
		os.Exit(cli.ExitCodeFor(err))
	}
}
```

Nessuna logica in `main` oltre a questa.

### File: `internal/cli/root.go`

```go
// Execute builds the root command and runs it with the given arguments.
func Execute(ctx context.Context, args []string) error

// NewRootCommand assembles the whole command tree. Every subcommand
// registers itself here; there is no init()-based registration.
func NewRootCommand() *cobra.Command
```

Flag globali persistenti (definiti una sola volta, qui):

| Flag | Tipo | Default | Significato |
|---|---|---|---|
| `--json` | bool | false | output strutturato su stdout |
| `--quiet, -q` | bool | false | silenzia il progresso |
| `--verbose, -v` | count | 0 | livello di log (1=debug, 2=trace) |
| `--no-color` | bool | auto | disattiva ANSI (auto se stdout non è TTY) |
| `--config` | string | `$XDG_CONFIG_HOME/backimage/config.yaml` | file di configurazione |

### File: `internal/cli/version.go`

`backimage version` stampa `Version`, `Commit`, `Date`, `runtime.Version()`, `GOOS/GOARCH`. Con `--json` stampa lo stesso in JSON.

### Test richiesti (`internal/cli/root_test.go`)
- `backimage version` esce 0 e contiene la stringa di versione;
- `backimage version --json` produce JSON valido con le 6 chiavi attese;
- un sottocomando inesistente esce con codice **2**;
- `--verbose --verbose` porta il livello a 2.

### Definition of Done
- [ ] `bin/backimage version` funziona
- [ ] copertura `internal/cli` ≥ 80 %
- [ ] `make check` verde

---

## 00.6 internal/cli: output, errori, codici di uscita, logging

**Agente: Sonnet**

### File: `internal/cli/errors.go`

```go
// Kind classifies an error for exit-code mapping and user messaging.
type Kind int

const (
	KindGeneric      Kind = 1
	KindUsage        Kind = 2
	KindPermission   Kind = 3
	KindPassphrase   Kind = 4
	KindIntegrity    Kind = 5
	KindNetwork      Kind = 6
	KindInterrupted  Kind = 7
)

// Error is a user-facing error carrying a Kind and an optional remediation hint.
type Error struct {
	Kind Kind
	Msg  string
	Hint string // actionable instruction shown to the user, may be empty
	Err  error
}

func (e *Error) Error() string
func (e *Error) Unwrap() error

// New builds a *Error.
func New(kind Kind, hint string, format string, args ...any) *Error

// ExitCodeFor maps any error to a process exit code.
func ExitCodeFor(err error) int
```

**Regola di presentazione**: quando `Hint` non è vuoto, l'output su stderr è
```
error: <Msg>
hint:  <Hint>
```
Questa è la leva che rende la decisione D09 utilizzabile: mai fallire senza dire cosa fare.

### File: `internal/cli/output.go`

```go
// Printer renders results either as human text or as JSON, never both.
type Printer interface {
	// Result prints the final payload of a command.
	Result(v any) error
	// Infof writes progress/diagnostics to stderr; suppressed by --quiet.
	Infof(format string, args ...any)
	// Warnf writes a warning to stderr; never suppressed.
	Warnf(format string, args ...any)
}

// NewPrinter returns a JSON or text printer according to opts.
func NewPrinter(out io.Writer, errOut io.Writer, opts Options) Printer
```

**Invariante critica**: `Infof`/`Warnf` scrivono **sempre** su stderr. Nessun byte di diagnostica può finire su stdout, altrimenti `docker run … tar > f.tar` produce archivi corrotti. Va scritto un test che lo verifica.

### File: `internal/cli/logging.go`

`log/slog` con handler testuale su stderr; livello da `--verbose`. Nessun logger globale: il logger sta nel `context.Context` (`cli.WithLogger`, `cli.LoggerFrom`).

### Test richiesti
- tabella `Kind` → exit code (7 casi);
- errore con `Hint` produce due righe su stderr e zero byte su stdout;
- `Printer` JSON produce un solo oggetto JSON su stdout;
- test che `Infof` non scrive mai su stdout (usa un `bytes.Buffer` distinto).

### Definition of Done
- [ ] copertura `internal/cli` ≥ 85 %
- [ ] `make check` verde

---

## 00.7 Build a due stadi con go:embed

**Agente: Sonnet**

Il binario principale deve incorporare i binari self-extract per `linux/amd64` e `linux/arm64`, così da poterli inserire nell'immagine senza rete.

### File: `internal/embedded/embed.go`

```go
// Package embedded exposes the self-extract binaries embedded at build time.
package embedded

//go:embed backimage-selfextract-linux-amd64 backimage-selfextract-linux-arm64
var fs embed.FS

// ErrNotEmbedded is returned when the binary was built without the self-extract payload.
var ErrNotEmbedded = errors.New("self-extract binary not embedded in this build")

// SelfExtract returns the static self-extract binary for the given GOARCH
// ("amd64" or "arm64"). The returned slice must not be modified.
func SelfExtract(goarch string) ([]byte, error)

// Architectures lists the GOARCH values available in this build.
func Architectures() []string
```

### Problema dell'uovo e della gallina, e sua soluzione

`go:embed` fallisce se i file non esistono. Soluzione prescritta:

1. `internal/embedded/backimage-selfextract-linux-amd64` e `…-arm64` sono **file placeholder** committati nel repo, contenenti la sola riga `PLACEHOLDER` (17 byte). Sono in `.gitignore`? **No**: vanno committati, altrimenti la build da sorgente pulito fallisce.
2. `make selfextract` li **sovrascrive** con i binari veri.
3. `SelfExtract` rifiuta un payload che inizia con `PLACEHOLDER` restituendo `ErrNotEmbedded`.
4. `make embed` esegue `selfextract` e poi `build`, ed è il target usato da CI e da GoReleaser.
5. `git diff` mostrerà i placeholder modificati dopo una build locale: `scripts/check-deps.sh` **non** deve lamentarsene; aggiungere una nota in `docs/BUILD.md` che spiega di non committare i binari veri (hook consigliato o `git update-index --skip-worktree`).

### Test richiesti
- `SelfExtract("amd64")` su build placeholder ritorna `ErrNotEmbedded`;
- `SelfExtract("mips")` ritorna un errore che nomina l'architettura;
- test gated `//go:build embedded` che, quando i binari veri sono presenti, verifica che siano ELF (magic `\x7fELF`) e di dimensione > 1 MB.

### Definition of Done
- [ ] `make embed` produce un `bin/backimage` che, con `SelfExtract`, restituisce ELF validi
- [ ] `go build ./...` da clone pulito (senza `make selfextract`) **funziona comunque**
- [ ] `make check` verde

---

## 00.8 Scheletro documentazione

**Agente: Haiku**

### File da creare

| File | Contenuto minimo |
|---|---|
| `README.md` | titolo, una frase di descrizione, sezione "Stato: in sviluppo", installazione da sorgente, `make check` |
| `docs/DEPENDENCIES.md` | tabella dei moduli consentiti (copiare dalla §6 di `overview.md`), formato `- <module> — <motivo> — fase <NN>` |
| `docs/BUILD.md` | build a due stadi, piattaforme, nota sui placeholder embedded, requisiti CI |
| `docs/ARCHITECTURE.md` | copia delle §3 e §4 di `overview.md` (layout immagine e formati dati), da mantenere allineata fase per fase |
| `docs/CONTRIBUTING.md` | le dieci regole ferree (§8.1 di `overview.md`), convenzioni commit, come far girare i gate |
| `CHANGELOG.md` | formato Keep a Changelog, sezione `## [Unreleased]` |

### Definition of Done
- [ ] `make docs-check` esce 0
- [ ] `docs/DEPENDENCIES.md` elenca esattamente i moduli presenti in `go.mod`

---

## Gate di fase 00

Da eseguire in ordine; tutti devono passare.

| Gate | Comando | Criterio |
|---|---|---|
| G1–G3 | `make fmt vet lint` | exit 0 |
| G4 | `make build-all` | 8 binari in `dist/` |
| G5 | `make test` | exit 0 |
| G6 | `make race` | exit 0 |
| G7 | `make cover PKG=./internal/...` | ≥ 80 % |
| G9 | `make deps-check` | exit 0 |
| G10 | `make docs-check` | exit 0 |
| **GS-00.1** | `./dist/backimage_linux_amd64 version --json \| jq -e .version` | exit 0 |
| **GS-00.2** | `make embed && go run ./cmd/backimage version` | exit 0 |
| **GS-00.3** | `go list -deps ./cmd/backimage-selfextract \| grep -c cobra` | risultato `0` |
| G11 | revisione Opus | approvazione in `resume.md` |

**Deliverable documentali della fase**: `README.md`, `docs/DEPENDENCIES.md`, `docs/BUILD.md`, `docs/ARCHITECTURE.md`, `docs/CONTRIBUTING.md`, `CHANGELOG.md`.

**Rischio noto**: `golangci-lint` cambia comportamento fra versioni. Fissare la versione nel workflow CI e in `docs/BUILD.md` (es. `v1.64.x`); non usare `latest`.
