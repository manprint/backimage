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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/manprint/backimage/internal/buildinfo"
	"github.com/manprint/backimage/internal/embedded"
	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/server"
	"github.com/manprint/backimage/pkg/transport"
	"github.com/spf13/cobra"
)

func newListenRemoteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "listen-remote [flags]",
		Short: "receive encrypted backup streams and publish them to a registry",
		Long: "receive encrypted backup streams and publish them to a registry.\n\n" +
			"Every flag can also be set through the environment as BACKIMAGE_<FLAG>\n" +
			"with dashes replaced by underscores (--bind-address becomes\n" +
			"BACKIMAGE_BIND_ADDRESS); an explicit flag always wins. This is what makes\n" +
			"the container image configurable without a shell in the entrypoint.",
		Args:    cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error { return applyEnvDefaults(cmd) },
		RunE:    runListenRemote,
	}
	f := cmd.Flags()
	f.String("bind-address", "0.0.0.0:7575", "address to listen on, HOST:PORT (0.0.0.0 = every interface)")
	f.Bool("udp", false, "use QUIC instead of TCP")
	f.Bool("also-tcp", false, "when using --udp, also listen on TCP at the same address")
	f.String("tls-cert", "", "PEM server certificate")
	f.String("tls-key", "", "PEM server private key")
	f.String("tls-ca", "", "PEM CA bundle used to authenticate mTLS clients")
	f.Bool("tls-self-signed", false, "generate a self-signed certificate and print its SHA-256 pin; persisted in --tls-cert/--tls-key or under --work-dir when either is set")
	f.String("auth-token", "", "pre-shared client authentication token")
	f.String("auth-token-file", "", "read the pre-shared token from a file")
	f.Bool("insecure-no-auth", false, "allow unauthenticated clients (strongly discouraged)")
	f.StringSlice("allow-repo", nil, "repository prefix a client may push to, e.g. ghcr.io/team/ (repeatable; empty = any)")
	f.Int("max-sessions", 4, "maximum concurrent backup sessions; server disk needed is 2 x layer size x sessions")
	f.String("max-bytes", "0", "maximum bytes accepted per session, e.g. 200GiB (0 = unlimited)")
	f.String("rate-limit", "0", "bytes per second per session, e.g. 80MiB (0 = unlimited)")
	f.String("metrics-address", "", "serve /healthz and /metrics on this HOST:PORT (empty = disabled)")
	f.String("log-format", "text", "diagnostics format: text|json")
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
	tlsConfig, pin, mtls, err := listenTLSConfig(cmd, bind, workDir, printer)
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

// selfSignedDirName holds the persistent self-signed material generated under
// --work-dir when no explicit path is given.
const selfSignedDirName = "tls"

func listenTLSConfig(cmd *cobra.Command, bind, workDir string, printer Printer) (*tls.Config, string, bool, error) {
	selfSigned := getFlagBool(cmd, "tls-self-signed")
	certPath, keyPath := getFlagString(cmd, "tls-cert"), getFlagString(cmd, "tls-key")
	var cert tls.Certificate
	var pin string
	var err error
	switch {
	case selfSigned:
		cert, pin, err = selfSignedListenCertificate(bind, certPath, keyPath, workDir, printer)
	default:
		pair, pairErr := optionalKeyPair(certPath, keyPath)
		if pairErr != nil {
			return nil, "", false, pairErr
		}
		if pair == nil {
			return nil, "", false, errors.New("TLS certificate required: use --tls-cert/--tls-key or --tls-self-signed")
		}
		cert = *pair
		// The pin is what a client passes to --tls-pin, so it is printed for
		// a provided certificate too, not only for a generated one.
		pin, err = transport.CertificatePin(cert)
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

// selfSignedListenCertificate resolves the self-signed material. It persists
// the key pair whenever a location is available (explicit --tls-cert/--tls-key
// or --work-dir) so the pin survives a restart; only a server with nowhere to
// write falls back to an ephemeral certificate.
func selfSignedListenCertificate(bind, certPath, keyPath, workDir string, printer Printer) (tls.Certificate, string, error) {
	hosts, err := selfSignedHosts(bind)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if (certPath == "") != (keyPath == "") {
		return tls.Certificate{}, "", errors.New("--tls-cert and --tls-key must be provided together")
	}
	if certPath == "" && workDir != "" {
		dir := filepath.Join(workDir, selfSignedDirName)
		certPath = filepath.Join(dir, "self-signed.crt")
		keyPath = filepath.Join(dir, "self-signed.key")
	}
	if certPath == "" {
		cert, pin, genErr := transport.SelfSignedCertificate(hosts, time.Now())
		if genErr != nil {
			return tls.Certificate{}, "", genErr
		}
		printer.Warnf("ephemeral TLS certificate: the fingerprint changes at every restart; pass --work-dir or --tls-cert/--tls-key to persist it")
		return cert, pin, nil
	}
	cert, pin, created, err := transport.LoadOrCreateSelfSigned(certPath, keyPath, hosts, time.Now())
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if created {
		printer.Infof("generated a persistent self-signed certificate in %s (valid %d years)", certPath, int(transport.PersistentCertificateValidity.Hours()/24/365))
	} else {
		printer.Infof("reusing the self-signed certificate in %s", certPath)
	}
	return cert, pin, nil
}

// selfSignedHosts lists the SAN entries of a generated certificate. A wildcard
// bind address names no host, so it is dropped rather than embedded as 0.0.0.0.
func selfSignedHosts(bind string) ([]string, error) {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return nil, fmt.Errorf("--bind-address: %w", err)
	}
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() {
			return append(hosts, localAddresses()...), nil
		}
	}
	if host != "" {
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// localAddresses returns the non-loopback unicast addresses of this host, so a
// certificate bound to 0.0.0.0 still names the LAN address clients dial.
func localAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || !ipNet.IP.IsGlobalUnicast() {
			continue
		}
		out = append(out, ipNet.IP.String())
	}
	return out
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
