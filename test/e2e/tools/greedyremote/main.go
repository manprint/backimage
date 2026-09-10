// Command greedyremote is a backimage remote server that asks its clients for
// registry credentials they never agreed to hand over.
//
// It models the A06 attacker: a server the client authenticated to, and which
// then names a repository or an action of its own choosing in the token
// request. A correct client derives the scope from the reference the user
// typed and refuses, without calling its credential provider and without
// putting a token on the wire.
//
// It exists for the phase A4 e2e only; it is not part of the shipped CLI.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/manprint/backimage/pkg/server"
	"github.com/manprint/backimage/pkg/transport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "greedyremote:", err)
		os.Exit(1)
	}
}

func run() error {
	bind := flag.String("bind", "127.0.0.1:7590", "address to listen on")
	certFile := flag.String("tls-cert", "", "PEM server certificate")
	keyFile := flag.String("tls-key", "", "PEM server private key")
	tokenFile := flag.String("auth-token-file", "", "pre-shared client authentication token")
	repository := flag.String("repository", "", "repository to demand credentials for")
	actions := flag.String("actions", "pull,push", "comma separated actions to demand")
	flag.Parse()

	if *certFile == "" || *keyFile == "" || *tokenFile == "" || *repository == "" {
		return fmt.Errorf("--tls-cert, --tls-key, --auth-token-file and --repository are mandatory")
	}
	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return err
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		return err
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	listener, err := transport.NewListener("tcp", *bind, transport.Config{TLS: tlsConfig})
	if err != nil {
		return err
	}
	defer listener.Close()

	demanded := make([]string, 0, 2)
	for _, action := range strings.Split(*actions, ",") {
		if action = strings.TrimSpace(action); action != "" {
			demanded = append(demanded, action)
		}
	}
	sink := &greedySink{repository: *repository, actions: demanded}
	srv, err := server.New(server.Config{
		Session: server.SessionConfig{
			Version: "greedyremote", AuthToken: []byte(strings.TrimRight(string(token), "\r\n")),
			AllowNoAuth: false,
		},
		MaxSessions: 2,
		OnError:     func(err error) { fmt.Fprintln(os.Stderr, "session:", err) },
	}, sink)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "greedyremote: listening on %s, demanding %s:%s\n",
		listener.Addr(), *repository, strings.Join(demanded, ","))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Serve(ctx, listener); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// greedySink answers every reference with the scope the operator asked for.
// Nothing below TokenScope is ever reached: a client that refuses the scope
// closes the session before the first layer.
type greedySink struct {
	repository string
	actions    []string
}

func (s *greedySink) TokenScope(string) (string, []string, error) {
	return s.repository, append([]string(nil), s.actions...), nil
}

func (*greedySink) KnownBlobs(context.Context, string) ([]string, error) { return nil, nil }
func (*greedySink) BlobExists(context.Context, string, string) (bool, error) {
	return false, nil
}

func (*greedySink) OpenBlob(context.Context, string, string, int64) (server.BlobWriter, error) {
	return nil, fmt.Errorf("greedyremote never accepts data: the client should have refused the scope")
}

func (*greedySink) CommitBackup(context.Context, server.Backup) (string, error) {
	return "", fmt.Errorf("greedyremote never commits a backup")
}

// CommitStream exists only so the session advertises protocol v2: the default
// client mode is streaming, and a server without it sends the client back to
// the layer protocol before the token request is ever reached.
func (*greedySink) CommitStream(context.Context, server.StreamCommit) (string, error) {
	return "", fmt.Errorf("greedyremote never commits a backup")
}

var _ server.TokenRequestSource = (*greedySink)(nil)
var _ server.StreamCommitter = (*greedySink)(nil)
