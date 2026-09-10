package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Binding is the authenticated link between the metadata files of one backup.
//
// manifest.json and chunks.json are public and carry no signature of their
// own, and the only cross-check that ever existed was that the chunk counts
// agreed. Nothing stopped anybody from composing the manifest of one backup
// with the chunk table of another, or with an older index, as long as the
// counts matched: every blob would still authenticate, because they were all
// sealed with the same repository key.
//
// The private blob is sealed, so what it says about the other files cannot be
// edited without the key. It therefore says all of it: what the manifest is,
// what the chunk table is, which index blob belongs to this backup, and what
// the encryption policy was supposed to be. The reader checks it immediately
// after Unlock, before a single byte reaches a tar parser.
//
// Unencrypted backups have no authenticated file to put this in. The property
// is not available for schema 1, and pretending otherwise by storing the same
// digests in the clear would only move the problem.
type Binding struct {
	// Manifest is the digest of the public manifest in its canonical form
	// (see ManifestBindingDigest).
	Manifest string `json:"manifest"`
	// Chunks is the digest of the public chunk table (see
	// ChunkTableBindingDigest).
	Chunks string `json:"chunks"`
	// Index is the digest of the stored index blob, the bytes as they sit in
	// the image. The manifest does not carry it, so without this an older
	// index of the same repository could be served in its place.
	Index string `json:"index"`
	// Policy is what the writer expected the reader to find.
	Policy BindingPolicy `json:"policy"`
}

// BindingPolicy is the shape of the backup as its writer declared it. The
// same values live in the public manifest, where they can be rewritten; here
// they are authenticated, so a mismatch is a rewritten manifest.
type BindingPolicy struct {
	Schema          int    `json:"schema"`
	AEAD            string `json:"aead"`
	EnvelopeVersion int    `json:"envelopeVersion"`
	NonceMode       string `json:"nonceMode"`
	// IndexEncrypted states that the index blob is sealed. A backup that
	// declares encryption and serves a plaintext index is the downgrade the
	// keyed opener refuses; this says so before the blob is even fetched.
	IndexEncrypted bool `json:"indexEncrypted"`
}

// ManifestBindingDigest is the digest of the manifest in the canonical form
// both halves of the binding agree on.
//
// It is computed from the parsed structure, not from the bytes of the file:
// whitespace and key order are not part of the contract, and the writer
// cannot hash the final file anyway because the file carries the digest of
// the private blob, which in turn carries this digest. Excluding the private
// reference breaks that cycle; the reference is not left unprotected, since
// the blob it points at only opens under the backup key.
//
// The fields the private blob restores after unlocking — sources, host,
// totals, key fingerprint, recipients — are excluded too, so the digest is
// the same before and after MergePrivate. They are already authenticated:
// they are the private blob's own content.
func ManifestBindingDigest(m *Manifest) (string, error) {
	if m == nil {
		return "", fmt.Errorf("%w: nil manifest", ErrBadSchema)
	}
	canonical := *m
	canonical.Private = nil
	canonical.Sources = nil
	canonical.Host = HostInfo{}
	canonical.Totals = Totals{}
	canonical.Encryption.KeyFingerprint = ""
	canonical.Encryption.Recipients = nil
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: manifest is not serialisable: %w", ErrBadSchema, err)
	}
	return digestOf(encoded), nil
}

// ChunkTableBindingDigest is the digest of the chunk table in canonical form:
// only the public fields, so it does not change when MergePrivate puts the
// plaintext digests back. Those travel in the private blob and are
// authenticated there.
func ChunkTableBindingDigest(t *ChunkTable) (string, error) {
	if t == nil {
		return "", fmt.Errorf("%w: nil chunk table", ErrBadSchema)
	}
	public := make([]Chunk, len(t.Chunks))
	for i, c := range t.Chunks {
		public[i] = Chunk{I: c.I, P: c.P, Ss: c.Ss, Sb: c.Sb}
	}
	encoded, err := json.Marshal(ChunkTable{SchemaVersion: t.SchemaVersion, Chunks: public})
	if err != nil {
		return "", fmt.Errorf("%w: chunk table is not serialisable: %w", ErrBadSchema, err)
	}
	return digestOf(encoded), nil
}

