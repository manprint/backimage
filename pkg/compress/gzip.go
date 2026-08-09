package compress

import (
	"io"

	gzip "github.com/klauspost/compress/gzip"
)

type gzipCodec struct{}

func (c *gzipCodec) ID() ID                      { return Gzip }
func (c *gzipCodec) Name() string                { return "gzip" }
func (c *gzipCodec) MediaTypeSuffix() string     { return "gzip" }
func (c *gzipCodec) Levels() (min, max, def int) { return 1, 9, 6 }

func (c *gzipCodec) NewWriter(w io.Writer, level int) (io.WriteCloser, error) {
	if level < 1 || level > 9 {
		return nil, UsageErrorf("gzip level %d out of range [1, 9]", level)
	}
	zw, err := gzip.NewWriterLevel(w, level)
	if err != nil {
		return nil, err
	}
	// The underlying writer must survive Close.
	return &closeOnly{WriteCloser: zw}, nil
}

func (c *gzipCodec) NewReader(r io.Reader) (io.ReadCloser, error) {
	return gzip.NewReader(r)
}

// closeOnly forwards Close to the wrapped compressor, never to an outer
// writer: the caller decides when the stream it writes into is done.
type closeOnly struct {
	io.WriteCloser
}

func (c *closeOnly) Close() error { return c.WriteCloser.Close() }
