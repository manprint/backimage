// Package registry authenticates to and talks with OCI registries.
//
// It owns the layered credential keychain (explicit, backimage store,
// docker config, anonymous), the ephemeral bearer-token provider with
// proactive refresh, and the resumable parallel blob push (phase 05).
package registry
