package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/manprint/backimage/pkg/archive"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/recovery"
)

func newInspectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect IMAGE",
		Short: "show the public metadata of a backup image",
		Long: "show the public metadata of a backup image.\n\n" +
			"IMAGE is a reference such as ghcr.io/me/dumps:nightly-20260810T031500Z,\n" +
			"or a local source when --local-repo/--oci-layout is used. Public\n" +
			"metadata (sizes, counts, compression, encryption) needs no passphrase;\n" +
			"--files reads the encrypted index and therefore does.\n\n" +
			"  backimage inspect ghcr.io/me/dumps:daily\n" +
			"  backimage inspect ghcr.io/me/dumps:daily --files --passphrase-file ./pass",
		Args: cobra.ExactArgs(1),
		RunE: runInspect,
	}
	addSourceFlags(cmd, false)
	cmd.Flags().Bool("files", false, "also list archived files (decrypts the index: needs the passphrase or age identity)")
	cmd.Flags().Bool("layers", false, "show per-layer digest, size and chunk count")
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
	cmd := &cobra.Command{
		Use:   "ls IMAGE [PATH]",
		Short: "list the files archived inside a backup image",
		Long: "list the files archived inside a backup image.\n\n" +
			"PATH restricts the listing to one directory inside the archive (default:\n" +
			"everything). Reading the file list decrypts the index, so an encrypted\n" +
			"backup needs the passphrase or the age identity. No data layer is\n" +
			"downloaded.\n\n" +
			"  backimage ls ghcr.io/me/dumps:daily --passphrase-file ./pass\n" +
			"  backimage ls ghcr.io/me/dumps:daily var/log -l",
		Args: cobra.RangeArgs(1, 2),
		RunE: runLS,
	}
	addSourceFlags(cmd, false)
	cmd.Flags().BoolP("long", "l", false, "long listing: mode, owner, size and modification time")
	cmd.Flags().StringSlice("include", nil, "list only paths matching this glob, e.g. '**/*.pdf' (repeatable)")
	cmd.Flags().StringSlice("exclude", nil, "skip paths matching this glob (repeatable)")
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
	cmd := &cobra.Command{
		Use:   "find IMAGE PATTERN",
		Short: "search archived paths by glob pattern",
		Long: "search archived paths by glob pattern.\n\n" +
			"PATTERN is a shell-style glob matched against the whole archived path;\n" +
			"quote it so the local shell does not expand it first. Like `ls`, this\n" +
			"reads the index only and needs the passphrase for an encrypted backup.\n\n" +
			"  backimage find ghcr.io/me/dumps:daily '**/*.conf' --passphrase-file ./pass\n" +
			"  backimage find ghcr.io/me/dumps:daily 'etc/nginx/*' -l",
		Args: cobra.ExactArgs(2),
		RunE: runFind,
	}
	addSourceFlags(cmd, false)
	cmd.Flags().BoolP("long", "l", false, "long listing: mode, owner, size and modification time")
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
	cmd := &cobra.Command{
		Use:   "verify IMAGE",
		Short: "check that a backup image is complete and intact",
		Long: "check that a backup image is complete and intact.\n\n" +
			"By default every data layer is downloaded and each chunk digest is\n" +
			"recomputed, which costs the full backup size in network traffic; --quick\n" +
			"validates manifest, index and layer digests only. Run this before\n" +
			"trusting a backup for a restore. Exit code 5 means an integrity failure.\n\n" +
			"  backimage verify ghcr.io/me/dumps:daily --quick\n" +
			"  backimage verify ghcr.io/me/dumps:daily --passphrase-file ./pass --continue",
		Args: cobra.ExactArgs(1),
		RunE: runVerify,
	}
	addSourceFlags(cmd, false)
	cmd.Flags().Bool("quick", false, "check public metadata and layer digests only, without downloading the data layers")
	cmd.Flags().Bool("continue", false, "do not stop at the first integrity error: report them all")
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
	return &cobra.Command{
		Use:   "doctor [PATH...]",
		Short: "check privileges and runtime environment before a backup",
		Long: "check privileges and runtime environment before a backup.\n\n" +
			"With PATH arguments it reports whether those sources can be read whole\n" +
			"(ownership, xattrs, ACLs, sparse files); without arguments it checks the\n" +
			"environment only. Each unavailable capability comes with a remedy.\n\n" +
			"  backimage doctor\n" +
			"  sudo backimage doctor /srv/data /var/lib/postgresql",
		Args: cobra.ArbitraryArgs,
		RunE: runDoctor,
	}
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
