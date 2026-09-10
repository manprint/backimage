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
// With -swap it plays a second, narrower attacker: one who moves a validly
// sealed chunk to another position. A convergent nonce deliberately leaves the
// chunk index out of the authenticated data, so that blob opens cleanly where
// it does not belong and only the plaintext digest in the sealed private blob
// disagrees. That is the fixture the phase A3 e2e needs.
//
// With -set-envelope-version, -set-nonce-mode and -graft-chunks it plays the
// A05/A20 attacker: someone who rewrites the public metadata — the fields a
// reader used to trust for planning, and the chunk table that carries no
// signature of its own — while the sealed blobs stay exactly as they were.
//
// With -downgrade-key it plays the opposite half of A05: the key file is
// rewrapped with the same passphrase around key material whose attestation
// says something else, which is what a backup written by an older release
// looks like from the outside.
//
// It exists for the phase A1, A3 and A6 e2e only; it is not part of the
// shipped CLI.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	layout          string
	root            string
	passphraseFile  string
	forge           string
	swap            string
	envelopeVersion int
	nonceMode       string
	graftChunks     string
	downgradeKey    string
	outLayout       string
	outRef          string
	push            string
	daemon          string
}

func run() error {
	var o options
	flag.StringVar(&o.layout, "layout", "", "source OCI layout directory produced by `backimage backup --output oci-layout`")
	flag.StringVar(&o.root, "root", "", "directory the image filesystem is unpacked into (mandatory)")
	flag.StringVar(&o.passphraseFile, "passphrase-file", "", "file holding the backup passphrase")
	flag.StringVar(&o.forge, "forge", "", "comma separated list of blobs to strip: data,index,private")
	flag.StringVar(&o.swap, "swap", "", "move the stored blob of chunk J onto chunk I, as `I:J`: a validly sealed chunk in the wrong place")
	flag.IntVar(&o.envelopeVersion, "set-envelope-version", -1, "rewrite manifest.encryption.envelopeVersion, leaving the age key file untouched")
	flag.StringVar(&o.nonceMode, "set-nonce-mode", "", "rewrite manifest.encryption.nonceMode, leaving the age key file untouched")
	flag.StringVar(&o.graftChunks, "graft-chunks", "", "replace chunks.json with this file, taken from another backup")
	flag.StringVar(&o.downgradeKey, "downgrade-key", "", "rewrap keys.pass.age around a weaker attestation: legacy|epoch|nonce|single-use")
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

	// Public metadata rewrites: no key needed, which is the point — they are
	// what anybody who can push a tag can do.
	if o.envelopeVersion >= 0 {
		m.Encryption.EnvelopeVersion = o.envelopeVersion
	}
	if o.nonceMode != "" {
		m.Encryption.NonceMode = o.nonceMode
	}
	if o.graftChunks != "" {
		grafted, graftErr := os.Open(o.graftChunks)
		if graftErr != nil {
			return graftErr
		}
		table, graftErr = index.ReadChunkTable(grafted)
		grafted.Close()
		if graftErr != nil {
			return graftErr
		}
	}

	if o.downgradeKey != "" {
		if err := downgradeKeyFile(backupDir, o.passphraseFile, o.downgradeKey); err != nil {
			return err
		}
	}

	targets := parseTargets(o.forge)
	if len(targets) > 0 || o.swap != "" {
		km, err := unwrap(backupDir, o.passphraseFile)
		if err != nil {
			return err
		}
		defer km.Wipe()
		if len(targets) > 0 {
			if err := forge(backupDir, m, table, km, targets); err != nil {
				return err
			}
		}
		if o.swap != "" {
			if err := swapChunk(backupDir, m, table, km, o.swap); err != nil {
				return err
			}
		}
	}
	// The attacker modelled here holds the key whenever a passphrase was
	// given, so the sealed binding of A6.3 is not what stops them: they can
	// re-seal it over the metadata they just repaired. Doing so keeps each
	// fixture measuring the rule it was written for instead of stopping at
	// the binding. Without a key, the binding is left as it is — and then it
	// is the thing that refuses, which is the point of A6.3.
	if o.passphraseFile != "" && !targets["private"] {
		if err := resealBinding(backupDir, m, table, o.passphraseFile); err != nil {
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

// resealBinding recomputes the binding inside the private blob over the
// metadata as it now stands, and re-seals it with the backup key.
//
// It is a no-op on a backup that has no private blob, or whose private blob
// carries no binding (every format before 0.4.1).
func resealBinding(dir string, m *index.Manifest, t *index.ChunkTable, passphraseFile string) error {
	if m.Private == nil {
		return nil
	}
	km, err := unwrap(dir, passphraseFile)
	if err != nil {
		return err
	}
	defer km.Wipe()
	opener, err := crypt.NewKeyedOpener(km)
	if err != nil {
		return err
	}
	blob, err := os.ReadFile(filepath.Join(dir, m.Private.Path))
	if err != nil {
		return err
	}
	private, err := index.ReadPrivate(bytes.NewReader(blob), opener)
	if err != nil {
		return err
	}
	if private.Binding == nil {
		return nil
	}
	indexBlob, err := os.ReadFile(filepath.Join(dir, m.Index.Path))
	if err != nil {
		return err
	}
	binding, err := index.NewBinding(m, t, indexBlob)
	if err != nil {
		return err
	}
	private.Binding = binding
	mode := crypt.NonceRandom
	if m.Encryption.NonceMode == "convergent" {
		mode = crypt.NonceConvergent
	}
	sealer, err := crypt.NewSealer(km, mode)
	if err != nil {
		return err
	}
	var sealed bytes.Buffer
	if err := index.WritePrivate(&sealed, private, sealer); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, m.Private.Path), sealed.Bytes(), 0o600); err != nil {
		return err
	}
	m.Private.StoredSha256 = restamp(m.Private.StoredSha256, digestOf(sealed.Bytes()))
	return nil
}

// downgradeKeyFile rewrites keys.pass.age around key material that attests
// something weaker than what the run actually did. The secrets are untouched:
// only the attestation moves, so every blob of the backup still opens. That is
// the point — the next --dedup run must refuse to seal again with it, and the
// refusal has to come from here and not from the public manifest.
func downgradeKeyFile(dir, passphraseFile, mode string) error {
	km, err := unwrap(dir, passphraseFile)
	if err != nil {
		return err
	}
	defer km.Wipe()
	switch mode {
	case "legacy":
		km.SchemaVersion = 1
		km.EnvelopeVersion, km.NonceMode, km.Reuse = 0, "", ""
	case "epoch":
		km.EnvelopeVersion = crypt.EnvelopeVersion - 1
	case "nonce":
		km.NonceMode = crypt.NonceModeName(crypt.NonceRandom)
	case "single-use":
		km.Reuse = crypt.ReuseNever
	default:
		return fmt.Errorf("unknown -downgrade-key mode %q", mode)
	}
	pass, err := os.ReadFile(passphraseFile)
	if err != nil {
		return err
	}
	pass = []byte(strings.TrimRight(string(pass), "\r\n"))
	var wrapped strings.Builder
	if err := crypt.WrapKeys(&wrapped, km, crypt.Recipients{Passphrase: pass}); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "keys.pass.age"), []byte(wrapped.String()), 0o600)
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

