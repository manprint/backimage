// Package protocol defines the versioned remote wire protocol.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is the only protocol version understood by this build.
const Version uint32 = 1

// FrameType identifies the payload of a frame.
type FrameType uint8

const (
	FrameControl   FrameType = 1
	FrameData      FrameType = 2
	FrameKeepalive FrameType = 3
)

// MaxFrameSize is the largest accepted payload.
const MaxFrameSize = 4 << 20

var (
	ErrFrameTooLarge = errors.New("protocol frame exceeds maximum size")
	ErrFrameType     = errors.New("unknown protocol frame type")
)

func validFrameType(t FrameType) bool {
	return t == FrameControl || t == FrameData || t == FrameKeepalive
}

// WriteFrame writes one complete frame. Short writes are handled by io.Copy.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if !validFrameType(t) {
		return fmt.Errorf("%w: %d", ErrFrameType, t)
	}
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(payload), MaxFrameSize)
	}
	var header [5]byte
	header[0] = byte(t)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := io.Copy(w, io.MultiReader(bytesReader(header[:]), bytesReader(payload))); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

// ReadFrame reads one frame into buf, reusing its allocation when possible.
// The declared size is checked before the payload buffer is grown.
func ReadFrame(r io.Reader, buf []byte) (FrameType, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, buf[:0], err
	}
	t := FrameType(header[0])
	if !validFrameType(t) {
		return 0, buf[:0], fmt.Errorf("%w: %d", ErrFrameType, t)
	}
	n := int(binary.BigEndian.Uint32(header[1:]))
	if n > MaxFrameSize {
		return 0, buf[:0], fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, MaxFrameSize)
	}
	if cap(buf) < n {
		buf = make([]byte, n)
	} else {
		buf = buf[:n]
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, buf[:0], err
	}
	return t, buf, nil
}

// byteSliceReader is allocation-free and sufficient for WriteFrame's two slices.
type byteSliceReader struct{ b []byte }

func bytesReader(b []byte) *byteSliceReader { return &byteSliceReader{b: b} }

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
