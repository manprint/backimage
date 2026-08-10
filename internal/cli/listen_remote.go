package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fpierri/backimage/internal/buildinfo"
	"github.com/fpierri/backimage/internal/embedded"
	"github.com/fpierri/backimage/pkg/protocol"
	"github.com/fpierri/backimage/pkg/server"
	"github.com/fpierri/backimage/pkg/transport"
	"github.com/spf13/cobra"
)

func newListenRemoteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "listen-remote [flags]",
		Short: "receive encrypted backup streams and publish them to a registry",
		Args:  cobra.NoArgs,
		RunE:  runListenRemote,
	}
	f := cmd.Flags()
	f.String("bind-address", "0.0.0.0:7575", "listen address")
	f.Bool("udp", false, "use QUIC instead of TCP")
	f.Bool("also-tcp", false, "when using --udp, also listen on TCP at the same address")
	f.String("tls-cert", "", "PEM server certificate")
	f.String("tls-key", "", "PEM server private key")
	f.String("tls-ca", "", "PEM CA bundle used to authenticate mTLS clients")
	f.Bool("tls-self-signed", false, "generate an ephemeral certificate and print its SHA-256 pin")
	f.String("auth-token", "", "pre-shared client authentication token")
	f.String("auth-token-file", "", "read the pre-shared token from a file")
	f.Bool("insecure-no-auth", false, "allow unauthenticated clients (strongly discouraged)")
	f.StringSlice("allow-repo", nil, "allowed repository prefix (repeatable)")
	f.Int("max-sessions", 4, "maximum concurrent sessions")
	f.String("max-bytes", "0", "maximum bytes per session (0 = unlimited)")
	f.String("rate-limit", "0", "bytes per second per session (0 = unlimited)")
	f.String("metrics-address", "", "listen address for /healthz and /metrics")
	f.String("log-format", "text", "text|json")
	f.String("work-dir", "", "directory for the per-layer spool of streaming sessions (default $TMPDIR)")
	f.Bool("spool", false, "deprecated: the streaming protocol always spools one layer at a time")
	addQUICExperimentalFlags(cmd)
	return cmd
}

