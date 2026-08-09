// Package docker contains the small subset of Docker Engine operations that
// backimage needs when explicitly managing local images.
package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/client"
)

// RemoveLocalImage removes a local image through the Docker Engine API.
// Force is intentional: the self-extractor may still be running from the
// image being removed.
func RemoveLocalImage(ctx context.Context, ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("image reference is empty")
	}

	api, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("create Docker client: %w", err)
	}
	defer api.Close()

	if _, err := api.ImageRemove(ctx, ref, client.ImageRemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove Docker image %q: %w", ref, err)
	}
	return nil
}
