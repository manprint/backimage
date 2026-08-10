package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/manprint/backimage/pkg/registry"
)

func newRepoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "inspect and clean up an OCI backup repository",
		Long: "inspect and clean up an OCI backup repository.\n\n" +
			"REPO is a repository without a tag, e.g. ghcr.io/me/dumps or\n" +
			"docker.io/me/backups. Credentials come from `backimage login`.",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "stats REPO",
		Short: "show shared OCI blobs and effective repository storage",
		Long: "show shared OCI blobs and effective repository storage.\n\n" +
			"Storage is what the registry stores once deduplication between tags is\n" +
			"taken into account; referred bytes is the sum over all tags.",
		Args: cobra.ExactArgs(1),
		RunE: runRepoStats,
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "tags REPO",
		Short: "list repository tags with digest and creation time",
		Long: "list repository tags with digest and creation time.\n\n" +
			"Columns: tag, manifest digest, creation time (RFC3339, UTC). A dash\n" +
			"means the image carries no creation timestamp; `prune` never removes\n" +
			"such a tag. Use --json for machine-readable output.",
		Args: cobra.ExactArgs(1),
		RunE: runRepoTags,
	})
	rm := &cobra.Command{
		Use:   "rm REPO:TAG|REPO@DIGEST",
		Short: "delete one tag or manifest from the registry",
		Long: "delete one tag or manifest from the registry.\n\n" +
			"Irreversible: the manifest is deleted by digest, and a registry with\n" +
			"deletion disabled rejects the request. When several tags point at the\n" +
			"same manifest the command refuses to proceed unless --force is given,\n" +
			"because deleting the manifest removes all of them at once.",
		Args: cobra.ExactArgs(1),
		RunE: runRepoRemove,
	}
	rm.Flags().Bool("force", false, "delete the manifest even when other tags point at it (they are removed too)")
	rm.Flags().Bool("yes", false, "required to actually delete: without it the command refuses to run")
	cmd.AddCommand(rm)
	prune := &cobra.Command{
		Use:   "prune REPO",
		Short: "delete old backup tags according to a retention policy",
		Long: "delete old backup tags according to a retention policy.\n\n" +
			"A tag is kept when at least one rule selects it, and deleted only when\n" +
			"no rule does. With no rule at all nothing is deleted. Tags without a\n" +
			"creation timestamp are always kept, so a non-backimage tag is never\n" +
			"removed by accident.\n\n" +
			"Durations accept " + durationUnitsHelp + ".\n\n" +
			"Examples:\n" +
			"  # keep the 7 newest backups, delete the rest\n" +
			"  backimage repo prune ghcr.io/me/dumps --keep-last 7 --dry-run\n\n" +
			"  # delete everything older than 3 days\n" +
			"  backimage repo prune ghcr.io/me/dumps --delete-older-than 3d --yes\n\n" +
			"  # delete backups older than 12 hours, but always keep the 2 newest\n" +
			"  # and every tag named release-*\n" +
			"  backimage repo prune ghcr.io/me/dumps --delete-older-than 12h \\\n" +
			"    --keep-last 2 --keep-tag 'release-*' --yes\n\n" +
			"Always run with --dry-run first: deletions cannot be undone.",
		Args: cobra.ExactArgs(1),
		RunE: runRepoPrune,
	}
	prune.Flags().Int("keep-last", 0, "keep the N newest backups regardless of age (0 = rule disabled)")
	prune.Flags().Var(newHumanDuration(new(time.Duration)), "keep-within",
		"keep backups newer than this age ("+durationUnitsHelp+")")
	prune.Flags().Var(newHumanDuration(new(time.Duration)), "delete-older-than",
		"delete backups older than this age; inverse wording of --keep-within, same rule ("+durationUnitsHelp+")")
	prune.Flags().StringSlice("keep-tag", nil, "glob pattern of tag names to keep, e.g. 'release-*' (repeatable)")
	prune.Flags().Bool("dry-run", false, "list what would be deleted and exit without touching the registry")
	prune.Flags().Bool("yes", false, "required to actually delete: without it the command refuses to run")
	cmd.AddCommand(prune)
	cmd.AddCommand(&cobra.Command{
		Use:   "caps REGISTRY",
		Short: "show which lifecycle operations a registry supports",
		Long: "show which lifecycle operations a registry supports.\n\n" +
			"REGISTRY is a host, e.g. ghcr.io or docker.io. Capabilities are the\n" +
			"protocol features the adapter implements, not a permission check on\n" +
			"your account.",
		Args: cobra.ExactArgs(1),
		RunE: runRepoCaps,
	})
	return cmd
}