// BlobBindingDigest is the digest of a stored blob, in the same shape the
// manifest uses for its own references.
func BlobBindingDigest(blob []byte) string { return digestOf(blob) }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NewBinding builds the binding of one backup. The manifest and the chunk
// table must already be in their public form, and indexBlob must be the
// stored bytes of the index.
func NewBinding(m *Manifest, t *ChunkTable, indexBlob []byte) (*Binding, error) {
	manifestDigest, err := ManifestBindingDigest(m)
	if err != nil {
		return nil, err
	}
	chunksDigest, err := ChunkTableBindingDigest(t)
	if err != nil {
		return nil, err
	}
	return &Binding{
		Manifest: manifestDigest,
		Chunks:   chunksDigest,
		Index:    BlobBindingDigest(indexBlob),
		Policy: BindingPolicy{
			Schema:          m.SchemaVersion,
			AEAD:            m.Encryption.AEAD,
			EnvelopeVersion: m.Encryption.EnvelopeVersion,
			NonceMode:       m.Encryption.NonceMode,
			IndexEncrypted:  m.Index.Encrypted,
		},
	}, nil
}

// Check verifies the manifest and the chunk table against this binding, in
// the order the reader needs: policy first, because a backup that is not the
// shape it claims must not have its digests believed either.
func (b *Binding) Check(m *Manifest, t *ChunkTable) error {
	if b == nil {
		return fmt.Errorf("%w: no binding", ErrBadSchema)
	}
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrBadSchema)
	}
	if err := b.checkPolicy(m); err != nil {
		return err
	}
	manifestDigest, err := ManifestBindingDigest(m)
	if err != nil {
		return err
	}
	if manifestDigest != b.Manifest {
		return fmt.Errorf("%w: manifest.json does not belong to this backup (%s, the sealed metadata names %s)",
			ErrBadSchema, manifestDigest, b.Manifest)
	}
	chunksDigest, err := ChunkTableBindingDigest(t)
	if err != nil {
		return err
	}
	if chunksDigest != b.Chunks {
		return fmt.Errorf("%w: chunks.json does not belong to this backup (%s, the sealed metadata names %s)",
			ErrBadSchema, chunksDigest, b.Chunks)
	}
	return nil
}

// CheckIndexBlob verifies the stored index blob against this binding. It is
// separate because the index is fetched later, when a reader actually needs
// the file table.
func (b *Binding) CheckIndexBlob(blob []byte) error {
	if b == nil || b.Index == "" {
		return nil
	}
	if got := BlobBindingDigest(blob); got != b.Index {
		return fmt.Errorf("%w: the index blob does not belong to this backup (%s, the sealed metadata names %s)",
			ErrBadSchema, got, b.Index)
	}
	return nil
}

func (b *Binding) checkPolicy(m *Manifest) error {
	type field struct {
		name      string
		want, got any
	}
	for _, f := range []field{
		{"schema", b.Policy.Schema, m.SchemaVersion},
		{"aead", b.Policy.AEAD, m.Encryption.AEAD},
		{"envelopeVersion", b.Policy.EnvelopeVersion, m.Encryption.EnvelopeVersion},
		{"nonceMode", b.Policy.NonceMode, m.Encryption.NonceMode},
		{"index encrypted", b.Policy.IndexEncrypted, m.Index.Encrypted},
	} {
		if f.want != f.got {
			return fmt.Errorf("%w: manifest.json declares %s %v, the sealed policy says %v",
				ErrBadSchema, f.name, f.got, f.want)
		}
	}
	return nil
}
