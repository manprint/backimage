package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	defaultKeepalive = 30 * time.Second
	defaultIdle      = 120 * time.Second
)

func init() {
	if err := Register("tcp", newTCPDialer, newTCPListener); err != nil {
		panic(err)
	}
}

type tcpDialer struct{ cfg Config }

func newTCPDialer(cfg Config) (Dialer, error) {
	tlsCfg, err := tls13Config(cfg.TLS)
	if err != nil {
		return nil, err
	}
	cfg.TLS = tlsCfg
	setDefaults(&cfg)
	return &tcpDialer{cfg: cfg}, nil
}

func (d *tcpDialer) Name() string { return "tcp" }

func (d *tcpDialer) Dial(ctx context.Context, addr string) (Stream, error) {
	nd := net.Dialer{KeepAlive: d.cfg.Keepalive}
	raw, err := nd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tcp, ok := raw.(*net.TCPConn)
	if ok {
		if err := tcp.SetKeepAlive(true); err != nil {
			return nil, errors.Join(fmt.Errorf("enable TCP keepalive: %w", err), raw.Close())
		}
		if err := tcp.SetKeepAlivePeriod(d.cfg.Keepalive); err != nil {
			return nil, errors.Join(fmt.Errorf("set TCP keepalive period: %w", err), raw.Close())
		}
	}
	cfg := d.cfg.TLS.Clone()
	if cfg.ServerName == "" && !cfg.InsecureSkipVerify {
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr == nil {
			cfg.ServerName = host
		}
	}
	conn := tls.Client(raw, cfg)
	if err := handshakeWithTimeout(ctx, conn, d.cfg.IdleTimeout); err != nil {
		return nil, errors.Join(fmt.Errorf("TLS handshake: %w", err), raw.Close())
	}
	stream, err := newIdleConn(conn, d.cfg.IdleTimeout)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("set connection deadline: %w", err), conn.Close())
	}
	return stream, nil
}

// handshakeTimeout bounds a TLS handshake that has no other deadline. A peer
// that opens the socket and then says nothing must not hold the accept loop.
const handshakeTimeout = 10 * time.Second

func handshakeWithTimeout(ctx context.Context, conn *tls.Conn, idle time.Duration) error {
	limit := handshakeTimeout
	if idle > 0 && idle < limit {
		limit = idle
	}
	// The deadline covers the handshake itself; idleConn arms the session one
	// afterwards. HandshakeContext alone only reacts to ctx, which for a
	// server is the whole process lifetime.
	if err := conn.SetDeadline(time.Now().Add(limit)); err != nil {
		return err
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		return err
	}
	return conn.SetDeadline(time.Time{})
}

// maxPendingHandshakes bounds how many peers may be mid-handshake at once. It
// is the back-pressure that keeps a flood of half-open connections from
// becoming unbounded goroutines, without letting any single one of them stall
// the peers behind it.
const maxPendingHandshakes = 64

type tcpListener struct {
	ln  *net.TCPListener
	cfg Config

	ready    chan Stream
	closed   chan struct{}
	once     sync.Once
	closeErr error
}

func newTCPListener(addr string, cfg Config) (Listener, error) {
	tlsCfg, err := tls13Config(cfg.TLS)
	if err != nil {
		return nil, err
	}
	if len(tlsCfg.Certificates) == 0 && tlsCfg.GetCertificate == nil {
		return nil, errors.New("TLS server certificate is required")
	}
	cfg.TLS = tlsCfg
	setDefaults(&cfg)
	addr = defaultAddr(addr)
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return nil, err
	}
	l := &tcpListener{
		ln: ln, cfg: cfg,
		ready: make(chan Stream), closed: make(chan struct{}),
	}
	go l.acceptLoop()
	return l, nil
}

// acceptLoop keeps the TLS handshake off the accept path. Performed inline it
// would let one peer that opens a socket and then goes silent block every
// other client for as long as it cares to hold the connection.
func (l *tcpListener) acceptLoop() {
	pending := make(chan struct{}, maxPendingHandshakes)
	for {
		raw, err := l.ln.AcceptTCP()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		select {
		case pending <- struct{}{}:
		case <-l.closed:
			_ = raw.Close()
			return
		}
		go func() {
			defer func() { <-pending }()
			stream, err := l.handshake(raw)
			if err != nil {
				_ = raw.Close()
				return
			}
			// Unbuffered: a handshaked peer waits for a consumer, so the
			// server's session cap still bounds what is held open.
			select {
			case l.ready <- stream:
			case <-l.closed:
				_ = stream.Close()
			}
		}()
	}
}

func (l *tcpListener) handshake(raw *net.TCPConn) (Stream, error) {
	if err := raw.SetKeepAlive(true); err != nil {
		return nil, fmt.Errorf("enable TCP keepalive: %w", err)
	}
	if err := raw.SetKeepAlivePeriod(l.cfg.Keepalive); err != nil {
		return nil, fmt.Errorf("set TCP keepalive period: %w", err)
	}
	conn := tls.Server(raw, l.cfg.TLS.Clone())
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	if err := handshakeWithTimeout(ctx, conn, l.cfg.IdleTimeout); err != nil {
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	return newIdleConn(conn, l.cfg.IdleTimeout)
}

func (l *tcpListener) Accept(ctx context.Context) (Stream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, net.ErrClosed
	case stream := <-l.ready:
		return stream, nil
	}
}

func (l *tcpListener) Addr() net.Addr { return l.ln.Addr() }

func (l *tcpListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		l.closeErr = l.ln.Close()
	})
	return l.closeErr
}

func setDefaults(cfg *Config) {
	if cfg.Keepalive <= 0 {
		cfg.Keepalive = defaultKeepalive
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdle
	}
}

func defaultAddr(addr string) string {
	if addr == "" {
		return "0.0.0.0:7575"
	}
	return addr
}
