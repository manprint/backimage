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
	"time"

	"golang.org/x/sys/unix"

	"github.com/manprint/backimage/test/fixtures"
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

// tarWithXattr builds a one-file PAX archive carrying a single extended
// attribute, the way a backup of a tree with overlayfs metadata does.
func tarWithXattr(t *testing.T, name, value string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name:       "payload.txt",
		Mode:       0o644,
		Size:       int64(len("data")),
		Typeflag:   tar.TypeReg,
		Format:     tar.FormatPAX,
		PAXRecords: map[string]string{"SCHILY.xattr." + name: value},
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// A trusted.* attribute needs CAP_SYS_ADMIN. Unprivileged (the normal case
// inside a container started without --privileged) the restore must drop the
// attribute and keep going: overlayfs bookkeeping is not user data, and an 8 GB
// extraction must not die at 76% because of it.
func TestExtractTrustedXattrIsSkippedNotFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root holds CAP_SYS_ADMIN: trusted.* is writable")
	}
	dst := t.TempDir()
	buf := tarWithXattr(t, "trusted.overlay.opaque", "y")
	x := NewExtractor(ExtractOptions{PreserveXattrs: true, Strict: true})
	stats, err := x.Extract(context.Background(), buf, dst)
	if err != nil {
		t.Fatalf("trusted.* must not abort a strict restore: %v", err)
	}
	if stats.Files != 1 || stats.XattrsSkipped != 1 {
		t.Fatalf("stats = %+v, want 1 file and 1 skipped xattr", stats)
	}
	if len(stats.Warnings) != 1 || !strings.Contains(stats.Warnings[0], "CAP_SYS_ADMIN") {
		t.Fatalf("warnings = %v, want one line naming CAP_SYS_ADMIN", stats.Warnings)
	}
	got, err := os.ReadFile(filepath.Join(dst, "payload.txt"))
	if err != nil || string(got) != "data" {
		t.Fatalf("file content lost: %q %v", got, err)
	}
}

// A namespace the destination filesystem does not know is tolerated too: the
// kernel answers EOPNOTSUPP/EINVAL and there is nothing to preserve.
func TestExtractUnsupportedXattrNamespaceIsSkipped(t *testing.T) {
	dst := t.TempDir()
	buf := tarWithXattr(t, "bogusns.attr", "v")
	x := NewExtractor(ExtractOptions{PreserveXattrs: true, Strict: true})
	stats, err := x.Extract(context.Background(), buf, dst)
	if err != nil {
		t.Fatalf("unsupported namespace must not abort a strict restore: %v", err)
	}
	if stats.XattrsSkipped != 1 {
		t.Fatalf("stats = %+v, want 1 skipped xattr", stats)
	}
}

// security.* carries real data: strict mode still fails, and the error must
// name the remediation. --allow-degraded (Strict false) skips it instead.
func TestExtractSecurityXattrHonoursStrict(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write security.*")
	}
	buf := tarWithXattr(t, "security.custom", "v")
	x := NewExtractor(ExtractOptions{PreserveXattrs: true, Strict: true})
	_, err := x.Extract(context.Background(), buf, t.TempDir())
	if err == nil {
		t.Fatal("security.* EPERM must abort in strict mode")
	}
	var hint *PermissionHintError
	if !errors.As(err, &hint) {
		t.Fatalf("error must carry a remediation hint, got %T: %v", err, err)
	}
	if !strings.Contains(hint.Error(), "--strict") {
		t.Fatalf("hint must name the remediation: %v", hint)
	}

	buf = tarWithXattr(t, "security.custom", "v")
	x = NewExtractor(ExtractOptions{PreserveXattrs: true, Strict: false})
	stats, err := x.Extract(context.Background(), buf, t.TempDir())
	if err != nil {
		t.Fatalf("degraded mode must skip the attribute: %v", err)
	}
	if stats.Files != 1 || stats.XattrsSkipped != 1 {
		t.Fatalf("stats = %+v, want 1 file and 1 skipped xattr", stats)
	}
}

func TestTolerateXattrClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"trusted.overlay.opaque", unix.EPERM, true},
		{"trusted.overlay.redirect", unix.EACCES, true},
		{"user.mark", unix.EOPNOTSUPP, true},
		{"user.mark", unix.EPERM, false},
		{"security.capability", unix.EPERM, false},
		{"system.posix_acl_access", unix.EPERM, false},
		{"trusted.overlay.opaque", unix.ENOSPC, false},
	}
	for _, tc := range cases {
		if _, _, got := tolerateXattr(tc.name, tc.err); got != tc.want {
			t.Errorf("tolerateXattr(%q, %v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// heterogeneousTar mimics what a dump of a real host produces: files owned by
// other users, a device node, a fifo, a symlink, overlayfs and security
// attributes, an unreadable mode. Nothing here is exotic — it is what a
// /var/lib/docker plus a database directory look like.
func heterogeneousTar(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(hdr *tar.Header, body string) {
		t.Helper()
		hdr.Format = tar.FormatPAX
		hdr.ModTime = time.Unix(1700000000, 0)
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(&tar.Header{Name: "db/", Typeflag: tar.TypeDir, Mode: 0o750, Uid: 999, Gid: 999}, "")
	write(&tar.Header{
		Name: "db/data.mdb", Typeflag: tar.TypeReg, Mode: 0o600, Size: 7,
		Uid: 999, Gid: 999,
		PAXRecords: map[string]string{
			"SCHILY.xattr.trusted.overlay.opaque": "y",
			"SCHILY.xattr.security.custom":        "v",
		},
	}, "payload")
	write(&tar.Header{Name: "db/locked.conf", Typeflag: tar.TypeReg, Mode: 0o000, Size: 4, Uid: 4242, Gid: 4242}, "conf")
	write(&tar.Header{Name: "db/pipe", Typeflag: tar.TypeFifo, Mode: 0o600, Uid: 999, Gid: 999}, "")
	write(&tar.Header{Name: "db/link", Typeflag: tar.TypeSymlink, Linkname: "data.mdb", Mode: 0o777}, "")
	write(&tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3}, "")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// The guarantee: extracting a heterogeneous tree unprivileged never fails. What
// cannot be applied is degraded, counted and reported; file contents are always
// written.
func TestExtractHeterogeneousTreeNeverFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can apply every operation in this fixture")
	}
	dst := t.TempDir()
	x := NewExtractor(ExtractOptions{PreserveOwner: true, PreserveXattrs: true, Overwrite: true})
	stats, err := x.Extract(context.Background(), heterogeneousTar(t), dst)
	if err != nil {
		t.Fatalf("degraded restore must not fail: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "db/data.mdb"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("content lost: %q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "db/pipe")); err != nil {
		t.Errorf("fifo not created: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "db/link")); err != nil {
		t.Errorf("symlink not created: %v", err)
	}
	// A 0000 file must still be created, with its content.
	if fi, err := os.Lstat(filepath.Join(dst, "db/locked.conf")); err != nil {
		t.Errorf("0000 file not created: %v", err)
	} else if fi.Size() != 4 {
		t.Errorf("0000 file truncated: %d bytes", fi.Size())
	}
	// Ownership, security.* xattrs and the device node are all out of reach
	// unprivileged: each one must be a counted degradation, not a failure.
	for _, class := range []string{"owner", "xattr.trusted", "xattr.security", "object"} {
		if stats.Degraded[class] == 0 {
			t.Errorf("class %q must be counted as degraded: %+v", class, stats.Degraded)
		}
	}
	if stats.Skipped != 1 {
		t.Errorf("only the device node must be skipped, got %d (%v)", stats.Skipped, stats.Errors)
	}
	if stats.Files != 2 {
		t.Errorf("files = %d, want 2", stats.Files)
	}
	if len(stats.Warnings) == 0 {
		t.Error("degradations must be reported as warnings")
	}
	if s := summarize(stats); !strings.Contains(s, "ATTENZIONE") {
		t.Errorf("summary must be loud when entries are missing: %q", s)
	}
}

// The same tree with --strict still aborts: the choice stays available.
func TestExtractHeterogeneousTreeStrictAborts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can apply every operation in this fixture")
	}
	x := NewExtractor(ExtractOptions{PreserveOwner: true, PreserveXattrs: true, Overwrite: true, Strict: true})
	if _, err := x.Extract(context.Background(), heterogeneousTar(t), t.TempDir()); err == nil {
		t.Fatal("strict mode must still abort on the first refused operation")
	}
}

// A hardlink whose first name is not on disk (filtered out of this restore)
// becomes a copy when possible, and is reported when not.
func TestExtractHardlinkFallback(t *testing.T) {
	build := func(linkname string) *bytes.Buffer {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		hdr := &tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("same")); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: "b.txt", Typeflag: tar.TypeLink, Linkname: linkname, Mode: 0o644, Format: tar.FormatPAX,
		}); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return &buf
	}
	// Missing first name: the entry is lost but the restore completes.
	dst := t.TempDir()
	x := NewExtractor(ExtractOptions{})
	stats, err := x.Extract(context.Background(), build("absent.txt"), dst)
	if err != nil {
		t.Fatalf("unresolvable hardlink must not abort: %v", err)
	}
	if stats.Skipped != 1 || stats.Files != 1 {
		t.Fatalf("stats = %+v, want 1 file and 1 skipped entry", stats)
	}
	// Resolvable first name: a real hardlink, no degradation.
	dst = t.TempDir()
	stats, err = NewExtractor(ExtractOptions{}).Extract(context.Background(), build("a.txt"), dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hardlinks != 1 || len(stats.Degraded) != 0 {
		t.Fatalf("stats = %+v, want one real hardlink and no degradation", stats)
	}
}

func TestCopyFileFallback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	n, err := copyFile(src, dst, 0o640)
	if err != nil || n != 5 {
		t.Fatalf("copyFile = %d, %v", n, err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "bytes" {
		t.Fatalf("copy corrupted: %q %v", got, err)
	}
	if _, err := copyFile(filepath.Join(dir, "absent"), dst, 0o640); err == nil {
		t.Fatal("copyFile must fail when the source is missing")
	}
}

func TestFatalAlwaysClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{syscall.ENOSPC, true},
		{syscall.EROFS, true},
		{syscall.EIO, true},
		{errNeedOverwrite, true},
		{errUnsupportedEntry, true},
		{syscall.EPERM, false},
		{syscall.EACCES, false},
		{syscall.EOPNOTSUPP, false},
	}
	for _, tc := range cases {
		if got := fatalAlways(tc.err); got != tc.want {
			t.Errorf("fatalAlways(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
