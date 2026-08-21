package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/manprint/backimage/pkg/restore"
)

// The full read-back must download the published layers again and recompute
// every stored digest. Here the registry is honest, so it has to pass, and the
// log must carry the evidence an operator can audit.
func TestVerifyAfterPushFullReReadsTheImage(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()

	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "payload.bin"), []byte(strings.Repeat("backimage", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := pipelinePushConfig(srv.URL, tree)
	cfg.VerifyAfterPush = VerifyPushFull
	cfg.Platforms = []string{"linux/amd64"}
	var log []string
	cfg.Progress = func(message string) { log = append(log, message) }
	prepPushDirs(&cfg)

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("backup with full verification: %v", err)
	}
	joined := strings.Join(log, "\n")
	for _, want := range []string{
		"verifica rapida superata",
		"verifica completa",
		"chunk con digest memorizzato coincidente", //nolint:misspell // Messaggio in italiano.
		"integrità: registrati",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the log must carry the evidence %q:\n%s", want, joined)
		}
	}
}

// A registry that mangles a data layer after publishing it is caught by the
// same streaming read-back the full level runs, here driven directly so the
// corruption can be injected between the push and the verification.
func TestFullReadBackCatchesCorruptedLayerOverHTTP(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()

	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "payload.bin"), []byte(strings.Repeat("backimage", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := pipelinePushConfig(srv.URL, tree)
	cfg.Platforms = []string{"linux/amd64"}
	cfg.VerifyAfterPush = VerifyPushOff
	// Uncompressed layers, so flipping one byte of the blob leaves the tar
	// readable: the corruption has to be caught by a digest, not by a decoder.
	cfg.Compression = "store"
	prepPushDirs(&cfg)
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	// Flip one byte in the largest blob. This fake does not recompute digests,
	// so a HEAD keeps saying everything is fine: only re-reading the bytes can
	// notice, which is exactly what the full level does.
	reg.mu.Lock()
	biggest := ""
	for digest, body := range reg.blobs {
		if biggest == "" || len(body) > len(reg.blobs[biggest]) {
			biggest = digest
		}
	}
	body := reg.blobs[biggest]
	body[len(body)/2] ^= 0xff
	reg.mu.Unlock()
	if biggest == "" {
		t.Fatal("no blob was published")
	}

	ref, err := name.ParseReference(refTo(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	src, err := restore.FromRegistry(context.Background(), ref, nil, restore.SourceOptions{
		Platform: "linux/amd64", CacheSize: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	report, err := restore.VerifyStoredSource(context.Background(), src, true, nil)
	if !errors.Is(err, restore.ErrStoredMismatch) {
		t.Fatalf("the corrupted layer must be reported, got %v (report %+v)", err, report)
	}
	if len(report.Errors) == 0 || !strings.Contains(strings.Join(report.Errors, "\n"), "digest") {
		t.Fatalf("the report must name the digest that disagreed: %+v", report)
	}
}
