package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fpierri/backimage/pkg/registry"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// defaultRegistryHost is the canonical key for Docker Hub.
const defaultRegistryHost = "index.docker.io"

// authFilePath returns the credential store path; overridable through the
// BACKIMAGE_AUTH_FILE environment variable (used by the e2e and tests).
func authFilePath() string {
	if p := os.Getenv("BACKIMAGE_AUTH_FILE"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "backimage", "auth.json")
}

func newLoginCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login [REGISTRY]",
		Short: "store registry credentials for backimage",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runLogin,
	}
	cmd.Flags().StringP("username", "u", "", "registry username")
	cmd.Flags().StringP("password", "p", "", "password or token (visible in `ps`, prefer --password-stdin)")
	cmd.Flags().Bool("password-stdin", false, "read the password from stdin")
	cmd.Flags().String("token", "", "ready-made token (alternative to username/password)")
	cmd.Flags().Bool("list", false, "list configured registries (never the secrets)")
	return cmd
}

func runLogin(cmd *cobra.Command, args []string) error {
	opts, err := parseOptions(cmd.Root())
	if err != nil {
		return err
	}
	if list, lerr := cmd.Flags().GetBool("list"); lerr == nil && list {
		if len(args) != 0 || cmd.Flags().Changed("username") || cmd.Flags().Changed("password") ||
			cmd.Flags().Changed("password-stdin") || cmd.Flags().Changed("token") {
			return New(KindUsage, "", "--list cannot be combined with a registry or credentials")
		}
		return listLogins(cmd, opts)
	}

	host := defaultRegistryHost
	if len(args) == 1 {
		host = args[0]
	}
	host = registry.CanonicalHost(host)
	username, uerr := cmd.Flags().GetString("username")
	if uerr != nil {
		return New(KindGeneric, "", "username flag: %v", uerr)
	}
	password, perr := cmd.Flags().GetString("password")
	if perr != nil {
		return New(KindGeneric, "", "password flag: %v", perr)
	}
	passwordStdin, serr := cmd.Flags().GetBool("password-stdin")
	if serr != nil {
		return New(KindGeneric, "", "password-stdin flag: %v", serr)
	}
	token, terr := cmd.Flags().GetString("token")
	if terr != nil {
		return New(KindGeneric, "", "token flag: %v", terr)
	}

	if token != "" && (username != "" || password != "" || passwordStdin) {
		return New(KindUsage, "", "--token is an alternative to --username/--password")
	}

	isToken := token != ""
	switch {
	case isToken:
		password = token
	case passwordStdin:
		if password != "" {
			return New(KindUsage, "", "cannot combine --password and --password-stdin")
		}
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return New(KindGeneric, "", "reading stdin: %v", err)
		}
		password = strings.TrimRight(string(b), "\n")
	case password != "":
		cmd.PrintErr("warning: the password is visible in the process list: prefer --password-stdin\n")
	case term.IsTerminal(int(os.Stdin.Fd())):
		if username == "" {
			username = strings.TrimSpace(promptOnTTY("username"))
		}
		pw := promptOnTTY("password")
		password = pw
	case username == "":
		return New(KindUsage, "", "provide --username and --password-stdin (or --token) for %s", host)
	case password == "":
		return New(KindUsage, "", "provide --password-stdin (or --password) for %s", host)
	}

	store, err := registry.NewStore(authFilePath())
	if err != nil {
		return New(KindPermission, err.Error(), "opening credential store")
	}
	creds := registry.Credentials{Registry: host, Username: username, Secret: password}
	if isToken {
		creds = registry.TokenCredentials(host, token)
	}

	printer := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	if err := registry.VerifyCredentials(cmd.Context(), host, creds); err != nil {
		return New(KindNetwork, "check the username and password; the entry was not saved", "%v", err)
	}
	if err := store.Put(creds); err != nil {
		return New(KindGeneric, "", "saving credentials: %v", err)
	}
	return printer.Result(fmt.Sprintf("login succeeded for %s", host))
}

func runLogout(cmd *cobra.Command, args []string) error {
	opts, err := parseOptions(cmd.Root())
	if err != nil {
		return err
	}
	if len(args) > 1 {
		return New(KindUsage, "", "logout takes at most one registry")
	}
	host := defaultRegistryHost
	if len(args) == 1 {
		host = args[0]
	}
	host = registry.CanonicalHost(host)
	store, err := registry.NewStore(authFilePath())
	if err != nil {
		return New(KindGeneric, err.Error(), "opening credential store")
	}
	if err := store.Delete(host); err != nil {
		return New(KindGeneric, "", "removing credentials: %v", err)
	}
	return NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts).Result(fmt.Sprintf("logged out of %s", host))
}

// listLogins prints the configured registries, never the secrets.
func listLogins(cmd *cobra.Command, opts Options) error {
	store, err := registry.NewStore(authFilePath())
	if err != nil {
		return New(KindGeneric, err.Error(), "opening credential store")
	}
	hosts, err := store.List()
	if err != nil {
		return New(KindGeneric, "", "listing credentials: %v", err)
	}
	if opts.JSON {
		return NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts).Result(hosts)
	}
	for _, h := range hosts {
		fmt.Fprintln(cmd.OutOrStdout(), h)
	}
	return nil
}

func promptOnTTY(label string) string {
	fmt.Fprint(os.Stderr, label+": ")
	b, rerr := term.ReadPassword(int(os.Stdin.Fd()))
	if rerr != nil {
		fmt.Fprintln(os.Stderr)
		return ""
	}
	fmt.Fprintln(os.Stderr)
	return string(b)
}

func newLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout [REGISTRY]",
		Short: "remove registry credentials",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runLogout,
	}
}
