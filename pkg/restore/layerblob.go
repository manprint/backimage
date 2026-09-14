package restore

import (
	"bufio"
	"fmt"
	"io"

	"github.com/manprint/backimage/pkg/compress"
)

// layerPeekBytes is one tar header block plus room for the magic that sits at
// offset 257 of it. Every sniff below is done on a buffered reader of at least
// this size, so nothing is consumed to look at it.
const layerPeekBytes = 4096

// openLayerTar returns the tar stream a layer of a backup image carries.
//
// The blob the backup wrote for a data layer is codec(tar), with codec the one
// named in the manifest. A registry and an OCI layout hand that blob back byte
// for byte, so applying the codec once is all it takes. The local Docker
// daemon does not: it re-labels every layer it stores as
// application/vnd.docker.image.rootfs.diff.tar.gzip and gzips whatever it
// kept, so the same blob comes back with one more wrapper around it — a gzip
// around a zstd around the tar. Applying the declared codec to that produced
// "invalid input: magic number mismatch", which is why `restore --local-repo`
// could not read a single backup.
//
// So the wrappers are removed for what they are. The declared codec is never
// guessed at and is applied exactly once; what varies is only whether the
// source put one transport wrapper around it, and the walk is bounded, so no
// layer can make the reader stack decompressors.
func openLayerTar(raw io.ReadCloser, codecName string) (io.ReadCloser, error) {
	codec, err := compress.Get(codecName)
	if err != nil {
		raw.Close()
		return nil, err
	}
	gzipCodec, err := compress.Get("gzip")
	if err != nil {
		raw.Close()
		return nil, err
	}
	chain := &layerChain{closers: []io.Closer{raw}}
	br := bufio.NewReaderSize(raw, layerPeekBytes)

	switch {
	case isTarHeader(peekHead(br)):
		// A tar already in front means the source undid everything on its own:
		// a daemon that decompressed the layer when it loaded the image stores
		// the bare tar and hands that back. Applying the codec here would be
		// decompressing something that is not compressed.
		chain.r = br
		return chain, nil
	case isGzipHeader(peekHead(br)) && codec.ID() != compress.Gzip:
		// A gzip in front of a backup that is not gzipped is not the backup's:
		// it belongs to whoever stored the layer.
		next, err := gzipCodec.NewReader(br)
		if err != nil {
			chain.Close()
			return nil, fmt.Errorf("involucro gzip del layer: %w", err)
		}
		chain.closers = append(chain.closers, next)
		br = bufio.NewReaderSize(next, layerPeekBytes)
	}

	decoded, err := codec.NewReader(br)
	if err != nil {
		chain.Close()
		return nil, err
	}
	chain.closers = append(chain.closers, decoded)
	out := bufio.NewReaderSize(decoded, layerPeekBytes)

	// With a gzip backup the wrapper and the codec are the same magic seen from
	// outside, so the ambiguity is settled from the inside: behind the first
	// gzip there is either the tar or the one the backup itself wrote.
	if codec.ID() == compress.Gzip && isGzipHeader(peekHead(out)) {
		next, err := gzipCodec.NewReader(out)
		if err != nil {
			chain.Close()
			return nil, fmt.Errorf("involucro gzip del layer: %w", err)
		}
		chain.closers = append(chain.closers, next)
		out = bufio.NewReaderSize(next, layerPeekBytes)
	}
	chain.r = out
	return chain, nil
}

// layerChain is the reader at the end of the decompression chain, carrying
// everything that has to be closed with it.
type layerChain struct {
	r       io.Reader
	closers []io.Closer
}

func (c *layerChain) Read(p []byte) (int, error) { return c.r.Read(p) }

// Close closes the chain outermost-last, so every decoder is released before
// the stream it was reading from.
func (c *layerChain) Close() error {
	var first error
	for i := len(c.closers) - 1; i >= 0; i-- {
		if err := c.closers[i].Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// peekHead returns what is available of the first layerPeekBytes without
// consuming it. A short stream is not an error here: it simply matches none of
// the shapes below, and the reader that follows produces the real diagnosis.
func peekHead(br *bufio.Reader) []byte {
	b, err := br.Peek(layerPeekBytes)
	if err != nil && len(b) == 0 {
		return nil
	}
	return b
}

// isTarHeader reports whether b starts with a tar header block. Both the USTAR
// and the PAX writer put "ustar" at offset 257; GNU writes "ustar " there.
func isTarHeader(b []byte) bool {
	const magicOffset = 257
	if len(b) < magicOffset+5 {
		return false
	}
	return string(b[magicOffset:magicOffset+5]) == "ustar"
}

func isGzipHeader(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}
