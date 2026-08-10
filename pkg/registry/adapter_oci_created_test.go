package registry

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// A backimage tag points at a multi-arch index, so ListTags must reach the
// timestamp through the index annotations or a child manifest. A zero time
// makes retention keep the tag forever, so this is what breaks prune.
func TestListTagsCreatedFromIndex(t *testing.T) {
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(nopLogger(t))))
	defer srv.Close()
	host := mustHost(t, srv.URL)

	created := time.Date(2026, 8, 9, 21, 8, 47, 0, time.UTC)
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Created = v1.Time{Time: created}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatal(err)
	}

	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := img.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	var idx v1.ImageIndex = empty.Index
	idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
		Add: img,
		Descriptor: v1.Descriptor{
			MediaType: types.OCIManifestSchema1,
			Size:      int64(len(raw)),
			Digest:    digest,
			Platform:  &v1.Platform{OS: "linux", Architecture: "amd64"},
		},
	})

	// Deliberately annotation-free: this is the shape already published by
	// earlier backimage versions.
	tagged := host + "/backup:snap-20260809T210847Z"
	ref, err := name.NewTag(tagged)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}

	a := &ociAdapter{host: host}
	tags, err := a.ListTags(context.Background(), ref.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 {
		t.Fatalf("ListTags returned %d tags", len(tags))
	}
	if !tags[0].Created.Equal(created) {
		t.Fatalf("Created = %v, want %v", tags[0].Created, created)
	}

	// The whole point: a dated tag is now prunable.
	_, remove := Policy{KeepLast: 0, KeepWithin: time.Hour}.Apply(tags, time.Now())
	if len(remove) != 1 {
		t.Fatalf("retention kept an old tag: remove = %v", remove)
	}
}

func TestListTagsCreatedFromIndexAnnotations(t *testing.T) {
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(nopLogger(t))))
	defer srv.Close()
	host := mustHost(t, srv.URL)

	created := time.Date(2026, 8, 10, 20, 54, 35, 0, time.UTC)
	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := img.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	var idx v1.ImageIndex = empty.Index
	idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
		Add: img,
		Descriptor: v1.Descriptor{
			MediaType: types.OCIManifestSchema1,
			Size:      int64(len(raw)),
			Digest:    digest,
			Platform:  &v1.Platform{OS: "linux", Architecture: "amd64"},
		},
	})
	idx = mutate.Annotations(idx, map[string]string{
		annotationCreated: created.Format(time.RFC3339),
	}).(v1.ImageIndex)

	ref, err := name.NewTag(host + "/backup:snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}

	a := &ociAdapter{host: host}
	tags, err := a.ListTags(context.Background(), ref.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || !tags[0].Created.Equal(created) {
		t.Fatalf("Created = %v, want %v", tags, created)
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func nopLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}
