package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
)

// BlobClient performs bounded-memory, authenticated registry blob uploads.
// It is deliberately narrower than Push: callers stream one already-digested
// layer and publish manifests separately.
type BlobClient struct {
	p         *pusher
	chunkSize int
}

// NewBlobClient creates a client for the repository in ref. provider may block
// while a remote session obtains a fresh bearer token.
func NewBlobClient(ctx context.Context, ref name.Reference, provider Provider, chunkSize int) (*BlobClient, error) {
	if provider == nil {
		return nil, errors.New("registry token provider is required")
	}
	if chunkSize <= 0 {
		chunkSize = 32 << 20
	}
	if chunkSize > 64<<20 {
		return nil, errors.New("registry upload chunk exceeds 64 MiB memory limit")
	}
	base, err := httpBase(ref.Context().RegistryStr())
	if err != nil {
		return nil, err
	}
	scope := Scope{Repository: ref.Context().RepositoryStr(), Actions: []string{"pull", "push"}}
	p := &pusher{
		ctx:  ctx,
		ref:  ref,
		base: base + "/v2/" + ref.Context().RepositoryStr(),
		client: &http.Client{Transport: NewRoundTripper(
			http.DefaultTransport, provider, scope,
		)},
		opts: PushOptions{ChunkSize: int64(chunkSize), MaxRetries: 5},
	}
	return &BlobClient{p: p, chunkSize: chunkSize}, nil
}

func (c *BlobClient) Exists(digest string) (bool, error) {
	return c.p.blobExists(digest)
}

// Open starts a registry upload session. At most chunkSize bytes are retained.
func (c *BlobClient) Open(digest string) (*BlobUpload, error) {
	if !validSHA256Digest(digest) {
		return nil, fmt.Errorf("invalid blob digest %q", digest)
	}
	loc, err := c.p.startUpload()
	if err != nil {
		return nil, err
	}
	if loc == "" {
		return nil, errors.New("registry returned an empty upload location")
	}
	return &BlobUpload{
		client: c,
		digest: digest,
		loc:    loc,
		buf:    make([]byte, 0, c.chunkSize),
	}, nil
}

// BlobUpload implements io.Writer and commits with the declared digest.
type BlobUpload struct {
	mu        sync.Mutex
	client    *BlobClient
	digest    string
	loc       string
	buf       []byte
	err       error
	committed bool
	aborted   bool
}

func (u *BlobUpload) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil {
		return 0, u.err
	}
	if u.committed || u.aborted {
		return 0, errors.New("registry blob upload is closed")
	}
	written := 0
	for len(p) > 0 {
		space := cap(u.buf) - len(u.buf)
		if space == 0 {
			if err := u.flush(); err != nil {
				u.err = err
				return written, err
			}
			space = cap(u.buf)
		}
		n := len(p)
		if n > space {
			n = space
		}
		u.buf = append(u.buf, p[:n]...)
		p = p[n:]
		written += n
	}
	return written, nil
}

func (u *BlobUpload) Commit(context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil {
		return u.err
	}
	if u.aborted {
		return errors.New("registry blob upload was aborted")
	}
	if u.committed {
		return nil
	}
	if err := u.flush(); err != nil {
		u.err = err
		return err
	}
	if err := u.client.p.putFinal(u.loc, u.digest); err != nil {
		u.err = err
		return err
	}
	u.committed = true
	return nil
}

func (u *BlobUpload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.committed || u.aborted {
		return nil
	}
	u.aborted = true
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.loc, nil)
	if err != nil {
		return err
	}
	resp, err := u.client.p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("drain upload abort response: %w", err)
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("abort upload answered %d", resp.StatusCode)
	}
	return nil
}

func (u *BlobUpload) flush() error {
	if len(u.buf) == 0 {
		return nil
	}
	loc, err := u.client.p.patch(u.loc, u.buf)
	if err != nil {
		return err
	}
	u.loc = loc
	u.buf = u.buf[:0]
	return nil
}

func validSHA256Digest(d string) bool {
	if len(d) != 71 || d[:7] != "sha256:" {
		return false
	}
	for _, c := range d[7:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
