package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
)

// This file is the end-to-end regression for the authentication bypass on the
// encrypted read path. The forgeries below need no key at all: a backimage
// envelope with aead=none is something anyone who can rewrite a blob can
// produce, and every public field it depends on — stored size, stored digest,
// layer size — is public precisely so a reader can fetch blobs before holding
// a key. What used to be missing is the reader refusing to read them.
//
// Every case asserts three things, and the third is the one that matters: the
// call fails, the failure is classified as integrity or format, and **not one
// byte** reached the consumer. A restore that errors after writing part of a
// forged tar has still written it.

// countingWriter fails the test if anything is written to it.
type countingWriter struct{ n int }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += len(p); return len(p), nil }

func clearSealer(t *testing.T) crypt.Sealer {
	t.Helper()
	s, err := crypt.NewSealer(nil, crypt.NonceRandom)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func openUnlocked(t *testing.T, f fixture) *Backup {
	t.Helper()
	b, err := OpenLocal(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	if err := b.Unlock(context.Background(), crypt.Identity{Passphrase: []byte(fixturePassphrase)}); err != nil {
		t.Fatal(err)
	}
	return b
}

// forgeClearData rewrites the whole data layer as unauthenticated blobs and
// updates the public bookkeeping to match, exactly as an attacker with write
// access to the image would.
func forgeClearData(t *testing.T, f fixture) {
	t.Helper()
	codec, err := compress.Get("store")
	if err != nil {
		t.Fatal(err)
	}
	sealer := clearSealer(t)

	table := readChunkTable(t, f)
	var stored bytes.Buffer
	for i := range table.Chunks {
		row := &table.Chunks[i]
		start := int64(0)
		for j := 0; j < i; j++ {
			start += table.Chunks[j].Pb
		}
		plain := f.tarBytes[start : start+row.Pb]
		blob, err := sealer.Seal(nil, crypt.RoleData, uint32(i), codec, plain)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(blob)
		row.Sb = int64(len(blob))
		row.Ss = "sha256:" + hex.EncodeToString(sum[:])
		stored.Write(blob)
	}
	if err := os.WriteFile(f.chunkPath, stored.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.root, "chunks.json"), func(w io.Writer) error {
		return index.WriteChunkTable(w, table)
	})

	m := readManifest(t, f)
	m.Layers[0].StoredBytes = int64(stored.Len())
	writeFile(t, filepath.Join(f.root, "manifest.json"), func(w io.Writer) error {
		return index.WriteManifest(w, m)
	})
}

// forgeClearIndex replaces the sealed file index with an unauthenticated one
// carrying the same entries: the blob still opens, nothing signs it.
func forgeClearIndex(t *testing.T, f fixture) {
	t.Helper()
	b := openUnlocked(t, f)
	idx, err := b.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.root, "index.json.zst"), func(w io.Writer) error {
		return index.WriteIndex(w, idx, clearSealer(t))
	})
}

// forgeClearPrivate replaces the sealed confidential metadata — which is where
// the plaintext digests live, so it is the last link of the integrity chain —
// with an unauthenticated copy of itself.
func forgeClearPrivate(t *testing.T, f fixture) {
	t.Helper()
	opener, err := crypt.NewKeyedOpener(f.km)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.root, index.PrivatePath))
	if err != nil {
		t.Fatal(err)
	}
	private, err := index.ReadPrivate(bytes.NewReader(raw), opener)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.root, index.PrivatePath), func(w io.Writer) error {
		return index.WritePrivate(w, private, clearSealer(t))
	})
}

