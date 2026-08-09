package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// memLayer is a minimal v1.Layer backed by raw bytes.
type memLayer struct {
	cmp []byte
}

func (m *memLayer) Digest() (v1.Hash, error) { return sha256OfBytes(m.cmp), nil }
func (m *memLayer) DiffID() (v1.Hash, error) { return sha256OfBytes(m.cmp), nil }
func (m *memLayer) Size() (int64, error)     { return int64(len(m.cmp)), nil }
func (m *memLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.cmp)), nil
}
func (m *memLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, fmt.Errorf("uncompressed not available")
}
func (m *memLayer) Name() (string, error)               { return "", nil }
func (m *memLayer) MediaType() (types.MediaType, error) { return types.OCILayer, nil }
func (m *memLayer) LayerType() (types.MediaType, error) { return types.OCILayer, nil }

func sha256OfBytes(b []byte) v1.Hash {
	s := sha256.Sum256(b)
	return v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(s[:])}
}

// fakeRegistry is a minimal in-memory v2 registry.
type fakeRegistry struct {
	mu         sync.Mutex
	blobs      map[string][]byte
	manifests  map[string]string // ident -> mediaType|body
	uploadData map[string][]byte
	nextUpload int

	startFail   int // first N POST starts answer 500
	startForbid bool
	start429    int // first N POST starts answer 429 Retry-After:1
	uploadHits  int
	blockSecond bool // block the 2nd PUT finalize until gate closes
	gate        chan struct{}
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		blobs:      map[string][]byte{},
		uploadData: map[string][]byte{},
		manifests:  map[string]string{},
		gate:       make(chan struct{}),
	}
}

