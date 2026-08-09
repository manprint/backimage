// Package crypt encrypts every chunk of a backup.
//
// Invariant processing order: tar -> compression -> encryption. The stored
// blob envelope (magic BIMGCHK1, codec id, aead id, flags, nonce, payload)
// is specified in plan/overview.md §4.4 and implemented in envelope.go.
//
// Security properties and limits:
//
//   - Without the passphrase or the age private key the backup is
//     UNRECOVERABLE. There is no backdoor: no master key, no escrow, no
//     recovery endpoint. Losing the secret is losing the backup.
//   - AES-256-GCM is used with a fresh 12-byte random nonce per chunk in
//     NonceRandom mode (the default).
//   - Convergent mode (phase 10) derives the nonce from the plaintext
//     digest. This enables content-defined deduplication at the cost of a
//     known, documented confidentiality trade-off: two identical chunks are
//     visibly identical. It is NOT enabled by default.
//   - GCM limit: 2^32 chunks per DEK. At 4 MiB per chunk this is 16 EiB,
//     out of reach for a single backup.
//   - The envelope reveals metadata that confidentiality cannot hide: layer
//     names, blob sizes and blob count remain visible in the registry.
//
// This package must depend only on the standard library, filippo.io/age,
// golang.org/x/term and pkg/compress: it is imported by the self-extract
// binary (phase 06) which has an 8 MB budget and cannot import cobra or
// go-containerregistry.
package crypt
