package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/fpierri/backimage/pkg/archive"
	"github.com/fpierri/backimage/pkg/cpu"
	"github.com/fpierri/backimage/pkg/crypt"
	dockerd "github.com/fpierri/backimage/pkg/docker"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/progress"
	"github.com/fpierri/backimage/pkg/recovery"
	"github.com/fpierri/backimage/pkg/registry"
	restorepkg "github.com/fpierri/backimage/pkg/restore"
)

type sourceFlags struct {
	repo            string
	localRepo       bool
	ociLayout       string
	platform        string
	cacheSize       string
	passphraseFile  string
	passphraseStdin bool
	identity        string
}

var (
	fromRegistryCLI   = restorepkg.FromRegistry
	fromLayoutCLI     = restorepkg.FromOCILayout
	fromDaemonCLI     = restorepkg.FromDaemon
	removeDockerImage = dockerd.RemoveLocalImage
)

func addSourceFlags(f *cobra.Command, repoAlias bool) {
	flags := f.Flags()
	if repoAlias {
		flags.String("repo", "", "image reference (alias for positional IMAGE)")
	}
	flags.Bool("local-repo", false, "read the image from the local Docker daemon")
	flags.String("oci-layout", "", "read from a local OCI layout directory")
	flags.String("platform", "linux/amd64", "source platform")
	flags.String("cache-size", "2GiB", "maximum downloaded-layer cache size")
	flags.String("passphrase-file", "", "read passphrase from a file")
	flags.Bool("passphrase-stdin", false, "read passphrase from stdin")
	flags.String("identity", "", "age private key file")
}

func readSourceFlags(cmd *cobra.Command) sourceFlags {
	f := sourceFlags{
		localRepo: getFlagBool(cmd, "local-repo"), ociLayout: getFlagString(cmd, "oci-layout"),
		platform: getFlagString(cmd, "platform"), cacheSize: getFlagString(cmd, "cache-size"),
		passphraseFile: getFlagString(cmd, "passphrase-file"), passphraseStdin: getFlagBool(cmd, "passphrase-stdin"),
		identity: getFlagString(cmd, "identity"),
	}
	if cmd.Flags().Lookup("repo") != nil {
		f.repo = getFlagString(cmd, "repo")
	}
	return f
}

func resolveReference(args []string, alias string) (string, error) {
	if alias != "" && len(args) > 0 {
		return "", usageErrorf("IMAGE posizionale e --repo sono mutuamente esclusivi")
	}
	if alias != "" {
		return alias, nil
	}
	if len(args) == 0 {
		return "", usageErrorf("IMAGE è obbligatoria")
	}
	return args[0], nil
}

func openImageSource(ctx context.Context, refText string, flags sourceFlags) (restorepkg.Source, error) {
	if flags.localRepo && flags.ociLayout != "" {
		return nil, usageErrorf("--local-repo e --oci-layout sono mutuamente esclusivi")
	}
	ref, err := name.ParseReference(refText)
	if err != nil {
		return nil, usageErrorf("reference %q non valida: %v", refText, err)
	}
	if flags.localRepo {
		s, err := fromDaemonCLI(ctx, ref)
		if err != nil {
			return nil, &Error{Kind: KindNetwork, Msg: "lettura daemon fallita", Err: err}
		}
		return s, nil
	}
	if flags.ociLayout != "" {
		return fromLayoutCLI(flags.ociLayout, refText)
	}
	cacheBytes, err := parseSize(flags.cacheSize)
	if err != nil {
		return nil, usageErrorf("--cache-size: %v", err)
	}
	store, err := registry.NewStore(authFilePath())
	if err != nil {
		return nil, err
	}
	s, err := fromRegistryCLI(ctx, ref, registry.NewKeychain(nil, store), restorepkg.SourceOptions{
		Platform: flags.platform, CacheSize: cacheBytes,
	})
	if err != nil {
		return nil, &Error{Kind: KindNetwork, Msg: "lettura registry fallita", Err: err}
	}
	return s, nil
}

var openSourceForCLI = openImageSource

func unlockBackup(ctx context.Context, b *recovery.Backup, flags sourceFlags, required bool) error {
	if !b.Manifest.Encryption.Enabled {
		return nil
	}
	if flags.identity != "" {
		if err := b.Unlock(ctx, crypt.Identity{AgeKeyFile: flags.identity}); err != nil {
			return &Error{Kind: KindPassphrase, Msg: "identità age non valida", Err: err}
		}
		return nil
	}
	hasEnv := false
	if _, ok := os.LookupEnv("BACKIMAGE_PASSPHRASE"); ok {
		hasEnv = true
	}
	if !required && flags.passphraseFile == "" && !flags.passphraseStdin && !hasEnv {
		return nil
	}
	pass, err := crypt.ReadPassphrase(crypt.PassphraseSource{
		File: flags.passphraseFile, Stdin: flags.passphraseStdin,
		EnvVar: "BACKIMAGE_PASSPHRASE", Prompt: required,
	})
	if err != nil {
		return &Error{Kind: KindPassphrase, Msg: "passphrase richiesta", Err: err}
	}
	defer wipeBytes(pass)
	if err := b.Unlock(ctx, crypt.Identity{Passphrase: pass}); err != nil {
		return &Error{Kind: KindPassphrase, Msg: "passphrase errata", Err: err}
	}
	return nil
}

func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func newRestoreCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "restore [IMAGE]", Short: "restore a backup image to tar or disk", Args: cobra.MaximumNArgs(1), RunE: runRestore}
	addSourceFlags(cmd, true)
	f := cmd.Flags()
	f.BoolP("extract", "x", false, "extract instead of writing a tar")
	f.StringP("destination", "C", ".", "destination directory")
	f.StringP("output", "o", "", "tar filename (- for stdout)")
	f.StringSlice("include", nil, "include glob (repeatable)")
	f.StringSlice("exclude", nil, "exclude glob (repeatable)")
	f.Int("strip-components", 0, "remove leading path components")
	f.Int("cpus", cpu.Default(), "maximum CPUs used during restore (default: half available CPUs)")
	f.Bool("no-preserve-owner", false, "do not preserve ownership")
	f.Bool("remove-local-image", false, "remove the local Docker image after a successful restore")
	f.Bool("overwrite", false, "replace an existing output")
	f.Bool("no-verify", false, "skip plaintext chunk digest verification")
	f.Int("jobs", 3, "parallel layer downloads")
	return cmd
}

func runRestore(cmd *cobra.Command, args []string) error {
	started := time.Now()
	flags := readSourceFlags(cmd)
	refText, err := resolveReference(args, flags.repo)
	if err != nil {
		return err
	}
	if getFlagInt(cmd, "strip-components") < 0 {
		return usageErrorf("--strip-components non può essere negativo")
	}
	restoreCPUs, err := cpu.Apply(getFlagInt(cmd, "cpus"))
	if err != nil {
		return usageErrorf("--cpus: %v", err)
	}
	defer restoreCPUs()
	ctx := cmd.Context()
	source, err := openSourceForCLI(ctx, refText, flags)
	if err != nil {
		return err
	}
	b, err := recovery.OpenBlobSource(ctx, source)
	if err != nil {
		source.Close()
		return err
	}
	defer b.Close()
	if err := unlockBackup(ctx, b, flags, true); err != nil {
		return err
	}

	includes, excludes := getFlagStrings(cmd, "include"), getFlagStrings(cmd, "exclude")
	var idx *index.Index
	var selected []index.FileEntry
	if len(includes) > 0 || len(excludes) > 0 {
		idx, err = b.Index(ctx)
		if err != nil {
			return err
		}
		selected, err = index.EntriesMatching(idx, includes, excludes)
		if err != nil {
			return usageErrorf("%v", err)
		}
		if len(selected) == 0 {
			return usageErrorf("nessuna voce selezionata su %d; usa `backimage ls %s`", len(idx.Entries), refText)
		}
	}
	stream := func(w io.Writer) error {
		if idx != nil {
			return b.StreamSelectedTar(ctx, idx, selected, w, !getFlagBool(cmd, "no-verify"))
		}
		return b.StreamTar(ctx, w, !getFlagBool(cmd, "no-verify"))
	}
	if getFlagBool(cmd, "extract") {
		total := b.Manifest.Totals.BytesRaw
		if idx != nil {
			total = selectedBytes(selected)
		}
		err = restoreExtract(cmd, stream, idx != nil, restoreProgress(cmd, total))
	} else {
		err = restoreTar(cmd, refText, stream)
	}
	if err != nil {
		if errors.Is(err, crypt.ErrIntegrity) {
			return &Error{Kind: KindIntegrity, Msg: "verifica restore fallita", Err: err}
		}
		return err
	}
	imageRemoved := false
	if getFlagBool(cmd, "remove-local-image") {
		if err := removeDockerImage(ctx, refText); err != nil {
			return &Error{Kind: KindNetwork, Msg: "rimozione immagine locale fallita", Err: err}
		}
		imageRemoved = true
	}
	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), mustOptions(cmd))
	if mustOptions(cmd).JSON {
		return printerResult(pr, map[string]any{"ok": true, "reference": refText, "extract": getFlagBool(cmd, "extract"), "remove_local_image": imageRemoved, "duration": time.Since(started).String()})
	}
	pr.Infof("restore completato in %s", time.Since(started).Round(time.Millisecond))
	return nil
}

