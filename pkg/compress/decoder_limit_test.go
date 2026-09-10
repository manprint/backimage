package compress

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// wideWindowFrame is a valid zstd frame whose header asks the decoder for a
// window far larger than the payload needs. It costs 115 bytes to publish.
func wideWindowFrame(t *testing.T, window int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf, zstd.WithWindowSize(window), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, io.LimitReader(zeroSource{}, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type zeroSource struct{}

func (zeroSource) Read(p []byte) (int, error) { return len(p), nil }

// TestADecoderIsNotToldToHoldWhateverTheFrameAsksFor is A7.3 for the codec
// everything else goes through. The library default is 64 GiB, chosen by
// whoever wrote the frame — and every frame this project reads comes out of
// an image somebody else can publish.
func TestADecoderIsNotToldToHoldWhateverTheFrameAsksFor(t *testing.T) {
	codec, err := Get("zstd")
	if err != nil {
		t.Fatal(err)
	}
	frame := wideWindowFrame(t, 2*MaxDecoderMemory)
	if len(frame) > 4096 {
		t.Fatalf("the fixture must be cheap to publish, it is %d bytes", len(frame))
	}
	r, err := codec.NewReader(bytes.NewReader(frame))
	if err == nil {
		_, err = io.Copy(io.Discard, r)
		r.Close()
	}
	if err == nil {
		t.Fatal("a frame asking for more than MaxDecoderMemory must be refused")
	}
	if !strings.Contains(err.Error(), "window size") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

// TestAnOrdinaryFrameStillDecodes is the control: the bound must be above
// anything this project writes.
func TestAnOrdinaryFrameStillDecodes(t *testing.T) {
	codec, err := Get("zstd")
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("ordinary payload "), 1<<16)
	var buf bytes.Buffer
	w, err := codec.NewWriter(&buf, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := codec.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("an ordinary frame must decode: %d bytes, %v", len(got), err)
	}
}
