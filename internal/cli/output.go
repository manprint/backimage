package cli

import (
	"encoding/json"
	"fmt"
	"io"
)

// Printer renders results either as human text or as JSON, never both.
type Printer interface {
	// Result prints the final payload of a command.
	Result(v any) error
	// Infof writes progress/diagnostics to stderr; suppressed by --quiet.
	Infof(format string, args ...any)
	// Warnf writes a warning to stderr; never suppressed.
	Warnf(format string, args ...any)
}

type printer struct {
	out    io.Writer
	errOut io.Writer
	json   bool
	quiet  bool
}

// NewPrinter returns a JSON or text printer according to opts.
func NewPrinter(out io.Writer, errOut io.Writer, opts Options) Printer {
	return &printer{out: out, errOut: errOut, json: opts.JSON, quiet: opts.Quiet}
}

func (p *printer) Result(v any) error {
	if p.json {
		out, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("json output: %w", err)
		}
		_, err = fmt.Fprintln(p.out, string(out))
		return err
	}
	_, err := fmt.Fprintln(p.out, v)
	return err
}

func (p *printer) Infof(format string, args ...any) {
	if p.quiet {
		return
	}
	fmt.Fprintf(p.errOut, format+"\n", args...)
}

func (p *printer) Warnf(format string, args ...any) {
	fmt.Fprintf(p.errOut, "warning: "+format+"\n", args...)
}
