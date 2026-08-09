package main

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

	"github.com/fpierri/backimage/internal/cli"
	"github.com/fpierri/backimage/pkg/compress"
	"github.com/fpierri/backimage/pkg/crypt"
	"github.com/fpierri/backimage/pkg/index"
)

const commandTestPass = "selfextract-test-passphrase"

type commandFixture struct {
	root, blob string
	tar        []byte
}

func newCommandFixture(t *testing.T, encrypted bool) commandFixture {
	t.Helper()
	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	mtime := time.Unix(1234, 0).UTC()
	if err := tw.WriteHeader(&tar.Header{Name: "root", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mtime}); err != nil {
		t.Fatal(err)
	}
	fileOffset := int64(512)
	content := []byte("hello from selfextract\n")
	if err := tw.WriteHeader(&tar.Header{Name: "root/a.txt", Typeflag: tar.TypeReg, Mode: 0o640, Size: int64(len(content)), ModTime: mtime}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	codec, err := compress.Get("store")
	if err != nil {
		t.Fatal(err)
	}
	ph := sha256.Sum256(plain.Bytes())
	stored := append([]byte(nil), plain.Bytes()...)
	var km *crypt.KeyMaterial
	var sealer crypt.Sealer
	if encrypted {
		km, err = crypt.NewKeyMaterial()
		if err != nil {
			t.Fatal(err)
		}
		defer km.Wipe()
		sealer, err = crypt.NewSealer(km, crypt.NonceRandom)
		if err != nil {
			t.Fatal(err)
		}
		stored, err = sealer.Seal(nil, 0, codec, plain.Bytes(), ph)
		if err != nil {
			t.Fatal(err)
		}
	}
	sh := sha256.Sum256(stored)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(root, "data", "000000.blob")
	if err := os.WriteFile(blob, stored, 0o600); err != nil {
		t.Fatal(err)
	}
	m := &index.Manifest{
		SchemaVersion: 1, Tool: index.ToolInfo{Name: "backimage", Version: "test"}, CreatedAt: mtime,
		Sources: []string{"/source"}, Totals: index.Totals{Files: 1, Dirs: 1, BytesRaw: int64(len(content)), BytesStored: int64(len(stored))},
		Archive:  index.ArchiveInfo{Format: "tar", Compression: "store"},
		Chunking: index.ChunkingInfo{Strategy: "length", TargetChunkBytes: int64(len(plain.Bytes())), Count: 1},
		Layers:   []index.LayerInfo{{Index: 0, Digest: "sha256:fixture", ChunkFrom: 0, ChunkTo: 0, StoredBytes: int64(len(stored))}},
		Index:    index.Ref{Path: "index.json.zst", Encrypted: encrypted},
	}
	if encrypted {
		m.Encryption = index.EncryptionInfo{Enabled: true, KDF: "scrypt-age", AEAD: "aes256-gcm", NonceMode: "random"}
	}
	writeCommandFile(t, filepath.Join(root, "manifest.json"), func(w io.Writer) error { return index.WriteManifest(w, m) })
	ct := &index.ChunkTable{SchemaVersion: 1, Chunks: []index.Chunk{{
		I: 0, P: "backup/data/000000.blob", Ps: "sha256:" + hex.EncodeToString(ph[:]), Ss: "sha256:" + hex.EncodeToString(sh[:]),
		Pb: int64(plain.Len()), Sb: int64(len(stored)),
	}}}
	writeCommandFile(t, filepath.Join(root, "chunks.json"), func(w io.Writer) error { return index.WriteChunkTable(w, ct) })
	fh := sha256.Sum256(content)
	idx := &index.Index{SchemaVersion: 1, Entries: []index.FileEntry{
		{Path: "root", Type: index.TypeDir, Mode: "0755", MTime: mtime, TarOffset: 0},
		{Path: "root/a.txt", Type: index.TypeRegular, Mode: "0640", Size: int64(len(content)), MTime: mtime, TarOffset: fileOffset, SHA256: hex.EncodeToString(fh[:])},
	}}
	writeCommandFile(t, filepath.Join(root, "index.json.zst"), func(w io.Writer) error { return index.WriteIndex(w, idx, sealer) })
	if encrypted {
		writeCommandFile(t, filepath.Join(root, "keys.pass.age"), func(w io.Writer) error {
			return crypt.WrapKeys(w, km, crypt.Recipients{Passphrase: []byte(commandTestPass)})
		})
	}
	return commandFixture{root: root, blob: blob, tar: append([]byte(nil), plain.Bytes()...)}
}

func writeCommandFile(t *testing.T, path string, fn func(io.Writer) error) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(f); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func captureRun(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return captureRunMode(t, false, args...)
}

func captureRunMode(t *testing.T, tty bool, args ...string) (string, string, error) {
	t.Helper()
	oldOut, oldErr, oldTTY := stdout, stderr, stdoutIsTerminal
	var out, errOut bytes.Buffer
	stdout, stderr = &out, &errOut
	stdoutIsTerminal = func() bool { return tty }
	defer func() { stdout, stderr, stdoutIsTerminal = oldOut, oldErr, oldTTY }()
	err := run(context.Background(), args)
	return out.String(), errOut.String(), err
}

func TestDispatchInfoAndUsage(t *testing.T) {
	f := newCommandFixture(t, false)
	out, _, err := captureRun(t, "info", "--root", f.root)
	if err != nil || !strings.Contains(out, "backup backimage test") {
		t.Fatalf("info = %q, %v", out, err)
	}
	out, _, err = captureRun(t, "info", "--root", f.root, "--json")
	var m index.Manifest
	if err != nil || json.Unmarshal([]byte(out), &m) != nil || m.Tool.Version != "test" {
		t.Fatalf("info json = %q, %v", out, err)
	}
	out, _, err = captureRun(t, "--help")
	if err != nil || !strings.Contains(out, "self-extracting backup") {
		t.Fatalf("help = %q, %v", out, err)
	}
	_, _, err = captureRun(t, "wat")
	if exitCode(err) != exitUsage {
		t.Fatalf("unknown exit = %d, %v", exitCode(err), err)
	}
	_, _, err = captureRun(t, "info", "--root", filepath.Join(f.root, "missing"))
	if exitCode(err) != exitGeneric {
		t.Fatalf("missing info exit = %d", exitCode(err))
	}
}

func TestListEncryptedAndFormats(t *testing.T) {
	f := newCommandFixture(t, true)
	t.Setenv("BACKIMAGE_PASSPHRASE", commandTestPass)
	out, _, err := captureRun(t, "list", "--root", f.root, "--include", "**/a.txt")
	if err != nil || out != "root/a.txt\n" {
		t.Fatalf("list = %q, %v", out, err)
	}
	out, _, err = captureRun(t, "list", "--root", f.root, "-l")
	if err != nil || !strings.Contains(out, "-rw-r-----") {
		t.Fatalf("long list = %q, %v", out, err)
	}
	out, _, err = captureRun(t, "list", "--root", f.root, "--json", "--exclude", "root")
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("json list = %q, %v", out, err)
	}
	t.Setenv("BACKIMAGE_PASSPHRASE", "wrong")
	_, _, err = captureRun(t, "list", "--root", f.root)
	if exitCode(err) != exitPassphrase {
		t.Fatalf("wrong pass exit = %d, %v", exitCode(err), err)
	}
}

