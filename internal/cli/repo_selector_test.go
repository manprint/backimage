package cli

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/manprint/backimage/pkg/registry"
)

// --- Flag validation: every mistake must cost zero network round trips -------

func TestTagSelectorFlagsRejectBadPatterns(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"invalid tag-regex", []string{"--tag-regex", "db_["}, "--tag-regex"},
		{"invalid group-by-regex", []string{"--group-by-regex", "db_[)"}, "--group-by-regex"},
		// An explicit empty value must not silently stand for "every tag" on a
		// command that deletes.
		{"empty tag-regex", []string{"--tag-regex", ""}, "vuoto"},
		{"blank tag-regex", []string{"--tag-regex", "   "}, "vuoto"},
		// Without a capture group every tag would be its own group and
		// --keep-last would keep them all: a silent no-op, not a policy.
		{"group-by without capture group", []string{"--group-by-regex", "db_.*"}, "gruppo di cattura"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, sub := range []string{"prune", "tags"} {
				args := append([]string{"repo", sub, "example.invalid/me/x"}, c.args...)
				if sub == "prune" {
					args = append(args, "--keep-last", "3", "--dry-run")
				}
				_, _, err := runRoot(t, args...)
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("repo %s: error = %v, want it to mention %q", sub, err, c.want)
				}
				// example.invalid does not resolve: a network error here would
				// mean the pattern was compiled after the first request.
				if strings.Contains(err.Error(), "example.invalid") {
					t.Fatalf("repo %s reached the registry before validating: %v", sub, err)
				}
			}
		})
	}
}

func TestTagSelectorFlagsAreSharedByPruneAndTags(t *testing.T) {
	cmd := newRepoCommand()
	for _, sub := range []string{"prune", "tags"} {
		c, _, err := cmd.Find([]string{sub})
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"tag-regex", "group-by-regex"} {
			f := c.Flags().Lookup(flag)
			if f == nil {
				t.Fatalf("repo %s has no --%s", sub, flag)
			}
			// The preview must describe the very selection the prune acts on,
			// down to the help text the user reads.
			if !strings.Contains(f.Usage, "whole tag") && !strings.Contains(f.Usage, "whole-tag") {
				t.Fatalf("repo %s --%s does not document the whole-tag match: %q", sub, flag, f.Usage)
			}
		}
	}
}

// --- Rendering ---------------------------------------------------------------

func TestPrunePlanTextWithoutSelectorsIsUnchanged(t *testing.T) {
	// Golden text of v0.3.0: adding selectors must not move a byte for anyone
	// who does not use them.
	created := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	tags := []registry.TagInfo{
		{Tag: "old", Created: created, Digest: v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("ab", 32)}},
	}
	p := registry.Policy{KeepLast: 2}
	got := prunePlanText(true, p, registry.Plan{Keep: nil, Remove: tags})
	want := "regole attive: mantieni i 2 più recenti\n" +
		"1 tag da eliminare (dry-run, nessuna modifica al registry), 0 conservati:\n" +
		"  old\t2026-08-01T10:00:00Z\tsha256:" + strings.Repeat("ab", 32) + "\n" +
		"ripetere senza --dry-run e con --yes per applicare."
	if got != want {
		t.Fatalf("text drifted:\n got %q\nwant %q", got, want)
	}
}

func TestPrunePlanTextReportsScopeAndGroups(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var tags []registry.TagInfo
	for _, family := range []string{"db", "app"} {
		for i := 1; i <= 4; i++ {
			tags = append(tags, registry.TagInfo{
				Tag:     family + "_" + strconv.Itoa(i),
				Created: now.AddDate(0, 0, -i),
				Digest:  v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("cd", 32)},
			})
		}
	}
	tags = append(tags, registry.TagInfo{Tag: "latest", Created: now.AddDate(0, 0, -9)})

	p := registry.Policy{
		KeepLast: 3,
		Scope:    mustCompile(t, `db_.*|app_.*`),
		GroupBy:  mustCompile(t, `([a-z]+)_.*`),
	}
	text := prunePlanText(true, p, p.PlanFor(tags, now))
	for _, want := range []string{
		`ambito: --tag-regex "db_.*|app_.*" — 8 tag su 9 selezionati`,
		`gruppi: 2 (--group-by-regex "([a-z]+)_.*")`,
		`gruppo "app" — 4 tag: 3 conservati, 1 da eliminare`,
		`gruppo "db" — 4 tag: 3 conservati, 1 da eliminare`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
	// The user must see the anchored pattern they typed, not the wrapped one.
	if strings.Contains(text, `\A(?:`) {
		t.Fatalf("the internal anchoring leaked into the output: %q", text)
	}
}

