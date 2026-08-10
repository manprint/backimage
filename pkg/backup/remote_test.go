package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/index"
	backremote "github.com/manprint/backimage/pkg/remote"
)

type captureRemote struct {
	backup backremote.Backup
}

func (c *captureRemote) Upload(_ context.Context, backup backremote.Backup) (backremote.Result, error) {
	c.backup = backup
	for _, layer := range backup.Layers {
		r, err := layer.Compressed()
		if err != nil {
			return backremote.Result{}, err
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			_ = r.Close()
			return backremote.Result{}, err
		}
		if err := r.Close(); err != nil {
			return backremote.Result{}, err
		}
	}
	return backremote.Result{Digest: backup.ExpectedDigest, BlobsSkipped: 2}, nil
}

func TestPipelineRemotePayloadUsesSameBuiltLayers(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "file"), bytes.Repeat([]byte("x"), 32<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	capture := new(captureRemote)
	created := "2026-08-09T10:00:00Z"
	res, err := Run(context.Background(), Config{
		RootPaths: []string{tree}, Ref: "example.com/me/remote:t1",
		Compression: "zstd", Level: 1, Jobs: 1, MaxLayerSize: 1 << 20,
		AllowDegraded: true, SelfExtract: stubSelf, Encrypt: false,
		Runnable: false, Platforms: []string{"linux/amd64"},
		TempDir: t.TempDir(), Remote: capture, Created: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest == "" || res.Digest != capture.backup.ExpectedDigest || res.SkippedBlobs != 2 {
		t.Fatalf("result = %#v", res)
	}
	if capture.backup.Start == nil || len(capture.backup.Layers) != res.Layers {
		t.Fatalf("payload layers=%d result=%d", len(capture.backup.Layers), res.Layers)
	}
	if capture.backup.Start.LayerCount != uint32(len(capture.backup.Layers)) || capture.backup.Start.EstimatedBytes == 0 {
		t.Fatalf("start = %#v", capture.backup.Start)
	}
	metadata := tarFile(t, capture.backup.Start.MetadataLayer, "/backup/manifest.json")
	manifest, err := index.ReadManifest(bytes.NewReader(metadata))
	if err != nil {
		t.Fatal(err)
	}
	wantCreated, _ := time.Parse(time.RFC3339, created)
	if !manifest.CreatedAt.Equal(wantCreated) {
		t.Fatalf("createdAt = %s, want %s", manifest.CreatedAt, wantCreated)
	}
}

func TestPipelineRemoteValidation(t *testing.T) {
	cfg := Config{RootPaths: []string{"."}, Ref: "example.com/me/x:t", Remote: new(captureRemote), Output: "tar", OutputPath: "x"}
	if err := Validate(cfg); err == nil {
		t.Fatal("remote with local output accepted")
	}
	cfg.Output = ""
	cfg.Created = "not-a-time"
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("invalid created timestamp accepted")
	}
}

func tarFile(t *testing.T, layer []byte, want string) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(layer))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == want {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
	t.Fatalf("%s not found in metadata layer", want)
	return nil
}
