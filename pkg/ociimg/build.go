// Package pkg/ociimg assembles runnable, pullable OCI images from the
// encrypted backup produced by phases 02-03 (phase 04).
package ociimg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/manprint/backimage/pkg/compress"
	"github.com/manprint/backimage/pkg/index"
)

var (
	errNoEntrypoint     = errors.New("image has no entrypoint: pass the self-extracting bootstrap binary")
	errNonStandardCodec = fmt.Errorf("non-standard layer media type: %s", forbiddenCodecHint)
)

// forbiddenCodecHint is the D02 hint (phase 02.1).
const forbiddenCodecHint = "xz and lz4 produce non-standard layer media types: use --compression zstd|gzip, or --runnable=false if you only need a pullable image"

// BuildOptions describes the inputs of one platform image.
type BuildOptions struct {
	Platform    v1.Platform
	SelfExtract []byte // static binary placed at /backimage (mandatory)
	Runnable    bool   // false = pull-only image; gates the D02 guard
	Manifest    *index.Manifest
	ChunkTable  *index.ChunkTable
	IndexBlob   []byte // index blob; already compressed (.zst) and possibly encrypted
	// PrivateBlob is the sealed confidential metadata of an encrypted backup
	// (index.PrivatePath). It is empty when encryption is disabled.
	PrivateBlob []byte
	KeyFiles    map[string][]byte // relative paths under /backup -> content ("keys.age", ...)
	DataLayers  []v1.Layer        // data layers; must be identical across platforms
	Codec       compress.Codec
	Created     string // RFC3339; empty = wall clock (not reproducible)
}

const (
	executablePath = "/backimage"
	metaDir        = "/backup"
	indexName      = "index.json.zst"

	labelCreated       = "org.opencontainers.image.created"
	labelTitle         = "org.opencontainers.image.title"
	labelDescription   = "org.opencontainers.image.description"
	labelSchemaVersion = "dev.backimage.schema-version"
	labelToolVersion   = "dev.backimage.tool-version"
	labelEncrypted     = "dev.backimage.encrypted"
	labelCompression   = "dev.backimage.compression"
	labelFiles         = "dev.backimage.files"
	labelBytesRaw      = "dev.backimage.bytes-raw"
	labelChunks        = "dev.backimage.chunks"
	labelSources       = "dev.backimage.sources"
)

const (
	defaultTitle       = "backimage backup"
	defaultDescription = "run this image to restore the backup"
)

// imageLabels assembles the stable label set attached both to the config and
// to the manifest annotations. Sources are omitted (--no-metadata) when
// m.Sources is nil.
//
// An encrypted backup publishes no label describing its content: source paths,
// file count and raw size are readable from the registry without pulling the
// image, so they would defeat the confidentiality of the backup. They live in
// the encrypted private blob instead.
func imageLabels(m *index.Manifest, created, codecName string) map[string]string {
	l := map[string]string{
		labelCreated:       created,
		labelTitle:         defaultTitle,
		labelDescription:   defaultDescription,
		labelSchemaVersion: strconv.Itoa(index.SchemaVersion),
		labelToolVersion:   "dev",
		labelEncrypted:     "false",
		labelCompression:   codecName,
		labelFiles:         "0",
		labelBytesRaw:      "0",
		labelChunks:        "0",
	}
	if m == nil {
		return l
	}
	if m.SchemaVersion > 0 {
		l[labelSchemaVersion] = strconv.Itoa(m.SchemaVersion)
	}
	if m.Tool.Version != "" {
		l[labelToolVersion] = m.Tool.Version
	}
	if m.Chunking.Count > 0 {
		l[labelChunks] = strconv.Itoa(m.Chunking.Count)
	}
	if m.Encryption.Enabled {
		l[labelEncrypted] = "true"
		delete(l, labelFiles)
		delete(l, labelBytesRaw)
		return l
	}
	l[labelFiles] = strconv.FormatInt(m.Totals.Files, 10)
	l[labelBytesRaw] = strconv.FormatInt(m.Totals.BytesRaw, 10)
	if m.Sources != nil {
		l[labelSources] = strings.Join(m.Sources, ";")
	}
	return l
}

// BuildImage assembles one platform image with layer ordering:
//
//	layer 0 = /backimage (entrypoint, mode 0755)
//	layer 1 = /backup (manifest.json, chunks.json, index.json.zst, keys)
//	layers 2..N = DataLayers
func BuildImage(opts BuildOptions) (v1.Image, error) {
	if len(opts.SelfExtract) == 0 {
		return nil, errNoEntrypoint
	}
	if opts.Runnable && opts.Codec != nil && opts.Codec.MediaTypeSuffix() == "" {
		return nil, fmt.Errorf("%w: %s", errNonStandardCodec, opts.Codec.Name())
	}
	created := opts.Created
	if created == "" {
		created = time.Now().UTC().Format(time.RFC3339)
	}

	exeLayer, err := buildExecutableLayer(opts.SelfExtract)
	if err != nil {
		return nil, err
	}
	metaLayer, err := buildMetaLayer(opts)
	if err != nil {
		return nil, err
	}

	img, err := mutate.AppendLayers(empty.Image, exeLayer, metaLayer)
	if err != nil {
		return nil, err
	}
	for _, dl := range opts.DataLayers {
		if dl == nil {
			return nil, errors.New("nil data layer")
		}
		img, err = mutate.AppendLayers(img, dl)
		if err != nil {
			return nil, err
		}
	}

	codecName := "store"
	if opts.Codec != nil {
		codecName = opts.Codec.Name()
	}
	labels := imageLabels(opts.Manifest, created, codecName)

	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg.Architecture = opts.Platform.Architecture
	cfg.OS = opts.Platform.OS
	cfg.Config = v1.Config{
		Entrypoint: []string{executablePath},
		Cmd:        []string{"info"},
		WorkingDir: "/",
		User:       "0:0",
		Env:        nil,
		Labels:     labels,
	}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		return nil, err
	}
	img = mutate.Annotations(img, labels).(v1.Image)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	return img, nil
}

