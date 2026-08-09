// Package transport provides pluggable, encrypted remote streams.
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Stream is a bidirectional byte stream with deadlines.
type Stream interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

// ConnectionCloser releases transport resources after the protocol stream has
// completed. Stream.Close alone only sends the stream FIN for QUIC.
type ConnectionCloser interface {
	CloseConnection() error
}

// Dialer opens a Stream to a server.
type Dialer interface {
	Dial(context.Context, string) (Stream, error)
	Name() string
}

// Listener accepts Streams.
type Listener interface {
	Accept(context.Context) (Stream, error)
	Addr() net.Addr
	Close() error
}

// Config holds the TLS material shared by transports.
type Config struct {
	TLS         *tls.Config
	Keepalive   time.Duration
	IdleTimeout time.Duration

	// QUICStreams limits incoming QUIC streams. Protocol v1 uses one stream per
	// backup session; values other than one are reserved for transport tuning.
	QUICStreams int
	// QUICWindow overrides the initial per-stream QUIC receive window in bytes.
	// Zero selects the production defaults.
	QUICWindow uint64
}

type dialerFactory func(Config) (Dialer, error)
type listenerFactory func(string, Config) (Listener, error)

var (
	registryMu sync.RWMutex
	dialers    = map[string]dialerFactory{}
	listeners  = map[string]listenerFactory{}
)

// Register makes an optional transport available. It is primarily used by the
// QUIC implementation added in phase 09.
func Register(name string, d dialerFactory, l listenerFactory) error {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" || d == nil || l == nil {
		return errors.New("transport registration needs a name, dialer, and listener")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := dialers[name]; exists {
		return fmt.Errorf("transport %q already registered", name)
	}
	dialers[name], listeners[name] = d, l
	return nil
}

// NewDialer returns the registered dialer named name.
func NewDialer(name string, cfg Config) (Dialer, error) {
	registryMu.RLock()
	f := dialers[strings.ToLower(name)]
	registryMu.RUnlock()
	if f == nil {
		return nil, fmt.Errorf("unknown transport %q", name)
	}
	return f(cfg)
}

// NewListener returns the registered listener named name.
func NewListener(name, addr string, cfg Config) (Listener, error) {
	registryMu.RLock()
	f := listeners[strings.ToLower(name)]
	registryMu.RUnlock()
	if f == nil {
		return nil, fmt.Errorf("unknown transport %q", name)
	}
	return f(addr, cfg)
}

func tls13Config(in *tls.Config) (*tls.Config, error) {
	if in == nil {
		return nil, errors.New("TLS configuration is required")
	}
	cfg := in.Clone()
	if cfg.MaxVersion != 0 && cfg.MaxVersion < tls.VersionTLS13 {
		return nil, errors.New("TLS 1.3 is required")
	}
	cfg.MinVersion = tls.VersionTLS13
	return cfg, nil
}
