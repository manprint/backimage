package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type ociAdapter struct {
	host     string
	keychain Keychain
}

func (a *ociAdapter) Name() string { return "oci" }
func (a *ociAdapter) Capabilities(context.Context) (Capability, error) {
	// DELETE is attempted only on the exact user-selected manifest. A probe
	// would itself be a destructive request on implementations with loose
	// digest validation, so availability is reported as protocol support.
	return CapListTags | CapDeleteManifest | CapDeleteTag | CapUsageStats, nil
}
func (a *ociAdapter) options(ctx context.Context) []remote.Option {
	o := []remote.Option{remote.WithContext(ctx)}
	if a.keychain != nil {
		o = append(o, remote.WithAuthFromKeychain(a.keychain))
	}
	return o
}
func (a *ociAdapter) ListTags(ctx context.Context, repo name.Repository) ([]TagInfo, error) {
	tags, err := remote.List(repo, a.options(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	sort.Strings(tags)
	infos := make([]TagInfo, 0, len(tags))
	for _, tag := range tags {
		ref := repo.Tag(tag)
		desc, err := remote.Get(ref, a.options(ctx)...)
		if err != nil {
			return nil, fmt.Errorf("read tag %s: %w", tag, err)
		}
		info := TagInfo{Tag: tag, Digest: desc.Digest, Size: desc.Size}
		if img, err := desc.Image(); err == nil {
			if cfg, err := img.ConfigFile(); err == nil {
				info.Created = cfg.Created.Time
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}
func (a *ociAdapter) DeleteManifest(ctx context.Context, ref name.Digest) error {
	if err := remote.Delete(ref, a.options(ctx)...); err != nil {
		return fmt.Errorf("delete manifest %s: %w; if this registry disables deletes, enable REGISTRY_STORAGE_DELETE_ENABLED=true", ref.Name(), err)
	}
	return nil
}
func (a *ociAdapter) DeleteTag(ctx context.Context, ref name.Tag, force bool) error {
	desc, err := remote.Get(ref, a.options(ctx)...)
	if err != nil {
		return fmt.Errorf("resolve tag %s: %w", ref.Name(), err)
	}
	all, err := a.ListTags(ctx, ref.Context())
	if err != nil {
		return err
	}
	shared := make([]string, 0, 1)
	for _, t := range all {
		if t.Digest.String() == desc.Digest.String() {
			shared = append(shared, t.Tag)
		}
	}
	if len(shared) > 1 && !force {
		return fmt.Errorf("refusing to delete %s: manifest is also referenced by tags %s (use --force to delete them together)", ref.Name(), strings.Join(shared, ", "))
	}
	digest := ref.Context().Digest(desc.Digest.String())
	return a.DeleteManifest(ctx, digest)
}
func (a *ociAdapter) Usage(ctx context.Context, repo name.Repository) (RepositoryStats, error) {
	return Stats(ctx, repo, a.keychain)
}
