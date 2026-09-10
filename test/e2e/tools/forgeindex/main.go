// Command forgeindex rewrites the file index of an unencrypted backup into a
// shape its readers have always assumed and never used to verify.
//
// The shapes are not corruption: every one of them is well-formed JSON, with
// valid types and plausible values, and every one of them breaks something a
// reader does downstream. Two entries with the same path make the selection
// of a file ambiguous; tar offsets that do not grow break the partial
// recovery, which computes the end of an entry from the start of the next
// one; a path longer than a filesystem will accept cannot be restored and is
// only there to be allocated.
//
// It writes the index the way the writer would refuse to: index.WriteIndex
// validates the entries, so the blob is assembled here from the raw JSON.
// That is the point — the fixture has to be something the writer of this
// tree would never produce, and a reader has to refuse it anyway.
//
// It exists for the phase A7 e2e only; it is not part of the shipped CLI.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/manprint/backimage/pkg/index"
)

func main() {
	root := flag.String("root", "", "directory holding the backup tree (manifest.json, chunks.json, index blob)")
	shape := flag.String("shape", "", "duplicate-path, backwards-offsets or long-path")
	flag.Parse()
	if *root == "" || *shape == "" {
		fmt.Fprintln(os.Stderr, "usage: forgeindex -root DIR -shape SHAPE")
		os.Exit(2)
	}
	if err := run(*root, *shape); err != nil {
		fmt.Fprintln(os.Stderr, "forgeindex:", err)
		os.Exit(1)
	}
}

func run(root, shape string) error {
	manifestFile, err := os.Open(filepath.Join(root, "manifest.json"))
	if err != nil {
		return err
	}
	m, err := index.ReadManifest(manifestFile)
	manifestFile.Close()
	if err != nil {
		return err
	}
	if m.Encryption.Enabled {
		return errors.New("this tool only rewrites the plaintext index of an unencrypted backup")
	}
	path := filepath.Join(root, filepath.FromSlash(m.Index.Path))
	blob, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	idx, err := decode(blob)
	if err != nil {
		return err
	}
	if len(idx.Entries) < 2 {
		return fmt.Errorf("the index has %d entries, the shapes below need at least two", len(idx.Entries))
	}
	if err := forge(idx, shape); err != nil {
		return err
	}
	encoded, err := encode(idx)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return err
	}
	fmt.Printf("forged %s: %s\n", m.Index.Path, shape)
	return nil
}

func forge(idx *index.Index, shape string) error {
	switch shape {
	case "duplicate-path":
		idx.Entries[1].Path = idx.Entries[0].Path
	case "backwards-offsets":
		idx.Entries[1].TarOffset = idx.Entries[0].TarOffset
	case "long-path":
		idx.Entries[0].Path = "deep/" + strings.Repeat("a", 8192)
	default:
		return fmt.Errorf("unknown shape %q", shape)
	}
	return nil
}

// decode reads the index blob of an unencrypted backup, which is a bare zstd
// frame: nothing to unseal, and no key to unseal it with.
func decode(blob []byte) (*index.Index, error) {
	zr, err := zstd.NewReader(bytes.NewReader(blob), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	idx := &index.Index{}
	if err := json.Unmarshal(plain, idx); err != nil {
		return nil, err
	}
	return idx, nil
}

func encode(idx *index.Index) ([]byte, error) {
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(zw).Encode(idx); err != nil {
		zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
