// Package cpu contains runtime CPU-budget helpers used by restore paths.
package cpu

import (
	"fmt"
	"runtime"
)

// Default returns half the available CPUs, with one CPU as the minimum.
func Default() int {
	n := runtime.NumCPU() / 2
	if n < 1 {
		return 1
	}
	return n
}

// Apply limits the Go scheduler for the duration of an operation and returns
// a function that restores the previous setting.
func Apply(n int) (restore func(), err error) {
	if n <= 0 {
		return nil, fmt.Errorf("cpus must be greater than zero")
	}
	previous := runtime.GOMAXPROCS(n)
	return func() { runtime.GOMAXPROCS(previous) }, nil
}