// swapChunk moves the stored bytes of chunk src onto the slot of chunk dst.
//
// This is the reuse a convergent nonce makes possible on purpose: the chunk
// index is left out of the authenticated data so equal payloads dedup, which
// also means a blob sealed at one position opens cleanly at another. The AEAD
// tag verifies, the stored size matches, and the public stored digest is
// fixed up here the way anybody rewriting chunks.json would. The only thing
// that still disagrees is the plaintext digest kept in the sealed private
// blob, which no one without the key can rewrite.
//
// It prints the plaintext offset at which a restore must stop, so the caller
// can assert that not one byte of the substituted chunk reached the output.
func swapChunk(dir string, m *index.Manifest, t *index.ChunkTable, km *crypt.KeyMaterial, spec string) error {
	dst, src, err := parseSwap(spec, len(t.Chunks))
	if err != nil {
		return err
	}
	target, source := t.Chunks[dst], t.Chunks[src]
	if target.Sb != source.Sb {
		return fmt.Errorf("chunk %d stores %d bytes and chunk %d stores %d: the swap needs them equal",
			dst, target.Sb, src, source.Sb)
	}
	if target.Ss == source.Ss {
		return fmt.Errorf("chunks %d and %d hold the same stored bytes: swapping them proves nothing", dst, src)
	}

	offsets := blobOffsets(t)
	sourceFile := filepath.Join(dir, strings.TrimPrefix(source.P, "backup/"))
	sourceBlob, err := os.ReadFile(sourceFile)
	if err != nil {
		return err
	}
	if offsets[src]+source.Sb > int64(len(sourceBlob)) {
		return fmt.Errorf("chunk %d runs past the end of %s", src, source.P)
	}
	moved := append([]byte(nil), sourceBlob[offsets[src]:offsets[src]+source.Sb]...)

	targetFile := filepath.Join(dir, strings.TrimPrefix(target.P, "backup/"))
	targetBlob, err := os.ReadFile(targetFile)
	if err != nil {
		return err
	}
	if offsets[dst]+target.Sb > int64(len(targetBlob)) {
		return fmt.Errorf("chunk %d runs past the end of %s", dst, target.P)
	}
	copy(targetBlob[offsets[dst]:offsets[dst]+target.Sb], moved)

	// The blob file is named after its own digest, so rewriting its content
	// renames it and repoints every chunk row that lived in it.
	newDigest := digestOf(targetBlob)
	newPath := "backup/data/" + strings.TrimPrefix(newDigest, "sha256:") + ".blob"
	if err := os.Remove(targetFile); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, strings.TrimPrefix(newPath, "backup/")), targetBlob, 0o644); err != nil {
		return err
	}
	oldPath := target.P
	for i := range t.Chunks {
		if t.Chunks[i].P == oldPath {
			t.Chunks[i].P = newPath
		}
	}
	t.Chunks[dst].Ss = source.Ss
	for li := range m.Layers {
		if t.Chunks[m.Layers[li].ChunkFrom].P == newPath {
			m.Layers[li].Digest = newDigest
		}
	}

	stop, err := plaintextOffset(dir, km, dst)
	if err != nil {
		return err
	}
	fmt.Println(stop)
	return nil
}

