package remote

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/server"
	"github.com/manprint/backimage/pkg/transport"
)

// scriptedDialer is a server that answers the client's Hello with script and
// then stays silent until the client hangs up. It counts the StreamStart
// messages it receives.
type scriptedDialer struct {
	script  []*protocol.ServerMessage
	started atomic.Int32
}

func (d *scriptedDialer) Name() string { return "pipe" }

func (d *scriptedDialer) Dial(_ context.Context, _ string) (transport.Stream, error) {
	client, peer := net.Pipe()
	go d.serve(peer)
	return client, nil
}

func (d *scriptedDialer) serve(peer net.Conn) {
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(20 * time.Second))
	hello := make(chan struct{})
	go func() {
		first := true
		for {
			typ, payload, err := protocol.ReadFrame(peer, nil)
			if err != nil {
				return
			}
			if typ != protocol.FrameControl {
				continue
			}
			msg, err := protocol.DecodeClientMessage(payload)
			if err != nil {
				return
			}
			if msg.GetStreamStart() != nil {
				d.started.Add(1)
			}
			if first {
				first = false
				close(hello)
			}
		}
	}()
	<-hello
	for _, msg := range d.script {
		if err := protocol.WriteServerMessage(peer, msg); err != nil {
			return
		}
	}
	// Silent from here: the client must not wait for anything more.
	_, _ = io.Copy(io.Discard, peer)
}

func uploadStreamWithin(t *testing.T, d *scriptedDialer, limit time.Duration) error {
	t.Helper()
	client, err := New(Config{Dialer: d, Address: "pipe", Backoffs: []time.Duration{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	_, err = client.UploadStream(ctx, StreamBackup{
		Start: streamStart(1),
		Source: func(_ context.Context, w io.Writer) error {
			_, err := w.Write([]byte("tar"))
			return err
		},
	})
	return err
}

// TestStreamRefusesAProtocolVersionItDoesNotSpeak: the layer path refused a
// HelloAck above its own version, the streaming path accepted any version from
// 2 up and went on as if the server spoke v2.
func TestStreamRefusesAProtocolVersionItDoesNotSpeak(t *testing.T) {
	d := &scriptedDialer{script: []*protocol.ServerMessage{{Msg: &protocol.ServerMessage_HelloAck{HelloAck: &protocol.HelloAck{
		ServerVersion: "future", ProtocolVersion: protocol.Version + 1, Streaming: true,
	}}}}}
	err := uploadStreamWithin(t, d, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "HelloAck") {
		t.Fatalf("UploadStream = %v, want the HelloAck refused", err)
	}
	if d.started.Load() != 0 {
		t.Fatal("the client started a stream on a protocol it does not speak")
	}
}

// TestStreamFailsOnAMessageOutsideTheProtocol: the stream reader dropped any
// message it had no case for, so a peer answering with, say, a LayerAck left
// the client waiting for a BackupEnd that would never come.
func TestStreamFailsOnAMessageOutsideTheProtocol(t *testing.T) {
	d := &scriptedDialer{script: []*protocol.ServerMessage{
		{Msg: &protocol.ServerMessage_HelloAck{HelloAck: &protocol.HelloAck{
			ServerVersion: "odd", ProtocolVersion: protocol.Version, Streaming: true,
		}}},
		{Msg: &protocol.ServerMessage_StreamAck{StreamAck: &protocol.StreamAck{Ready: true}}},
		{Msg: &protocol.ServerMessage_LayerAck{LayerAck: &protocol.LayerAck{Index: 0}}},
	}}
	err := uploadStreamWithin(t, d, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("UploadStream = %v, want an unexpected message reported", err)
	}
}

// TestLayerQuotaIgnoresSkippedLayers is the regression for the client that
// counted a layer the server skipped — already in the registry — as sent. The
// server counts received bytes only, so the client refused backups the server
// would have accepted.
func TestLayerQuotaIgnoresSkippedLayers(t *testing.T) {
	present := testLayer(t, make([]byte, 2<<10))
	fresh := testLayer(t, []byte(strings.Repeat("x", 2<<10)))
	presentSize, _ := present.Size()
	freshSize, _ := fresh.Size()
	quota := max(presentSize, freshSize) + 1
	if presentSize+freshSize <= quota {
		t.Fatal("test layers too small to tell the two rules apart")
	}
	sink := newTestSink()
	digest, _ := present.Digest()
	r, _ := present.Compressed()
	sink.blobs[digest.String()], _ = io.ReadAll(r)
	_ = r.Close()

	dialer := &sessionDialer{cfg: server.SessionConfig{AllowNoAuth: true, MaxBytes: uint64(quota)}, sink: sink}
	client, err := New(Config{Dialer: dialer, Address: "pipe", Backoffs: []time.Duration{}})
	if err != nil {
		t.Fatal(err)
	}
	backup := testBackup(present)
	backup.Layers = []v1.Layer{present, fresh}
	backup.Start.LayerCount = 2
	backup.Start.EstimatedBytes = uint64(freshSize)
	result, err := client.Upload(context.Background(), backup)
	if err != nil {
		t.Fatalf("upload within the quota the server enforces: %v (server: %v)", err, dialer.ServerError())
	}
	if result.BlobsSkipped != 1 {
		t.Fatalf("result = %+v, want the present layer skipped", result)
	}
}
