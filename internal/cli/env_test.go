package cli

import (
	"strings"
	"testing"
)

func TestEnvVarFor(t *testing.T) {
	for flag, want := range map[string]string{
		"bind-address":    "BACKIMAGE_BIND_ADDRESS",
		"udp":             "BACKIMAGE_UDP",
		"tls-self-signed": "BACKIMAGE_TLS_SELF_SIGNED",
	} {
		if got := EnvVarFor(flag); got != want {
			t.Fatalf("EnvVarFor(%q) = %q, want %q", flag, got, want)
		}
	}
}

func TestApplyEnvDefaults(t *testing.T) {
	t.Setenv("BACKIMAGE_BIND_ADDRESS", "0.0.0.0:9000")
	t.Setenv("BACKIMAGE_UDP", "true")
	t.Setenv("BACKIMAGE_MAX_SESSIONS", "3")
	t.Setenv("BACKIMAGE_ALLOW_REPO", "ghcr.io/team/a,ghcr.io/team/b")
	t.Setenv("BACKIMAGE_WORK_DIR", "  /data  ")
	t.Setenv("BACKIMAGE_METRICS_ADDRESS", "") // empty means unset
	t.Setenv("BACKIMAGE_LOG_FORMAT", "json")

	cmd := newListenRemoteCommand()
	// An explicit flag must win over the environment.
	if err := cmd.ParseFlags([]string{"--log-format", "text"}); err != nil {
		t.Fatal(err)
	}
	if err := applyEnvDefaults(cmd); err != nil {
		t.Fatal(err)
	}
	if got := getFlagString(cmd, "bind-address"); got != "0.0.0.0:9000" {
		t.Fatalf("bind-address = %q", got)
	}
	if !getFlagBool(cmd, "udp") {
		t.Fatal("udp not set from the environment")
	}
	if got := getFlagInt(cmd, "max-sessions"); got != 3 {
		t.Fatalf("max-sessions = %d", got)
	}
	if got := getFlagStrings(cmd, "allow-repo"); len(got) != 2 || got[0] != "ghcr.io/team/a" {
		t.Fatalf("allow-repo = %v", got)
	}
	if got := getFlagString(cmd, "work-dir"); got != "/data" {
		t.Fatalf("work-dir = %q", got)
	}
	if got := getFlagString(cmd, "metrics-address"); got != "" {
		t.Fatalf("metrics-address = %q, want empty", got)
	}
	if got := getFlagString(cmd, "log-format"); got != "text" {
		t.Fatalf("log-format = %q, want the explicit flag to win", got)
	}
}

func TestApplyEnvDefaultsRejectsBadValue(t *testing.T) {
	t.Setenv("BACKIMAGE_MAX_SESSIONS", "many")
	cmd := newListenRemoteCommand()
	err := applyEnvDefaults(cmd)
	if err == nil {
		t.Fatal("applyEnvDefaults accepted a non-numeric session count")
	}
	if got := err.Error(); !strings.Contains(got, "BACKIMAGE_MAX_SESSIONS") {
		t.Fatalf("error = %q, want the variable name", got)
	}
}

func TestApplyEnvDefaultsIncludesInheritedFlags(t *testing.T) {
	t.Setenv("BACKIMAGE_JSON", "true")
	t.Setenv("BACKIMAGE_QUIET", "true")
	t.Setenv("BACKIMAGE_VERBOSE", "2")
	t.Setenv("BACKIMAGE_NO_COLOR", "true")

	root := NewRootCommand()
	listen, _, err := root.Find([]string{"listen-remote"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an explicit CLI value: it must beat BACKIMAGE_QUIET=true.
	if err := root.ParseFlags([]string{"--quiet=false"}); err != nil {
		t.Fatal(err)
	}
	if err := applyEnvDefaults(listen); err != nil {
		t.Fatal(err)
	}

	json, _ := root.PersistentFlags().GetBool("json")
	quiet, _ := root.PersistentFlags().GetBool("quiet")
	verbose, _ := root.PersistentFlags().GetCount("verbose")
	noColor, _ := root.PersistentFlags().GetBool("no-color")
	if !json || quiet || verbose != 2 || !noColor {
		t.Fatalf("inherited flags: json=%v quiet=%v verbose=%d no-color=%v", json, quiet, verbose, noColor)
	}
}
