package remote

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/manprint/backimage/pkg/protocol"
	"github.com/manprint/backimage/pkg/registry"
)

// maxSessionScopes bounds how many distinct scopes one session may ask its
// credential provider for.
//
// A backup needs one: the repository it is pushing to. The allowance exists
// because a registry may answer a push with a pull scope for the same
// repository, not because a server should be able to enumerate. It also caps
// the refresh goroutines, which are started one per scope.
const maxSessionScopes = 4

// allowedActions is what a backup upload legitimately needs. Anything else —
// delete, catalog, a wildcard — is a request this client will not relay.
var allowedActions = map[string]bool{"pull": true, "push": true}

// scopeGuard decides what the client is willing to ask its credential provider
// for. The answer comes from the reference the user chose locally, never from
// the peer.
//
// The server used to name both the repository and the actions, and the client
// passed them straight to Provider.Get: a server could ask for
// "unrelated/repository:delete", the provider would be asked to mint it and
// the resulting token went back over the wire. What the registry then granted
// depended on the account's privileges — the defect is that the decision was
// not ours to make.
type scopeGuard struct {
	repository string

	mu   sync.Mutex
	seen map[string]bool
}

// newScopeGuard derives the only repository this session may ask about from
// the reference the caller is pushing to.
func newScopeGuard(reference string) (*scopeGuard, error) {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return nil, fmt.Errorf("remote reference %q: %w", reference, err)
	}
	return &scopeGuard{repository: ref.Context().RepositoryStr(), seen: map[string]bool{}}, nil
}

// check turns a server request into the scope this client is willing to mint,
// or refuses it. It is the single validation point: v1 acks, v2 acks, the
// asynchronous token requests of both, and the refresh loop all pass here.
func (g *scopeGuard) check(request *protocol.TokenRequest) (registry.Scope, error) {
	var scope registry.Scope
	if request == nil {
		return scope, refuseScope("remote server sent an empty token request")
	}
	if request.Repository != g.repository {
		return scope, refuseScope(
			"remote server asked for credentials on %q, this backup pushes to %q: refusing to ask the credential provider",
			request.Repository, g.repository)
	}
	if len(request.Actions) == 0 {
		return scope, refuseScope("remote server asked for credentials on %q with no action", request.Repository)
	}
	seen := make(map[string]bool, len(request.Actions))
	actions := make([]string, 0, len(request.Actions))
	for _, action := range request.Actions {
		if !allowedActions[action] {
			return scope, refuseScope(
				"remote server asked for the %q action on %q: only %s are relayed",
				action, request.Repository, strings.Join(sortedActions(), " and "))
		}
		if seen[action] {
			return scope, refuseScope("remote server repeated the %q action on %q", action, request.Repository)
		}
		seen[action] = true
		actions = append(actions, action)
	}
	scope = registry.Scope{Repository: g.repository, Actions: actions}

	g.mu.Lock()
	defer g.mu.Unlock()
	key := scope.String()
	if !g.seen[key] && len(g.seen) >= maxSessionScopes {
		return registry.Scope{}, refuseScope(
			"remote server asked for a %d-th distinct credential scope in one session (%s)", len(g.seen)+1, key)
	}
	g.seen[key] = true
	return scope, nil
}

// refuseScope reports a scope refusal as a non-transient authorisation
// failure, so the retry loop does not reconnect to a server that will ask the
// same thing again, and the CLI exits 3 rather than 1.
func refuseScope(format string, args ...any) error {
	return &Error{Kind: 3, Message: fmt.Sprintf(format, args...)}
}

func sortedActions() []string {
	out := make([]string, 0, len(allowedActions))
	for action := range allowedActions {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}
