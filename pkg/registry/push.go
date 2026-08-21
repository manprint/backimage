package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"sync/atomic"
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

// fallbackChunkSize is used when a registry refuses a single-request upload
// with 413, and by callers that ask for chunking without naming a size.
const fallbackChunkSize = 32 << 20

// PushOptions tunes the upload.
type PushOptions struct {
	Jobs int // parallel blob uploads, default 3
	// ChunkSize is the HTTP PATCH chunk size. Zero, the default, sends each
	// blob as one streamed request instead: chunking costs a full round trip
	// per chunk, and registries normally persist a chunk to their backing
	// store before answering, which caps a push at a fraction of the link
	// speed. Set it only for a registry that refuses large bodies; a 413 also
	// switches the running push over on its own.
	ChunkSize  int64
	MaxRetries int // per blob attempt budget, default 5
	// Verify decides what is re-read from the registry once everything has
	// been published. VerifyQuick, the default, proves presence, size and
	// digest of every blob and manifest without downloading any data.
	Verify     VerifyLevel
	Checkpoint CheckpointStore
	ID         string // deterministic checkpoint id
	Manifest   []byte // manifest.json bytes carried by the checkpoint
	Progress   chan<- Progress
}

// VerifyLevel selects the post-push read-back performed by Push.
type VerifyLevel int

const (
	// VerifyQuick re-reads only metadata: one HEAD per blob (presence, size
	// and, when the registry sends it, Docker-Content-Digest) plus a GET of
	// every manifest whose body is rehashed, and the tag resolution. Costs a
	// few KB and no disk. It is the default because it closes the two holes a
	// push otherwise leaves open: a blob skipped because the registry said it
	// already existed, and a blob skipped because a checkpoint said so — in
	// both cases nothing had ever confirmed what the registry really holds.
	VerifyQuick VerifyLevel = iota
	// VerifyOff publishes without reading anything back.
	VerifyOff
)

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
		client:       newRegistryClient(NewProvider(ref.Context().RegistryStr(), auth), scope),
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
		client:       newRegistryClient(provider, scope),
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

	// chunkFallback is set once a registry rejects a single-request upload,
	// so the remaining blobs of the same push do not repeat the attempt.
	chunkFallback atomic.Int64
	// fallbackChunk overrides the size used by that fallback; zero means
	// fallbackChunkSize. Tests set it to keep fixtures small.
	fallbackChunk int64
}

// fallbackChunkBytes is the chunk size used after a registry refuses a
// single-request upload.
func (p *pusher) fallbackChunkBytes() int64 {
	if p.fallbackChunk > 0 {
		return p.fallbackChunk
	}
	return fallbackChunkSize
}

