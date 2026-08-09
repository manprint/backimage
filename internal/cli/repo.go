package cli

import (
	"fmt"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/fpierri/backimage/pkg/registry"
)

func newRepoCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "repo", Short: "inspect an OCI backup repository"}
	cmd.AddCommand(&cobra.Command{
		Use:   "stats REPO",
		Short: "show shared OCI blobs and effective repository storage",
		Args:  cobra.ExactArgs(1),
		RunE:  runRepoStats,
	})
	cmd.AddCommand(&cobra.Command{Use: "tags REPO", Short: "list repository tags", Args: cobra.ExactArgs(1), RunE: runRepoTags})
	rm := &cobra.Command{Use: "rm REPO:TAG|REPO@DIGEST", Short: "delete an OCI manifest", Args: cobra.ExactArgs(1), RunE: runRepoRemove}
	rm.Flags().Bool("force", false, "delete a manifest even when other tags reference it")
	rm.Flags().Bool("yes", false, "confirm this destructive operation")
	cmd.AddCommand(rm)
	prune := &cobra.Command{Use: "prune REPO", Short: "apply a retention policy", Args: cobra.ExactArgs(1), RunE: runRepoPrune}
	prune.Flags().Int("keep-last", 0, "keep the newest N backups")
	prune.Flags().Duration("keep-within", 0, "keep backups newer than this duration")
	prune.Flags().StringSlice("keep-tag", nil, "glob pattern for tags to keep")
	prune.Flags().Bool("dry-run", false, "show deletions without changing the registry")
	prune.Flags().Bool("yes", false, "confirm destructive deletions")
	cmd.AddCommand(prune)
	cmd.AddCommand(&cobra.Command{Use: "caps REGISTRY", Short: "show repository lifecycle capabilities", Args: cobra.ExactArgs(1), RunE: runRepoCaps})
	return cmd
}

func repoAdapter(cmd *cobra.Command, repo name.Repository) (registry.Adapter, Options, error) {
	opts, err := parseOptions(cmd.Root())
	if err != nil {
		return nil, Options{}, err
	}
	store, err := registry.NewStore(authFilePath())
	if err != nil {
		return nil, Options{}, err
	}
	a, err := registry.AdapterFor(repo.RegistryStr(), registry.NewKeychain(nil, store))
	return a, opts, err
}

func runRepoTags(cmd *cobra.Command, args []string) error {
	repo, err := name.NewRepository(args[0])
	if err != nil {
		return New(KindUsage, "", "repository %q: %v", args[0], err)
	}
	a, opts, err := repoAdapter(cmd, repo)
	if err != nil {
		return New(KindGeneric, "", "%v", err)
	}
	tags, err := a.ListTags(cmd.Context(), repo)
	if err != nil {
		return New(KindNetwork, "", "%v", err)
	}
	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	if opts.JSON {
		return printerResult(pr, tags)
	}
	for _, tag := range tags {
		if err := printerResult(pr, fmt.Sprintf("%s\t%s\t%s", tag.Tag, tag.Digest.String(), tag.Created.UTC().Format(time.RFC3339))); err != nil {
			return err
		}
	}
	return nil
}

func runRepoRemove(cmd *cobra.Command, args []string) error {
	if !getFlagBool(cmd, "yes") {
		return New(KindUsage, "", "refusing destructive deletion without --yes")
	}
	ref, err := name.ParseReference(args[0])
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	a, opts, err := repoAdapter(cmd, ref.Context())
	if err != nil {
		return New(KindGeneric, "", "%v", err)
	}
	if d, ok := ref.(name.Digest); ok {
		err = a.DeleteManifest(cmd.Context(), d)
	} else if t, ok := ref.(name.Tag); ok {
		err = a.DeleteTag(cmd.Context(), t, getFlagBool(cmd, "force"))
	} else {
		err = fmt.Errorf("unsupported reference")
	}
	if err != nil {
		return New(KindNetwork, "", "%v", err)
	}
	return printerResult(NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts), map[string]any{"deleted": ref.Name()})
}

func runRepoPrune(cmd *cobra.Command, args []string) error {
	repo, err := name.NewRepository(args[0])
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	a, opts, err := repoAdapter(cmd, repo)
	if err != nil {
		return New(KindGeneric, "", "%v", err)
	}
	tags, err := a.ListTags(cmd.Context(), repo)
	if err != nil {
		return New(KindNetwork, "", "%v", err)
	}
	keepWithin, err := cmd.Flags().GetDuration("keep-within")
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	keepTags, err := cmd.Flags().GetStringSlice("keep-tag")
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	p := registry.Policy{KeepLast: getFlagInt(cmd, "keep-last"), KeepWithin: keepWithin, KeepTags: keepTags}
	_, remove := p.Apply(tags, time.Now())
	dry := getFlagBool(cmd, "dry-run")
	if len(remove) > 0 && !dry && !getFlagBool(cmd, "yes") {
		return New(KindUsage, "", "refusing destructive prune without --yes")
	}
	if !dry {
		for _, tag := range remove {
			if err := a.DeleteTag(cmd.Context(), repo.Tag(tag.Tag), false); err != nil {
				return New(KindNetwork, "", "%v", err)
			}
		}
	}
	return printerResult(NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts), map[string]any{"dryRun": dry, "remove": remove})
}

func runRepoCaps(cmd *cobra.Command, args []string) error {
	a, err := registry.AdapterFor(args[0], nil)
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	caps, err := a.Capabilities(cmd.Context())
	if err != nil {
		return New(KindNetwork, "", "%v", err)
	}
	return printerResult(NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), Options{}), map[string]any{"adapter": a.Name(), "capabilities": caps})
}

func runRepoStats(cmd *cobra.Command, args []string) error {
	opts, err := parseOptions(cmd.Root())
	if err != nil {
		return New(KindGeneric, "", "%v", err)
	}
	repo, err := name.NewRepository(args[0])
	if err != nil {
		return New(KindUsage, "", "repository %q: %v", args[0], err)
	}
	store, err := registry.NewStore(authFilePath())
	if err != nil {
		return New(KindGeneric, "", "credential store: %v", err)
	}
	stats, err := registry.Stats(cmd.Context(), repo, registry.NewKeychain(nil, store))
	if err != nil {
		return New(KindNetwork, "", "%v", err)
	}
	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	if opts.JSON {
		return printerResult(pr, stats)
	}
	return printerResult(pr, fmt.Sprintf("repository %s\n  tag       %d\n  blob      %d unici (%d condivisi)\n  storage   %d byte\n  riferiti  %d byte",
		repo.Name(), stats.Tags, stats.UniqueBlobs, stats.SharedBlobs, stats.StorageBytes, stats.ReferencedBytes))
}
