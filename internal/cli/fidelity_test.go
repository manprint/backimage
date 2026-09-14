package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/manprint/backimage/pkg/archive"
)

// TestStrictFidelityError covers the promise --strict used to break: a restore
// that completed but was not 1:1 exited 0, so nothing downstream could tell a
// faithful copy from a degraded one.
//
// Two metadata losses never abort mid-restore — an extended attribute the
// destination refuses outright, and trusted.* without CAP_SYS_ADMIN — because
// stopping would leave a half-written tree behind a loss nothing could have
// prevented there. They are tolerated, counted and warned about; what changes
// is that with --strict they now decide the exit code.
func TestStrictFidelityError(t *testing.T) {
	cases := []struct {
		name    string
		strict  bool
		stats   archive.Stats
		wantErr bool
	}{
		{
			name:   "faithful restore in strict mode succeeds",
			strict: true,
			stats:  archive.Stats{Files: 10},
		},
		{
			name:   "degraded restore without strict succeeds",
			strict: false,
			stats:  archive.Stats{Files: 10, Degraded: map[string]int64{"xattr.system": 3}},
		},
		{
			name:    "tolerated xattr loss in strict mode fails",
			strict:  true,
			stats:   archive.Stats{Files: 10, Degraded: map[string]int64{"xattr.system": 3}},
			wantErr: true,
		},
		{
			name:    "trusted xattr loss in strict mode fails",
			strict:  true,
			stats:   archive.Stats{Files: 1, Degraded: map[string]int64{"xattr.trusted": 1}},
			wantErr: true,
		},
		{
			name:    "an entry that was never created fails in strict mode",
			strict:  true,
			stats:   archive.Stats{Files: 4, Skipped: 1},
			wantErr: true,
		},
		{
			name:   "an empty degraded map is not a degradation",
			strict: true,
			stats:  archive.Stats{Files: 4, Degraded: map[string]int64{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := strictFidelityError(tc.strict, tc.stats)
			if (err != nil) != tc.wantErr {
				t.Fatalf("strictFidelityError() = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if got := ExitCodeFor(err); got != int(KindFidelity) {
				t.Errorf("exit code = %d, want %d", got, int(KindFidelity))
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error %v is not a *Error", err)
			}
			// The message has to be actionable on its own: a script logs it,
			// a person reads it without the surrounding output.
			if !strings.Contains(ce.Msg, "NON 1:1") {
				t.Errorf("message %q does not state the verdict", ce.Msg)
			}
			if ce.Hint == "" {
				t.Error("a fidelity failure must carry a remediation")
			}
		})
	}
}

// TestStrictFidelityErrorIsNotAnIntegrityFailure keeps the two classes apart.
// Exit 5 means the backup is not what it claims to be — a script may well page
// somebody. Exit 8 means the backup was perfect and the destination could not
// hold all of it, which is a different problem with a different remedy.
func TestStrictFidelityErrorIsNotAnIntegrityFailure(t *testing.T) {
	err := strictFidelityError(true, archive.Stats{Degraded: map[string]int64{"owner": 2}})
	if err == nil {
		t.Fatal("want a fidelity error")
	}
	if got := ExitCodeFor(err); got == int(KindIntegrity) {
		t.Fatalf("a metadata loss must not be reported as an integrity failure (exit %d)", got)
	}
	if errors.Is(err, ErrIntegrity) {
		t.Error("a metadata loss must not unwrap to ErrIntegrity")
	}
}

// TestDegradedJSONShapeIsStable: the --json document of a faithful restore and
// of a degraded one must have the same keys, so a consumer can read
// result.degraded without first checking whether the field exists.
func TestDegradedJSONShapeIsStable(t *testing.T) {
	if got := degradedCounts(nil); got == nil || len(got) != 0 {
		t.Errorf("degradedCounts(nil) = %v, want an empty map", got)
	}
	if got := degradedExamples(nil); got == nil || len(got) != 0 {
		t.Errorf("degradedExamples(nil) = %v, want an empty map", got)
	}
	counts := map[string]int64{"xattr.system": 2}
	if got := degradedCounts(counts); got["xattr.system"] != 2 {
		t.Errorf("degradedCounts lost its content: %v", got)
	}
}

// TestRecoveryInstructionsAreMaximumFidelity checks the block printed after
// every backup. It is the only restore documentation most users will ever
// read, and it is pasted verbatim, so every option in it has to exist and be
// the one that actually yields a faithful copy.
func TestRecoveryInstructionsAreMaximumFidelity(t *testing.T) {
	out := recoveryInstructions("ghcr.io/me/dumps:daily", true, true)
	commands := strings.SplitN(out, "Verifiche del ripristino:", 2)[0]

	for _, want := range []string{
		// Maximum fidelity on both paths means privileges plus --strict:
		// without it a restore that dropped owner, mode or extended
		// attributes still exits 0, and the heading would be a lie.
		"sudo backimage restore ghcr.io/me/dumps:daily --extract --destination ./restore --strict",
		"docker run --rm --privileged",
		"ghcr.io/me/dumps:daily extract --out /restore --strict",
		"-v \"$PWD/restore:/restore\"",
	} {
		if !strings.Contains(commands, want) {
			t.Errorf("recovery commands missing %q:\n%s", want, commands)
		}
	}

	// The Docker socket and BACKIMAGE_IMAGE_REF existed only to serve
	// --remove-local-image, which the self-extractor no longer has, and
	// nothing inside it ever read that variable. What remained was a
	// paste-ready command mounting the Docker socket — host root — into a
	// --privileged container for no purpose at all.
	for _, unwanted := range []string{"/var/run/docker.sock", "BACKIMAGE_IMAGE_REF"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("recovery instructions still mention %q:\n%s", unwanted, out)
		}
	}

	// Every option named in the tips has to exist on the command it is
	// attributed to. --remove-local-image is the one that did not: the tip
	// told the user to add it to the docker run command, where it is refused.
	if strings.Contains(out, "--remove-local-image") && !strings.Contains(out, "al comando backimage") { //nolint:misspell // Messaggio CLI italiano.
		t.Error("the --remove-local-image tip must say it is a host-only flag")
	}

	// The closing verdict is what the user is told to look for, so its name
	// here must match what the extractor actually prints.
	sample := archive.Stats{Files: 1}.ClosingVerdict(true)
	marker := "ESITO: estrazione 1:1"
	if !strings.HasPrefix(sample, marker) {
		t.Fatalf("ClosingVerdict no longer starts with %q: %q", marker, sample)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("recovery instructions do not name the verdict line %q:\n%s", marker, out)
	}
}

// TestRecoveryInstructionsWithoutRunnable: a backup built with
// --runnable=false has no self-extractor, and the block must say so instead of
// printing a docker command that cannot work.
func TestRecoveryInstructionsWithoutRunnable(t *testing.T) {
	out := recoveryInstructions("ghcr.io/me/dumps:daily", true, false)
	if strings.Contains(out, "docker run --rm") {
		t.Error("a non-runnable backup must not print a docker run command")
	}
	if !strings.Contains(out, "non disponibile") {
		t.Error("a non-runnable backup must say the container form is unavailable")
	}
	if !strings.Contains(out, "--strict") {
		t.Error("the CLI command must still be the maximum-fidelity one")
	}
}

// TestRecoveryInstructionsUnencrypted: no passphrase plumbing when there is no
// passphrase to pass.
func TestRecoveryInstructionsUnencrypted(t *testing.T) {
	out := recoveryInstructions("ghcr.io/me/dumps:daily", false, true)
	for _, unwanted := range []string{"BACKUP_PASSPHRASE", "--passphrase-stdin"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("an unencrypted backup must not mention %q:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "--strict") {
		t.Error("the commands must still be the maximum-fidelity ones")
	}
}
