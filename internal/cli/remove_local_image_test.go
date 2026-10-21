package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	restorepkg "github.com/manprint/backimage/pkg/restore"
)

func stubImageRemoval(t *testing.T) *string {
	t.Helper()
	old := removeDockerImage
	t.Cleanup(func() { removeDockerImage = old })
	removed := new(string)
	removeDockerImage = func(_ context.Context, ref string) error {
		*removed = ref
		return nil
	}
	return removed
}

// The image deleted is the daemon's. A restore read from a registry or a
// layout never pulled it, and used to delete whatever local image happened
// to carry the same reference.
func TestRemoveLocalImageRequiresTheDaemonSource(t *testing.T) {
	removed := stubImageRemoval(t)
	old := openSourceForCLI
	t.Cleanup(func() { openSourceForCLI = old })
	opened := false
	openSourceForCLI = func(context.Context, string, sourceFlags) (restorepkg.Source, error) {
		opened = true
		s, _ := newMockImageSource(t, false)
		return s, nil
	}
	for _, extra := range [][]string{nil, {"--oci-layout", t.TempDir()}} {
		args := append([]string{"restore", "example.test/repo:tag", "--extract", "-C", t.TempDir(),
			"--no-preserve-owner", "--remove-local-image"}, extra...)
		_, _, err := runRoot(t, args...)
		if ExitCodeFor(err) != int(KindUsage) {
			t.Fatalf("%v: exit = %d (%v), want %d", extra, ExitCodeFor(err), err, KindUsage)
		}
		if opened || *removed != "" {
			t.Fatalf("%v: refused too late: source opened %v, image removed %q", extra, opened, *removed)
		}
	}
}

// "Never discard the image a restore just failed to reproduce faithfully"
// held only with --strict: a restore that lost owners or attributes without
// --strict deleted the only copy of what it had not restored.
func TestRemoveLocalImageKeepsTheImageAfterADegradedRestore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root restores the archived owner: nothing degrades")
	}
	if runtime.GOOS == "windows" {
		// Owners are not restored there by design (docs/FIDELITY.md) and
		// count as no difference, so this fixture cannot degrade.
		t.Skip("no owner to lose on windows")
	}
	removed := stubImageRemoval(t)
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	dst := filepath.Join(t.TempDir(), "extract")
	out, stderr, err := runRoot(t, "--json", "restore", "example.test/repo:tag", "--local-repo", "--extract", "-C", dst,
		"--remove-local-image")
	if err != nil {
		t.Fatalf("degraded restore = %v\n%s", err, stderr)
	}
	var result struct {
		Degraded         map[string]int64 `json:"degraded"`
		RemoveLocalImage bool             `json:"remove_local_image"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	var degraded int64
	for _, n := range result.Degraded {
		degraded += n
	}
	if degraded == 0 {
		t.Fatalf("the fixture restore did not degrade (owner kept as non-root?): %q", out)
	}
	if *removed != "" || result.RemoveLocalImage {
		t.Fatalf("image %q removed after a restore with %d differences", *removed, degraded)
	}
	if !strings.Contains(stderr, "immagine locale conservata") {
		t.Fatalf("the kept image is not explained: %q", stderr)
	}
}
