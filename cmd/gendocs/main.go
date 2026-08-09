// Command gendocs generates the committed CLI reference from cobra.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/fpierri/backimage/internal/cli"
)

func main() {
	out := flag.String("out", "docs/cli.md", "output markdown file")
	flag.Parse()
	root := cli.NewRootCommand()
	root.DisableAutoGenTag = true
	var commands []*cobra.Command
	walk(root, &commands)
	sort.Slice(commands, func(i, j int) bool { return commands[i].CommandPath() < commands[j].CommandPath() })
	var all bytes.Buffer
	all.WriteString("# Riferimento CLI\n\n_File generato da `go run ./cmd/gendocs`; non modificare a mano._\n\n")
	for _, command := range commands {
		var one bytes.Buffer
		if err := doc.GenMarkdownCustom(command, &one, func(string) string { return "" }); err != nil {
			fatal(err)
		}
		all.Write(one.Bytes())
		all.WriteByte('\n')
	}
	if err := os.WriteFile(*out, all.Bytes(), 0o644); err != nil {
		fatal(err)
	}
}

func walk(command *cobra.Command, out *[]*cobra.Command) {
	*out = append(*out, command)
	for _, child := range command.Commands() {
		if child.Hidden {
			continue
		}
		walk(child, out)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
