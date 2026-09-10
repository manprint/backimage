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

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
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
	PrivateBlob(context.Context) ([]byte, error)
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
	// binding is the authenticated link this backup declared, when it has
	// one. The index blob is checked against it when it is read.
	binding  *index.Binding
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
	m, err := index.ReadManifest(index.LimitMetadata(mr, measure(mr), "manifest.json"))
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
	t, err := index.ReadChunkTable(index.LimitMetadata(cr, measure(cr), "chunks.json"))
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
	if err := checkEncryptionShape(m); err != nil {
		return nil, err
	}
	// Every stored size the reader is about to allocate from comes from
	// chunks.json, which is public. Check the whole table against the manifest
	// once, here, so no later code path has to decide whether a number it was
	// handed is plausible.
	if err := index.ValidateChunkTable(m, t); err != nil {
		return nil, err
	}
	b := &Backup{source: source, blobs: blobs, Manifest: m, Chunks: t}
	b.computeLayout()
	if !m.Encryption.Enabled {
		// Declared unencrypted: this reader can never authenticate anything,
		// and says so. An encrypted blob found later is an error rather than a
		// passphrase prompt.
		b.opener = crypt.NewClearOpener()
	}
	// Declared encrypted: no opener until Unlock produces a keyed one. The
	// choice is made here, once, from the manifest — never later from the
	// header of the blob being read.
	return b, nil
}

// checkEncryptionShape rejects manifests whose encryption fields do not
// describe a shape this program ever writes.
//
// It runs before any blob is touched because these are the fields the rest of
// the reader trusts: Encryption.Enabled picks the opener, and the private
// reference is where the plaintext digests live. A manifest that claims
// encryption while pointing at no private blob, or that claims none while
// pointing at one, is either corrupt or assembled — either way it must not
// reach the point where a blob decides what happens next.
func checkEncryptionShape(m *index.Manifest) error {
	if m.Private != nil && !m.Encryption.Enabled {
		return fmt.Errorf("%w: unencrypted backup with an encrypted private metadata blob", index.ErrBadSchema)
	}
	if m.Encryption.Enabled && m.SchemaVersion >= index.SchemaVersionPrivate && m.Private == nil {
		return fmt.Errorf("%w: encrypted backup of schema %d without its private metadata blob",
			index.ErrBadSchema, m.SchemaVersion)
	}
	return nil
}

// computeLayout derives the per-chunk offsets inside each shared layer blob
// (from the public stored sizes) and the plaintext prefix sums (from the plain
// sizes). In an encrypted backup the plain sizes are confidential, so the
// prefix table is meaningful only once loadPrivate has filled them in; every
// caller of prefix already requires an unlocked backup.
func (b *Backup) computeLayout() {
	chunks := b.Chunks.Chunks
	b.offsets = make([]int64, len(chunks))
	b.prefix = make([]int64, len(chunks)+1)
	byPath := make(map[string]int64)
	for i, c := range chunks {
		b.offsets[i] = byPath[c.P]
		byPath[c.P] += c.Sb
		b.prefix[i+1] = b.prefix[i] + c.Pb
	}
}

