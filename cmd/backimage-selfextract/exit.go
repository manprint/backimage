package main

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/fpierri/backimage/pkg/crypt"
)

// Keep these values aligned with internal/cli/errors.go without importing
// that cobra-bearing package into the size-constrained bootstrap binary.
const (
	exitGeneric = 1 + iota
	exitUsage
	exitPermission
	exitPassphrase
	exitIntegrity
	exitNetwork
	exitInterrupted
)

type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

func withCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

func usageErrorf(format string, args ...any) error {
	return withCode(exitUsage, fmt.Errorf(format, args...))
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	if errors.Is(err, crypt.ErrWrongPassphrase) || errors.Is(err, crypt.ErrNoPassphrase) || errors.Is(err, crypt.ErrEmptyPassphrase) {
		return exitPassphrase
	}
	if errors.Is(err, crypt.ErrIntegrity) {
		return exitIntegrity
	}
	if errors.Is(err, fs.ErrPermission) {
		return exitPermission
	}
	return exitGeneric
}