func restoreTar(cmd *cobra.Command, refText string, stream func(io.Writer) error) error {
	out := getFlagString(cmd, "output")
	dest := getFlagString(cmd, "destination")
	if out == "" {
		out = defaultTarName(refText)
	}
	if out == "-" {
		if mustOptions(cmd).JSON {
			return usageErrorf("--json non è coerente con --output -")
		}
		if f, ok := cmd.OutOrStdout().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			return usageErrorf("il restore tar scrive dati binari: reindirizza stdout")
		}
		return stream(cmd.OutOrStdout())
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dest, out)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if getFlagBool(cmd, "overwrite") {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(out, flags, 0o600)
	if err != nil {
		return err
	}
	if err := stream(f); err != nil {
		f.Close()
		os.Remove(out)
		return err
	}
	return f.Close()
}

func restoreExtract(cmd *cobra.Command, stream func(io.Writer) error, alreadyFiltered bool, report func(int64)) error {
	dest := getFlagString(cmd, "destination")
	if dest == "" {
		return usageErrorf("--destination non può essere vuota")
	}
	if !getFlagBool(cmd, "overwrite") {
		if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
			return usageErrorf("destinazione %s non vuota; usa --overwrite", dest)
		}
	}
	if !getFlagBool(cmd, "no-preserve-owner") {
		caps, err := archive.PreflightRestore(cmd.Context(), dest)
		if err != nil {
			return err
		}
		for _, cap := range caps {
			if !cap.Available {
				return &Error{Kind: KindPermission, Msg: cap.Reason, Hint: cap.Remedy}
			}
		}
	}
	includes, excludes := getFlagStrings(cmd, "include"), getFlagStrings(cmd, "exclude")
	if alreadyFiltered {
		includes, excludes = nil, nil
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { err := stream(pw); _ = pw.CloseWithError(err); done <- err }()
	if report != nil {
		report(0)
	}
	progressReader := progress.NewReader(pr, report)
	x := archive.NewExtractor(archive.ExtractOptions{
		PreserveOwner: !getFlagBool(cmd, "no-preserve-owner"), PreserveXattrs: true,
		Overwrite: getFlagBool(cmd, "overwrite"), Includes: includes, Excludes: excludes,
		StripComponents: getFlagInt(cmd, "strip-components"), Strict: true,
	})
	_, extractErr := x.Extract(cmd.Context(), progressReader, dest)
	if extractErr != nil {
		_ = pr.CloseWithError(extractErr)
	}
	streamErr := <-done
	if extractErr != nil {
		return extractErr
	}
	return streamErr
}

func selectedBytes(entries []index.FileEntry) int64 {
	var total int64
	for _, entry := range entries {
		if entry.Size > 0 {
			total += entry.Size
		}
	}
	return total
}

func restoreProgress(cmd *cobra.Command, total int64) func(int64) {
	if mustOptions(cmd).Quiet {
		return nil
	}
	return func(done int64) {
		fmt.Fprintln(cmd.ErrOrStderr(), progress.Message("restore", done, total))
	}
}

func defaultTarName(refText string) string {
	ref, err := name.ParseReference(refText)
	if err != nil {
		return "backup.tar"
	}
	repo := ref.Context().RepositoryStr()
	segment := filepath.Base(repo)
	tag := "latest"
	if t, ok := ref.(name.Tag); ok {
		tag = t.TagStr()
	}
	return sanitizeName(segment+"_"+tag) + ".tar"
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

func mustOptions(cmd *cobra.Command) Options {
	o, err := parseOptions(cmd.Root())
	if err != nil {
		panic(err)
	}
	return o
}

func usageErrorf(format string, args ...any) error {
	return New(KindUsage, "", format, args...)
}