// Zero matches on a destructive command is a typo, and the message must say how
// the anchoring works instead of reporting a quiet success.
func TestPrunePlanTextZeroMatchExplainsTheAnchoring(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := []registry.TagInfo{{Tag: "db_1", Created: now.AddDate(0, 0, -3)}}
	p := registry.Policy{KeepLast: 1, Scope: mustCompile(t, `db_`)}
	text := prunePlanText(true, p, p.PlanFor(tags, now))
	for _, want := range []string{"nessun tag corrisponde", "tag intero", "'db_.*'"} {
		if !strings.Contains(text, want) {
			t.Fatalf("zero-match text %q missing %q", text, want)
		}
	}
}

// A selector is not a rule: --tag-regex alone must land in the no-rule branch.
func TestPrunePlanTextSelectorAloneIsStillNoRule(t *testing.T) {
	p := registry.Policy{Scope: mustCompile(t, `db_.*`)}
	text := prunePlanText(false, p, p.PlanFor(nil, time.Now()))
	if !strings.Contains(text, "nessuna regola") {
		t.Fatalf("text = %q", text)
	}
}

func TestPruneJSONKeepsTheOldFieldsAndAddsTheBreakdown(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := []registry.TagInfo{
		{Tag: "db_1", Created: now.AddDate(0, 0, -1)},
		{Tag: "db_2", Created: now.AddDate(0, 0, -2)},
		{Tag: "app_1", Created: now.AddDate(0, 0, -1)},
	}

	plain := registry.Policy{KeepLast: 1}
	out := pruneJSON(true, plain, plain.PlanFor(tags, now))
	for _, key := range []string{"dryRun", "kept", "remove"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("the v0.3.0 field %q disappeared", key)
		}
	}
	// Without selectors nothing new is emitted, so no consumer sees a change.
	for _, key := range []string{"scope", "groupBy", "groups"} {
		if _, ok := out[key]; ok {
			t.Fatalf("field %q appeared without a selector", key)
		}
	}

	p := registry.Policy{KeepLast: 1, Scope: mustCompile(t, `db_.*`), GroupBy: mustCompile(t, `([a-z]+)_.*`)}
	out = pruneJSON(true, p, p.PlanFor(tags, now))
	scope, ok := out["scope"].(map[string]any)
	if !ok {
		t.Fatalf("scope = %#v", out["scope"])
	}
	if scope["tagRegex"] != `db_.*` || scope["matched"] != 2 || scope["total"] != 3 {
		t.Fatalf("scope = %#v", scope)
	}
	groups, ok := out["groups"].([]map[string]any)
	if !ok || len(groups) != 1 || groups[0]["key"] != "db" {
		t.Fatalf("groups = %#v", out["groups"])
	}
}

// --- Shared manifests -------------------------------------------------------

func TestSharedManifestConflictsNamesBothSides(t *testing.T) {
	shared := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("11", 32)}
	lone := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("22", 32)}
	all := []registry.TagInfo{
		{Tag: "db_1", Digest: shared},
		{Tag: "app_1", Digest: shared},
		{Tag: "db_2", Digest: lone},
	}

	// db_1 goes, app_1 stays, and they are the same manifest: refuse.
	lines := sharedManifestConflicts(all, []registry.TagInfo{{Tag: "db_1", Digest: shared}})
	if len(lines) != 1 {
		t.Fatalf("conflicts = %#v", lines)
	}
	for _, want := range []string{shared.String(), "db_1", "app_1"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("conflict line %q missing %q", lines[0], want)
		}
	}

	// Both tags of the shared manifest are going: that is the legitimate case.
	lines = sharedManifestConflicts(all, []registry.TagInfo{
		{Tag: "db_1", Digest: shared},
		{Tag: "app_1", Digest: shared},
	})
	if len(lines) != 0 {
		t.Fatalf("a fully doomed manifest was reported as a conflict: %#v", lines)
	}

	// A manifest with a single tag is never a conflict.
	if lines = sharedManifestConflicts(all, []registry.TagInfo{{Tag: "db_2", Digest: lone}}); len(lines) != 0 {
		t.Fatalf("conflicts = %#v", lines)
	}
}

