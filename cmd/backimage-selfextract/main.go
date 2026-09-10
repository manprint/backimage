package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/manprint/backimage/internal/buildinfo"
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
  --remove-local-image remove the Docker image after successful extraction;
                       requires BACKIMAGE_IMAGE_REF and /var/run/docker.sock

The passphrase is also read from $BACKIMAGE_PASSPHRASE, or prompted on the
controlling terminal when required.
`)
}
