BIN            := backimage
MODULE         := github.com/fpierri/backimage
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

.PHONY: check fmt vet lint build build-all test race cover e2e bench-transport deps-check docs-check proto-check clean selfextract embed

check: fmt vet lint build test race deps-check docs-check proto-check   ## gate unico

fmt:            # G1 — fallisce se ci sono file non formattati
	@out="$$(gofmt -l . | grep -v '^vendor/' || true)"; \
	 if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:            # G2
	go vet ./...

lint:           # G3
	$(HOME)/go/bin/golangci-lint run

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
	  rm -f internal/embedded/backimage-selfextract-linux-$$arch; \
	  GOOS=linux GOARCH=$$arch go build -ldflags '-s -w' \
	    -o internal/embedded/backimage-selfextract-linux-$$arch \
	    ./cmd/backimage-selfextract || exit 1; \
	done

embed: selfextract build   ## build a due stadi completa

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

clean:
	rm -rf bin dist coverage.out internal/embedded/backimage-selfextract-*
