package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/fpierri/backimage/pkg/compress"
	"github.com/fpierri/backimage/pkg/crypt"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/recovery"
	"github.com/fpierri/backimage/pkg/registry"
	restorepkg "github.com/fpierri/backimage/pkg/restore"
)

const cliRestorePass = "cli-restore-passphrase"

type mockImageSource struct {
	manifest    *index.Manifest
	table       *index.ChunkTable
	keys        map[string][]byte
	idx         []byte
	blobs       [][]byte
	reads       int
	closed      bool
	manifestErr error
	tableErr    error
	indexErr    error
}

func (s *mockImageSource) Manifest(context.Context) (*index.Manifest, error) {
	return s.manifest, s.manifestErr
}
func (s *mockImageSource) ChunkTable(context.Context) (*index.ChunkTable, error) {
	return s.table, s.tableErr
}
func (s *mockImageSource) KeyFile(_ context.Context, name string) ([]byte, error) {
	b, ok := s.keys[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), b...), nil
}
func (s *mockImageSource) IndexBlob(context.Context) ([]byte, error) {
	return append([]byte(nil), s.idx...), s.indexErr
}
func (s *mockImageSource) Blob(_ context.Context, i int) ([]byte, error) {
	s.reads++
	if i < 0 || i >= len(s.blobs) {
		return nil, errors.New("blob out of range")
	}
	return append([]byte(nil), s.blobs[i]...), nil
}
func (s *mockImageSource) Close() error { s.closed = true; return nil }

func newMockImageSource(t *testing.T, encrypted bool) (*mockImageSource, []byte) {
	t.Helper()
	mtime := time.Unix(123, 0).UTC()
	content := []byte("restore me\n")
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "root", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mtime}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "root/file.txt", Typeflag: tar.TypeReg, Mode: 0o640, Size: int64(len(content)), ModTime: mtime}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	plain := archive.Bytes()
	ph := sha256.Sum256(plain)
	stored := append([]byte(nil), plain...)
	var km *crypt.KeyMaterial
	var sealer crypt.Sealer
	var err error
	if encrypted {
		km, err = crypt.NewKeyMaterial()
		if err != nil {
			t.Fatal(err)
		}
		defer km.Wipe()
		codec, _ := compress.Get("store")
		sealer, err = crypt.NewSealer(km, crypt.NonceRandom)
		if err != nil {
			t.Fatal(err)
		}
		stored, err = sealer.Seal(nil, 0, codec, plain, ph)
		if err != nil {
			t.Fatal(err)
		}
	}
	sh := sha256.Sum256(stored)
	m := &index.Manifest{
		SchemaVersion: 1, Tool: index.ToolInfo{Name: "backimage", Version: "test"}, CreatedAt: mtime,
		Sources: []string{"/source"}, Totals: index.Totals{Files: 1, Dirs: 1, BytesRaw: int64(len(content)), BytesStored: int64(len(stored))},
		Archive:  index.ArchiveInfo{Format: "tar", Compression: "store"},
		Chunking: index.ChunkingInfo{Strategy: "length", TargetChunkBytes: int64(len(plain)), Count: 1},
		Layers:   []index.LayerInfo{{Index: 0, Digest: "sha256:data", ChunkFrom: 0, ChunkTo: 0, StoredBytes: int64(len(stored))}},
		Index:    index.Ref{Path: "index.json.zst", Encrypted: encrypted},
	}
	if encrypted {
		m.Encryption = index.EncryptionInfo{Enabled: true, KDF: "scrypt-age", AEAD: "aes256-gcm", NonceMode: "random"}
	}
	ct := &index.ChunkTable{SchemaVersion: 1, Chunks: []index.Chunk{{I: 0, P: "backup/data/000000.blob", Ps: "sha256:" + hex.EncodeToString(ph[:]), Ss: "sha256:" + hex.EncodeToString(sh[:]), Pb: int64(len(plain)), Sb: int64(len(stored))}}}
	fh := sha256.Sum256(content)
	idx := &index.Index{SchemaVersion: 1, Entries: []index.FileEntry{
		{Path: "root", Type: index.TypeDir, Mode: "0755", MTime: mtime, TarOffset: 0},
		{Path: "root/file.txt", Type: index.TypeRegular, Mode: "0640", Size: int64(len(content)), MTime: mtime, TarOffset: 512, SHA256: hex.EncodeToString(fh[:])},
	}}
	var ib bytes.Buffer
	if err := index.WriteIndex(&ib, idx, sealer); err != nil {
		t.Fatal(err)
	}
	keys := map[string][]byte{}
	if encrypted {
		var kb bytes.Buffer
		if err := crypt.WrapKeys(&kb, km, crypt.Recipients{Passphrase: []byte(cliRestorePass)}); err != nil {
			t.Fatal(err)
		}
		keys["keys.pass.age"] = kb.Bytes()
	}
	return &mockImageSource{manifest: m, table: ct, keys: keys, idx: ib.Bytes(), blobs: [][]byte{stored}}, append([]byte(nil), plain...)
}

