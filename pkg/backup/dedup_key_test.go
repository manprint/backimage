package backup

import (
	"bytes"
	"context"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
)

const dedupTestPassphrase = "same passphrase"

// wrapMaterial returns km as the keys.pass.age file of a base backup.
func wrapMaterial(t *testing.T, km *crypt.KeyMaterial) []byte {
	t.Helper()
	var wrapped bytes.Buffer
	if err := crypt.WrapKeys(&wrapped, km, crypt.Recipients{Passphrase: []byte(dedupTestPassphrase)}); err != nil {
		t.Fatal(err)
	}
	return wrapped.Bytes()
}

// baseWith builds a dedup base whose key file is wrapped, and whose public
// manifest says exactly what the caller wants it to say.
func baseWith(t *testing.T, km *crypt.KeyMaterial, enc index.EncryptionInfo, tag string) *dedupBase {
	t.Helper()
	return &dedupBase{
		manifest: &index.Manifest{
			CreatedAt:  time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
			Encryption: enc,
		},
		keyFiles: map[string][]byte{"keys.pass.age": wrapMaterial(t, km)},
		tag:      tag,
	}
}

// currentEncryption is a public manifest describing a perfectly current,
// reusable key. Every test below hands it to a base whose key material says
// otherwise: if the manifest were still the authority, they would all pass.
func currentEncryption() index.EncryptionInfo {
	return index.EncryptionInfo{
		Enabled:         true,
		NonceMode:       "convergent",
		EnvelopeVersion: crypt.EnvelopeVersion,
	}
}

func reuse(t *testing.T, previous *dedupBase, cfg Config) (*crypt.KeyMaterial, string) {
	t.Helper()
	km, refusal := reuseDedupKey(previous, cfg, []byte(dedupTestPassphrase))
	return km, refusal
}

// TestOnlyTheAttestationDecidesReuse is A05. The public fields of
// manifest.json are writable by anyone who can push a tag: an attacker who
// wants a burned key to seal again only has to say that it is fine. Here the
// manifest always says it is fine, and every refusal has to come from the
// material inside the age blob, which that attacker cannot edit.
func TestOnlyTheAttestationDecidesReuse(t *testing.T) {
	fresh, err := crypt.NewDedupKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Wipe()

	legacy := fresh.Clone()
	legacy.SchemaVersion = 1
	legacy.EnvelopeVersion, legacy.NonceMode, legacy.Reuse = 0, "", ""

	oldEpoch := fresh.Clone()
	oldEpoch.EnvelopeVersion = crypt.EnvelopeVersion - 1

	newEpoch := fresh.Clone()
	newEpoch.EnvelopeVersion = crypt.EnvelopeVersion + 1

	randomMode, err := crypt.NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer randomMode.Wipe()

	singleUse := fresh.Clone()
	singleUse.Reuse = crypt.ReuseNever

	cases := []struct {
		name     string
		material *crypt.KeyMaterial
		want     string
	}{
		{"pre-0.4.1 material has no attestation", legacy, "epoca crittografica"},
		{"older envelope", oldEpoch, "un'altra versione"},
		{"newer envelope", newEpoch, "un'altra versione"},
		{"random nonces", randomMode, "modalita' nonce"},
		{"policy says single use", singleUse, "non riusabile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := baseWith(t, tc.material, currentEncryption(), "t1")
			km, refusal := reuse(t, previous, Config{})
			if km != nil {
				km.Wipe()
				t.Fatal("the manifest was believed over the attestation: a burned key was reused")
			}
			if !strings.Contains(refusal, tc.want) {
				t.Fatalf("refusal = %q, want it to mention %q", refusal, tc.want)
			}
		})
	}
}

// TestALyingManifestCannotBurnAGoodKeyEither is the same rule read from the
// other side. The public field is a planning hint, so rewriting it — to a
// legacy version, to another nonce mode, to nothing at all — must not cost a
// full re-upload on a key that is perfectly sound.
func TestALyingManifestCannotBurnAGoodKeyEither(t *testing.T) {
	fresh, err := crypt.NewDedupKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Wipe()

	manifests := map[string]index.EncryptionInfo{
		"envelope version absent, as 0.2.3 wrote it": {Enabled: true, NonceMode: "convergent"},
		"envelope version from another epoch":        {Enabled: true, NonceMode: "convergent", EnvelopeVersion: 1},
		"nonce mode rewritten":                       {Enabled: true, NonceMode: "random", EnvelopeVersion: crypt.EnvelopeVersion},
	}
	for name, enc := range manifests {
		t.Run(name, func(t *testing.T) {
			previous := baseWith(t, fresh, enc, "t1")
			km, refusal := reuse(t, previous, Config{})
			if km == nil {
				t.Fatalf("a sound key was refused because of a public field: %q", refusal)
			}
			defer km.Wipe()
			if !bytes.Equal(km.DEK, fresh.DEK) || !bytes.Equal(km.NonceKey, fresh.NonceKey) {
				t.Fatal("reused key material must be the previous one")
			}
		})
	}
}

