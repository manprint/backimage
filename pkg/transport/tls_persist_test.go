package transport

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteKeyPairRemovesKeyWhenCertificatePublishFails(t *testing.T) {
	cert, _, err := SelfSignedCertificateFor([]string{"localhost"}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certPath := t.TempDir() // Publishing a file over this directory must fail.
	keyPath := filepath.Join(t.TempDir(), "server.key")

	if err := writeKeyPair(cert, certPath, keyPath); err == nil {
		t.Fatal("writeKeyPair succeeded with a directory as certificate path")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("private key survived certificate failure: %v", err)
	}
}
