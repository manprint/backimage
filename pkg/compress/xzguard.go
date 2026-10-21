package compress

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// MaxXzDictionary bounds the LZMA2 dictionary an xz block may ask the decoder
// for. The decoder allocates the dictionary the block header declares, up to
// 4 GiB, before it has produced a single byte, so the limit has to be applied
// to the header itself: a cap on the output comes too late. 64 MiB is the
// largest dictionary any xz preset uses (-9); this project's writer asks for
// 8 MiB.
const MaxXzDictionary = 64 << 20

var errXzFraming = errors.New("xz: malformed stream")

// xzGuard passes an xz stream through unchanged while following its framing,
// and fails the read that carries a block header asking for more than
// MaxXzDictionary — before the decoder behind it can see that header.
//
// Following the framing needs no decompression: every LZMA2 chunk header
// states the size of its payload, so the guard knows where each block ends
// and the next header starts. It accepts what the xz format allows (several
// streams, stream padding) and only one filter chain, a single LZMA2 filter,
// which is all the decoder implements anyway.
type xzGuard struct {
	r   io.Reader
	err error

	state xzState
	need  int    // bytes still to collect into buf, or to skip
	buf   []byte // the field being collected

	checkSize int   // per-block check size of the current stream
	blockData int64 // compressed bytes of the current block so far
	records   uint64
	indexLen  int64
	varint    uint64
	shift     uint
	field     int // which varint of an index record is being read
}

type xzState int

const (
	xzStreamHeader xzState = iota
	xzBlockOrIndex
	xzBlockHeader
	xzChunkControl
	xzChunkSizes
	xzSkip
	xzBlockPadding
	xzCheck
	xzIndexCount
	xzIndexRecord
	xzIndexPadding
	xzIndexCRC
	xzStreamFooter
	xzStreamPadding
)

func newXzGuard(r io.Reader) *xzGuard {
	return &xzGuard{r: r, state: xzStreamHeader, need: 12}
}

func (g *xzGuard) Read(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	n, err := g.r.Read(p)
	if perr := g.feed(p[:n]); perr != nil {
		g.err = perr
		return 0, perr
	}
	return n, err
}

// feed advances the parser over b. Every state consumes at least one byte or
// changes state, so the loop always terminates.
func (g *xzGuard) feed(b []byte) error {
	for len(b) > 0 {
		switch g.state {
		case xzSkip:
			k := min(int64(len(b)), int64(g.need))
			g.need -= int(k)
			g.blockData += k
			b = b[k:]
			if g.need == 0 {
				g.state = xzChunkControl
			}
		case xzStreamHeader, xzBlockHeader, xzChunkSizes, xzIndexCRC, xzStreamFooter, xzCheck, xzBlockPadding, xzIndexPadding:
			k := min(len(b), g.need)
			g.buf = append(g.buf, b[:k]...)
			g.need -= k
			b = b[k:]
			if g.need == 0 {
				if err := g.complete(); err != nil {
					return err
				}
			}
		case xzBlockOrIndex:
			c := b[0]
			b = b[1:]
			if c == 0 {
				g.state, g.indexLen = xzIndexCount, 1
				g.varint, g.shift = 0, 0
				continue
			}
			g.state, g.need = xzBlockHeader, int(c)*4+3 // (c+1)*4, the size byte already read
			g.buf = append(g.buf[:0], c)
		case xzChunkControl:
			c := b[0]
			b = b[1:]
			g.blockData++
			switch {
			case c == 0:
				g.state, g.need = xzBlockPadding, int((4-g.blockData%4)%4)
				g.buf = g.buf[:0]
				if g.need == 0 {
					if err := g.complete(); err != nil {
						return err
					}
				}
			case c == 1 || c == 2:
				g.state, g.need = xzChunkSizes, 2
				g.buf = append(g.buf[:0], c)
			case c >= 0x80:
				g.state, g.need = xzChunkSizes, 4
				if c >= 0xc0 {
					g.need = 5
				}
				g.buf = append(g.buf[:0], c)
			default:
				return fmt.Errorf("%w: invalid LZMA2 control byte %#x", errXzFraming, c)
			}
		case xzIndexCount, xzIndexRecord:
			c := b[0]
			b = b[1:]
			g.indexLen++
			done, err := g.varintByte(c)
			if err != nil {
				return err
			}
			if !done {
				continue
			}
			if g.state == xzIndexCount {
				g.records, g.field = g.varint, 0
			} else if g.field++; g.field == 2 {
				g.records--
				g.field = 0
			}
			g.varint, g.shift = 0, 0
			if g.records == 0 && g.field == 0 {
				g.state, g.need = xzIndexPadding, int((4-g.indexLen%4)%4)
				g.buf = g.buf[:0]
				if g.need == 0 {
					if err := g.complete(); err != nil {
						return err
					}
				}
			} else {
				g.state = xzIndexRecord
			}
		case xzStreamPadding:
			if b[0] == 0 {
				b = b[1:]
				continue
			}
			g.state, g.need = xzStreamHeader, 12
			g.buf = g.buf[:0]
		}
	}
	return nil
}