func TestPruneDigestsDedupesAndRefusesUndigestedTags(t *testing.T) {
	shared := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("33", 32)}
	other := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("44", 32)}
	got, err := pruneDigests([]registry.TagInfo{
		{Tag: "db_1", Digest: shared},
		{Tag: "app_1", Digest: shared},
		{Tag: "db_2", Digest: other},
	})
	if err != nil {
		t.Fatal(err)
	}
	// One request per manifest, in the order the plan showed them.
	if !slices.Equal(got, []string{shared.String(), other.String()}) {
		t.Fatalf("digests = %v", got)
	}
	if _, err := pruneDigests([]registry.TagInfo{{Tag: "db_1"}}); err == nil {
		t.Fatal("a tag without a digest was accepted for deletion")
	}
}

// --- Against a real registry -------------------------------------------------

func TestPruneTagRegexDeletesOnlyTheSelectedFamily(t *testing.T) {
	probe, repo, digests := newTestRepo(t, map[string]int{"db": 4, "app": 4})

	stdout, _, err := runRoot(t, "repo", "prune", repo, "--tag-regex", "db_.*",
		"--keep-last", "3", "--yes", "--json")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	var out struct {
		Kept   int `json:"kept"`
		Remove []struct {
			Tag string `json:"tag"`
		} `json:"remove"`
		Scope struct {
			TagRegex string `json:"tagRegex"`
			Matched  int    `json:"matched"`
			Total    int    `json:"total"`
		} `json:"scope"`
	}
	mustJSON(t, stdout, &out)
	if out.Scope.Matched != 4 || out.Scope.Total != 8 || out.Scope.TagRegex != "db_.*" {
		t.Fatalf("scope = %+v", out.Scope)
	}
	if len(out.Remove) != 1 || out.Remove[0].Tag != "db_1" {
		t.Fatalf("removed %+v, want only db_1 (the oldest of the family)", out.Remove)
	}
	// The registry must have been asked for exactly that one manifest and no
	// other: an out-of-scope family is never even mentioned.
	if got := probe.Deletes(); !slices.Equal(got, []string{digests["db_1"]}) {
		t.Fatalf("DELETE requests = %v, want only db_1 (%s)", got, digests["db_1"])
	}
	if out.Kept != 7 {
		t.Fatalf("kept = %d, want 7", out.Kept)
	}
}

func TestPruneGroupByKeepsTheNewestOfEveryFamily(t *testing.T) {
	probe, repo, digests := newTestRepo(t, map[string]int{"db": 4, "app": 4})

	stdout, _, err := runRoot(t, "repo", "prune", repo, "--group-by-regex", "([a-z]+)_.*",
		"--keep-last", "3", "--yes", "--json")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	var out struct {
		Remove []struct {
			Tag string `json:"tag"`
		} `json:"remove"`
		GroupBy struct {
			Groups    int `json:"groups"`
			Ungrouped int `json:"ungrouped"`
		} `json:"groupBy"`
		Groups []struct {
			Key  string `json:"key"`
			Tags int    `json:"tags"`
			Kept int    `json:"kept"`
		} `json:"groups"`
	}
	mustJSON(t, stdout, &out)
	if out.GroupBy.Groups != 2 || out.GroupBy.Ungrouped != 0 {
		t.Fatalf("groupBy = %+v", out.GroupBy)
	}
	// Every family kept its own three: the rule was not spent on one of them.
	for _, g := range out.Groups {
		if g.Tags != 4 || g.Kept != 3 {
			t.Fatalf("group %q = %d tags, %d kept", g.Key, g.Tags, g.Kept)
		}
	}
	removed := make([]string, 0, len(out.Remove))
	for _, r := range out.Remove {
		removed = append(removed, r.Tag)
	}
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"app_1", "db_1"}) {
		t.Fatalf("removed %v, want the oldest of each family", removed)
	}
	want := []string{digests["app_1"], digests["db_1"]}
	slices.Sort(want)
	if got := probe.Deletes(); !slices.Equal(got, want) {
		t.Fatalf("DELETE requests = %v, want %v", got, want)
	}
}

// A selector without a retention rule must be a no-op, on a real registry too.
func TestPruneTagRegexWithoutRuleDeletesNothing(t *testing.T) {
	probe, repo, _ := newTestRepo(t, map[string]int{"db": 3})

	stdout, _, err := runRoot(t, "repo", "prune", repo, "--tag-regex", "db_.*", "--yes")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !strings.Contains(stdout, "nessuna regola") {
		t.Fatalf("stdout = %q", stdout)
	}
	if got := probe.Deletes(); len(got) != 0 {
		t.Fatalf("a selector without a rule sent %v", got)
	}
}

