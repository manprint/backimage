package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/manprint/backimage/internal/buildinfo"
)

var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

func main() {
	os.Exit(mainExit(os.Args[1:]))
}

// signalGrace is how long a run has, after the first SIGINT or SIGTERM, to
// wind down on its own before the process ends anyway.
const signalGrace = 5 * time.Second

// mainExit runs one command and returns the process exit status.
//
// SIGINT and SIGTERM cancel the run instead of ending the process: the
// extraction stops between entries and reports how it ended, and the exit
// status is the documented 7 rather than the 130/143 of a killed process. In
// a container this binary is PID 1, and the kernel delivers no default action
// to PID 1 at all: without a handler `docker stop` waits out its timeout and
// falls back to SIGKILL. A read that does not watch the context — a blob or
// a passphrase FIFO nobody writes, a terminal prompt — would hold the process
// just the same, so a second signal, or the grace period running out, ends it
// at once with exit 7.
func mainExit(args []string) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go windDown(signals, cancel, signalGrace, func() {
		fmt.Fprintln(stderr, "errore: interrotto")
		os.Exit(exitInterrupted)
	})
	err := interrupted(ctx, run(ctx, args))
	if err != nil {
		fmt.Fprintln(stderr, "errore:", err)
	}
	return exitCode(err)
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

// interrupted reports a run that failed after ctx was cancelled as
// interrupted, whatever the command made of the cancellation: a truncated
// stream is a consequence of the signal, not damage in the backup.
func interrupted(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	return withCode(exitInterrupted, fmt.Errorf("interrotto: %w", err))
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"info"}
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		usage(stdout)
		return nil
	}
	if strings.HasPrefix(args[0], "-") {
		return cmdInfo(ctx, args)
	}
	switch args[0] {
	case "version":
		return cmdVersion(args[1:])
	case "info":
		return cmdInfo(ctx, args[1:])
	case "list", "ls":
		return cmdList(ctx, args[1:])
	case "tar":
		return cmdTar(ctx, args[1:])
	case "extract":
		return cmdExtract(ctx, args[1:])
	case "verify":
		return cmdVerify(ctx, args[1:])
	default:
		return usageErrorf("operazione sconosciuta %q; usa --help", args[0])
	}
}

// cmdVersion prints the build identity of this extractor. It exists so the
// image can be asked what it carries without a passphrase, and so the -X
// stamps in LDFLAGS_EMBED reference a symbol that is actually linked in: an
// unreferenced buildinfo would make the linker drop them without a word, and
// internal/embedded's coeval-asset test would have nothing to compare.
func cmdVersion(args []string) error {
	if len(args) > 0 {
		return usageErrorf("version does not take arguments")
	}
	fmt.Fprintln(stdout, buildinfo.String())
	return nil
}

func usage(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprint(w, `backimage self-extracting backup

Usage:
  docker run --rm IMAGE [command] [flags]

Commands:
  info                 show public backup metadata (default, no passphrase needed)
  version              show the build identity of this extractor
  list                 list archived files
  tar                  write the plaintext tar archive to stdout
  extract              extract files to a directory
  verify               check the integrity of every blob

Common flags:
  --root DIR           backup root (default /backup)
  --passphrase-stdin   read the passphrase from stdin
  --passphrase-file F  read the passphrase from file F
  --identity F         age private key file
  --allow-unencrypted  accept an unencrypted backup even with a credential given

Extract flags:
  --cpus N             maximum CPUs used during extraction (default: half available CPUs)

Image cleanup is a host operation: this binary carries no Docker client and
never asks for the daemon socket. Use "backimage restore --remove-local-image"
on the host that pulled the image.

The passphrase is also read from $BACKIMAGE_PASSPHRASE, or prompted on the
controlling terminal when required.
`)
}
