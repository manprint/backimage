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

	"github.com/manprint/backimage/pkg/archive"
	"github.com/manprint/backimage/pkg/cpu"
	"github.com/manprint/backimage/pkg/crypt"
	dockerd "github.com/manprint/backimage/pkg/docker"
	"github.com/manprint/backimage/pkg/index"
	"github.com/manprint/backimage/pkg/progress"
	"github.com/manprint/backimage/pkg/recovery"
	"github.com/manprint/backimage/pkg/registry"
	restorepkg "github.com/manprint/backimage/pkg/restore"
)

type sourceFlags struct {
	repo            string
	localRepo       bool
	ociLayout       string
	platform        string
	cacheSize       string
	passphraseFile  string
	passphraseStdin bool
	password        string
	passwordSet     bool
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
	flags.Bool("local-repo", false, "read the image from the local Docker daemon instead of a registry")
	flags.String("oci-layout", "", "read the image from this local OCI layout directory")
	flags.String("platform", "linux/amd64", "platform variant to read from the multi-arch image, OS/ARCH")
	flags.String("cache-size", "2GiB", "maximum size of the downloaded-layer cache, e.g. 512MiB, 4GiB (0 disables it)")
	flags.String("passphrase-file", "", "read the backup passphrase from this file (first line)")
	flags.Bool("passphrase-stdin", false, "read the backup passphrase from stdin")
	flags.String("password", "", "backup passphrase inline (visible in shell history and in `ps`: prefer --passphrase-file)")
	flags.String("identity", "", "age private key file, for a backup encrypted with --recipient")
}

func readSourceFlags(cmd *cobra.Command) sourceFlags {
	f := sourceFlags{
		localRepo: getFlagBool(cmd, "local-repo"), ociLayout: getFlagString(cmd, "oci-layout"),
		platform: getFlagString(cmd, "platform"), cacheSize: getFlagString(cmd, "cache-size"),
		passphraseFile: getFlagString(cmd, "passphrase-file"), passphraseStdin: getFlagBool(cmd, "passphrase-stdin"),
		password: getFlagString(cmd, "password"), passwordSet: cmd.Flags().Changed("password"),
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
	if flags.passwordSet && flags.password == "" {
		return &Error{Kind: KindUsage, Msg: "--password non può essere vuota"}
	}
	hasEnv := false
	if _, ok := os.LookupEnv("BACKIMAGE_PASSPHRASE"); ok {
		hasEnv = true
	}
	if !required && flags.password == "" && flags.passphraseFile == "" && !flags.passphraseStdin && !hasEnv {
		return nil
	}
	var direct []byte
	if flags.password != "" {
		direct = []byte(flags.password)
	}
	pass, err := crypt.ReadPassphrase(crypt.PassphraseSource{
		Direct: direct, File: flags.passphraseFile, Stdin: flags.passphraseStdin,
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
	cmd := &cobra.Command{
		Use:   "restore [IMAGE]",
		Short: "restore a backup image to disk or to a tar file",
		Long: "restore a backup image to disk or to a tar file.\n\n" +
			"IMAGE is the reference to restore (or --repo, or a local source with\n" +
			"--local-repo/--oci-layout). Choose one of two outcomes: -x extracts into\n" +
			"--destination, -o writes a tar file (- for stdout). Without either, a tar\n" +
			"is written to stdout.\n\n" +
			"  # extract everything into /restore\n" +
			"  backimage restore ghcr.io/me/dumps:daily -x -C /restore --passphrase-file ./pass\n\n" +
			"  # only the PDFs, without the leading directory level\n" +
			"  backimage restore ghcr.io/me/dumps:daily -x -C . \\\n" +
			"    --include '**/*.pdf' --strip-components 1 --passphrase-file ./pass\n\n" +
			"  # keep the archive as a tar file\n" +
			"  backimage restore ghcr.io/me/dumps:daily -o backup.tar --passphrase-file ./pass",
		Args: cobra.MaximumNArgs(1),
		RunE: runRestore,
	}
	addSourceFlags(cmd, true)
	f := cmd.Flags()
	f.BoolP("extract", "x", false, "extract the files into --destination instead of writing a tar")
	f.StringP("destination", "C", ".", "directory the files are extracted into (with -x)")
	f.StringP("output", "o", "", "write the archive to this tar file; - means stdout")
	f.StringSlice("include", nil, "restore only paths matching this glob, e.g. '**/*.pdf' (repeatable)")
	f.StringSlice("exclude", nil, "skip paths matching this glob (repeatable)")
	f.Int("strip-components", 0, "drop this many leading path components from each restored path (like tar)")
	f.Int("cpus", cpu.Default(), "maximum CPUs used for decompression and decryption (default: half the available CPUs)")
	f.Bool("no-preserve-owner", false, "restore files as the current user instead of the archived owner")
	f.Bool("remove-local-image", false, "delete the pulled Docker image once the restore succeeded")
	f.Bool("overwrite", false, "allow writing over an existing tar file or a non-empty destination")
	f.Bool("no-verify", false, "skip the plaintext chunk digest check (faster, unsafe)")
	f.Int("jobs", 3, "number of concurrent layer downloads")
	return cmd
}

func runRestore(cmd *cobra.Command, args []string) error {
	started := time.Now()
	log := func(message string) {
		if !mustOptions(cmd).Quiet {
			progress.WriteLine(cmd.ErrOrStderr(), message)
		}
	}
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
	log("restore: apertura sorgente")
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
	b.SetProgress(log)
	log("restore: metadati backup letti")
	if err := unlockBackup(ctx, b, flags, true); err != nil {
		return err
	}
	log("restore: backup sbloccato e pronto")

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
		log("restore: ricostruzione tar in corso")
		err = restoreTar(cmd, refText, stream)
		if err == nil {
			log("restore: ricostruzione tar completata")
		}
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
	log(fmt.Sprintf("restore completato in %s", time.Since(started).Round(time.Millisecond)))
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
	restoreLog(cmd, "restore: estrazione filesystem in corso")
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
		Progress: func(message string) { restoreLog(cmd, message) },
	})
	_, extractErr := x.Extract(cmd.Context(), progressReader, dest)
	if extractErr == nil {
		progressReader.Finish()
		restoreLog(cmd, "restore: verifica e finalizzazione filesystem completate")
	}
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
		progress.WriteLine(cmd.ErrOrStderr(), progress.Message("restore", done, total))
	}
}

func restoreLog(cmd *cobra.Command, message string) {
	if !mustOptions(cmd).Quiet {
		progress.WriteLine(cmd.ErrOrStderr(), message)
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
