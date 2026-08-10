package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Progress reports a phase or outcome of one blob transfer. Event is one of
// checking, uploading, completed, skipped, manifests or published.
type Progress struct {
	Event          string `json:"event,omitempty"`
	Blob           string `json:"blob"`
	Layer          int    `json:"layer,omitempty"`
	Total          int64  `json:"total,omitempty"`
	Completed      int64  `json:"completed,omitempty"`
	Skipped        bool   `json:"skipped,omitempty"`
	FromCheckpoint bool   `json:"fromCheckpoint,omitempty"`
}

// PushOptions tunes the upload.
type PushOptions struct {
	Jobs       int   // parallel blob uploads, default 3
	ChunkSize  int64 // HTTP PATCH chunk size, default 32 MiB
	MaxRetries int   // per blob attempt budget, default 5
	Checkpoint CheckpointStore
	ID         string // deterministic checkpoint id
	Manifest   []byte // manifest.json bytes carried by the checkpoint
	Progress   chan<- Progress
}

// blobTask is one blob to make present on the registry.
type blobTask struct {
	layer  int
	digest string
	size   int64
	open   func() (io.ReadCloser, error)
}

// Push uploads the blobs of the platform images plus the multi-arch index
// to ref, skipping blobs already present. Confirmed blobs are recorded in
// the checkpoint store so an interrupted backup resumes without
// re-uploading. Platform manifests and the index are pushed after all
// blobs are present.
//
// images maps "os/arch" to its assembled image; idx is the multi-arch
// index built on top of them. All requests flow through one bearer
// transport, so token refreshes are shared and coalesced.
func Push(ctx context.Context, ref name.Reference, images map[string]v1.Image, idx v1.ImageIndex, kc Keychain, opts PushOptions) error {
	if opts.Jobs <= 0 {
		opts.Jobs = 3
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 32 << 20
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 5
	}
	tasks, staged, taggedDigest, tagged, err := collectTasksBlobs(images, idx)
	if err != nil {
		return err
	}

	auth := authn.Anonymous
	if kc != nil {
		a, err := kc.Resolve(ref.Context())
		if err != nil {
			return fmt.Errorf("resolving registry credentials: %w", err)
		}
		auth = a
	}
	base, err := httpBase(ref.Context().RegistryStr())
	if err != nil {
		return err
	}
	scope := Scope{Repository: ref.Context().RepositoryStr(), Actions: []string{"pull", "push"}}
	p := &pusher{
		ctx:          ctx,
		ref:          ref,
		base:         base + "/v2/" + ref.Context().RepositoryStr(),
		tasks:        tasks,
		staged:       staged,
		taggedDigest: taggedDigest,
		tagged:       tagged,
		opts:         opts,
		done:         make(map[string]bool, len(tasks)),
		client: &http.Client{
			Transport: NewRoundTripper(http.DefaultTransport, NewProvider(ref.Context().RegistryStr(), auth), scope),
		},
	}
	if opts.Checkpoint != nil && opts.ID != "" {
		ck, err := opts.Checkpoint.Load(opts.ID)
		switch {
		case err == nil:
			if ck.ID != opts.ID || (ck.Ref != "" && ck.Ref != ref.Name()) {
				return fmt.Errorf("checkpoint %s does not match backup %s", opts.ID, ref.Name())
			}
			for _, d := range ck.DoneBlobs {
				p.done[d] = true
			}
		case errors.Is(err, ErrCheckpointNotFound):
			// Fresh run.
		default:
			return err
		}
	}
	return p.run()
}

// PushWithProvider publishes an index using an already-scoped, refreshable
// token provider. It is used by the remote server, whose credentials arrive
// over the control stream and must never be written to disk.
func PushWithProvider(ctx context.Context, ref name.Reference, images map[string]v1.Image, idx v1.ImageIndex, provider Provider, opts PushOptions) error {
	if provider == nil {
		return errors.New("registry token provider is required")
	}
	if opts.Jobs <= 0 {
		opts.Jobs = 3
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 32 << 20
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 5
	}
	tasks, staged, taggedDigest, tagged, err := collectTasksBlobs(images, idx)
	if err != nil {
		return err
	}
	base, err := httpBase(ref.Context().RegistryStr())
	if err != nil {
		return err
	}
	scope := Scope{Repository: ref.Context().RepositoryStr(), Actions: []string{"pull", "push"}}
	p := &pusher{
		ctx:          ctx,
		ref:          ref,
		base:         base + "/v2/" + ref.Context().RepositoryStr(),
		tasks:        tasks,
		staged:       staged,
		taggedDigest: taggedDigest,
		tagged:       tagged,
		opts:         opts,
		done:         make(map[string]bool, len(tasks)),
		client: &http.Client{Transport: NewRoundTripper(
			http.DefaultTransport, provider, scope,
		)},
	}
	if opts.Checkpoint != nil && opts.ID != "" {
		ck, loadErr := opts.Checkpoint.Load(opts.ID)
		switch {
		case loadErr == nil:
			if ck.ID != opts.ID || (ck.Ref != "" && ck.Ref != ref.Name()) {
				return fmt.Errorf("checkpoint %s does not match backup %s", opts.ID, ref.Name())
			}
			for _, d := range ck.DoneBlobs {
				p.done[d] = true
			}
		case errors.Is(loadErr, ErrCheckpointNotFound):
		default:
			return loadErr
		}
	}
	return p.run()
}

