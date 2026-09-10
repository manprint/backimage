package index

import (
	"bytes"
	"errors"
	"testing"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/crypt"
)

// clearSealer produces the blobs an attacker can write without any key: valid
// backimage envelopes with aead=none.
func clearSealer(t *testing.T) crypt.Sealer {
	t.Helper()
	s, err := crypt.NewSealer(nil, crypt.NonceRandom)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadIndexOfEncryptedBackupRejectsAClearEnvelope(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteIndex(&buf, sampleIndex(), clearSealer(t)); err != nil {
		t.Fatal(err)
	}
	_, opener, _ := testSealerOpener(t, crypt.NonceRandom)
	if _, err := ReadIndex(bytes.NewReader(buf.Bytes()), opener); err == nil {
		t.Fatal("an unauthenticated index must not be readable inside an encrypted backup")
	} else if !errors.Is(err, crypt.ErrIntegrity) {
		t.Fatalf("want an integrity failure, got %v", err)
	}
}

// A stripped index is the cheaper attack: drop the envelope entirely and leave
// the bare zstd frame. Before, the reader sniffed the blob and read it as
// plain zstd, so the key it was holding never came into it.
func TestReadIndexOfEncryptedBackupRejectsBareZstd(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteIndex(&buf, sampleIndex(), nil); err != nil {
		t.Fatal(err)
	}
	_, opener, _ := testSealerOpener(t, crypt.NonceRandom)
	if _, err := ReadIndex(bytes.NewReader(buf.Bytes()), opener); err == nil {
		t.Fatal("an index with no envelope must not be readable inside an encrypted backup")
	} else if !errors.Is(err, ErrBadSchema) {
		t.Fatalf("want a format failure, got %v", err)
	}
}

func TestReadIndexRequiresAnExplicitExpectation(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteIndex(&buf, sampleIndex(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndex(bytes.NewReader(buf.Bytes()), nil); err == nil {
		t.Fatal("reading an index without stating the expected encryption must fail")
	} else if !errors.Is(err, ErrBadSchema) {
		t.Fatalf("want a format failure, got %v", err)
	}
}

func TestReadPrivateRefusesAnOpenerWithoutAKey(t *testing.T) {
	sealer, _, _ := testSealerOpener(t, crypt.NonceRandom)
	var buf bytes.Buffer
	if err := WritePrivate(&buf, &Private{SchemaVersion: SchemaVersionPrivate}, sealer); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(bytes.NewReader(buf.Bytes()), crypt.NewClearOpener()); err == nil {
		t.Fatal("the confidential metadata must not be read by an opener that cannot authenticate")
	} else if !errors.Is(err, ErrBadSchema) {
		t.Fatalf("want a format failure, got %v", err)
	}
}

// The envelope magic says "backimage blob", not "authenticated blob": a header
// declaring aead=none carries it just as well. This is the case the magic
// check alone used to let through.
func TestReadPrivateRejectsAClearEnvelope(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrivate(&buf, &Private{SchemaVersion: SchemaVersionPrivate}, clearSealer(t)); err != nil {
		t.Fatal(err)
	}
	if !crypt.IsEnvelope(buf.Bytes()) {
		t.Fatal("the forged blob must still look like a backimage envelope")
	}
	_, opener, _ := testSealerOpener(t, crypt.NonceRandom)
	if _, err := ReadPrivate(bytes.NewReader(buf.Bytes()), opener); err == nil {
		t.Fatal("unauthenticated private metadata must be refused")
	} else if !errors.Is(err, crypt.ErrIntegrity) {
		t.Fatalf("want an integrity failure, got %v", err)
	}
}

// The refusal paths of the metadata readers are the other half of A01: a
// reader that authenticates its blobs but then accepts any shape inside them
// has only moved the problem one layer in. These exercise the rejections that
// stand between a rewritten metadata file and the rest of the program.

func TestReadChunkTableRejectsMalformedRows(t *testing.T) {
	cases := map[string]string{
		"not json":         `{`,
		"future schema":    `{"schemaVersion":99,"chunks":[]}`,
		"reordered index":  `{"schemaVersion":1,"chunks":[{"i":7,"p":"data/0.blob","ss":"sha256:` + dig(1) + `"}]}`,
		"no blob path":     `{"schemaVersion":1,"chunks":[{"i":0,"p":"","ss":"sha256:` + dig(1) + `"}]}`,
		"negative size":    `{"schemaVersion":1,"chunks":[{"i":0,"p":"data/0.blob","sb":-1,"ss":"sha256:` + dig(1) + `"}]}`,
		"bad digest":       `{"schemaVersion":1,"chunks":[{"i":0,"p":"data/0.blob","ss":"sha256:zz"}]}`,
		"bad plain digest": `{"schemaVersion":1,"chunks":[{"i":0,"p":"data/0.blob","ss":"sha256:` + dig(1) + `","ps":"nope"}]}`,
	}
	for name, blob := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadChunkTable(bytes.NewReader([]byte(blob))); err == nil {
				t.Fatal("a malformed chunk table must be refused")
			}
		})
	}
}

func TestMergePrivateRejectsMismatchedMetadata(t *testing.T) {
	table := &ChunkTable{SchemaVersion: SchemaVersionPrivate, Chunks: []Chunk{
		{I: 0, P: "data/0.blob", Ss: "sha256:" + dig(1)},
	}}
	m := &Manifest{SchemaVersion: SchemaVersionPrivate}

	if err := MergePrivate(nil, table, &Private{SchemaVersion: SchemaVersionPrivate}); err == nil {
		t.Fatal("a nil manifest must be refused")
	}
	if err := MergePrivate(m, table, nil); err == nil {
		t.Fatal("nil private metadata must be refused")
	}
	if err := MergePrivate(m, table, &Private{SchemaVersion: 99}); err == nil {
		t.Fatal("a future private schema must be refused")
	}
	// Count mismatch: the case that lets metadata from another backup in.
	if err := MergePrivate(m, table, &Private{SchemaVersion: SchemaVersionPrivate}); err == nil {
		t.Fatal("private metadata describing a different number of chunks must be refused")
	}
	bad := &Private{SchemaVersion: SchemaVersionPrivate, Chunks: []ChunkSecret{{Ps: "nope", Pb: 1}}}
	if err := MergePrivate(m, table, bad); err == nil {
		t.Fatal("a malformed plaintext digest must be refused")
	}
	negative := &Private{SchemaVersion: SchemaVersionPrivate, Chunks: []ChunkSecret{{Ps: "sha256:" + dig(1), Pb: -1}}}
	if err := MergePrivate(m, table, negative); err == nil {
		t.Fatal("a negative plaintext size must be refused")
	}
}

func TestReadPrivateRejectsGarbageUnderTheSeal(t *testing.T) {
	sealer, opener, _ := testSealerOpener(t, crypt.NonceRandom)
	// Authenticated, and still not private metadata: the seal proves who wrote
	// the bytes, not that they parse.
	blob, err := sealer.Seal(nil, crypt.RolePrivate, 0, zstdCodec(t), []byte("not zstd json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(bytes.NewReader(blob), opener); err == nil {
		t.Fatal("a sealed blob that is not private metadata must be refused")
	}
}

func zstdCodec(t *testing.T) compress.Codec {
	t.Helper()
	c, err := compress.ByID(compress.Zstd)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
