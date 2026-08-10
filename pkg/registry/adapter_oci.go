package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// annotationCreated is the OCI annotation backimage writes on every image and
// index it pushes; it is the primary source of a tag's creation time.
const annotationCreated = "org.opencontainers.image.created"

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
		info.Created = a.tagCreated(ctx, repo, desc)
		infos = append(infos, info)
	}
	return infos, nil
}

// tagCreated resolves the creation time of a tag. A backimage tag points at a
// multi-arch index, so desc.Image() fails and the per-platform config must be
// reached through a child manifest; retention treats a zero time as "unknown"
// and never prunes it, which is why every step below has a fallback.
func (a *ociAdapter) tagCreated(ctx context.Context, repo name.Repository, desc *remote.Descriptor) time.Time {
	// 1) annotations on the manifest or index itself: no extra request.
	if ts, ok := createdFromManifest(desc.Manifest); ok {
		return ts
	}
	// 2) single-platform image: read its config.
	if !desc.MediaType.IsIndex() {
		if img, err := desc.Image(); err == nil {
			if cfg, err := img.ConfigFile(); err == nil {
				return cfg.Created.Time
			}
		}
		return time.Time{}
	}
	// 3) index: the platform images carry the timestamp in config and
	// annotations. Data layers are identical across platforms, so the first
	// child that answers is authoritative.
	idx, err := desc.ImageIndex()
	if err != nil {
		return time.Time{}
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return time.Time{}
	}
	for _, child := range manifest.Manifests {
		if ts, ok := createdFromAnnotations(child.Annotations); ok {
			return ts
		}
		img, err := idx.Image(child.Digest)
		if err != nil {
			continue
		}
		if raw, err := img.RawManifest(); err == nil {
			if ts, ok := createdFromManifest(raw); ok {
				return ts
			}
		}
		if cfg, err := img.ConfigFile(); err == nil && !cfg.Created.Time.IsZero() {
			return cfg.Created.Time
		}
	}
	return time.Time{}
}

// createdFromManifest reads org.opencontainers.image.created from the
// annotations of a raw manifest or index document.
func createdFromManifest(raw []byte) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var doc struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return time.Time{}, false
	}
	return createdFromAnnotations(doc.Annotations)
}

func createdFromAnnotations(ann map[string]string) (time.Time, bool) {
	v := ann[annotationCreated]
	if v == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, v)
	if err != nil || ts.IsZero() {
		return time.Time{}, false
	}
	return ts, true
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
