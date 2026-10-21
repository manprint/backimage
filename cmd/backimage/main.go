package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/manprint/backimage/internal/cli"
)

// signalGrace is how long a run has, after the first SIGINT or SIGTERM, to
// wind down on its own before the process ends anyway.
const signalGrace = 5 * time.Second

func main() {
	os.Exit(mainExit(os.Args[1:]))
}

// mainExit runs one command and returns the process exit status.
//
// SIGINT and SIGTERM cancel the run's context instead of killing the
// process, so every command winds down the way it does on any other error —
// restore removes the tar it had started — and exits 7 rather than 130/143.
// A step that does not watch the context, such as opening a passphrase FIFO
// nobody writes, would hold the process forever, and the handlers of backup
// and listen-remote swallow later signals too: a second signal, or the grace
// period running out, ends the process at once with exit 7.
func mainExit(args []string) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go windDown(signals, cancel, signalGrace, func() {
		fmt.Fprintln(os.Stderr, "error: interrupted")
		os.Exit(int(cli.KindInterrupted))
	})
	err := cli.Execute(ctx, args)
	if err == nil {
		return 0
	}
	cli.ReportError(err)
	return cli.ExitCodeFor(err)
}

// windDown cancels the run on the first signal and calls exit on the second,
// or once grace has passed without the run ending first.
func windDown(signals <-chan os.Signal, cancel context.CancelFunc, grace time.Duration, exit func()) {
	<-signals
	cancel()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-signals:
	case <-timer.C:
	}
	exit()
}
