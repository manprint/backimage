// Package cli wires the command tree, flag handling, output and exit codes.
//
// Exit codes: 0 ok, 1 generic, 2 usage, 3 insufficient privileges,
// 4 wrong passphrase, 5 integrity failure, 6 network/registry failure,
// 7 interrupted operation. All diagnostics go to stderr; stdout carries
// only the requested result (or JSON when --json is set).
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// countVerbose counts -v/--verbose occurrences before cobra parses flags,
// so the logger can be built with the right level.
func countVerbose(args []string) int {
	n := 0
	for _, a := range args {
		switch {
		case a == "-v" || a == "--verbose":
			n++
		case strings.HasPrefix(a, "--verbose="):
			n++
		}
	}
	if n > 2 {
		n = 2
	}
	return n
}

// Execute builds the root command and runs it with the given arguments.
func Execute(ctx context.Context, args []string) error {
	root := NewRootCommand()
	root.SetArgs(args)
	root.SetContext(WithLogger(ctx, NewLoggerFor(os.Stderr, countVerbose(args))))
	err := root.ExecuteContext(ctx)
	return classify(err)
}

// classify maps cobra-level usage errors (unknown command/flag) to KindUsage.
func classify(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// Flag-level mistakes are usage errors (exit 2), including the ones cobra
	// and pflag report before RunE is reached.
	for _, usage := range []string{
		"unknown command", "unknown flag", "unknown shorthand flag",
		"invalid argument", "flag needs an argument", "required flag",
		"accepts", "requires at least", "requires at most",
	} {
		if strings.Contains(msg, usage) {
			return &Error{Kind: KindUsage, Msg: msg}
		}
	}
	return err
}

// NewRootCommand assembles the whole command tree. Every subcommand
// registers itself here; there is no init()-based registration.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "backimage",
		Short: "store backups inside runnable, encrypted OCI images",
		Long: "backimage archives, compresses, encrypts and stores your files " +
			"inside a multi-arch OCI image that can be restored with plain docker run.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().Bool("json", false, "structured JSON output on stdout")
	root.PersistentFlags().BoolP("quiet", "q", false, "suppress progress output")
	root.PersistentFlags().CountP("verbose", "v", "log verbosity (repeat: -v debug, -vv trace)")
	root.PersistentFlags().Bool("no-color", false, "disable ANSI colors (auto-detected)")
	root.PersistentFlags().String("config", "", "config file (default $XDG_CONFIG_HOME/backimage/config.yaml)")

	root.AddCommand(
		newVersionCommand(),
		newLoginCommand(),
		newBackupCommand(),
		newLogoutCommand(),
		newRestoreCommand(),
		newInspectCommand(),
		newLSCommand(),
		newFindCommand(),
		newVerifyCommand(),
		newDoctorCommand(),
		newRepoCommand(),
		newListenRemoteCommand(),
	)

	return root
}

// Options carries the resolved global flags used by output helpers.
type Options struct {
	JSON    bool
	Quiet   bool
	Verbose int
	NoColor bool
	RootCtx context.Context
}

// parseOptions extracts global options from persistent flags of root.
func parseOptions(root *cobra.Command) (Options, error) {
	json, err := root.PersistentFlags().GetBool("json")
	if err != nil {
		return Options{}, fmt.Errorf("json flag: %w", err)
	}
	quiet, err := root.PersistentFlags().GetBool("quiet")
	if err != nil {
		return Options{}, fmt.Errorf("quiet flag: %w", err)
	}
	verbose, err := root.PersistentFlags().GetCount("verbose")
	if err != nil {
		return Options{}, fmt.Errorf("verbose flag: %w", err)
	}
	noColor, err := root.PersistentFlags().GetBool("no-color")
	if err != nil {
		return Options{}, fmt.Errorf("no-color flag: %w", err)
	}
	return Options{JSON: json, Quiet: quiet, Verbose: verbose, NoColor: noColor}, nil
}