// uploadChunkSize returns the chunk size in force, or 0 when blobs are sent
// in a single request.
func (p *pusher) uploadChunkSize() int64 {
	if v := p.chunkFallback.Load(); v > 0 {
		return v
	}
	return p.opts.ChunkSize
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
	if p.opts.Verify != VerifyOff {
		p.reportPhase("verifying")
		blobs, manifests, err := p.verifyPushed()
		if err != nil {
			return err
		}
		// Completed/Total carry the evidence the caller logs: how many blobs
		// and manifests were actually re-read, not just that a phase ran.
		p.reportCounts("verified", int64(blobs), int64(manifests))
	}
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

// reportCounts emits a phase event carrying two tallies, used as the audit
// evidence of the post-push verification.
func (p *pusher) reportCounts(event string, completed, total int64) {
	if p.opts.Progress == nil {
		return
	}
	select {
	case p.opts.Progress <- Progress{Event: event, Completed: completed, Total: total}:
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
	ok, size, err := p.blobExists(t.digest)
	if err != nil {
		return fmt.Errorf("checking blob %s: %w", t.digest, err)
	}
	// A blob whose stored size disagrees with ours is not the blob we mean to
	// publish, whatever its digest claims: re-upload instead of trusting it.
	// Compared only when the registry actually reported a length: some
	// answer a HEAD with Content-Length: 0, and no blob we publish is empty.
	if ok && size > 0 && size != t.size {
		p.reportEvent(t, "size-mismatch", false, false)
		ok = false
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

// blobExists reports whether the registry already holds digest, and the size
// it reports for it (-1 when the answer carries no Content-Length).
func (p *pusher) blobExists(digest string) (bool, int64, error) {
	req, err := http.NewRequestWithContext(p.ctx, http.MethodHead, p.base+"/blobs/"+digest, nil)
	if err != nil {
		return false, -1, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, -1, err
	}
	defer resp.Body.Close()
	_, cnl := io.Copy(io.Discard, resp.Body)
	_ = cnl
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted:
		size := int64(-1)
		if resp.ContentLength >= 0 {
			size = resp.ContentLength
		}
		if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != digest {
			return false, -1, &httpError{code: resp.StatusCode, msg: fmt.Sprintf(
				"registry %s: blob %s answered digest %s", p.ref.Context().RegistryStr(), digest, got)}
		}
		return true, size, nil
	case http.StatusNotFound:
		return false, -1, nil
	case http.StatusUnauthorized:
		return false, -1, &httpError{code: http.StatusUnauthorized, msg: fmt.Sprintf("registry %s: unauthorized", p.ref.Context().RegistryStr())}
	default:
		return false, -1, &httpError{code: resp.StatusCode, msg: fmt.Sprintf("registry %s: HEAD answered %d", p.ref.Context().RegistryStr(), resp.StatusCode)}
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

// doUpload performs one full upload attempt for t: one streamed request per
// blob, or chunked when the caller asked for it or a registry refused the
// body size.
func (p *pusher) doUpload(t blobTask) error {
	if size := p.uploadChunkSize(); size > 0 {
		return p.uploadChunked(t, size)
	}
	err := p.uploadSingle(t)
	var hr *httpError
	if errors.As(err, &hr) && hr.code == http.StatusRequestEntityTooLarge {
		// The registry caps the request body. Fall back for this blob and
		// for the rest of the push.
		size := p.fallbackChunkBytes()
		p.chunkFallback.CompareAndSwap(0, size)
		return p.uploadChunked(t, size)
	}
	return err
}

// uploadSingle sends the whole blob as one streamed PATCH: three round trips
// per blob instead of one per chunk, and the body never lands in memory.
func (p *pusher) uploadSingle(t blobTask) error {
	// Open first: a source that cannot be read must not leave an orphan
	// upload session behind on the registry.
	rc, err := t.open()
	if err != nil {
		return err
	}
	defer rc.Close()
	loc, err := p.startUpload()
	if err != nil {
		return err
	}
	if loc == "" {
		return &httpError{code: 0, msg: "registry: empty upload location"}
	}
	loc, err = p.patchStream(loc, rc, t.size, t.open)
	if err != nil {
		return err
	}
	return p.putFinal(loc, t.digest)
}

// uploadChunked sends the blob in PATCH chunks of at most size bytes. Each
// chunk costs a full round trip, so this is the fallback, not the default.
func (p *pusher) uploadChunked(t blobTask, size int64) error {
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
	buf := make([]byte, size)
	for {
		n, rerr := io.ReadFull(rc, buf)
		if n > 0 {
			loc, err = p.patch(loc, buf[:n])
			if err != nil {
				return err
			}
		}
		if rerr != nil {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			return rerr
		}
	}

	// 3. finalize
	return p.putFinal(loc, t.digest)
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
	req.ContentLength = int64(len(chunk))
	return p.sendPatch(loc, req)
}

// patchStream sends body as the request payload without buffering it. size
// must be the exact byte count: registries need a Content-Length, and a
// streamed body would otherwise be sent with chunked transfer encoding.
// reopen lets the bearer round tripper replay the request after a 401.
func (p *pusher) patchStream(loc string, body io.Reader, size int64, reopen func() (io.ReadCloser, error)) (string, error) {
	if size <= 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPatch, loc, body)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	if reopen != nil {
		req.GetBody = reopen
	}
	return p.sendPatch(loc, req)
}

// sendPatch performs one PATCH and returns the location of the next one.
func (p *pusher) sendPatch(loc string, req *http.Request) (string, error) {
	req.Header.Set("Content-Type", "application/octet-stream")
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

// verifyPushed re-reads what was just published. Nothing here downloads a
// data layer: a blob is proven by its HEAD (presence, size, and the digest the
// registry itself attributes to it), a manifest by fetching its body and
// rehashing it, and the tag by the digest it resolves to. Registries are
// required to validate the digest of a blob when the upload is finalised, so
// this pass is about what the registry *kept*, not about the transfer.
func (p *pusher) verifyPushed() (int, int, error) {
	blobs := 0
	for _, t := range p.tasks {
		if err := p.ctx.Err(); err != nil {
			return blobs, 0, err
		}
		ok, size, err := p.blobExists(t.digest)
		if err != nil {
			return blobs, 0, fmt.Errorf("verifica blob %s: %w", t.digest, err)
		}
		if !ok {
			return blobs, 0, fmt.Errorf("verifica: il registry non conserva il blob %s appena pubblicato", t.digest)
		}
		if size > 0 && size != t.size {
			return blobs, 0, fmt.Errorf("verifica: blob %s conservato con %d byte invece di %d", t.digest, size, t.size) //nolint:misspell // Messaggio in italiano.
		}
		blobs++
	}
	digests := make([]string, 0, len(p.staged))
	for d := range p.staged {
		digests = append(digests, d)
	}
	sort.Strings(digests)
	manifests := 0
	for _, d := range digests {
		if err := p.verifyManifest(d, p.staged[d]); err != nil {
			return blobs, manifests, err
		}
		manifests++
	}
	if err := p.verifyTag(); err != nil {
		return blobs, manifests, err
	}
	return blobs, manifests, nil
}

// verifyManifest fetches one manifest by digest and rehashes the body: the
// digest is recomputed locally, so a registry that rewrites or truncates a
// manifest cannot pass.
func (p *pusher) verifyManifest(digest string, m rawManifest) error {
	req, err := http.NewRequestWithContext(p.ctx, http.MethodGet, p.base+"/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", string(m.mediaType))
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("verifica manifest %s: %w", digest, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &httpError{code: resp.StatusCode, msg: fmt.Sprintf("verifica manifest %s: risposta %d", digest, resp.StatusCode)}
	}
	sum := sha256.Sum256(body)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != digest {
		return fmt.Errorf("verifica manifest %s: il body scaricato ha digest %s", digest, got)
	}
	if !bytes.Equal(body, m.body) {
		return fmt.Errorf("verifica manifest %s: il body scaricato differisce da quello pubblicato", digest)
	}
	return nil
}

// verifyTag proves that the caller's reference resolves to the index we
// published, not to an older or concurrent one.
func (p *pusher) verifyTag() error {
	ident := p.ref.Identifier()
	req, err := http.NewRequestWithContext(p.ctx, http.MethodHead, p.base+"/manifests/"+ident, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", string(p.tagged.mediaType))
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, cnl := io.Copy(io.Discard, resp.Body)
	_ = cnl
	if resp.StatusCode != http.StatusOK {
		return &httpError{code: resp.StatusCode, msg: fmt.Sprintf("verifica tag %s: risposta %d", ident, resp.StatusCode)}
	}
	// Not every registry sends the header on a HEAD; when it does it must
	// name the index we tagged.
	if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != p.taggedDigest {
		return fmt.Errorf("verifica tag %s: risolve a %s invece di %s", ident, got, p.taggedDigest)
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
