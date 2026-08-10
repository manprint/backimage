// Package recovery reconstructs the plaintext tar stream stored in a
// backimage backup.  It deliberately depends only on the backup format
// packages, so it can be linked into the small self-extracting binary.
package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fpierri/backimage/pkg/compress"
	"github.com/fpierri/backimage/pkg/crypt"
	"github.com/fpierri/backimage/pkg/index"
)

// Source exposes files from the merged filesystem of a backup image.
// Implementations may be local, registry-backed, an OCI layout, or a daemon.
type Source interface {
	Open(context.Context, string) (io.ReadCloser, error)
	Close() error
}

// BlobSource is the random-access contract used by registry/OCI readers. It
// returns exact stored chunks, avoiding repeated scans of a shared layer blob.
type BlobSource interface {
	Manifest(context.Context) (*index.Manifest, error)
	ChunkTable(context.Context) (*index.ChunkTable, error)
	KeyFile(context.Context, string) ([]byte, error)
	IndexBlob(context.Context) ([]byte, error)
	Blob(context.Context, int) ([]byte, error)
	Close() error
}

// LocalSource reads a backup mounted or unpacked on the local filesystem.
type LocalSource struct{ Root string }

// Open opens name below Root and rejects paths which could escape it.
func (s *LocalSource) Open(_ context.Context, name string) (io.ReadCloser, error) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "/")
	name = strings.TrimPrefix(name, "backup/")
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("unsafe backup path %q", name)
	}
	return os.Open(filepath.Join(s.Root, clean))
}

// Close implements Source.
func (*LocalSource) Close() error { return nil }

// Backup is a validated backup metadata set plus its lazy data source.
type Backup struct {
	source   Source
	blobs    BlobSource
	Manifest *index.Manifest
	Chunks   *index.ChunkTable
	offsets  []int64
	prefix   []int64
	opener   crypt.Opener
	key      *crypt.KeyMaterial
	progress func(string)
}

// SetProgress installs an optional diagnostic callback used while rebuilding
// the plaintext stream. The callback is never invoked when it is nil.
func (b *Backup) SetProgress(fn func(string)) { b.progress = fn }

func (b *Backup) reportProgress(message string) {
	if b.progress != nil {
		b.progress(message)
	}
}

// Open reads and validates public metadata. Data blobs remain lazy.
func Open(ctx context.Context, source Source) (*Backup, error) {
	if source == nil {
		return nil, errors.New("nil backup source")
	}
	mr, err := source.Open(ctx, "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("questa immagine non è un backup backimage: %w", err)
	}
	m, err := index.ReadManifest(mr)
	closeErr := mr.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	cr, err := source.Open(ctx, "chunks.json")
	if err != nil {
		return nil, fmt.Errorf("opening chunks.json: %w", err)
	}
	t, err := index.ReadChunkTable(cr)
	closeErr = cr.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if m.Chunking.Count != len(t.Chunks) {
		return nil, fmt.Errorf("%w: manifest has %d chunks, table has %d", index.ErrBadSchema, m.Chunking.Count, len(t.Chunks))
	}

	b, err := newBackup(source, nil, m, t)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// OpenBlobSource reads validated metadata from a random-access image source.
func OpenBlobSource(ctx context.Context, source BlobSource) (*Backup, error) {
	if source == nil {
		return nil, errors.New("nil backup blob source")
	}
	m, err := source.Manifest(ctx)
	if err != nil {
		return nil, err
	}
	t, err := source.ChunkTable(ctx)
	if err != nil {
		return nil, err
	}
	return newBackup(nil, source, m, t)
}

func newBackup(source Source, blobs BlobSource, m *index.Manifest, t *index.ChunkTable) (*Backup, error) {
	if m == nil || t == nil {
		return nil, fmt.Errorf("%w: missing manifest or chunk table", index.ErrBadSchema)
	}
	if m.Chunking.Count != len(t.Chunks) {
		return nil, fmt.Errorf("%w: manifest has %d chunks, table has %d", index.ErrBadSchema, m.Chunking.Count, len(t.Chunks))
	}
	b := &Backup{
		source: source, blobs: blobs, Manifest: m, Chunks: t,
		offsets: make([]int64, len(t.Chunks)), prefix: make([]int64, len(t.Chunks)+1),
	}
	byPath := make(map[string]int64)
	for i, c := range t.Chunks {
		b.offsets[i] = byPath[c.P]
		byPath[c.P] += c.Sb
		b.prefix[i+1] = b.prefix[i] + c.Pb
	}
	if !m.Encryption.Enabled {
		var err error
		b.opener, err = crypt.NewOpener(nil)
		if err != nil {
			return nil, err
		}
	}
	return b, nil
}

// OpenLocal opens a backup directory such as /backup.
func OpenLocal(ctx context.Context, root string) (*Backup, error) {
	return Open(ctx, &LocalSource{Root: root})
}

// Close wipes key material and closes the source.
func (b *Backup) Close() error {
	if b.key != nil {
		b.key.Wipe()
		b.key = nil
	}
	if b.blobs != nil {
		return b.blobs.Close()
	}
	return b.source.Close()
}

// Unlock unwraps the backup key using a passphrase or an age identity.
func (b *Backup) Unlock(ctx context.Context, identity crypt.Identity) error {
	if !b.Manifest.Encryption.Enabled {
		return nil
	}
	name := "keys.pass.age"
	if identity.AgeKeyFile != "" {
		name = "keys.age"
	}
	var r io.ReadCloser
	var err error
	if b.blobs != nil {
		var data []byte
		data, err = b.blobs.KeyFile(ctx, name)
		if err == nil {
			r = io.NopCloser(bytes.NewReader(data))
		}
	} else {
		r, err = b.source.Open(ctx, name)
	}
	if err != nil {
		return fmt.Errorf("opening %s: %w", name, err)
	}
	km, err := crypt.UnwrapKeys(r, identity)
	closeErr := r.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		km.Wipe()
		return closeErr
	}
	opener, err := crypt.NewOpener(km)
	if err != nil {
		km.Wipe()
		return err
	}
	if b.key != nil {
		b.key.Wipe()
	}
	b.key, b.opener = km, opener
	return nil
}

