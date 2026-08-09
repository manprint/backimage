package ociimg

import (
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func testIndex(t *testing.T) (v1.ImageIndex, map[string]v1.Image) {
	t.Helper()
	data := sampleDataLayers(t)
	build := func(arch string) BuiltImage {
		o := sampleOpts(t, data)
		o.Platform = v1.Platform{OS: "linux", Architecture: arch}
		img, err := BuildImage(o)
		if err != nil {
			t.Fatal(err)
		}
		return BuiltImage{Platform: v1.Platform{OS: "linux", Architecture: arch}, Image: img}
	}
	imgs := []BuiltImage{build("amd64"), build("arm64")}
	idx, err := BuildIndex(imgs)
	if err != nil {
		t.Fatal(err)
	}
	byArch := map[string]v1.Image{}
	for _, b := range imgs {
		byArch[b.Platform.OS+"/"+b.Platform.Architecture] = b.Image
	}
	return idx, byArch
}

func hostImage(byArch map[string]v1.Image) v1.Image {
	img, ok := byArch[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return nil
	}
	return img
}

func TestWriteOCILayout(t *testing.T) {
	idx, _ := testIndex(t)
	dir := t.TempDir()
	w, err := NewWriter(TargetOCILayout, dir, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := name.ParseReference("test/img:v1")
	if err := w.Write(t.Context(), ref, idx, nil); err != nil {
		t.Fatal(err)
	}
	back, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := back.IndexManifest(); err != nil {
		t.Fatal(err)
	}
	want, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := back.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("layout digest %s != %s", got, want)
	}
}

func TestWriteTar(t *testing.T) {
	idx, byArch := testIndex(t)
	want := hostImage(byArch)
	if want == nil {
		t.Skipf("host platform %s/%s not built by this test set", runtime.GOOS, runtime.GOARCH)
	}
	p := filepath.Join(t.TempDir(), "img.tar")
	w, err := NewWriter(TargetTar, p, WriterOptions{Images: byArch})
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := name.ParseReference("test/img:v1")
	if err := w.Write(t.Context(), ref, idx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	img, err := tarball.ImageFromPath(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantD, err := want.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	got, err := img.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	if got != wantD {
		t.Fatalf("tar config digest %s != %s", got, wantD)
	}
	wl, err := want.Layers()
	if err != nil {
		t.Fatal(err)
	}
	gl, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(gl) != len(wl) {
		t.Fatalf("tar layers = %d, want %d", len(gl), len(wl))
	}
	for i := range wl {
		wa := tarEntries(t, wl[i])
		ga := tarEntries(t, gl[i])
		if len(ga) != len(wa) {
			t.Fatalf("layer %d: entries %d != %d", i, len(ga), len(wa))
		}
		for name, wh := range wa {
			gh, ok := ga[name]
			if !ok {
				t.Fatalf("layer %d: missing %s after tar roundtrip", i, name)
			}
			if gh.Typeflag != wh.Typeflag || gh.Mode != wh.Mode {
				t.Fatalf("layer %d %s: type/mode %d/%d != %d/%d", i, name, gh.Typeflag, gh.Mode, wh.Typeflag, wh.Mode)
			}
		}
	}
}

func TestWriteRegistry(t *testing.T) {
	idx, _ := testIndex(t)
	srv := httptest.NewServer(registry.New())
	defer srv.Close()

	port := srv.Listener.Addr().(*net.TCPAddr).Port
	host := "127.0.0.1:" + strconvItoa(port)
	w, err := NewWriter(TargetRegistry, "", WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(host + "/test/img:v1")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan Progress, 64)
	if err := w.Write(t.Context(), ref, idx, ch); err != nil {
		t.Fatal(err)
	}
	close(ch)
	var n int
	for range ch {
		n++
	}
	if n == 0 {
		t.Fatal("no progress updates reported")
	}

	pulled, err := remote.Index(ref)
	if err != nil {
		t.Fatal(err)
	}
	want, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := pulled.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("pulled digest %s != %s", got, want)
	}
}

func TestNewWriterErrors(t *testing.T) {
	if _, err := NewWriter(Target("bogus"), "", WriterOptions{}); err == nil {
		t.Fatal("want unsupported target error")
	}
	if _, err := NewWriter(TargetOCILayout, "", WriterOptions{}); err == nil {
		t.Fatal("want missing path error")
	}
	if _, err := NewWriter(TargetTar, "", WriterOptions{}); err == nil {
		t.Fatal("want missing path error")
	}
}

func TestWriteTarHostPlatformMissing(t *testing.T) {
	idx, _ := testIndex(t)
	byArch := map[string]v1.Image{}
	w, err := NewWriter(TargetTar, filepath.Join(t.TempDir(), "x.tar"), WriterOptions{Images: byArch})
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := name.ParseReference("test/img:v1")
	err = w.Write(t.Context(), ref, idx, nil)
	if err == nil {
		t.Fatal("want error for missing host platform")
	}
}

func strconvItoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func TestWriterNames(t *testing.T) {
	for _, target := range []Target{TargetRegistry, TargetDaemon, TargetOCILayout, TargetTar} {
		w, err := NewWriter(target, filepath.Join(t.TempDir(), "x"), WriterOptions{})
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if w.Name() != target {
			t.Errorf("%s: Name() = %s", target, w.Name())
		}
	}
}

func TestWriteDaemonRequiresTag(t *testing.T) {
	idx, byArch := testIndex(t)
	host := hostImage(byArch)
	if host == nil {
		t.Skip("host platform not in test set")
	}
	ref, _ := name.NewDigest("localhost:5000/img@sha256:" + strings.Repeat("0", 64))
	w, err := NewWriter(TargetDaemon, "", WriterOptions{Images: byArch})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(t.Context(), ref, idx, nil); err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatalf("want tag-required error, got %v", err)
	}
}

func TestWriteDaemonSuccess(t *testing.T) {
	idx, byArch := testIndex(t)
	host := hostImage(byArch)
	if host == nil {
		t.Skip("host platform not in test set")
	}
	old := daemonWrite
	defer func() { daemonWrite = old }()
	daemonWrite = func(name.Tag, v1.Image) (string, error) { return "loaded", nil }
	ref, _ := name.ParseReference("localhost:5000/test/daemon-ok:v1")
	w, err := NewWriter(TargetDaemon, "", WriterOptions{Images: byArch})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(t.Context(), ref, idx, nil); err != nil {
		t.Fatalf("daemon write: %v", err)
	}
}

func TestWriterRuntimeOption(t *testing.T) {
	w, err := NewWriter(TargetTar, filepath.Join(t.TempDir(), "x.tar"), WriterOptions{Runtime: v1.Platform{OS: "linux", Architecture: "s390x"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Name()
}
