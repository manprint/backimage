package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// tagPager serves /v2/team/r/tags/list through page(n), n counting the requests.
func tagPager(t *testing.T, page func(w http.ResponseWriter, r *http.Request, n int)) name.Repository {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		n++
		page(w, r, n)
	}))
	t.Cleanup(srv.Close)
	repo, err := name.NewRepository(strings.TrimPrefix(srv.URL, "http://")+"/team/r", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// listWithin runs the listing with a deadline, so a regression fails the test
// instead of hanging it.
func listWithin(t *testing.T, repo name.Repository, limits tagLimits) ([]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tags, err := listTagNames(ctx, repo, limits)
	if ctx.Err() != nil {
		t.Fatalf("the listing ran until the deadline (%d tags collected)", len(tags))
	}
	return tags, err
}

func TestTagListingRefusesALoop(t *testing.T) {
	repo := tagPager(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Link", `</v2/team/r/tags/list?last=a>; rel="next"`)
		fmt.Fprint(w, `{"name":"team/r","tags":["a"]}`)
	})
	if _, err := listWithin(t, repo, defaultTagLimits); err == nil || !strings.Contains(err.Error(), "loops") {
		t.Fatalf("a page pointing at itself = %v, want a loop error", err)
	}
}

func TestTagListingBoundsPagesTagsAndBodies(t *testing.T) {
	endless := tagPager(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Link", fmt.Sprintf(`</v2/team/r/tags/list?last=t%d>; rel="next"`, n))
		fmt.Fprintf(w, `{"name":"team/r","tags":["t%d"]}`, n)
	})
	if _, err := listWithin(t, endless, tagLimits{pages: 50, tags: 1000, pageBytes: 1 << 20}); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Fatalf("an endless pagination = %v, want the page bound", err)
	}
	if _, err := listWithin(t, endless, tagLimits{pages: 1000, tags: 20, pageBytes: 1 << 20}); err == nil || !strings.Contains(err.Error(), "tags") {
		t.Fatalf("an endless pagination = %v, want the tag bound", err)
	}
	huge := tagPager(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		fmt.Fprint(w, `{"name":"team/r","tags":["`+strings.Repeat("a", 2<<20)+`"]}`)
	})
	if _, err := listWithin(t, huge, tagLimits{pages: 10, tags: 10, pageBytes: 1 << 20}); err == nil {
		t.Fatal("a page over the size bound was decoded")
	}
}

func TestTagListingFollowsAnOrdinaryPagination(t *testing.T) {
	repo := tagPager(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n < 3 {
			w.Header().Set("Link", fmt.Sprintf(`</v2/team/r/tags/list?last=t%d>; rel="next"`, n))
		}
		body := fmt.Sprintf(`{"name":"team/r","tags":["t%d"]}`, n)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		fmt.Fprint(w, body)
	})
	// The page body is exactly the bound: it still reads to its end.
	tags, err := listWithin(t, repo, tagLimits{pages: 10, tags: 10, pageBytes: int64(len(`{"name":"team/r","tags":["t1"]}`))})
	if err != nil || strings.Join(tags, ",") != "t1,t2,t3" {
		t.Fatalf("tags = %v, %v", tags, err)
	}
}

// countIndex reported the error of the image attempt when the child was an
// index that failed to load, hiding the real cause.
func TestCountIndexReportsTheChildIndexError(t *testing.T) {
	cause := errors.New("child index unreadable")
	idx := &failingIndex{children: []v1.Descriptor{{Digest: v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}}}, indexErr: cause}
	err := countIndex(idx, map[string]countedBlob{})
	if !errors.Is(err, cause) {
		t.Fatalf("countIndex = %v, want it to carry %v", err, cause)
	}
}

// failingIndex is an index whose one child loads neither as an image nor as
// an index; the methods countIndex does not call are left to the nil base.
type failingIndex struct {
	base     v1.ImageIndex
	children []v1.Descriptor
	indexErr error
}

func (f *failingIndex) MediaType() (types.MediaType, error) { return f.base.MediaType() }
func (f *failingIndex) Digest() (v1.Hash, error)            { return f.base.Digest() }
func (f *failingIndex) Size() (int64, error)                { return f.base.Size() }
func (f *failingIndex) RawManifest() ([]byte, error)        { return f.base.RawManifest() }

func (f *failingIndex) IndexManifest() (*v1.IndexManifest, error) {
	return &v1.IndexManifest{Manifests: f.children}, nil
}

func (f *failingIndex) Image(v1.Hash) (v1.Image, error) {
	return nil, errors.New("unexpected media type for Image")
}

func (f *failingIndex) ImageIndex(v1.Hash) (v1.ImageIndex, error) { return nil, f.indexErr }
