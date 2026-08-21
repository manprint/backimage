package restore

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/index"
)

// ErrStoredVerifyUnsupported is returned by VerifyStoredSource for a source
// that cannot stream its data layers.
var ErrStoredVerifyUnsupported = errors.New("questa sorgente non supporta la verifica in streaming")

// ErrStoredMismatch reports that at least one digest did not match.
var ErrStoredMismatch = errors.New("integrità: i byte pubblicati non corrispondono ai digest del backup")

// StoredReport is the outcome of a streaming verification.
type StoredReport struct {
	Layers int      `json:"layers"`
	Chunks int      `json:"chunks"`
	Bytes  int64    `json:"bytes"`
	OK     bool     `json:"ok"`
	Errors []string `json:"errors,omitempty"`
}

// StoredVerifier is implemented by sources that can prove that the bytes they
// serve are the bytes the manifest and the chunk table describe, without
// materialising anything on disk and without the backup key.
type StoredVerifier interface {
	VerifyStored(ctx context.Context, keepGoing bool, progress func(string)) (StoredReport, error)
}

// VerifyStoredSource runs the streaming verification when src supports it.
func VerifyStoredSource(ctx context.Context, src Source, keepGoing bool, progress func(string)) (StoredReport, error) {
	v, ok := src.(StoredVerifier)
	if !ok {
		return StoredReport{}, ErrStoredVerifyUnsupported
	}
	return v.VerifyStored(ctx, keepGoing, progress)
}

// VerifyStored re-reads every data layer and recomputes three independent
// digests: the compressed digest of the layer, which must match the one the
// OCI manifest references; the digest of the blob inside it, which must match
// the one the backup metadata recorded when the layer was rolled; and the
// stored digest of every chunk, which must match the chunk table.
//
// The chunks of one layer are contiguous ranges of a single blob (see Blob),
// so the whole check is one sequential pass per layer: nothing is written to
// disk and the working set is one chunk. The backup key is never needed. This
// answers "does the registry serve the bytes we published?", which is a
// different question from "do those bytes decrypt to the expected plaintext?"
// — the latter is what Verify with a passphrase answers.
func (s *imageSource) VerifyStored(ctx context.Context, keepGoing bool, progress func(string)) (StoredReport, error) {
	report := StoredReport{OK: true}
	manifest, err := s.Manifest(ctx)
	if err != nil {
		return report, err
	}
	table, err := s.ChunkTable(ctx)
	if err != nil {
		return report, err
	}
	layers, err := s.image.Layers()
	if err != nil {
		return report, err
	}
	fail := func(err error) error {
		report.OK = false
		report.Errors = append(report.Errors, err.Error())
		if !keepGoing {
			return err
		}
		return nil
	}

	ordered := make([]index.LayerInfo, len(manifest.Layers))
	copy(ordered, manifest.Layers)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })

	buf := make([]byte, 0)
	for n, meta := range ordered {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		imageLayer := meta.Index + 2
		if imageLayer < 2 || imageLayer >= len(layers) {
			if stop := fail(fmt.Errorf("data layer %d assente (l'immagine ha %d layer)", meta.Index, len(layers))); stop != nil {
				return report, stop
			}
			continue
		}
		ociDigest, err := layers[imageLayer].Digest()
		if err != nil {
			return report, err
		}
		if progress != nil {
			progress(fmt.Sprintf("verifica: layer %d/%d: rilettura in streaming (%d chunk)",
				n+1, len(ordered), meta.ChunkTo-meta.ChunkFrom+1))
		}
		chunks, read, err := s.verifyOneLayer(ctx, layers[imageLayer].Compressed, ociDigest.String(), meta,
			manifest.Archive.Compression, table, &buf, fail)
		report.Chunks += chunks
		report.Bytes += read
		report.Layers++
		if err != nil {
			return report, err
		}
	}
	if !report.OK {
		return report, ErrStoredMismatch
	}
	return report, nil
}