func parseSwap(spec string, chunks int) (dst, src int, err error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("--swap wants %q, got %q", "I:J", spec)
	}
	if dst, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, fmt.Errorf("--swap %q: %w", spec, err)
	}
	if src, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, fmt.Errorf("--swap %q: %w", spec, err)
	}
	if dst < 0 || dst >= chunks || src < 0 || src >= chunks || dst == src {
		return 0, 0, fmt.Errorf("--swap %q: indexes outside a table of %d chunks", spec, chunks)
	}
	return dst, src, nil
}

// blobOffsets gives the byte offset of every chunk inside its own layer blob.
func blobOffsets(t *index.ChunkTable) []int64 {
	out := make([]int64, len(t.Chunks))
	seen := make(map[string]int64, len(t.Chunks))
	for i, c := range t.Chunks {
		out[i] = seen[c.P]
		seen[c.P] += c.Sb
	}
	return out
}

// plaintextOffset is where chunk index starts in the reconstructed tar, read
// from the sealed private blob because a schema 2 backup keeps plain sizes
// out of chunks.json.
func plaintextOffset(dir string, km *crypt.KeyMaterial, chunkIndex int) (int64, error) {
	opener, err := crypt.NewKeyedOpener(km)
	if err != nil {
		return 0, err
	}
	f, err := os.Open(filepath.Join(dir, index.PrivatePath))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	private, err := index.ReadPrivate(f, opener)
	if err != nil {
		return 0, err
	}
	if chunkIndex > len(private.Chunks) {
		return 0, fmt.Errorf("private metadata describes %d chunks, need %d", len(private.Chunks), chunkIndex)
	}
	var total int64
	for _, c := range private.Chunks[:chunkIndex] {
		total += c.Pb
	}
	return total, nil
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
