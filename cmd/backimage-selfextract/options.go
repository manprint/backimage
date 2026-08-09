package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/fpierri/backimage/pkg/crypt"
	"github.com/fpierri/backimage/pkg/recovery"
)

type commonOptions struct {
	root            string
	passphraseFile  string
	passphraseStdin bool
	identity        string
}

func newFlagSet(name string, common *commonOptions) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&common.root, "root", "/backup", "backup root")
	fs.StringVar(&common.passphraseFile, "passphrase-file", "", "read passphrase from file")
	fs.BoolVar(&common.passphraseStdin, "passphrase-stdin", false, "read passphrase from stdin")
	fs.StringVar(&common.identity, "identity", "", "age private key file")
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
	if o.identity != "" || o.passphraseFile != "" || o.passphraseStdin {
		return true
	}
	_, ok := os.LookupEnv("BACKIMAGE_PASSPHRASE")
	return ok
}

func (o commonOptions) unlock(ctx context.Context, b *recovery.Backup, required bool) error {
	if !b.Manifest.Encryption.Enabled {
		return nil
	}
	if o.identity != "" {
		return b.Unlock(ctx, crypt.Identity{AgeKeyFile: o.identity})
	}
	if !required && !o.hasCredential() {
		return nil
	}
	pass, err := crypt.ReadPassphrase(crypt.PassphraseSource{
		File: o.passphraseFile, Stdin: o.passphraseStdin,
		EnvVar: "BACKIMAGE_PASSPHRASE", Prompt: required,
	})
	if err != nil {
		return err
	}
	defer wipe(pass)
	if err := b.Unlock(ctx, crypt.Identity{Passphrase: pass}); err != nil {
		return fmt.Errorf("passphrase errata: %w", err)
	}
	return nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

type multiFlag []string

func (m *multiFlag) String() string         { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(value string) error { *m = append(*m, value); return nil }
