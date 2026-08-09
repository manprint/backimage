// Package chunk cuts a plaintext stream into the units the blob envelope
// references. The Chunk shape (index, plain bytes, plain SHA-256) is fixed now
// so that content-defined chunking (decision D15, phase 10) can be dropped in
// without a format migration.
package chunk

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io"
)

// Chunk is one unit of the plaintext stream.
type Chunk struct {
	Index      int
	PlainBytes int64
	PlainSHA   [32]byte
	Data       []byte // valid only until the next call to Next
}

// Splitter cuts a stream into chunks.
type Splitter interface {
	// Name identifies the strategy: "fixed" or "cdc".
	Name() string
	// Next returns the next chunk, or io.EOF. The returned Data buffer is
	// reused: callers must consume it before calling Next again.
	Next() (*Chunk, error)
}

// ErrBadSize is returned by NewFixed for sizes outside [1 MiB, 1 GiB].
var ErrBadSize = errors.New("chunk: size out of range [1 MiB, 1 GiB]")

const (
	MinChunkSize = 1 << 20 // 1 MiB
	MaxChunkSize = 1 << 30 // 1 GiB
)

type fixedSplitter struct {
	r      io.Reader
	size   int64
	buf    []byte
	index  int
	filled int64
	h      hash.Hash
	done   bool
}

// NewFixed splits r into chunks of exactly size bytes (the last one may be
// shorter).
func NewFixed(r io.Reader, size int64) (Splitter, error) {
	if size < MinChunkSize || size > MaxChunkSize {
		return nil, ErrBadSize
	}
	return &fixedSplitter{r: r, size: size, buf: make([]byte, size), h: sha256.New()}, nil
}

func (s *fixedSplitter) Name() string { return "fixed" }

// Next returns the next chunk. The Data slice aliases an internal buffer that
// is reused on every call, so callers must copy before the next Next.
func (s *fixedSplitter) Next() (*Chunk, error) {
	if s.done {
		return nil, io.EOF
	}
	s.filled = 0
	s.h.Reset()
	for {
		n, err := s.r.Read(s.buf[s.filled:])
		if n > 0 {
			s.filled += int64(n)
			s.h.Write(s.buf[s.filled-int64(n) : s.filled])
			if s.filled == s.size {
				break
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	if s.filled == 0 {
		s.done = true
		return nil, io.EOF
	}
	c := &Chunk{
		Index:      s.index,
		PlainBytes: s.filled,
		Data:       s.buf[:s.filled],
	}
	h := s.h.Sum(nil)
	copy(c.PlainSHA[:], h)
	if s.filled < s.size {
		s.done = true
	}
	s.index++
	return c, nil
}
