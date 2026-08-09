// Package progress provides sparse progress reporting for streaming work.
package progress

import (
	"fmt"
	"io"
	"time"
)

// Reader wraps a stream and reports the number of bytes consumed. Reports are
// throttled so a large extraction does not flood stderr.
type Reader struct {
	io.Reader
	Report func(done int64)

	done          int64
	lastReported  int64
	lastReportAt  time.Time
	intervalBytes int64
	interval      time.Duration
}

// NewReader returns a reader that reports at least every 16 MiB or two
// seconds, and once more when the stream reaches EOF.
func NewReader(r io.Reader, report func(done int64)) *Reader {
	return &Reader{
		Reader:        r,
		Report:        report,
		intervalBytes: 16 << 20,
		interval:      2 * time.Second,
	}
}

// Message formats a human-readable progress line for a byte stream.
func Message(label string, done, total int64) string {
	if total > 0 {
		percent := float64(done) * 100 / float64(total)
		if percent > 100 {
			percent = 100
		}
		return fmt.Sprintf("%s: %.0f%% (%.1f/%.1f MiB)", label, percent, float64(done)/(1<<20), float64(total)/(1<<20))
	}
	return fmt.Sprintf("%s: %.1f MiB elaborati", label, float64(done)/(1<<20))
}

// Read implements io.Reader and reports consumed bytes at sparse intervals.
func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.done += int64(n)
		r.report(false)
	}
	if err == io.EOF {
		r.report(true)
	}
	return n, err
}

func (r *Reader) report(force bool) {
	if r.Report == nil {
		return
	}
	now := time.Now()
	if !force && !r.lastReportAt.IsZero() && now.Sub(r.lastReportAt) < r.interval && r.done-r.lastReported < r.intervalBytes {
		return
	}
	r.Report(r.done)
	r.lastReported = r.done
	r.lastReportAt = now
}
