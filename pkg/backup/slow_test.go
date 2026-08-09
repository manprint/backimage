//go:build slow

package backup

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestSlowMemoryCap verifies the plan's memory prescription: a 2 GiB backup
// with --max-layer-size 64MiB and 2 jobs must stay far below the source size.
// Layers are materialized to disk, never to RAM.
// Run with `go test -tags slow ./pkg/backup/`.
func TestSlowMemoryCap(t *testing.T) {
	tree := t.TempDir()
	f, err := os.Create(filepath.Join(tree, "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	total := int64(0)
	for total < 2<<30 {
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		n, err := f.Write(buf)
		if err != nil {
			t.Fatal(err)
		}
		total += int64(n)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "out")

	// Track the live heap peak while the pipeline runs (page cache is
	// reclaimable kernel memory and is not counted).
	var peak int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.ReadMemStats(&ms)
			if int64(ms.HeapInuse) > peak {
				peak = int64(ms.HeapInuse)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	res, err := Run(context.Background(), Config{
		RootPaths:     []string{tree},
		Ref:           "demo.invalid/repo/slow:latest",
		Output:        "oci-layout",
		OutputPath:    out,
		Compression:   "zstd",
		MaxLayerSize:  64 << 20,
		Jobs:          2,
		AllowDegraded: true,
		SelfExtract:   stubSelf,
		TempDir:       t.TempDir(),
		CheckpointDir: t.TempDir(),
		Resume:        false,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	close(stop)
	wg.Wait()
	if res.BytesRaw < 2<<30 {
		t.Fatalf("expected >= 2GiB raw, got %d", res.BytesRaw)
	}
	if peak > 512<<20 {
		t.Fatalf("peak heap %d MiB > 512 MiB: memory scales with backup size", peak>>20)
	}
	t.Logf("res.Layers=%d Chunks=%d peakHeap=%dMiB", res.Layers, res.Chunks, peak>>20)
}
