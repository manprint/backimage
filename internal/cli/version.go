package cli

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/manprint/backimage/internal/buildinfo"
	"github.com/spf13/cobra"
)

type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	GOOS    string `json:"goos"`
	GOARCH  string `json:"goarch"`
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := parseOptions(cmd.Root())
			if err != nil {
				return err
			}
			v := versionInfo{
				Version: buildinfo.Version,
				Commit:  buildinfo.Commit,
				Date:    buildinfo.Date,
				Go:      runtime.Version(),
				GOOS:    runtime.GOOS,
				GOARCH:  runtime.GOARCH,
			}
			if opts.JSON {
				out, err := json.MarshalIndent(v, "", "  ")
				if err != nil {
					return fmt.Errorf("version json: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(out))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backimage version %s\n", v.Version)
			fmt.Fprintf(cmd.OutOrStdout(), "commit   %s\n", v.Commit)
			fmt.Fprintf(cmd.OutOrStdout(), "date     %s\n", v.Date)
			fmt.Fprintf(cmd.OutOrStdout(), "go       %s\n", v.Go)
			fmt.Fprintf(cmd.OutOrStdout(), "platform %s/%s\n", v.GOOS, v.GOARCH)
			return nil
		},
	}
}