// loadPrivate opens the encrypted metadata blob and merges it back into the
// in-memory manifest and chunk table, so the rest of the package sees the same
// shape it has always seen. It is a no-op for a backup without one: an
// unencrypted backup, or one written by an older schema 1 backimage.
func (b *Backup) loadPrivate(ctx context.Context) error {
	ref := b.Manifest.Private
	if ref == nil || b.opener == nil {
		return nil
	}
	b.reportProgress("restore: lettura metadati privati cifrati")
	var data []byte
	var err error
	if b.blobs != nil {
		data, err = b.blobs.PrivateBlob(ctx)
	} else {
		var r io.ReadCloser
		r, err = b.source.Open(ctx, ref.Path)
		if err == nil {
			data, err = readMetadataBlob(r, ref.Path)
			closeErr := r.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		return fmt.Errorf("opening %s: %w", ref.Path, err)
	}
	private, err := index.ReadPrivate(bytes.NewReader(data), b.opener)
	if err != nil {
		return err
	}
	// Before anything else is believed: the sealed blob says which manifest
	// and which chunk table belong to this backup, and what shape they were
	// supposed to have. Checked here, the composition of pieces from
	// different backups under one key never reaches a parser, let alone a
	// destination directory.
	if err := b.checkBinding(private); err != nil {
		return err
	}
	if err := index.MergePrivate(b.Manifest, b.Chunks, private); err != nil {
		return err
	}
	b.binding = private.Binding
	b.computeLayout()
	return nil
}

// checkBinding verifies the authenticated link between the metadata files,
// and refuses a backup that should carry one and does not.
//
// "Should" is decided from authenticated state only: the key material of a
// backup written from 0.4.1 on attests the envelope it was made for, and
// every such backup binds its metadata. A blob with no binding under a key
// that attests the current envelope is therefore a private blob from
// somewhere else. Older key material attests nothing, so those backups are
// read as they always were: the property did not exist when they were
// written, and inventing it now would only refuse honest data.
func (b *Backup) checkBinding(private *index.Private) error {
	if private.Binding == nil {
		if b.key.Attested() && b.key.EnvelopeVersion >= crypt.EnvelopeVersion {
			return fmt.Errorf("%w: the key of this backup was made by a release that always binds "+
				"its metadata, but the sealed metadata carries no binding", index.ErrBadSchema)
		}
		return nil
	}
	return private.Binding.Check(b.Manifest, b.Chunks)
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
	b.reportProgress(fmt.Sprintf("restore: apertura file chiavi %s", name))
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
	if len(identity.Passphrase) > 0 {
		b.reportProgress("restore: derivazione chiave dalla passphrase con scrypt in corso")
	} else {
		b.reportProgress("restore: sblocco chiavi con identità age in corso")
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
	opener, err := crypt.NewKeyedOpener(km)
	if err != nil {
		km.Wipe()
		return err
	}
	if b.key != nil {
		b.key.Wipe()
	}
	b.key, b.opener = km, opener
	b.reportProgress("restore: chiavi backup sbloccate")
	if err := b.loadPrivate(ctx); err != nil {
		return err
	}
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
	var data []byte
	var err error
	if b.blobs != nil {
		data, err = b.blobs.IndexBlob(ctx)
	} else {
		var r io.ReadCloser
		r, err = b.source.Open(ctx, b.Manifest.Index.Path)
		if err == nil {
			data, err = readMetadataBlob(r, b.Manifest.Index.Path)
			if closeErr := r.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("opening index: %w", err)
	}
	// The manifest carries no digest of the index blob, so an index from
	// another backup of the same repository would open and parse. The sealed
	// binding names the one that belongs here.
	if err := b.binding.CheckIndexBlob(data); err != nil {
		return nil, err
	}
	return index.ReadIndex(bytes.NewReader(data), b.opener)
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
	if err := skipTo(r, b.offsets[i]); err != nil {
		return nil, fmt.Errorf("chunk %d seek: %w", i, err)
	}
	// ValidateChunkTable has already bounded Sb by what the manifest declares.
	// When the source can say how large the blob really is, that is a better
	// authority than either file: check it before allocating.
	if err := fitsInBlob(r, b.offsets[i], c.Sb); err != nil {
		return nil, fmt.Errorf("chunk %d: %w", i, err)
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

// measure reports the size of r when it is a regular file, and 0 when the
// source cannot say. It is the tighter of the two caps whenever it answers.
func measure(r io.Reader) int64 {
	stat, ok := r.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return 0
	}
	fi, err := stat.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	return fi.Size()
}

// readMetadataBlob reads one metadata blob with a cap on what it may hold.
//
// Where the source can measure the blob — a local backup and the
// self-extracting image both hand back an *os.File — the cap is the real size
// of the file, and there is nothing left to declare. Everywhere else it is
// index.DefaultMaxMetadataBytes, which is what LimitMetadata falls back to.
func readMetadataBlob(r io.Reader, what string) ([]byte, error) {
	return io.ReadAll(index.LimitMetadata(r, measure(r), what))
}

// fitsInBlob refuses a stored size that the blob cannot hold, when the source
// is able to say how large the blob is.
//
// A local backup and the self-extracting image both hand back an *os.File, so
// this is the common case and it costs one fstat. A source that cannot answer
// leaves the declared size bounded only by the manifest, which
// index.ValidateChunkTable has already checked.
func fitsInBlob(r io.Reader, offset, size int64) error {
	stat, ok := r.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return nil
	}
	fi, err := stat.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	if offset+size > fi.Size() {
		return fmt.Errorf("%w: %d stored bytes declared at offset %d of a %d byte blob",
			index.ErrBadSchema, size, offset, fi.Size())
	}
	return nil
}

// plainChunkPayload returns the authenticated compressed payload of one chunk
// together with the codec that produced it.
//
// It is the form PlainChunk is built on, kept separate because the caller —
// not the reader — must own the buffer: verifying a chunk before writing it
// means decompressing the same payload twice, and a reader that wipes its
// backing bytes on Close leaves nothing for the second pass. The caller is
// responsible for clearing what it gets back.
func (b *Backup) plainChunkPayload(ctx context.Context, i int) ([]byte, compress.ID, error) {
	if b.Manifest.Encryption.Enabled && b.key == nil {
		return nil, 0, crypt.ErrWrongPassphrase
	}
	stored, err := b.StoredChunk(ctx, i)
	if err != nil {
		return nil, 0, err
	}
	if b.Manifest.Encryption.Enabled {
		payload, codecID, openErr := b.opener.Open(nil, crypt.RoleData, uint32(i), stored)
		clear(stored)
		if openErr != nil {
			return nil, 0, fmt.Errorf("chunk %d authentication: %w", i, openErr)
		}
		return payload, codecID, nil
	}
	codec, err := compress.Get(b.Manifest.Archive.Compression)
	if err != nil {
		clear(stored)
		return nil, 0, err
	}
	return stored, codec.ID(), nil
}

// decompressPayload opens one decompression pass over an already
// authenticated payload. It does not own the payload: closing the reader
// leaves the bytes intact, so the same payload can be read again.
func decompressPayload(payload []byte, codecID compress.ID, i int, plainCap int64) (io.ReadCloser, error) {
	codec, err := compress.ByID(codecID)
	if err != nil {
		return nil, err
	}
	r, err := codec.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("chunk %d decompress: %w", i, err)
	}
	// How much plaintext this chunk is supposed to yield is recorded in the
	// backup — in the sealed private blob when there is one, in the manifest
	// otherwise. A frame that produces more than that is a bomb, and saying
	// so while decompressing costs nothing; noticing afterwards costs
	// whatever it produced.
	return &cappedReader{ReadCloser: r, r: index.LimitBytes(r, plainCap, fmt.Sprintf("the plaintext of chunk %d", i))}, nil
}

// cappedReader reads through a limit while closing the decompressor beneath
// it.
type cappedReader struct {
	io.ReadCloser
	r io.Reader
}

func (c *cappedReader) Read(p []byte) (int, error) { return c.r.Read(p) }

// plainCap is the largest plaintext chunk i may produce. The per-chunk size
// is authenticated (it travels in the sealed private blob of an encrypted
// backup) and exact; the manifest bound is the fallback for a backup that
// does not carry one, or for a read before the private blob is merged.
func (b *Backup) plainCap(i int) int64 {
	if i >= 0 && i < len(b.Chunks.Chunks) && b.Chunks.Chunks[i].Pb > 0 {
		return b.Chunks.Chunks[i].Pb
	}
	return index.MaxPlainChunkBytes(b.Manifest)
}

// skipTo positions r at offset.
//
// Chunks of a layer are concatenated in one blob file, so reading chunk i
// means reaching the sum of the stored sizes before it. Discarding those bytes
// made a full restore quadratic: a 1 GiB layer of 16 MiB chunks read ~32 GiB
// to deliver 1 GiB, because every chunk re-read the layer from the start.
// LocalSource.Open returns an *os.File, and so does the self-extracting image
// path, so the common case is a single lseek. The discard stays for the
// sources that cannot seek.
func skipTo(r io.Reader, offset int64) error {
	if offset == 0 {
		return nil
	}
	if s, ok := r.(io.Seeker); ok {
		if _, err := s.Seek(offset, io.SeekStart); err == nil {
			return nil
		}
		// A reader that claims io.Seeker but refuses the call is still
		// readable: fall through to the discard rather than fail the restore.
	}
	_, err := io.CopyN(io.Discard, r, offset)
	return err
}

// PlainChunk returns a streaming decompressor for one chunk. The caller must
// close it. Authentication is completed before this function returns.
func (b *Backup) PlainChunk(ctx context.Context, i int) (io.ReadCloser, error) {
	payload, codecID, err := b.plainChunkPayload(ctx, i)
	if err != nil {
		return nil, err
	}
	r, err := decompressPayload(payload, codecID, i, b.plainCap(i))
	if err != nil {
		clear(payload)
		return nil, err
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

// mustVerify decides whether the per-chunk plaintext digest is checked.
//
// On an encrypted backup it always is, whatever the caller asked for. Since
// 0.2.3 that digest lives in the sealed private blob, which makes it the last
// link of the integrity chain rather than the corruption check it used to be:
// it is what refuses a chunk moved between two backups that share a
// repository key, a splice AES-GCM cannot see on its own because convergent
// mode deliberately leaves the chunk position out of the authenticated data.
// Trading it for speed would reopen that hole, so --no-verify only ever
// applies to a plaintext backup, where every digest is public anyway.
func (b *Backup) mustVerify(verify bool) bool {
	return verify || b.Manifest.Encryption.Enabled
}

// StreamTar writes the reconstructed plaintext tar. It uses memory bounded
// by one stored chunk regardless of total backup size.
func (b *Backup) StreamTar(ctx context.Context, dst io.Writer, verify bool) error {
	verify = b.mustVerify(verify)
	for i := range b.Chunks.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.streamOneChunk(ctx, i, dst, verify); err != nil {
			return err
		}
	}
	b.reportIntegrity(len(b.Chunks.Chunks), len(b.Chunks.Chunks), verify)
	return nil
}

// streamOneChunk writes one chunk of the reconstructed tar.
//
// When the chunk has to be verified it is decompressed twice: the first pass
// feeds the digest and nothing else, the second one — reached only if the
// first agreed with the size and digest recorded when the backup was made —
// writes the bytes out. Checking after a single pass through
// io.MultiWriter(dst, h) meant the caller had already received the whole
// chunk by the time the mismatch was found, so the refusal came after the
// effects: on a damaged or substituted chunk about 10 KiB reached the tar
// before the error did.
//
// The case that makes this matter is not a broken AEAD tag — that is refused
// in plainChunkPayload, before a byte is decompressed — but a blob that is
// validly sealed and in the wrong place: with a convergent nonce the chunk
// index is deliberately outside the authenticated data, so a blob moved
// between two backups that share a dedup key opens cleanly and only the
// plaintext digest in the sealed private blob says otherwise.
//
// The second pass costs one more decompression and no extra memory: the
// compressed payload is already resident, so re-reading it is a second pass
// over a []byte. With --no-verify on a plaintext backup there is no digest to
// check and nothing to gain from the split, so that path keeps the single
// pass it always had.
func (b *Backup) streamOneChunk(ctx context.Context, i int, dst io.Writer, verify bool) error {
	c := b.Chunks.Chunks[i]
	total := len(b.Chunks.Chunks)
	b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: lettura blob, decrittazione e preparazione decompressione", i+1, total))
	payload, codecID, err := b.plainChunkPayload(ctx, i)
	if err != nil {
		return err
	}
	defer clear(payload)

	if !verify {
		n, err := b.copyPass(payload, codecID, i, dst)
		if err != nil {
			return err
		}
		if n != c.Pb {
			return fmt.Errorf("%w: chunk %d plaintext size %d, want %d", crypt.ErrIntegrity, i, n, c.Pb)
		}
		b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: controllato e scritto", i+1, total))
		return nil
	}

	b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: passata 1 di 2, verifica prima di scrivere", i+1, total))
	h := sha256.New()
	n, err := b.copyPass(payload, codecID, i, h)
	if err != nil {
		return err
	}
	if n != c.Pb {
		return fmt.Errorf("%w: chunk %d plaintext size %d, want %d", crypt.ErrIntegrity, i, n, c.Pb)
	}
	if !digestMatches(c.Ps, h.Sum(nil)) {
		return fmt.Errorf("%w: chunk %d plaintext digest mismatch", crypt.ErrIntegrity, i)
	}

	b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: passata 2 di 2, scrittura", i+1, total))
	written, err := b.copyPass(payload, codecID, i, dst)
	if err != nil {
		return err
	}
	if written != n {
		return fmt.Errorf("%w: chunk %d wrote %d bytes after verifying %d", crypt.ErrIntegrity, i, written, n)
	}
	b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: controllato e scritto", i+1, total))
	return nil
}

// copyPass decompresses payload once into dst and reports how much plaintext
// came out. It leaves payload untouched so it can be read again.
func (b *Backup) copyPass(payload []byte, codecID compress.ID, i int, dst io.Writer) (int64, error) {
	r, err := decompressPayload(payload, codecID, i, b.plainCap(i))
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(dst, r)
	closeErr := r.Close()
	if copyErr != nil {
		return n, fmt.Errorf("chunk %d decompress: %w", i, copyErr)
	}
	if closeErr != nil {
		return n, fmt.Errorf("chunk %d close: %w", i, closeErr)
	}
	return n, nil
}

// reportIntegrity states, as audit evidence, how much of the backup was read
// back and whether every chunk matched the digest recorded when it was made.
func (b *Backup) reportIntegrity(used, total int, verified bool) {
	if verified {
		b.reportProgress(fmt.Sprintf(
			"restore: integrità: %d/%d chunk letti e verificati (dimensione e digest plaintext coincidono con quelli registrati nel backup)",
			used, total))
		return
	}
	b.reportProgress(fmt.Sprintf(
		"restore: integrità: %d/%d chunk letti, digest plaintext NON verificati (--no-verify attivo su un backup non cifrato)",
		used, total))
}

// plainChunkBytes decrypts, decompresses and (when asked) verifies one chunk,
// returning its plaintext. It is the single place where a chunk turns into
// bytes, shared by the selective and the partial restore.
func (b *Backup) plainChunkBytes(ctx context.Context, chunkIndex int, verify bool) ([]byte, error) {
	b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: lettura blob, decrittazione e preparazione decompressione",
		chunkIndex+1, len(b.Chunks.Chunks)))
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
	b.reportProgress(fmt.Sprintf("restore: chunk %d/%d: controllato e pronto per la selezione",
		chunkIndex+1, len(b.Chunks.Chunks)))
	return data, nil
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
	verify = b.mustVerify(verify)
	wanted := selectionSet(idx, selected)

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
	used := 0
	var cache []byte
	defer clear(cache)
	load := func(chunkIndex int) ([]byte, error) {
		if chunkIndex == cacheIndex {
			return cache, nil
		}
		used++
		clear(cache)
		data, err := b.plainChunkBytes(ctx, chunkIndex, verify)
		if err != nil {
			return nil, err
		}
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
	if _, err := dst.Write(make([]byte, 1024)); err != nil {
		return err
	}
	b.reportIntegrity(used, len(b.Chunks.Chunks), verify)
	return nil
}

// selectionSet expands the entries the user asked for into the set of index
// paths a tar must carry for them to be usable.
//
// It is shared by the selective stream and by its partial variant so the two
// cannot drift: a selection that means one thing with --continue and another
// without it is the kind of difference nobody notices until a restore is
// missing a parent directory.
func selectionSet(idx *index.Index, selected []index.FileEntry) map[string]bool {
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
	return wanted
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
