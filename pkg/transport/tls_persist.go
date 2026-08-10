package transport

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CertificatePin returns the lowercase SHA-256 fingerprint of the leaf
// certificate, the value a client passes to PinnedClientTLS.
func CertificatePin(cert tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", errors.New("TLS certificate carries no leaf")
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:]), nil
}

// LoadOrCreateSelfSigned returns the key pair at certPath/keyPath, generating
// and persisting a long-lived self-signed one when either file is missing.
// created reports whether this call wrote new files, so the caller can tell a
// stable pin from a fresh one. An ephemeral certificate would change the pin
// at every restart and break every pinning client.
func LoadOrCreateSelfSigned(certPath, keyPath string, hosts []string, now time.Time) (cert tls.Certificate, pin string, created bool, err error) {
	if certPath == "" || keyPath == "" {
		return tls.Certificate{}, "", false, errors.New("certificate and key path are both required")
	}
	certExists, err := regularFileExists(certPath)
	if err != nil {
		return tls.Certificate{}, "", false, err
	}
	keyExists, err := regularFileExists(keyPath)
	if err != nil {
		return tls.Certificate{}, "", false, err
	}
	if certExists != keyExists {
		return tls.Certificate{}, "", false, fmt.Errorf("incomplete TLS material: %s and %s must both exist or both be absent", certPath, keyPath)
	}
	if certExists {
		cert, err = tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, "", false, fmt.Errorf("load TLS key pair: %w", err)
		}
		pin, err = CertificatePin(cert)
		return cert, pin, false, err
	}

	cert, pin, err = SelfSignedCertificateFor(hosts, now, PersistentCertificateValidity)
	if err != nil {
		return tls.Certificate{}, "", false, err
	}
	if err := writeKeyPair(cert, certPath, keyPath); err != nil {
		return tls.Certificate{}, "", false, err
	}
	return cert, pin, true, nil
}

func writeKeyPair(cert tls.Certificate, certPath, keyPath string) error {
	der, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return fmt.Errorf("encode TLS key: %w", err)
	}
	for _, dir := range []string{filepath.Dir(certPath), filepath.Dir(keyPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	// The key is written first and at 0600: a readable certificate without a
	// key is recoverable, a world-readable key is not.
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", keyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil { // #nosec G306 -- a certificate is public by design.
		return fmt.Errorf("write %s: %w", certPath, err)
	}
	return nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return false, fmt.Errorf("%s is a directory", path)
	}
	return true, nil
}
