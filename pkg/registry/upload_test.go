package registry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// testPusher builds a pusher aimed at srv, bypassing Push so a test can drive
// a single blob and keep the fallback fixture small.
func testPusher(t *testing.T, srvURL string, opts PushOptions) *pusher {
	t.Helper()
	ref := refFor(srvURL)
	base, err := httpBase(ref.Context().RegistryStr())
	if err != nil {
		t.Fatalf("httpBase: %v", err)
	}
	scope := Scope{Repository: ref.Context().RepositoryStr(), Actions: []string{"pull", "push"}}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 2
	}
	return &pusher{
		ctx:    context.Background(),
		ref:    ref,
		base:   base + "/v2/" + ref.Context().RepositoryStr(),
		opts:   opts,
		done:   map[string]bool{},
		client: newRegistryClient(testTokenProvider(), scope),
	}
}

func blobTaskFor(payload []byte) blobTask {
	return blobTask{
		layer:  0,
		digest: sha256OfBytes(payload).String(),
		size:   int64(len(payload)),
		open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(payload)), nil
		},
	}
}

func (f *fakeRegistry) patchShape() ([]int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.patchBodies...), append([]string(nil), f.patchTE...)
}

// TestPushSendsOneRequestPerBlob is the regression guard for the throughput
// fix: by default a blob must cost one PATCH, not one per chunk, and it must
// carry a Content-Length rather than chunked transfer encoding.
func TestPushSendsOneRequestPerBlob(t *testing.T) {
	fr := newFakeRegistry()
	srv := fr.server()
	defer srv.Close()

	payloads := [][]byte{bytes.Repeat([]byte("a"), 5000), bytes.Repeat([]byte("b"), 9000)}
	img := testImage(t, payloads...)
	if err := pushOne(t, srv.URL, img, PushOptions{Jobs: 1}); err != nil {
		t.Fatalf("push: %v", err)
	}

	bodies, encodings := fr.patchShape()
	if len(bodies) != 3 { // config + two layers
		t.Fatalf("PATCH requests = %d %v, want 3 (one per blob)", len(bodies), bodies)
	}
	// Every PATCH must carry a whole blob: the two layers verbatim, plus the
	// config. Any chunking would show up as more, smaller bodies.
	whole := map[int]bool{5000: true, 9000: true}
	for i, size := range bodies {
		if size == 0 {
			t.Errorf("PATCH %d carried an empty body", i)
		}
		delete(whole, size)
	}
	if len(whole) != 0 {
		t.Errorf("PATCH bodies %v do not contain each layer in one piece; missing sizes %v", bodies, whole)
	}
	for i, te := range encodings {
		if te != "" {
			t.Errorf("PATCH %d used transfer encoding %q; registries need a Content-Length", i, te)
		}
	}
	fr.mu.Lock()
	stored := len(fr.blobs)
	fr.mu.Unlock()
	if stored != 3 {
		t.Fatalf("blobs stored = %d, want 3", stored)
	}
}

