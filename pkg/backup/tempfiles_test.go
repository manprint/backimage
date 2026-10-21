package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/recovery"
	"github.com/manprint/backimage/pkg/restore"
)

// randomTree writes n incompressible files of size bytes each.
func randomTree(t *testing.T, n, size int) string {
	t.Helper()
	tree := t.TempDir()
	for i := 0; i < n; i++ {
		b := make([]byte, size)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tree, fmt.Sprintf("f%03d", i)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tree
}

func layoutConfig(tree, out, tmp string, k int) Config {
	return Config{
		RootPaths: []string{tree}, Ref: fmt.Sprintf("example.com/t/b%d:tag", k),
		Compression: "zstd", Jobs: 2, MaxLayerSize: 4 << 20, AllowDegraded: true,
		SelfExtract: stubSelf, Platforms: []string{"linux/amd64"},
		Output: "oci-layout", OutputPath: out, TempDir: tmp,
	}
}

func fullyVerify(t *testing.T, layout string) {
	t.Helper()
	src, err := restore.FromOCILayout(layout, "", restore.SourceOptions{})
	if err != nil {
		t.Fatalf("open %s: %v", layout, err)
	}
	img, err := recovery.OpenBlobSource(context.Background(), src)
	if err != nil {
		t.Fatalf("open blob source %s: %v", layout, err)
	}
	res, err := img.Verify(context.Background(), true, false)
	if err != nil || !res.OK {
		t.Fatalf("full verify of %s: ok=%v err=%v", layout, res.OK, err)
	}
}

func leftoverTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	left := make([]string, 0, len(ents))
	for _, e := range ents {
		left = append(left, e.Name())
	}
	return left
}

// TestConcurrentBackupsShareTempDir is the regression for two backups that
// run at the same time with the same --temp-dir, which is what two cron jobs
// on one host get by default. The spool used to be named after the layer
// ordinal alone, so both runs wrote the same file: one failed, and the other
// reported success while publishing a layer whose chunks no longer matched
// their digests.
func TestConcurrentBackupsShareTempDir(t *testing.T) {
	shared := t.TempDir()
	var wg sync.WaitGroup
	outs := make([]string, 2)
	errs := make([]error, 2)
	for k := 0; k < 2; k++ {
		tree := randomTree(t, 24, 1<<20)
		outs[k] = filepath.Join(t.TempDir(), "layout")
		wg.Add(1)
		go func(k int, tree string) {
			defer wg.Done()
			_, errs[k] = Run(context.Background(), layoutConfig(tree, outs[k], shared, k))
		}(k, tree)
	}
	wg.Wait()
	for k := 0; k < 2; k++ {
		if errs[k] != nil {
			t.Fatalf("backup %d: %v", k, errs[k])
		}
		fullyVerify(t, outs[k])
	}
	if left := leftoverTempFiles(t, shared); len(left) > 0 {
		t.Fatalf("temp files left in the shared dir: %v", left)
	}
}

// TestSpoolDoesNotFollowAPlantedSymlink covers the same naming flaw from the
// other side: in a shared /tmp anyone could pre-create the predictable spool
// name as a symlink, and the backup truncated and overwrote its target.
func TestSpoolDoesNotFollowAPlantedSymlink(t *testing.T) {
	tmp := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	const content = "must survive the backup"
	if err := os.WriteFile(victim, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		name := filepath.Join(tmp, fmt.Sprintf("backimage-layer-%06d.blob.tmp", i))
		if err := os.Symlink(victim, name); err != nil {
			t.Fatal(err)
		}
	}
	tree := randomTree(t, 6, 1<<20)
	out := filepath.Join(t.TempDir(), "layout")
	if _, err := Run(context.Background(), layoutConfig(tree, out, tmp, 0)); err != nil {
		t.Fatalf("backup: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("symlink target was overwritten: %d bytes now", len(got))
	}
	fullyVerify(t, out)
}

// TestFailedBuildLeavesNoTempFiles is the regression for a backup that fails
// while it streams: the cleanup used to be registered only after build and
// finalize had both succeeded, so the spool and every layer already produced
// stayed in --temp-dir, and the archive goroutine stayed blocked on the pipe
// with source files open.
func TestFailedBuildLeavesNoTempFiles(t *testing.T) {
	tmp := t.TempDir()
	tree := randomTree(t, 40, 1<<20)
	out := filepath.Join(t.TempDir(), "layout")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := layoutConfig(tree, out, tmp, 0)
	cfg.Progress = func(msg string) {
		// The first mid-stream report comes after 16 MiB: by then layers
		// have been rolled and a spool is open.
		if strings.HasPrefix(msg, "dump:") && !strings.Contains(msg, " 0 chunk)") {
			cancel()
		}
	}
	before := runtime.NumGoroutine()
	_, err := Run(ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if left := leftoverTempFiles(t, tmp); len(left) > 0 {
		t.Fatalf("temp files left after a failed build: %v", left)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines: %d before, %d after a failed build\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
}
