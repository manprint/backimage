package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/registry"
)

// TestPerSessionSinkIsolatesRegistryTokens pins the credential boundary
// between concurrent clients. Both sessions push to the same repository, so a
// broker shared by the whole server would happily serve client A's bearer
// token to client B's push.
func TestPerSessionSinkIsolatesRegistryTokens(t *testing.T) {
	const scopeRepo = "team/backups"
	var mu sync.Mutex
	var brokers []*TokenBroker

	cfg := Config{
		Session:     SessionConfig{AllowNoAuth: true},
		MaxSessions: 2,
		NewSink: func() (Sink, error) {
			broker := NewTokenBroker(200 * time.Millisecond)
			mu.Lock()
			brokers = append(brokers, broker)
			mu.Unlock()
			return &brokerSink{broker: broker}, nil
		},
	}
	srv, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	// New() builds one sink to validate the factory; the two below stand for
	// two concurrent client sessions.
	first, err := srv.sessionSink()
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.sessionSink()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("both sessions received the same sink")
	}

	// Client A hands its registry token to its own session.
	first.(*brokerSink).broker.ProvideToken(&protocol.Token{
		Value: "client-a-secret", Repository: scopeRepo,
		Actions: []string{"pull", "push"}, ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	})

	scope := registry.Scope{Repository: scopeRepo, Actions: []string{"pull", "push"}}
	ctx := context.Background()

	got, err := first.(*brokerSink).broker.Get(ctx, scope)
	if err != nil {
		t.Fatalf("the session that supplied the token cannot use it: %v", err)
	}
	if got.Value != "client-a-secret" {
		t.Fatalf("token = %q", got.Value)
	}

	// Client B's session targets the same repository and must not find it.
	if _, err := second.(*brokerSink).broker.Get(ctx, scope); !errors.Is(err, ErrTokenTimeout) {
		t.Fatalf("a second session reached another session's registry token: err = %v", err)
	}

	mu.Lock()
	count := len(brokers)
	mu.Unlock()
	if count < 3 { // one probe from New plus the two sessions
		t.Fatalf("brokers created = %d, want one per session", count)
	}
}

// TestSharedSinkStillSupported keeps the single-sink construction working for
// callers that do not need isolation, such as the tests and embedders that
// pass a sink directly.
func TestSharedSinkStillSupported(t *testing.T) {
	sink := &brokerSink{broker: NewTokenBroker(time.Second)}
	srv, err := New(Config{Session: SessionConfig{AllowNoAuth: true}}, sink)
	if err != nil {
		t.Fatal(err)
	}
	got, err := srv.sessionSink()
	if err != nil {
		t.Fatal(err)
	}
	if got != Sink(sink) {
		t.Fatal("a server built with an explicit sink handed out a different one")
	}
}

// brokerSink is the smallest Sink that carries a token broker.
type brokerSink struct{ broker *TokenBroker }

func (s *brokerSink) KnownBlobs(context.Context, string) ([]string, error) { return nil, nil }
func (s *brokerSink) BlobExists(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s *brokerSink) OpenBlob(context.Context, string, string, int64) (BlobWriter, error) {
	return nil, errors.New("not implemented")
}
func (s *brokerSink) CommitBackup(context.Context, Backup) (string, error) {
	return "", errors.New("not implemented")
}
func (s *brokerSink) ProvideToken(tok *protocol.Token) { s.broker.ProvideToken(tok) }
