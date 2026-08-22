//go:build unix

package archive

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manprint/backimage/test/fixtures"
)

func writeTree(t *testing.T, dir string, feats fixtures.Feature) *fixtures.Manifest {
	t.Helper()
	return fixtures.Build(t, dir, feats)
}

func TestWriterRoundTripInMemory(t *testing.T) {
	src := t.TempDir()
	feats := fixtures.FeatBasic | fixtures.FeatPerms | fixtures.FeatSymlinks |
		fixtures.FeatHardlinks | fixtures.FeatXattrs | fixtures.FeatNames | fixtures.FeatTimes
	BuildLocal(t, src, feats)

	var buf bytes.Buffer
	ctx := context.Background()
	w := NewWriter(&buf, Options{})
	if err := w.AddRoot(ctx, src); err != nil {
		t.Fatal(err)
	}
	stats, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files == 0 || stats.Dirs == 0 {
		t.Fatalf("stats: %+v", stats)
	}

	r := NewReader(&buf)
	compareReaderToTree(t, src, r, stats)
}

func TestWriterDeterministic(t *testing.T) {
	src := t.TempDir()
	feats := fixtures.FeatBasic | fixtures.FeatPerms | fixtures.FeatSymlinks |
		fixtures.FeatHardlinks | fixtures.FeatXattrs | fixtures.FeatNames | fixtures.FeatTimes
	writeTree(t, src, feats)

	// Pin atime/ctime so the byte comparison is immune to readdir-induced atime drift.
	pin := func() {
		filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			mt := fi.ModTime()
			return os.Chtimes(p, mt, mt)
		})
	}
	pin()
	var b1, b2 bytes.Buffer
	opts := Options{}
	w1 := NewWriter(&b1, opts)
	if err := w1.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w1.Close()
	pin()
	w2 := NewWriter(&b2, opts)
	if err := w2.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w2.Close()
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Fatalf("two archives of the same tree differ byte-wise (dedup phase impossible):\ngot1 %d bytes\ngot2 %d bytes", b1.Len(), b2.Len())
	}
}

func TestWriterHardlinkSinglePayload(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, fixtures.FeatBasic|fixtures.FeatHardlinks)
	if len(mHardlinkGroup(t, src)) != 3 {
		t.Fatal("fixture must build 3 hard-link paths")
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var bodies int
	for _, e := range w.Entries() {
		if e.Type == TypeRegular && strings.Contains(e.Path, "/hard/") {
			bodies++
		}
	}
	if bodies != 1 {
		t.Fatalf("expected exactly 1 data payload in hard/, got %d; entries: %v", bodies, entryTypes(w.Entries()))
	}
}

func TestWriterRootCollision(t *testing.T) {
	base := t.TempDir()
	r1 := base + "/a/same"
	r2 := base + "/b/same"
	os.MkdirAll(r1, 0o755)
	os.MkdirAll(r2, 0o755)
	w := NewWriter(io.Discard, Options{})
	if err := w.AddRoot(context.Background(), r1); err != nil {
		t.Fatal(err)
	}
	err := w.AddRoot(context.Background(), r2)
	if err == nil {
		t.Fatal("expected collision error")
	}
	var rc *RootCollisionError
	if !isRootCollision(err, &rc) {
		t.Fatalf("want RootCollisionError, got %T", err)
	}
	if rc.Hint() == "" {
		t.Fatalf("collision error carries no hint: %v", err)
	}
	if !strings.Contains(rc.Error(), "share the basename") {
		t.Fatalf("unexpected RootCollisionError text: %q", rc.Error())
	}
}

func TestWriterExcludes(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, fixtures.FeatBasic)
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Excludes: []string{"001/basic/*.txt", "001/empty-dir"}})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w.Close()
	for _, e := range w.Entries() {
		if strings.HasSuffix(e.Path, ".txt") || e.Path == "001/empty-dir" {
			t.Errorf("excluded entry emitted: %s", e.Path)
		}
	}
}

func TestWriterTarTvGated(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	src := t.TempDir()
	writeTree(t, src, fixtures.FeatBasic|fixtures.FeatPerms|fixtures.FeatSymlinks|
		fixtures.FeatHardlinks|fixtures.FeatNames)
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w.Close()
	f, err := os.CreateTemp(t.TempDir(), "out.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(buf.Bytes())
	f.Close()
	out, err := exec.Command("tar", "--xattrs", "--acls", "-tvf", f.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("GNU tar rejects our archive: %v\n%s", err, out)
	}
}

func TestWriterSocketsSkipped(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, fixtures.FeatBasic)
	sock := src + "/sock"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot create unix socket: %v", err)
	}
	defer l.Close()
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{})
	// socket was created after Build; AddRoot walks and must skip it
	_ = os.Remove // silence unused if socket skipped earlier
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w.Close()
	stats, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 1 {
		t.Fatalf("expected socket skipped, stats=%+v", stats)
	}
}