// collectTasksBlobs deduplicates blob tasks across platforms and captures
// the raw platform manifests plus the index for the publish step.
func collectTasksBlobs(images map[string]v1.Image, idx v1.ImageIndex) ([]blobTask, map[string]rawManifest, string, rawManifest, error) {
	platforms := make([]string, 0, len(images))
	for pl := range images {
		platforms = append(platforms, pl)
	}
	sort.Strings(platforms)

	var tasks []blobTask
	seen := map[string]bool{}
	for _, pl := range platforms {
		img := images[pl]
		config, err := img.RawConfigFile()
		if err != nil {
			return nil, nil, "", rawManifest{}, fmt.Errorf("platform %s config: %w", pl, err)
		}
		configDigest, err := img.ConfigName()
		if err != nil {
			return nil, nil, "", rawManifest{}, fmt.Errorf("platform %s config digest: %w", pl, err)
		}
		if !seen[configDigest.String()] {
			seen[configDigest.String()] = true
			body := append([]byte(nil), config...)
			tasks = append(tasks, blobTask{
				layer:  -1,
				digest: configDigest.String(),
				size:   int64(len(body)),
				open: func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(body)), nil
				},
			})
		}
		layers, err := img.Layers()
		if err != nil {
			return nil, nil, "", rawManifest{}, fmt.Errorf("platform %s: %w", pl, err)
		}
		for i, l := range layers {
			d, err := l.Digest()
			if err != nil {
				return nil, nil, "", rawManifest{}, err
			}
			if seen[d.String()] {
				continue
			}
			seen[d.String()] = true
			sz, err := l.Size()
			if err != nil {
				return nil, nil, "", rawManifest{}, err
			}
			tasks = append(tasks, blobTask{
				layer:  i,
				digest: d.String(),
				size:   sz,
				open:   l.Compressed,
			})
		}
	}

	staged := make(map[string]rawManifest, len(platforms)+1)
	for _, pl := range platforms {
		img := images[pl]
		raw, err := img.RawManifest()
		if err != nil {
			return nil, nil, "", rawManifest{}, err
		}
		d, err := img.Digest()
		if err != nil {
			return nil, nil, "", rawManifest{}, err
		}
		mt, err := img.MediaType()
		if err != nil {
			return nil, nil, "", rawManifest{}, err
		}
		staged[d.String()] = rawManifest{body: raw, mediaType: mt}
	}
	rawIdx, err := idx.RawManifest()
	if err != nil {
		return nil, nil, "", rawManifest{}, err
	}
	idD, err := idx.Digest()
	if err != nil {
		return nil, nil, "", rawManifest{}, err
	}
	idxMedia, err := idx.MediaType()
	if err != nil {
		return nil, nil, "", rawManifest{}, err
	}
	tagged := rawManifest{body: rawIdx, mediaType: idxMedia}
	staged[idD.String()] = tagged
	return tasks, staged, idD.String(), tagged, nil
}

type rawManifest struct {
	body      []byte
	mediaType types.MediaType
}

// pusher drives the upload job queue.
type pusher struct {
	ctx          context.Context
	ref          name.Reference
	base         string
	tasks        []blobTask
	staged       map[string]rawManifest
	taggedDigest string
	tagged       rawManifest
	opts         PushOptions
	client       *http.Client

	mu     sync.Mutex
	ckptMu sync.Mutex
	done   map[string]bool
}

