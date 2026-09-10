// Command forgeclear rewrites an encrypted backup so that some of its blobs
// carry no authentication tag at all: the AEAD field of the envelope becomes
// "none" and the payload travels in the clear.
//
// It models the A01 attacker: someone who can rewrite the bytes of an image
// but does not know the key. Everything a reader can check without the key is
// made consistent again — chunk sizes, stored digests, blob file names, layer
// metadata — so the only thing left that can refuse the backup is the rule
// that an encrypted backup has no unauthenticated blobs.
//
// It exists for the phase A1 e2e only; it is not part of the shipped CLI.
package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/ociimg"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forgeclear:", err)
		os.Exit(1)
	}
}

type options struct {
	layout         string
	root           string
	passphraseFile string
	forge          string
	outLayout      string
	outRef         string
	push           string
	daemon         string
}

func run() error {
	var o options
	flag.StringVar(&o.layout, "layout", "", "source OCI layout directory produced by `backimage backup --output oci-layout`")
	flag.StringVar(&o.root, "root", "", "directory the image filesystem is unpacked into (mandatory)")
	flag.StringVar(&o.passphraseFile, "passphrase-file", "", "file holding the backup passphrase")
	flag.StringVar(&o.forge, "forge", "", "comma separated list of blobs to strip: data,index,private")
	flag.StringVar(&o.outLayout, "out-layout", "", "write the forged image to this OCI layout directory")
	flag.StringVar(&o.outRef, "out-ref", "", "reference used for --out-layout")
	flag.StringVar(&o.push, "push", "", "push the forged image to this registry reference")
	flag.StringVar(&o.daemon, "daemon", "", "load the forged image into the local Docker daemon under this reference")
	flag.Parse()

	if o.layout == "" || o.root == "" {
		return fmt.Errorf("--layout and --root are mandatory")
	}
	ctx := context.Background()

	img, err := firstImage(o.layout)
	if err != nil {
		return err
	}
	if err := unpack(img, o.root); err != nil {
		return err
	}

	backupDir := filepath.Join(o.root, "backup")
	m, table, err := readMeta(backupDir)
	if err != nil {
		return err
	}

	targets := parseTargets(o.forge)
	if len(targets) > 0 {
		km, err := unwrap(backupDir, o.passphraseFile)
		if err != nil {
			return err
		}
		defer km.Wipe()
		if err := forge(backupDir, m, table, km, targets); err != nil {
			return err
		}
	}
	if err := writeMeta(backupDir, m, table); err != nil {
		return err
	}

	if o.outLayout == "" && o.push == "" && o.daemon == "" {
		return nil
	}
	forged, img, err := rebuild(o.root, m, table)
	if err != nil {
		return err
	}
	images := map[string]v1.Image{"linux/amd64": img}
	if o.outLayout != "" {
		if err := write(ctx, ociimg.TargetOCILayout, o.outLayout, o.outRef, forged, images); err != nil {
			return err
		}
	}
	if o.push != "" {
		if err := write(ctx, ociimg.TargetRegistry, "", o.push, forged, images); err != nil {
			return err
		}
	}
	if o.daemon != "" {
		if err := write(ctx, ociimg.TargetDaemon, "", o.daemon, forged, images); err != nil {
			return err
		}
	}
	return nil
}

// firstImage returns the linux/amd64 image of the layout, or the only image
// when the index carries no platform.
func firstImage(dir string) (v1.Image, error) {
	idx, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		return nil, fmt.Errorf("reading layout %s: %w", dir, err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	for _, d := range im.Manifests {
		if d.MediaType.IsIndex() {
			child, err := idx.ImageIndex(d.Digest)
			if err != nil {
				return nil, err
			}
			cm, err := child.IndexManifest()
			if err != nil {
				return nil, err
			}
			for _, cd := range cm.Manifests {
				if cd.Platform == nil || (cd.Platform.OS == "linux" && cd.Platform.Architecture == "amd64") {
					return child.Image(cd.Digest)
				}
			}
			continue
		}
		if d.Platform == nil || (d.Platform.OS == "linux" && d.Platform.Architecture == "amd64") {
			return idx.Image(d.Digest)
		}
	}
	return nil, fmt.Errorf("no linux/amd64 image in %s", dir)
}

// unpack writes the flattened filesystem of img under dir, so that dir/backup
// is a backup root a self-extracting binary can read with --root.
func unpack(img v1.Image, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rc := mutate.Extract(img)
	defer rc.Close()
	return extractTar(rc, dir)
}

func readMeta(dir string) (*index.Manifest, *index.ChunkTable, error) {
	mf, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, nil, err
	}
	defer mf.Close()
	m, err := index.ReadManifest(mf)
	if err != nil {
		return nil, nil, err
	}
	if !m.Encryption.Enabled {
		return nil, nil, fmt.Errorf("the source backup is not encrypted: nothing to downgrade")
	}
	cf, err := os.Open(filepath.Join(dir, "chunks.json"))
	if err != nil {
		return nil, nil, err
	}
	defer cf.Close()
	t, err := index.ReadChunkTable(cf)
	if err != nil {
		return nil, nil, err
	}
	return m, t, nil
}