// verifyOneLayer streams one data layer exactly once: the compressed bytes
// feed a sha256 compared with the layer digest, while the blob inside the
// layer tar is consumed chunk by chunk.
func (s *imageSource) verifyOneLayer(
	ctx context.Context,
	open func() (io.ReadCloser, error),
	ociDigest string,
	meta index.LayerInfo,
	codecName string,
	table *index.ChunkTable,
	buf *[]byte,
	fail func(error) error,
) (int, int64, error) {
	if meta.ChunkFrom < 0 || meta.ChunkTo >= len(table.Chunks) || meta.ChunkTo < meta.ChunkFrom {
		return 0, 0, fail(fmt.Errorf("data layer %d: intervallo chunk %d-%d fuori dalla tabella (%d chunk)",
			meta.Index, meta.ChunkFrom, meta.ChunkTo, len(table.Chunks)))
	}
	raw, err := open()
	if err != nil {
		return 0, 0, err
	}
	defer raw.Close()
	codec, err := compress.Get(codecName)
	if err != nil {
		return 0, 0, err
	}
	hash := sha256.New()
	counted := io.TeeReader(&contextReader{ctx: ctx, r: raw}, hash)
	decoded, err := codec.NewReader(counted)
	if err != nil {
		return 0, 0, err
	}
	defer decoded.Close()

	var verified int
	var read int64
	// The blob digest recorded by the backup covers the concatenation of the
	// stored chunks, which is exactly what this pass reads in order.
	blobHash := sha256.New()
	blobPath := strings.TrimPrefix(table.Chunks[meta.ChunkFrom].P, "/")
	tr := tar.NewReader(decoded)
	found := false
	for !found {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return verified, read, fail(fmt.Errorf("data layer %d: tar illeggibile: %w", meta.Index, err))
		}
		if strings.TrimPrefix(h.Name, "/") != blobPath {
			continue
		}
		found = true
		if h.Size != meta.StoredBytes {
			if stop := fail(fmt.Errorf("data layer %d: blob di %d byte, attesi %d", meta.Index, h.Size, meta.StoredBytes)); stop != nil {
				return verified, read, stop
			}
		}
		for i := meta.ChunkFrom; i <= meta.ChunkTo; i++ {
			if err := ctx.Err(); err != nil {
				return verified, read, err
			}
			c := table.Chunks[i]
			if int64(cap(*buf)) < c.Sb {
				*buf = make([]byte, c.Sb)
			}
			b := (*buf)[:c.Sb]
			if _, err := io.ReadFull(tr, b); err != nil {
				return verified, read, fail(fmt.Errorf("chunk %d troncato nel layer %d: %w", i, meta.Index, err))
			}
			read += c.Sb
			blobHash.Write(b)
			sum := sha256.Sum256(b)
			if got := "sha256:" + hex.EncodeToString(sum[:]); got != c.Ss {
				if stop := fail(fmt.Errorf("chunk %d: digest memorizzato %s, atteso %s", i, got, c.Ss)); stop != nil {
					return verified, read, stop
				}
				continue
			}
			verified++
		}
	}
	if !found {
		return verified, read, fail(fmt.Errorf("data layer %d: blob %s non trovato", meta.Index, blobPath))
	}
	// Drain the rest so the recomputed digest covers the whole compressed
	// stream, not just the part the chunks needed.
	if _, err := io.Copy(io.Discard, decoded); err != nil {
		return verified, read, fail(fmt.Errorf("data layer %d: lettura incompleta: %w", meta.Index, err))
	}
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return verified, read, fail(fmt.Errorf("data layer %d: coda del layer: %w", meta.Index, err))
	}
	if got := "sha256:" + hex.EncodeToString(blobHash.Sum(nil)); got != meta.Digest {
		if stop := fail(fmt.Errorf("data layer %d: digest del blob %s, atteso %s (metadati del backup)",
			meta.Index, got, meta.Digest)); stop != nil {
			return verified, read, stop
		}
	}
	if got := "sha256:" + hex.EncodeToString(hash.Sum(nil)); got != ociDigest {
		if stop := fail(fmt.Errorf("data layer %d: digest compresso %s, atteso %s (manifest OCI)",
			meta.Index, got, ociDigest)); stop != nil {
			return verified, read, stop
		}
	}
	return verified, read, nil
}
