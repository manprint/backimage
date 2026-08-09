# Dipendenze

Elenco **chiuso**: aggiungere un modulo fuori da questa tabella richiede
l'approvazione dell'architetto e l'aggiornamento di questo file.
Verificato da `make deps-check` (G9).

- github.com/spf13/cobra — CLI principale — fase 00
- github.com/google/go-containerregistry — OCI, registry, daemon — fase 04
- github.com/klauspost/compress — zstd, gzip — fase 02
- github.com/ulikunitz/xz — xz — fase 02
- github.com/pierrec/lz4/v4 — lz4 — fase 02
- filippo.io/age — incapsulamento DEK — fase 03
- golang.org/x/sys — xattr, stat, syscall — fase 01
- golang.org/x/term — prompt passphrase — fase 03
- github.com/Microsoft/go-winio — metadati Windows — fase 01
- github.com/quic-go/quic-go — trasporto QUIC — fase 09
- google.golang.org/protobuf — messaggi di controllo — fase 08
- github.com/restic/chunker — CDC (dedup) — fase 10
- github.com/moby/moby/client — rimozione esplicita delle immagini Docker locali — fase 07
- github.com/stretchr/testify — asserzioni nei test — fase 00
- github.com/dustin/go-humanize — formattazione dimensioni — fase 00

## Vincolo di dimensione del self-extract

Il binario embedded (`cmd/backimage-selfextract`) può importare **solo**:
`pkg/archive`, `pkg/compress`, `pkg/crypt`, `pkg/index`, stdlib,
`filippo.io/age`, `golang.org/x/term`, `golang.org/x/sys`, `pkg/docker` e le
librerie di compressione. Vietati: cobra, go-containerregistry, quic-go,
protobuf.
Budget: ≤ 8 MB non compresso. Verificato da `make deps-check`.
