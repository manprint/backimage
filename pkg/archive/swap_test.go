//go:build unix

package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests below put the walk in the state it is in between two of its own
// steps — an lstat taken, a directory listed — then change the tree the way a
// user who owns part of it can, and let the walk continue. Nothing in them is
// timing dependent: the swap happens exactly inside the window a real race
// would have to hit.

// secretOutside writes a file outside every tree under test and returns its
// path. Its bytes must never reach an archive.
func secretOutside(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("ROOT-ONLY-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// payloads returns every regular payload of the archive in buf, by name.
func payloads(t *testing.T, buf *bytes.Buffer) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(b)
	}
}

func assertNoSecret(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	for name, body := range payloads(t, buf) {
		if strings.Contains(body, "ROOT-ONLY-SECRET") {
			t.Fatalf("%s carries the bytes of a file outside the backup root", name)
		}
	}
}

// TestADirectorySwappedForASymlinkAfterListingCannotRedirectItsChildren: the
// walk lists D, then D's owner renames it away and puts a symlink to another
// directory in its place. The names already listed must still resolve in the
// directory that was listed. Resolving them by path again, as the walk used
// to, read the symlink's target: /etc/shadow archived as D/shadow.
func TestADirectorySwappedForASymlinkAfterListingCannotRedirectItsChildren(t *testing.T) {
	secret := secretOutside(t, "shadow")
	root := filepath.Join(t.TempDir(), "root")
	d := filepath.Join(root, "D")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "shadow"), []byte("decoy"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(d)
	if err != nil {
		t.Fatal(err)
	}
	l, err := source{path: d}.list(st)
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()

	if err := os.Rename(d, d+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(secret), d); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	w := newWriter(&buf, Options{Strict: true})
	if err := w.walkChildren(context.Background(), "root/D", root, d, l); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, &buf)
	if got := payloads(t, &buf)["root/D/shadow"]; got != "decoy" {
		t.Fatalf("root/D/shadow = %q, want the content of the directory that was listed", got)
	}
}

