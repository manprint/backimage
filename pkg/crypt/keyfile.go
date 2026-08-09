package crypt

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

// ErrWrongPassphrase is returned when no identity can open the key file.
// It maps to exit code 4 in the CLI. The message never distinguishes a wrong
// passphrase from a corrupt file, so callers cannot build an oracle.
var ErrWrongPassphrase = errors.New("wrong passphrase or key")

// Recipients describes who can open the backup.
type Recipients struct {
	Passphrase []byte   // optional; when set, an age scrypt recipient is added
	AgeKeys    []string // optional; "age1..." X25519 public keys
}

// Identity is either a passphrase or an age X25519 identity.
type Identity struct {
	Passphrase []byte
	AgeKeyFile string // path to a file containing "AGE-SECRET-KEY-1-..."
}

// ErrMixedRecipients is returned when both a passphrase and age keys are
// given: age cannot mix scrypt with X25519 in one file. The manifest writer
// (phase 05) must produce two files in that case (keys.age and keys.pass.age,
// same KeyMaterial), and the self-extract tries keys.age first.
var ErrMixedRecipients = errors.New("mixed recipients: produce two key files (keys.age + keys.pass.age)")

// WrapKeys serialises km as JSON and encrypts it for the given recipients,
// writing one ASCII-armored age file to w.
//
// Use exactly one recipient kind here; for the two-file layout requested by
// the user call WrapKeys twice (once per kind) writing keys.age and
// keys.pass.age respectively.
func WrapKeys(w io.Writer, km *KeyMaterial, rcpt Recipients) error {
	if len(rcpt.Passphrase) > 0 && len(rcpt.AgeKeys) > 0 {
		return ErrMixedRecipients
	}
	if len(rcpt.Passphrase) == 0 && len(rcpt.AgeKeys) == 0 {
		return errors.New("no recipients: refusing to produce an unopenable backup")
	}
	if len(rcpt.Passphrase) > 0 {
		r, err := age.NewScryptRecipient(string(rcpt.Passphrase))
		if err != nil {
			return err
		}
		return encryptTo(w, km, r)
	}
	if len(rcpt.AgeKeys) > 1 {
		return errors.New("multiple age keys: not supported in one file")
	}
	r, err := age.ParseX25519Recipient(rcpt.AgeKeys[0])
	if err != nil {
		return fmt.Errorf("age recipient: %w", err)
	}
	return encryptTo(w, km, r)
}

func encryptTo(w io.Writer, km *KeyMaterial, rcpt age.Recipient) error {
	plain, err := km.MarshalJSON()
	if err != nil {
		return err
	}
	defer zero(plain)
	out, err := age.Encrypt(w, rcpt)
	if err != nil {
		return err
	}
	if _, err := out.Write(plain); err != nil {
		return err
	}
	return out.Close()
}

// UnwrapKeys decrypts an age file produced by WrapKeys.
func UnwrapKeys(r io.Reader, id Identity) (*KeyMaterial, error) {
	var identities []age.Identity
	if len(id.Passphrase) > 0 {
		ident, err := age.NewScryptIdentity(string(id.Passphrase))
		if err != nil {
			return nil, err
		}
		identities = append(identities, ident)
	}
	if id.AgeKeyFile != "" {
		data, err := os.ReadFile(id.AgeKeyFile)
		if err != nil {
			return nil, fmt.Errorf("reading identity file: %w", err)
		}
		defer zero(data)
		ident, err := age.ParseX25519Identity(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, fmt.Errorf("parsing identity file: %w", err)
		}
		identities = append(identities, ident)
	}
	if len(identities) == 0 {
		return nil, ErrWrongPassphrase
	}
	dec, err := age.Decrypt(r, identities...)
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	data, err := io.ReadAll(dec)
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	defer zero(data)
	km := &KeyMaterial{}
	if err := km.UnmarshalJSON(data); err != nil {
		return nil, ErrWrongPassphrase
	}
	if err := km.Validate(); err != nil {
		return nil, ErrWrongPassphrase
	}
	return km, nil
}
