package remote

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fpierri/backimage/pkg/protocol"
	"github.com/fpierri/backimage/pkg/server"
)

type streamingSink struct {
	*testSink
	mu      sync.Mutex
	commits int32
	raw     int64
	layers  int
}

func newStreamingSink() *streamingSink { return &streamingSink{testSink: newTestSink()} }

func (s *streamingSink) CommitStream(_ context.Context, commit server.StreamCommit) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	s.raw = commit.Manifest.Totals.BytesRaw
	s.layers = len(commit.Layers)
	return "sha256:streamed", nil
}

func streamPayload(t *testing.T, size int) []byte {
	t.Helper()
	buf := make([]byte, size)
	source := rand.New(rand.NewSource(11))
	if _, err := source.Read(buf); err != nil {
		t.Fatal(err)
	}
	// A minimal but valid tar: the server parses the stream to build the index.
	return tarOf(t, buf)
}

func TestClientUploadStreamRunsTheServerPipeline(t *testing.T) {
	stream := streamPayload(t, 6<<20)
	sink := newStreamingSink()
	dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true, ProgressInterval: time.Nanosecond}, sink: sink}
	client, err := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{}})
	if err != nil {
		t.Fatal(err)
	}
	var progressSeen atomic.Int32
	result, err := client.UploadStream(context.Background(), StreamBackup{
		Start: streamStart(uint64(len(stream))),
		Source: func(_ context.Context, w io.Writer) error {
			_, copyErr := io.Copy(w, bytes.NewReader(stream))
			return copyErr
		},
		Progress: func(*protocol.StreamProgress) { progressSeen.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest != "sha256:streamed" || result.Attempts != 1 {
		t.Fatalf("result = %#v", result)
	}
	if result.Layers == 0 || result.Chunks == 0 || result.StoredBytes == 0 || result.Files != 1 {
		t.Fatalf("server counters missing: %#v", result)
	}
	if sink.commits != 1 || sink.layers != int(result.Layers) {
		t.Fatalf("commits=%d layers=%d result=%#v", sink.commits, sink.layers, result)
	}
	if progressSeen.Load() == 0 {
		t.Fatal("no server-side progress reached the client")
	}
}

func TestClientUploadStreamRejectsNonStreamingServer(t *testing.T) {
	// testSink cannot publish streams: the server must say so before any byte
	// of the archive is produced.
	dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true}, sink: newTestSink()}
	client, _ := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{0}})
	var sourceCalls atomic.Int32
	_, err := client.UploadStream(context.Background(), StreamBackup{
		Start: streamStart(1024),
		Source: func(_ context.Context, w io.Writer) error {
			sourceCalls.Add(1)
			_, copyErr := w.Write([]byte("never sent"))
			return copyErr
		},
	})
	var remoteErr *Error
	if !errors.As(err, &remoteErr) || !strings.Contains(remoteErr.Message, "streaming") {
		t.Fatalf("error = %v", err)
	}
	if sourceCalls.Load() != 0 {
		t.Fatalf("the archive was produced anyway: %d calls", sourceCalls.Load())
	}
	if dialer.attempts.Load() != 1 {
		t.Fatalf("attempts = %d, a capability error must not be retried", dialer.attempts.Load())
	}
}

func TestClientUploadStreamRetriesAndRebuildsTheArchive(t *testing.T) {
	stream := streamPayload(t, 2<<20)
	dialer := &sessionDialer{
		cfg:  server.SessionConfig{AllowNoAuth: true, ProgressInterval: time.Hour},
		sink: newStreamingSink(), fail: 1,
	}
	client, _ := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{0}})
	var sourceCalls atomic.Int32
	result, err := client.UploadStream(context.Background(), StreamBackup{
		Start: streamStart(uint64(len(stream))),
		Source: func(_ context.Context, w io.Writer) error {
			sourceCalls.Add(1)
			_, copyErr := io.Copy(w, bytes.NewReader(stream))
			return copyErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempts != 2 || sourceCalls.Load() != 1 {
		// The first attempt fails at dial time, so the archive is produced once.
		t.Fatalf("attempts = %d, source calls = %d", result.Attempts, sourceCalls.Load())
	}
}

func TestClientUploadStreamSurfacesServerFailure(t *testing.T) {
	stream := streamPayload(t, 8<<20)
	sink := &brokenStreamSink{streamingSink: newStreamingSink()}
	dialer := &sessionDialer{
		cfg:  server.SessionConfig{AllowNoAuth: true, ProgressInterval: time.Hour},
		sink: sink,
	}
	client, _ := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{}})
	_, err := client.UploadStream(context.Background(), StreamBackup{
		Start: streamStart(uint64(len(stream))),
		Source: func(_ context.Context, w io.Writer) error {
			_, copyErr := io.Copy(w, bytes.NewReader(stream))
			return copyErr
		},
	})
	if err == nil {
		t.Fatal("a failing server pipeline must fail the client")
	}
	if !strings.Contains(err.Error(), "pipeline") && !strings.Contains(err.Error(), "registry") {
		t.Fatalf("error = %v", err)
	}
}

// brokenStreamSink accepts the session but cannot upload a single blob.
type brokenStreamSink struct{ *streamingSink }

func (s *brokenStreamSink) OpenBlob(context.Context, string, string, int64) (server.BlobWriter, error) {
	return nil, errors.New("registry upload start failed")
}

func TestClientUploadStreamValidation(t *testing.T) {
	client, _ := New(Config{Dialer: &sessionDialer{}, Address: "pipe", Backoffs: []time.Duration{}})
	if _, err := client.UploadStream(context.Background(), StreamBackup{}); err == nil {
		t.Fatal("empty stream backup accepted")
	}
	if _, err := client.UploadStream(context.Background(), StreamBackup{Start: streamStart(1)}); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestClientUploadStreamReportsQuota(t *testing.T) {
	dialer := &sessionDialer{
		cfg:  server.SessionConfig{AllowNoAuth: true, MaxBytes: 16},
		sink: newStreamingSink(),
	}
	client, _ := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{}})
	_, err := client.UploadStream(context.Background(), StreamBackup{
		Start:  streamStart(1 << 20),
		Source: func(context.Context, io.Writer) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("error = %v", err)
	}
}

// tarOf wraps payload in a one-entry tar, the shape the server expects on the
// wire.
func tarOf(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{
		Name: "data.bin", Mode: 0o644, Size: int64(len(payload)),
		Typeflag: tar.TypeReg, Format: tar.FormatPAX, ModTime: time.Unix(0, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func streamStart(estimated uint64) *protocol.StreamStart {
	return &protocol.StreamStart{
		Reference:      "registry.test/me/repo:t",
		ToolVersion:    "test",
		Archive:        &protocol.ArchiveConfig{Compression: "zstd"},
		Platforms:      []*protocol.Platform{{Os: "linux", Architecture: "amd64"}},
		EstimatedBytes: estimated,
		MaxLayerBytes:  4 << 20,
		Encryption:     &protocol.EncryptionConfig{},
	}
}
