package server

import (
	"context"
	"errors"
	"fmt"
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
}

// Server accepts independent, stateless sessions with a hard concurrency cap.
type Server struct {
	cfg  Config
	sink Sink
	sem  chan struct{}
	wg   sync.WaitGroup
}

func New(cfg Config, sink Sink) (*Server, error) {
	if sink == nil {
		return nil, errors.New("server sink is required")
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 4
	}
	if cfg.Metrics == nil {
		cfg.Metrics = new(Metrics)
	}
	cfg.Session.Metrics = cfg.Metrics
	if _, err := NewSession(cfg.Session, sink); err != nil {
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
	session, err := NewSession(s.cfg.Session, s.sink)
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
}

func (s *Server) report(err error) {
	if s.cfg.OnError != nil {
		s.cfg.OnError(err)
	}
}
