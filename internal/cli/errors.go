package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"

	"github.com/manprint/backimage/pkg/backup"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/registry"
)

// Kind classifies an error for exit-code mapping and user messaging.
type Kind int

const (
	KindGeneric     Kind = 1
	KindUsage       Kind = 2
	KindPermission  Kind = 3
	KindPassphrase  Kind = 4
	KindIntegrity   Kind = 5
	KindNetwork     Kind = 6
	KindInterrupted Kind = 7
	// KindFidelity: the data came back whole but the destination refused
	// metadata the archive carried, and --strict was asked for. Distinct from
	// KindIntegrity on purpose: the backup is exactly what it claims to be,
	// it is the restored tree that is not a faithful copy.
	KindFidelity Kind = 8
)

// Error is a user-facing error carrying a Kind and an optional remediation hint.
type Error struct {
	Kind Kind
	Msg  string
	Hint string // actionable instruction shown to the user, may be empty
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a *Error.
func New(kind Kind, hint string, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...), Hint: hint}
}

// ExitCodeFor maps any error to a process exit code.
func ExitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var ce *Error
	if errors.As(err, &ce) {
		if ce.Kind >= KindGeneric && ce.Kind <= KindFidelity {
			return int(ce.Kind)
		}
	}
	var usageErr interface{ UsageError() bool }
	if errors.As(err, &usageErr) && usageErr.UsageError() {
		return int(KindUsage)
	}
	// The sentinels below are the fallback for a path that did not classify
	// its error by hand: the documented exit code must not depend on every
	// caller remembering to.
	if errors.Is(err, ErrPassphrase) || errors.Is(err, crypt.ErrPassphrase) || errors.Is(err, crypt.ErrWrongPassphrase) ||
		errors.Is(err, crypt.ErrNoPassphrase) || errors.Is(err, crypt.ErrEmptyPassphrase) {
		return int(KindPassphrase)
	}
	if errors.Is(err, ErrInterrupted) || errors.Is(err, context.Canceled) {
		return int(KindInterrupted)
	}
	// crypt.ErrIntegrity and index.ErrBadSchema are the two shapes a refused
	// blob arrives in: an authentication failure and a blob that is not the
	// authenticated envelope the backup claims. Both are integrity answers,
	// and reporting them as the generic exit 1 hides from a script the one
	// distinction that matters — the backup is not what it says it is.
	// A read-back that found the registry holding something else belongs
	// here too: the registry answered, and what it holds is not the backup.
	if errors.Is(err, ErrIntegrity) || errors.Is(err, crypt.ErrIntegrity) || errors.Is(err, index.ErrBadSchema) ||
		errors.Is(err, registry.ErrPublishedMismatch) {
		return int(KindIntegrity)
	}
	if errors.Is(err, backup.ErrPrivileges) || errors.Is(err, fs.ErrPermission) {
		return int(KindPermission)
	}
	if isNetworkError(err) {
		return int(KindNetwork)
	}
	return int(KindGeneric)
}

// isNetworkError reports a failure of the network, not of the local system.
// net.Error alone does not say that: syscall.Errno satisfies it too, and a
// missing file or a full disk would exit 6. errors.As stops at the outermost
// match, so an Errno the net package wrapped is still seen as the *OpError.
func isNetworkError(err error) bool {
	var netErr net.Error
	if !errors.As(err, &netErr) {
		return false
	}
	_, local := netErr.(syscall.Errno)
	return !local
}
