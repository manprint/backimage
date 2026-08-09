package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(stderr, "errore:", err)
		os.Exit(exitCode(err))
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"info"}
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		usage(stdout)
		return nil
	}
	if args[0][0] == '-' {
		return cmdInfo(ctx, args)
	}
	switch args[0] {
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

func usage(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprint(w, `backimage self-extracting backup

Usage:
  docker run --rm IMAGE [command] [flags]

Commands:
  info                 show public backup metadata (default, no passphrase needed)
  list                 list archived files
  tar                  write the plaintext tar archive to stdout
  extract              extract files to a directory
  verify               check the integrity of every blob

Common flags:
  --root DIR           backup root (default /backup)
  --passphrase-stdin   read the passphrase from stdin
  --passphrase-file F  read the passphrase from file F
  --identity F         age private key file

The passphrase is also read from $BACKIMAGE_PASSPHRASE, or prompted on the
controlling terminal when required.
`)
}
