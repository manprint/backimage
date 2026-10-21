package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layoutBackup writes a small unencrypted backup to an OCI layout and returns
// the layout path and the reference to restore it by.
func layoutBackup(t *testing.T) (layout, ref string) {
	t.Helper()
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "dir", "file.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout = filepath.Join(t.TempDir(), "layout")
	if _, stderr, err := runRoot(t, "backup", tree, "--repo", "example.test/team/flags", "--tag", "t1",
		"--output", "oci-layout", "--output-path", layout, "--no-encrypt", "--allow-degraded",
		"--platform", "linux/amd64"); err != nil {
		t.Fatalf("backup: %v\n%s", err, stderr)
	}
	return layout, "example.test/team/flags:t1"
}

// -x and -o name two outcomes. With both, -o was dropped without a word: the
// files were extracted and the tar the user asked for never appeared. The
// extraction-only flags were dropped the same way on a tar restore, so a
// "stripped" tar kept every leading component.
func TestRestoreRefusesFlagsItWouldIgnore(t *testing.T) {
	layout, ref := layoutBackup(t)
	for _, tc := range []struct {
		name string
		args []string
		flag string
	}{
		{"extract and output", []string{"-x", "-o", "out.tar"}, "-o"},
		{"strip on a tar", []string{"-o", "out.tar", "--strip-components", "1"}, "--strip-components"},
		{"strict on a tar", []string{"-o", "out.tar", "--strict"}, "--strict"},
		{"owner on a tar", []string{"-o", "out.tar", "--no-preserve-owner"}, "--no-preserve-owner"},
		{"xattrs on a tar", []string{"-o", "out.tar", "--no-preserve-xattrs"}, "--no-preserve-xattrs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			args := append([]string{"restore", ref, "--oci-layout", layout, "--destination", dst}, tc.args...)
			_, _, err := runRoot(t, args...)
			if ExitCodeFor(err) != int(KindUsage) || !strings.Contains(err.Error(), tc.flag) {
				t.Fatalf("restore %v = exit %d (%v), want a usage error naming %s", tc.args, ExitCodeFor(err), err, tc.flag)
			}
			if entries, _ := os.ReadDir(dst); len(entries) != 0 {
				t.Fatalf("a refused restore wrote %d entries into the destination", len(entries))
			}
		})
	}
}

// The CDC parameters only mean something to a --dedup backup; without it they
// were parsed, validated and thrown away.
func TestBackupRefusesDedupParametersWithoutDedup(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, flag := range [][]string{
		{"--dedup-chunk-min", "64KiB"},
		{"--dedup-chunk-avg", "256KiB"},
		{"--dedup-chunk-max", "1MiB"},
		{"--dedup-polynomial", "0x3DA3358B4DC173"},
	} {
		args := append([]string{"backup", tree, "--repo", "example.test/team/p", "--tag", "t1", "--output", "oci-layout",
			"--output-path", filepath.Join(t.TempDir(), "layout"), "--no-encrypt", "--allow-degraded",
			"--platform", "linux/amd64"}, flag...)
		_, _, err := runRoot(t, args...)
		if ExitCodeFor(err) != int(KindUsage) || !strings.Contains(err.Error(), flag[0]) {
			t.Fatalf("backup %v without --dedup = exit %d (%v), want a usage error naming %s", flag, ExitCodeFor(err), err, flag[0])
		}
	}
}

// The precedence is flag > env. An env var filling the alternative of a flag
// given on the command line turned that override into a conflict error: a
// compose file setting BACKIMAGE_AUTH_TOKEN could not be overridden with
// --auth-token-file.
func TestEnvDoesNotFillTheAlternativeOfAnExplicitFlag(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("file on the command line", func(t *testing.T) {
		t.Setenv("BACKIMAGE_AUTH_TOKEN", "from-env")
		cmd := newListenRemoteCommand()
		if err := cmd.ParseFlags([]string{"--auth-token-file", tokenFile}); err != nil {
			t.Fatal(err)
		}
		if err := applyEnvDefaults(cmd); err != nil {
			t.Fatal(err)
		}
		if got, err := sharedAuthToken(cmd); err != nil || got != "from-file" {
			t.Fatalf("token = %q, %v; want the command line's", got, err)
		}
	})
	t.Run("token on the command line", func(t *testing.T) {
		t.Setenv("BACKIMAGE_AUTH_TOKEN_FILE", tokenFile)
		cmd := newListenRemoteCommand()
		if err := cmd.ParseFlags([]string{"--auth-token", "from-flag"}); err != nil {
			t.Fatal(err)
		}
		if err := applyEnvDefaults(cmd); err != nil {
			t.Fatal(err)
		}
		if got, err := sharedAuthToken(cmd); err != nil || got != "from-flag" {
			t.Fatalf("token = %q, %v; want the command line's", got, err)
		}
	})
	// Both in the environment is a configuration that contradicts itself:
	// that conflict stays an error.
	t.Run("both in the environment", func(t *testing.T) {
		t.Setenv("BACKIMAGE_AUTH_TOKEN", "from-env")
		t.Setenv("BACKIMAGE_AUTH_TOKEN_FILE", tokenFile)
		cmd := newListenRemoteCommand()
		if err := applyEnvDefaults(cmd); err != nil {
			t.Fatal(err)
		}
		if _, err := sharedAuthToken(cmd); err == nil {
			t.Fatal("two token sources in the environment were accepted")
		}
	})
}

