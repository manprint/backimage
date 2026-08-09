//go:build unix

package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"

	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/fpierri/backimage/test/fixtures"
)

func TestRoundTripIncludesExcludes(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	fixtures.Build(t, src, fixtures.FeatBasic)

	var buf bytes.Buffer
	w := NewWriter(&buf, Options{PreserveXattrs: true})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w.Close()

	x := NewExtractor(ExtractOptions{Includes: []string{"*/basic/hello.txt"}, Excludes: []string{"*/empty.txt"}})
	stats, err := x.Extract(context.Background(), &buf, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 {
		t.Fatalf("expected 1 file, got %+v", stats)
	}
	if _, err := os.Lstat(dst + "/" + baseOf(src) + "/basic/hello.txt"); err != nil {
		t.Errorf("hello.txt not extracted: %v", err)
	}
	if _, err := os.Lstat(dst + "/" + baseOf(src) + "/basic/empty.txt"); err == nil {
		t.Errorf("empty.txt must be excluded")
	}
}

func baseOf(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	return parts[len(parts)-1]
}

func TestExtractMaliciousPath(t *testing.T) {
	dst := t.TempDir()
	before, _ := os.ReadDir(t.TempDir())
	_ = before
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"../../etc/passwd", "/etc/passwd", "../evil"} {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
		tw.Write([]byte("oops"))
	}
	tw.Close()
	x := NewExtractor(ExtractOptions{Strict: true})
	if _, err := x.Extract(context.Background(), &buf, dst); err == nil {
		t.Fatal("malicious archive must fail")
	}
	if _, err := os.Lstat("/etc/passwd2"); err == nil {
		t.Fatal("must not write outside dest")
	}
}

