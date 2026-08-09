package cpu

import (
	"runtime"
	"testing"
)

func TestDefaultIsHalfWithMinimumOne(t *testing.T) {
	want := runtime.NumCPU() / 2
	if want < 1 {
		want = 1
	}
	if got := Default(); got != want {
		t.Fatalf("Default() = %d, want %d", got, want)
	}
}

func TestApplyRejectsNonPositive(t *testing.T) {
	if _, err := Apply(0); err == nil {
		t.Fatal("Apply(0) unexpectedly succeeded")
	}
	if _, err := Apply(-1); err == nil {
		t.Fatal("Apply(-1) unexpectedly succeeded")
	}
}

func TestApplyRestoresPreviousLimit(t *testing.T) {
	previous := runtime.GOMAXPROCS(0)
	restore, err := Apply(1)
	if err != nil {
		t.Fatal(err)
	}
	if got := runtime.GOMAXPROCS(0); got != 1 {
		t.Fatalf("GOMAXPROCS after Apply = %d, want 1", got)
	}
	restore()
	if got := runtime.GOMAXPROCS(0); got != previous {
		t.Fatalf("GOMAXPROCS after restore = %d, want %d", got, previous)
	}
}
