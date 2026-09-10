package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
)

// The extractor is the other entry point into the same backup: a diagnosis
// that holds on one executable and not on the other is not a diagnosis.
func TestUnlockErrorMatchesTheHostClassification(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantText string
	}{
		{"unauthenticated private blob",
			fmt.Errorf("%w: unauthenticated blob in an encrypted backup", crypt.ErrIntegrity),
			exitIntegrity, "non autenticati"},
		{"private blob is not an envelope", index.ErrBadSchema, exitIntegrity, "non autenticati"},
		{"wrong passphrase", crypt.ErrWrongPassphrase, exitPassphrase, "passphrase errata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unlockError(tc.err)
			if code := exitCode(got); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (%v)", code, tc.wantCode, got)
			}
			if !strings.Contains(got.Error(), tc.wantText) {
				t.Fatalf("message %q does not mention %q", got.Error(), tc.wantText)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("the cause was dropped: %v", got)
			}
		})
	}
}