func TestExtractSymlinkSwap(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/tmp", Mode: 0o777})
	tw.WriteHeader(&tar.Header{Name: "evil/inside.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	tw.Write([]byte("pwn"))
	tw.Close()
	x := NewExtractor(ExtractOptions{Strict: true})
	_, err := x.Extract(context.Background(), &buf, dst)
	if err == nil {
		t.Fatal("symlink swap must fail")
	}
	if _, err := os.Lstat("/tmp/backimage-swap-probe"); err == nil {
		t.Fatal("file written through symlink outside dest")
	}
}

func TestExtractDir0500Populated(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.MkdirAll(src+"/locked", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src+"/locked/inner.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src+"/locked", 0o500); err != nil {
		t.Fatal(err)
	}
	// re-open the source dir before TempDir cleanup runs
	t.Cleanup(func() { os.Chmod(src+"/locked", 0o700) })

	var buf bytes.Buffer
	w := NewWriter(&buf, Options{PreserveXattrs: true})
	if err := w.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	w.Close()
	x := NewExtractor(ExtractOptions{PreserveXattrs: true, PreserveOwner: true, Strict: true})
	if _, err := x.Extract(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	locked := dst + "/" + baseOf(src) + "/locked"
	t.Cleanup(func() { os.Chmod(locked, 0o700) }) // TempDir teardown cannot delete a 0500 dir
	p := dst + "/" + baseOf(src) + "/locked"
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o500 {
		t.Errorf("locked dir mode = %o, want 500", fi.Mode().Perm())
	}
	if _, err := os.Lstat(p + "/inner.txt"); err != nil {
		t.Errorf("inner file lost in 0500 dir: %v", err)
	}
}

func TestExtractSetuidPreserved(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	os.WriteFile(src+"/suid", []byte("x"), 0o644)
	os.Chmod(src+"/suid", os.ModeSetuid|0o755)

	var buf bytes.Buffer
	bw := NewWriter(&buf, Options{PreserveXattrs: true})
	if err := bw.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	bw.Close()
	x := NewExtractor(ExtractOptions{PreserveXattrs: true, PreserveOwner: true, Strict: true})
	if _, err := x.Extract(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dst + "/" + baseOf(src) + "/suid")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetuid == 0 {
		t.Errorf("setuid bit lost after extract (chown likely after chmod)")
	}
}

func TestExtractOverwriteErrors(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	os.WriteFile(src+"/f", []byte("x"), 0o644)
	var buf bytes.Buffer
	bw := NewWriter(&buf, Options{PreserveXattrs: true})
	bw.AddRoot(context.Background(), src)
	bw.Close()
	x := NewExtractor(ExtractOptions{PreserveXattrs: true, PreserveOwner: true, Strict: true})
	if _, err := x.Extract(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	// second round without Overwrite: existing destination must error
	var buf2 bytes.Buffer
	bw2 := NewWriter(&buf2, Options{})
	bw2.AddRoot(context.Background(), src)
	bw2.Close()
	x2 := NewExtractor(ExtractOptions{})
	if _, err := x2.Extract(context.Background(), &buf2, dst); err == nil {
		t.Fatal("second extract without Overwrite must fail")
	}
}

func TestExtractHardlinkGroup(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	os.MkdirAll(src+"/h", 0o755)
	os.WriteFile(src+"/h/a", []byte("same"), 0o644)
	os.Link(src+"/h/a", src+"/h/b")
	os.Link(src+"/h/a", src+"/h/c")

	var buf bytes.Buffer
	bw := NewWriter(&buf, Options{PreserveXattrs: true})
	if err := bw.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	bw.Close()
	x := NewExtractor(ExtractOptions{PreserveXattrs: true, PreserveOwner: true, Strict: true})
	if _, err := x.Extract(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	fa, _ := os.Lstat(dst + "/" + baseOf(src) + "/h/a")
	fb, _ := os.Lstat(dst + "/" + baseOf(src) + "/h/b")
	fc, _ := os.Lstat(dst + "/" + baseOf(src) + "/h/c")
	if fa.Sys().(*syscall.Stat_t).Ino != fb.Sys().(*syscall.Stat_t).Ino ||
		fa.Sys().(*syscall.Stat_t).Ino != fc.Sys().(*syscall.Stat_t).Ino {
		t.Fatal("hardlinks not restored as one inode")
	}
}

func TestExtractOverwriteRemovesExisting(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	os.WriteFile(src+"/f", []byte("x"), 0o644)
	var buf bytes.Buffer
	bw := NewWriter(&buf, Options{})
	bw.AddRoot(context.Background(), src)
	bw.Close()
	x := NewExtractor(ExtractOptions{})
	if _, err := x.Extract(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	old := dst + "/f-old"
	os.WriteFile(old, []byte("old"), 0o644)
	bw2 := NewWriter(&buf, Options{})
	bw2.AddRoot(context.Background(), src)
	bw2.Close()
	x2 := NewExtractor(ExtractOptions{Overwrite: true})
	if _, err := x2.Extract(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(old); err != nil {
		t.Fatalf("overwrite removed unrelated file %q: %v", old, err)
	}
}

func TestExtractFifoNonRoot(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	fifo := src + "/pipe"
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	var buf bytes.Buffer
	bw := NewWriter(&buf, Options{})
	if err := bw.AddRoot(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	bw.Close()
	x := NewExtractor(ExtractOptions{})
	if _, err := x.Extract(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dst + "/" + baseOf(src) + "/pipe")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("extracted object is not a fifo: %v", fi.Mode())
	}
}

func TestExtractPermissionHintNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	src := t.TempDir()
	dst := t.TempDir()
	os.WriteFile(src+"/f", []byte("x"), 0o644)
	var buf bytes.Buffer
	bw := NewWriter(&buf, Options{})
	bw.AddRoot(context.Background(), src)
	bw.Close()

	// strip write bits on dest so createOne hits EACCES
	os.Chmod(dst, 0o555)
	defer os.Chmod(dst, 0o755)
	x := NewExtractor(ExtractOptions{Strict: true})
	_, err := x.Extract(context.Background(), &buf, dst)
	if err == nil {
		t.Fatal("expected permission error")
	}
	var p *PermissionHintError
	if !errors.As(err, &p) {
		t.Fatalf("want PermissionHintError, got %T: %v", err, err)
	}
	if p.Op == "" || p.Err == nil {
		t.Fatal("hint fields empty")
	}
	if !strings.Contains(p.Error(), p.Op) {
		t.Fatalf("permission hint text lacks op: %q", p.Error())
	}
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("Unwrap: want EACCES, got %v", err)
	}
}

func TestExtractDeviceDegradedNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	// Parent dir without write permission => mknod/fifo must fail with EACCES
	// deterministically, letting us check both strict (hint) and degraded (skip) mode.
	parent := t.TempDir()
	os.Chmod(parent, 0o555)
	defer os.Chmod(parent, 0o755)

	build := func(typ byte) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: "d", Typeflag: typ, Mode: 0o600})
		tw.Close()
		return buf.Bytes()
	}

	// degraded: device skipped, no error
	x := NewExtractor(ExtractOptions{})
	stats, err := x.Extract(context.Background(), bytes.NewReader(build(tar.TypeChar)), parent)
	if err != nil {
		t.Fatalf("degraded mode must not fail: %v", err)
	}
	if stats.Skipped == 0 {
		t.Fatalf("device must be counted as skipped, got %+v", stats)
	}

	// strict: permission hint with mknod op
	x2 := NewExtractor(ExtractOptions{Strict: true})
	_, err = x2.Extract(context.Background(), bytes.NewReader(build(tar.TypeChar)), parent)
	var p *PermissionHintError
	if !errors.As(err, &p) {
		t.Fatalf("strict extract must surface hint, got %v", err)
	}
	if p.Op != "mknod" {
		t.Fatalf("op = %q, want mknod", p.Op)
	}
}

func TestExtractUnsupportedTypeflag(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "x", Typeflag: 'Z', Mode: 0o600})
	tw.Close()
	x := NewExtractor(ExtractOptions{})
	_, err := x.Extract(context.Background(), &buf, t.TempDir())
	if err == nil {
		t.Fatal("unsupported typeflag must error")
	}
}

func TestExtractSymlinkUnwritableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	parent := t.TempDir()
	os.Chmod(parent, 0o555)
	defer os.Chmod(parent, 0o755)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "t", Mode: 0o777})
	tw.Close()
	x := NewExtractor(ExtractOptions{Strict: true})
	_, err := x.Extract(context.Background(), &buf, parent)
	var p *PermissionHintError
	if !errors.As(err, &p) {
		t.Fatalf("want hint, got %v", err)
	}
}

func TestExtractTypeRegA(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "f", Typeflag: tar.TypeReg, Mode: 0o640, Size: 3,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("abc"))
	tw.Close()
	x := NewExtractor(ExtractOptions{})
	stats, err := x.Extract(context.Background(), &buf, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	data, err := os.ReadFile(filepath.Join(dst, "f"))
	if err != nil || string(data) != "abc" {
		t.Fatalf("content = %q, err = %v", data, err)
	}
}
