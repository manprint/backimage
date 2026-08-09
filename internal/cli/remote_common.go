package cli

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/fpierri/backimage/pkg/backup"
	"github.com/fpierri/backimage/pkg/registry"
	backremote "github.com/fpierri/backimage/pkg/remote"
	"github.com/fpierri/backimage/pkg/transport"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"
)

func newBackupRemote(cmd *cobra.Command, reference, address string, kc registry.Keychain) (backup.RemoteUploader, error) {
	tlsConfig, err := clientTLSConfig(cmd)
	if err != nil {
		return nil, New(KindUsage, "", "%v", err)
	}
	transportName := "tcp"
	if getFlagBool(cmd, "udp") {
		transportName = "quic"
	}
	transportConfig := transport.Config{TLS: tlsConfig}
	if err := applyQUICExperimentalFlags(cmd, &transportConfig); err != nil {
		return nil, New(KindUsage, "", "%v", err)
	}
	dialer, err := transport.NewDialer(transportName, transportConfig)
	if err != nil {
		return nil, New(KindUsage, "", "remote transport: %v", err)
	}
	ref, err := name.ParseReference(reference)
	if err != nil {
		return nil, New(KindUsage, "", "remote reference: %v", err)
	}
	auth, err := kc.Resolve(ref.Context())
	if err != nil {
		return nil, New(KindPermission, "", "registry credentials: %v", err)
	}
	provider := registry.NewProvider(ref.Context().RegistryStr(), auth)
	sharedToken, err := sharedAuthToken(cmd)
	if err != nil {
		return nil, New(KindUsage, "", "%v", err)
	}
	client, err := backremote.New(backremote.Config{
		Dialer: dialer, Address: address, AuthToken: sharedToken,
		Provider: provider,
	})
	if err != nil {
		return nil, New(KindUsage, "", "remote client: %v", err)
	}
	return client, nil
}

func addQUICExperimentalFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.Int("x-quic-streams", 1, "experimental QUIC stream count")
	f.Uint64("x-quic-window", 0, "experimental QUIC initial stream window in bytes")
	f.Bool("x-quic-gso", true, "experimental QUIC UDP GSO")
	f.String("x-quic-cc", "cubic", "experimental QUIC congestion controller")
	for _, name := range []string{"x-quic-streams", "x-quic-window", "x-quic-gso", "x-quic-cc"} {
		if err := f.MarkHidden(name); err != nil {
			flagErr(name, err)
		}
	}
}

func applyQUICExperimentalFlags(cmd *cobra.Command, cfg *transport.Config) error {
	if !getFlagBool(cmd, "udp") {
		return nil
	}
	streams := getFlagInt(cmd, "x-quic-streams")
	if streams != 1 {
		return errors.New("protocol v1 uses one sequential QUIC stream; --x-quic-streams must be 1")
	}
	if cc := getFlagString(cmd, "x-quic-cc"); cc != "cubic" {
		return fmt.Errorf("--x-quic-cc=%q is not supported by this quic-go version", cc)
	}
	cfg.QUICStreams = streams
	cfg.QUICWindow = getFlagUint64(cmd, "x-quic-window")
	if cmd.Flags().Changed("x-quic-gso") {
		if getFlagBool(cmd, "x-quic-gso") {
			if err := os.Unsetenv("QUIC_GO_DISABLE_GSO"); err != nil {
				return fmt.Errorf("enable QUIC GSO: %w", err)
			}
		} else if err := os.Setenv("QUIC_GO_DISABLE_GSO", "true"); err != nil {
			return fmt.Errorf("disable QUIC GSO: %w", err)
		}
	}
	return nil
}

func clientTLSConfig(cmd *cobra.Command) (*tls.Config, error) {
	cert, err := optionalKeyPair(getFlagString(cmd, "tls-cert"), getFlagString(cmd, "tls-key"))
	if err != nil {
		return nil, err
	}
	if pin := getFlagString(cmd, "tls-pin"); pin != "" {
		return transport.PinnedClientTLS(pin, cert)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	if caPath := getFlagString(cmd, "tls-ca"); caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read --tls-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("--tls-ca contains no certificates")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func optionalKeyPair(certPath, keyPath string) (*tls.Certificate, error) {
	if (certPath == "") != (keyPath == "") {
		return nil, errors.New("--tls-cert and --tls-key must be provided together")
	}
	if certPath == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	return &cert, nil
}

func sharedAuthToken(cmd *cobra.Command) (string, error) {
	direct := getFlagString(cmd, "auth-token")
	file := getFlagString(cmd, "auth-token-file")
	if direct != "" && file != "" {
		return "", errors.New("--auth-token and --auth-token-file are mutually exclusive")
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read --auth-token-file: %w", err)
		}
		direct = strings.TrimSpace(string(data))
		for i := range data {
			data[i] = 0
		}
	}
	if strings.ContainsAny(direct, "\r\n") {
		return "", errors.New("authentication token must be a single line")
	}
	return direct, nil
}