// The pre-check must complete before the first DELETE: a refusal halfway through
// would leave the registry matching neither the plan nor the starting point.
func TestPruneRefusesSharedManifestWithoutDeletingAnything(t *testing.T) {
	probe := startTestRegistry(t)
	repo := probe.host + "/e2e/shared"
	now := time.Now().UTC()

	// db_1 and app_1 are the same manifest: identical dumps of two families.
	img := randomImage(t, now.AddDate(0, 0, -9))
	pushImage(t, repo+":db_1", img)
	pushImage(t, repo+":app_1", img)
	// A newer, distinct db backup, so the policy has something to keep.
	pushImage(t, repo+":db_2", randomImage(t, now.AddDate(0, 0, -1)))

	_, _, err := runRoot(t, "repo", "prune", repo, "--tag-regex", "db_.*", "--keep-last", "1", "--yes")
	if err == nil {
		t.Fatal("the prune deleted a manifest still referenced by a kept tag")
	}
	for _, want := range []string{"db_1", "app_1", "nessun tag è stato rimosso"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
	if got := probe.Deletes(); len(got) != 0 {
		t.Fatalf("the refusal still sent %v", got)
	}
}

// When every tag of a shared manifest is in the removal set, one DELETE takes
// them all: no --force, and no request repeated per tag.
func TestPruneDeletesSharedManifestWhenAllItsTagsGo(t *testing.T) {
	probe := startTestRegistry(t)
	repo := probe.host + "/e2e/allshared"
	now := time.Now().UTC()

	old := randomImage(t, now.AddDate(0, 0, -9))
	shared := pushImage(t, repo+":db_1", old)
	pushImage(t, repo+":db_2", old) // same manifest, both destined to go
	pushImage(t, repo+":db_9", randomImage(t, now))

	stdout, _, err := runRoot(t, "repo", "prune", repo, "--tag-regex", "db_.*",
		"--keep-last", "1", "--yes", "--json")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	var out struct {
		Remove []struct {
			Tag string `json:"tag"`
		} `json:"remove"`
	}
	mustJSON(t, stdout, &out)
	if len(out.Remove) != 2 {
		t.Fatalf("removed %+v, want both db_1 and db_2", out.Remove)
	}
	if got := probe.Deletes(); !slices.Equal(got, []string{shared}) {
		t.Fatalf("DELETE requests = %v, want one request for %s", got, shared)
	}
}

// GS-13.5: the read-only preview and the destructive command must select the
// same set. Anything less makes the preview a liability.
func TestRepoTagsPreviewMatchesWhatPruneWouldRemove(t *testing.T) {
	_, repo, _ := newTestRepo(t, map[string]int{"db": 4, "app": 3})

	stdout, _, err := runRoot(t, "repo", "tags", repo, "--tag-regex", "db_.*", "--json")
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	var preview []struct {
		Tag string `json:"tag"`
	}
	mustJSON(t, stdout, &preview)
	previewed := make([]string, 0, len(preview))
	for _, tag := range preview {
		previewed = append(previewed, tag.Tag)
	}
	slices.Sort(previewed)

	// keep-within 1s removes everything in scope, so the removal set is exactly
	// the selection — the two can be compared tag by tag.
	stdout, _, err = runRoot(t, "repo", "prune", repo, "--tag-regex", "db_.*",
		"--keep-within", "1s", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	var plan struct {
		Remove []struct {
			Tag string `json:"tag"`
		} `json:"remove"`
		Scope struct {
			Matched int `json:"matched"`
		} `json:"scope"`
	}
	mustJSON(t, stdout, &plan)
	doomed := make([]string, 0, len(plan.Remove))
	for _, tag := range plan.Remove {
		doomed = append(doomed, tag.Tag)
	}
	slices.Sort(doomed)

	if !slices.Equal(previewed, doomed) {
		t.Fatalf("`repo tags` previewed %v but `repo prune` would act on %v", previewed, doomed)
	}
	if plan.Scope.Matched != len(previewed) {
		t.Fatalf("prune matched %d tags, the preview listed %d", plan.Scope.Matched, len(previewed))
	}
}

func TestRepoTagsWithoutSelectorsListsEverything(t *testing.T) {
	_, repo, _ := newTestRepo(t, map[string]int{"db": 2, "app": 2})
	stdout, _, err := runRoot(t, "repo", "tags", repo)
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	// No selector, no summary line: the listing is what it always was.
	if strings.Contains(stdout, "selezionati") {
		t.Fatalf("a summary line appeared without a selector: %q", stdout)
	}
	for _, tag := range []string{"db_1", "db_2", "app_1", "app_2"} {
		if !strings.Contains(stdout, tag) {
			t.Fatalf("stdout %q missing %q", stdout, tag)
		}
	}
}

func TestRepoTagsGroupsThePreview(t *testing.T) {
	_, repo, _ := newTestRepo(t, map[string]int{"db": 2, "app": 2})
	stdout, _, err := runRoot(t, "repo", "tags", repo, "--group-by-regex", "([a-z]+)_.*")
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	for _, want := range []string{"selezionati 4 tag su 4", `gruppo "app" — 2 tag`, `gruppo "db" — 2 tag`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout %q missing %q", stdout, want)
		}
	}
}

// --- helpers -----------------------------------------------------------------

func mustCompile(t *testing.T, expr string) *regexp.Regexp {
	t.Helper()
	re, err := registry.CompileTagPattern(expr)
	if err != nil {
		t.Fatalf("CompileTagPattern(%q) = %v", expr, err)
	}
	return re
}

// registryProbe is a fake registry that records the DELETE requests it receives.
// ggcr's in-memory registry keys a manifest by tag and by digest independently,
// so deleting the digest leaves the tag entry behind and the tag list cannot
// tell whether a deletion happened. What the prune sends can, and it is what the
// safety rules are about: how many manifests it deletes, which ones, and whether
// it sends anything at all before the plan is validated. The tag actually
// disappearing is asserted against a real registry:2 in test/e2e/phase_13.sh.
type registryProbe struct {
	host    string
	mu      sync.Mutex
	deletes []string
}

func (p *registryProbe) record(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletes = append(p.deletes, path)
}

// Deletes lists the manifest references the prune asked the registry to remove.
func (p *registryProbe) Deletes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.deletes))
	for _, path := range p.deletes {
		if i := strings.LastIndex(path, "/"); i >= 0 {
			out = append(out, path[i+1:])
		}
	}
	slices.Sort(out)
	return out
}

