package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// envPrefix namespaces the environment variables that configure a command.
const envPrefix = "BACKIMAGE_"

// EnvVarFor returns the environment variable that configures a flag:
// --bind-address becomes BACKIMAGE_BIND_ADDRESS.
func EnvVarFor(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// applyEnvDefaults fills the flags of cmd that the command line left untouched
// from the environment. An explicit flag always wins, so a container image can
// be configured entirely through env vars while still allowing an override on
// the command line. Repeatable flags accept a comma-separated list.
func applyEnvDefaults(cmd *cobra.Command) error {
	var errs []error
	apply := func(f *pflag.Flag) {
		if f.Changed {
			return
		}
		raw, ok := os.LookupEnv(EnvVarFor(f.Name))
		if !ok {
			return
		}
		value := strings.TrimSpace(raw)
		if value == "" {
			// An empty variable means "unset": compose files often declare a
			// variable with no value as documentation.
			return
		}
		if err := f.Value.Set(value); err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: %w", EnvVarFor(f.Name), value, err))
			return
		}
		f.Changed = true
	}
	// cmd.Flags excludes persistent flags inherited from parents. Visit both
	// sets so root options such as --json and --quiet also honor BACKIMAGE_*.
	cmd.InheritedFlags().VisitAll(apply)
	cmd.Flags().VisitAll(apply)
	return errors.Join(errs...)
}
