package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
)

// A tampered backup and a mistyped passphrase both fail the unlock, and until
// A01 both were reported as "passphrase errata" with exit 4. The distinction
// is the whole point of the downgrade guard: a script that sees 4 retries with
// another credential, one that sees 5 knows the image is not what it claims.
func TestUnlockErrorSeparatesTamperingFromACredential(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Kind
	}{
		{"unauthenticated private blob", fmt.Errorf("opening private metadata: %w",
			fmt.Errorf("%w: unauthenticated blob in an encrypted backup", crypt.ErrIntegrity)), KindIntegrity},
		{"private blob is not an envelope", fmt.Errorf("reading private metadata: %w", index.ErrBadSchema), KindIntegrity},
		{"wrong passphrase", crypt.ErrWrongPassphrase, KindPassphrase},
		{"unrelated failure", errors.New("connection reset"), KindPassphrase},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unlockError("passphrase errata", tc.err)
			if got.Kind != tc.want {
				t.Fatalf("Kind = %d, want %d (%v)", got.Kind, tc.want, got)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("the cause was dropped: %v", got)
			}
			if ExitCodeFor(got) != int(tc.want) {
				t.Fatalf("exit code = %d, want %d", ExitCodeFor(got), tc.want)
			}
		})
	}
}

// The exit code has to hold for a bare sentinel too: not every refusal travels
// inside a *Error, and a plain crypt.ErrIntegrity used to exit 1.
func TestExitCodeForRawIntegritySentinels(t *testing.T) {
	for _, err := range []error{
		crypt.ErrIntegrity,
		fmt.Errorf("chunk 3: %w", crypt.ErrIntegrity),
		index.ErrBadSchema,
		fmt.Errorf("index: %w", index.ErrBadSchema),
	} {
		if got := ExitCodeFor(err); got != int(KindIntegrity) {
			t.Fatalf("ExitCodeFor(%v) = %d, want %d", err, got, KindIntegrity)
		}
	}
}
