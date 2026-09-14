// Command daemonlayers prints, for every layer of an image in the local
// container daemon, the media type the daemon advertises and the first bytes
// of each representation it can serve.
//
// It exists so an e2e phase can assert the premise the reader is built on
// instead of assuming it: the daemon does not hand a layer back as it was
// published. It re-labels every one of them tar+gzip and gzips what it kept,
// so the blob a backup wrote comes back with one wrapper too many. Reading it
// as the published blob is what made `restore --local-repo` fail on every
// backup (plan/astra/bugs.md, B-A001).
//
// Output is one line per layer, field=value, so a shell can grep it:
//
//	layer=2 mediatype=application/vnd.docker.image.rootfs.diff.tar.gzip compressed=1f8b0800 uncompressed=28b52ffd
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: daemonlayers IMAGE")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "daemonlayers:", err)
		os.Exit(1)
	}
}

func run(ref string) error {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return err
	}
	img, err := daemon.Image(parsed)
	if err != nil {
		return err
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	for i, l := range layers {
		mt, err := l.MediaType()
		if err != nil {
			return err
		}
		fmt.Printf("layer=%d mediatype=%s compressed=%s uncompressed=%s\n",
			i, mt, head(l.Compressed), head(l.Uncompressed))
	}
	return nil
}

// head returns the first four bytes of a representation in hex, or "err" when
// the daemon cannot produce it at all.
func head(open func() (io.ReadCloser, error)) string {
	r, err := open()
	if err != nil {
		return "err"
	}
	defer r.Close()
	buf := make([]byte, 4)
	n, err := io.ReadFull(r, buf)
	if err != nil && n == 0 {
		return "empty"
	}
	return fmt.Sprintf("%x", buf[:n])
}
