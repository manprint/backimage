package cli

import (
	"errors"
	"fmt"
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
		if ce.Kind >= KindGeneric && ce.Kind <= KindInterrupted {
			return int(ce.Kind)
		}
	}
	var usageErr interface{ UsageError() bool }
	if errors.As(err, &usageErr) && usageErr.UsageError() {
		return int(KindUsage)
	}
	if errors.Is(err, ErrPassphrase) {
		return int(KindPassphrase)
	}
	if errors.Is(err, ErrInterrupted) {
		return int(KindInterrupted)
	}
	if errors.Is(err, ErrIntegrity) {
		return int(KindIntegrity)
	}
	return int(KindGeneric)
}
