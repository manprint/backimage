package cli

import (
	"fmt"
	"regexp"
	"slices"
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
	tags := &cobra.Command{
		Use:   "tags REPO",
		Short: "list repository tags with digest and creation time",
		Long: "list repository tags with digest and creation time.\n\n" +
			"Columns: tag, manifest digest, creation time (RFC3339, UTC). A dash\n" +
			"means the image carries no creation timestamp; `prune` never removes\n" +
			"such a tag. Use --json for machine-readable output.\n\n" +
			"--tag-regex and --group-by-regex are the very selectors `prune` uses,\n" +
			"evaluated by the same code: run them here first to see exactly which\n" +
			"tags a prune would consider, with no way to delete anything.\n\n" +
			"Examples:\n" +
			"  # which tags would `prune --tag-regex` act on?\n" +
			"  backimage repo tags ghcr.io/me/dumps --tag-regex 'db_.*'\n\n" +
			"  # which groups would `prune --group-by-regex` see?\n" +
			"  backimage repo tags ghcr.io/me/dumps --group-by-regex '([a-z]+)_.*'",
		Args: cobra.ExactArgs(1),
		RunE: runRepoTags,
	}
	addTagSelectorFlags(tags)
	cmd.AddCommand(tags)
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
			"  # of the database backups keep the 3 newest; leave every other tag alone\n" +
			"  backimage repo prune ghcr.io/me/dumps --tag-regex 'db_.*' --keep-last 3 --dry-run\n\n" +
			"  # keep the 3 newest of every family (db_*, app_*, ...) in one pass\n" +
			"  backimage repo prune ghcr.io/me/dumps --group-by-regex '([a-z]+)_.*' \\\n" +
			"    --keep-last 3 --dry-run\n\n" +
			"A regex only narrows what the rules reach: it never selects a tag for\n" +
			"deletion by itself, so --tag-regex without a retention rule deletes\n" +
			"nothing. The pattern must match the whole tag: 'db_' selects nothing,\n" +
			"'db_.*' selects db_1. Preview a selection with `repo tags --tag-regex`,\n" +
			"which cannot delete anything.\n\n" +
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
	addTagSelectorFlags(prune)
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

// addTagSelectorFlags declares the two tag selectors on a command. `prune` and
// `tags` must share them verbatim: if the preview offered by `tags` described a
// selection different from the one `prune` acts on, it would be worse than no
// preview at all.
func addTagSelectorFlags(cmd *cobra.Command) {
	cmd.Flags().String("tag-regex", "",
		"restrict the operation to tag names matching this regex; the pattern must match the whole tag, and non-matching tags are never touched")
	cmd.Flags().String("group-by-regex", "",
		"partition tags by the capture group(s) of this regex, e.g. '([a-z]+)_.*'; retention rules then apply independently inside each group (whole-tag match, at least one capture group)")
}

// tagSelectors compiles the two selector flags. A flag left out yields nil,
// meaning no restriction; an explicitly empty value is rejected, because a
// --tag-regex ” silently standing for "every tag" would be a trap on a command
// that deletes. Compilation happens before any network call, so a typo in a
// pattern costs no round trip.
func tagSelectors(cmd *cobra.Command) (scope, groupBy *regexp.Regexp, err error) {
	if scope, err = compileTagFlag(cmd, "tag-regex"); err != nil {
		return nil, nil, err
	}
	if groupBy, err = compileTagFlag(cmd, "group-by-regex"); err != nil {
		return nil, nil, err
	}
	if groupBy != nil && groupBy.NumSubexp() == 0 {
		return nil, nil, fmt.Errorf("--group-by-regex serve almeno un gruppo di cattura per costruire la chiave di gruppo: " +
			"senza, ogni tag sarebbe un gruppo a sé e --keep-last li conserverebbe tutti; ad esempio '([a-z]+)_.*'")
	}
	return scope, groupBy, nil
}

func compileTagFlag(cmd *cobra.Command, name string) (*regexp.Regexp, error) {
	flag := cmd.Flags().Lookup(name)
	if flag == nil || !flag.Changed {
		return nil, nil
	}
	raw := flag.Value.String()
	re, err := registry.CompileTagPattern(raw)
	if err != nil {
		return nil, fmt.Errorf("--%s %q: %w", name, raw, err)
	}
	return re, nil
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
	if d <= 0 {
		return 0, false, fmt.Errorf("--%s must be greater than zero", name)
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
	scope, groupBy, err := tagSelectors(cmd)
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
	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	if scope == nil && groupBy == nil {
		// Without a selector the listing stays exactly what it was: same order,
		// same rows, same JSON shape.
		if opts.JSON {
			return printerResult(pr, tags)
		}
		return printTagRows(pr, "", tags)
	}
	// Select through the policy, not through a local match: this is the preview
	// of an irreversible command, and it must not be able to disagree with it.
	sel := (registry.Policy{Scope: scope, GroupBy: groupBy}).Select(tags)
	selected := sel.Selected()
	if opts.JSON {
		// The shape stays the array v0.3.0 emitted; the selectors filter its
		// contents, and its length is the match count.
		return printerResult(pr, selected)
	}
	if err := printerResult(pr, tagSelectionSummary(scope, groupBy, sel)); err != nil {
		return err
	}
	if groupBy == nil {
		return printTagRows(pr, "", selected)
	}
	for _, g := range sel.Groups {
		if err := printerResult(pr, fmt.Sprintf("gruppo %q — %d tag", g.Key, g.Tags)); err != nil {
			return err
		}
		if err := printTagRows(pr, "  ", g.Keep); err != nil {
			return err
		}
	}
	return nil
}

func printTagRows(pr Printer, indent string, tags []registry.TagInfo) error {
	for _, tag := range tags {
		if err := printerResult(pr, fmt.Sprintf("%s%s\t%s\t%s", indent, tag.Tag, tag.Digest.String(), tagCreatedColumn(tag))); err != nil {
			return err
		}
	}
	return nil
}

// tagSelectionSummary states how much of the repository a selector reached.
// Zero of many means a typo; all of all means a pattern wider than intended.
// Both are cheap to see here and expensive to discover after a prune.
func tagSelectionSummary(scope, groupBy *regexp.Regexp, sel registry.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "selezionati %d tag su %d", len(sel.Selected()), sel.Total)
	if scope != nil {
		fmt.Fprintf(&b, " (--tag-regex %q)", registry.PatternSource(scope))
	}
	if groupBy != nil {
		fmt.Fprintf(&b, " in %d grupp%s (--group-by-regex %q)", len(sel.Groups), plural(len(sel.Groups), "o", "i"), registry.PatternSource(groupBy))
		if sel.Ungrouped > 0 {
			fmt.Fprintf(&b, ", %d non raggruppat%s", sel.Ungrouped, plural(sel.Ungrouped, "o", "i"))
		}
	}
	if len(sel.Groups) == 0 {
		b.WriteString("\nIl pattern deve corrispondere al tag intero: 'db_' non seleziona nulla, 'db_.*' sì.")
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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
	scope, groupBy, err := tagSelectors(cmd)
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	p := registry.Policy{
		KeepLast:   getFlagInt(cmd, "keep-last"),
		KeepWithin: keepWithin,
		KeepTags:   keepTags,
		Scope:      scope,
		GroupBy:    groupBy,
	}

	a, opts, err := repoAdapter(cmd, repo)
	if err != nil {
		return New(KindGeneric, "", "%v", err)
	}
	tags, err := a.ListTags(cmd.Context(), repo)
	if err != nil {
		return New(KindNetwork, "", "%v", err)
	}
	// One resolution, one clock: what gets rendered and what gets deleted come
	// from the same plan.
	plan := p.PlanFor(tags, time.Now())
	dry := getFlagBool(cmd, "dry-run")
	if len(plan.Remove) > 0 && !dry && !getFlagBool(cmd, "yes") {
		return New(KindUsage, "", "refusing destructive prune without --yes: %d tag(s) would be deleted, run with --dry-run to review them", len(plan.Remove))
	}
	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	if !dry {
		if err := pruneDelete(cmd, a, repo, tags, plan.Remove); err != nil {
			return err
		}
	}
	if opts.JSON {
		return printerResult(pr, pruneJSON(dry, p, plan))
	}
	return printerResult(pr, prunePlanText(dry, p, plan))
}

// pruneDelete removes the planned manifests. The whole plan is validated before
// the first request: a manifest another tag still points at cannot be deleted
// without taking that tag down too, and discovering it halfway through the loop
// would leave the registry matching neither the plan nor the starting point.
//
// Deletion is then one request per distinct manifest. Several tags can share
// one, the OCI delete is by digest anyway, and going through DeleteTag would
// re-list the whole repository on every single tag.
func pruneDelete(cmd *cobra.Command, a registry.Adapter, repo name.Repository, all, remove []registry.TagInfo) error {
	digests, err := pruneDigests(remove)
	if err != nil {
		return New(KindGeneric, "", "%v", err)
	}
	if conflicts := sharedManifestConflicts(all, remove); len(conflicts) > 0 {
		return New(KindUsage, "",
			"il prune si fermerebbe a metà strada: alcuni manifest da eliminare sono referenziati anche da tag che la policy conserva\n%s\n"+
				"nessun tag è stato rimosso. Restringere il pattern, oppure eliminare insieme i tag che condividono il manifest con `repo rm --force`.",
			strings.Join(conflicts, "\n"))
	}
	// The pre-check removes the predictable reason for stopping halfway, but a
	// sequence of HTTP deletions cannot be atomic: a transient failure still
	// leaves part of the plan applied. Say how far it got, so the operator knows
	// what state the repository is in without having to diff it.
	for i, digest := range digests {
		if err := a.DeleteManifest(cmd.Context(), repo.Digest(digest)); err != nil {
			return New(KindNetwork, "",
				"eliminazione interrotta su %s: %v\n%d manifest su %d erano già stati eliminati; "+
					"rieseguire lo stesso prune per completare",
				digest, err, i, len(digests))
		}
	}
	return nil
}

// pruneDigests lists the distinct manifests behind the removal set, in the order
// the user was shown. A tag without a digest cannot be deleted by digest, and
// guessing one is not an option on an irreversible path.
func pruneDigests(remove []registry.TagInfo) ([]string, error) {
	seen := map[string]bool{}
	digests := make([]string, 0, len(remove))
	for _, tag := range remove {
		if tag.Digest.Algorithm == "" || tag.Digest.Hex == "" {
			return nil, fmt.Errorf("il tag %q non riporta un digest e non si può eliminare in sicurezza", tag.Tag)
		}
		digest := tag.Digest.String()
		if seen[digest] {
			continue
		}
		seen[digest] = true
		digests = append(digests, digest)
	}
	return digests, nil
}

// sharedManifestConflicts reports the manifests in remove that the repository
// still reaches through a tag nobody asked to delete. One line per manifest,
// naming both sides: the remedy depends on which tags are involved.
func sharedManifestConflicts(all, remove []registry.TagInfo) []string {
	doomed := map[string][]string{}
	for _, tag := range remove {
		digest := tag.Digest.String()
		doomed[digest] = append(doomed[digest], tag.Tag)
	}
	survivors := map[string][]string{}
	for _, tag := range all {
		digest := tag.Digest.String()
		if _, planned := doomed[digest]; !planned {
			continue
		}
		if !slices.Contains(doomed[digest], tag.Tag) {
			survivors[digest] = append(survivors[digest], tag.Tag)
		}
	}
	lines := make([]string, 0, len(survivors))
	for _, tag := range remove {
		digest := tag.Digest.String()
		kept, ok := survivors[digest]
		if !ok {
			continue
		}
		delete(survivors, digest)
		lines = append(lines, fmt.Sprintf("  %s\n    da eliminare: %s\n    conservati:   %s",
			digest, strings.Join(doomed[digest], ", "), strings.Join(kept, ", ")))
	}
	return lines
}

// pruneJSON keeps the fields v0.3.0 emitted where they were and adds the
// selector breakdown beside them, so an existing consumer keeps working.
func pruneJSON(dry bool, p registry.Policy, plan registry.Plan) map[string]any {
	out := map[string]any{"dryRun": dry, "kept": len(plan.Keep), "remove": plan.Remove}
	if p.Scope != nil {
		out["scope"] = map[string]any{
			"tagRegex": registry.PatternSource(p.Scope),
			"matched":  plan.InScope,
			"total":    plan.Total,
		}
	}
	if p.GroupBy != nil {
		groups := make([]map[string]any, 0, len(plan.Groups))
		for _, g := range plan.Groups {
			groups = append(groups, map[string]any{"key": g.Key, "tags": g.Tags, "kept": len(g.Keep), "remove": g.Remove})
		}
		out["groupBy"] = map[string]any{
			"regex":     registry.PatternSource(p.GroupBy),
			"groups":    len(plan.Groups),
			"ungrouped": plan.Ungrouped,
		}
		out["groups"] = groups
	}
	return out
}

// prunePlanText renders the prune outcome for a human: which tags go, which
// rules were active, how far the selectors reached, and whether anything was
// actually deleted. With no selector the output is what v0.3.0 printed.
func prunePlanText(dry bool, p registry.Policy, plan registry.Plan) string {
	var b strings.Builder
	rules := activePruneRules(p)
	if len(rules) == 0 {
		// A selector is not a rule, so this is still the no-rule case even when
		// --tag-regex was given, and nothing is deleted.
		return "nessuna regola di retention indicata: nulla da eliminare.\n" +
			"Usare --keep-last, --keep-within/--delete-older-than o --keep-tag; " +
			"`backimage repo prune --help` mostra gli esempi."
	}
	fmt.Fprintf(&b, "regole attive: %s\n", strings.Join(rules, ", "))
	if p.Scope != nil {
		fmt.Fprintf(&b, "ambito: --tag-regex %q — %d tag su %d selezionati\n",
			registry.PatternSource(p.Scope), plan.InScope, plan.Total)
	}
	if p.GroupBy != nil {
		fmt.Fprintf(&b, "gruppi: %d (--group-by-regex %q)", len(plan.Groups), registry.PatternSource(p.GroupBy))
		if plan.Ungrouped > 0 {
			suffix := plural(plan.Ungrouped, "o", "i")
			fmt.Fprintf(&b, ", %d tag non raggruppat%s e quindi non toccat%s", plan.Ungrouped, suffix, suffix)
		}
		b.WriteString("\n")
	}
	if (p.Scope != nil || p.GroupBy != nil) && len(plan.Groups) == 0 {
		// Zero matches on a destructive command is a typo, not a success.
		return b.String() + "nessun tag corrisponde al pattern: nulla da eliminare.\n" +
			"Il pattern deve corrispondere al tag intero: 'db_' non seleziona nulla, 'db_.*' sì."
	}
	if len(plan.Remove) == 0 {
		fmt.Fprintf(&b, "nessun tag da eliminare (%d conservati).", len(plan.Keep))
		return b.String()
	}
	verb := "eliminati"
	if dry {
		verb = "da eliminare (dry-run, nessuna modifica al registry)"
	}
	fmt.Fprintf(&b, "%d tag %s, %d conservati:\n", len(plan.Remove), verb, len(plan.Keep))
	if p.GroupBy != nil {
		// Every group is listed, including the ones losing nothing: seeing a
		// group that was read and fully kept is what makes the plan checkable.
		for _, g := range plan.Groups {
			fmt.Fprintf(&b, "  gruppo %q — %d tag: %d conservati, %d da eliminare\n",
				g.Key, g.Tags, len(g.Keep), len(g.Remove))
			for _, tag := range g.Remove {
				fmt.Fprintf(&b, "    %s\t%s\t%s\n", tag.Tag, tagCreatedColumn(tag), tag.Digest.String())
			}
		}
	} else {
		for _, tag := range plan.Remove {
			fmt.Fprintf(&b, "  %s\t%s\t%s\n", tag.Tag, tagCreatedColumn(tag), tag.Digest.String())
		}
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