func readManifest(t *testing.T, f fixture) *index.Manifest {
	t.Helper()
	r, err := os.Open(filepath.Join(f.root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m, err := index.ReadManifest(r)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func readChunkTable(t *testing.T, f fixture) *index.ChunkTable {
	t.Helper()
	r, err := os.Open(filepath.Join(f.root, "chunks.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	table, err := index.ReadChunkTable(r)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func isRefusal(err error) bool {
	return errors.Is(err, crypt.ErrIntegrity) ||
		errors.Is(err, index.ErrBadSchema) ||
		errors.Is(err, crypt.ErrWrongPassphrase)
}

func TestEncryptedBackupRefusesDowngradedBlobs(t *testing.T) {
	// Which blob is forged decides which surfaces must refuse: StreamTar never
	// looks at the file index, so a forged index cannot make it serve anything
	// wrong, and asserting otherwise would be testing the test. Each case
	// states the reach of its own forgery, and every surface outside that
	// reach is asserted to still work — a fix that refuses everything is not a
	// fix.
	cases := []struct {
		name          string
		forge         func(*testing.T, fixture)
		dataForged    bool
		indexForged   bool
		refusedAtOpen bool // the private blob is read during Unlock
	}{
		{"data", forgeClearData, true, false, false},
		{"index", forgeClearIndex, false, true, false},
		{"private", forgeClearPrivate, false, false, true},
		{"all", func(t *testing.T, f fixture) {
			// Order matters: index and private must be read through the real
			// key before the data layer stops authenticating.
			forgeClearIndex(t, f)
			forgeClearPrivate(t, f)
			forgeClearData(t, f)
		}, true, true, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, verify := range []bool{true, false} {
				label := "verify"
				if !verify {
					label = "no-verify"
				}
				t.Run(label, func(t *testing.T) {
					f := makePrivateFixture(t, 4096)
					c.forge(t, f)
					runDowngradeAssertions(t, f, verify, c.dataForged, c.indexForged, c.refusedAtOpen)
				})
			}
		})
	}
}

// runDowngradeAssertions exercises every way a reader can be asked for
// plaintext. A hole in one of them is a hole in all of them: they are
// different entry points into the same bytes.
func runDowngradeAssertions(t *testing.T, f fixture, verify, dataForged, indexForged, refusedAtOpen bool) {
	t.Helper()
	ctx := context.Background()

	b, err := OpenLocal(ctx, f.root)
	if err != nil {
		// A forgery caught while opening the backup is a refusal too, as long
		// as it is classified as one.
		if !isRefusal(err) {
			t.Fatalf("OpenLocal: want a refusal, got %v", err)
		}
		return
	}
	defer b.Close()

	unlockErr := b.Unlock(ctx, crypt.Identity{Passphrase: []byte(fixturePassphrase)})
	if refusedAtOpen && unlockErr == nil {
		t.Fatal("unauthenticated private metadata must be refused while unlocking, " +
			"before any data path becomes reachable")
	}
	if unlockErr != nil {
		// The private blob is refused at unlock time, before any data path is
		// reachable: nothing further can be exercised, and nothing further
		// needs to be.
		if !isRefusal(unlockErr) {
			t.Fatalf("Unlock: want a refusal, got %v", unlockErr)
		}
		return
	}

	t.Run("StreamTar", func(t *testing.T) {
		w := &countingWriter{}
		err := b.StreamTar(ctx, w, verify)
		if dataForged {
			requireRefusalWithoutOutput(t, err, w.n)
			return
		}
		if err != nil {
			t.Fatalf("an untouched data path must still stream: %v", err)
		}
	})

	idx, idxErr := b.Index(ctx)
	if indexForged {
		if idxErr == nil {
			t.Fatal("an unauthenticated file index must not be decoded")
		}
		if !isRefusal(idxErr) {
			t.Fatalf("Index: want a refusal, got %v", idxErr)
		}
	} else if idxErr != nil {
		t.Fatalf("an untouched index must still be readable: %v", idxErr)
	}

	if idx != nil {
		t.Run("StreamSelectedTar", func(t *testing.T) {
			w := &countingWriter{}
			err := b.StreamSelectedTar(ctx, idx, idx.Entries, w, verify)
			if dataForged {
				requireRefusalWithoutOutput(t, err, w.n)
				return
			}
			if err != nil {
				t.Fatalf("an untouched data path must still stream a selection: %v", err)
			}
		})
		t.Run("StreamTarPartial", func(t *testing.T) {
			w := &countingWriter{}
			report, err := b.StreamTarPartial(ctx, idx, w, verify)
			if !dataForged {
				if err != nil {
					t.Fatalf("an untouched data path must still recover: %v", err)
				}
				return
			}
			// The partial recovery is allowed to succeed while dropping every
			// entry: what it must never do is write forged content. The 1024
			// bytes it always emits are the empty tar trailer.
			if err != nil && !isRefusal(err) {
				t.Fatalf("StreamTarPartial: want a refusal, got %v", err)
			}
			if err == nil && report.Entries != 0 {
				t.Fatalf("partial recovery emitted %d forged entries", report.Entries)
			}
			if err == nil && w.n > 1024 {
				t.Fatalf("partial recovery wrote %d bytes of forged content", w.n)
			}
		})
	}

	t.Run("Verify", func(t *testing.T) {
		res, err := b.Verify(ctx, true, false)
		if !dataForged && !indexForged {
			if err != nil || !res.OK {
				t.Fatalf("an untouched backup must verify: %v %+v", err, res)
			}
			return
		}
		if err == nil && res.OK {
			t.Fatalf("verify reported a healthy backup on forged blobs: %+v", res)
		}
		if err != nil && !isRefusal(err) {
			t.Fatalf("Verify: want a refusal, got %v", err)
		}
	})
}

func requireRefusalWithoutOutput(t *testing.T, err error, written int) {
	t.Helper()
	if err == nil {
		t.Fatal("a forged blob must not be served")
	}
	if !isRefusal(err) {
		t.Fatalf("want an integrity or format failure, got %v", err)
	}
	if written != 0 {
		t.Fatalf("%d bytes of unverified plaintext reached the consumer before the refusal", written)
	}
}