func startTestRegistry(t *testing.T) *registryProbe {
	t.Helper()
	// Keep the prune away from the real credential store.
	t.Setenv("BACKIMAGE_AUTH_FILE", filepath.Join(t.TempDir(), "auth.json"))
	probe := &registryProbe{}
	inner := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			probe.record(r.URL.Path)
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	probe.host = u.Host
	return probe
}

// newTestRepo publishes families of dated tags: family_N, where a higher N is
// newer, so "keep the 3 newest of db" reads directly off the tag names. It
// returns the digest of every tag, so a test can name the manifest it expects
// the prune to delete.
func newTestRepo(t *testing.T, families map[string]int) (*registryProbe, string, map[string]string) {
	t.Helper()
	probe := startTestRegistry(t)
	repo := probe.host + "/e2e/dumps"
	now := time.Now().UTC().Truncate(time.Second)
	keys := make([]string, 0, len(families))
	for family := range families {
		keys = append(keys, family)
	}
	slices.Sort(keys)
	digests := map[string]string{}
	for _, family := range keys {
		for i := 1; i <= families[family]; i++ {
			age := time.Duration(families[family]-i+1) * 24 * time.Hour
			tag := family + "_" + strconv.Itoa(i)
			digests[tag] = pushImage(t, repo+":"+tag, randomImage(t, now.Add(-age)))
		}
	}
	return probe, repo, digests
}

func randomImage(t *testing.T, created time.Time) v1.Image {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Created = v1.Time{Time: created}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func pushImage(t *testing.T, ref string, img v1.Image) string {
	t.Helper()
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest.String()
}

func mustJSON(t *testing.T, stdout string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), into); err != nil {
		t.Fatalf("json %q: %v", stdout, err)
	}
}

func TestRepoTagsSummarySpeaksOfASingleGroup(t *testing.T) {
	_, repo, _ := newTestRepo(t, map[string]int{"db": 2})
	stdout, _, err := runRoot(t, "repo", "tags", repo, "--group-by-regex", "([a-z]+)_.*")
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	if !strings.Contains(stdout, "in 1 gruppo (") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// A pattern that reaches nothing must say so, and say why, on the read-only
// command too: this is where a typo is supposed to be caught.
func TestRepoTagsZeroMatchExplainsTheAnchoring(t *testing.T) {
	_, repo, _ := newTestRepo(t, map[string]int{"db": 2})
	stdout, _, err := runRoot(t, "repo", "tags", repo, "--tag-regex", "db_")
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	for _, want := range []string{"selezionati 0 tag su 2", "tag intero", "'db_.*'"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout %q missing %q", stdout, want)
		}
	}
}

