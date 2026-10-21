package registry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// tagLimits bound one tag listing. go-containerregistry follows Link:
// rel="next" with no limit: a registry that points back at a page already
// read, or never stops answering with a next one, held the command forever
// while the list grew until memory ran out.
type tagLimits struct {
	pages     int   // pages followed
	tags      int   // tags collected
	pageBytes int64 // body of one page
}

var defaultTagLimits = tagLimits{pages: 10_000, tags: 1_000_000, pageBytes: 32 << 20}

// ListTagNames returns the tags of repo, following the pagination with a
// bound on pages, tags and page size, and refusing a next page already read.
// opts are the remote options of the caller (context, keychain); a transport
// they set is replaced by the bounded default one.
func ListTagNames(ctx context.Context, repo name.Repository, opts ...remote.Option) ([]string, error) {
	return listTagNames(ctx, repo, defaultTagLimits, opts...)
}

func listTagNames(ctx context.Context, repo name.Repository, limits tagLimits, opts ...remote.Option) ([]string, error) {
	opts = append(opts, remote.WithContext(ctx),
		remote.WithTransport(&boundedTagPages{base: remote.DefaultTransport, limit: limits.pageBytes}))
	puller, err := remote.NewPuller(opts...)
	if err != nil {
		return nil, err
	}
	lister, err := puller.Lister(ctx, repo)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	tags := []string{}
	for pages := 0; lister.HasNext(); pages++ {
		if pages >= limits.pages {
			return nil, fmt.Errorf("tag listing of %s exceeds %d pages", repo, limits.pages)
		}
		page, err := lister.Next(ctx)
		if err != nil {
			return nil, err
		}
		tags = append(tags, page.Tags...)
		if len(tags) > limits.tags {
			return nil, fmt.Errorf("tag listing of %s exceeds %d tags", repo, limits.tags)
		}
		if page.Next != "" {
			if seen[page.Next] {
				return nil, fmt.Errorf("tag listing of %s loops back to a page already read", repo)
			}
			seen[page.Next] = true
		}
	}
	return tags, nil
}

// boundedTagPages caps the body of a tags/list answer, which the library
// decodes whole.
type boundedTagPages struct {
	base  http.RoundTripper
	limit int64
}

func (t *boundedTagPages) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || !strings.HasSuffix(req.URL.Path, "/tags/list") {
		return resp, err
	}
	resp.Body = &boundedBody{rc: resp.Body, left: t.limit}
	return resp, nil
}

// boundedBody fails once more than its limit is read; a body of exactly the
// limit still ends with io.EOF.
type boundedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var probe [1]byte
		n, err := b.rc.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("tag page exceeds the size limit")
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *boundedBody) Close() error { return b.rc.Close() }
