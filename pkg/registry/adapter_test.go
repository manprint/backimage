package registry

import (
	"context"
	"testing"
)

func TestAdapterForGenericOCI(t *testing.T) {
	a, err := AdapterFor("localhost:5000", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name() != "oci" {
		t.Fatalf("Name() = %q", a.Name())
	}
	caps, err := a.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps&CapListTags == 0 || caps&CapDeleteManifest == 0 || caps&CapUsageStats == 0 {
		t.Fatalf("incomplete OCI capabilities: %b", caps)
	}
}

func TestAdapterForRejectsEmptyHost(t *testing.T) {
	if _, err := AdapterFor("", nil); err == nil {
		t.Fatal("AdapterFor accepted an empty host")
	}
}

// TestCapabilityNames covers what `backimage repo caps` prints. It used to
// print the bitmask itself — "capabilities:45" — which names no operation at
// all, so the command could not answer the one question it exists for.
func TestCapabilityNames(t *testing.T) {
	cases := []struct {
		name string
		caps Capability
		want []string
	}{
		{"none", 0, []string{}},
		{"one", CapListTags, []string{"list-tags"}},
		{
			name: "what the generic OCI adapter declares",
			caps: CapListTags | CapDeleteManifest | CapDeleteTag | CapUsageStats,
			want: []string{"list-tags", "delete-manifest", "delete-tag", "usage-stats"},
		},
		{"every known bit", CapListTags | CapListRepos | CapDeleteManifest | CapDeleteTag | CapGarbageCollect | CapUsageStats,
			[]string{"list-tags", "list-repos", "delete-manifest", "delete-tag", "garbage-collect", "usage-stats"}},
		// A bit this build has no name for must still be visible: silently
		// dropping it would under-report what a newer adapter supports.
		{"an unknown bit", CapListTags | 1<<20, []string{"list-tags", "unknown-0x100000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.caps.Names()
			if len(got) != len(tc.want) {
				t.Fatalf("Names() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Names() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestCapabilityNamesCoversEveryDeclaredBit: a capability added to the const
// block without a name here would print as "unknown-0x..." to users.
func TestCapabilityNamesCoversEveryDeclaredBit(t *testing.T) {
	declared := CapListTags | CapListRepos | CapDeleteManifest | CapDeleteTag | CapGarbageCollect | CapUsageStats
	for _, name := range declared.Names() {
		if len(name) > 8 && name[:8] == "unknown-" {
			t.Fatalf("a declared capability has no name: %s", name)
		}
	}
}

func TestCapabilityHas(t *testing.T) {
	caps := CapListTags | CapDeleteTag
	if !caps.Has(CapListTags) || !caps.Has(CapListTags|CapDeleteTag) {
		t.Fatal("Has must report the bits that are set")
	}
	if caps.Has(CapGarbageCollect) || caps.Has(CapListTags|CapGarbageCollect) {
		t.Fatal("Has must require every bit of want")
	}
}