func withMockSource(t *testing.T, source restorepkg.Source) {
	t.Helper()
	old := openSourceForCLI
	openSourceForCLI = func(context.Context, string, sourceFlags) (restorepkg.Source, error) { return source, nil }
	t.Cleanup(func() { openSourceForCLI = old })
}

func TestReadCommandsWithMockSource(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	out, _, err := runRoot(t, "inspect", "example.test/repo:tag", "--layers")
	if err != nil || !strings.Contains(out, "layer") {
		t.Fatalf("inspect = %q, %v", out, err)
	}
	out, _, err = runRoot(t, "--json", "inspect", "example.test/repo:tag", "--files")
	var inspected inspectResult
	if err != nil || json.Unmarshal([]byte(out), &inspected) != nil || len(inspected.Files) != 2 {
		t.Fatalf("inspect json = %q, %v", out, err)
	}
	out, _, err = runRoot(t, "ls", "example.test/repo:tag", "root", "-l")
	if err != nil || !strings.Contains(out, "root/file.txt") || !strings.Contains(out, "-rw-r-----") {
		t.Fatalf("ls = %q, %v", out, err)
	}
	out, _, err = runRoot(t, "--json", "find", "example.test/repo:tag", "**/file.txt")
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("find = %q, %v", out, err)
	}
	out, _, err = runRoot(t, "verify", "example.test/repo:tag", "--quick", "--json")
	if err != nil || !strings.Contains(out, `"quick":true`) {
		t.Fatalf("quick verify = %q, %v", out, err)
	}
	out, _, err = runRoot(t, "verify", "example.test/repo:tag")
	if err != nil || !strings.Contains(out, "backup integro") {
		t.Fatalf("verify = %q, %v", out, err)
	}
	if !s.closed {
		t.Fatal("sources were not closed")
	}
}

func TestRestoreTarExtractPartialAndJSON(t *testing.T) {
	s, tarBytes := newMockImageSource(t, false)
	withMockSource(t, s)
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "out.tar")
	_, _, err := runRoot(t, "restore", "example.test/repo:tag", "-o", tarPath)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(tarPath)
	if !bytes.Equal(got, tarBytes) {
		t.Fatal("restore tar differs")
	}
	_, _, err = runRoot(t, "restore", "example.test/repo:tag", "-o", tarPath)
	if err == nil {
		t.Fatal("existing tar unexpectedly overwritten")
	}
	_, _, err = runRoot(t, "restore", "example.test/repo:tag", "-o", tarPath, "--overwrite")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "extract")
	_, _, err = runRoot(t, "restore", "example.test/repo:tag", "--extract", "-C", dst, "--include", "**/file.txt", "--no-preserve-owner")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dst, "root", "file.txt"))
	if err != nil || string(content) != "restore me\n" {
		t.Fatalf("extracted = %q, %v", content, err)
	}
	jsonTar := filepath.Join(dir, "json.tar")
	out, _, err := runRoot(t, "--json", "restore", "example.test/repo:tag", "-o", jsonTar)
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("restore json = %q, %v", out, err)
	}
}

