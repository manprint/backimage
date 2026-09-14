package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/manprint/backimage/pkg/compress"
)

// blobTar is the shape every data layer has inside: a tar carrying the one
// blob file the chunk table points at.
func blobTar(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func compressWith(t *testing.T, codecName string, raw []byte) []byte {
	t.Helper()
	codec, err := compress.Get(codecName)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, _, level := codec.Levels()
	w, err := codec.NewWriter(&out, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// readBlob runs the peeled stream the way materialize does and returns the
// content of the one file it finds.
func readBlob(t *testing.T, rc io.ReadCloser, name string) []byte {
	t.Helper()
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("looking for %s: %v", name, err)
		}
		if strings.TrimPrefix(h.Name, "/") != strings.TrimPrefix(name, "/") {
			continue
		}
		out, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

// TestOpenLayerTarUndoesExactlyWhatTheSourceAdded is the matrix the defect
// lived in: for every codec a backup can be written with, the reader has to
// cope both with the blob served as published (registry, OCI layout) and with
// the same blob gzipped once more on the way out (local Docker daemon), and
// with a source that already undid everything (a daemon that decompressed the
// layer when it loaded the image and hands back the bare tar).
func TestOpenLayerTarUndoesExactlyWhatTheSourceAdded(t *testing.T) {
	const blobName = "backup/data/000000.blob"
	content := bytes.Repeat([]byte("payload-"), 500)
	plain := blobTar(t, blobName, content)

	for _, codecName := range compress.Names() {
		for _, shape := range []struct {
			name string
			wrap func([]byte) []byte
		}{
			{"as published", func(b []byte) []byte { return b }},
			{"gzipped once more by the daemon", func(b []byte) []byte { return compressWith(t, "gzip", b) }},
		} {
			t.Run(codecName+"/"+shape.name, func(t *testing.T) {
				served := shape.wrap(compressWith(t, codecName, plain))
				rc, err := openLayerTar(io.NopCloser(bytes.NewReader(served)), codecName)
				if err != nil {
					t.Fatalf("openLayerTar: %v", err)
				}
				if got := readBlob(t, rc, blobName); !bytes.Equal(got, content) {
					t.Fatalf("blob came back as %d bytes, want %d", len(got), len(content))
				}
			})
		}
		t.Run(codecName+"/already undone by the source", func(t *testing.T) {
			rc, err := openLayerTar(io.NopCloser(bytes.NewReader(plain)), codecName)
			if err != nil {
				t.Fatalf("openLayerTar: %v", err)
			}
			if got := readBlob(t, rc, blobName); !bytes.Equal(got, content) {
				t.Fatal("a layer already served as a tar must be read as it is")
			}
		})
	}
}

// TestOpenLayerTarKeepsTheGzipBackupUnambiguous: with a gzip backup the
// wrapper and the backup's own codec are the same magic from the outside, so
// the only thing that tells them apart is what is behind the first one.
func TestOpenLayerTarKeepsTheGzipBackupUnambiguous(t *testing.T) {
	const blobName = "backup/data/000000.blob"
	content := []byte("gzip backup content")
	plain := blobTar(t, blobName, content)
	once := compressWith(t, "gzip", plain)
	twice := compressWith(t, "gzip", once)

	for name, served := range map[string][]byte{"one gzip": once, "two gzips": twice} {
		t.Run(name, func(t *testing.T) {
			rc, err := openLayerTar(io.NopCloser(bytes.NewReader(served)), "gzip")
			if err != nil {
				t.Fatalf("openLayerTar: %v", err)
			}
			if got := readBlob(t, rc, blobName); !bytes.Equal(got, content) {
				t.Fatalf("blob = %q, want %q", got, content)
			}
		})
	}
}

// TestDaemonWrappingWasTheOriginalFailure pins the defect itself: read the way
// the source used to read it — the declared codec straight onto the bytes the
// daemon serves — the very same layer fails, and with the very same message
// users reported.
func TestDaemonWrappingWasTheOriginalFailure(t *testing.T) {
	const blobName = "backup/data/000000.blob"
	served := compressWith(t, "gzip", compressWith(t, "zstd", blobTar(t, blobName, []byte("x"))))

	codec, err := compress.Get("zstd")
	if err != nil {
		t.Fatal(err)
	}
	r, err := codec.NewReader(bytes.NewReader(served))
	if err == nil {
		_, err = io.ReadAll(r)
		r.Close()
	}
	if err == nil {
		t.Fatal("applying the declared codec to the daemon's bytes must fail; the fix would be untested otherwise")
	}
	if !strings.Contains(err.Error(), "magic number mismatch") {
		t.Fatalf("the reported symptom was %q, got %v", "magic number mismatch", err)
	}

	rc, err := openLayerTar(io.NopCloser(bytes.NewReader(served)), "zstd")
	if err != nil {
		t.Fatalf("openLayerTar must read the same bytes: %v", err)
	}
	rc.Close()
}

// TestOpenLayerTarRefusesAnUnknownCodec: the codec is named by the manifest and
// is never guessed at, so an unknown one is an error, not a reason to sniff.
func TestOpenLayerTarRefusesAnUnknownCodec(t *testing.T) {
	src := &countingCloser{Reader: bytes.NewReader(blobTar(t, "b", []byte("x")))}
	if _, err := openLayerTar(src, "no-such-codec"); err == nil {
		t.Fatal("want an error for an unknown codec")
	}
	if src.closed.Load() != 1 {
		t.Fatal("the layer must be closed when the chain cannot be built")
	}
}

// TestOpenLayerTarClosesTheWholeChain: every decoder it stacked, and the layer
// underneath, must be released by the single Close the caller makes.
func TestOpenLayerTarClosesTheWholeChain(t *testing.T) {
	served := compressWith(t, "gzip", compressWith(t, "zstd", blobTar(t, "b", []byte("x"))))
	src := &countingCloser{Reader: bytes.NewReader(served)}
	rc, err := openLayerTar(src, "zstd")
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := src.closed.Load(); got != 1 {
		t.Fatalf("layer closed %d times, want 1", got)
	}
}

type countingCloser struct {
	io.Reader
	closed atomic.Int32
}

func (c *countingCloser) Close() error {
	c.closed.Add(1)
	return nil
}

// daemonLayer presents a layer the way ggcr presents one read back from the
// local Docker daemon: what the daemon kept is what Uncompressed() returns —
// our own blob, which the daemon never decompressed — while Compressed()
// gzips it on the way out, because the manifest inside `docker save` labels
// every layer tar+gzip.
type daemonLayer struct {
	v1.Layer
	blob       []byte
	compressed atomic.Int32
}

func (l *daemonLayer) MediaType() (types.MediaType, error) { return types.DockerLayer, nil }

func (l *daemonLayer) Uncompressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.blob)), nil
}

