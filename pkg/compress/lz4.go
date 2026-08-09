package compress

import (
	"io"

	"github.com/pierrec/lz4/v4"
)

type lz4Codec struct{}

func (c *lz4Codec) ID() ID                      { return Lz4 }
func (c *lz4Codec) Name() string                { return "lz4" }
func (c *lz4Codec) MediaTypeSuffix() string     { return "" }
func (c *lz4Codec) Levels() (min, max, def int) { return 0, 9, 1 }

func (c *lz4Codec) NewWriter(w io.Writer, level int) (io.WriteCloser, error) {
	if level < 0 || level > 9 {
		return nil, UsageErrorf("lz4 level %d out of range [0, 9]", level)
	}
	lw := lz4.NewWriter(w)
	// pierrec/lz4 levels are sparse enum values: Fast=0, LevelN=1<<(7+N).
	// Our 0..9 range maps onto them directly.
	if err := lw.Apply(lz4.CompressionLevelOption(lzLevel(level))); err != nil {
		return nil, err
	}
	return &closeOnly{WriteCloser: lw}, nil
}

func lzLevel(level int) lz4.CompressionLevel {
	if level == 0 {
		return lz4.Fast
	}
	return lz4.CompressionLevel(1 << (8 + level))
}

func (c *lz4Codec) NewReader(r io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(lz4.NewReader(r)), nil
}
