package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/transport"
)

type Config struct {
	Session     SessionConfig
	MaxSessions int
	Metrics     *Metrics
	OnError     func(error)

	// NewSink builds the registry-facing half of one session. A sink carries
	// the token broker the client fills with its registry credentials, so a
	// shared one lets any session push with any other session's bearer token
	// as soon as two clients target the same repository. Set this to isolate
	// them; when nil every session shares the sink passed to New.
	NewSink func() (Sink, error)
}

// Server accepts independent, stateless sessions with a hard concurrency cap.
type Server struct {
	cfg  Config
	sink Sink
	sem  chan struct{}
	wg   sync.WaitGroup
}

func New(cfg Config, sink Sink) (*Server, error) {
	if sink == nil && cfg.NewSink == nil {
		return nil, errors.New("server sink is required")
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 4
	}
	if cfg.Metrics == nil {
		cfg.Metrics = new(Metrics)
	}
	cfg.Session.Metrics = cfg.Metrics
	// Build one sink up front so a misconfigured factory fails at startup
	// rather than on the first client.
	probe := sink
	if cfg.NewSink != nil {
		built, err := cfg.NewSink()
		if err != nil {
			return nil, err
		}
		if built == nil {
			return nil, errors.New("server sink factory returned no sink")
		}
		probe = built
	}
	if _, err := NewSession(cfg.Session, probe); err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, sink: sink, sem: make(chan struct{}, cfg.MaxSessions)}, nil
}

func (s *Server) Metrics() *Metrics { return s.cfg.Metrics }

func (s *Server) Serve(ctx context.Context, listener transport.Listener) error {
	if listener == nil {
		return errors.New("transport listener is required")
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.report(fmt.Errorf("close remote listener: %w", err))
		}
		s.wg.Wait()
	}()
	for {
		stream, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A closed listener never recovers; retrying it is a busy loop.
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			s.report(fmt.Errorf("accept remote session: %w", err))
			continue
		}
		select {
		case s.sem <- struct{}{}:
			s.wg.Add(1)
			go s.serveOne(ctx, stream)
		default:
			if err := protocol.WriteServerMessage(stream, &protocol.ServerMessage{Msg: &protocol.ServerMessage_Error{Error: &protocol.Error{
				Kind: ErrorNetwork, Message: fmt.Sprintf("maximum concurrent sessions reached (%d)", cap(s.sem)), Hint: "retry later",
			}}}); err != nil {
				s.report(fmt.Errorf("report session limit: %w", err))
			}
			if err := stream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				s.report(fmt.Errorf("close rejected session: %w", err))
			}
			s.cfg.Metrics.addError(ErrorNetwork)
		}
	}
}

func (s *Server) serveOne(ctx context.Context, stream transport.Stream) {
	defer s.wg.Done()
	defer func() { <-s.sem }()
	stopClose := context.AfterFunc(ctx, func() {
		if err := stream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.report(fmt.Errorf("close canceled session: %w", err))
		}
	})
	defer stopClose()
	start := time.Now()
	s.cfg.Metrics.sessionStarted()
	defer func() {
		s.cfg.Metrics.sessionDone(time.Since(start))
	}()
	sink, err := s.sessionSink()
	if err != nil {
		if closeErr := stream.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			s.report(fmt.Errorf("close invalid session: %w", closeErr))
		}
		s.report(err)
		return
	}
	session, err := NewSession(s.cfg.Session, sink)
	if err != nil {
		if closeErr := stream.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			s.report(fmt.Errorf("close invalid session: %w", closeErr))
		}
		s.report(err)
		return
	}
	if err := session.Run(ctx, stream); err != nil && !errors.Is(err, context.Canceled) {
		s.report(err)
	}
	releaseConnection(stream)
}

// connectionLinger bounds the wait for a peer to close its half after the
// session ends.
const connectionLinger = 5 * time.Second

// releaseConnection tears down the transport once the protocol is done. For
// QUIC, Stream.Close only sends the stream FIN and the connection would linger
// until MaxIdleTimeout. Closing it outright is not an option either:
// CONNECTION_CLOSE discards data the peer has not acknowledged, which is
// exactly the BackupEnd it is waiting for. So wait for the peer to finish
// first, and give up after a bounded linger.
func releaseConnection(stream transport.Stream) {
	closer, ok := stream.(transport.ConnectionCloser)
	if !ok {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// EOF means the peer closed its side, so it has read everything.
		_, _ = io.Copy(io.Discard, stream)
	}()
	select {
	case <-done:
	case <-time.After(connectionLinger):
	}
	_ = closer.CloseConnection()
}

// sessionSink gives this session its own registry-facing half when a factory
// is configured, so the registry token one client hands over stays reachable
// only by that client's session.
func (s *Server) sessionSink() (Sink, error) {
	if s.cfg.NewSink == nil {
		return s.sink, nil
	}
	sink, err := s.cfg.NewSink()
	if err != nil {
		return nil, fmt.Errorf("build session sink: %w", err)
	}
	if sink == nil {
		return nil, errors.New("server sink factory returned no sink")
	}
	return sink, nil
}

func (s *Server) report(err error) {
	if s.cfg.OnError != nil {
		s.cfg.OnError(err)
	}
}
