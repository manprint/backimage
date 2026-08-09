package chunk

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/restic/chunker"
)

// ErrBadCDCParams is returned when content-defined chunking parameters are
// outside the reproducible, supported range.
var ErrBadCDCParams = errors.New("chunk: invalid CDC parameters")

// CDCParams tunes the content-defined chunker. The polynomial is part of the
// on-disk format: changing it makes later backups unable to share chunks with
// older ones.
type CDCParams struct {
	Min, Avg, Max int64
	Polynomial    uint64
}

// fixedCDCPolynomial is the irreducible Rabin polynomial used by restic's
// examples and tests. It must never be changed without a format migration.
const fixedCDCPolynomial uint64 = 0x3DA3358B4DC173

// DefaultCDCParams returns the parameters selected by --dedup.
func DefaultCDCParams() CDCParams {
	return CDCParams{
		Min:        1 << 20,
		Avg:        4 << 20,
		Max:        16 << 20,
		Polynomial: fixedCDCPolynomial,
	}
}

// NormalizeCDCParams fills zero values from DefaultCDCParams and validates the
// resulting parameters. It is exported so the pipeline can reject bad command
// line settings before starting the archive stream.
func NormalizeCDCParams(p CDCParams) (CDCParams, error) {
	d := DefaultCDCParams()
	if p.Min == 0 {
		p.Min = d.Min
	}
	if p.Avg == 0 {
		p.Avg = d.Avg
	}
	if p.Max == 0 {
		p.Max = d.Max
	}
	if p.Polynomial == 0 {
		p.Polynomial = d.Polynomial
	}
	if p.Min < MinChunkSize || p.Min >= p.Avg || p.Avg > p.Max || p.Max > MaxChunkSize {
		return CDCParams{}, fmt.Errorf("%w: min=%d avg=%d max=%d", ErrBadCDCParams, p.Min, p.Avg, p.Max)
	}
	pol := chunker.Pol(p.Polynomial)
	if p.Polynomial == 0 || pol.Deg() < 8 || pol.Deg() > 53 || !pol.Irreducible() {
		return CDCParams{}, fmt.Errorf("%w: polynomial=%#x", ErrBadCDCParams, p.Polynomial)
	}
	return p, nil
}

type cdcSplitter struct {
	c       *chunker.Chunker
	params  CDCParams
	buf     []byte
	index   int
	invalid error
}

// NewCDC splits r on Rabin content-defined boundaries. Its signature mirrors
// Splitter construction in the original design, so invalid params are
// returned by the first Next call rather than panicking or silently changing
// the backup format. Production callers should preflight with
// NormalizeCDCParams.
func NewCDC(r io.Reader, p CDCParams) Splitter {
	p, err := NormalizeCDCParams(p)
	if err != nil {
		return &cdcSplitter{invalid: err}
	}
	// restic/chunker starts testing the Rabin mask after Min bytes. Select the
	// mask for the remaining mean distance, so CDCParams.Avg describes the
	// total average chunk size rather than Min plus a second average.
	avgBits := bits.Len64(uint64(p.Avg-p.Min)) - 1
	return &cdcSplitter{
		c:      chunker.New(r, chunker.Pol(p.Polynomial), chunker.WithBoundaries(uint(p.Min), uint(p.Max)), chunker.WithAverageBits(avgBits)),
		params: p,
		buf:    make([]byte, 0, p.Max),
	}
}

func (*cdcSplitter) Name() string { return "cdc" }

func (s *cdcSplitter) Next() (*Chunk, error) {
	if s.invalid != nil {
		return nil, s.invalid
	}
	c, err := s.c.Next(s.buf[:0])
	if err != nil {
		return nil, err
	}
	s.buf = c.Data
	out := &Chunk{
		Index:      s.index,
		PlainBytes: int64(len(c.Data)),
		Data:       c.Data,
		PlainSHA:   sha256.Sum256(c.Data),
	}
	s.index++
	return out, nil
}
