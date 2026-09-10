package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/recovery"
)

type commonOptions struct {
	root             string
	passphraseFile   string
	passphraseStdin  bool
	password         string
	identity         string
	allowUnencrypted bool
}

func newFlagSet(name string, common *commonOptions) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&common.root, "root", "/backup", "backup root")
	fs.StringVar(&common.passphraseFile, "passphrase-file", "", "read passphrase from file")
	fs.BoolVar(&common.passphraseStdin, "passphrase-stdin", false, "read passphrase from stdin")
	fs.StringVar(&common.password, "password", "", "passphrase (visible in shell history and process listings)")
	fs.StringVar(&common.identity, "identity", "", "age private key file")
	fs.BoolVar(&common.allowUnencrypted, "allow-unencrypted", false,
		"accept an unencrypted backup even when a passphrase or an identity was supplied")
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return withCode(exitUsage, err)
	}
	if fs.NArg() != 0 {
		return usageErrorf("argomenti inattesi: %v", fs.Args())
	}
	return nil
}

func (o commonOptions) hasCredential() bool {
	if o.identity != "" || o.passphraseFile != "" || o.passphraseStdin || o.password != "" {
		return true
	}
	_, ok := os.LookupEnv("BACKIMAGE_PASSPHRASE")
	return ok
}

func (o commonOptions) unlock(ctx context.Context, b *recovery.Backup, required bool) error {
	if !b.Manifest.Encryption.Enabled {
		return o.requireEncryption()
	}
	if o.identity != "" {
		if err := b.Unlock(ctx, crypt.Identity{AgeKeyFile: o.identity}); err != nil {
			return unlockError(err)
		}
		return nil
	}
	if !required && !o.hasCredential() {
		return nil
	}
	var direct []byte
	if o.password != "" {
		direct = []byte(o.password)
	}
	pass, err := crypt.ReadPassphrase(crypt.PassphraseSource{
		Direct: direct, File: o.passphraseFile, Stdin: o.passphraseStdin,
		EnvVar: "BACKIMAGE_PASSPHRASE", Prompt: required,
	})
	if err != nil {
		return err
	}
	defer wipe(pass)
	if err := b.Unlock(ctx, crypt.Identity{Passphrase: pass}); err != nil {
		return unlockError(err)
	}
	return nil
}

// unlockError keeps the extractor's diagnosis aligned with the host binary's:
// a private metadata blob that fails authentication is a tampered backup, not
// a mistyped passphrase, and saying otherwise sends the user to fix the wrong
// thing.
func unlockError(err error) error {
	if errors.Is(err, crypt.ErrIntegrity) || errors.Is(err, index.ErrBadSchema) {
		return withCode(exitIntegrity, fmt.Errorf(
			"metadati privati del backup non autenticati, potrebbe essere stato manomesso o sostituito: %w", err))
	}
	return fmt.Errorf("passphrase errata: %w", err)
}

// requireEncryption refuses an unencrypted backup to a caller who supplied a
// credential. The policy has to live here as well as in the host binary: the
// extractor is a separate entry point into the same backup, and a rule that
// holds on one executable and not on the other is not a rule.
//
// The substitution it catches is the whole image being replaced by a plaintext
// one. Without the check the extractor sees encryption disabled, ignores the
// passphrase it was given and extracts attacker-controlled files while
// reporting success.
func (o commonOptions) requireEncryption() error {
	if o.allowUnencrypted || !o.hasCredential() {
		return nil
	}
	return withCode(exitIntegrity, errors.New(
		"backup non cifrato, ma è stata fornita una credenziale: potrebbe essere stato sostituito; "+
			"se il backup è davvero in chiaro usa --allow-unencrypted"))
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

type multiFlag []string

func (m *multiFlag) String() string         { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(value string) error { *m = append(*m, value); return nil }