func runListenRemote(cmd *cobra.Command, _ []string) error {
	opts, err := parseOptions(cmd.Root())
	if err != nil {
		return err
	}
	printer := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	workDir := getFlagString(cmd, "work-dir")
	if getFlagBool(cmd, "spool") {
		printer.Warnf("--spool is deprecated: streaming sessions always spool one layer at a time in --work-dir")
	}
	if workDir != "" {
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			return New(KindUsage, "", "--work-dir: %v", err)
		}
	}
	if getFlagBool(cmd, "also-tcp") && !getFlagBool(cmd, "udp") {
		return New(KindUsage, "", "--also-tcp requires --udp")
	}
	if format := getFlagString(cmd, "log-format"); format != "text" && format != "json" {
		return New(KindUsage, "", "--log-format must be text or json")
	}
	sharedToken, err := sharedAuthToken(cmd)
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	bind := getFlagString(cmd, "bind-address")
	tlsConfig, pin, mtls, err := listenTLSConfig(cmd, bind)
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	insecure := getFlagBool(cmd, "insecure-no-auth")
	if sharedToken == "" && !mtls && !insecure {
		return New(KindUsage, "", "client authentication is required: configure mTLS, --auth-token, or explicitly --insecure-no-auth")
	}
	if insecure {
		printer.Warnf("WARNING: remote server authentication is disabled")
	}
	if pin != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "TLS fingerprint SHA256:%s\n", pin)
	}
	maxBytes, err := parseLimitFlag(getFlagString(cmd, "max-bytes"))
	if err != nil {
		return New(KindUsage, "", "--max-bytes: %v", err)
	}
	rateLimit, err := parseLimitFlag(getFlagString(cmd, "rate-limit"))
	if err != nil {
		return New(KindUsage, "", "--rate-limit: %v", err)
	}
	maxSessions := getFlagInt(cmd, "max-sessions")
	if maxSessions <= 0 {
		return New(KindUsage, "", "--max-sessions must be positive")
	}

	transportName := "tcp"
	if getFlagBool(cmd, "udp") {
		transportName = "quic"
	}
	transportConfig := transport.Config{TLS: tlsConfig}
	if err := applyQUICExperimentalFlags(cmd, &transportConfig); err != nil {
		return New(KindUsage, "", "%v", err)
	}
	listener, err := transport.NewListener(transportName, bind, transportConfig)
	if err != nil {
		return New(KindNetwork, "", "listen %s: %v", bind, err)
	}
	var alsoTCP transport.Listener
	if getFlagBool(cmd, "also-tcp") {
		alsoTCP, err = transport.NewListener("tcp", bind, transport.Config{TLS: tlsConfig})
		if err != nil {
			return New(KindNetwork, "", "listen TCP %s: %v", bind, errors.Join(err, listener.Close()))
		}
	}
	broker := server.NewTokenBroker(30 * time.Second)
	sink, err := server.NewRegistrySink(server.RegistrySinkOptions{
		Broker: broker, Jobs: maxSessions, SelfExtract: embedded.SelfExtract,
	})
	if err != nil {
		return New(KindGeneric, "", "registry sink: %v", errors.Join(err, listener.Close(), closeOptionalListener(alsoTCP)))
	}
	metrics := new(server.Metrics)
	remoteServer, err := server.New(server.Config{
		Session: server.SessionConfig{
			Version: buildinfo.Version, AuthToken: []byte(sharedToken),
			AllowNoAuth: insecure || mtls, AllowedRepos: getFlagStrings(cmd, "allow-repo"),
			MaxBytes: maxBytes, RateLimit: rateLimit, Metrics: metrics,
			TempDir: workDir,
		},
		MaxSessions: maxSessions, Metrics: metrics,
		OnError: func(err error) { printer.Warnf("remote session: %v", err) },
	}, sink)
	if err != nil {
		return New(KindUsage, "", "%v", errors.Join(err, listener.Close(), closeOptionalListener(alsoTCP)))
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	metricsServer, metricsErr := startMetricsServer(getFlagString(cmd, "metrics-address"), metrics, func(err error) {
		printer.Warnf("metrics server: %v", err)
	})
	if metricsErr != nil {
		return New(KindNetwork, "", "metrics: %v", errors.Join(metricsErr, listener.Close(), closeOptionalListener(alsoTCP)))
	}
	if metricsServer != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := metricsServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				printer.Warnf("metrics shutdown: %v", err)
			}
		}()
	}
	spoolDir := workDir
	if spoolDir == "" {
		spoolDir = os.TempDir()
	}
	printer.Infof("listening on %s via %s (TLS 1.3, protocol v%d, streaming pipeline, layer spool in %s)",
		listener.Addr(), transportName, protocol.Version, spoolDir)
	if alsoTCP == nil {
		err = remoteServer.Serve(ctx, listener)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			return New(KindNetwork, "", "remote server: %v", err)
		}
		return nil
	}
	printer.Infof("also listening on %s via tcp", alsoTCP.Addr())
	errs := make(chan error, 2)
	go func() { errs <- remoteServer.Serve(ctx, listener) }()
	go func() { errs <- remoteServer.Serve(ctx, alsoTCP) }()
	for range 2 {
		if serveErr := <-errs; serveErr != nil && !errors.Is(serveErr, context.Canceled) {
			return New(KindNetwork, "", "remote server: %v", serveErr)
		}
	}
	return nil
}

func closeOptionalListener(listener transport.Listener) error {
	if listener == nil {
		return nil
	}
	return listener.Close()
}

func listenTLSConfig(cmd *cobra.Command, bind string) (*tls.Config, string, bool, error) {
	selfSigned := getFlagBool(cmd, "tls-self-signed")
	certPath, keyPath := getFlagString(cmd, "tls-cert"), getFlagString(cmd, "tls-key")
	if selfSigned && (certPath != "" || keyPath != "") {
		return nil, "", false, errors.New("--tls-self-signed conflicts with --tls-cert/--tls-key")
	}
	var cert tls.Certificate
	var pin string
	var err error
	if selfSigned {
		host, _, splitErr := net.SplitHostPort(bind)
		if splitErr != nil {
			return nil, "", false, fmt.Errorf("--bind-address: %w", splitErr)
		}
		cert, pin, err = transport.SelfSignedCertificate([]string{host, "localhost", "127.0.0.1"}, time.Now())
	} else {
		pair, pairErr := optionalKeyPair(certPath, keyPath)
		if pairErr != nil {
			return nil, "", false, pairErr
		}
		if pair == nil {
			return nil, "", false, errors.New("TLS certificate required: use --tls-cert/--tls-key or --tls-self-signed")
		}
		cert = *pair
	}
	if err != nil {
		return nil, "", false, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
	mtls := false
	if caPath := getFlagString(cmd, "tls-ca"); caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, "", false, fmt.Errorf("read --tls-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, "", false, errors.New("--tls-ca contains no certificates")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		mtls = true
	}
	return cfg, pin, mtls, nil
}

func parseLimitFlag(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}
	n, err := parseSize(value)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errors.New("value cannot be negative")
	}
	return uint64(n), nil
}

func startMetricsServer(address string, metrics *server.Metrics, onError func(error)) (*http.Server, error) {
	if address == "" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	httpServer := &http.Server{
		Handler: metrics.Handler(), ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && onError != nil {
			onError(err)
		}
	}()
	return httpServer, nil
}
