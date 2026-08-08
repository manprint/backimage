// Package embedded exposes the self-extract binaries embedded at build time.
package embedded

import (
	"embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed backimage-selfextract-linux-amd64 backimage-selfextract-linux-arm64
var fs embed.FS

// ErrNotEmbedded is returned when the binary was built without the self-extract payload.
var ErrNotEmbedded = errors.New("self-extract binary not embedded in this build")

const placeholder = "PLACEHOLDER"

// SelfExtract returns the static self-extract binary for the given GOARCH
// ("amd64" or "arm64"). The returned slice must not be modified.
func SelfExtract(goarch string) ([]byte, error) {
	name := "backimage-selfextract-linux-" + goarch
	data, err := fs.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("self-extract binary for %s: %w", goarch, err)
	}
	if strings.HasPrefix(string(data), placeholder) {
		return nil, ErrNotEmbedded
	}
	return data, nil
}

// Architectures lists the GOARCH values available in this build.
func Architectures() []string {
	return []string{"amd64", "arm64"}
}
