// Package index defines the three metadata contracts of a backimage backup:
//
//	/backup/manifest.json            public, small, no chunk list
//	/backup/chunks.json              public, may be large
//	/backup/index.json.zst[.age]     per-file table, encrypted when enabled
//
// The JSON tags are part of the wire contract (overview.md §4.1–4.3) and
// must not be renamed. schemaVersion 1 is the only supported version: every
// reader rejects newer schemas with the prescribed error.
package index

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/fpierri/backimage/pkg/compress"
	"github.com/fpierri/backimage/pkg/crypt"
)

// SchemaVersion is the version written into every metadata file.
const SchemaVersion = 1

// ErrBadSchema wraps validation failures of every metadata file.
var ErrBadSchema = errors.New("invalid backup metadata")

// ErrFutureSchema is wrapped when the file was written by a newer tool.
var ErrFutureSchema = errors.New("backup created by a more recent backimage")

// Entry type codes (strings, stable across platforms).
const (
	TypeRegular  = "reg"
	TypeDir      = "dir"
	TypeSymlink  = "sym"
	TypeHardlink = "hard"
	TypeChar     = "chr"
	TypeBlock    = "blk"
	TypeFifo     = "fifo"
)

// entryTypes is the closed set of valid type codes.
var entryTypes = map[string]bool{
	TypeRegular: true, TypeDir: true, TypeSymlink: true, TypeHardlink: true,
	TypeChar: true, TypeBlock: true, TypeFifo: true,
}

// ToolInfo identifies the producing binary.
type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// HostInfo identifies the backup host.
type HostInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// Totals counts archived objects and bytes.
type Totals struct {
	Files       int64 `json:"files"`
	Dirs        int64 `json:"dirs"`
	Symlinks    int64 `json:"symlinks"`
	Hardlinks   int64 `json:"hardlinks"`
	Devices     int64 `json:"devices"`
	BytesRaw    int64 `json:"bytesRaw"`
	BytesStored int64 `json:"bytesStored"`
}

// ArchiveInfo describes the tar pipeline.
type ArchiveInfo struct {
	Format           string `json:"format"`
	Compression      string `json:"compression"`
	CompressionLevel int    `json:"compressionLevel"`
}

// EncryptionInfo describes how chunks are encrypted.
type EncryptionInfo struct {
	Enabled    bool     `json:"enabled"`
	KDF        string   `json:"kdf"`
	AEAD       string   `json:"aead"`
	NonceMode  string   `json:"nonceMode"`
	Recipients []string `json:"recipients"`
}

// ChunkingInfo describes the chunker.
type ChunkingInfo struct {
	Strategy         string `json:"strategy"`
	TargetChunkBytes int64  `json:"targetChunkBytes"`
	Count            int    `json:"count"`
}

// LayerInfo describes one shared data layer.
type LayerInfo struct {
	Index       int    `json:"index"`
	Digest      string `json:"digest"`
	ChunkFrom   int    `json:"chunkFrom"`
	ChunkTo     int    `json:"chunkTo"`
	StoredBytes int64  `json:"storedBytes"`
}

// Ref locates the per-file index blob.
type Ref struct {
	Path         string `json:"path"`
	StoredSha256 string `json:"storedSha256"`
	Encrypted    bool   `json:"encrypted"`
}

// Manifest is the public, unencrypted description of a backup.
type Manifest struct {
	SchemaVersion int            `json:"schemaVersion"`
	Tool          ToolInfo       `json:"tool"`
	CreatedAt     time.Time      `json:"createdAt"`
	Sources       []string       `json:"sources"`
	Host          HostInfo       `json:"host"`
	Totals        Totals         `json:"totals"`
	Archive       ArchiveInfo    `json:"archive"`
	Encryption    EncryptionInfo `json:"encryption"`
	Chunking      ChunkingInfo   `json:"chunking"`
	Layers        []LayerInfo    `json:"layers"`
	Index         Ref            `json:"index"`
}

// WriteManifest serialises m as indented JSON.
func WriteManifest(w io.Writer, m *Manifest) error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrBadSchema)
	}
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(m)
}