func TestTarGuardRoundTripAndIntegrity(t *testing.T) {
	f := newCommandFixture(t, false)
	_, _, err := captureRunMode(t, true, "tar", "--root", f.root)
	if exitCode(err) != exitUsage {
		t.Fatalf("tty tar exit = %d, %v", exitCode(err), err)
	}
	out, _, err := captureRun(t, "tar", "--root", f.root)
	if err != nil || !bytes.Equal([]byte(out), f.tar) {
		t.Fatalf("tar bytes=%d want=%d err=%v", len(out), len(f.tar), err)
	}
	data, _ := os.ReadFile(f.blob)
	data[700] ^= 1
	os.WriteFile(f.blob, data, 0o600)
	_, _, err = captureRun(t, "tar", "--root", f.root)
	if exitCode(err) != exitIntegrity {
		t.Fatalf("corrupt tar exit = %d, %v", exitCode(err), err)
	}
}

func TestExtractPartialStripAndValidation(t *testing.T) {
	f := newCommandFixture(t, false)
	_, _, err := captureRun(t, "extract", "--root", f.root)
	if exitCode(err) != exitUsage {
		t.Fatalf("missing out exit = %d", exitCode(err))
	}
	dst := filepath.Join(t.TempDir(), "restore")
	out, _, err := captureRun(t, "extract", "--root", f.root, "--out", dst, "--include", "**/a.txt", "--strip-components", "1", "--no-preserve-owner")
	if err != nil || !strings.Contains(out, "estratti: 1 file") {
		t.Fatalf("extract = %q, %v", out, err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(got) != "hello from selfextract\n" {
		t.Fatalf("restored = %q, %v", got, err)
	}
	_, _, err = captureRun(t, "extract", "--root", f.root, "--out", dst, "--no-preserve-owner")
	if exitCode(err) != exitUsage {
		t.Fatalf("nonempty out exit = %d", exitCode(err))
	}
	_, _, err = captureRun(t, "extract", "--root", f.root, "--out", t.TempDir(), "--strip-components", "-1")
	if exitCode(err) != exitUsage {
		t.Fatalf("negative strip exit = %d", exitCode(err))
	}
	_, _, err = captureRun(t, "extract", "--root", f.root, "--out", t.TempDir(), "--cpus", "0")
	if exitCode(err) != exitUsage {
		t.Fatalf("invalid cpus exit = %d", exitCode(err))
	}
}

func TestExtractRemovesLocalImageAfterSuccess(t *testing.T) {
	f := newCommandFixture(t, false)
	dst := filepath.Join(t.TempDir(), "restore")
	t.Setenv("BACKIMAGE_IMAGE_REF", "syncbssuser/mindhunt:mindhunters-test")
	oldRemove := removeDockerImage
	t.Cleanup(func() { removeDockerImage = oldRemove })
	var removed string
	removeDockerImage = func(_ context.Context, ref string) error {
		removed = ref
		return nil
	}

	out, _, err := captureRun(t, "extract", "--root", f.root, "--out", dst, "--no-preserve-owner", "--remove-local-image")
	if err != nil {
		t.Fatalf("extract with image removal = %v", err)
	}
	if !strings.Contains(out, "estratti: 1 file") {
		t.Fatalf("extract output = %q", out)
	}
	if removed != "syncbssuser/mindhunt:mindhunters-test" {
		t.Fatalf("removed image = %q", removed)
	}
}

func TestVerifyPartialFullJSONAndCorruption(t *testing.T) {
	f := newCommandFixture(t, true)
	os.Unsetenv("BACKIMAGE_PASSPHRASE")
	out, _, err := captureRun(t, "verify", "--root", f.root)
	if err != nil || !strings.Contains(out, "verifica parziale") {
		t.Fatalf("partial verify = %q, %v", out, err)
	}
	t.Setenv("BACKIMAGE_PASSPHRASE", commandTestPass)
	out, _, err = captureRun(t, "verify", "--root", f.root, "--json")
	if err != nil || !json.Valid([]byte(out)) || !strings.Contains(out, `"full":true`) {
		t.Fatalf("full verify = %q, %v", out, err)
	}
	data, _ := os.ReadFile(f.blob)
	data[len(data)-1] ^= 1
	os.WriteFile(f.blob, data, 0o600)
	out, _, err = captureRun(t, "verify", "--root", f.root, "--continue", "--json")
	if exitCode(err) != exitIntegrity || !json.Valid([]byte(out)) {
		t.Fatalf("corrupt verify = %q, %v", out, err)
	}
}

func TestExitCodesStayAligned(t *testing.T) {
	want := []int{int(cli.KindGeneric), int(cli.KindUsage), int(cli.KindPermission), int(cli.KindPassphrase), int(cli.KindIntegrity), int(cli.KindNetwork), int(cli.KindInterrupted)}
	got := []int{exitGeneric, exitUsage, exitPermission, exitPassphrase, exitIntegrity, exitNetwork, exitInterrupted}
	if !bytes.Equal(intsToBytes(got), intsToBytes(want)) {
		t.Fatalf("exit codes = %v, want %v", got, want)
	}
	if exitCode(nil) != 0 || exitCode(os.ErrPermission) != exitPermission || exitCode(crypt.ErrIntegrity) != exitIntegrity {
		t.Fatal("exitCode sentinel mapping drift")
	}
	if !errors.Is(withCode(exitUsage, io.EOF), io.EOF) {
		t.Fatal("coded error does not unwrap")
	}
}

func intsToBytes(v []int) []byte {
	out := make([]byte, len(v))
	for i := range v {
		out[i] = byte(v[i])
	}
	return out
}