// TestRotateKeyIsAnExplicitDecision covers the other half of A05: rotation
// exists, it is asked for, and it is announced with its cost.
func TestRotateKeyIsAnExplicitDecision(t *testing.T) {
	fresh, err := crypt.NewDedupKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Wipe()
	previous := baseWith(t, fresh, currentEncryption(), "t1")

	km, refusal := reuse(t, previous, Config{RotateKey: true})
	if km != nil {
		km.Wipe()
		t.Fatal("--rotate-key must not reuse the previous key")
	}
	if !strings.Contains(refusal, "--rotate-key") || !strings.Contains(refusal, "ricarica tutti i blob") {
		t.Fatalf("rotation must announce itself and its cost, got %q", refusal)
	}

	km, refusal = reuse(t, previous, Config{})
	if km == nil {
		t.Fatalf("without --rotate-key the same key must still be reused: %q", refusal)
	}
	km.Wipe()
}

// TestFreshMaterialAttestsWhatTheRunIsDoing checks the writer side: a --dedup
// run must produce material a later run can accept, and a plain encrypted run
// must produce material no later run will ever reuse.
func TestFreshMaterialAttestsWhatTheRunIsDoing(t *testing.T) {
	dedupKey, err := crypt.NewDedupKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer dedupKey.Wipe()
	if err := dedupKey.ReusableFor(crypt.EnvelopeVersion, crypt.NonceConvergent); err != nil {
		t.Fatalf("a fresh dedup key must be reusable by the next run: %v", err)
	}
	single, err := crypt.NewKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	defer single.Wipe()
	if err := single.ReusableFor(crypt.EnvelopeVersion, crypt.NonceConvergent); err == nil {
		t.Fatal("a key generated for a run without --dedup must never be reused")
	}
}

// TestRotationCostsOneFullReupload measures what the changelog promises: the
// backup after a rotation shares nothing, and the one after that dedups
// normally again.
func TestRotationCostsOneFullReupload(t *testing.T) {
	reg := newMemReg()
	srv := reg.server()
	defer srv.Close()

	// Incompressible noise from a fixed seed: a compressible fixture would
	// store a couple of kilobytes and the measurement would say nothing.
	tree := t.TempDir()
	body := make([]byte, 8<<20)
	if _, err := mrand.New(mrand.NewSource(7)).Read(body); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "data.bin"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	baseRef := strings.TrimPrefix(srv.URL, "http://") + "/me/rotation"
	cfg := pipelinePushConfig(srv.URL, tree)
	cfg.TempDir = t.TempDir()
	cfg.CheckpointDir = t.TempDir()
	cfg.Encrypt = true
	cfg.Passphrase = func() ([]byte, error) { return []byte(dedupTestPassphrase), nil }
	cfg.Dedup = true
	cfg.MaxLayerSize = 16 << 20
	cfg.Runnable = false
	cfg.Platforms = []string{"linux/amd64"}

	run := func(tag string, rotate bool) (Result, []string) {
		t.Helper()
		var warnings []string
		cfg.Ref = baseRef + ":" + tag
		cfg.RotateKey = rotate
		cfg.Progress = func(s string) { warnings = append(warnings, s) }
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatalf("backup %s: %v", tag, err)
		}
		return res, warnings
	}

	first, _ := run("t1", false)
	shared, _ := run("t2", false)
	if shared.SkippedBytes == 0 {
		t.Fatalf("without rotation the second backup must share blobs: %+v", shared)
	}
	rotated, warnings := run("t3", true)
	// A handful of small blobs (the stub layer, the config) are the same
	// bytes whatever the key, so the claim is about the data: the rotated
	// backup re-uploads all of it.
	if rotated.SkippedBytes*4 > shared.SkippedBytes {
		t.Fatalf("a rotated key must re-upload the data: skipped %d bytes, the shared run skipped %d",
			rotated.SkippedBytes, shared.SkippedBytes)
	}
	if rotated.UploadedBytes < first.UploadedBytes/2 {
		t.Fatalf("a rotated key must pay a full upload: %d bytes against %d of the first backup",
			rotated.UploadedBytes, first.UploadedBytes)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "--rotate-key") {
		t.Fatalf("the rotation was not announced: %v", warnings)
	}
	after, _ := run("t4", false)
	// Same order of magnitude as a normal incremental run: which of the two
	// skips a couple of kilobytes more depends on which small blob the base
	// tag already had, and that is not what this measures.
	if after.SkippedBytes < shared.SkippedBytes/2 {
		t.Fatalf("dedup must work again with the rotated key: %+v", after)
	}
	t.Logf("uploaded/skipped bytes: first=%d/%d shared=%d/%d rotated=%d/%d after=%d/%d",
		first.UploadedBytes, first.SkippedBytes, shared.UploadedBytes, shared.SkippedBytes,
		rotated.UploadedBytes, rotated.SkippedBytes, after.UploadedBytes, after.SkippedBytes)
}
