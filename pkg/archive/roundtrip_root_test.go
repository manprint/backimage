//go:build root && unix

package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fpierri/backimage/test/fixtures"
)

// TestRoundTripRoot exercises the root-gated feature matrix (ACLs, caps,
// devices, fifos, ownership). Compiled only with -tags root; running it
// without privileges fails the fixture build, which is the gate behaviour.
func TestRoundTripRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("TestRoundTripRoot requires root: run 'sudo -E go test -tags root ./pkg/archive/'")
	}
	cases := []struct {
		name  string
		feats fixtures.Feature
		opts  fixtures.CompareOptions
	}{
		{
			name: "root-features",
			feats: fixtures.FeatACLs | fixtures.FeatCaps | fixtures.FeatDevices |
				fixtures.FeatFifos | fixtures.FeatOwnership,
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
				t.Fatalf("root round-trip not faithful, %d diffs:\n%+v", len(diffs), diffs)
			}
		})
	}
}
