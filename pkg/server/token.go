package server

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fpierri/backimage/pkg/protocol"
	"github.com/fpierri/backimage/pkg/registry"
)

var ErrTokenTimeout = errors.New("timed out waiting for a registry token")

// TokenBroker is a memory-only registry.Provider fed by protocol Token
// messages. Get blocks without polling until a matching, valid token arrives.
type TokenBroker struct {
	mu      sync.Mutex
	tokens  map[string]*registry.Token
	notify  chan struct{}
	timeout time.Duration
	now     func() time.Time
}

func NewTokenBroker(timeout time.Duration) *TokenBroker {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &TokenBroker{
		tokens:  make(map[string]*registry.Token),
		notify:  make(chan struct{}),
		timeout: timeout,
		now:     time.Now,
	}
}

func (b *TokenBroker) ProvideToken(tok *protocol.Token) {
	if tok == nil || (tok.Value == "" && !tok.Anonymous) || tok.Repository == "" || tok.ExpiresAtUnix <= 0 {
		return
	}
	expires := time.Unix(tok.ExpiresAtUnix, 0)
	if !expires.After(b.now()) {
		return
	}
	scope := registry.Scope{Repository: tok.Repository, Actions: canonicalActions(tok.Actions)}
	value := registry.NewDelegatedToken(tok.Value, expires, scope, tok.Anonymous)
	b.mu.Lock()
	b.tokens[scopeKey(scope)] = value
	close(b.notify)
	b.notify = make(chan struct{})
	b.mu.Unlock()
}

func (b *TokenBroker) Get(ctx context.Context, scope registry.Scope) (*registry.Token, error) {
	waitCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	want := registry.Scope{Repository: scope.Repository, Actions: canonicalActions(scope.Actions)}
	for {
		b.mu.Lock()
		tok := b.tokens[scopeKey(want)]
		if tok != nil && tok.ExpiresAt.After(b.now().Add(time.Second)) {
			copyToken := *tok
			b.mu.Unlock()
			return &copyToken, nil
		}
		notify := b.notify
		b.mu.Unlock()
		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, ErrTokenTimeout
			}
			return nil, waitCtx.Err()
		case <-notify:
		}
	}
}

func (b *TokenBroker) Invalidate(scope registry.Scope) {
	b.mu.Lock()
	delete(b.tokens, scopeKey(registry.Scope{Repository: scope.Repository, Actions: canonicalActions(scope.Actions)}))
	close(b.notify)
	b.notify = make(chan struct{})
	b.mu.Unlock()
}

func scopeKey(scope registry.Scope) string {
	return scope.Repository + "\x00" + strings.Join(canonicalActions(scope.Actions), ",")
}

func canonicalActions(actions []string) []string {
	out := append([]string(nil), actions...)
	sort.Strings(out)
	return out
}
