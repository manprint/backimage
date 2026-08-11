package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/recovery"
	"github.com/manprint/backimage/pkg/registry"
	backremote "github.com/manprint/backimage/pkg/remote"
	"github.com/manprint/backimage/pkg/restore"
	"github.com/manprint/backimage/pkg/server"
	"github.com/manprint/backimage/pkg/transport"
)

func netPipe() (net.Conn, net.Conn) { return net.Pipe() }

// streamDialer runs a real server session at the other end of an in-process
// pipe, so the test exercises the whole protocol v2 path.
type streamDialer struct {
	cfg  server.SessionConfig
	sink server.Sink
}

func (d *streamDialer) Name() string { return "pipe" }

func (d *streamDialer) Dial(ctx context.Context, _ string) (transport.Stream, error) {
	session, err := server.NewSession(d.cfg, d.sink)
	if err != nil {
		return nil, err
	}
	client, peer := netPipe()
	go func() { _ = session.Run(ctx, peer) }()
	return client, nil
}

// TestRemoteStreamBackupRoundTrip proves the acceptance criterion of the
// streaming protocol: the client only walks the filesystem and writes a tar,
// the server builds and pushes everything, and the published image restores
// byte for byte.
func TestRemoteStreamBackupRoundTrip(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()

	tree := t.TempDir()
	payload := make([]byte, 40<<20)
	if _, err := rand.New(rand.NewSource(3)).Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "data.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "sub", "note.txt"), []byte("streamed backup"), 0o644); err != nil {
		t.Fatal(err)
	}

	ref := refTo(srv.URL)
	client := streamClient(t, ref, srv.URL)
	tempDir := t.TempDir()
	cfg := Config{
		RootPaths:     []string{tree},
		Ref:           ref,
		Compression:   "zstd",
		MaxLayerSize:  16 << 20,
		AllowDegraded: true,
		Encrypt:       true,
		Passphrase:    func() ([]byte, error) { return []byte("stream passphrase"), nil },
		Runnable:      false,
		Platforms:     []string{"linux/amd64"},
		TempDir:       tempDir,
		SelfExtract:   stubSelf,
		RemoteStream:  client,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("streaming backup: %v", err)
	}
	if res.Digest == "" || res.Layers < 2 || res.Chunks == 0 || res.BytesStored == 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.BytesRaw < int64(len(payload)) {
		t.Fatalf("raw bytes = %d, want >= %d", res.BytesRaw, len(payload))
	}
	// The whole point of the streaming protocol: no layer ever touches this host.
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("client temp dir is not empty: %v", entries)
	}

	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	source, err := restore.FromRegistry(context.Background(), parsed, nil, restore.SourceOptions{
		CacheDir: t.TempDir(), CacheSize: 8 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	manifest, err := source.Manifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Encryption.Enabled || manifest.Encryption.NonceMode != "random" {
		t.Fatalf("manifest encryption = %+v", manifest.Encryption)
	}
	if manifest.Archive.Compression != "zstd" {
		t.Fatalf("manifest archive = %+v", manifest.Archive)
	}
	// The server-built image must hide the same fields the local pipeline hides:
	// nothing describing the plaintext is readable before the unlock below.
	if manifest.Totals != (index.Totals{}) || manifest.Sources != nil {
		t.Fatalf("public manifest leaks content: %+v / %v", manifest.Totals, manifest.Sources)
	}
	if manifest.Private == nil || manifest.SchemaVersion != index.SchemaVersionPrivate {
		t.Fatalf("encrypted manifest without private metadata: schema %d, private %+v", manifest.SchemaVersion, manifest.Private)
	}

	backupImage, err := recovery.OpenBlobSource(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer backupImage.Close()
	if err := backupImage.Unlock(context.Background(), crypt.Identity{Passphrase: []byte("stream passphrase")}); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// After the unlock the private metadata is merged back, so readers keep
	// seeing the shape they always saw.
	if backupImage.Manifest.Totals.Files != 2 {
		t.Fatalf("totals after unlock = %+v", backupImage.Manifest.Totals)
	}
	if len(backupImage.Manifest.Sources) != 1 || backupImage.Manifest.Sources[0] != tree {
		t.Fatalf("sources after unlock = %v", backupImage.Manifest.Sources)
	}
	for i, c := range backupImage.Chunks.Chunks {
		if c.Ps == "" || c.Pb == 0 {
			t.Fatalf("chunk %d has no plaintext metadata after unlock: %+v", i, c)
		}
	}
	var restored bytes.Buffer
	if err := backupImage.StreamTar(context.Background(), &restored, true); err != nil {
		t.Fatalf("stream tar: %v", err)
	}
	found := false
	reader := tar.NewReader(bytes.NewReader(restored.Bytes()))
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if !strings.HasSuffix(header.Name, "data.bin") {
			continue
		}
		content, readErr := io.ReadAll(reader)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(content, payload) {
			t.Fatalf("restored payload differs: %d vs %d bytes", len(content), len(payload))
		}
		found = true
	}
	if !found {
		t.Fatal("data.bin missing from the restored archive")
	}
}

func streamClient(t *testing.T, ref, registryURL string) *backremote.Client {
	t.Helper()
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	keychain := registry.NewKeychain(nil, nil)
	auth, err := keychain.Resolve(parsed.Context())
	if err != nil {
		t.Fatal(err)
	}
	sink, err := server.NewRegistrySink(server.RegistrySinkOptions{
		Broker: server.NewTokenBroker(5 * time.Second), Jobs: 2,
		SelfExtract: stubSelf,
	})
	if err != nil {
		t.Fatal(err)
	}
	dialer := &streamDialer{
		cfg:  server.SessionConfig{AllowNoAuth: true, TempDir: t.TempDir(), ProgressInterval: 10 * time.Millisecond},
		sink: sink,
	}
	client, err := backremote.New(backremote.Config{
		Dialer: dialer, Address: strings.TrimPrefix(registryURL, "http://"),
		Provider: registry.NewProvider(parsed.Context().RegistryStr(), auth),
		Backoffs: []time.Duration{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
