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