func TestRestoreRemovesLocalImageAfterSuccess(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	oldRemove := removeDockerImage
	t.Cleanup(func() { removeDockerImage = oldRemove })
	var removed string
	removeDockerImage = func(_ context.Context, ref string) error {
		removed = ref
		return nil
	}

	dst := filepath.Join(t.TempDir(), "extract")
	_, _, err := runRoot(t, "restore", "example.test/repo:tag", "--extract", "-C", dst, "--no-preserve-owner", "--remove-local-image")
	if err != nil {
		t.Fatalf("restore with image removal = %v", err)
	}
	if removed != "example.test/repo:tag" {
		t.Fatalf("removed image = %q", removed)
	}
}

func TestRestoreWrongPassphraseBeforeBlob(t *testing.T) {
	s, _ := newMockImageSource(t, true)
	withMockSource(t, s)
	t.Setenv("BACKIMAGE_PASSPHRASE", "wrong")
	_, _, err := runRoot(t, "restore", "example.test/repo:tag", "-o", filepath.Join(t.TempDir(), "bad.tar"))
	if ExitCodeFor(err) != int(KindPassphrase) || s.reads != 0 {
		t.Fatalf("wrong pass = code %d, reads %d, err %v", ExitCodeFor(err), s.reads, err)
	}
	s2, _ := newMockImageSource(t, true)
	withMockSource(t, s2)
	t.Setenv("BACKIMAGE_PASSPHRASE", cliRestorePass)
	_, _, err = runRoot(t, "restore", "example.test/repo:tag", "-o", filepath.Join(t.TempDir(), "ok.tar"))
	if err != nil || s2.reads == 0 {
		t.Fatalf("correct pass = reads %d, err %v", s2.reads, err)
	}
}

func TestRestoreUsageAndHelpers(t *testing.T) {
	if _, err := resolveReference([]string{"x"}, "y"); ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("conflict = %v", err)
	}
	if _, err := resolveReference(nil, ""); ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("missing = %v", err)
	}
	if got := defaultTarName("example.test/team/dumps:daily"); got != "dumps_daily.tar" {
		t.Fatalf("default name = %q", got)
	}
	if got := sanitizeName("a/b:c"); got != "a_b_c" {
		t.Fatalf("sanitize = %q", got)
	}
	check := writableCheck("tmp", t.TempDir())
	if !check.Available {
		t.Fatalf("writable check = %+v", check)
	}
	if optionalReason(nil) != "" || optionalReason(io.EOF) == "" {
		t.Fatal("optionalReason")
	}
}

func TestRestoreStdoutEmptySelectionAndValidation(t *testing.T) {
	s, tarBytes := newMockImageSource(t, false)
	withMockSource(t, s)
	out, _, err := runRoot(t, "restore", "example.test/repo:tag", "-o", "-")
	if err != nil || !bytes.Equal([]byte(out), tarBytes) {
		t.Fatalf("stdout tar = %d bytes, %v", len(out), err)
	}
	_, _, err = runRoot(t, "--json", "restore", "example.test/repo:tag", "-o", "-")
	if ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("json stdout = %v", err)
	}
	_, _, err = runRoot(t, "restore", "example.test/repo:tag", "-o", filepath.Join(t.TempDir(), "none.tar"), "--include", "absent")
	if ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("empty selection = %v", err)
	}
	_, _, err = runRoot(t, "restore", "example.test/repo:tag", "--strip-components", "-1")
	if ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("negative strip = %v", err)
	}
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "existing"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = runRoot(t, "restore", "example.test/repo:tag", "--extract", "-C", dest, "--no-preserve-owner")
	if ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("nonempty extraction = %v", err)
	}
}

