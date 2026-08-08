package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// ErrPassphrase marks a wrong or missing passphrase (exit code 4).
var ErrPassphrase = errors.New("wrong passphrase or key")

// ErrIntegrity marks an integrity failure (exit code 5).
var ErrIntegrity = errors.New("integrity check failed")

// ErrInterrupted marks a cancelled operation (exit code 7).
var ErrInterrupted = errors.New("operation interrupted")

// ErrNetwork marks a network or registry failure (exit code 6).
var ErrNetwork = errors.New("network or registry failure")

type ctxKey struct{}

// WithLogger returns a context that carries the given logger.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// LoggerFrom extracts the logger from ctx, or returns a no-op one.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// NewLoggerFor builds the slog logger for the given verbosity on errOut.
// Verbosity: 0=warn, 1=debug, 2+=trace.
func NewLoggerFor(errOut io.Writer, verbose int) *slog.Logger {
	level := slog.LevelWarn
	switch {
	case verbose >= 2:
		level = slog.LevelDebug - 4 // trace
	case verbose >= 1:
		level = slog.LevelDebug
	}
	h := slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}

// ReportError renders err to stderr with its optional hint.
func ReportError(err error) {
	ReportErrorTo(os.Stderr, err)
}

// ReportErrorTo renders err to w: "error: ..." plus "hint: ..." when present.
func ReportErrorTo(w io.Writer, err error) {
	fmt.Fprintf(w, "error: %v\n", err)
	var ce *Error
	if errors.As(err, &ce) && ce.Hint != "" {
		fmt.Fprintf(w, "hint:  %s\n", ce.Hint)
	}
}