// IsUnlocked reports whether plaintext can be read.
func (b *Backup) IsUnlocked() bool {
	return !b.Manifest.Encryption.Enabled || b.key != nil
}

// Index decrypts and decodes the per-file index.
func (b *Backup) Index(ctx context.Context) (*index.Index, error) {
	if b.Manifest.Encryption.Enabled && b.key == nil {
		return nil, crypt.ErrWrongPassphrase
	}
	var r io.ReadCloser
	var err error
	if b.blobs != nil {
		var data []byte
		data, err = b.blobs.IndexBlob(ctx)
		if err == nil {
			r = io.NopCloser(bytes.NewReader(data))
		}
	} else {
		r, err = b.source.Open(ctx, b.Manifest.Index.Path)
	}
	if err != nil {
		return nil, fmt.Errorf("opening index: %w", err)
	}
	idx, err := index.ReadIndex(r, b.opener)
	closeErr := r.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return idx, nil
}

// StoredChunk reads exactly one stored chunk from its shared layer blob.
func (b *Backup) StoredChunk(ctx context.Context, i int) ([]byte, error) {
	if i < 0 || i >= len(b.Chunks.Chunks) {
		return nil, fmt.Errorf("chunk %d out of range", i)
	}
	c := b.Chunks.Chunks[i]
	if b.blobs != nil {
		buf, err := b.blobs.Blob(ctx, i)
		if err != nil {
			return nil, fmt.Errorf("chunk %d: %w", i, err)
		}
		if int64(len(buf)) != c.Sb {
			clear(buf)
			return nil, fmt.Errorf("chunk %d stored size %d, want %d", i, len(buf), c.Sb)
		}
		return buf, nil
	}
	r, err := b.source.Open(ctx, c.P)
	if err != nil {
		return nil, fmt.Errorf("chunk %d: %w", i, err)
	}
	defer r.Close()
	if _, err := io.CopyN(io.Discard, r, b.offsets[i]); err != nil {
		return nil, fmt.Errorf("chunk %d seek: %w", i, err)
	}
	if c.Sb > int64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("chunk %d too large", i)
	}
	buf := make([]byte, int(c.Sb))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("chunk %d truncated: %w", i, err)
	}
	return buf, nil
}