// TestASubdirectorySwappedAfterItsLstatIsNotDescended: the swap lands between
// the lstat that says "directory" and the open that lists it. The symlink is
// followed by nobody: the directory is refused as replaced.
func TestASubdirectorySwappedAfterItsLstatIsNotDescended(t *testing.T) {
	secret := secretOutside(t, "key")
	root := filepath.Join(t.TempDir(), "root")
	d := filepath.Join(root, "D")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	src := source{dir: parent, name: "D", path: d}
	st, err := src.lstat()
	if err != nil {
		t.Fatal(err)
	}
	// Another directory inside the root, so os.Root itself follows the
	// symlink to it.
	if err := os.Mkdir(filepath.Join(root, "E"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(d, d+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("E", d); err != nil {
		t.Fatal(err)
	}
	if _, err := src.list(st); !errors.Is(err, errSourceChanged) {
		t.Fatalf("list of a directory swapped for an in-root symlink = %v, want errSourceChanged", err)
	}
	if err := os.Remove(d); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(secret), d); err != nil {
		t.Fatal(err)
	}
	if l, err := src.list(st); err == nil {
		l.close()
		t.Fatal("list of a directory swapped for a symlink out of the root succeeded")
	}
}

// swapAfterLstat builds root/f, takes its lstat, then lets swap replace it.
// It returns what the walk would hold at that moment.
//
// swap builds the replacement at a side path and renames it over f, as an
// attacker would: removing f first lets the filesystem hand its inode number
// to whatever is created next, and an object with the same inode number is
// indistinguishable — but then its content is the attacker's own, not a
// foreign file's.
func swapAfterLstat(t *testing.T, swap func(root, p string)) (source, os.FileInfo, func()) {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, "f")
	if err := os.WriteFile(p, []byte("sixteen bytes..."), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	src := source{dir: dir, name: "f", path: p}
	st, err := src.lstat()
	if err != nil {
		t.Fatal(err)
	}
	swap(root, p)
	return src, st, func() { _ = dir.Close() }
}

// TestARegularFileSwappedAfterLstatIsNeverRead covers every replacement a user
// owning the directory can make in the window between lstat and open. None of
// them may put foreign bytes in the archive or block the walk.
func TestARegularFileSwappedAfterLstatIsNeverRead(t *testing.T) {
	renameOver := func(t *testing.T, side, p string) {
		t.Helper()
		if err := os.Rename(side, p); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, root, p string){
		"symlink out of the root": func(t *testing.T, _, p string) {
			if err := os.Symlink(secretOutside(t, "secret"), p+".new"); err != nil {
				t.Fatal(err)
			}
			renameOver(t, p+".new", p)
		},
		// os.Root follows a symlink that stays inside the root, O_NOFOLLOW
		// or not: only the identity check refuses it.
		"symlink inside the root": func(t *testing.T, root, p string) {
			other := filepath.Join(root, "other")
			if err := os.WriteFile(other, []byte("ROOT-ONLY-SECRET"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("other", p+".new"); err != nil {
				t.Fatal(err)
			}
			renameOver(t, p+".new", p)
		},
		"file renamed over it": func(t *testing.T, _, p string) {
			if err := os.WriteFile(p+".new", []byte("ROOT-ONLY-SECRET"), 0o644); err != nil {
				t.Fatal(err)
			}
			renameOver(t, p+".new", p)
		},
		"fifo": func(t *testing.T, _, p string) {
			if err := syscall.Mkfifo(p+".new", 0o644); err != nil {
				t.Fatal(err)
			}
			renameOver(t, p+".new", p)
		},
	}
	for name, swap := range cases {
		for _, strict := range []bool{true, false} {
			mode := "degraded"
			if strict {
				mode = "strict"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				src, st, done := swapAfterLstat(t, func(root, p string) { swap(t, root, p) })
				defer done()
				var buf bytes.Buffer
				w := newWriter(&buf, Options{Strict: strict})
				result := make(chan error, 1)
				go func() { result <- w.emitOne(context.Background(), "f", src, st, nil) }()
				var err error
				select {
				case err = <-result:
				case <-time.After(5 * time.Second):
					// Unblock a reader stuck on the FIFO before failing.
					if f, openErr := os.OpenFile(src.path, os.O_WRONLY|syscall.O_NONBLOCK, 0); openErr == nil {
						_ = f.Close()
					}
					t.Fatal("the walk blocked on the replaced entry")
				}
				if strict {
					if err == nil {
						t.Fatal("strict mode archived a replaced file")
					}
					return
				}
				if err != nil {
					t.Fatalf("degraded mode: %v", err)
				}
				stats, err := w.Close()
				if err != nil {
					t.Fatal(err)
				}
				assertNoSecret(t, &buf)
				if stats.ContentSkipped != 1 {
					t.Fatalf("ContentSkipped = %d, want 1: the payload of the replaced file is missing", stats.ContentSkipped)
				}
			})
		}
	}
}

// TestAReplacedRootFileIsNeverRead: a root given as a file is opened by its
// path, and still checked against its lstat.
func TestAReplacedRootFileIsNeverRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := source{name: "f", path: root}
	st, err := src.lstat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretOutside(t, "secret"), root); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := newWriter(&buf, Options{Strict: true})
	if err := w.emitOne(context.Background(), "f", src, st, nil); err == nil {
		t.Fatal("a root file swapped for a symlink was archived")
	}
}

// TestExcludedEntriesAreNeverOpened: an exclude keeps the backup away from a
// path, including from opening it. A FIFO nobody writes to would block an
// open without O_NONBLOCK; an excluded one must not even be looked at, and its
// unreadable neighbour's metadata error must not fail a strict backup.
func TestExcludedEntriesAreNeverOpened(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file regardless of its mode")
	}
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "locked")
	if err := os.WriteFile(locked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if err := os.WriteFile(filepath.Join(root, "kept"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, stats := archivePaths(t, root, Options{Strict: true, Excludes: []string{"tree/locked"}})
	if strings.Join(paths, ",") != "tree,tree/kept" {
		t.Fatalf("paths = %v", paths)
	}
	if stats.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", stats.Skipped)
	}
}

// TestADeepTreeHoldsOneDescriptorPerLevel: the walk keeps the handle of every
// directory above the entry it is archiving, and nothing else. A tree deeper
// than the descriptors a leak of two per level would allow still archives.
func TestADeepTreeHoldsOneDescriptorPerLevel(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Skip(err)
	}
	const depth = 200
	restore := lim
	lim.Cur = depth + 150 // one per level plus the test binary's own
	if lim.Cur > lim.Max {
		t.Skip("hard descriptor limit too low for the test")
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &restore) })

	root := filepath.Join(t.TempDir(), "deep")
	p := root
	for i := 0; i < depth; i++ {
		p = filepath.Join(p, "d")
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "leaf"), []byte("leaf"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, _ := archivePaths(t, root, Options{Strict: true})
	if len(paths) != depth+2 {
		t.Fatalf("archived %d entries, want %d", len(paths), depth+2)
	}
}