func (g *xzGuard) varintByte(c byte) (bool, error) {
	if g.shift >= 63 {
		return false, fmt.Errorf("%w: integer too long", errXzFraming)
	}
	g.varint |= uint64(c&0x7f) << g.shift
	g.shift += 7
	return c&0x80 == 0, nil
}

var xzMagic = []byte{0xfd, '7', 'z', 'X', 'Z', 0}

// complete acts on a field that has been collected in full.
func (g *xzGuard) complete() error {
	switch g.state {
	case xzStreamHeader:
		if !bytes.Equal(g.buf[:6], xzMagic) || g.buf[6] != 0 || g.buf[7] > 0x0f {
			return fmt.Errorf("%w: bad stream header", errXzFraming)
		}
		g.checkSize = xzCheckSize(g.buf[7])
		g.state = xzBlockOrIndex
	case xzBlockHeader:
		if err := g.blockHeader(g.buf); err != nil {
			return err
		}
		g.state, g.blockData = xzChunkControl, 0
	case xzChunkSizes:
		c := g.buf[0]
		if c == 1 || c == 2 {
			g.need = (int(g.buf[1])<<8 | int(g.buf[2])) + 1
		} else {
			g.need = (int(g.buf[3])<<8 | int(g.buf[4])) + 1
		}
		g.blockData += int64(len(g.buf) - 1)
		g.state = xzSkip
	case xzBlockPadding:
		g.state, g.need = xzCheck, g.checkSize
		g.buf = g.buf[:0]
		if g.need == 0 {
			g.state = xzBlockOrIndex
		}
	case xzCheck:
		g.state = xzBlockOrIndex
	case xzIndexPadding:
		g.state, g.need = xzIndexCRC, 4
		g.buf = g.buf[:0]
	case xzIndexCRC:
		g.state, g.need = xzStreamFooter, 12
		g.buf = g.buf[:0]
	case xzStreamFooter:
		if g.buf[10] != 'Y' || g.buf[11] != 'Z' {
			return fmt.Errorf("%w: bad stream footer", errXzFraming)
		}
		g.state = xzStreamPadding
	}
	g.buf = g.buf[:0]
	return nil
}

// blockHeader checks one block header: a single LZMA2 filter whose
// dictionary is within MaxXzDictionary.
func (g *xzGuard) blockHeader(h []byte) error {
	if len(h) < 8 {
		return fmt.Errorf("%w: short block header", errXzFraming)
	}
	flags := h[1]
	if flags&0x3c != 0 {
		return fmt.Errorf("%w: reserved block flags", errXzFraming)
	}
	if flags&0x03 != 0 {
		return fmt.Errorf("%w: only a single LZMA2 filter is supported", errXzFraming)
	}
	p := h[2 : len(h)-4] // after size and flags, before the CRC
	read := func() (uint64, error) {
		var v uint64
		for i := 0; i < 9 && i < len(p); i++ {
			v |= uint64(p[i]&0x7f) << (7 * uint(i))
			if p[i]&0x80 == 0 {
				p = p[i+1:]
				return v, nil
			}
		}
		return 0, fmt.Errorf("%w: bad integer in block header", errXzFraming)
	}
	for _, present := range []bool{flags&0x40 != 0, flags&0x80 != 0} {
		if present {
			if _, err := read(); err != nil {
				return err
			}
		}
	}
	id, err := read()
	if err != nil {
		return err
	}
	size, err := read()
	if err != nil {
		return err
	}
	if id != 0x21 || size != 1 || len(p) < 1 {
		return fmt.Errorf("%w: only a single LZMA2 filter is supported", errXzFraming)
	}
	dict, err := lzma2Dictionary(p[0])
	if err != nil {
		return err
	}
	if dict > MaxXzDictionary {
		return fmt.Errorf("xz: block asks for a %d MiB dictionary, more than the %d MiB a reader will allocate",
			dict>>20, MaxXzDictionary>>20)
	}
	return nil
}

// lzma2Dictionary decodes the LZMA2 dictionary-size property.
func lzma2Dictionary(b byte) (int64, error) {
	switch {
	case b > 40:
		return 0, fmt.Errorf("%w: invalid LZMA2 dictionary byte %d", errXzFraming, b)
	case b == 40:
		return 1<<32 - 1, nil
	}
	return int64(2|b&1) << (b/2 + 11), nil
}

// xzCheckSize is the size of the integrity check each block carries, per
// check ID: 0, then 4, 8, 16, 32 and 64 bytes for each run of three IDs.
func xzCheckSize(id byte) int {
	if id == 0 {
		return 0
	}
	return 4 << ((id - 1) / 3)
}
