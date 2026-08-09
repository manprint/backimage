//go:build unix

package archive

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/fpierri/backimage/test/fixtures"
)

// BuildLocal builds a fixture tree under dir (the fixture package requires a
// testing.T, which is only legal in _test files).
func BuildLocal(t *testing.T, dir string, feats fixtures.Feature) *fixtures.Manifest {
	t.Helper()
	m := fixtures.Build(t, dir, feats)
	return m
}

func mHardlinkGroup(t *testing.T, dir string) []string {
	t.Helper()
	paths := []string{"hard/orig.txt", "hard/link2.txt", "hard/link3.txt"}
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(dir, p)); err != nil {
			t.Fatalf("fixture missing %s: %v", p, err)
		}
	}
	return paths
}

func entryTypes(entries []Entry) map[EntryType]int {
	out := map[EntryType]int{}
	for _, e := range entries {
		out[e.Type]++
	}
	return out
}

func isRootCollision(err error, target **RootCollisionError) bool {
	return errors.As(err, target)
}

// listenOnUnix binds a unix-domain socket (used to test the "skipped" path).
var _ = listenOnUnix // referenced from network-triggered test files

func listenOnUnix(path string) error {
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	return l.Close()
}

// compareReaderToTree drains the reader and verifies every entry matches what
// is on disk in src (path existence, content, type, size).
func compareReaderToTree(t *testing.T, src string, r Reader, stats Stats) {
	t.Helper()
	seen := map[string]bool{}
	for {
		e, body, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		fi, err := os.Lstat(filepath.Join(filepath.Dir(src), filepath.FromSlash(e.Path)))
		if err != nil {
			t.Fatalf("entry %q not on disk: %v", e.Path, err)
		}
		if e.Type == TypeRegular {
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("read body %q: %v", e.Path, err)
			}
			want, _ := os.ReadFile(filepath.Join(filepath.Dir(src), filepath.FromSlash(e.Path)))
			if !bytes.Equal(got, want) {
				t.Errorf("content mismatch %q", e.Path)
			}
			if e.Size != int64(len(got)) {
				t.Errorf("size %q: entry %d body %d", e.Path, e.Size, len(got))
			}
		} else if fi.Mode().IsRegular() && e.Type != TypeHardlink && e.Type != TypeDir {
			t.Errorf("type mismatch %q: disk regular, entry %v", e.Path, e.Type)
		}
		seen[e.Path] = true
	}
}
