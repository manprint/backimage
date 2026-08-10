//go:build unix

package archive

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/manprint/backimage/test/fixtures"
)

// TestRoundTrip covers the non-root feature matrix: each case archives the
// fixture tree, extracts it, and requires a zero-difference CompareTrees run
// against the per-case relaxed comparison options.
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		feats fixtures.Feature
		opts  fixtures.CompareOptions
	}{
		{
			name:  "basic",
			feats: fixtures.FeatBasic,
			opts:  fixtures.CompareOptions{},
		},
		{
			name:  "perms-symlinks-hardlinks",
			feats: fixtures.FeatPerms | fixtures.FeatSymlinks | fixtures.FeatHardlinks,
			opts:  fixtures.CompareOptions{},
		},
		{
			name: "full-non-root",
			feats: fixtures.FeatBasic | fixtures.FeatPerms | fixtures.FeatSymlinks |
				fixtures.FeatHardlinks | fixtures.FeatXattrs | fixtures.FeatNames | fixtures.FeatTimes,
			opts: fixtures.CompareOptions{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := t.TempDir()
			dst := t.TempDir()
			fixtures.Build(t, src, tc.feats)

			var buf bytes.Buffer
			w := NewWriter(&buf, Options{PreserveXattrs: true})
			if err := w.AddRoot(context.Background(), src); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Close(); err != nil {
				t.Fatal(err)
			}

			x := NewExtractor(ExtractOptions{PreserveXattrs: true, PreserveOwner: true, Strict: true})
			if _, err := x.Extract(context.Background(), &buf, dst); err != nil {
				t.Fatal(err)
			}

			got := filepath.Join(dst, baseOf(src))
			diffs := fixtures.CompareTrees(t, src, got, tc.opts)
			if len(diffs) != 0 {
				t.Fatalf("round-trip %q not faithful, %d diffs:\n%+v", tc.name, len(diffs), diffs)
			}
		})
	}
}
