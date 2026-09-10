BIN            := backimage
MODULE         := github.com/manprint/backimage
# Pinned so the local gate and CI run the same linter: the v1 series is EOL and
# .golangci.yml is on the v2 schema, which a v1 binary cannot parse.
GOLANGCI       := $(HOME)/go/bin/golangci-lint
GOLANGCI_VERSION := v2.1.6
GOVULNCHECK    := $(HOME)/go/bin/govulncheck
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT         ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE           ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS        := -s -w \
  -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
  -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
  -X $(MODULE)/internal/buildinfo.Date=$(DATE)
# Stamp of the embedded self-extract assets. DATE is deliberately omitted: with
# it the binary changes on every build, so the digest of the tool layer of every
# produced image would change too, at identical code. Without it two builds of
# the same commit are byte-identical.
LDFLAGS_EMBED  := -s -w \
  -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
  -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT)
export CGO_ENABLED := 0

PLATFORMS := linux/amd64 linux/arm64 linux/arm linux/riscv64 \
             darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: check fmt vet lint build build-all test race cover e2e bench-transport deps-check docs-check proto-check vuln clean selfextract embed

check: fmt vet lint build test race deps-check docs-check proto-check vuln   ## gate unico

fmt:            # G1 — fallisce se ci sono file non formattati
	@out="$$(gofmt -l . | grep -v '^vendor/' || true)"; \
	 if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:            # G2
	go vet ./...

lint:           # G3
	@have="$$($(GOLANGCI) version --short 2>/dev/null || echo missing)"; \
	 case "$$have" in $(GOLANGCI_VERSION)*) ;; *) \
	   echo "golangci-lint $(GOLANGCI_VERSION) expected, found $$have"; exit 1;; esac
	$(GOLANGCI) run

build: selfextract   # G4 (host)
	# selfextract is a prerequisite, not a convenience: internal/embedded is
	# compiled into this binary, so building without regenerating it embeds
	# whatever extractor happens to be on disk and the local tests then measure
	# code nobody is writing.
	go build -ldflags '$(LDFLAGS)' -o bin/$(BIN) ./cmd/backimage

build-all: selfextract   # G4 (tutte le piattaforme)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = windows ] && ext=".exe"; \
	  echo "building $$os/$$arch"; \
	  GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' \
	    -o dist/$(BIN)_$${os}_$${arch}$$ext ./cmd/backimage || exit 1; \
	done

selfextract:    # binari embeddabili: SOLO linux/amd64 e linux/arm64
	@for arch in amd64 arm64; do \
	  rm -f internal/embedded/backimage-selfextract-linux-$$arch; \
	  GOOS=linux GOARCH=$$arch go build -ldflags '$(LDFLAGS_EMBED)' \
	    -o internal/embedded/backimage-selfextract-linux-$$arch \
	    ./cmd/backimage-selfextract || exit 1; \
	done

embed: build   ## alias documentato: `build` rigenera già gli asset

test:           # G5
	go test ./...

race:           # G6
	# Package-level parallelism makes timing-sensitive transport tests flaky on
	# constrained CI runners; tests inside each package remain fully parallel.
	CGO_ENABLED=1 go test -race -p 1 ./...

cover:          # G7 — uso: make cover PKG=./pkg/archive/...
	go test -coverprofile=coverage.raw.out $(PKG)
	@{ head -n 1 coverage.raw.out; tail -n +2 coverage.raw.out | grep -v '\.pb\.go:'; } > coverage.out
	@rm -f coverage.raw.out
	@go tool cover -func=coverage.out | tail -1

e2e:            # G8 — uso: make e2e PHASE=04
	bash test/e2e/phase_$(PHASE).sh

bench-transport: # GS-09.4 — benchmark TCP/QUIC, richiede root per netem completo
	bash test/bench/transport/run.sh

deps-check:     # G9
	bash scripts/check-deps.sh

docs-check:     # G10
	bash scripts/check-docs.sh

proto-check:    # GS-08.9
	bash scripts/check-proto.sh

vuln:           # G11 — advisory raggiungibili dal nostro codice
	@test -x $(GOVULNCHECK) || { \
	   echo "govulncheck missing: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	$(GOVULNCHECK) ./...

clean:
	rm -rf bin dist coverage.out internal/embedded/backimage-selfextract-*
