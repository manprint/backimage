package registry

import (
	"bytes"
	"strings"
	"testing"
)

// The read-back is the whole point of the post-push verification: a registry
// that accepts an upload and then does not hold it must fail the backup, not
// let it exit 0.
func TestPushVerificationCatchesLostBlob(t *testing.T) {
	fr := newFakeRegistry()
	fr.dropAfterPublish = true
	srv := fr.server()
	defer srv.Close()

	err := pushOne(t, srv.URL, testImage(t, bytes.Repeat([]byte("a"), 32)), PushOptions{Jobs: 1})
	if err == nil {
		t.Fatal("a registry that lost a blob must fail the push")
	}
	if !strings.Contains(err.Error(), "non conserva il blob") {
		t.Fatalf("error must name the missing blob: %v", err)
	}
}

func TestPushVerificationCatchesWrongStoredSize(t *testing.T) {
	fr := newFakeRegistry()
	fr.resizeAfterPublish = true
	srv := fr.server()
	defer srv.Close()

	err := pushOne(t, srv.URL, testImage(t, bytes.Repeat([]byte("b"), 32)), PushOptions{Jobs: 1})
	if err == nil {
		t.Fatal("a blob stored with another length must fail the push")
	}
	if !strings.Contains(err.Error(), "byte invece di") {
		t.Fatalf("error must report both sizes: %v", err)
	}
}

func TestPushVerificationCatchesTamperedManifest(t *testing.T) {
	fr := newFakeRegistry()
	fr.tamperManifest = true
	srv := fr.server()
	defer srv.Close()

	err := pushOne(t, srv.URL, testImage(t, bytes.Repeat([]byte("c"), 32)), PushOptions{Jobs: 1})
	if err == nil {
		t.Fatal("a mutated manifest body must fail the push")
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("error must name the manifest: %v", err)
	}
}

// VerifyOff keeps the old behaviour for callers that cannot afford the extra
// round trips.
func TestPushVerificationCanBeDisabled(t *testing.T) {
	fr := newFakeRegistry()
	fr.dropAfterPublish = true
	srv := fr.server()
	defer srv.Close()

	if err := pushOne(t, srv.URL, testImage(t, bytes.Repeat([]byte("d"), 32)),
		PushOptions{Jobs: 1, Verify: VerifyOff}); err != nil {
		t.Fatalf("with VerifyOff the push must not read anything back: %v", err)
	}
}

// A blob the registry already claims to hold, but with a different length, is
// re-uploaded instead of being trusted.
func TestPushReuploadsBlobStoredWithAnotherSize(t *testing.T) {
	fr := newFakeRegistry()
	srv := fr.server()
	defer srv.Close()

	payload := bytes.Repeat([]byte("e"), 64)
	img := testImage(t, payload)
	// Seed every blob of the image with a truncated body: the HEAD says
	// "present" and the size says "not this one".
	if err := pushOne(t, srv.URL, img, PushOptions{Jobs: 1, Verify: VerifyOff}); err != nil {
		t.Fatal(err)
	}
	fr.mu.Lock()
	for digest, body := range fr.blobs {
		fr.blobs[digest] = body[:len(body)/2]
	}
	fr.published = false
	before := fr.uploadHits
	fr.mu.Unlock()

	if err := pushOne(t, srv.URL, img, PushOptions{Jobs: 1}); err != nil {
		t.Fatalf("the second push must repair the short blobs: %v", err)
	}
	fr.mu.Lock()
	after := fr.uploadHits
	fr.mu.Unlock()
	if after <= before {
		t.Fatalf("uploads = %d then %d: the short blobs were trusted instead of re-sent", before, after)
	}
}
