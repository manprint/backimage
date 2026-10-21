package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --continue used to swap the selective stream for the tolerant one, which
// has no notion of a selection: every entry came out, and with --overwrite it
// went over whatever the destination held.
func TestExtractContinueHonoursTheFilters(t *testing.T) {
	f := newCommandFixtureFiles(t, false,
		fixtureFile{name: "root/a.txt", content: "wanted\n"},
		fixtureFile{name: "root/b.txt", content: "not asked for\n"})
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dst, "root", "b.txt")
	if err := os.WriteFile(local, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := captureRun(t, "extract", "--root", f.root, "--out", dst, "--continue", "--overwrite",
		"--include", "root/a.txt", "--no-preserve-owner")
	if err != nil {
		t.Fatalf("extract --continue --include = %q, %v", out, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "root", "a.txt")); err != nil || string(got) != "wanted\n" {
		t.Fatalf("selected file = %q, %v", got, err)
	}
	if got, err := os.ReadFile(local); err != nil || string(got) != "local edit\n" {
		t.Fatalf("an entry outside the selection was written over the destination: %q, %v", got, err)
	}
}

// With --json the command returned before looking at the partial report, so a
// recovery that lost entries exited 0 and its JSON said nothing about them.
func TestExtractContinueJSONReportsLostEntries(t *testing.T) {
	f := newCommandFixture(t, false)
	data, err := os.ReadFile(f.blob)
	if err != nil {
		t.Fatal(err)
	}
	data[700] ^= 1
	if err := os.WriteFile(f.blob, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"extract", "--root", f.root, "--out", t.TempDir(), "--continue", "--no-preserve-owner"}
		if asJSON {
			args = append(args, "--json")
		}
		out, _, err := captureRun(t, args...)
		if exitCode(err) != exitIntegrity {
			t.Fatalf("json=%v: exit = %d (%v), want %d", asJSON, exitCode(err), err, exitIntegrity)
		}
		if !asJSON {
			continue
		}
		var result struct {
			Files   int64
			Partial *struct {
				Skipped   int   `json:"skipped"`
				BadChunks []int `json:"badChunks"`
			} `json:"partial"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("json output %q: %v", out, err)
		}
		if result.Partial == nil || result.Partial.Skipped == 0 || len(result.Partial.BadChunks) != 1 {
			t.Fatalf("json output does not report the lost entries: %q", out)
		}
	}
}

// A selection that matches nothing is a mistake, as in the host binary: it
// must not read the whole backup to extract nothing and exit 0.
func TestExtractRefusesASelectionThatMatchesNothing(t *testing.T) {
	f := newCommandFixture(t, false)
	dst := filepath.Join(t.TempDir(), "restore")
	_, _, err := captureRun(t, "extract", "--root", f.root, "--out", dst, "--include", "nope/**", "--no-preserve-owner")
	if exitCode(err) != exitUsage {
		t.Fatalf("empty selection exit = %d (%v), want %d", exitCode(err), err, exitUsage)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("destination created for an empty selection: %v", err)
	}
}
