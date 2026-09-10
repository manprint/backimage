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
	repo             string
	localRepo        bool
	ociLayout        string
	platform         string
	cacheSize        string
	passphraseFile   string
	passphraseStdin  bool
	password         string
	passwordSet      bool
	identity         string
	allowUnencrypted bool
	registryUser     string // --registry-user: which login to use on this host
}

// hasCredential reports whether the caller offered a way to unlock a backup.
// Any of them means the caller believes this backup is encrypted, which is the
// premise requireEncryption checks.
func (f sourceFlags) hasCredential() bool {
	if f.identity != "" || f.passphraseFile != "" || f.passphraseStdin || f.password != "" {
		return true
	}
	_, ok := os.LookupEnv("BACKIMAGE_PASSPHRASE")
	return ok
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
	flags.Bool("allow-unencrypted", false, "accept an unencrypted backup even when a passphrase or an identity was supplied")
}

func readSourceFlags(cmd *cobra.Command) sourceFlags {
	f := sourceFlags{
		localRepo: getFlagBool(cmd, "local-repo"), ociLayout: getFlagString(cmd, "oci-layout"),
		platform: getFlagString(cmd, "platform"), cacheSize: getFlagString(cmd, "cache-size"),
		passphraseFile: getFlagString(cmd, "passphrase-file"), passphraseStdin: getFlagBool(cmd, "passphrase-stdin"),
		password: getFlagString(cmd, "password"), passwordSet: cmd.Flags().Changed("password"),
		identity: getFlagString(cmd, "identity"), allowUnencrypted: getFlagBool(cmd, "allow-unencrypted"),
		registryUser: registryUser(cmd),
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
	s, err := fromRegistryCLI(ctx, ref, registry.NewKeychainForUser(nil, store, flags.registryUser), restorepkg.SourceOptions{
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
		return requireEncryption(flags)
	}
	if flags.identity != "" {
		if err := b.Unlock(ctx, crypt.Identity{AgeKeyFile: flags.identity}); err != nil {
			return unlockError("identità age non valida", err)
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
		return unlockError("passphrase errata", err)
	}
	return nil
}

// unlockError classifies a failed unlock.
//
// Two very different events end up here. The credential can be wrong, which is
// the user's problem and exits 4. Or the private metadata blob can fail
// authentication — the A01 downgrade, where the blob of an encrypted backup
// arrives with no tag at all — which is the backup's problem and exits 5.
// Calling the second one "passphrase errata" sent the user looking for a typo
// while the honest answer was that the image no longer matches what it claims
// to be, and a script watching exit codes saw a credential error where the
// backup had been tampered with.
func unlockError(fallback string, err error) *Error {
	if errors.Is(err, crypt.ErrIntegrity) || errors.Is(err, index.ErrBadSchema) {
		return &Error{
			Kind: KindIntegrity,
			Msg:  "metadati privati del backup non autenticati",
			Hint: "il backup potrebbe essere stato manomesso o sostituito: confronta il digest dell'immagine con quello atteso",
			Err:  err,
		}
	}
	return &Error{Kind: KindPassphrase, Msg: fallback, Err: err}
}

// requireEncryption turns "the backup is not encrypted, so the credential you
// gave is unused" into an error instead of a silent success.
//
// The strict opener keeps a downgrade from happening inside an encrypted
// backup; it says nothing about the whole backup being swapped for a plaintext
// schema 1 one. That substitution used to be invisible: the reader noticed the
// manifest said encryption was off, dropped the passphrase on the floor and
// restored attacker-controlled files reporting success. Whoever supplies a
// passphrase or an identity has stated what they expect to be reading, and a
// backup that does not match it is a failure of that expectation.
func requireEncryption(flags sourceFlags) error {
	if flags.allowUnencrypted || !flags.hasCredential() {
		return nil
	}
	return &Error{
		Kind: KindIntegrity,
		Msg:  "backup non cifrato, ma è stata fornita una credenziale: potrebbe essere stato sostituito",
		Hint: "se il backup è davvero in chiaro usa --allow-unencrypted, oppure rimuovi passphrase e identità",
	}
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
	f.Bool("no-preserve-xattrs", false, "do not restore extended attributes")
	f.Bool("strict", false, "abort the extraction when any metadata operation is refused, instead of degrading and reporting it")
	f.Bool("continue", false, "do not stop at the first damaged chunk: restore every entry that verifies and report the ones lost")
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
	keepGoing := getFlagBool(cmd, "continue")
	if keepGoing && idx == nil {
		// The partial recovery works from the file index: it is what maps a
		// damaged chunk to the entries that live in it.
		idx, err = b.Index(ctx)
		if err != nil {
			return err
		}
	}
	var partial recovery.PartialReport
	var extracted archive.Stats
	stream := func(w io.Writer) error {
		if keepGoing {
			var perr error
			if selected != nil {
				// --continue used to swap the selective stream for the
				// tolerant one, which had no notion of a selection: the whole
				// backup came out and, with --overwrite, went over the
				// destination. Tolerating damaged chunks and honouring the
				// filters are independent properties.
				partial, perr = b.StreamSelectedTarPartial(ctx, idx, selected, w, !getFlagBool(cmd, "no-verify"))
				return perr
			}
			partial, perr = b.StreamTarPartial(ctx, idx, w, !getFlagBool(cmd, "no-verify"))
			return perr
		}
		if selected != nil {
			return b.StreamSelectedTar(ctx, idx, selected, w, !getFlagBool(cmd, "no-verify"))
		}
		return b.StreamTar(ctx, w, !getFlagBool(cmd, "no-verify"))
	}
	if getFlagBool(cmd, "extract") {
		total := b.Manifest.Totals.BytesRaw
		if selected != nil {
			total = selectedBytes(selected)
		}
		extracted, err = restoreExtract(cmd, stream, selected != nil, restoreProgress(cmd, total))
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
	// Audit evidence of a partial recovery, and a non-zero exit: data is
	// missing, however much was salvaged.
	if keepGoing {
		for _, line := range partial.Summary() {
			log("restore: " + line)
		}
		if partial.Skipped > 0 {
			return &Error{Kind: KindIntegrity, Msg: fmt.Sprintf(
				"recupero parziale: %d entry non recuperate dai chunk danneggiati %v", partial.Skipped, partial.BadChunks)}
		}
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
		result := map[string]any{"ok": true, "reference": refText, "extract": getFlagBool(cmd, "extract"),
			"remove_local_image": imageRemoved, "duration": time.Since(started).String()}
		if getFlagBool(cmd, "extract") {
			// What the extractor could not write belongs in the machine
			// readable answer, not only in a warning on stderr.
			result["skipped"] = extracted.Skipped
			result["skipped_reasons"] = errorTexts(extracted.Errors)
			if len(extracted.Warnings) > 0 {
				result["warnings"] = extracted.Warnings
			}
		}
		return printerResult(pr, result)
	}
	if extracted.Skipped > 0 {
		log(fmt.Sprintf("restore: attenzione: %d entry non ripristinate (vedi --json per l'elenco)", extracted.Skipped))
	}
	log(fmt.Sprintf("restore completato in %s", time.Since(started).Round(time.Millisecond)))
	return nil
}

// errorTexts renders the per-entry failures for the JSON output.
func errorTexts(errs []error) []string {
	if len(errs) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(errs))
	for _, err := range errs {
		out = append(out, err.Error())
	}
	return out
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

// restoreExtract writes the stream into --destination.
//
// alreadyFiltered says the stream itself already contains only the selected
// entries, so the extractor must not filter a second time — a second pass over
// an already-filtered stream drops the parent directories the selection pulled
// in on purpose.
// restoreExtract writes the stream into the destination and returns what the
// extractor made of it. The stats used to be discarded: an entry the extractor
// skipped and recorded — a hardlink whose first name is not part of this
// restore, a name the platform cannot hold — reached the user as a warning
// line on stderr and nothing at all in --json, so automation could not see
// that the restore was incomplete.
func restoreExtract(cmd *cobra.Command, stream func(io.Writer) error, alreadyFiltered bool, report func(int64)) (archive.Stats, error) {
	var stats archive.Stats
	dest := getFlagString(cmd, "destination")
	if dest == "" {
		return stats, usageErrorf("--destination non può essere vuota")
	}
	if !getFlagBool(cmd, "overwrite") {
		if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
			return stats, usageErrorf("destinazione %s non vuota; usa --overwrite", dest)
		}
	}
	strict := getFlagBool(cmd, "strict")
	if !getFlagBool(cmd, "no-preserve-owner") {
		caps, err := archive.PreflightRestore(cmd.Context(), dest)
		if err != nil {
			return stats, err
		}
		for _, cap := range caps {
			if cap.Available {
				continue
			}
			// A missing privilege only degrades metadata, so it stops the
			// restore in --strict mode only. Advisory capabilities (trusted.*
			// xattrs: overlayfs bookkeeping) never stop it.
			if strict && archive.BlockingCapability(cap) {
				return stats, &Error{Kind: KindPermission, Msg: cap.Reason, Hint: cap.Remedy}
			}
			restoreLog(cmd, "restore: attenzione: "+cap.Reason+" — "+cap.Remedy)
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
		PreserveOwner:  !getFlagBool(cmd, "no-preserve-owner"),
		PreserveXattrs: !getFlagBool(cmd, "no-preserve-xattrs"),
		Overwrite:      getFlagBool(cmd, "overwrite"), Includes: includes, Excludes: excludes,
		StripComponents: getFlagInt(cmd, "strip-components"),
		Strict:          strict,
		Progress:        func(message string) { restoreLog(cmd, message) },
	})
	stats, extractErr := x.Extract(cmd.Context(), progressReader, dest)
	if extractErr == nil {
		progressReader.Finish()
		restoreLog(cmd, "restore: verifica e finalizzazione filesystem completate")
	}
	if extractErr != nil {
		_ = pr.CloseWithError(extractErr)
	}
	streamErr := <-done
	if extractErr != nil {
		return stats, extractErr
	}
	return stats, streamErr
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