func writeMeta(dir string, m *index.Manifest, t *index.ChunkTable) error {
	mf, err := os.Create(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	if err := index.WriteManifest(mf, m); err != nil {
		mf.Close()
		return err
	}
	if err := mf.Close(); err != nil {
		return err
	}
	cf, err := os.Create(filepath.Join(dir, "chunks.json"))
	if err != nil {
		return err
	}
	if err := index.WriteChunkTable(cf, t); err != nil {
		cf.Close()
		return err
	}
	return cf.Close()
}

func parseTargets(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out[part] = true
		}
	}
	return out
}

func unwrap(dir, passphraseFile string) (*crypt.KeyMaterial, error) {
	if passphraseFile == "" {
		return nil, fmt.Errorf("--passphrase-file is required to rewrite blobs")
	}
	pass, err := os.ReadFile(passphraseFile)
	if err != nil {
		return nil, err
	}
	pass = []byte(strings.TrimRight(string(pass), "\r\n"))
	f, err := os.Open(filepath.Join(dir, "keys.pass.age"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return crypt.UnwrapKeys(f, crypt.Identity{Passphrase: pass})
}

// forge rewrites the requested blobs as clear envelopes and repairs every
// public number that describes them.
func forge(dir string, m *index.Manifest, t *index.ChunkTable, km *crypt.KeyMaterial, targets map[string]bool) error {
	opener, err := crypt.NewKeyedOpener(km)
	if err != nil {
		return err
	}
	clear, err := crypt.NewSealer(nil, crypt.NonceRandom)
	if err != nil {
		return err
	}
	for kind := range targets {
		switch kind {
		case "data":
			if err := forgeData(dir, m, t, opener, clear); err != nil {
				return err
			}
		case "index":
			ref, err := forgeBlob(dir, m.Index.Path, crypt.RoleIndex, opener, clear)
			if err != nil {
				return err
			}
			m.Index.StoredSha256 = restamp(m.Index.StoredSha256, ref)
		case "private":
			if m.Private == nil {
				return fmt.Errorf("the source backup has no private blob")
			}
			ref, err := forgeBlob(dir, m.Private.Path, crypt.RolePrivate, opener, clear)
			if err != nil {
				return err
			}
			m.Private.StoredSha256 = restamp(m.Private.StoredSha256, ref)
		default:
			return fmt.Errorf("unknown forge target %q", kind)
		}
	}
	return nil
}

// forgeData rewrites one data blob per layer. Chunks of a layer are stored
// concatenated in a single file named after the digest of that file, so the
// file is renamed as well and every chunk row is repointed at it.
func forgeData(dir string, m *index.Manifest, t *index.ChunkTable, opener crypt.Opener, clear crypt.Sealer) error {
	for li := range m.Layers {
		layerInfo := &m.Layers[li]
		if layerInfo.ChunkFrom < 0 || layerInfo.ChunkTo >= len(t.Chunks) {
			return fmt.Errorf("layer %d: chunk range %d-%d outside a table of %d",
				layerInfo.Index, layerInfo.ChunkFrom, layerInfo.ChunkTo, len(t.Chunks))
		}
		oldPath := t.Chunks[layerInfo.ChunkFrom].P
		stored, err := os.ReadFile(filepath.Join(dir, strings.TrimPrefix(oldPath, "backup/")))
		if err != nil {
			return err
		}
		var out []byte
		var offset int64
		sizes := make([]int64, 0, layerInfo.ChunkTo-layerInfo.ChunkFrom+1)
		for i := layerInfo.ChunkFrom; i <= layerInfo.ChunkTo; i++ {
			c := t.Chunks[i]
			if c.P != oldPath {
				return fmt.Errorf("chunk %d lives in %q, not in the layer blob %q", i, c.P, oldPath)
			}
			if offset+c.Sb > int64(len(stored)) {
				return fmt.Errorf("chunk %d runs past the end of %s", i, oldPath)
			}
			payload, codecID, err := opener.Open(nil, crypt.RoleData, uint32(i), stored[offset:offset+c.Sb])
			if err != nil {
				return fmt.Errorf("opening chunk %d: %w", i, err)
			}
			codec, err := compress.ByID(codecID)
			if err != nil {
				return err
			}
			sealed, err := clear.Seal(nil, crypt.RoleData, uint32(i), codec, payload)
			if err != nil {
				return err
			}
			out = append(out, sealed...)
			sizes = append(sizes, int64(len(sealed)))
			offset += c.Sb
		}
		newDigest := digestOf(out)
		newPath := "backup/data/" + strings.TrimPrefix(newDigest, "sha256:") + ".blob"
		if err := os.Remove(filepath.Join(dir, strings.TrimPrefix(oldPath, "backup/"))); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, strings.TrimPrefix(newPath, "backup/")), out, 0o644); err != nil {
			return err
		}
		var at int64
		for n, i := 0, layerInfo.ChunkFrom; i <= layerInfo.ChunkTo; n, i = n+1, i+1 {
			t.Chunks[i].P = newPath
			t.Chunks[i].Sb = sizes[n]
			t.Chunks[i].Ss = digestOf(out[at : at+sizes[n]])
			at += sizes[n]
		}
		layerInfo.Digest = newDigest
		layerInfo.StoredBytes = int64(len(out))
	}
	return nil
}

