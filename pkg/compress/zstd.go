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

// zstdWorkers reports the encoder concurrency.
//
// It follows GOMAXPROCS for speed, which means the worker count differs between
// machines and with load. Deduplication compares stored blob digests, so it only
// works if the output does not depend on that. klauspost/compress keeps the
// frame identical across worker counts, and
// TestZstdOutputIndependentOfWorkerCount pins that down: it overrides this
// variable, which is why it is a variable. If a future version ever breaks the
// property, the test fails instead of the hit rate quietly collapsing.
var zstdWorkers = func() int {
	procs := runtime.GOMAXPROCS(0)
	if procs > 4 {
		procs = 4
	}
	return procs
}

func (c *zstdCodec) NewWriter(w io.Writer, level int) (io.WriteCloser, error) {
	if level < 1 || level > 4 {
		return nil, UsageErrorf("zstd level %d out of range [1, 4]", level)
	}
	zw, err := zstd.NewWriter(w,
		zstd.WithEncoderLevel(c.encoderLevel(level)),
		zstd.WithEncoderConcurrency(zstdWorkers()),
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
