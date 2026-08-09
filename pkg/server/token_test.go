package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fpierri/backimage/pkg/protocol"
	"github.com/fpierri/backimage/pkg/registry"
)

func TestTokenBrokerWaitRefreshAndInvalidate(t *testing.T) {
	b := NewTokenBroker(time.Second)
	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	scope := registry.Scope{Repository: "me/repo", Actions: []string{"push", "pull"}}
	result := make(chan *registry.Token, 1)
	errs := make(chan error, 1)
	go func() {
		tok, err := b.Get(context.Background(), scope)
		result <- tok
		errs <- err
	}()

	b.ProvideToken(&protocol.Token{Value: "first", Repository: "me/repo", Actions: []string{"pull", "push"}, ExpiresAtUnix: now.Add(time.Minute).Unix()})
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if tok := <-result; tok == nil || tok.Value != "first" {
		t.Fatalf("token = %#v", tok)
	}
	b.Invalidate(scope)

	b.ProvideToken(&protocol.Token{Value: "second", Repository: "me/repo", Actions: []string{"push", "pull"}, ExpiresAtUnix: now.Add(2 * time.Minute).Unix()})
	tok, err := b.Get(context.Background(), scope)
	if err != nil || tok.Value != "second" {
		t.Fatalf("refreshed token = %#v, %v", tok, err)
	}
}

func TestTokenBrokerTimeoutAndInvalidTokens(t *testing.T) {
	b := NewTokenBroker(20 * time.Millisecond)
	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	b.ProvideToken(nil)
	b.ProvideToken(&protocol.Token{Value: "", Repository: "repo", ExpiresAtUnix: now.Add(time.Hour).Unix()})
	b.ProvideToken(&protocol.Token{Value: "expired", Repository: "repo", ExpiresAtUnix: now.Add(-time.Second).Unix()})
	_, err := b.Get(context.Background(), registry.Scope{Repository: "repo", Actions: []string{"pull"}})
	if !errors.Is(err, ErrTokenTimeout) {
		t.Fatalf("error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewTokenBroker(time.Second).Get(ctx, registry.Scope{Repository: "repo"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
}