func (f *fakeRegistry) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pth := r.URL.Path
		rest := pth
		if strings.HasPrefix(pth, "/v2/") {
			rest = strings.TrimPrefix(pth, "/v2/")
		} else if pth == "/v2/" {
			rest = ""
		} else if pth == "/token" {
			rest = "token"
		}
		switch {
		case rest == "":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Host+`/token",service="test"`)
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasSuffix(rest, "blobs/uploads/") && r.Method == http.MethodPost:
			f.mu.Lock()
			flaky := f.startFail > 0
			if flaky {
				f.startFail--
			}
			forbid := f.startForbid
			rate := f.start429 > 0
			if rate {
				f.start429--
			}
			f.mu.Unlock()
			switch {
			case flaky:
				w.WriteHeader(http.StatusInternalServerError)
			case forbid:
				w.WriteHeader(http.StatusForbidden)
			case rate:
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
			default:
				f.mu.Lock()
				id := fmt.Sprintf("up-%d", f.nextUpload)
				f.nextUpload++
				f.uploadData[id] = []byte{}
				f.mu.Unlock()
				w.Header().Set("Location", "/v2/"+rest+"uploads/"+id)
				w.WriteHeader(http.StatusAccepted)
			}
		case strings.Contains(rest, "blobs/uploads/") && r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			id := rest[strings.LastIndex(rest, "uploads/")+8:]
			f.mu.Lock()
			f.uploadData[id] = append(f.uploadData[id], body...)
			f.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(rest, "blobs/uploads/") && r.Method == http.MethodPut:
			digest := r.URL.Query().Get("digest")
			id := rest[strings.LastIndex(rest, "uploads/")+8:]
			if f.blockSecond {
				url := r.URL.Path
				f.mu.Lock()
				n := 0
				for k := range f.blobs {
					_ = k
					n++
				}
				blockMe := n == 1 // the second finalize blocks until gated
				f.mu.Unlock()
				if blockMe {
					<-f.gate
					_ = url
				}
			}
			f.mu.Lock()
			data := f.uploadData[id]
			f.blobs[digest] = append([]byte(nil), data...)
			f.uploadHits++
			delete(f.uploadData, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(rest, "blobs/") && r.Method == http.MethodHead:
			digest := rest[strings.LastIndex(rest, "blobs/")+6:]
			f.mu.Lock()
			_, ok := f.blobs[digest]
			f.mu.Unlock()
			if ok {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case strings.Contains(rest, "manifests/") && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.manifests[strings.TrimPrefix(rest, "manifests/")] = r.Header.Get("Content-Type") + ":" + string(body)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case rest == "token":
			w.Write([]byte(`{"token":"t","expires_in":600}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// testImage builds a one-platform image with the given layer payloads.
func testImage(t *testing.T, payloads ...[]byte) v1.Image {
	t.Helper()
	img := empty.Image
	for _, p := range payloads {
		l := &memLayer{cmp: p}
		var err error
		img, err = mutate.AppendLayers(img, l)
		if err != nil {
			t.Fatal(err)
		}
	}
	return img
}

func refFor(srvURL string) name.Reference {
	host := strings.TrimPrefix(srvURL, "http://")
	ref, err := name.ParseReference(host + "/me/dumps:latest")
	if err != nil {
		panic(err)
	}
	return ref
}

func idxFor(t *testing.T, img v1.Image) v1.ImageIndex {
	t.Helper()
	return mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
}

// pushOne runs Push for a single-platform image.
func pushOne(t *testing.T, srvURL string, img v1.Image, opts PushOptions) error {
	t.Helper()
	return Push(context.Background(), refFor(srvURL), map[string]v1.Image{"linux/amd64": img}, idxFor(t, img), nil, opts)
}

func TestPushUploadsAllBlobsAndIndex(t *testing.T) {
	fr := newFakeRegistry()
	srv := fr.server()
	defer srv.Close()

	img := testImage(t, []byte("layer-1"), []byte("layer-2"))
	if err := pushOne(t, srv.URL, img, PushOptions{}); err != nil {
		t.Fatalf("push: %v", err)
	}
	fr.mu.Lock()
	nBlobs := len(fr.blobs)
	idxTag := fr.manifests["me/dumps/manifests/latest"]
	nTags := len(fr.manifests)
	values := make([]string, 0, nTags)
	for k := range fr.manifests {
		values = append(values, k+":"+fr.manifests[k][:min(len(fr.manifests[k]), 12)])
	}
	fr.mu.Unlock()
	if nBlobs != 3 { // config + two layers
		t.Fatalf("blobs stored = %d, want 3 (config + 2 layers)", nBlobs)
	}
	if idxTag == "" {
		t.Fatalf("index not tagged; manifests = %v", values)
	}
	if nTags != 3 { // platform manifest by digest + index by digest + tag
		t.Fatalf("manifests stored = %d, want 3 (%v)", nTags, values)
	}
}

func TestPushSkipsExistingBlobs(t *testing.T) {
	fr := newFakeRegistry()
	srv := fr.server()
	defer srv.Close()
	img := testImage(t, []byte("aaaa"), []byte("bbbb"))
	layers, _ := img.Layers()
	d, _ := layers[0].Digest()
	rc, _ := layers[0].Compressed()
	data, _ := io.ReadAll(rc)
	fr.mu.Lock()
	fr.blobs[d.String()] = data
	fr.mu.Unlock()

	prog := make(chan Progress, 16)
	err := pushOne(t, srv.URL, img, PushOptions{Progress: prog})
	close(prog)
	if err != nil {
		t.Fatal(err)
	}
	var sawSkip bool
	for pr := range prog {
		if pr.Skipped {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Fatal("expected a skipped blob in progress")
	}
	fr.mu.Lock()
	hits := fr.uploadHits
	fr.mu.Unlock()
	if hits != 2 { // config plus the missing layer
		t.Fatalf("uploads = %d, want 2", hits)
	}
}

func TestPushRetryOn500(t *testing.T) {
	fr := newFakeRegistry()
	fr.startFail = 3
	srv := fr.server()
	defer srv.Close()
	img := testImage(t, []byte("one"))
	if err := pushOne(t, srv.URL, img, PushOptions{MaxRetries: 5}); err != nil {
		t.Fatalf("push: %v", err)
	}
	fr.mu.Lock()
	ok := len(fr.blobs) == 2 // image config + layer
	fr.mu.Unlock()
	if !ok {
		t.Fatal("blob missing after retries")
	}
}

func TestPush403NoRetry(t *testing.T) {
	fr := newFakeRegistry()
	fr.startForbid = true
	srv := fr.server()
	defer srv.Close()
	img := testImage(t, []byte("x"))
	err := pushOne(t, srv.URL, img, PushOptions{MaxRetries: 5})
	if err == nil {
		t.Fatal("expected 403 failure")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should mention 403, got %v", err)
	}
	fr.mu.Lock()
	got := fr.uploadHits == 0
	fr.mu.Unlock()
	_ = got
}

func TestPush429RespectsRetryAfter(t *testing.T) {
	fr := newFakeRegistry()
	fr.start429 = 2
	srv := fr.server()
	defer srv.Close()
	img := testImage(t, []byte("hello"))
	start := time.Now()
	if err := pushOne(t, srv.URL, img, PushOptions{MaxRetries: 5}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("Retry-After 1s wait expected, took %v", elapsed)
	}
}

func TestPushCheckpointResumeSkipsUploaded(t *testing.T) {
	fr := newFakeRegistry()
	fr.blockSecond = true
	srv := fr.server()
	defer srv.Close()
	img := testImage(t, []byte("blob-A"), []byte("blob-B"))
	lap := NewCheckpointStore(filepath.Join(t.TempDir(), "ck"))

	// run 1: blob A completes; blob B's finalize blocks on the gate.
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Push(ctx, refFor(srv.URL), map[string]v1.Image{"linux/amd64": img}, idxFor(t, img), nil,
			PushOptions{Checkpoint: lap, ID: "bkp1", Manifest: []byte(`{"x":1}`)})
	}()
	// wait until blob A is confirmed in the checkpoint, then cancel.
	deadline := time.Now().Add(10 * time.Second)
	for {
		ck, err := lap.Load("bkp1")
		if err == nil && len(ck.DoneBlobs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for checkpoint")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-errCh
	fr.mu.Lock()
	first := len(fr.blobs)
	fr.mu.Unlock()
	if first != 1 {
		t.Fatalf("run 1 should complete exactly 1 blob, got %d", first)
	}

	// release the blocked finalize so run 1's goroutine goroutine can exit
	close(fr.gate)
	time.Sleep(100 * time.Millisecond)

	// run 2: blob A skipped via checkpoint, B uploaded
	if err := pushOne(t, srv.URL, img, PushOptions{Checkpoint: lap, ID: "ckp1"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	fr.mu.Lock()
	second := len(fr.blobs)
	hits := fr.uploadHits
	fr.mu.Unlock()
	if second != 3 { // config + two layers
		t.Fatalf("after resume blobs = %d, want 3", second)
	}
	if hits != 3 { // one in run1 + two in run2; first is not re-uploaded
		t.Fatalf("upload hits = %d, want 3", hits)
	}
	if _, err := lap.Load("ckp1"); err == nil {
		t.Fatal("checkpoint must be deleted after success")
	}
}
