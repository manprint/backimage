package compress

import "io"

type storeCodec struct{}

func (c *storeCodec) ID() ID                      { return Store }
func (c *storeCodec) Name() string                { return "store" }
func (c *storeCodec) MediaTypeSuffix() string     { return "none" }
func (c *storeCodec) Levels() (min, max, def int) { return 0, 0, 0 }

func (c *storeCodec) NewWriter(w io.Writer, level int) (io.WriteCloser, error) {
	if level != 0 {
		return nil, UsageErrorf("store codec has a single level (0), got %d", level)
	}
	return &nopWriteCloser{w: w}, nil
}

func (c *storeCodec) NewReader(r io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(r), nil
}

type nopWriteCloser struct{ w io.Writer }

func (n *nopWriteCloser) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n *nopWriteCloser) Close() error                { return nil }
