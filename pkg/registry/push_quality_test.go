package registry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
)

func newDirectPusher(t *testing.T, handler http.Handler) (*pusher, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	ref, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://") + "/me/repo:t")
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return &pusher{
		ctx:    context.Background(),
		ref:    ref,
		base:   srv.URL + "/v2/me/repo",
		client: srv.Client(),
		done:   map[string]bool{},
		opts:   PushOptions{MaxRetries: 1},
	}, srv.Close
}

func TestPusherHTTPStatusBranches(t *testing.T) {
	statuses := map[string]int{
		"/ok":      http.StatusOK,
		"/accept":  http.StatusAccepted,
		"/missing": http.StatusNotFound,
		"/unauth":  http.StatusUnauthorized,
		"/error":   http.StatusInternalServerError,
	}
	p, closeFn := newDirectPusher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := "/" + strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
		status := statuses[first]
		if status == 0 {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
	}))
	defer closeFn()
	root := strings.TrimSuffix(p.base, "/v2/me/repo")
	for path, want := range map[string]bool{"/ok": true, "/accept": true, "/missing": false} {
		p.base = root + path + "/x"
		got, _, err := p.blobExists("sha256:x")
		if err != nil || got != want {
			t.Fatalf("blobExists(%s) = %v, %v", path, got, err)
		}
	}
	for _, path := range []string{"/unauth", "/error"} {
		p.base = root + path + "/x"
		if _, _, err := p.blobExists("sha256:x"); err == nil {
			t.Fatalf("blobExists(%s) should fail", path)
		}
	}
}

func TestPusherPatchAndFinalizeBranches(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusNoContent)
	p, closeFn := newDirectPusher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rate" {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer closeFn()
	root := strings.TrimSuffix(p.base, "/v2/me/repo")
	next, err := p.patch(root+"/patch", []byte("chunk"))
	if err != nil || next != root+"/patch" {
		t.Fatalf("patch no-content = %q, %v", next, err)
	}
	if err := p.putFinal(root+"/finish", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	status.Store(http.StatusTooManyRequests)
	if err := p.putFinal(root+"/rate", "sha256:x"); err == nil {
		t.Fatal("429 finalize accepted")
	}
	if _, err := p.patch(root+"/rate", []byte("x")); err == nil {
		t.Fatal("429 patch accepted")
	}
	status.Store(http.StatusBadRequest)
	if err := p.putFinal(root+"/finish", "sha256:x"); err == nil {
		t.Fatal("400 finalize accepted")
	}
	if _, err := p.patch(root+"/patch", []byte("x")); err == nil {
		t.Fatal("400 patch accepted")
	}
}

func TestPusherStartAndExecuteErrorBranches(t *testing.T) {
	var mode atomic.Int64
	p, closeFn := newDirectPusher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 0:
			w.Header().Set("Location", "/upload/session")
			w.WriteHeader(http.StatusCreated)
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer closeFn()
	loc, err := p.startUpload()
	if err != nil || !strings.HasSuffix(loc, "/upload/session") {
		t.Fatalf("start upload = %q, %v", loc, err)
	}
	mode.Store(1)
	if _, err := p.startUpload(); err == nil {
		t.Fatal("500 start upload accepted")
	}
	mode.Store(2)
	if err := p.doUpload(blobTask{digest: "sha256:x", open: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("x")), nil
	}}); err == nil || !strings.Contains(err.Error(), "empty upload location") {
		t.Fatalf("empty upload location error = %v", err)
	}
	if err := p.doUpload(blobTask{open: func() (io.ReadCloser, error) { return nil, errors.New("open failed") }}); err == nil {
		t.Fatal("blob open error swallowed")
	}
	p.done["sha256:done"] = true
	if err := p.executeBlob(blobTask{digest: "sha256:done"}); err != nil {
		t.Fatalf("completed blob should be skipped: %v", err)
	}
}

func TestPushRejectsBrokenDependencies(t *testing.T) {
	fr := newFakeRegistry()
	srv := fr.server()
	defer srv.Close()
	img := testImage(t, []byte("layer"))
	ref := refFor(srv.URL)
	err := Push(context.Background(), ref, map[string]v1.Image{"linux/amd64": img}, idxFor(t, img), NewKeychain(nil, failingStore{}), PushOptions{})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("keychain failure = %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Push(context.Background(), ref, map[string]v1.Image{"linux/amd64": img}, idxFor(t, img), nil,
		PushOptions{Checkpoint: NewCheckpointStore(dir), ID: "broken"})
	if err == nil || !strings.Contains(err.Error(), "parsing checkpoint") {
		t.Fatalf("corrupt checkpoint failure = %v", err)
	}
}

func TestPushHelpers(t *testing.T) {
	base := "http://example.test/v2/repo"
	if resolveLocation("", base) != "" ||
		resolveLocation("https://cdn.test/u", base) != "https://cdn.test/u" ||
		resolveLocation("/v2/repo/u", base) != "http://example.test/v2/repo/u" {
		t.Fatal("location resolution failed")
	}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("Retry-After", "2")
	if retryAfterFrom(resp) != 2*time.Second {
		t.Fatal("Retry-After seconds not parsed")
	}
	resp.Header.Set("Retry-After", "bogus")
	if retryAfterFrom(resp) != 0 {
		t.Fatal("invalid Retry-After accepted")
	}
	if d := backoffDelay(8); d < 16*time.Second || d >= 20*time.Second {
		t.Fatalf("backoff cap/jitter = %v", d)
	}
}

func TestRoundTripperBypassAndRefreshAlias(t *testing.T) {
	var hits int
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		if r.Header.Get("Authorization") != "already-set" {
			return nil, errors.New("authorization overwritten")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	rt := NewRoundTripper(base, NewStaticProvider(nil), Scope{}).(*bearerAuth)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.test", nil)
	req.Header.Set("Authorization", "already-set")
	resp, err := rt.RefreshToken(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits != 1 {
		t.Fatalf("transport hits = %d", hits)
	}
}