// ReadManifest parses and validates a manifest.
func ReadManifest(r io.Reader) (*Manifest, error) {
	m := &Manifest{}
	if err := json.NewDecoder(r).Decode(m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if err := checkSchema(m.SchemaVersion); err != nil {
		return nil, err
	}
	if m.Tool.Name == "" || m.Tool.Version == "" {
		return nil, fmt.Errorf("%w: tool name/version missing", ErrBadSchema)
	}
	if m.CreatedAt.IsZero() {
		return nil, fmt.Errorf("%w: createdAt is zero", ErrBadSchema)
	}
	if m.Archive.Format == "" || m.Archive.Compression == "" {
		return nil, fmt.Errorf("%w: archive.format/compression missing", ErrBadSchema)
	}
	if m.Chunking.Strategy == "" || m.Chunking.TargetChunkBytes <= 0 {
		return nil, fmt.Errorf("%w: chunking missing", ErrBadSchema)
	}
	if m.Index.Path == "" {
		return nil, fmt.Errorf("%w: index.path missing", ErrBadSchema)
	}
	for i, l := range m.Layers {
		if l.Digest == "" || l.ChunkFrom < 0 || l.ChunkTo < l.ChunkFrom {
			return nil, fmt.Errorf("%w: malformed layer %d", ErrBadSchema, i)
		}
	}
	return m, nil
}

// Chunk maps one chunk index to its stored blob.
type Chunk struct {
	I  int    `json:"i"`
	P  string `json:"p"`  // blob path inside the image, e.g. backup/data/000000.blob
	Ps string `json:"ps"` // sha256 of the plaintext chunk (dedup, phase 10)
	Ss string `json:"ss"` // sha256 of the stored blob
	Pb int64  `json:"pb"` // plain bytes
	Sb int64  `json:"sb"` // stored bytes
}

// ChunkTable maps chunks to blobs. It can be large and is written separately.
type ChunkTable struct {
	SchemaVersion int     `json:"schemaVersion"`
	Chunks        []Chunk `json:"chunks"`
}

// WriteChunkTable serialises t as compact JSON (no indentation).
func WriteChunkTable(w io.Writer, t *ChunkTable) error {
	if t == nil {
		return fmt.Errorf("%w: nil chunk table", ErrBadSchema)
	}
	return json.NewEncoder(w).Encode(t)
}

// ReadChunkTable parses and validates a chunk table.
func ReadChunkTable(r io.Reader) (*ChunkTable, error) {
	t := &ChunkTable{}
	if err := json.NewDecoder(r).Decode(t); err != nil {
		return nil, fmt.Errorf("parsing chunk table: %w", err)
	}
	if err := checkSchema(t.SchemaVersion); err != nil {
		return nil, err
	}
	for i, c := range t.Chunks {
		if c.I != i {
			return nil, fmt.Errorf("%w: chunk[%d] has index %d", ErrBadSchema, i, c.I)
		}
		if c.P == "" {
			return nil, fmt.Errorf("%w: chunk[%d] missing blob path", ErrBadSchema, i)
		}
		if c.Pb < 0 || c.Sb < 0 {
			return nil, fmt.Errorf("%w: chunk[%d] negative sizes", ErrBadSchema, i)
		}
		if !validDigest(c.Ps) || !validDigest(c.Ss) {
			return nil, fmt.Errorf("%w: chunk[%d] bad sha256 digest", ErrBadSchema, i)
		}
	}
	return t, nil
}

// FileEntry describes one archived filesystem object.
type FileEntry struct {
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       string    `json:"mode"` // octal string, e.g. "0644"
	UID        int       `json:"uid"`
	GID        int       `json:"gid"`
	UName      string    `json:"uname"`
	GName      string    `json:"gname"`
	MTime      time.Time `json:"mtime"`
	LinkTarget string    `json:"linkTarget"`
	TarOffset  int64     `json:"tarOffset"`
	SHA256     string    `json:"sha256"`
}

// Index is the per-file table. It is encrypted when encryption is enabled.
type Index struct {
	SchemaVersion int         `json:"schemaVersion"`
	Entries       []FileEntry `json:"entries"`
}

// FormatMode renders a file mode as an octal string with a leading zero
// ("0644", "04755"): one leader plus 3–4 octal digits.
func FormatMode(m uint32) string {
	d := strconv.FormatUint(uint64(m), 8)
	if len(d) < 3 {
		d = strings.Repeat("0", 3-len(d)) + d
	}
	return "0" + d
}

// ParseMode parses an octal mode string ("0644", "04755", up to 6 digits).
func ParseMode(s string) (uint32, error) {
	if s == "" || len(s) > 6 || s[0] != '0' {
		return 0, fmt.Errorf("invalid mode %q (want octal with leading zero)", s)
	}
	var v uint32
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '7' {
			return 0, fmt.Errorf("invalid mode %q", s)
		}
		v = v*8 + uint32(c-'0')
	}
	return v, nil
}

// WriteIndex serialises the index as JSON, compresses it with zstd and, when
// sealer is non-nil, wraps it in a crypt envelope. The zstd frame is produced
// with a single worker: equal inputs yield equal bytes.
func WriteIndex(w io.Writer, idx *Index, sealer crypt.Sealer) error {
	if idx == nil {
		return fmt.Errorf("%w: nil index", ErrBadSchema)
	}
	if err := checkSchema(idx.SchemaVersion); err != nil {
		return err
	}
	if err := validateEntries(idx.Entries); err != nil {
		return err
	}

	// Streaming: JSON is encoded straight into the zstd writer so the entry
	// list is never materialised as a plaintext buffer. The encrypted path
	// needs the whole compressed payload for the GCM envelope, so it lands in
	// a small buffer instead.
	if sealer == nil {
		return writeIndexCompressed(w, idx)
	}

	var z bytes.Buffer
	if err := writeIndexCompressed(&z, idx); err != nil {
		return err
	}
	codec, err := compress.ByID(compress.Zstd)
	if err != nil {
		return err
	}
	out, err := sealer.Seal(nil, 0, codec, z.Bytes(), [32]byte{})
	if err != nil {
		return fmt.Errorf("sealing index: %w", err)
	}
	_, err = w.Write(out)
	return err
}