func TestVerifyCorruptionAndDoctor(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	s.blobs[0][10] ^= 1
	withMockSource(t, s)
	out, _, err := runRoot(t, "--json", "verify", "example.test/repo:tag", "--continue")
	if ExitCodeFor(err) != int(KindIntegrity) || !json.Valid([]byte(out)) {
		t.Fatalf("corrupt verify = %q, %v", out, err)
	}
	out, _, err = runRoot(t, "--json", "doctor", t.TempDir())
	if !json.Valid([]byte(out)) || (err != nil && ExitCodeFor(err) != int(KindPermission)) {
		t.Fatalf("doctor = %q, %v", out, err)
	}
}

func TestOpenImageSourceValidation(t *testing.T) {
	ctx := context.Background()
	if _, err := openImageSource(ctx, "example.test/repo:tag", sourceFlags{localRepo: true, ociLayout: "x"}); ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("source conflict = %v", err)
	}
	if _, err := openImageSource(ctx, "bad ref !", sourceFlags{}); ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("bad ref = %v", err)
	}
	if _, err := openImageSource(ctx, "example.test/repo:tag", sourceFlags{cacheSize: "wat"}); ExitCodeFor(err) != int(KindUsage) {
		t.Fatalf("bad cache = %v", err)
	}
	if _, err := openImageSource(ctx, "example.test/repo:tag", sourceFlags{ociLayout: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing layout accepted")
	}
}

func TestOpenImageSourceFactories(t *testing.T) {
	ctx := context.Background()
	s, _ := newMockImageSource(t, false)
	oldRegistry, oldLayout, oldDaemon := fromRegistryCLI, fromLayoutCLI, fromDaemonCLI
	t.Cleanup(func() { fromRegistryCLI, fromLayoutCLI, fromDaemonCLI = oldRegistry, oldLayout, oldDaemon })
	fromRegistryCLI = func(context.Context, name.Reference, registry.Keychain, restorepkg.SourceOptions) (restorepkg.Source, error) {
		return s, nil
	}
	fromLayoutCLI = func(string, string) (restorepkg.Source, error) { return s, nil }
	fromDaemonCLI = func(context.Context, name.Reference) (restorepkg.Source, error) { return s, nil }
	t.Setenv("BACKIMAGE_AUTH_FILE", filepath.Join(t.TempDir(), "auth.json"))
	for _, flags := range []sourceFlags{{cacheSize: "1MiB", platform: "linux/amd64"}, {ociLayout: "/layout"}, {localRepo: true}} {
		got, err := openImageSource(ctx, "example.test/repo:tag", flags)
		if err != nil || got != s {
			t.Fatalf("factory flags %+v = %v, %v", flags, got, err)
		}
	}
	fromRegistryCLI = func(context.Context, name.Reference, registry.Keychain, restorepkg.SourceOptions) (restorepkg.Source, error) {
		return nil, io.EOF
	}
	if _, err := openImageSource(ctx, "example.test/repo:tag", sourceFlags{cacheSize: "1MiB"}); ExitCodeFor(err) != int(KindNetwork) {
		t.Fatalf("registry error = %v", err)
	}
	fromDaemonCLI = func(context.Context, name.Reference) (restorepkg.Source, error) { return nil, io.EOF }
	if _, err := openImageSource(ctx, "example.test/repo:tag", sourceFlags{localRepo: true}); ExitCodeFor(err) != int(KindNetwork) {
		t.Fatalf("daemon error = %v", err)
	}
}

