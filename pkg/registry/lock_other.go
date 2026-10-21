//go:build (!unix && !windows) || aix

package registry

import "os"

// No advisory lock is available here: concurrent logins on this platform
// keep the old last-writer-wins behaviour.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }
