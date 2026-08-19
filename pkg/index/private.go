package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/crypt"
)

// PrivatePath is the name of the encrypted metadata blob inside /backup.
const PrivatePath = "private.json.zst"

// ChunkSecret holds the per-chunk fields which describe the plaintext. They
// are confidential: a plaintext digest lets anybody holding the image confirm
// whether a guessed file is part of the backup, without the key. Their
// position in Private.Chunks is the chunk index.
type ChunkSecret struct {
	Ps string `json:"ps"` // sha256 of the plaintext chunk
	Pb int64  `json:"pb"` // plain bytes
}

// PrivateEncryption carries the encryption details which identify the key and
// its recipients. They say nothing about the plaintext but they do link
// backups to each other, so they travel encrypted too.
type PrivateEncryption struct {
	KeyFingerprint string   `json:"keyFingerprint,omitempty"`
	Recipients     []string `json:"recipients,omitempty"`
}

// Private is the confidential half of the backup metadata. It exists only for
// encrypted backups, where manifest.json and chunks.json keep just what a
// reader needs to fetch and verify stored blobs without the key.
type Private struct {
	SchemaVersion int               `json:"schemaVersion"`
	Sources       []string          `json:"sources,omitempty"`
	Host          HostInfo          `json:"host"`
	Totals        Totals            `json:"totals"`
	Encryption    PrivateEncryption `json:"encryption"`
	Chunks        []ChunkSecret     `json:"chunks"`
}

// SplitPrivate moves the confidential fields of m and t into a Private value.
// Both arguments are stripped in place, so what reaches the image is the public
// half only. It returns nil when encryption is disabled: there is nothing to
// protect and the schema 1 layout is kept untouched.
func SplitPrivate(m *Manifest, t *ChunkTable) *Private {
	if m == nil || !m.Encryption.Enabled {
		return nil
	}
	p := &Private{
		SchemaVersion: SchemaVersionPrivate,
		Sources:       m.Sources,
		Host:          m.Host,
		Totals:        m.Totals,
		Encryption: PrivateEncryption{
			KeyFingerprint: m.Encryption.KeyFingerprint,
			Recipients:     m.Encryption.Recipients,
		},
	}
	m.SchemaVersion = SchemaVersionPrivate
	m.Sources = nil
	m.Host = HostInfo{}
	m.Totals = Totals{}
	m.Encryption.KeyFingerprint = ""
	m.Encryption.Recipients = nil
	m.Private = &Ref{Path: PrivatePath, Encrypted: true}
	if t == nil {
		return p
	}
	t.SchemaVersion = SchemaVersionPrivate
	p.Chunks = make([]ChunkSecret, len(t.Chunks))
	for i := range t.Chunks {
		p.Chunks[i] = ChunkSecret{Ps: t.Chunks[i].Ps, Pb: t.Chunks[i].Pb}
		t.Chunks[i].Ps = ""
		t.Chunks[i].Pb = 0
	}
	return p
}

// MergePrivate puts the confidential fields back into the in-memory manifest
// and chunk table, so every reader downstream of an unlock sees the same shape
// as an unencrypted backup.
func MergePrivate(m *Manifest, t *ChunkTable, p *Private) error {
	if m == nil || p == nil {
		return fmt.Errorf("%w: nil manifest or private metadata", ErrBadSchema)
	}
	if err := checkSchema(p.SchemaVersion); err != nil {
		return err
	}
	m.Sources = p.Sources
	m.Host = p.Host
	m.Totals = p.Totals
	m.Encryption.KeyFingerprint = p.Encryption.KeyFingerprint
	m.Encryption.Recipients = p.Encryption.Recipients
	if t == nil {
		return nil
	}
	if len(p.Chunks) != len(t.Chunks) {
		return fmt.Errorf("%w: private metadata describes %d chunks, table has %d",
			ErrBadSchema, len(p.Chunks), len(t.Chunks))
	}
	for i := range t.Chunks {
		secret := p.Chunks[i]
		if !validDigest(secret.Ps) {
			return fmt.Errorf("%w: private chunk[%d] bad sha256 digest", ErrBadSchema, i)
		}
		if secret.Pb < 0 {
			return fmt.Errorf("%w: private chunk[%d] negative plain size", ErrBadSchema, i)
		}
		t.Chunks[i].Ps = secret.Ps
		t.Chunks[i].Pb = secret.Pb
	}
	return nil
}

// WritePrivate serialises p as JSON, compresses it with zstd and seals it with
// the backup key. sealer is mandatory: this blob exists to be encrypted.
func WritePrivate(w io.Writer, p *Private, sealer crypt.Sealer) error {
	if p == nil {
		return fmt.Errorf("%w: nil private metadata", ErrBadSchema)
	}
	if sealer == nil {
		return fmt.Errorf("%w: private metadata requires encryption", ErrBadSchema)
	}
	if err := checkSchema(p.SchemaVersion); err != nil {
		return err
	}
	var z bytes.Buffer
	if err := writeZstdJSON(&z, p); err != nil {
		return err
	}
	out, err := sealMetadata(z.Bytes(), crypt.RolePrivate, sealer)
	if err != nil {
		return fmt.Errorf("sealing private metadata: %w", err)
	}
	_, err = w.Write(out)
	return err
}

// ReadPrivate reverses WritePrivate. opener must hold the backup key.
func ReadPrivate(r io.Reader, opener crypt.Opener) (*Private, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading private metadata: %w", err)
	}
	if !crypt.IsEnvelope(raw) {
		return nil, fmt.Errorf("%w: private metadata is not an encrypted blob", ErrBadSchema)
	}
	if opener == nil {
		return nil, crypt.ErrWrongPassphrase
	}
	payload, _, err := opener.Open(nil, crypt.RolePrivate, 0, raw)
	if err != nil {
		return nil, fmt.Errorf("opening private metadata: %w", err)
	}
	defer clear(payload)
	zr, err := newZstdReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	p := &Private{}
	if err := json.NewDecoder(zr).Decode(p); err != nil {
		return nil, fmt.Errorf("parsing private metadata: %w", err)
	}
	if err := checkSchema(p.SchemaVersion); err != nil {
		return nil, err
	}
	return p, nil
}

// sealMetadata wraps one small metadata payload in a crypt envelope. role tells
// the envelope which blob this is, so the file index and the private metadata
// cannot be swapped for each other under the same key. In convergent mode the
// nonce is derived by crypt from the payload itself and never from a constant:
// two backups sharing a repository key would otherwise reuse the same nonce on
// different metadata, which breaks AES-GCM.
func sealMetadata(payload []byte, role crypt.Role, sealer crypt.Sealer) ([]byte, error) {
	codec, err := compress.ByID(compress.Zstd)
	if err != nil {
		return nil, err
	}
	return sealer.Seal(nil, role, 0, codec, payload)
}
