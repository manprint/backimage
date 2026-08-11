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
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	// Fully write both files before publishing either one. The renames keep
	// readers from observing partial PEM data; if the second rename fails, the
	// first published file is removed so the next start is not wedged on an
	// incomplete pair.
	keyTemp, err := writeTLSFileTemp(keyPath, keyPEM, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(keyTemp)
	certTemp, err := writeTLSFileTemp(certPath, certPEM, 0o644) // #nosec G306 -- a certificate is public by design.
	if err != nil {
		return err
	}
	defer os.Remove(certTemp)

	if err := os.Rename(keyTemp, keyPath); err != nil {
		return fmt.Errorf("publish %s: %w", keyPath, err)
	}
	if err := os.Rename(certTemp, certPath); err != nil {
		publishErr := fmt.Errorf("publish %s: %w", certPath, err)
		if cleanupErr := os.Remove(keyPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			return errors.Join(publishErr, fmt.Errorf("remove incomplete %s: %w", keyPath, cleanupErr))
		}
		return publishErr
	}
	return nil
}

func writeTLSFileTemp(path string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temporary %s: %w", path, err)
	}
	temp := f.Name()
	fail := func(action string, err error) (string, error) {
		_ = f.Close()
		_ = os.Remove(temp)
		return "", fmt.Errorf("%s temporary %s: %w", action, path, err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail("chmod", err)
	}
	if _, err := f.Write(data); err != nil {
		return fail("write", err)
	}
	if err := f.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(temp)
		return "", fmt.Errorf("close temporary %s: %w", path, err)
	}
	return temp, nil
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
