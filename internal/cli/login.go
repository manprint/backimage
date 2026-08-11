package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/manprint/backimage/pkg/registry"
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
		Long: "store registry credentials for backimage.\n\n" +
			"REGISTRY is a host, e.g. ghcr.io or docker.io (default: docker.io).\n" +
			"Credentials are kept in backimage's own auth file, separate from\n" +
			"Docker's. Several accounts can be logged in on the same host: each\n" +
			"--username is stored separately and none overwrites another.\n\n" +
			"Which account is used is decided by the repository namespace:\n" +
			"docker.io/user2/img uses the login named user2. When the namespace\n" +
			"matches no account the command stops instead of guessing; pick one\n" +
			"with --registry-user NAME (or --registry-user " + registry.AnonymousUser + " for an\n" +
			"unauthenticated request).\n\n" +
			"On Docker Hub the password must be an access token, and a repository\n" +
			"must include the namespace: docker.io/USER/NAME.\n\n" +
			"  backimage login docker.io --username user1 --password-stdin < t1.txt\n" +
			"  backimage login docker.io --username user2 --password-stdin < t2.txt\n" +
			"  backimage login ghcr.io --username me --password-stdin < token.txt\n" +
			"  backimage login --list",
		Args: cobra.MaximumNArgs(1),
		RunE: runLogin,
	}
	cmd.Flags().StringP("username", "u", "", "registry username")
	cmd.Flags().StringP("password", "p", "", "password or token (visible in `ps`, prefer --password-stdin)")
	cmd.Flags().Bool("password-stdin", false, "read the password from stdin")
	cmd.Flags().String("token", "", "ready-made token (alternative to username/password)")
	cmd.Flags().Bool("list", false, "list the stored logins with provider, registry account and local owner (never the secrets)")
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
	accounts, err := store.Accounts()
	if err != nil {
		return New(KindGeneric, "", "reading credentials: %v", err)
	}
	names := accountNamesFor(accounts, host)
	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	if len(names) == 0 {
		return pr.Result(fmt.Sprintf("no login stored for %s", host))
	}

	user := strings.TrimSpace(getFlagString(cmd, "user"))
	all := getFlagBool(cmd, "all")
	if all && user != "" {
		return New(KindUsage, "", "--all and --user are mutually exclusive")
	}
	// Removing three Docker Hub logins because one was meant is not
	// recoverable, so ambiguity stops the command instead of guessing.
	if !all && user == "" && len(names) > 1 {
		return New(KindUsage, "", "%d logins for %s: %s; use --user NAME or --all",
			len(names), host, strings.Join(names, ", "))
	}
	if all || (user == "" && len(names) == 1 && names[0] == registry.TokenAccountName) {
		if err := store.Delete(host); err != nil {
			return New(KindGeneric, "", "removing credentials: %v", err)
		}
		return pr.Result(fmt.Sprintf("logged out of %s (%s)", host, strings.Join(names, ", ")))
	}
	if user == "" {
		user = names[0]
	}
	removed, err := store.DeleteFor(host, user)
	if err != nil {
		return New(KindGeneric, "", "removing credentials: %v", err)
	}
	if !removed {
		return New(KindUsage, "", "no login for %s as %q: stored accounts are %s", host, user, strings.Join(names, ", "))
	}
	return pr.Result(fmt.Sprintf("logged out of %s (%s)", host, user))
}

// accountNamesFor lists the account names of one host, with the public token
// selector for a host-wide token that carries no username.
func accountNamesFor(accounts []registry.Account, host string) []string {
	names := make([]string, 0, len(accounts))
	for _, a := range accounts {
		if a.Registry != host {
			continue
		}
		if a.Token {
			names = append(names, registry.TokenAccountName)
			continue
		}
		names = append(names, a.Username)
	}
	return names
}

// loginRow is one line of `login --list`: which provider, which account on
// that provider, and which local user owns the credential file. With several
// Docker Hub accounts the first two columns tell them apart, and the third
// explains why a sudo session sees a different set than the plain user.
type loginRow struct {
	Provider  string `json:"provider"`
	Account   string `json:"account"`
	Token     bool   `json:"token,omitempty"`
	LocalUser string `json:"localUser"`
	AuthFile  string `json:"authFile"`
}

// listLogins prints the configured logins, never the secrets.
func listLogins(cmd *cobra.Command, opts Options) error {
	path := authFilePath()
	store, err := registry.NewStore(path)
	if err != nil {
		return New(KindGeneric, err.Error(), "opening credential store")
	}
	accounts, err := store.Accounts()
	if err != nil {
		return New(KindGeneric, "", "listing credentials: %v", err)
	}
	owner := fileOwner(path)
	rows := make([]loginRow, 0, len(accounts))
	for _, a := range accounts {
		account := a.Username
		if a.Token {
			account = registry.TokenAccountName
		}
		rows = append(rows, loginRow{Provider: a.Registry, Account: account, Token: a.Token, LocalUser: owner, AuthFile: path})
	}
	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
	if opts.JSON {
		return pr.Result(rows)
	}
	if len(rows) == 0 {
		pr.Infof("nessun login salvato in %s", path)
		return nil
	}
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 2, 4, 3, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tACCOUNT\tLOGIN COME\tFILE")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Provider, r.Account, r.LocalUser, r.AuthFile)
	}
	return w.Flush()
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
	cmd := &cobra.Command{
		Use:   "logout [REGISTRY]",
		Short: "remove stored registry credentials",
		Long: "remove stored registry credentials.\n\n" +
			"REGISTRY is a host, e.g. ghcr.io (default: docker.io). Only backimage's\n" +
			"auth file is touched; Docker's credentials are left alone.\n\n" +
			"With one account on the host it is removed directly. With several the\n" +
			"command stops and lists them: pick one with --user NAME, or remove them\n" +
			"all with --all.\n\n" +
			"  backimage logout docker.io --user user2\n" +
			"  backimage logout docker.io --all",
		Args: cobra.MaximumNArgs(1),
		RunE: runLogout,
	}
	cmd.Flags().String("user", "", "account to remove when the registry holds several logins")
	cmd.Flags().Bool("all", false, "remove every account stored for the registry")
	return cmd
}
