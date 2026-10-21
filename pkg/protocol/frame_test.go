package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

func TestFrameRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, MaxFrameSize} {
		t.Run(string(rune(n)), func(t *testing.T) {
			want := bytes.Repeat([]byte{byte(n + 1)}, n)
			var wire bytes.Buffer
			if err := WriteFrame(&wire, FrameData, want); err != nil {
				t.Fatal(err)
			}
			typ, got, err := ReadFrame(oneByteReader{&wire}, make([]byte, 0, n))
			if err != nil {
				t.Fatal(err)
			}
			if typ != FrameData || !bytes.Equal(got, want) {
				t.Fatalf("round-trip mismatch type=%d size=%d", typ, len(got))
			}
		})
	}
}

func TestFrameRejectsOversizeBeforePayloadRead(t *testing.T) {
	var header [5]byte
	header[0] = byte(FrameData)
	binary.BigEndian.PutUint32(header[1:], MaxFrameSize+1)
	_, _, err := ReadFrame(bytes.NewReader(header[:]), nil)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("error = %v", err)
	}
	if err := WriteFrame(io.Discard, FrameData, make([]byte, MaxFrameSize+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("write error = %v", err)
	}
}

func TestFrameRejectsUnknownTypeAndShortInput(t *testing.T) {
	if err := WriteFrame(io.Discard, 99, nil); !errors.Is(err, ErrFrameType) {
		t.Fatalf("write type error = %v", err)
	}
	var wire bytes.Buffer
	wire.Write([]byte{99, 0, 0, 0, 0})
	if _, _, err := ReadFrame(&wire, nil); !errors.Is(err, ErrFrameType) {
		t.Fatalf("read type error = %v", err)
	}
	if _, _, err := ReadFrame(bytes.NewReader([]byte{1}), nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short header = %v", err)
	}
	var short bytes.Buffer
	short.Write([]byte{byte(FrameData), 0, 0, 0, 2, 1})
	if _, _, err := ReadFrame(&short, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short body = %v", err)
	}
}

func TestReadFrameReusesBuffer(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteFrame(&wire, FrameKeepalive, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 32)
	_, got, err := ReadFrame(&wire, buf)
	if err != nil {
		t.Fatal(err)
	}
	if &got[:cap(got)][0] != &buf[:cap(buf)][0] {
		t.Fatal("buffer was not reused")
	}
}

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{byte(FrameControl), 0, 0, 0, 0})
	f.Add([]byte{byte(FrameData), 0, 0, 0, 1, 7})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = ReadFrame(bytes.NewReader(data), nil)
	})
}

// TestFrameRejectsSizesAboveTheIntRange is the regression for the panic on
// 32-bit targets: the declared size was converted to int before the limit
// check, so 2^31 and above became negative, passed it, and buf[:n] panicked
// on the very first frame a server reads — the one that carries the
// authentication.
func TestFrameRejectsSizesAboveTheIntRange(t *testing.T) {
	for _, size := range []uint32{1 << 31, 1<<31 + 1, 1<<32 - 1} {
		var header [5]byte
		header[0] = byte(FrameControl)
		binary.BigEndian.PutUint32(header[1:], size)
		_, _, err := ReadFrame(bytes.NewReader(header[:]), nil)
		if !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("size %d: err = %v, want ErrFrameTooLarge", size, err)
		}
	}
}
