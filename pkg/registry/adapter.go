package registry

import (
	"context"
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
