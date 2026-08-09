package compress

import (
	"io"
	"runtime"

	"github.com/klauspost/compress/zstd"
)

type zstdCodec struct{}

func (c *zstdCodec) ID() ID                      { return Zstd }
func (c *zstdCodec) Name() string                { return "zstd" }
func (c *zstdCodec) MediaTypeSuffix() string     { return "zstd" }
func (c *zstdCodec) Levels() (min, max, def int) { return 1, 4, 2 }

// encoderLevel maps our 1..4 range onto klauspost's speed constants, which
// are ints 1 (SpeedFastest) .. 4 (SpeedBestCompression).
func (c *zstdCodec) encoderLevel(level int) zstd.EncoderLevel {
	return zstd.EncoderLevel(level)
}

func (c *zstdCodec) NewWriter(w io.Writer, level int) (io.WriteCloser, error) {
	if level < 1 || level > 4 {
		return nil, UsageErrorf("zstd level %d out of range [1, 4]", level)
	}
	procs := runtime.GOMAXPROCS(0)
	if procs > 4 {
		procs = 4
	}
	zw, err := zstd.NewWriter(w,
		zstd.WithEncoderLevel(c.encoderLevel(level)),
		zstd.WithEncoderConcurrency(procs),
	)
	if err != nil {
		return nil, err
	}
	return &closeOnly{WriteCloser: zw}, nil
}

func (c *zstdCodec) NewReader(r io.Reader) (io.ReadCloser, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	return dec.IOReadCloser(), nil
}
