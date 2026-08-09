package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fpierri/backimage/pkg/archive"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/recovery"
)

func newInspectCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "inspect IMAGE", Short: "show public backup metadata", Args: cobra.ExactArgs(1), RunE: runInspect}
	addSourceFlags(cmd, false)
	cmd.Flags().Bool("files", false, "also list archived files (requires credentials)")
	cmd.Flags().Bool("layers", false, "show data layer details")
	return cmd
}

type inspectResult struct {
	Reference string            `json:"reference"`
	Manifest  *index.Manifest   `json:"manifest"`
	Layers    []index.LayerInfo `json:"layers,omitempty"`
	Files     []index.FileEntry `json:"files,omitempty"`
}

func runInspect(cmd *cobra.Command, args []string) error {
	flags := readSourceFlags(cmd)
	s, err := openSourceForCLI(cmd.Context(), args[0], flags)
	if err != nil {
		return err
	}
	b, err := recovery.OpenBlobSource(cmd.Context(), s)
	if err != nil {
		s.Close()
		return err
	}
	defer b.Close()
	res := inspectResult{Reference: args[0], Manifest: b.Manifest}
	if getFlagBool(cmd, "layers") {
		res.Layers = b.Manifest.Layers
	}
	if getFlagBool(cmd, "files") {
		if err := unlockBackup(cmd.Context(), b, flags, true); err != nil {
			return err
		}
		idx, err := b.Index(cmd.Context())
		if err != nil {
			return err
		}
		res.Files = idx.Entries
	}
	if mustOptions(cmd).JSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
	}
	m := b.Manifest
	fmt.Fprintf(cmd.OutOrStdout(), "riferimento   %s\n", args[0])
	fmt.Fprintf(cmd.OutOrStdout(), "creato        %s\n", m.CreatedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(cmd.OutOrStdout(), "sorgenti      %s\n", strings.Join(m.Sources, ", "))
	fmt.Fprintf(cmd.OutOrStdout(), "contenuto     %d file, %d byte -> %d byte (%s:%d)\n", m.Totals.Files, m.Totals.BytesRaw, m.Totals.BytesStored, m.Archive.Compression, m.Archive.CompressionLevel)
	fmt.Fprintf(cmd.OutOrStdout(), "cifratura     %t (%s)\n", m.Encryption.Enabled, m.Encryption.AEAD)
	fmt.Fprintf(cmd.OutOrStdout(), "layer         %d dati + 1 metadati + 1 binario\n", len(m.Layers))
	if getFlagBool(cmd, "layers") {
		for _, layer := range m.Layers {
			fmt.Fprintf(cmd.OutOrStdout(), "%3d  %-71s %12d  %d-%d\n", layer.Index, layer.Digest, layer.StoredBytes, layer.ChunkFrom, layer.ChunkTo)
		}
	}
	if len(res.Files) > 0 {
		return index.WriteEntries(cmd.OutOrStdout(), res.Files, false, false)
	}
	return nil
}

func newLSCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls IMAGE [PATH]", Short: "list archived files", Args: cobra.RangeArgs(1, 2), RunE: runLS}
	addSourceFlags(cmd, false)
	cmd.Flags().BoolP("long", "l", false, "long listing")
	cmd.Flags().StringSlice("include", nil, "include glob (repeatable)")
	cmd.Flags().StringSlice("exclude", nil, "exclude glob (repeatable)")
	return cmd
}

func runLS(cmd *cobra.Command, args []string) error {
	flags := readSourceFlags(cmd)
	s, err := openSourceForCLI(cmd.Context(), args[0], flags)
	if err != nil {
		return err
	}
	b, err := recovery.OpenBlobSource(cmd.Context(), s)
	if err != nil {
		s.Close()
		return err
	}
	defer b.Close()
	if err := unlockBackup(cmd.Context(), b, flags, true); err != nil {
		return err
	}
	idx, err := b.Index(cmd.Context())
	if err != nil {
		return err
	}
	includes := getFlagStrings(cmd, "include")
	if len(args) == 2 {
		includes = append(includes, strings.TrimSuffix(args[1], "/"))
	}
	entries, err := index.EntriesMatching(idx, includes, getFlagStrings(cmd, "exclude"))
	if err != nil {
		return usageErrorf("%v", err)
	}
	return index.WriteEntries(cmd.OutOrStdout(), entries, getFlagBool(cmd, "long"), mustOptions(cmd).JSON)
}

func newFindCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "find IMAGE PATTERN", Short: "find archived paths by glob", Args: cobra.ExactArgs(2), RunE: runFind}
	addSourceFlags(cmd, false)
	cmd.Flags().BoolP("long", "l", false, "long listing")
	return cmd
}

func runFind(cmd *cobra.Command, args []string) error {
	flags := readSourceFlags(cmd)
	s, err := openSourceForCLI(cmd.Context(), args[0], flags)
	if err != nil {
		return err
	}
	b, err := recovery.OpenBlobSource(cmd.Context(), s)
	if err != nil {
		s.Close()
		return err
	}
	defer b.Close()
	if err := unlockBackup(cmd.Context(), b, flags, true); err != nil {
		return err
	}
	idx, err := b.Index(cmd.Context())
	if err != nil {
		return err
	}
	entries, err := index.EntriesMatching(idx, []string{args[1]}, nil)
	if err != nil {
		return usageErrorf("%v", err)
	}
	return index.WriteEntries(cmd.OutOrStdout(), entries, getFlagBool(cmd, "long"), mustOptions(cmd).JSON)
}

func newVerifyCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "verify IMAGE", Short: "verify a backup image", Args: cobra.ExactArgs(1), RunE: runVerify}
	addSourceFlags(cmd, false)
	cmd.Flags().Bool("quick", false, "validate public metadata without downloading data layers")
	cmd.Flags().Bool("continue", false, "report every integrity error")
	return cmd
}

func runVerify(cmd *cobra.Command, args []string) error {
	flags := readSourceFlags(cmd)
	s, err := openSourceForCLI(cmd.Context(), args[0], flags)
	if err != nil {
		return err
	}
	b, err := recovery.OpenBlobSource(cmd.Context(), s)
	if err != nil {
		s.Close()
		return err
	}
	defer b.Close()
	if getFlagBool(cmd, "quick") {
		res := map[string]any{"ok": true, "quick": true, "chunks": len(b.Chunks.Chunks), "layers": len(b.Manifest.Layers)}
		if mustOptions(cmd).JSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "ok: metadati coerenti (%d chunk, %d layer); i blob OCI sono content-addressed\n", len(b.Chunks.Chunks), len(b.Manifest.Layers))
		return nil
	}
	if err := unlockBackup(cmd.Context(), b, flags, true); err != nil {
		return err
	}
	res, verifyErr := b.Verify(cmd.Context(), true, getFlagBool(cmd, "continue"))
	if mustOptions(cmd).JSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(res); err != nil {
			return err
		}
	} else if verifyErr == nil {
		fmt.Fprintf(cmd.OutOrStdout(), "ok: backup integro (%d chunk, %d voci)\n", res.Chunks, res.Entries)
	} else {
		for _, message := range res.Errors {
			fmt.Fprintln(cmd.ErrOrStderr(), message)
		}
	}
	if verifyErr != nil {
		return &Error{Kind: KindIntegrity, Msg: "backup corrotto", Err: verifyErr}
	}
	return nil
}

type doctorCheck struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Required  bool   `json:"required"`
	Reason    string `json:"reason,omitempty"`
	Remedy    string `json:"remedy,omitempty"`
}

func newDoctorCommand() *cobra.Command {
	return &cobra.Command{Use: "doctor [PATH...]", Short: "check privileges and runtime environment", Args: cobra.ArbitraryArgs, RunE: runDoctor}
}

func runDoctor(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		args = []string{"."}
	}
	caps, err := archive.PreflightBackup(cmd.Context(), args)
	if err != nil {
		return err
	}
	checks := make([]doctorCheck, 0, len(caps)+3)
	failed := false
	for _, cap := range caps {
		checks = append(checks, doctorCheck{Name: cap.Name, Available: cap.Available, Required: true, Reason: cap.Reason, Remedy: cap.Remedy})
		if !cap.Available {
			failed = true
		}
	}
	_, dockerErr := os.Stat("/var/run/docker.sock")
	checks = append(checks, doctorCheck{Name: "docker", Available: dockerErr == nil, Required: false, Reason: optionalReason(dockerErr), Remedy: "start Docker; needed only for --local-repo"})
	checks = append(checks, writableCheck("temp-dir", os.TempDir()))
	cache, cacheErr := os.UserCacheDir()
	if cacheErr != nil {
		checks = append(checks, doctorCheck{Name: "cache-dir", Required: true, Reason: cacheErr.Error(), Remedy: "set XDG_CACHE_HOME to a writable directory"})
		failed = true
	} else {
		check := writableCheck("cache-dir", filepath.Join(cache, "backimage"))
		checks = append(checks, check)
		if !check.Available {
			failed = true
		}
	}
	if mustOptions(cmd).JSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(checks); err != nil {
			return err
		}
	} else {
		for _, check := range checks {
			mark := "✓"
			if !check.Available {
				mark = "✗"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %-22s %s\n", mark, check.Name, check.Reason)
			if !check.Available && check.Remedy != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  rimedio: %s\n", check.Remedy)
			}
		}
	}
	if failed {
		return &Error{Kind: KindPermission, Msg: "mancano privilegi necessari", Hint: "applica i rimedi mostrati e ripeti `backimage doctor`"}
	}
	return nil
}

func optionalReason(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writableCheck(name, dir string) doctorCheck {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return doctorCheck{Name: name, Required: true, Reason: err.Error(), Remedy: "choose a writable directory"}
	}
	f, err := os.CreateTemp(dir, ".backimage-doctor-*")
	if err != nil {
		return doctorCheck{Name: name, Required: true, Reason: err.Error(), Remedy: "choose a writable directory"}
	}
	path := f.Name()
	closeErr := f.Close()
	removeErr := os.Remove(path)
	if closeErr != nil {
		err = closeErr
	} else if removeErr != nil {
		err = removeErr
	}
	return doctorCheck{Name: name, Available: err == nil, Required: true, Reason: optionalReason(err), Remedy: "choose a writable directory"}
}
