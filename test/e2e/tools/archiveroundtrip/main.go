// archiveroundtrip materialises a backimage round-trip from the command line:
// archives SRC into TAR, then extracts TAR into DST.
//
// Usage: archiveroundtrip SRC TAR DST
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/fpierri/backimage/pkg/archive"
)

func main() {
	flag.Parse()
	if flag.NArg() != 3 {
		fmt.Fprintln(os.Stderr, "usage: archiveroundtrip SRC TAR DST")
		os.Exit(2)
	}
	src, tarPath, dst := flag.Arg(0), flag.Arg(1), flag.Arg(2)

	out, err := os.Create(tarPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create tar: %v\n", err)
		os.Exit(1)
	}

	w := archive.NewWriter(out, archive.Options{PreserveXattrs: true})
	if err := w.AddRoot(context.Background(), src); err != nil {
		fmt.Fprintf(os.Stderr, "archive: %v\n", err)
		os.Exit(1)
	}
	stats, err := w.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive close: %v\n", err)
		os.Exit(1)
	}
	if err := out.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close: %v\n", err)
		os.Exit(1)
	}

	f, err := os.Open(tarPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open tar: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	x := archive.NewExtractor(archive.ExtractOptions{
		PreserveXattrs: true,
		PreserveOwner:  true,
		Strict:         true,
	})
	xs, err := x.Extract(context.Background(), f, dst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "extract: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("archive: files=%d dirs=%d symlinks=%d hardlinks=%d devices=%d fifos=%d skipped=%d bytes=%d\n",
		stats.Files, stats.Dirs, stats.Symlinks, stats.Hardlinks, stats.Devices, stats.Fifos, stats.Skipped, stats.BytesRaw)
	fmt.Printf("extract: files=%d dirs=%d symlinks=%d hardlinks=%d devices=%d fifos=%d skipped=%d bytes=%d\n",
		xs.Files, xs.Dirs, xs.Symlinks, xs.Hardlinks, xs.Devices, xs.Fifos, xs.Skipped, xs.BytesRaw)
}