// pruneDuration reads a duration flag declared through humanDuration.
func pruneDuration(cmd *cobra.Command, name string) (time.Duration, bool, error) {
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		return 0, false, fmt.Errorf("unknown flag --%s", name)
	}
	if !flag.Changed {
		return 0, false, nil
	}
	d, err := parseHumanDuration(flag.Value.String())
	if err != nil {
		return 0, false, fmt.Errorf("--%s: %w", name, err)
	}
	if d < 0 {
		return 0, false, fmt.Errorf("--%s cannot be negative", name)
	}
	return d, true, nil
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
	a, err := registry.AdapterFor(repo.RegistryStr(), registry.NewKeychainForUser(nil, store, registryUser(cmd)))
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
		if err := printerResult(pr, fmt.Sprintf("%s\t%s\t%s", tag.Tag, tag.Digest.String(), tagCreatedColumn(tag))); err != nil {
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
	// The policy is resolved before any network call: a flag mistake must not
	// cost a round trip to the registry.
	keepWithin, keepWithinSet, err := pruneDuration(cmd, "keep-within")
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	olderThan, olderThanSet, err := pruneDuration(cmd, "delete-older-than")
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	// The two flags express the same rule from opposite sides; accepting both
	// would leave the reader guessing which one won.
	if keepWithinSet && olderThanSet {
		return New(KindUsage, "", "--keep-within and --delete-older-than are the same rule worded differently: use one")
	}
	if olderThanSet {
		keepWithin = olderThan
	}
	keepTags, err := cmd.Flags().GetStringSlice("keep-tag")
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	p := registry.Policy{KeepLast: getFlagInt(cmd, "keep-last"), KeepWithin: keepWithin, KeepTags: keepTags}

	a, opts, err := repoAdapter(cmd, repo)
	if err != nil {
		return New(KindGeneric, "", "%v", err)
	}
	tags, err := a.ListTags(cmd.Context(), repo)
	if err != nil {
		return New(KindNetwork, "", "%v", err)
	}
	keep, remove := p.Apply(tags, time.Now())
	dry := getFlagBool(cmd, "dry-run")
	if len(remove) > 0 && !dry && !getFlagBool(cmd, "yes") {
		return New(KindUsage, "", "refusing destructive prune without --yes: %d tag(s) would be deleted, run with --dry-run to review them", len(remove))
	}
	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	if !dry {
		for _, tag := range remove {
			if err := a.DeleteTag(cmd.Context(), repo.Tag(tag.Tag), false); err != nil {
				return New(KindNetwork, "", "%v", err)
			}
		}
	}
	if opts.JSON {
		return printerResult(pr, map[string]any{"dryRun": dry, "kept": len(keep), "remove": remove})
	}
	return printerResult(pr, prunePlanText(dry, p, keep, remove))
}

// prunePlanText renders the prune outcome for a human: which tags go, which
// rules were active, and whether anything was actually deleted.
func prunePlanText(dry bool, p registry.Policy, keep, remove []registry.TagInfo) string {
	var b strings.Builder
	rules := activePruneRules(p)
	if len(rules) == 0 {
		return "nessuna regola di retention indicata: nulla da eliminare.\n" +
			"Usare --keep-last, --keep-within/--delete-older-than o --keep-tag; " +
			"`backimage repo prune --help` mostra gli esempi."
	}
	fmt.Fprintf(&b, "regole attive: %s\n", strings.Join(rules, ", "))
	if len(remove) == 0 {
		fmt.Fprintf(&b, "nessun tag da eliminare (%d conservati).", len(keep))
		return b.String()
	}
	verb := "eliminati"
	if dry {
		verb = "da eliminare (dry-run, nessuna modifica al registry)"
	}
	fmt.Fprintf(&b, "%d tag %s, %d conservati:\n", len(remove), verb, len(keep))
	for _, tag := range remove {
		fmt.Fprintf(&b, "  %s\t%s\t%s\n", tag.Tag, tagCreatedColumn(tag), tag.Digest.String())
	}
	if dry {
		b.WriteString("ripetere senza --dry-run e con --yes per applicare.")
	}
	return strings.TrimRight(b.String(), "\n")
}

func activePruneRules(p registry.Policy) []string {
	var rules []string
	if p.KeepLast > 0 {
		rules = append(rules, fmt.Sprintf("mantieni i %d più recenti", p.KeepLast))
	}
	if p.KeepWithin > 0 {
		rules = append(rules, fmt.Sprintf("mantieni più recenti di %s", formatHumanDuration(p.KeepWithin)))
	}
	if len(p.KeepTags) > 0 {
		rules = append(rules, fmt.Sprintf("mantieni i tag %s", strings.Join(p.KeepTags, ", ")))
	}
	return rules
}

// tagCreatedColumn renders a creation time, or a dash when the image carries
// none: the zero time printed as 0001-01-01 reads like a real date.
func tagCreatedColumn(tag registry.TagInfo) string {
	if tag.Created.IsZero() {
		return "-"
	}
	return tag.Created.UTC().Format(time.RFC3339)
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
	stats, err := registry.Stats(cmd.Context(), repo, registry.NewKeychainForUser(nil, store, registryUser(cmd)))
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
