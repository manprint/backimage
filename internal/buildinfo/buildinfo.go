// Package buildinfo carries build-time metadata injected via -ldflags.
package buildinfo

import (
	"fmt"
	"runtime"
)

// Populated at build time via -ldflags -X.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a single-line human readable build identifier.
func String() string {
	return fmt.Sprintf("backimage %s (commit %s, built %s, %s/%s)",
		Version, Commit, Date, runtime.GOOS, runtime.GOARCH)
}

// UserAgent returns the HTTP User-Agent used for registry requests.
func UserAgent() string {
	return fmt.Sprintf("backimage/%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH)
}