func (p *pusher) run() error {
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	p.ctx = ctx
	taskCh := make(chan blobTask, p.opts.Jobs)
	go func() {
		defer close(taskCh)
		for _, t := range p.tasks {
			if p.isDone(t.digest) {
				p.report(t, true, true)
				continue
			}
			select {
			case taskCh <- t:
			case <-p.ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, p.opts.Jobs)
	for i := 0; i < p.opts.Jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range taskCh {
				if err := p.executeBlob(t); err != nil {
					cancel()
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
	default:
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	p.reportPhase("manifests")
	if err := p.publishManifests(); err != nil {
		return err
	}
	p.reportPhase("published")
	if p.opts.Checkpoint != nil && p.opts.ID != "" {
		if derr := p.opts.Checkpoint.Delete(p.opts.ID); derr != nil {
			_ = derr // failing to clean the checkpoint is not fatal
		}
	}
	return nil
}

func (p *pusher) isDone(digest string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done[digest]
}

func (p *pusher) markDone(digest string) error {
	p.ckptMu.Lock()
	defer p.ckptMu.Unlock()
	p.mu.Lock()
	p.done[digest] = true
	done := make([]string, 0, len(p.done))
	for d := range p.done {
		done = append(done, d)
	}
	p.mu.Unlock()

	if p.opts.Checkpoint != nil && p.opts.ID != "" {
		ckpt := &Checkpoint{
			ID:        p.opts.ID,
			Ref:       p.ref.Name(),
			CreatedAt: time.Now(),
			DoneBlobs: done,
			Manifest:  p.opts.Manifest,
		}
		if err := p.opts.Checkpoint.Save(ckpt); err != nil {
			return fmt.Errorf("saving checkpoint: %w", err)
		}
	}
	return nil
}

func (p *pusher) report(t blobTask, skipped, fromCheckpoint bool) {
	event := "completed"
	if skipped {
		event = "skipped"
	}
	p.reportEvent(t, event, skipped, fromCheckpoint)
}

func (p *pusher) reportEvent(t blobTask, event string, skipped, fromCheckpoint bool) {
	if p.opts.Progress == nil {
		return
	}
	select {
	case p.opts.Progress <- Progress{Event: event, Blob: t.digest, Layer: t.layer, Total: t.size, Skipped: skipped, FromCheckpoint: fromCheckpoint}:
	case <-p.ctx.Done():
	}
}

func (p *pusher) reportPhase(event string) {
	if p.opts.Progress == nil {
		return
	}
	select {
	case p.opts.Progress <- Progress{Event: event}:
	case <-p.ctx.Done():
	}
}

// executeBlob uploads one blob, skipping it when already present.
func (p *pusher) executeBlob(t blobTask) error {
	if p.isDone(t.digest) {
		return nil
	}
	p.reportEvent(t, "checking", false, false)
	ok, err := p.blobExists(t.digest)
	if err != nil {
		return fmt.Errorf("checking blob %s: %w", t.digest, err)
	}
	if ok {
		if err := p.markDone(t.digest); err != nil {
			return err
		}
		p.report(t, true, false)
		return nil
	}
	p.reportEvent(t, "uploading", false, false)
	if err := p.uploadWithRetries(t); err != nil {
		return fmt.Errorf("blob %s: %w", t.digest, err)
	}
	if err := p.markDone(t.digest); err != nil {
		return err
	}
	p.report(t, false, false)
	return nil
}

func (p *pusher) blobExists(digest string) (bool, error) {
	req, err := http.NewRequestWithContext(p.ctx, http.MethodHead, p.base+"/blobs/"+digest, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, cnl := io.Copy(io.Discard, resp.Body)
	_ = cnl
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized:
		return false, &httpError{code: http.StatusUnauthorized, msg: fmt.Sprintf("registry %s: unauthorized", p.ref.Context().RegistryStr())}
	default:
		return false, &httpError{code: resp.StatusCode, msg: fmt.Sprintf("registry %s: HEAD answered %d", p.ref.Context().RegistryStr(), resp.StatusCode)}
	}
}

// uploadWithRetries runs the POST→PATCH*→PUT flow with a per-blob attempt
// budget: retry only on network errors, 5xx and 429 (honoring Retry-After).
func (p *pusher) uploadWithRetries(t blobTask) error {
	var lastErr error
	var wait time.Duration
	for attempt := 0; attempt <= p.opts.MaxRetries; attempt++ {
		if wait > 0 {
			if err := p.ctx.Err(); err != nil {
				return err
			}
			select {
			case <-time.After(wait):
			case <-p.ctx.Done():
				return p.ctx.Err()
			}
		}
		err := p.doUpload(t)
		if err == nil {
			return nil
		}
		lastErr = err
		var hr *httpError
		if errors.As(err, &hr) {
			switch {
			case hr.code == http.StatusForbidden:
				return err // never retry 403
			case hr.code == http.StatusTooManyRequests && hr.retryAfter > 0:
				wait = hr.retryAfter
				continue
			case hr.code >= 500:
				wait = backoffDelay(attempt + 1)
				continue
			default:
				return err
			}
		}
	}
	return fmt.Errorf("giving up after %d attempts: %w", p.opts.MaxRetries+1, lastErr)
}

type httpError struct {
	code       int
	msg        string
	retryAfter time.Duration
}

func (e *httpError) Error() string { return e.msg }

// doUpload performs one full upload attempt for t.
func (p *pusher) doUpload(t blobTask) error {
	rc, err := t.open()
	if err != nil {
		return err
	}
	defer rc.Close()

	// 1. start an upload session
	loc, err := p.startUpload()
	if err != nil {
		return err
	}
	if loc == "" {
		return &httpError{code: 0, msg: "registry: empty upload location"}
	}

	// 2. stream in PATCH chunks; a reader error aborts the session
	var sent int64
	buf := make([]byte, p.opts.ChunkSize)
	for {
		n, rerr := io.ReadFull(rc, buf)
		if n > 0 {
			loc, err = p.patch(loc, buf[:n])
			if err != nil {
				return err
			}
			sent += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			return rerr
		}
	}

	// 3. finalize
	if err := p.putFinal(loc, t.digest); err != nil {
		return err
	}
	return nil
}

func (p *pusher) startUpload() (string, error) {
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPost, p.base+"/blobs/uploads/", nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, cnl := io.Copy(io.Discard, resp.Body)
	_ = cnl
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		return "", &httpError{code: resp.StatusCode, msg: fmt.Sprintf("registry %s: start upload answered %d", p.ref.Context().RegistryStr(), resp.StatusCode), retryAfter: retryAfterFrom(resp)}
	}
	return resolveLocation(resp.Header.Get("Location"), p.base), nil
}

func (p *pusher) patch(loc string, chunk []byte) (string, error) {
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPatch, loc, bytes.NewReader(chunk))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(chunk))
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, cnl := io.Copy(io.Discard, resp.Body)
	_ = cnl
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", &httpError{code: resp.StatusCode, msg: "rate limited", retryAfter: retryAfterFrom(resp)}
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return "", &httpError{code: resp.StatusCode, msg: fmt.Sprintf("upload chunk answered %d", resp.StatusCode)}
	}
	next := resolveLocation(resp.Header.Get("Location"), p.base)
	if next == "" {
		next = loc
	}
	return next, nil
}

