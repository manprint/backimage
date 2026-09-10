package recovery

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
)

// formatFixturePassphrase is the passphrase scripts/make-format-fixtures.sh sealed
// the encrypted fixtures with.
const formatFixturePassphrase = "fixture-passphrase"

// The fixtures under testdata/ are complete backups, one per on-disk format
// this project has released. They exist because the writer is about to
// change: once it does, no build can produce the old bytes any more, and a
// compatibility test that has to generate its own input is a test that
// quietly stops covering anything.
//
// Regenerating a fixture is not a fix. If one of these tests fails, the
// reader lost the ability to read a format that is out in the world.
var formatFixtures = []struct {
	dir       string
	encrypted bool
	// schema is the metadata schema recorded in manifest.json: 1 keeps the
	// confidential fields in the clear, 2 moves them into the sealed blob.
	schema int
	// envelope is the crypt envelope version of the sealed blobs. Zero means
	// the manifest does not carry the field at all, which is what releases up
	// to 0.2.3 wrote — those blobs are version 1 in their own header.
	envelope int
}{
	{dir: "schema1-plain", encrypted: false, schema: 1, envelope: 0},
	// Envelope 2 is what 0.2.4 through 0.4.0 wrote. It is deliberately not
	// crypt.EnvelopeVersion: the day this project writes a new envelope, this
	// fixture must keep saying 2 and keep opening.
	{dir: "schema2-encrypted", encrypted: true, schema: 2, envelope: 2},
	{dir: "legacy-envelope1", encrypted: true, schema: 2, envelope: 0},
	// The format this build writes. It is frozen with the others so the next
	// change to the writer has to keep opening it too, and so the binding of
	// A6.3 is exercised from a file rather than from something the test just
	// built.
	{dir: "schema2-envelope3", encrypted: true, schema: 2, envelope: 3},
}

// fixtureContent is what every fixture holds, byte for byte. The three were
// produced from the same source tree by three different builds, so any
// difference between them is a difference of format, not of content.
var fixtureContent = map[string]string{
	"src/hello.txt":      "fixture payload\n",
	"src/sub/nested.txt": "nested\n",
}

var fixtureLayout = []struct {
	path string
	typ  string
}{
	{"src", index.TypeDir},
	{"src/hello.txt", index.TypeRegular},
	{"src/link", index.TypeSymlink},
	{"src/sub", index.TypeDir},
	{"src/sub/nested.txt", index.TypeRegular},
}

func openFixture(t *testing.T, dir string, encrypted bool) *Backup {
	t.Helper()
	b, err := OpenLocal(context.Background(), filepath.Join("testdata", dir))
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if encrypted {
		if err := b.Unlock(context.Background(), crypt.Identity{Passphrase: []byte(formatFixturePassphrase)}); err != nil {
			t.Fatalf("unlock %s: %v", dir, err)
		}
	}
	return b
}

func TestReleasedFormatsStillOpen(t *testing.T) {
	for _, f := range formatFixtures {
		t.Run(f.dir, func(t *testing.T) {
			b := openFixture(t, f.dir, f.encrypted)
			if got := b.Manifest.SchemaVersion; got != f.schema {
				t.Errorf("schema = %d, want %d", got, f.schema)
			}
			if got := b.Manifest.Encryption.Enabled; got != f.encrypted {
				t.Errorf("encrypted = %v, want %v", got, f.encrypted)
			}
			if got := b.Manifest.Encryption.EnvelopeVersion; got != f.envelope {
				t.Errorf("envelopeVersion = %d, want %d", got, f.envelope)
			}
			if len(b.Chunks.Chunks) == 0 {
				t.Fatal("no chunks")
			}
		})
	}
}

func TestReleasedFormatsStillListTheirEntries(t *testing.T) {
	for _, f := range formatFixtures {
		t.Run(f.dir, func(t *testing.T) {
			b := openFixture(t, f.dir, f.encrypted)
			idx, err := b.Index(context.Background())
			if err != nil {
				t.Fatalf("index: %v", err)
			}
			byPath := make(map[string]index.FileEntry, len(idx.Entries))
			for _, e := range idx.Entries {
				byPath[e.Path] = e
			}
			if len(byPath) != len(fixtureLayout) {
				t.Fatalf("index holds %d entries, want %d", len(byPath), len(fixtureLayout))
			}
			for _, want := range fixtureLayout {
				got, ok := byPath[want.path]
				if !ok {
					t.Errorf("entry %q missing", want.path)
					continue
				}
				if got.Type != want.typ {
					t.Errorf("entry %q type = %q, want %q", want.path, got.Type, want.typ)
				}
			}
		})
	}
}

func TestReleasedFormatsStillRestoreTheirContent(t *testing.T) {
	for _, f := range formatFixtures {
		t.Run(f.dir, func(t *testing.T) {
			b := openFixture(t, f.dir, f.encrypted)
			var buf bytes.Buffer
			if err := b.StreamTar(context.Background(), &buf, true); err != nil {
				t.Fatalf("stream tar: %v", err)
			}
			seen := map[string]string{}
			tr := tar.NewReader(&buf)
			for {
				hdr, err := tr.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("tar: %v", err)
				}
				if hdr.Typeflag != tar.TypeReg {
					continue
				}
				body, err := io.ReadAll(tr)
				if err != nil {
					t.Fatalf("tar body %q: %v", hdr.Name, err)
				}
				seen[hdr.Name] = string(body)
			}
			if len(seen) != len(fixtureContent) {
				t.Fatalf("tar holds %d regular files, want %d: %v", len(seen), len(fixtureContent), seen)
			}
			for path, want := range fixtureContent {
				if got := seen[path]; got != want {
					t.Errorf("%q = %q, want %q", path, got, want)
				}
			}
		})
	}
}

// Verify is the path that checks every stored and plaintext digest. A format
// that opens but cannot be verified is not readable in any sense that matters.
func TestReleasedFormatsStillVerify(t *testing.T) {
	for _, f := range formatFixtures {
		t.Run(f.dir, func(t *testing.T) {
			b := openFixture(t, f.dir, f.encrypted)
			report, err := b.Verify(context.Background(), true, false)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if !report.OK || len(report.Errors) != 0 {
				t.Fatalf("verify report = %+v", report)
			}
			if report.Chunks != len(b.Chunks.Chunks) {
				t.Errorf("verified %d chunks of %d", report.Chunks, len(b.Chunks.Chunks))
			}
			if !report.Full {
				t.Error("the full verification must have run")
			}
		})
	}
}

// A sealed blob from one backup must not open inside another. The fixtures
// were produced by three builds with three key materials, so swapping their
// data blobs is the cheapest standing check that the envelope authenticates
// what it claims to — and it stays meaningful across the format change this
// phase precedes.
func TestASealedBlobDoesNotTravelBetweenBackups(t *testing.T) {
	donor := openFixture(t, "schema2-encrypted", true)
	stolen, err := donor.StoredChunk(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	root := copyFixture(t, "legacy-envelope1")
	victim, err := OpenLocal(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer victim.Close()
	if err := victim.Unlock(context.Background(), crypt.Identity{Passphrase: []byte(formatFixturePassphrase)}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, strings.TrimPrefix(victim.Chunks.Chunks[0].P, "backup/"))
	if err := os.WriteFile(target, stolen, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := victim.PlainChunk(context.Background(), 0); err == nil {
		t.Fatal("a blob sealed by another backup was accepted")
	}
}

// copyFixture gives a test a writable copy of a frozen fixture: the ones
// under testdata are evidence and are never modified in place.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
	return dst
}
