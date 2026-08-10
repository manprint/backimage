package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// EphemeralCertificateValidity is the lifetime of a certificate that lives
// only as long as the process. PersistentCertificateValidity is the lifetime
// of one written to disk: a pinning client trusts the fingerprint, not an
// expiry date, and a pin that silently stops working is worse than a long
// lived key on a LAN.
const (
	EphemeralCertificateValidity  = 24 * time.Hour
	PersistentCertificateValidity = 10 * 365 * 24 * time.Hour
)

// SelfSignedCertificate creates an ephemeral ECDSA certificate and its
// lowercase SHA-256 fingerprint. It is suitable for TLS pinning on a LAN.
func SelfSignedCertificate(hosts []string, now time.Time) (tls.Certificate, string, error) {
	return SelfSignedCertificateFor(hosts, now, EphemeralCertificateValidity)
}

// SelfSignedCertificateFor is SelfSignedCertificate with an explicit validity.
func SelfSignedCertificateFor(hosts []string, now time.Time, validity time.Duration) (tls.Certificate, string, error) {
	if validity <= 0 {
		validity = EphemeralCertificateValidity
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if now.IsZero() {
		now = time.Now()
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "backimage"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if host != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	sum := sha256.Sum256(der)
	return cert, hex.EncodeToString(sum[:]), nil
}

// PinnedClientTLS returns a TLS 1.3 client config that authenticates the
// server certificate by its SHA-256 fingerprint.
func PinnedClientTLS(pin string, clientCert *tls.Certificate) (*tls.Config, error) {
	want, err := parsePin(pin)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // #nosec G402 -- VerifyConnection enforces the SHA-256 certificate pin.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("TLS peer sent no certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return errors.New("TLS certificate fingerprint mismatch")
			}
			return nil
		},
	}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	return cfg, nil
}

func parsePin(pin string) ([]byte, error) {
	pin = strings.ToLower(strings.TrimSpace(pin))
	pin = strings.ReplaceAll(pin, ":", "")
	b, err := hex.DecodeString(pin)
	if err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("TLS pin must be a SHA-256 fingerprint")
	}
	return b, nil
}