// forgeBlob rewrites one metadata blob in place and returns its new digest.
func forgeBlob(dir, name string, role crypt.Role, opener crypt.Opener, clear crypt.Sealer) (string, error) {
	path := filepath.Join(dir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	payload, codecID, err := opener.Open(nil, role, 0, raw)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", name, err)
	}
	codec, err := compress.ByID(codecID)
	if err != nil {
		return "", err
	}
	sealed, err := clear.Seal(nil, role, 0, codec, payload)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, sealed, 0o644); err != nil {
		return "", err
	}
	return digestOf(sealed), nil
}

// restamp keeps the shape of the digest field the backup already used.
func restamp(old, digest string) string {
	if old == "" {
		return ""
	}
	if strings.HasPrefix(old, "sha256:") {
		return digest
	}
	return strings.TrimPrefix(digest, "sha256:")
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// rebuild assembles a single-platform image from the forged root.
func rebuild(root string, m *index.Manifest, t *index.ChunkTable) (v1.ImageIndex, v1.Image, error) {
	backupDir := filepath.Join(root, "backup")
	selfExtract, err := os.ReadFile(filepath.Join(root, "backimage"))
	if err != nil {
		return nil, nil, err
	}
	codec, err := compress.Get(m.Archive.Compression)
	if err != nil {
		return nil, nil, err
	}
	indexBlob, err := os.ReadFile(filepath.Join(backupDir, m.Index.Path))
	if err != nil {
		return nil, nil, err
	}
	var privateBlob []byte
	if m.Private != nil {
		if privateBlob, err = os.ReadFile(filepath.Join(backupDir, m.Private.Path)); err != nil {
			return nil, nil, err
		}
	}
	keys := map[string][]byte{}
	for _, n := range []string{"keys.age", "keys.pass.age"} {
		data, err := os.ReadFile(filepath.Join(backupDir, n))
		if err == nil {
			keys[n] = data
		} else if !os.IsNotExist(err) {
			return nil, nil, err
		}
	}
	layers := make([]v1.Layer, 0, len(m.Layers))
	for _, li := range m.Layers {
		blobPath := t.Chunks[li.ChunkFrom].P
		data, err := os.ReadFile(filepath.Join(backupDir, strings.TrimPrefix(blobPath, "backup/")))
		if err != nil {
			return nil, nil, err
		}
		l, err := ociimg.NewLayer([]ociimg.LayerFile{{
			Path: "/" + blobPath,
			Mode: 0o644,
			Size: int64(len(data)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(string(data))), nil },
		}}, codec, m.Archive.CompressionLevel)
		if err != nil {
			return nil, nil, err
		}
		layers = append(layers, l)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	img, err := ociimg.BuildImage(ociimg.BuildOptions{
		Platform:    platform,
		SelfExtract: selfExtract,
		Runnable:    true,
		Manifest:    m,
		ChunkTable:  t,
		IndexBlob:   indexBlob,
		PrivateBlob: privateBlob,
		KeyFiles:    keys,
		DataLayers:  layers,
		Codec:       codec,
		Created:     "2026-01-01T00:00:00Z",
	})
	if err != nil {
		return nil, nil, err
	}
	idx, err := ociimg.BuildIndex([]ociimg.BuiltImage{{Platform: platform, Image: img}})
	if err != nil {
		return nil, nil, err
	}
	return idx, img, nil
}

func write(ctx context.Context, target ociimg.Target, path, ref string, idx v1.ImageIndex, images map[string]v1.Image) error {
	if ref == "" {
		return fmt.Errorf("target %s needs a reference", target)
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return err
	}
	w, err := ociimg.NewWriter(target, path, ociimg.WriterOptions{
		Images:  images,
		Runtime: v1.Platform{OS: "linux", Architecture: "amd64"},
	})
	if err != nil {
		return err
	}
	return w.Write(ctx, parsed, idx, nil)
}

// extractTar writes a flattened image tar under dir, ignoring anything that is
// not a directory or a regular file: a backup image has nothing else.
func extractTar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.Clean("/"+h.Name), "/")
		if rel == "" || strings.HasPrefix(rel, "..") {
			continue
		}
		target := filepath.Join(dir, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			// CopyN, not Copy: a forged image is untrusted input and the tar
			// header already says how many bytes the entry has.
			if _, err := io.CopyN(f, tr, h.Size); err != nil && !errors.Is(err, io.EOF) {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}
