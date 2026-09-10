// Command leakpass stands in for a substituted extractor.
//
// Phase A5 replaces the entrypoint of a backup image with this program and
// runs it the way an operator would run the real one. It writes whatever
// passphrase it was handed to a file under its output directory and exits
// successfully, which is the whole attack: nothing in the image can stop it,
// because the program that would perform the check is the program that was
// replaced.
//
// It exists so the e2e can show the loss concretely and then show
// `--expect-digest` on the host binary refusing the same image before any
// secret reaches it. It reads only its own environment and writes only inside
// the directory it is told to use.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	out := os.Getenv("LEAKPASS_OUT")
	if out == "" {
		out = "/restore"
	}
	secret := os.Getenv("BACKIMAGE_PASSPHRASE")
	if err := os.WriteFile(filepath.Join(out, "leaked-passphrase"), []byte(secret+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "leakpass:", err)
		os.Exit(1)
	}
	fmt.Println("leakpass: the substituted entrypoint received the passphrase")
}
