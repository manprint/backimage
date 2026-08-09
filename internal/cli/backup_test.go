package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupDryRunJSONHasNoSideEffects(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "f"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	authPath := filepath.Join(base, "config", "backimage", "auth.json")
	outPath := filepath.Join(base, "layout")
	t.Setenv("BACKIMAGE_AUTH_FILE", authPath)
	root := NewRootCommand()
	root.SetArgs([]string{
		"backup", tree, "--repo", "example.test/repo/backup", "--tag", "dry",
		"--dry-run", "--no-encrypt", "--allow-degraded", "--output", "oci-layout",
		"--output-path", outPath, "--json",
	})
	var out strings.Builder
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &value); err != nil {
		t.Fatalf("dry-run output is not JSON: %q: %v", out.String(), err)
	}
	if !strings.Contains(value, "dry-run") {
		t.Fatalf("unexpected dry-run result: %q", value)
	}
	for _, path := range []string{filepath.Dir(authPath), outPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("dry-run created %s: %v", path, err)
		}
	}
}

func TestBackupCLIValidation(t *testing.T) {
	tree := t.TempDir()
	tests := []struct {
		name string
		args []string
	}{
		{"missing repo", []string{"backup", tree}},
		{"bad size", []string{"backup", tree, "--repo", "example.test/repo/x", "--max-layer-size", "1MiBjunk"}},
		{"local output conflict", []string{"backup", tree, "--repo", "example.test/repo/x", "--local-repo", "--output", "registry"}},
		{"encryption conflict", []string{"backup", tree, "--repo", "example.test/repo/x", "--encrypt", "--no-encrypt"}},
		{"key while clear", []string{"backup", tree, "--repo", "example.test/repo/x", "--no-encrypt", "--passphrase-stdin"}},
		{"age identity needs dedup", []string{"backup", tree, "--repo", "example.test/repo/x", "--age-identity", "id.txt"}},
		{"bad CDC bounds", []string{"backup", tree, "--repo", "example.test/repo/x", "--dedup", "--dedup-chunk-min", "512KiB"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := NewRootCommand()
			root.SetArgs(tc.args)
			if err := root.Execute(); ExitCodeFor(err) != 2 {
				t.Fatalf("error = %v, exit=%d", err, ExitCodeFor(err))
			}
		})
	}
}

func TestParseSizeExact(t *testing.T) {
	for in, want := range map[string]int64{
		"": 0, "1024": 1024, "1KiB": 1 << 10, "2MiB": 2 << 20,
		"3GB": 3_000_000_000,
	} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"-1", "1.5GiB", "1MiBjunk", "999999999999999999999TiB"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) unexpectedly succeeded", in)
		}
	}
}

func TestLoginReadyTokenAndFlagConflicts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer ready" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	loginTestEnv(t)
	root := NewRootCommand()
	root.SetArgs([]string{"login", srv.URL, "--token", "ready"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"login", "--token", "x", "--username", "u"},
		{"login", "--list", "example.test"},
		{"login", "--username", "u"},
	} {
		root = NewRootCommand()
		root.SetArgs(args)
		if err := root.Execute(); ExitCodeFor(err) != 2 {
			t.Fatalf("args %v: err=%v exit=%d", args, err, ExitCodeFor(err))
		}
	}
}

func TestBackupOCILayoutSuccessHumanAndJSON(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "file.txt"), []byte("backup payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		layout := filepath.Join(t.TempDir(), "layout")
		args := []string{"backup", tree, "--repo", "example.test/team/success", "--tag", "t1", "--output", "oci-layout", "--output-path", layout, "--no-encrypt", "--allow-degraded", "--platform", "linux/amd64", "--max-layer-size", "8MiB"}
		if asJSON {
			args = append(args, "--json")
		}
		out, _, err := runRoot(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		if asJSON && !json.Valid([]byte(out)) {
			t.Fatalf("backup JSON = %q", out)
		}
		if !asJSON && !strings.Contains(out, "backup completato") {
			t.Fatalf("backup output = %q", out)
		}
		if _, err := os.Stat(filepath.Join(layout, "index.json")); err != nil {
			t.Fatalf("OCI layout missing: %v", err)
		}
	}
}