func TestReadCommandFailurePaths(t *testing.T) {
	old := openSourceForCLI
	t.Cleanup(func() { openSourceForCLI = old })
	openSourceForCLI = func(context.Context, string, sourceFlags) (restorepkg.Source, error) { return nil, io.EOF }
	for _, args := range [][]string{{"inspect", "x"}, {"ls", "x"}, {"find", "x", "*"}, {"verify", "x"}, {"restore", "x"}} {
		if _, _, err := runRoot(t, args...); err == nil {
			t.Errorf("%v accepted source failure", args)
		}
	}

	base, _ := newMockImageSource(t, false)
	base.manifestErr = io.ErrUnexpectedEOF
	openSourceForCLI = func(context.Context, string, sourceFlags) (restorepkg.Source, error) { return base, nil }
	for _, args := range [][]string{{"inspect", "x"}, {"ls", "x"}, {"find", "x", "*"}, {"verify", "x"}, {"restore", "x"}} {
		if _, _, err := runRoot(t, args...); err == nil {
			t.Errorf("%v accepted manifest failure", args)
		}
	}

	encrypted, _ := newMockImageSource(t, true)
	openSourceForCLI = func(context.Context, string, sourceFlags) (restorepkg.Source, error) { return encrypted, nil }
	t.Setenv("BACKIMAGE_PASSPHRASE", "wrong")
	for _, args := range [][]string{{"inspect", "x", "--files"}, {"ls", "x"}, {"find", "x", "*"}, {"verify", "x"}} {
		if _, _, err := runRoot(t, args...); ExitCodeFor(err) != int(KindPassphrase) {
			t.Errorf("%v wrong-pass = %v", args, err)
		}
	}

	badIndex, _ := newMockImageSource(t, false)
	badIndex.indexErr = io.ErrUnexpectedEOF
	openSourceForCLI = func(context.Context, string, sourceFlags) (restorepkg.Source, error) { return badIndex, nil }
	for _, args := range [][]string{{"inspect", "x", "--files"}, {"ls", "x"}, {"find", "x", "*"}} {
		if _, _, err := runRoot(t, args...); err == nil {
			t.Errorf("%v accepted index failure", args)
		}
	}
}

func TestIntegrityPermissionAndOptionalUnlockPaths(t *testing.T) {
	corrupt, _ := newMockImageSource(t, false)
	corrupt.blobs[0][20] ^= 1
	withMockSource(t, corrupt)
	outPath := filepath.Join(t.TempDir(), "corrupt.tar")
	_, _, err := runRoot(t, "restore", "x", "-o", outPath)
	if ExitCodeFor(err) != int(KindIntegrity) {
		t.Fatalf("corrupt restore = %v", err)
	}
	if _, statErr := os.Stat(outPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial output retained: %v", statErr)
	}

	good, _ := newMockImageSource(t, false)
	withMockSource(t, good)
	_, _, err = runRoot(t, "restore", "x", "--extract", "-C", filepath.Join(t.TempDir(), "dest"))
	if err != nil && ExitCodeFor(err) != int(KindPermission) {
		t.Fatalf("privilege preflight = %v", err)
	}

	encrypted, _ := newMockImageSource(t, true)
	b, err := recovery.OpenBlobSource(context.Background(), encrypted)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	oldPass, hadPass := os.LookupEnv("BACKIMAGE_PASSPHRASE")
	if err := os.Unsetenv("BACKIMAGE_PASSPHRASE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadPass {
			_ = os.Setenv("BACKIMAGE_PASSPHRASE", oldPass)
		} else {
			_ = os.Unsetenv("BACKIMAGE_PASSPHRASE")
		}
	})
	if err := unlockBackup(context.Background(), b, sourceFlags{}, false); err != nil {
		t.Fatalf("optional unlock = %v", err)
	}
}

func TestReadCommandGlobAndHumanDoctorBranches(t *testing.T) {
	s, _ := newMockImageSource(t, false)
	withMockSource(t, s)
	for _, args := range [][]string{{"ls", "x", "--include", "["}, {"find", "x", "["}} {
		if _, _, err := runRoot(t, args...); ExitCodeFor(err) != int(KindUsage) {
			t.Errorf("invalid glob %v = %v", args, err)
		}
	}
	s.blobs[0][30] ^= 1
	if _, _, err := runRoot(t, "verify", "x", "--continue"); ExitCodeFor(err) != int(KindIntegrity) {
		t.Fatalf("human corrupt verify = %v", err)
	}
	out, _, err := runRoot(t, "doctor", t.TempDir())
	if out == "" || (err != nil && ExitCodeFor(err) != int(KindPermission)) {
		t.Fatalf("human doctor = %q, %v", out, err)
	}
}
