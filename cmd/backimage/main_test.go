package main

import (
	"os"
	"strings"
	"testing"
)

// helperArgsEnv makes the test binary run main() with the arguments it
// holds, separated by \x1f, so a test can drive the real process: its signal
// handling and its exit status.
const helperArgsEnv = "BACKIMAGE_TEST_MAIN"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(helperArgsEnv); ok {
		os.Args = append([]string{"backimage"}, strings.Split(args, "\x1f")...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
