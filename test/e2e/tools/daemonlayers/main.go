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
// What the daemon kept underneath that wrapper depends on the storage driver:
// with the containerd snapshotter it is the published blob itself, and with the
// classic docker load it is the tar, because the daemon undid a compression it
// recognises. Both are printed as uncompressedkind so a phase can accept either
// without accepting "the blob came back as it was published", which is the one
// thing that would mean the premise is gone.
//
// Output is one line per layer, field=value, so a shell can grep it:
//
//	layer=2 mediatype=application/vnd.docker.image.rootfs.diff.tar.gzip compressed=1f8b0800 uncompressed=28b52ffd uncompressedkind=zstd
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
		raw, kind := sniff(l.Uncompressed)
		fmt.Printf("layer=%d mediatype=%s compressed=%s uncompressed=%s uncompressedkind=%s\n",
			i, mt, head(l.Compressed), raw, kind)
	}
	return nil
}

// sniff returns the first four bytes of a representation in hex and the name of
// what those bytes start: a compression the backup could have used, or the tar
// itself. A tar is recognised by the ustar magic at offset 257, which is why
// this reads further than head does.
func sniff(open func() (io.ReadCloser, error)) (string, string) {
	r, err := open()
	if err != nil {
		return "err", "err"
	}
	defer r.Close()
	buf := make([]byte, 512)
	n, err := io.ReadFull(r, buf)
	if err != nil && n == 0 {
		return "empty", "empty"
	}
	buf = buf[:n]
	first := buf
	if len(first) > 4 {
		first = first[:4]
	}
	return fmt.Sprintf("%x", first), kindOf(buf)
}

func kindOf(b []byte) string {
	const ustarOffset = 257
	if len(b) >= ustarOffset+5 && string(b[ustarOffset:ustarOffset+5]) == "ustar" {
		return "tar"
	}
	for _, m := range []struct {
		name  string
		magic []byte
	}{
		{"gzip", []byte{0x1f, 0x8b}},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
		{"xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}},
		{"lz4", []byte{0x04, 0x22, 0x4d, 0x18}},
	} {
		if len(b) >= len(m.magic) && string(b[:len(m.magic)]) == string(m.magic) {
			return m.name
		}
	}
	return "other"
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