// --log-format json was validated and then ignored: the diagnostics stayed
// text, and a collector configured for JSON dropped every line.
func TestListenRemoteLogFormatJSON(t *testing.T) {
	_, stderr, err := runRoot(t, "listen-remote", "--tls-self-signed", "--work-dir", t.TempDir(),
		"--insecure-no-auth", "--bind-address", "127.0.0.1:0", "--log-format", "json", "--max-bytes", "bogus")
	if ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("listen-remote = exit %d (%v), want the --max-bytes usage error", ExitCodeFor(err), err)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	levels := make([]string, 0, len(lines))
	pin := false
	for _, line := range lines {
		var record struct {
			Time  string `json:"time"`
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.Level == "" || record.Msg == "" || record.Time == "" {
			t.Fatalf("diagnostic line is not a JSON record: %q (%v)", line, err)
		}
		levels = append(levels, record.Level)
		pin = pin || strings.Contains(record.Msg, "TLS fingerprint SHA256:")
	}
	if !pin || !strings.Contains(strings.Join(levels, ","), "warning") {
		t.Fatalf("records = %v, pin printed = %v; want the pin and the no-auth warning", levels, pin)
	}
}

// The printer already prefixes warnings: the no-auth notice carried its own
// "WARNING:" on top, and read "warning: WARNING: ..." in text and
// "WARNING: ..." inside a record whose level already said so.
func TestListenRemoteNoAuthWarningIsPrefixedOnce(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			_, stderr, err := runRoot(t, "listen-remote", "--tls-self-signed", "--work-dir", t.TempDir(),
				"--insecure-no-auth", "--bind-address", "127.0.0.1:0", "--log-format", format, "--max-bytes", "bogus")
			if ExitCodeFor(err) != int(KindUsage) {
				t.Fatalf("listen-remote = exit %d (%v), want the --max-bytes usage error", ExitCodeFor(err), err)
			}
			if !strings.Contains(stderr, "remote server authentication is disabled") {
				t.Fatalf("no-auth warning missing:\n%s", stderr)
			}
			if strings.Contains(strings.ToLower(stderr), "warning: warning") || strings.Contains(stderr, "WARNING") {
				t.Fatalf("no-auth warning prefixed twice:\n%s", stderr)
			}
		})
	}
}

// The help said a restore without -x or -o wrote the tar to stdout; it writes
// <repo>_<tag>.tar into --destination, so `restore IMG | tar -x` as
// documented piped nothing and left a backup-sized file behind.
func TestRestoreWithoutAnOutcomeWritesTheDefaultTar(t *testing.T) {
	layout, ref := layoutBackup(t)
	dst := t.TempDir()
	stdout, stderr, err := runRoot(t, "restore", ref, "--oci-layout", layout, "--destination", dst, "--quiet")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, stderr)
	}
	if stdout != "" && !strings.Contains(stdout, "flags_t1.tar") {
		t.Fatalf("stdout carries data instead of the result line: %d bytes", len(stdout))
	}
	if _, err := os.Stat(filepath.Join(dst, "flags_t1.tar")); err != nil {
		t.Fatalf("default tar not written in --destination: %v", err)
	}
	long := newRestoreCommand().Long
	if strings.Contains(long, "Without either, a tar\nis written to stdout") || !strings.Contains(long, "<repo>_<tag>.tar") {
		t.Fatalf("restore help misdescribes the default outcome:\n%s", long)
	}
}

// The receiver publishes a remote backup and always reads it back the quick
// way: the protocol carries no level. --verify-after-push full or off with
// --remote was accepted and dropped, so the full read-back the user asked for
// never ran. The refusal comes before any connection: the address below
// would otherwise fail as a network error.
func TestBackupRefusesVerifyAfterPushWithRemote(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "file.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, level := range []string{"full", "off"} {
		t.Run(level, func(t *testing.T) {
			_, _, err := runRoot(t, "backup", tree, "--repo", "127.0.0.1:1/team/remote", "--tag", "t1",
				"--remote", "127.0.0.1:1", "--auth-token-file", filepath.Join(tree, "file.txt"),
				"--no-encrypt", "--verify-after-push", level)
			if ExitCodeFor(err) != int(KindUsage) || !strings.Contains(err.Error(), "--verify-after-push") {
				t.Fatalf("--verify-after-push %s with --remote = exit %d (%v), want a usage error naming the flag", level, ExitCodeFor(err), err)
			}
		})
	}
}