// BuiltImage pairs one platform with its assembled image.
type BuiltImage struct {
	Platform v1.Platform
	Image    v1.Image
}

// BuildIndex verifies that the shared layers (meta + data) are identical
// across platforms and assembles the multi-arch index.
func BuildIndex(imgs []BuiltImage) (v1.ImageIndex, error) {
	if len(imgs) == 0 {
		return nil, errors.New("no platform images")
	}
	layerSets := make([][]v1.Layer, len(imgs))
	for i, b := range imgs {
		layers, err := b.Image.Layers()
		if err != nil {
			return nil, fmt.Errorf("platform %s: %w", b.Platform, err)
		}
		if len(layers) < 2 {
			return nil, fmt.Errorf("platform %s: expected 2+ layers, got %d", b.Platform, len(layers))
		}
		layerSets[i] = layers
	}
	metaDigest, err := layerSets[0][1].Digest()
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(imgs); i++ {
		d, err := layerSets[i][1].Digest()
		if err != nil {
			return nil, err
		}
		if d != metaDigest {
			return nil, fmt.Errorf("platform %s: shared layer digest %s differs from %s: data layers must be identical across platforms", imgs[i].Platform, d, metaDigest)
		}
		for j := 1; j < len(layerSets[i]); j++ {
			di, err := layerSets[i][j].Digest()
			if err != nil {
				return nil, err
			}
			d0, err := layerSets[0][j].Digest()
			if err != nil {
				return nil, err
			}
			if di != d0 {
				return nil, fmt.Errorf("platform %s: data layers must be identical across platforms: layer %d digest %s differs from %s", imgs[i].Platform, j-1, di, d0)
			}
		}
	}

	sorted := make([]BuiltImage, len(imgs))
	copy(sorted, imgs)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Platform, sorted[j].Platform
		if a.OS != b.OS {
			return a.OS < b.OS
		}
		return a.Architecture < b.Architecture
	})

	var idx v1.ImageIndex = empty.Index
	for _, b := range sorted {
		raw, err := b.Image.RawManifest()
		if err != nil {
			return nil, err
		}
		digest, err := b.Image.Digest()
		if err != nil {
			return nil, err
		}
		p := b.Platform
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add: b.Image,
			Descriptor: v1.Descriptor{
				MediaType: types.OCIManifestSchema1,
				Size:      int64(len(raw)),
				Digest:    digest,
				Platform:  &p,
			},
		})
	}
	// Mirror the platform labels onto the index. A tag points at the index, so
	// without these a reader (retention, repo tags) must fetch a child manifest
	// just to learn when the backup was created.
	if m, err := sorted[0].Image.Manifest(); err == nil && len(m.Annotations) > 0 {
		if annotated, ok := mutate.Annotations(idx, m.Annotations).(v1.ImageIndex); ok {
			idx = annotated
		}
	}
	return idx, nil
}

func buildExecutableLayer(self []byte) (v1.Layer, error) {
	if len(self) == 0 {
		return nil, errNoEntrypoint
	}
	return NewLayer([]LayerFile{{
		Path: executablePath,
		Mode: 0o755,
		Size: int64(len(self)),
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(self)), nil
		},
	}}, mustStoreCodec(), 0)
}

func buildMetaLayer(opts BuildOptions) (v1.Layer, error) {
	var files []LayerFile
	app := func(name string, data []byte) {
		files = append(files, LayerFile{
			Path: metaDir + "/" + name,
			Mode: 0o644,
			Size: int64(len(data)),
			Open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(data)), nil
			},
		})
	}
	if opts.Manifest != nil {
		var b bytes.Buffer
		if err := index.WriteManifest(&b, opts.Manifest); err != nil {
			return nil, err
		}
		app("manifest.json", b.Bytes())
	}
	if opts.ChunkTable != nil {
		var b bytes.Buffer
		if err := index.WriteChunkTable(&b, opts.ChunkTable); err != nil {
			return nil, err
		}
		app("chunks.json", b.Bytes())
	}
	if len(opts.IndexBlob) > 0 {
		app(indexName, opts.IndexBlob)
	}
	if len(opts.PrivateBlob) > 0 {
		app(index.PrivatePath, opts.PrivateBlob)
	}
	names := make([]string, 0, len(opts.KeyFiles))
	for k := range opts.KeyFiles {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		app(n, opts.KeyFiles[n])
	}
	if len(files) == 0 {
		return nil, errors.New("no metadata to build the layer")
	}
	return NewLayer(files, mustStoreCodec(), 0)
}

func mustStoreCodec() compress.Codec {
	c, err := compress.ByID(compress.Store)
	if err != nil {
		panic(err)
	}
	return c
}