// PlainChunk returns a streaming decompressor for one chunk. The caller must
// close it. Authentication is completed before this function returns.
func (b *Backup) PlainChunk(ctx context.Context, i int) (io.ReadCloser, error) {
	if b.Manifest.Encryption.Enabled && b.key == nil {
		return nil, crypt.ErrWrongPassphrase
	}
	stored, err := b.StoredChunk(ctx, i)
	if err != nil {
		return nil, err
	}
	var payload []byte
	var codecID compress.ID
	if b.Manifest.Encryption.Enabled {
		payload, codecID, err = b.opener.Open(nil, uint32(i), stored)
		clear(stored)
		if err != nil {
			return nil, fmt.Errorf("chunk %d authentication: %w", i, err)
		}
	} else {
		payload = stored
		codec, getErr := compress.Get(b.Manifest.Archive.Compression)
		if getErr != nil {
			clear(payload)
			return nil, getErr
		}
		codecID = codec.ID()
	}
	codec, err := compress.ByID(codecID)
	if err != nil {
		clear(payload)
		return nil, err
	}
	r, err := codec.NewReader(bytes.NewReader(payload))
	if err != nil {
		clear(payload)
		return nil, fmt.Errorf("chunk %d decompress: %w", i, err)
	}
	return &bufferedReader{ReadCloser: r, data: payload}, nil
}

type bufferedReader struct {
	io.ReadCloser
	data []byte
}

func (r *bufferedReader) Close() error {
	err := r.ReadCloser.Close()
	clear(r.data)
	r.data = nil
	return err
}

// StreamTar writes the reconstructed plaintext tar. It uses memory bounded
// by one stored chunk regardless of total backup size.
func (b *Backup) StreamTar(ctx context.Context, dst io.Writer, verify bool) error {
	for i, c := range b.Chunks.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: lettura blob, decrittazione e preparazione decompressione", i+1, len(b.Chunks.Chunks)))
		r, err := b.PlainChunk(ctx, i)
		if err != nil {
			return err
		}
		h := sha256.New()
		w := dst
		if verify {
			w = io.MultiWriter(dst, h)
		}
		n, copyErr := io.Copy(w, r)
		closeErr := r.Close()
		if copyErr != nil {
			return fmt.Errorf("chunk %d decompress: %w", i, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("chunk %d close: %w", i, closeErr)
		}
		if n != c.Pb {
			return fmt.Errorf("%w: chunk %d plaintext size %d, want %d", crypt.ErrIntegrity, i, n, c.Pb)
		}
		b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: verifica digest", i+1, len(b.Chunks.Chunks)))
		if verify && !digestMatches(c.Ps, h.Sum(nil)) {
			return fmt.Errorf("%w: chunk %d plaintext digest mismatch", crypt.ErrIntegrity, i)
		}
		b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: controllato e scritto", i+1, len(b.Chunks.Chunks)))
	}
	return nil
}

type byteRange struct{ start, end int64 }

