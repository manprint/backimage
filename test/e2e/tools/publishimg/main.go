// Command publishimg assembles a two-platform image (three fake data layers
// and a fake /backimage entrypoint) and pushes it to a registry. It exists
// for the phase 04 e2e only; it is not part of the shipped CLI.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/ociimg"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: publishimg <ref> (e.g. localhost:5000/test/img:v1)")
		os.Exit(2)
	}
	ref, err := name.ParseReference(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	codec, err := compress.ByID(compress.Zstd)
	if err != nil {
		panic(err)
	}

	manifest := &index.Manifest{
		SchemaVersion: 1,
		Tool:          index.ToolInfo{Name: "backimage", Version: "0.4.0"},
		CreatedAt:     time.Now().UTC(),
		Sources:       []string{"/fake/data"},
		Totals:        index.Totals{Files: 3, Dirs: 1, BytesRaw: 15},
		Archive:       index.ArchiveInfo{Format: "tar", Compression: "zstd", CompressionLevel: 1},
		Encryption:    index.EncryptionInfo{Enabled: false, KDF: "", AEAD: "none", NonceMode: "random"},
		Chunking:      index.ChunkingInfo{Strategy: "length", TargetChunkBytes: 1 << 20, Count: 3},
		Layers: []index.LayerInfo{
			{Digest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", ChunkFrom: 0, ChunkTo: 2},
		},
		Index: index.Ref{Path: "index.json.zst", Encrypted: false},
	}
	chunks := &index.ChunkTable{
		SchemaVersion: 1,
		Chunks: []index.Chunk{
			{I: 0, P: "backup/data/000000.blob", Ps: strings.Repeat("0", 64), Ss: strings.Repeat("1", 64), Pb: 5, Sb: 5},
			{I: 1, P: "backup/data/000001.blob", Ps: strings.Repeat("2", 64), Ss: strings.Repeat("3", 64), Pb: 5, Sb: 5},
			{I: 2, P: "backup/data/000002.blob", Ps: strings.Repeat("4", 64), Ss: strings.Repeat("5", 64), Pb: 5, Sb: 5},
		},
	}

	var data []v1.Layer
	for i := 0; i < 3; i++ {
		b := fmt.Sprintf("chunk-%d-body", i)
		data = append(data, mustLayer(&ociimg.LayerFile{
			Path: fmt.Sprintf("/backup/data/00000%d.blob", i),
			Mode: 0o644,
			Size: int64(len(b)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(b)), nil },
		}, codec))
	}

	fake := []byte("#!/bin/sh\necho fake-restore\n")
	imgs := make([]ociimg.BuiltImage, 0, 2)
	for _, arch := range []string{"amd64", "arm64"} {
		img, err := ociimg.BuildImage(ociimg.BuildOptions{
			Platform:    v1.Platform{OS: "linux", Architecture: arch},
			SelfExtract: fake,
			Runnable:    true,
			Manifest:    manifest,
			ChunkTable:  chunks,
			IndexBlob:   []byte("fake-index"),
			DataLayers:  data,
			Codec:       codec,
			Created:     "2026-01-01T00:00:00Z",
		})
		if err != nil {
			panic(err)
		}
		imgs = append(imgs, ociimg.BuiltImage{Platform: v1.Platform{OS: "linux", Architecture: arch}, Image: img})
	}
	idx, err := ociimg.BuildIndex(imgs)
	if err != nil {
		panic(err)
	}

	w, err := ociimg.NewWriter(ociimg.TargetRegistry, "", ociimg.WriterOptions{})
	if err != nil {
		panic(err)
	}
	if err := w.Write(context.Background(), ref, idx, nil); err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	d, err := idx.Digest()
	if err != nil {
		fmt.Fprintln(os.Stderr, "digest:", err)
		os.Exit(1)
	}
	fmt.Println("pushed", ref.Name(), d.String())
}

func mustLayer(f *ociimg.LayerFile, codec compress.Codec) v1.Layer {
	l, err := ociimg.NewLayer([]ociimg.LayerFile{*f}, codec, 1)
	if err != nil {
		panic(err)
	}
	return l
}
