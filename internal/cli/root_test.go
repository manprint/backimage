package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/manprint/backimage/internal/buildinfo"
)

func runRoot(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCommand()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = classify(root.ExecuteContext(context.Background()))
	return out.String(), errBuf.String(), err
}

func TestVersionPlain(t *testing.T) {
	out, _, err := runRoot(t, "version")
	if err != nil {
		t.Fatalf("version failed: %v", err)
	}
	if !strings.Contains(out, buildinfo.Version) {
		t.Fatalf("output %q does not contain version %q", out, buildinfo.Version)
	}
}

func TestVersionJSON(t *testing.T) {
	out, _, err := runRoot(t, "version", "--json")
	if err != nil {
		t.Fatalf("version --json failed: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	for _, k := range []string{"version", "commit", "date", "go", "goos", "goarch"} {
		if _, ok := v[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
}

func TestUnknownSubcommandExitCode(t *testing.T) {
	_, _, err := runRoot(t, "nosuchcommand")
	if ExitCodeFor(err) != 2 {
		t.Fatalf("expected exit code 2, got %d (err=%v)", ExitCodeFor(err), err)
	}
}

func TestVerboseFlagCount(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"--verbose", "--verbose", "--help"})
	_ = root.Execute()
	opts, err := parseOptions(root)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.Verbose != 2 {
		t.Fatalf("expected verbosity 2, got %d", opts.Verbose)
	}
}