func TestWriterNonStrictCollectsErrors(t *testing.T) {
	src := t.TempDir()
	leaky := filepath.Join(src, "leaky")
	os.MkdirAll(leaky, 0o000)
	defer os.Chmod(leaky, 0o755)
	os.WriteFile(filepath.Join(leaky, "f"), []byte("x"), 0o644)
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatalf("non-strict walk must not fail: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	stats := w.Entries()
	found := false
	for _, e := range stats {
		if strings.HasSuffix(e.Path, "/leaky") {
			found = true
		}
	}
	if !found {
		t.Fatal("unreadable dir must still be archived as dir entry")
	}
}

func TestWriterStrictSkipsUnreadableDirContents(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	src := t.TempDir()
	locked := filepath.Join(src, "locked")
	os.MkdirAll(locked, 0o755)
	os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o644)
	os.Chmod(locked, 0o000)
	defer os.Chmod(locked, 0o755)
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Strict: true})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatalf("unreadable dir must not abort even in strict mode: %v", err)
	}
	w.Close()
	for _, e := range w.Entries() {
		if e.Path == "locked/f" {
			t.Fatal("children of unreadable dir must not be archived")
		}
	}
}

func TestWriterRootLstatError(t *testing.T) {
	w := NewWriter(io.Discard, Options{})
	if err := w.AddRoot(context.Background(), "/definitely/not/here"); err == nil {
		t.Fatal("missing root must error")
	}
}

func TestWriterSkippedSocketAndExcluded(t *testing.T) {
	src := t.TempDir()
	BuildLocal(t, src, fixtures.FeatBasic)
	os.Symlink("gone", filepath.Join(src, "broken-link"))
	os.WriteFile(filepath.Join(src, "skip.me"), []byte("y"), 0o644)
	os.MkdirAll(filepath.Join(src, "skipme-dir"), 0o755)
	os.WriteFile(filepath.Join(src, "skipme-dir", "f"), []byte("z"), 0o644)

	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Excludes: []string{"**/skip.me", "**/skipme-dir/**"}})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w.Close()
	// Archived names are rooted at the source basename, which t.TempDir() makes
	// "001". Asserting on the bare names would pass without excluding anything.
	base := filepath.Base(src)
	for _, e := range w.Entries() {
		switch e.Path {
		case base + "/skip.me", base + "/skipme-dir", base + "/skipme-dir/f":
			t.Fatalf("%q must be excluded, entries: %v", e.Path, entryTypes(w.Entries()))
		}
	}
	if len(w.Entries()) == 0 {
		t.Fatal("the writer emitted nothing: the assertions above would be vacuous")
	}
}

func TestWriterCancelledContext(t *testing.T) {
	src := t.TempDir()
	BuildLocal(t, src, fixtures.FeatBasic)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := NewWriter(io.Discard, Options{})
	if err := w.AddRoot(ctx, src); err == nil {
		t.Fatal("cancelled context must abort walk")
	}
}

// An exclusion must remove the whole subtree it names. It did not: the filter
// went through filepath.Match, where "**" is a single-segment wildcard, so
// "alice/.cache/**" dropped alice/.cache/cookies.db but archived
// alice/.cache/chromium/Default/Cookies — data the operator asked to leave out.
func TestWriterExcludesWholeSubtree(t *testing.T) {
	src := t.TempDir()
	base := filepath.Base(src)
	deep := filepath.Join(src, ".cache", "chromium", "Default")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(src, ".cache", "cookies.db"): "shallow",
		filepath.Join(deep, "Cookies"):             "deep",
		filepath.Join(src, "docs", "cv.pdf"):       "keep",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Excludes: []string{base + "/.cache/**"}})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w.Close()

	kept := make([]string, 0, len(w.Entries()))
	for _, e := range w.Entries() {
		kept = append(kept, e.Path)
		if strings.Contains(e.Path, "/.cache") {
			t.Errorf("excluded subtree leaked into the archive: %q", e.Path)
		}
	}
	// The exclusion must not have taken the rest of the tree with it.
	var sawPDF bool
	for _, p := range kept {
		if p == base+"/docs/cv.pdf" {
			sawPDF = true
		}
	}
	if !sawPDF {
		t.Fatalf("the exclusion removed unrelated paths, entries: %v", kept)
	}
}

// A pattern naming the directory itself, without the trailing "/**", must also
// take everything under it: "alice/.cache" is the spelling an operator reaches
// for first.
func TestWriterExcludesBareDirectoryName(t *testing.T) {
	src := t.TempDir()
	base := filepath.Base(src)
	if err := os.MkdirAll(filepath.Join(src, "cache", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "cache", "sub", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{Excludes: []string{base + "/cache"}})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w.Close()
	for _, e := range w.Entries() {
		if strings.Contains(e.Path, "cache") {
			t.Errorf("entry under an excluded directory was archived: %q", e.Path)
		}
	}
}