func (l *daemonLayer) Compressed() (io.ReadCloser, error) {
	l.compressed.Add(1)
	var out bytes.Buffer
	codec, err := compress.Get("gzip")
	if err != nil {
		return nil, err
	}
	_, _, level := codec.Levels()
	w, err := codec.NewWriter(&out, level)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(l.blob); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(out.Bytes())), nil
}

func (l *daemonLayer) DiffID() (v1.Hash, error) {
	sum := sha256.Sum256(l.blob)
	return v1.NewHash("sha256:" + hex.EncodeToString(sum[:]))
}

func (l *daemonLayer) Digest() (v1.Hash, error) {
	rc, err := l.Compressed()
	if err != nil {
		return v1.Hash{}, err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return v1.Hash{}, err
	}
	return v1.NewHash("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

// asDaemonImage re-presents a real backup image the way the local daemon hands
// it back: same layers, same contents, every one of them re-labelled tar+gzip.
func asDaemonImage(t *testing.T, img v1.Image) (*layersImage, []*daemonLayer) {
	t.Helper()
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	wrapped := make([]v1.Layer, 0, len(layers))
	originals := make([]*daemonLayer, 0, len(layers))
	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		blob, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		dl := &daemonLayer{Layer: l, blob: blob}
		wrapped = append(wrapped, dl)
		originals = append(originals, dl)
	}
	return &layersImage{Image: img, layers: wrapped}, originals
}

// TestDaemonSourceReadsWhatTheDaemonKept is the end of the chain: an image
// presented the way the daemon presents one is read back chunk for chunk, and
// without asking it for a representation it would have had to build — gzipping
// the whole backup on the way out only to gunzip it again.
func TestDaemonSourceReadsWhatTheDaemonKept(t *testing.T) {
	img, _, a, b := sourceFixture(t)
	daemonImg, layers := asDaemonImage(t, img)

	s, err := newImageSource(daemonImg, SourceOptions{CacheDir: t.TempDir(), CacheSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s.daemon = true
	for i, want := range [][]byte{a, b} {
		got, err := s.Blob(context.Background(), i)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("chunk %d = %q, want %q", i, got, want)
		}
	}
	if got := layers[2].compressed.Load(); got != 0 {
		t.Fatalf("the data layer was compressed %d times; the daemon's own bytes are the ones to read", got)
	}
}

// TestLayerBytesNamesTheRightAuthority: the digest a re-read is compared with
// has to belong to the representation being re-read, or the comparison is
// either a false alarm or a tautology.
func TestLayerBytesNamesTheRightAuthority(t *testing.T) {
	img, _, _, _ := sourceFixture(t)
	daemonImg, layers := asDaemonImage(t, img)
	data := layers[2]

	registrySource, err := newImageSource(img, SourceOptions{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	published, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	got, err := registrySource.layerBytes(published[2])
	if err != nil {
		t.Fatal(err)
	}
	want, err := published[2].Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got.digest != want {
		t.Errorf("registry layer digest = %s, want the published one %s", got.digest, want)
	}
	if !strings.Contains(got.label, "OCI") {
		t.Errorf("label %q does not name the manifest the digest comes from", got.label)
	}

	daemonSource, err := newImageSource(daemonImg, SourceOptions{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	daemonSource.daemon = true
	got, err = daemonSource.layerBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	want, err = data.DiffID()
	if err != nil {
		t.Fatal(err)
	}
	if got.digest != want {
		t.Errorf("daemon layer digest = %s, want the diffID %s", got.digest, want)
	}
	if !strings.Contains(got.label, "daemon") {
		t.Errorf("label %q does not say the digest is the daemon's", got.label)
	}
}

// TestVerifyStoredThroughTheDaemonRepresentation: the streaming verification
// reads a layer exactly once and compares what it read with a digest from
// elsewhere. Through the daemon that digest is the diffID Docker computed when
// it loaded the image, and the pass must still hold.
func TestVerifyStoredThroughTheDaemonRepresentation(t *testing.T) {
	daemonImg, _ := asDaemonImage(t, verifyFixture(t, nil))
	s, err := newImageSource(daemonImg, SourceOptions{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.daemon = true
	report, err := VerifyStoredSource(context.Background(), s, false, nil)
	if err != nil {
		t.Fatalf("verification through the daemon: %v", err)
	}
	if !report.OK || report.Layers != 1 || report.Chunks != 2 {
		t.Fatalf("report = %+v, want 1 layer and 2 chunks", report)
	}
}