// TestSingleRequestUploadIsNotChunkedAtAnySize sends a blob far larger than
// any chunk default. The payload is generated and discarded, so the test
// stays cheap while still proving that size alone never reintroduces
// per-chunk round trips.
func TestSingleRequestUploadIsNotChunkedAtAnySize(t *testing.T) {
	const size = 40 << 20
	var mu sync.Mutex
	var patches int
	var received int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", srvURL(r)+"/upload")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPatch:
			n, _ := io.Copy(io.Discard, r.Body)
			mu.Lock()
			patches++
			received += n
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	p := testPusher(t, srv.URL, PushOptions{})
	task := blobTask{
		digest: sha256OfBytes([]byte("large")).String(),
		size:   size,
		open: func() (io.ReadCloser, error) {
			return io.NopCloser(io.LimitReader(zeroReader{}, size)), nil
		},
	}
	if err := p.doUpload(task); err != nil {
		t.Fatalf("upload: %v", err)
	}
	mu.Lock()
	gotPatches, gotBytes := patches, received
	mu.Unlock()
	if gotPatches != 1 {
		t.Errorf("PATCH requests = %d for a %d MiB blob, want 1", gotPatches, size>>20)
	}
	if gotBytes != size {
		t.Errorf("registry received %d bytes, want %d", gotBytes, size)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

// TestUploadChunkSizeDefaults pins the semantics of the option itself, so a
// future default cannot quietly reintroduce per-chunk round trips.
func TestUploadChunkSizeDefaults(t *testing.T) {
	p := &pusher{}
	if got := p.uploadChunkSize(); got != 0 {
		t.Errorf("default chunk size = %d, want 0 (one request per blob)", got)
	}
	if got := p.fallbackChunkBytes(); got != fallbackChunkSize {
		t.Errorf("fallback chunk = %d, want %d", got, fallbackChunkSize)
	}
	p.opts.ChunkSize = 1 << 20
	if got := p.uploadChunkSize(); got != 1<<20 {
		t.Errorf("configured chunk size = %d, want 1 MiB", got)
	}
	p.chunkFallback.Store(4096)
	if got := p.uploadChunkSize(); got != 4096 {
		t.Errorf("after a 413 the chunk size = %d, want the latched 4096", got)
	}
}

// TestPushChunksWhenAsked keeps the chunked path working for the registries
// that need it.
func TestPushChunksWhenAsked(t *testing.T) {
	fr := newFakeRegistry()
	srv := fr.server()
	defer srv.Close()

	payload := bytes.Repeat([]byte("z"), 10)
	img := testImage(t, payload)
	if err := pushOne(t, srv.URL, img, PushOptions{Jobs: 1, ChunkSize: 4}); err != nil {
		t.Fatalf("push: %v", err)
	}
	bodies, _ := fr.patchShape()
	for _, size := range bodies {
		if size > 4 {
			t.Fatalf("PATCH body = %d bytes, above the requested 4 (%v)", size, bodies)
		}
	}
	if len(bodies) < 3 {
		t.Fatalf("PATCH requests = %v, want the 10 byte layer split in 3", bodies)
	}
	fr.mu.Lock()
	got := string(fr.blobs[sha256OfBytes(payload).String()])
	fr.mu.Unlock()
	if got != string(payload) {
		t.Fatalf("stored layer = %q, want %q", got, payload)
	}
}

// TestUploadFallsBackToChunksOn413 covers the registry that caps the request
// body: the blob must still land, and the rest of the push must not retry the
// single-request shape.
func TestUploadFallsBackToChunksOn413(t *testing.T) {
	fr := newFakeRegistry()
	fr.maxPatchBody = 4
	srv := fr.server()
	defer srv.Close()

	p := testPusher(t, srv.URL, PushOptions{})
	p.fallbackChunk = 4

	payload := []byte("0123456789")
	if err := p.executeBlob(blobTaskFor(payload)); err != nil {
		t.Fatalf("upload with 413 fallback: %v", err)
	}
	fr.mu.Lock()
	got := string(fr.blobs[sha256OfBytes(payload).String()])
	fr.mu.Unlock()
	if got != string(payload) {
		t.Fatalf("stored blob = %q, want %q", got, payload)
	}
	if p.chunkFallback.Load() != 4 {
		t.Errorf("chunk fallback = %d, want it latched at 4", p.chunkFallback.Load())
	}

	// The next blob must go straight to chunks: no oversized PATCH at all.
	second := []byte("abcdefgh")
	if err := p.executeBlob(blobTaskFor(second)); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	bodies, _ := fr.patchShape()
	oversized := 0
	for _, size := range bodies {
		if size > 4 {
			oversized++
		}
	}
	if oversized != 1 {
		t.Errorf("oversized PATCH requests = %d, want exactly the first attempt (%v)", oversized, bodies)
	}
}

// TestUploadReplaysStreamedBodyAfter401 pins the replay contract: a streamed
// request has no buffer to rewind, so it must carry GetBody or the blob would
// be truncated after a token refresh.
func TestUploadReplaysStreamedBodyAfter401(t *testing.T) {
	fr := newFakeRegistry()
	fr.unauthPatch = 1
	srv := fr.server()
	defer srv.Close()

	p := testPusher(t, srv.URL, PushOptions{})
	payload := bytes.Repeat([]byte("payload"), 512)
	if err := p.executeBlob(blobTaskFor(payload)); err != nil {
		t.Fatalf("upload across a 401: %v", err)
	}
	fr.mu.Lock()
	got := append([]byte(nil), fr.blobs[sha256OfBytes(payload).String()]...)
	fr.mu.Unlock()
	if !bytes.Equal(got, payload) {
		t.Fatalf("stored blob = %d bytes, want the full %d", len(got), len(payload))
	}
}

// TestBlobUploadOverlapsPatchWithWrites proves the double buffer: a Write that
// follows a full buffer must return while the PATCH of that buffer is still in
// flight. Without the overlap this test blocks until the timeout.
func TestBlobUploadOverlapsPatchWithWrites(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var recvMu sync.Mutex
	var received [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", srvURL(r)+"/upload")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			select {
			case entered <- struct{}{}:
				<-release // only the first PATCH waits
			default:
			}
			recvMu.Lock()
			received = append(received, body)
			recvMu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	provider := NewStaticProvider(func(context.Context, Scope) (*Token, error) {
		return &Token{Value: "x", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	client, err := NewBlobClient(context.Background(), blobTestRef(t, srv.URL), provider, 4)
	if err != nil {
		t.Fatal(err)
	}
	upload, err := client.Open(testBlobDigest([]byte("whatever")))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, werr := upload.Write([]byte("aaaabbbb")) // fills one buffer, then refills
		done <- werr
	}()
	select {
	case werr := <-done:
		if werr != nil {
			t.Fatalf("write: %v", werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked on the in-flight PATCH: the upload is not double buffered")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first PATCH never started")
	}
	close(release)
	if err := upload.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var all []byte
	recvMu.Lock()
	defer recvMu.Unlock()
	for _, part := range received {
		all = append(all, part...)
	}
	if string(all) != "aaaabbbb" {
		t.Fatalf("registry received %q, want the whole stream in order", all)
	}
}

// TestBlobUploadKeepsOrderAcrossManyChunks guards the buffer swap against
// losing or reordering data.
func TestBlobUploadKeepsOrderAcrossManyChunks(t *testing.T) {
	fr := newFakeRegistry()
	srv := fr.server()
	defer srv.Close()
	provider := NewStaticProvider(func(context.Context, Scope) (*Token, error) {
		return &Token{Value: "x", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	client, err := NewBlobClient(context.Background(), blobTestRef(t, srv.URL), provider, 7)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1000)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	digest := testBlobDigest(payload)
	upload, err := client.Open(digest)
	if err != nil {
		t.Fatal(err)
	}
	// Write in irregular slices so buffer boundaries fall mid-slice.
	for off := 0; off < len(payload); off += 13 {
		end := min(off+13, len(payload))
		if _, err := upload.Write(payload[off:end]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	if err := upload.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	fr.mu.Lock()
	got := append([]byte(nil), fr.blobs[digest]...)
	fr.mu.Unlock()
	if !bytes.Equal(got, payload) {
		t.Fatalf("stored blob differs: %d bytes stored, want %d", len(got), len(payload))
	}
}