func (p *pusher) putFinal(loc, digest string) error {
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPut, loc+sep+"digest="+url.QueryEscape(digest), nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, cnl := io.Copy(io.Discard, resp.Body)
	_ = cnl
	if resp.StatusCode == http.StatusTooManyRequests {
		return &httpError{code: resp.StatusCode, msg: "rate limited", retryAfter: retryAfterFrom(resp)}
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return &httpError{code: resp.StatusCode, msg: fmt.Sprintf("finalize upload answered %d", resp.StatusCode)}
	}
	return nil
}

// publishManifests uploads the platform manifests and the index, tagging
// the index with the caller's reference.
func (p *pusher) publishManifests() error {
	digests := make([]string, 0, len(p.staged))
	for d := range p.staged {
		digests = append(digests, d)
	}
	sort.Strings(digests)
	for _, d := range digests {
		if d == p.taggedDigest {
			continue
		}
		m := p.staged[d]
		if err := p.putManifest(d, m); err != nil {
			return err
		}
	}
	if err := p.putManifest(p.taggedDigest, p.tagged); err != nil {
		return err
	}
	// tag the index at the caller's reference
	if err := p.putTaggedManifest(p.ref.Identifier(), p.tagged); err != nil {
		return err
	}
	return nil
}

func (p *pusher) putManifest(digest string, m rawManifest) error {
	return p.putTaggedManifest(digest, m)
}

func (p *pusher) putTaggedManifest(ident string, m rawManifest) error {
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPut, p.base+"/manifests/"+ident, strings.NewReader(string(m.body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", string(m.mediaType))
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, cnl := io.Copy(io.Discard, resp.Body)
	_ = cnl
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return &httpError{code: resp.StatusCode, msg: fmt.Sprintf("publish manifest %s answered %d", ident, resp.StatusCode)}
	}
	return nil
}

// helpers

func resolveLocation(loc, base string) string {
	if loc == "" {
		return ""
	}
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return loc
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return loc
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	return baseURL.ResolveReference(ref).String()
}

func retryAfterFrom(resp *http.Response) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 0
}

// backoffDelay returns 1s, 2s, 4s, 8s, 16s capped, with jitter.
func backoffDelay(attempt int) time.Duration {
	d := time.Duration(1<<(attempt-1)) * time.Second
	if d > 16*time.Second {
		d = 16 * time.Second
	}
	// gosec G404: jitter non serve a sicurezza.
	jitter := time.Duration(rand.Int63n(int64(d) / 4)) // #nosec G404
	return d + jitter
}
