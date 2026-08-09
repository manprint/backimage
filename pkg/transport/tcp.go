package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
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
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("TLS handshake: %w", err), raw.Close())
	}
	if d.cfg.IdleTimeout > 0 {
		if err := conn.SetDeadline(time.Now().Add(d.cfg.IdleTimeout)); err != nil {
			return nil, errors.Join(fmt.Errorf("set connection deadline: %w", err), conn.Close())
		}
	}
	return conn, nil
}

type tcpListener struct {
	ln  *net.TCPListener
	cfg Config
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
	return &tcpListener{ln: ln, cfg: cfg}, nil
}

func (l *tcpListener) Accept(ctx context.Context) (Stream, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := l.ln.SetDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			return nil, fmt.Errorf("set listener deadline: %w", err)
		}
		raw, err := l.ln.AcceptTCP()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return nil, err
		}
		if err := raw.SetKeepAlive(true); err != nil {
			return nil, errors.Join(fmt.Errorf("enable TCP keepalive: %w", err), raw.Close())
		}
		if err := raw.SetKeepAlivePeriod(l.cfg.Keepalive); err != nil {
			return nil, errors.Join(fmt.Errorf("set TCP keepalive period: %w", err), raw.Close())
		}
		conn := tls.Server(raw, l.cfg.TLS.Clone())
		if err := conn.HandshakeContext(ctx); err != nil {
			return nil, errors.Join(fmt.Errorf("TLS handshake: %w", err), raw.Close())
		}
		if l.cfg.IdleTimeout > 0 {
			if err := conn.SetDeadline(time.Now().Add(l.cfg.IdleTimeout)); err != nil {
				return nil, errors.Join(fmt.Errorf("set connection deadline: %w", err), conn.Close())
			}
		}
		return conn, nil
	}
}

func (l *tcpListener) Addr() net.Addr { return l.ln.Addr() }
func (l *tcpListener) Close() error   { return l.ln.Close() }

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
