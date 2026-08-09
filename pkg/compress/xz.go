package compress

import (
	"io"

	"github.com/ulikunitz/xz"
)

type xzCodec struct{}

func (c *xzCodec) ID() ID                      { return Xz }
func (c *xzCodec) Name() string                { return "xz" }
func (c *xzCodec) MediaTypeSuffix() string     { return "" }
func (c *xzCodec) Levels() (min, max, def int) { return 0, 9, 6 }

func (c *xzCodec) NewWriter(w io.Writer, level int) (io.WriteCloser, error) {
	if level < 0 || level > 9 {
		return nil, UsageErrorf("xz level %d out of range [0, 9]", level)
	}
	// ulikunitz/xz exposes no compression-level knob; the level is accepted
	// for interface uniformity and ignored by the backend.
	xw, err := xz.NewWriter(w)
	if err != nil {
		return nil, err
	}
	return &closeOnly{WriteCloser: xw}, nil
}

func (c *xzCodec) NewReader(r io.Reader) (io.ReadCloser, error) {
	xr, err := xz.NewReader(r)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(xr), nil
}
