package crypt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func passphraseFromFile(t *testing.T, content string) ([]byte, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return ReadPassphrase(PassphraseSource{File: path})
}

func withStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		r.Close()
	})
	go func() {
		w.Write([]byte(content))
		w.Close()
	}()
}

// The same text has to be the same passphrase whichever way it arrives. The
// file used to keep everything but one final newline and stdin everything up
// to the first \n, \r included: a backup written by a cron job from a file
// could not be opened by typing the same line into stdin.
func TestPassphraseFileAndStdinReadTheSameLine(t *testing.T) {
	for _, content := range []string{
		"pass", "pass\n", "pass\r\n", "pass\n\n", "pass\r\n\r\n", "pass\nsecond line\n", "pass\r",
	} {
		fromFile, err := passphraseFromFile(t, content)
		if err != nil || string(fromFile) != "pass" {
			t.Errorf("file %q = %q, %v; want \"pass\"", content, fromFile, err)
		}
		withStdin(t, content)
		fromStdin, err := ReadPassphrase(PassphraseSource{Stdin: true})
		if err != nil || string(fromStdin) != "pass" {
			t.Errorf("stdin %q = %q, %v; want \"pass\"", content, fromStdin, err)
		}
	}
}

// A file whose first line is empty holds no passphrase under the one rule:
// sealing a backup with what follows would be a guess.
func TestPassphraseFileWithAnEmptyFirstLineIsEmpty(t *testing.T) {
	if _, err := passphraseFromFile(t, "\nsecret\n"); !errors.Is(err, ErrEmptyPassphrase) {
		t.Fatalf("empty first line = %v, want ErrEmptyPassphrase", err)
	}
}

// Backups sealed before the rule was unified used the old readings, and must
// stay readable from the same input: the candidates are the current reading
// first, then each earlier one that differs from it.
func TestPassphraseCandidatesKeepTheEarlierReadings(t *testing.T) {
	cases := []struct {
		content string
		stdin   bool
		want    []string
	}{
		{content: "pass\n", want: []string{"pass"}},
		{content: "pass\r\n", want: []string{"pass"}},
		{content: "pass\n\n", want: []string{"pass", "pass\n"}},
		{content: "first\nsecond\n", want: []string{"first", "first\nsecond"}},
		{content: "pass\r", want: []string{"pass", "pass\r"}},
		{content: "\nsecret\n", want: []string{"\nsecret"}},
		{content: "pass\n", stdin: true, want: []string{"pass"}},
		{content: "pass\r\n", stdin: true, want: []string{"pass", "pass\r"}},
	}
	for _, tc := range cases {
		src := PassphraseSource{Stdin: tc.stdin}
		if tc.stdin {
			withStdin(t, tc.content)
		} else {
			src.File = filepath.Join(t.TempDir(), "pass")
			if err := os.WriteFile(src.File, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := ReadPassphraseCandidates(src)
		if err != nil {
			t.Fatalf("%q (stdin %v): %v", tc.content, tc.stdin, err)
		}
		var texts []string
		for _, p := range got {
			texts = append(texts, string(p))
		}
		if strings.Join(texts, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("%q (stdin %v): candidates %q, want %q", tc.content, tc.stdin, texts, tc.want)
		}
	}
	// Sources that never had another reading have one candidate.
	got, err := ReadPassphraseCandidates(PassphraseSource{Direct: []byte("direct\n")})
	if err != nil || len(got) != 1 || string(got[0]) != "direct\n" {
		t.Fatalf("direct candidates = %q, %v", got, err)
	}
}

// Only the first line is used, and a file holding more than one line says so
// — without the content, which is the secret.
func TestPassphraseFileWithMoreLinesWarns(t *testing.T) {
	for content, wantWarning := range map[string]bool{
		"first-line-987\n":                  false,
		"first-line-987\n\n\r\n":            false,
		"first-line-987\nsecond-line-123\n": true,
	} {
		path := filepath.Join(t.TempDir(), "pass")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		var warnings []string
		if _, err := ReadPassphrase(PassphraseSource{File: path, Warn: func(m string) { warnings = append(warnings, m) }}); err != nil {
			t.Fatal(err)
		}
		if (len(warnings) > 0) != wantWarning {
			t.Fatalf("%q: warnings %q, want a warning: %v", content, warnings, wantWarning)
		}
		for _, w := range warnings {
			if strings.Contains(w, "first-line-987") || strings.Contains(w, "second-line-123") {
				t.Fatalf("warning leaks the passphrase file content: %q", w)
			}
		}
	}
}

// An empty environment variable is an unset one: it used to be an empty
// passphrase, an error, where nothing set would have moved on to the prompt.
func TestEmptyPassphraseEnvIsUnset(t *testing.T) {
	t.Setenv("BACKIMAGE_PASSPHRASE", "")
	if _, err := ReadPassphrase(PassphraseSource{}); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("empty env = %v, want ErrNoPassphrase", err)
	}
	if PassphraseEnvSet("") {
		t.Fatal("an empty BACKIMAGE_PASSPHRASE counts as a credential")
	}
	t.Setenv("BACKIMAGE_PASSPHRASE", "x")
	if !PassphraseEnvSet("") {
		t.Fatal("BACKIMAGE_PASSPHRASE=x does not count as a credential")
	}
}

// ReadPassphraseAll is the writer's side: the passphrase a backup is sealed
// under is the current reading, and an input with an empty first line has
// none, whatever an earlier release read there.
func TestReadPassphraseAllSealsUnderTheCurrentReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(path, []byte("pass\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, earlier, err := ReadPassphraseAll(PassphraseSource{File: path})
	if err != nil || string(p) != "pass" || len(earlier) != 1 || string(earlier[0]) != "pass\n" {
		t.Fatalf("ReadPassphraseAll = %q, %q, %v", p, earlier, err)
	}
	if err := os.WriteFile(path, []byte("\nsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadPassphraseAll(PassphraseSource{File: path}); !errors.Is(err, ErrEmptyPassphrase) {
		t.Fatalf("empty first line = %v, want ErrEmptyPassphrase", err)
	}
}