// writeIndexCompressed encodes idx as compact JSON compressed with zstd.
func writeIndexCompressed(dst io.Writer, idx *Index) error {
	zw, err := zstd.NewWriter(dst,
		zstd.WithEncoderConcurrency(1), // deterministic single-worker frame
	)
	if err != nil {
		return fmt.Errorf("zstd init: %w", err)
	}
	encodeErr := json.NewEncoder(zw).Encode(idx)
	closeErr := zw.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

// ReadIndex reverses WriteIndex. opener may be nil when the index is known
// to be unencrypted; a keyless opener from crypt handles clear envelopes.
func ReadIndex(r io.Reader, opener crypt.Opener) (*Index, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading index blob: %w", err)
	}
	payload := raw
	if crypt.IsEnvelope(raw) {
		if opener == nil {
			opener, err = crypt.NewOpener(nil)
			if err != nil {
				return nil, err
			}
		}
		payload, _, err = opener.Open(nil, 0, raw)
		if err != nil {
			return nil, fmt.Errorf("opening index envelope: %w", err)
		}
	}
	zr, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("zstd init: %w", err)
	}
	if err := zr.Reset(bytes.NewReader(payload)); err != nil {
		return nil, fmt.Errorf("zstd reset: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("zstd init: %w", err)
	}
	defer zr.Close()

	idx := &Index{}
	if err := decodeIndexStreaming(zr, idx); err != nil {
		return nil, err
	}
	if err := checkSchema(idx.SchemaVersion); err != nil {
		return nil, err
	}
	if err := validateEntries(idx.Entries); err != nil {
		return nil, err
	}
	return idx, nil
}

// decodeIndexStreaming de-escapes the compressed JSON without loading the
// whole entry list into memory: entries are decoded one at a time.
func decodeIndexStreaming(r io.Reader, idx *Index) error {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("index start: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("index: expected object start")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("index key: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("index: bad key token")
		}
		switch key {
		case "schemaVersion":
			if err := dec.Decode(&idx.SchemaVersion); err != nil {
				return fmt.Errorf("index schemaVersion: %w", err)
			}
		case "entries":
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := tok.(json.Delim); !ok || d != '[' {
				return fmt.Errorf("index: entries is not an array")
			}
			for dec.More() {
				var e FileEntry
				if err := dec.Decode(&e); err != nil {
					return fmt.Errorf("index entry: %w", err)
				}
				idx.Entries = append(idx.Entries, e)
			}
			if _, err := dec.Token(); err != nil { // closing ']'
				return fmt.Errorf("index entries end: %w", err)
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return fmt.Errorf("skipping key %q: %w", key, err)
			}
		}
	}
	return nil
}

func validateEntries(entries []FileEntry) error {
	for i, e := range entries {
		if e.Path == "" {
			return fmt.Errorf("%w: entry[%d] empty path", ErrBadSchema, i)
		}
		if !entryTypes[e.Type] {
			return fmt.Errorf("%w: entry[%d] unknown type %q", ErrBadSchema, i, e.Type)
		}
		if e.Size < 0 || e.TarOffset < 0 {
			return fmt.Errorf("%w: entry[%d] negative size/offset", ErrBadSchema, i)
		}
		if _, err := ParseMode(e.Mode); err != nil {
			return fmt.Errorf("%w: entry[%d]: %w", ErrBadSchema, i, err)
		}
		if e.Type == TypeRegular && !validHex64(e.SHA256) {
			return fmt.Errorf("%w: entry[%d] bad sha256", ErrBadSchema, i)
		}
		if (e.Type == TypeSymlink || e.Type == TypeHardlink) && e.LinkTarget == "" {
			return fmt.Errorf("%w: entry[%d] %v without link target", ErrBadSchema, i, e.Type)
		}
	}
	return nil
}

// validDigest checks "sha256:<64 lowercase hex>".
func validDigest(s string) bool {
	return strings.HasPrefix(s, "sha256:") && validHex64(s[len("sha256:"):])
}

// validHex64 checks exactly 64 lowercase hex digits.
func validHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
}

func checkSchema(got int) error {
	if got != SchemaVersion {
		return fmt.Errorf("%w: update backimage (schema %d, supported %d)",
			ErrFutureSchema, got, SchemaVersion)
	}
	return nil
}

// avoid unused import if build tags change
