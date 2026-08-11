package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

func TestBlobClientStreamsChunksAndCommits(t *testing.T) {
	fr := newFakeRegistry()
	srv := fr.server()
	defer srv.Close()
	ref := blobTestRef(t, srv.URL)
	provider := NewStaticProvider(func(_ context.Context, scope Scope) (*Token, error) {
		return &Token{Value: "token", ExpiresAt: time.Now().Add(time.Hour), Scope: scope}, nil
	})
	client, err := NewBlobClient(context.Background(), ref, provider, 3)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("abcdefgh")
	digest := testBlobDigest(data)
	upload, err := client.Open(digest)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := upload.Write(data); err != nil || n != len(data) {
		t.Fatalf("write = %d, %v", n, err)
	}
	if err := upload.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := upload.Commit(context.Background()); err != nil {
		t.Fatalf("idempotent commit: %v", err)
	}
	if _, err := upload.Write([]byte("x")); err == nil {
		t.Fatal("write after commit succeeded")
	}
	exists, err := client.Exists(digest)
	if err != nil || !exists {
		t.Fatalf("exists = %v, %v", exists, err)
	}
	fr.mu.Lock()
	got := append([]byte(nil), fr.blobs[digest]...)
	fr.mu.Unlock()
	if string(got) != string(data) {
		t.Fatalf("blob = %q", got)
	}
}

func TestBlobClientValidationAndAbort(t *testing.T) {
	ref, _ := name.ParseReference("localhost:5000/repo:t", name.Insecure)
	if _, err := NewBlobClient(context.Background(), ref, nil, 1); err == nil {
		t.Fatal("nil provider accepted")
	}
	provider := NewStaticProvider(func(context.Context, Scope) (*Token, error) {
		return &Token{Value: "x", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	if _, err := NewBlobClient(context.Background(), ref, provider, 65<<20); err == nil {
		t.Fatal("oversize chunk accepted")
	}

	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
			w.Header().Set("Location", srvURL(r)+"/upload/1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && r.URL.Path == "/upload/1":
			deletes++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client, err := NewBlobClient(context.Background(), blobTestRef(t, srv.URL), provider, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open("bad"); err == nil {
		t.Fatal("invalid digest accepted")
	}
	upload, err := client.Open(testBlobDigest([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if err := upload.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := upload.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("DELETE count = %d", deletes)
	}
	if _, err := upload.Write([]byte("x")); err == nil {
		t.Fatal("write after abort succeeded")
	}
	if err := upload.Commit(context.Background()); err == nil {
		t.Fatal("commit after abort succeeded")
	}
}

func TestBlobUploadStickyPatchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", srvURL(r)+"/upload")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	provider := NewStaticProvider(func(context.Context, Scope) (*Token, error) {
		return &Token{Value: "x", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	client, _ := NewBlobClient(context.Background(), blobTestRef(t, srv.URL), provider, 2)
	upload, err := client.Open(testBlobDigest([]byte("abc")))
	if err != nil {
		t.Fatal(err)
	}
	// A PATCH runs in the background, so its failure surfaces at the next
	// flush boundary at the latest — never later than Commit, and never
	// swallowed.
	var writeErr error
	for i := 0; i < 8 && writeErr == nil; i++ {
		_, writeErr = upload.Write([]byte("abc"))
	}
	if writeErr == nil {
		t.Error("PATCH error never surfaced through Write")
	}
	if err := upload.Commit(context.Background()); err == nil {
		t.Fatal("sticky error missing")
	}
	if _, err := upload.Write([]byte("x")); err == nil {
		t.Fatal("write after a failed upload succeeded")
	}
}

// TestBlobUploadReportsPatchErrorAtCommit pins the weakest case: a blob small
// enough that its only PATCH is the one Commit issues.
func TestBlobUploadReportsPatchErrorAtCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", srvURL(r)+"/upload")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	provider := NewStaticProvider(func(context.Context, Scope) (*Token, error) {
		return &Token{Value: "x", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	client, _ := NewBlobClient(context.Background(), blobTestRef(t, srv.URL), provider, 1<<20)
	upload, err := client.Open(testBlobDigest([]byte("abc")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Write([]byte("abc")); err != nil {
		t.Fatalf("buffered write: %v", err)
	}
	if err := upload.Commit(context.Background()); err == nil {
		t.Fatal("commit hid the PATCH failure")
	}
}

func blobTestRef(t *testing.T, serverURL string) name.Reference {
	t.Helper()
	ref, err := name.ParseReference(strings.TrimPrefix(serverURL, "http://")+"/repo:t", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func testBlobDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func srvURL(r *http.Request) string { return "http://" + r.Host }
