package server

import (
	"fmt"
	"net"
	"runtime"
	"testing"

	"github.com/manprint/backimage/pkg/protocol"
)

// allocatedBy reports how many bytes fn asked the allocator for. It is the
// measurement the phase asks for: an error class alone would be satisfied by
// a server that allocates the hostile amount first and complains afterwards.
func allocatedBy(t *testing.T, fn func()) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestAnAnnouncedLayerCountIsRefusedBeforeItIsAllocated is A7.5 on the server
// side. layer_count is a uint32 in a message of a few dozen bytes, and it used
// to become the capacity of the layer slice without ever being compared to
// anything: a peer that announced 4294967295 layers made this process ask for
// some 270 GB before sending one byte of data.
//
// The ceiling is the overlayfs budget of a runnable image, the same
// maxDataLayers the streaming pipeline already enforces while it assembles
// layers, so nothing new is invented here — only the moment it is checked.
func TestAnAnnouncedLayerCountIsRefusedBeforeItIsAllocated(t *testing.T) {
	for _, count := range []uint32{maxDataLayers + 1, 1 << 20, 1<<32 - 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			sink := newMemorySink()
			client, done := startSession(t, SessionConfig{AuthToken: []byte("x")}, sink)
			hello(t, client, "x")

			var got *protocol.Error
			allocated := allocatedBy(t, func() {
				writeClient(t, client, backupStart("repo:t", count, 1))
				got = readServer(t, client).GetError()
			})
			// One Layer is 5 machine words plus three string headers; the
			// refused counts would need gigabytes. A megabyte of slack covers
			// the protobuf decode and the error message.
			if allocated > 1<<20 {
				t.Fatalf("the server allocated %d bytes for a layer count it then refused", allocated)
			}
			if got == nil || got.Kind != ErrorUsage {
				t.Fatalf("error = %v, want a usage refusal", got)
			}
			_ = client.Close()
			if err := <-done; err == nil {
				t.Fatal("the session must end badly")
			}
			if sink.opened != 0 {
				t.Fatalf("blobs opened before the refusal: %d", sink.opened)
			}
		})
	}
}

// TestTheLargestLegitimateLayerCountIsStillAccepted keeps the bound from
// being a regression: a full image is exactly maxDataLayers data layers, and
// the client's own planner produces that for a large backup.
func TestTheLargestLegitimateLayerCountIsStillAccepted(t *testing.T) {
	sink := newMemorySink()
	client, done := startSession(t, SessionConfig{AuthToken: []byte("x")}, sink)
	hello(t, client, "x")
	writeClient(t, client, backupStart("repo:t", maxDataLayers, 1))
	if ack := readServer(t, client).GetBackupAck(); ack == nil || !ack.Ready {
		t.Fatalf("a full image was refused: %v", ack)
	}
	closeSession(t, client, done)
}

func closeSession(t *testing.T, client net.Conn, done <-chan error) {
	t.Helper()
	_ = client.Close()
	<-done
}