// StreamSelectedTar writes a valid tar containing only selected entries. It
// uses TarOffset boundaries from the full index and fetches only chunks which
// intersect those entries. Raw entry records (including PAX headers) are kept
// byte-for-byte, then a fresh tar trailer is appended.
func (b *Backup) StreamSelectedTar(ctx context.Context, idx *index.Index, selected []index.FileEntry, dst io.Writer, verify bool) error {
	if idx == nil {
		return errors.New("nil index")
	}
	wanted := make(map[string]bool, len(selected))
	for _, e := range selected {
		wanted[e.Path] = true
	}
	// Preserve explicit parent-directory records. Besides fidelity, this avoids
	// leaving synthetic 0700 root-owned parents on bind-mounted restores.
	for _, e := range selected {
		for parent := path.Dir(e.Path); parent != "." && parent != "/"; parent = path.Dir(parent) {
			wanted[parent] = true
		}
	}
	// A selected hardlink needs its first occurrence to have appeared earlier.
	for changed := true; changed; {
		changed = false
		for _, e := range idx.Entries {
			if wanted[e.Path] && e.Type == index.TypeHardlink && !wanted[e.LinkTarget] {
				wanted[e.LinkTarget] = true
				changed = true
			}
		}
	}

	total := b.prefix[len(b.prefix)-1]
	contentEnd := total
	if contentEnd >= 1024 {
		contentEnd -= 1024 // original two-block tar trailer is replaced below
	}
	ranges := make([]byteRange, 0, len(wanted))
	for i, e := range idx.Entries {
		if !wanted[e.Path] {
			continue
		}
		end := contentEnd
		if i+1 < len(idx.Entries) {
			end = idx.Entries[i+1].TarOffset
		}
		if e.TarOffset < 0 || end <= e.TarOffset || end > contentEnd {
			return fmt.Errorf("%w: invalid tar offsets for %q", index.ErrBadSchema, e.Path)
		}
		if len(ranges) > 0 && ranges[len(ranges)-1].end == e.TarOffset {
			ranges[len(ranges)-1].end = end
		} else {
			ranges = append(ranges, byteRange{start: e.TarOffset, end: end})
		}
	}

	cacheIndex := -1
	var cache []byte
	defer clear(cache)
	load := func(chunkIndex int) ([]byte, error) {
		if chunkIndex == cacheIndex {
			return cache, nil
		}
		clear(cache)
		b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: lettura blob, decrittazione e preparazione decompressione", chunkIndex+1, len(b.Chunks.Chunks)))
		r, err := b.PlainChunk(ctx, chunkIndex)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(r)
		closeErr := r.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			clear(data)
			return nil, err
		}
		c := b.Chunks.Chunks[chunkIndex]
		if int64(len(data)) != c.Pb {
			clear(data)
			return nil, fmt.Errorf("%w: chunk %d plaintext size mismatch", crypt.ErrIntegrity, chunkIndex)
		}
		if verify {
			b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: verifica digest", chunkIndex+1, len(b.Chunks.Chunks)))
			h := sha256.Sum256(data)
			if !digestMatches(c.Ps, h[:]) {
				clear(data)
				return nil, fmt.Errorf("%w: chunk %d plaintext digest mismatch", crypt.ErrIntegrity, chunkIndex)
			}
		}
		b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: controllato e pronto per la selezione", chunkIndex+1, len(b.Chunks.Chunks)))
		cacheIndex, cache = chunkIndex, data
		return cache, nil
	}

	for _, span := range ranges {
		for off := span.start; off < span.end; {
			i := sort.Search(len(b.Chunks.Chunks), func(i int) bool { return b.prefix[i+1] > off })
			if i >= len(b.Chunks.Chunks) {
				return fmt.Errorf("tar offset %d outside chunk table", off)
			}
			data, err := load(i)
			if err != nil {
				return err
			}
			within := off - b.prefix[i]
			n := int64(len(data)) - within
			if remaining := span.end - off; n > remaining {
				n = remaining
			}
			if n <= 0 {
				return fmt.Errorf("chunk %d does not cover tar offset %d", i, off)
			}
			if _, err := dst.Write(data[within : within+n]); err != nil {
				return err
			}
			off += n
		}
	}
	_, err := dst.Write(make([]byte, 1024))
	return err
}

// VerifyResult summarises an integrity pass.
type VerifyResult struct {
	Chunks  int      `json:"chunks"`
	Full    bool     `json:"full"`
	OK      bool     `json:"ok"`
	Errors  []string `json:"errors"`
	Entries int      `json:"entries,omitempty"`
}

// Verify checks every stored digest and, when full is true, the plaintext and
// encrypted index. Continue controls whether all failures are collected.
func (b *Backup) Verify(ctx context.Context, full, keepGoing bool) (VerifyResult, error) {
	res := VerifyResult{Chunks: len(b.Chunks.Chunks), Full: full, OK: true}
	add := func(err error) error {
		res.OK = false
		res.Errors = append(res.Errors, err.Error())
		if !keepGoing {
			return err
		}
		return nil
	}
	for i, c := range b.Chunks.Chunks {
		stored, err := b.StoredChunk(ctx, i)
		if err == nil {
			sum := sha256.Sum256(stored)
			if !digestMatches(c.Ss, sum[:]) {
				err = fmt.Errorf("%w: chunk %d stored digest mismatch", crypt.ErrIntegrity, i)
			}
		}
		clear(stored)
		if err != nil {
			if stop := add(err); stop != nil {
				return res, stop
			}
			continue
		}
		if full {
			r, err := b.PlainChunk(ctx, i)
			if err == nil {
				h := sha256.New()
				var n int64
				n, err = io.Copy(h, r)
				closeErr := r.Close()
				if err == nil {
					err = closeErr
				}
				if err == nil && (n != c.Pb || !digestMatches(c.Ps, h.Sum(nil))) {
					err = fmt.Errorf("%w: chunk %d plaintext mismatch", crypt.ErrIntegrity, i)
				}
			}
			if err != nil {
				if stop := add(err); stop != nil {
					return res, stop
				}
			}
		}
	}
	if full {
		idx, err := b.Index(ctx)
		if err != nil {
			if stop := add(err); stop != nil {
				return res, stop
			}
		} else {
			res.Entries = len(idx.Entries)
		}
	}
	if !res.OK {
		return res, crypt.ErrIntegrity
	}
	return res, nil
}

func digestMatches(want string, got []byte) bool {
	return strings.TrimPrefix(want, "sha256:") == hex.EncodeToString(got)
}
