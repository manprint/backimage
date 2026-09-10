package embedded

import (
	"bytes"
	debugbuild "debug/buildinfo"
	"errors"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/manprint/backimage/internal/buildinfo"
)

// TestEmbeddedAssetsAreCoevalWithTheCode fails when the self-extract binaries
// compiled into this build were produced from a different revision than the
// code around them.
//
// The failure it prevents is quiet: `make build` embeds whatever sits in
// internal/embedded, so with a stale asset a fix to pkg/archive can look like
// it works because the local e2e runs an extractor that does not contain it,
// or look broken for the same reason. Comparing file dates would not catch it
// — a rebuild of unrelated code refreshes nothing — so the comparison is on
// what the asset itself declares.
//
// The stamp is read out of the binary, never executed: the arm64 asset is not
// runnable on an amd64 host, and half a check is worse than none. Go records
// the linker flags of a build in its build info, which survives `-s -w`, so
// the -X values injected by LDFLAGS_EMBED can be read back from the bytes.
func TestEmbeddedAssetsAreCoevalWithTheCode(t *testing.T) {
	wantVersion, wantCommit := expectedStamp(t)

	for _, arch := range Architectures() {
		data, err := SelfExtract(arch)
		if err != nil {
			if errors.Is(err, ErrNotEmbedded) {
				t.Fatalf("%s asset is still the committed placeholder: run `make selfextract` "+
					"(or `make build`, which now depends on it) before the test suite", arch)
			}
			t.Fatalf("SelfExtract(%s): %v", arch, err)
		}

		info, err := debugbuild.Read(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("reading build info of the %s asset: %v", arch, err)
		}
		ldflags := buildSetting(info.Settings, "-ldflags")
		if ldflags == "" {
			t.Fatalf("the %s asset carries no -ldflags: it was not built by `make selfextract`", arch)
		}

		gotVersion := ldflagValue(ldflags, "Version")
		gotCommit := ldflagValue(ldflags, "Commit")
		if gotVersion == "" || gotCommit == "" {
			t.Fatalf("the %s asset has no buildinfo stamp (-ldflags %q): run `make selfextract`", arch, ldflags)
		}
		if gotVersion != wantVersion || gotCommit != wantCommit {
			t.Fatalf("the %s asset is not coeval with this code: it declares %s/%s, the tree is %s/%s; run `make selfextract`",
				arch, gotVersion, gotCommit, wantVersion, wantCommit)
		}
		if date := ldflagValue(ldflags, "Date"); date != "" {
			t.Fatalf("the %s asset carries a build date (%s): it makes the tool layer digest of every "+
				"produced image change at identical code; drop Date from LDFLAGS_EMBED", arch, date)
		}
	}
}

// expectedStamp returns the Version and Commit the assets must declare.
//
// When this test binary was itself linked with the stamps — anything built
// through LDFLAGS or LDFLAGS_EMBED — those are the authority, because they are
// literally the values the same make run injected. Plain `go test` links no
// stamps, so the values are derived from git exactly as the Makefile derives
// them. Never guessed, and never skipped: without git and without a stamp
// there is no way to tell coeval from stale, and saying so is the honest
// outcome.
func expectedStamp(t *testing.T) (version, commit string) {
	t.Helper()
	if buildinfo.Commit != "none" || buildinfo.Version != "dev" {
		return buildinfo.Version, buildinfo.Commit
	}
	return gitOutput(t, "describe", "--tags", "--always", "--dirty"),
		gitOutput(t, "rev-parse", "--short", "HEAD")
}

func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatalf("git %s: %v (this test needs either a stamped binary or a git checkout "+
			"to know which revision the assets should declare)", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func buildSetting(settings []debug.BuildSetting, key string) string {
	for _, s := range settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// ldflagValue extracts the value of -X <anything>/internal/buildinfo.<field>=…
// from a recorded -ldflags string.
func ldflagValue(ldflags, field string) string {
	marker := "internal/buildinfo." + field + "="
	i := strings.Index(ldflags, marker)
	if i < 0 {
		return ""
	}
	rest := ldflags[i+len(marker):]
	if j := strings.IndexAny(rest, " \t\"'"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
