package restore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/manprint/backimage/pkg/ociimg"
)

func TestParseExpectedDigest(t *testing.T) {
	if e, err := ParseExpectedDigest(""); err != nil || e.Set() || e.String() != "" {
		t.Fatalf("empty = %v, %v, set=%v", e, err, e.Set())
	}
	if e, err := ParseExpectedDigest("   "); err != nil || e.Set() {
		t.Fatalf("blank = %v, %v", e, err)
	}
	good := "sha256:" + strings.Repeat("ab", 32)
	e, err := ParseExpectedDigest(" " + good + " ")
	if err != nil || !e.Set() || e.String() != good {
		t.Fatalf("valid = %q, %v", e.String(), err)
	}
	for _, bad := range []string{"deadbeef", "sha256:zz", "md5:" + strings.Repeat("a", 32), "sha256:"} {
		if _, err := ParseExpectedDigest(bad); err == nil {
			t.Errorf("%q accepted as a digest", bad)
		}
	}
}

// An unset expectation must not turn into an anchor that matches nothing.
func TestNoExpectationAllowsEveryDigest(t *testing.T) {
	var none ExpectedDigest
	h, err := v1.NewHash("sha256:" + strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := none.matchResolved("anything", h); err != nil {
		t.Fatalf("no anchor must accept anything: %v", err)
	}
}

// The whole value of the flag is that the comparison happens BEFORE the
// source is read: a caller holding a trusted digest must never hand a
// passphrase, or an HTTP request for a layer, to the wrong image.
func TestRegistrySourceRefusesTheWrongDigestBeforeFetchingAnything(t *testing.T) {
	img, _, _, _ := sourceFixture(t)
	idx, err := ociimg.BuildIndex([]ociimg.BuiltImage{{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Image: img}})
	if err != nil {
		t.Fatal(err)
	}
	var blobRequests atomic.Int64
	inner := ggcrregistry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blobs/") && r.Method == http.MethodGet {
			blobRequests.Add(1)
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ref, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://")+"/repo:tag", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	published, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	blobRequests.Store(0)

	other := "sha256:" + strings.Repeat("11", 32)
	expect, err := ParseExpectedDigest(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromRegistry(context.Background(), ref, nil, SourceOptions{ExpectDigest: expect}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("wrong digest accepted: %v", err)
	} else if !strings.Contains(err.Error(), published.String()) || !strings.Contains(err.Error(), other) {
		t.Fatalf("the refusal must name both digests: %v", err)
	}
	if got := blobRequests.Load(); got != 0 {
		t.Fatalf("refused image still cost %d blob requests", got)
	}

	right, err := ParseExpectedDigest(published.String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := FromRegistry(context.Background(), ref, nil, SourceOptions{ExpectDigest: right, CacheDir: t.TempDir(), CacheSize: 1 << 20})
	if err != nil {
		t.Fatalf("the published digest must be accepted: %v", err)
	}
	defer s.Close()
	if _, err := s.Manifest(context.Background()); err != nil {
		t.Fatalf("manifest after a matching anchor: %v", err)
	}
}

// A layout has no registry to ask: the anchor is what index.json advertises,
// which is the digest a push of this layout would carry.
func TestOCILayoutRefusesTheWrongDigest(t *testing.T) {
	img, _, _, _ := sourceFixture(t)
	idx, err := ociimg.BuildIndex([]ociimg.BuiltImage{{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Image: img}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := lp.AppendIndex(idx); err != nil {
		t.Fatal(err)
	}
	advertised, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}

	wrong, err := ParseExpectedDigest("sha256:" + strings.Repeat("22", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromOCILayout(dir, "example.test/repo:tag", SourceOptions{ExpectDigest: wrong}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("wrong digest accepted: %v", err)
	}

	right, err := ParseExpectedDigest(advertised.String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := FromOCILayout(dir, "example.test/repo:tag", SourceOptions{ExpectDigest: right})
	if err != nil {
		t.Fatalf("the advertised digest must be accepted: %v", err)
	}
	defer s.Close()
	if _, err := s.Manifest(context.Background()); err != nil {
		t.Fatalf("manifest after a matching anchor: %v", err)
	}
}

// A caller may hold either identity of a local layout: the index digest the
// backup reported, or the per-platform manifest digest inside it. Both name
// the same object; an unrelated digest names nothing.
func TestBothIdentitiesOfALayoutAnchorIt(t *testing.T) {
	img, _, _, _ := sourceFixture(t)
	idx, err := ociimg.BuildIndex([]ociimg.BuiltImage{{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Image: img}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := layout.Write(dir, idx); err != nil {
		t.Fatal(err)
	}
	top, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	inner, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if top == inner {
		t.Fatal("the fixture must have two distinct identities to be worth testing")
	}
	for name, digest := range map[string]v1.Hash{"index": top, "platform manifest": inner} {
		expect, err := ParseExpectedDigest(digest.String())
		if err != nil {
			t.Fatal(err)
		}
		s, err := FromOCILayout(dir, "x", SourceOptions{ExpectDigest: expect})
		if err != nil {
			t.Fatalf("%s digest refused: %v", name, err)
		}
		s.Close()
	}
	unrelated, err := ParseExpectedDigest("sha256:" + strings.Repeat("44", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromOCILayout(dir, "x", SourceOptions{ExpectDigest: unrelated}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("unrelated digest accepted: %v", err)
	}
}
