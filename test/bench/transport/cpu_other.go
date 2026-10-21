//go:build !unix

package main

import "time"

// processCPU is not measured off Unix: the benchmark reports 0 CPU seconds.
func processCPU() time.Duration { return 0 }
