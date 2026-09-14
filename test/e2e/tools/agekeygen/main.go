// Command agekeygen writes one X25519 age identity file and prints its public
// key, so an e2e phase can exercise the recipient form of the encryption
// without depending on the age-keygen binary being installed.
//
// The file has the exact shape `age-keygen -o FILE` produces — two comment
// lines, then the secret key — because that shape is the point: a file a user
// actually has, not one somebody hand-trimmed. It is created 0600: it is a
// private key, even a throwaway one inside a temporary directory.
package main

import (
	"fmt"
	"os"
	"time"

	"filippo.io/age"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: agekeygen IDENTITY_FILE")
		os.Exit(2)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agekeygen:", err)
		os.Exit(1)
	}
	content := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		time.Now().UTC().Format(time.RFC3339), id.Recipient(), id)
	if err := os.WriteFile(os.Args[1], []byte(content), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "agekeygen:", err)
		os.Exit(1)
	}
	fmt.Println(id.Recipient())
}
