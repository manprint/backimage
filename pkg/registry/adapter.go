package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// Capability declares an operation offered by a registry adapter.
type Capability uint32

const (
	CapListTags Capability = 1 << iota
	CapListRepos
	CapDeleteManifest
	CapDeleteTag
	CapGarbageCollect
	CapUsageStats
)

// ErrSharedManifest is returned when a tag cannot be deleted on its own
// because other tags point at the same manifest. It is a refusal to do
// something the user did not ask for, not a transport failure: the CLI maps it
// to the usage exit code, the same one a missing --yes gets, so a script does
// not retry it the way it would retry a network error.
var ErrSharedManifest = errors.New("il manifest è condiviso da più tag")

// capabilityNames is the wire spelling of each bit, in declaration order. A
// capability is something a user is told about, so it has a name and not just
// a position: `repo caps` used to print the bitmask itself, and "45" says
// nothing about which operations a registry supports.
var capabilityNames = []struct {
	bit  Capability
	name string
}{
	{CapListTags, "list-tags"},
	{CapListRepos, "list-repos"},
	{CapDeleteManifest, "delete-manifest"},
	{CapDeleteTag, "delete-tag"},
	{CapGarbageCollect, "garbage-collect"},
	{CapUsageStats, "usage-stats"},
}

// Names returns the operations c declares, in declaration order. An unknown
// bit — one a newer adapter set and this build does not know — is reported as
// "unknown-0xN" rather than dropped: a reader has to see that something is
// there.
func (c Capability) Names() []string {
	out := make([]string, 0, len(capabilityNames))
	var known Capability
	for _, entry := range capabilityNames {
		known |= entry.bit
		if c&entry.bit != 0 {
			out = append(out, entry.name)
		}
	}
	if rest := c &^ known; rest != 0 {
		out = append(out, fmt.Sprintf("unknown-%#x", uint32(rest)))
	}
	return out
}

// Has reports whether c declares every bit of want.
func (c Capability) Has(want Capability) bool { return c&want == want }

// Adapter is the vendor-neutral lifecycle API used by the repo commands.
// Destructive methods are deliberately manifest based: OCI has no portable
// "delete just this tag" endpoint.
type Adapter interface {
	Name() string
	Capabilities(context.Context) (Capability, error)
	ListTags(context.Context, name.Repository) ([]TagInfo, error)
	DeleteTag(context.Context, name.Tag, bool) error
	DeleteManifest(context.Context, name.Digest) error
	Usage(context.Context, name.Repository) (RepositoryStats, error)
}

type adapterFactory func(Keychain) Adapter

var adapterFactories = map[string]adapterFactory{}

// RegisterAdapter lets a vendor adapter override the generic OCI behaviour.
// The longest matching lower-case host suffix wins.
func RegisterAdapter(hostSuffix string, factory func(Keychain) Adapter) {
	adapterFactories[strings.ToLower(strings.TrimSpace(hostSuffix))] = adapterFactory(factory)
}

// AdapterFor returns the registered adapter for host, or the safe generic OCI
// adapter. ECR intentionally falls back to OCI read-only semantics until a
// separately audited SigV4 implementation exists.
func AdapterFor(host string, keychain Keychain) (Adapter, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil, fmt.Errorf("registry host is required")
	}
	var selected string
	for suffix := range adapterFactories {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			if len(suffix) > len(selected) {
				selected = suffix
			}
		}
	}
	if selected != "" {
		return adapterFactories[selected](keychain), nil
	}
	return &ociAdapter{host: host, keychain: keychain}, nil
}
