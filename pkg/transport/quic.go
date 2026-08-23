package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
)

const (
	quicALPN             = "backimage/1"
	quicHandshakeTimeout = 10 * time.Second
	quicKeepalive        = 15 * time.Second

	quicInitialStreamWindow = 16 << 20
	quicInitialConnWindow   = 32 << 20
	quicMaxStreamWindow     = 64 << 20
	quicMaxConnWindow       = 128 << 20
)

func init() {
	if err := Register("quic", newQUICDialer, newQUICListener); err != nil {
		panic(err)
	}
}

type quicDialer struct{ cfg Config }

func newQUICDialer(cfg Config) (Dialer, error) {
	if _, err := quicTLSConfig(cfg.TLS, ""); err != nil {
		return nil, err
	}
	if cfg.QUICStreams < 0 {
		return nil, errors.New("QUIC stream limit cannot be negative")
	}
	return &quicDialer{cfg: cfg}, nil
}

func (d *quicDialer) Name() string { return "quic" }

func (d *quicDialer) Dial(ctx context.Context, addr string) (Stream, error) {
	tlsCfg, err := quicTLSConfig(d.cfg.TLS, addr)
	if err != nil {
		return nil, err
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, quicHandshakeTimeout)
	defer cancel()
	conn, err := quic.DialAddr(handshakeCtx, addr, tlsCfg, quicConfig(d.cfg))
	if err != nil {
		return nil, fmt.Errorf("QUIC handshake: %w", err)
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open QUIC backup stream: %w", err), conn.CloseWithError(0, ""))
	}
	return newQUICStream(conn, stream), nil
}

type quicListener struct {
	listener *quic.Listener

	ready    chan Stream
	closed   chan struct{}
	once     sync.Once
	closeErr error
}

func newQUICListener(addr string, cfg Config) (Listener, error) {
	tlsCfg, err := quicTLSConfig(cfg.TLS, "")
	if err != nil {
		return nil, err
	}
	if len(tlsCfg.Certificates) == 0 && tlsCfg.GetCertificate == nil {
		return nil, errors.New("TLS server certificate is required")
	}
	if cfg.QUICStreams < 0 {
		return nil, errors.New("QUIC stream limit cannot be negative")
	}
	listener, err := quic.ListenAddr(defaultAddr(addr), tlsCfg, quicConfig(cfg))
	if err != nil {
		return nil, err
	}
	l := &quicListener{
		listener: listener,
		ready:    make(chan Stream), closed: make(chan struct{}),
	}
	go l.acceptLoop()
	return l, nil
}

func (l *quicListener) Name() string { return "quic" }

func (l *quicListener) Addr() net.Addr { return l.listener.Addr() }

func (l *quicListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		l.closeErr = l.listener.Close()
	})
	return l.closeErr
}

// acceptLoop waits for the session stream off the accept path. A QUIC peer
// that completes the handshake and never opens a stream would otherwise hold
// the accept loop, and with it every other client, for the whole idle timeout.
func (l *quicListener) acceptLoop() {
	pending := make(chan struct{}, maxPendingHandshakes)
	for {
		conn, err := l.listener.Accept(context.Background())
		if err != nil {
			return
		}
		select {
		case pending <- struct{}{}:
		case <-l.closed:
			_ = conn.CloseWithError(0, "")
			return
		}
		go func() {
			defer func() { <-pending }()
			// The stream must arrive within the handshake budget: a
			// connection without one carries no session.
			ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
			defer cancel()
			stream, err := conn.AcceptStream(ctx)
			if err != nil {
				_ = conn.CloseWithError(0, "")
				return
			}
			select {
			case l.ready <- newQUICStream(conn, stream):
			case <-l.closed:
				_ = conn.CloseWithError(0, "")
			}
		}()
	}
}

func (l *quicListener) Accept(ctx context.Context) (Stream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, net.ErrClosed
	case stream := <-l.ready:
		return stream, nil
	}
}

type quicStream struct {
	*quic.Stream
	conn *quic.Conn

	closeOnce sync.Once
	closeErr  error
	connOnce  sync.Once
	connErr   error
}

func newQUICStream(conn *quic.Conn, stream *quic.Stream) *quicStream {
	return &quicStream{Stream: stream, conn: conn}
}

func (s *quicStream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.Stream.Close()
	})
	return s.closeErr
}

// CloseConnection closes the underlying QUIC connection after the protocol
// peer has observed the stream FIN and final control message.
func (s *quicStream) CloseConnection() error {
	s.connOnce.Do(func() {
		s.connErr = s.conn.CloseWithError(0, "")
	})
	return s.connErr
}

func quicTLSConfig(in *tls.Config, addr string) (*tls.Config, error) {
	cfg, err := tls13Config(in)
	if err != nil {
		return nil, err
	}
	if addr != "" && cfg.ServerName == "" && !cfg.InsecureSkipVerify {
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr == nil {
			cfg.ServerName = host
		}
	}
	cfg.NextProtos = []string{quicALPN}
	return cfg, nil
}

func quicConfig(cfg Config) *quic.Config {
	streams := cfg.QUICStreams
	if streams == 0 {
		streams = 1
	}
	initialStream := uint64(quicInitialStreamWindow)
	initialConn := uint64(quicInitialConnWindow)
	maxStream := uint64(quicMaxStreamWindow)
	maxConn := uint64(quicMaxConnWindow)
	if cfg.QUICWindow > 0 {
		initialStream = cfg.QUICWindow
		initialConn = cfg.QUICWindow * 2
		maxStream = cfg.QUICWindow * 4
		maxConn = cfg.QUICWindow * 8
	}
	return &quic.Config{
		HandshakeIdleTimeout:           quicHandshakeTimeout,
		MaxIdleTimeout:                 defaultIdle,
		KeepAlivePeriod:                quicKeepalive,
		InitialStreamReceiveWindow:     initialStream,
		InitialConnectionReceiveWindow: initialConn,
		MaxStreamReceiveWindow:         maxStream,
		MaxConnectionReceiveWindow:     maxConn,
		MaxIncomingStreams:             int64(streams),
		DisablePathMTUDiscovery:        false,
	}
}