// Concordanza: un solo tag fuori dai gruppi non è "1 non raggruppati".
func TestUngroupedCountAgreesInNumber(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := []registry.TagInfo{
		{Tag: "db_1", Created: now.AddDate(0, 0, -1)},
		{Tag: "db_2", Created: now.AddDate(0, 0, -2)},
		{Tag: "latest", Created: now.AddDate(0, 0, -9)},
		{Tag: "stable", Created: now.AddDate(0, 0, -8)},
	}
	groupBy := mustCompile(t, `([a-z]+)_.*`)

	one := registry.Policy{KeepLast: 1, GroupBy: groupBy}
	text := prunePlanText(true, one, one.PlanFor(tags[:3], now))
	if !strings.Contains(text, "1 tag non raggruppato e quindi non toccato") {
		t.Fatalf("singular form missing: %q", text)
	}
	text = prunePlanText(true, one, one.PlanFor(tags, now))
	if !strings.Contains(text, "2 tag non raggruppati e quindi non toccati") {
		t.Fatalf("plural form missing: %q", text)
	}

	if got := tagSelectionSummary(nil, groupBy, registry.Policy{GroupBy: groupBy}.Select(tags[:3])); !strings.Contains(got, "1 non raggruppato") {
		t.Fatalf("summary singular form missing: %q", got)
	}
	if got := tagSelectionSummary(nil, groupBy, registry.Policy{GroupBy: groupBy}.Select(tags)); !strings.Contains(got, "2 non raggruppati") {
		t.Fatalf("summary plural form missing: %q", got)
	}
}

// Without a rule the prune deletes nothing, and the JSON must not claim the
// pattern reached the whole repository.
func TestPruneJSONScopeIsHonestWithoutARule(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tags := []registry.TagInfo{
		{Tag: "db_1", Created: now.AddDate(0, 0, -1)},
		{Tag: "db_2", Created: now.AddDate(0, 0, -2)},
		{Tag: "app_1", Created: now.AddDate(0, 0, -1)},
	}
	p := registry.Policy{Scope: mustCompile(t, `db_.*`)}
	out := pruneJSON(true, p, p.PlanFor(tags, now))
	scope := out["scope"].(map[string]any)
	if scope["matched"] != 2 || scope["total"] != 3 {
		t.Fatalf("scope = %#v, want 2 of 3", scope)
	}
	if out["kept"] != 3 {
		t.Fatalf("kept = %v, want every tag", out["kept"])
	}
	if remove, ok := out["remove"].([]registry.TagInfo); ok && len(remove) != 0 {
		t.Fatalf("remove = %v", remove)
	}
}

// A sequence of HTTP deletions cannot be atomic. When one fails the command must
// say how much of the plan was already applied, or the operator has no way of
// knowing what state the repository is in.
func TestPruneReportsHowFarItGotWhenADeleteFails(t *testing.T) {
	t.Setenv("BACKIMAGE_AUTH_FILE", filepath.Join(t.TempDir(), "auth.json"))
	// The plan removes newest first, so db_2 is the second deletion. Refusing it
	// by digest rather than by call count keeps the test immune to the retries
	// ggcr performs on 5xx; a 403 is not retried.
	var blocked atomic.Value
	blocked.Store("")
	inner := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deny, _ := blocked.Load().(string); r.Method == http.MethodDelete && deny != "" && strings.Contains(r.URL.Path, deny) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	repo := u.Host + "/e2e/partial"
	now := time.Now().UTC()
	digests := map[string]string{}
	for i := 1; i <= 3; i++ {
		tag := "db_" + strconv.Itoa(i)
		digests[tag] = pushImage(t, repo+":"+tag, randomImage(t, now.AddDate(0, 0, -10+i)))
	}
	blocked.Store(digests["db_2"])

	_, _, err = runRoot(t, "repo", "prune", repo, "--tag-regex", "db_.*", "--keep-within", "1s", "--yes")
	if err == nil {
		t.Fatal("a failing DELETE was reported as success")
	}
	for _, want := range []string{"eliminazione interrotta", digests["db_2"], "1 manifest su 3", "rieseguire lo stesso prune"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}
