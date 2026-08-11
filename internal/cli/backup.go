package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/manprint/backimage/internal/buildinfo"
	"github.com/manprint/backimage/internal/embedded"
	"github.com/manprint/backimage/pkg/backup"
	"github.com/manprint/backimage/pkg/chunk"
	"github.com/manprint/backimage/pkg/crypt"
	"github.com/manprint/backimage/pkg/progress"
	"github.com/manprint/backimage/pkg/registry"
	backremote "github.com/manprint/backimage/pkg/remote"
)

func flagErr(name string, err error) {
	panic(fmt.Sprintf("flag %s: %v", name, err))
}

func getFlagString(cmd *cobra.Command, name string) string {
	v, err := cmd.Flags().GetString(name)
	if err != nil {
		flagErr(name, err)
	}
	return v
}

func getFlagInt(cmd *cobra.Command, name string) int {
	v, err := cmd.Flags().GetInt(name)
	if err != nil {
		flagErr(name, err)
	}
	return v
}

func getFlagUint64(cmd *cobra.Command, name string) uint64 {
	v, err := cmd.Flags().GetUint64(name)
	if err != nil {
		flagErr(name, err)
	}
	return v
}

func getFlagBool(cmd *cobra.Command, name string) bool {
	v, err := cmd.Flags().GetBool(name)
	if err != nil {
		flagErr(name, err)
	}
	return v
}

func getFlagStrings(cmd *cobra.Command, name string) []string {
	v, err := cmd.Flags().GetStringSlice(name)
	if err != nil {
		flagErr(name, err)
	}
	return v
}

func printerResult(pr Printer, v any) error {
	if err := pr.Result(v); err != nil {
		return New(KindGeneric, "", "%v", err)
	}
	return nil
}

func recoveryInstructions(ref string, encrypted, runnable bool) string {
	appPrefix := ""
	appPassphrase := ""
	dockerPassphrase := ""
	if encrypted {
		appPrefix = "printf '%s\\n' \"$BACKUP_PASSPHRASE\" | "
		appPassphrase = " --passphrase-stdin"                                //nolint:gosec // Command suggestion, not a credential.
		dockerPassphrase = " -e BACKIMAGE_PASSPHRASE=\"$BACKUP_PASSPHRASE\"" //nolint:gosec // Command suggestion, not a credential.
	}

	var out strings.Builder
	fmt.Fprint(&out, "\n\ncomandi per recuperare i dati:\n")
	fmt.Fprintf(&out, "  backimage:\n    %sbackimage restore %s --extract --destination ./restore%s\n", appPrefix, ref, appPassphrase)
	if runnable {
		fmt.Fprintf(&out, "  docker run:\n    docker run --rm%s -v \"$PWD/restore:/restore\" %s extract --out /restore\n", dockerPassphrase, ref)
	} else {
		fmt.Fprint(&out, "  docker run: non disponibile (backup creato con --runnable=false)\n")
	}
	fmt.Fprint(&out, "\nTips:\n")
	fmt.Fprint(&out, "  - Se non vuoi ripristinare ownership e gruppi, aggiungi --no-preserve-owner al comando backimage o a extract.\n") //nolint:misspell // Messaggio CLI italiano.
	fmt.Fprint(&out, "  - Per limitare la CPU, aggiungi --cpus N al comando backimage o a extract.\n")                                    //nolint:misspell // Messaggio CLI italiano.
	fmt.Fprint(&out, "  - Per estrarre solo una parte, aggiungi --include GLOB e/o --exclude GLOB.\n")
	fmt.Fprint(&out, "  - Per rimuovere l'immagine Docker dopo un'estrazione riuscita, aggiungi --remove-local-image. Con docker run servono anche\n")
	fmt.Fprintf(&out, "    -e BACKIMAGE_IMAGE_REF=\"%s\" e -v /var/run/docker.sock:/var/run/docker.sock.\n", ref)
	fmt.Fprint(&out, "  - Se la directory di destinazione non è vuota, aggiungi --overwrite.\n")
	return out.String()
}

func newBackupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup <PATH...> --repo IMAGE [flags]",
		Short: "archive, encrypt and push a backup to an OCI registry",
		Long: "archive, encrypt and push a backup to an OCI registry.\n\n" +
			"PATH is one or more files or directories; they are archived together in\n" +
			"the order given. The result is a multi-arch OCI image that restores\n" +
			"itself with a plain `docker run`.\n\n" +
			"Sizes accept binary units (512MiB, 2GiB); a bare number means bytes.\n\n" +
			"  # local pipeline, encrypted, timestamped tag\n" +
			"  backimage backup /srv/data --repo ghcr.io/me/dumps --tag daily --timestamp \\\n" +
			"    --passphrase-file ./pass\n\n" +
			"  # delegate archiving and push to a remote server (little local disk)\n" +
			"  backimage backup /srv/data --repo ghcr.io/me/dumps --tag nightly \\\n" +
			"    --remote backup.example:7575 --tls-pin <PIN> --passphrase-file ./pass\n\n" +
			"  # see the plan without writing anything\n" +
			"  backimage backup /srv/data --repo ghcr.io/me/dumps --dry-run",
		Args: cobra.MinimumNArgs(1),
		RunE: runBackup,
	}
	f := cmd.Flags()
	f.String("repo", "", "target repository without a tag, e.g. ghcr.io/me/dumps (required)")
	f.String("tag", "latest", "tag to publish; combine with --timestamp for one tag per run")
	f.Bool("timestamp", false, "append a UTC timestamp to --tag, e.g. daily-20260810T031500Z")
	f.String("timestamp-format", "20060102T150405Z", "Go time layout used by --timestamp (reference date 2006-01-02 15:04:05)")
	f.String("compression", "zstd", "layer codec: zstd|gzip|xz|lz4|none (xz and lz4 require --runnable=false)")
	f.Int("compression-level", 0, "codec compression level; 0 = codec default, higher = smaller and slower")
	f.String("max-layer-size", "1GiB", "target size of each OCI layer, e.g. 512MiB, 2GiB")
	f.Bool("encrypt", true, "encrypt chunks (default)")
	f.Bool("no-encrypt", false, "disable encryption (exclusive with --encrypt)")
	f.String("passphrase-file", "", "read the passphrase from a file")
	f.Bool("passphrase-stdin", false, "read the passphrase from stdin")
	f.String("password", "", "passphrase (visible in shell history and process listings)")
	f.StringSlice("recipient", nil, "age public key (repeatable)")
	f.String("age-identity", "", "age identity file used to reuse a deduplication key")
	f.Bool("dedup", false, "enable content-defined incremental deduplication (reveals chunk equality)")
	f.String("dedup-chunk-min", "", "advanced CDC minimum chunk size, e.g. 256KiB (default: codec choice)")
	f.String("dedup-chunk-avg", "", "advanced CDC average chunk size, e.g. 1MiB (default: codec choice)")
	f.String("dedup-chunk-max", "", "advanced CDC maximum chunk size, e.g. 4MiB (default: codec choice)")
	f.String("dedup-polynomial", "", "advanced Rabin polynomial (0x...) for CDC")
	f.Bool("local-repo", false, "output to the Docker daemon instead of a registry")
	f.String("output", "registry", "registry|daemon|oci-layout|tar")
	f.String("output-path", "", "destination for oci-layout/tar")
	f.StringSlice("exclude", nil, "glob pattern to exclude (repeatable)")
	f.Bool("one-file-system", false, "do not cross mount points")
	f.Bool("numeric-owner", false, "do not resolve user/group names")
	f.Bool("allow-degraded", false, "continue despite unreadable files")
	f.Int("jobs", 3, "number of concurrent blob uploads")
	f.String("upload-chunk-size", "0", "split each blob upload into HTTP chunks of this size, e.g. 32MiB; 0 sends one request per blob (fastest, use a value only for a registry that refuses large bodies)")
	f.StringSlice("platform", []string{"linux/amd64", "linux/arm64"}, "self-extract platforms (repeatable)")
	f.Bool("no-metadata", false, "omit source paths from labels")
	f.Bool("dry-run", false, "print the plan and exit without writing")
	f.Bool("resume", true, "resume from the checkpoint if present")
	f.Bool("runnable", true, "build runnable images (false allows non-standard codecs)")
	f.String("temp-dir", "", "spool directory (default $TMPDIR)")
	f.String("created", "", "fixed image creation time in RFC3339, e.g. 2026-08-10T03:15:00Z (reproducible builds)")
	f.String("remote", "", "delegate the backup to a remote backimage server, HOST:PORT")
	f.String("remote-mode", "stream", "stream: the server runs the whole pipeline (default); layers: legacy client-side pipeline")
	f.Bool("udp", false, "use QUIC instead of TCP for --remote")
	f.String("tls-pin", "", "remote server certificate SHA-256 fingerprint, hex only (drop the SHA256: prefix printed by the server)")
	f.String("tls-ca", "", "PEM CA bundle for the remote server")
	f.String("tls-cert", "", "PEM client certificate for mTLS")
	f.String("tls-key", "", "PEM client private key for mTLS")
	f.String("auth-token", "", "pre-shared remote authentication token")
	f.String("auth-token-file", "", "read the remote authentication token from a file")
	f.Bool("server-side-compress", false, "deprecated alias of --remote-mode stream (already the default)")
	addQUICExperimentalFlags(cmd)
	return cmd
}

func runBackup(cmd *cobra.Command, args []string) error {
	opts, err := parseOptions(cmd.Root())
	if err != nil {
		return err
	}
	pr := NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)

	repo := getFlagString(cmd, "repo")
	if repo == "" {
		return New(KindUsage, "", "missing --repo")
	}
	tag := getFlagString(cmd, "tag")
	if ts := getFlagBool(cmd, "timestamp"); ts {
		layout := getFlagString(cmd, "timestamp-format")
		tag += "-" + time.Now().UTC().Format(layout)
	}
	ref := strings.TrimSuffix(repo, "/") + ":" + tag

	compression := getFlagString(cmd, "compression")
	if compression == "none" {
		compression = "store"
	}
	level := getFlagInt(cmd, "compression-level")
	maxLayerStr := getFlagString(cmd, "max-layer-size")
	maxLayer, err := parseSize(maxLayerStr)
	if err != nil {
		return New(KindUsage, "", "max-layer-size: %v", err)
	}
	dedup := getFlagBool(cmd, "dedup")
	if dedup && !cmd.Flags().Changed("max-layer-size") {
		// Smaller default layers make the content-defined boundaries useful for
		// normal incremental backups while leaving the non-dedup default intact.
		maxLayer = 64 << 20
	}
	dedupParams, err := readDedupParams(cmd)
	if err != nil {
		return New(KindUsage, "", "%v", err)
	}
	if dedup {
		if _, err := chunk.NormalizeCDCParams(dedupParams); err != nil {
			return New(KindUsage, "", "%v", err)
		}
	}
	encrypt := getFlagBool(cmd, "encrypt")
	if noEncrypt := getFlagBool(cmd, "no-encrypt"); noEncrypt {
		if encrypt && !cmd.Flags().Changed("encrypt") {
			encrypt = false
		} else if encrypt {
			return New(KindUsage, "", "cannot combine --encrypt and --no-encrypt")
		}
	}

	passfile := getFlagString(cmd, "passphrase-file")
	passStdin := getFlagBool(cmd, "passphrase-stdin")
	password := getFlagString(cmd, "password")
	recipients := getFlagStrings(cmd, "recipient")
	ageIdentity := getFlagString(cmd, "age-identity")
	if cmd.Flags().Changed("password") && password == "" {
		return New(KindUsage, "", "--password non può essere vuota")
	}
	if !encrypt && (passfile != "" || passStdin || password != "" || len(recipients) > 0 || ageIdentity != "") {
		return New(KindUsage, "", "passphrase/recipient given but encryption disabled")
	}
	if ageIdentity != "" && !dedup {
		return New(KindUsage, "", "--age-identity requires --dedup")
	}

	var passFn func() ([]byte, error)
	if encrypt && (passfile != "" || passStdin || password != "") {
		var direct []byte
		if password != "" {
			direct = []byte(password)
		}
		src := crypt.PassphraseSource{
			Direct:  direct,
			File:    passfile,
			Stdin:   passStdin,
			Prompt:  password == "",
			Confirm: password == "",
		}
		passFn = func() ([]byte, error) {
			p, err := crypt.ReadPassphrase(src)
			if err != nil {
				if errors.Is(err, crypt.ErrEmptyPassphrase) || errors.Is(err, crypt.ErrNoPassphrase) {
					return nil, New(KindUsage, "", "passphrase: %v", err)
				}
				return nil, New(KindGeneric, "", "passphrase: %v", err)
			}
			return p, nil
		}
	}

	runnable := getFlagBool(cmd, "runnable")
	platforms := getFlagStrings(cmd, "platform")

	output := getFlagString(cmd, "output")
	if localRepo := getFlagBool(cmd, "local-repo"); localRepo {
		if cmd.Flags().Changed("output") {
			return New(KindUsage, "", "--local-repo cannot be combined with --output")
		}
		output = "daemon"
	}
	outputPath := getFlagString(cmd, "output-path")

	store, err := registry.NewStore(authFilePath())
	if err != nil {
		return New(KindGeneric, "", "credential store: %v", err)
	}
	kc := registry.NewKeychainForUser(nil, store, registryUser(cmd))
	var remoteUploader backup.RemoteUploader
	var remoteStream backup.RemoteStreamUploader
	remoteMode := getFlagString(cmd, "remote-mode")
	serverSideCompress := getFlagBool(cmd, "server-side-compress")
	if remoteAddr := getFlagString(cmd, "remote"); remoteAddr != "" {
		if output != "registry" {
			return New(KindUsage, "", "--remote cannot be combined with --output %s", output)
		}
		switch remoteMode {
		case "stream", "layers":
		default:
			return New(KindUsage, "", "--remote-mode must be stream or layers, got %q", remoteMode)
		}
		if serverSideCompress && remoteMode == "layers" {
			return New(KindUsage, "", "--server-side-compress requires --remote-mode stream")
		}
		client, clientErr := newBackupRemote(cmd, ref, remoteAddr, kc)
		if clientErr != nil {
			return clientErr
		}
		if remoteMode == "stream" {
			remoteStream = client
			progress.WriteLine(cmd.ErrOrStderr(), "remote: streaming mode; archiving, compression, encryption and push run on the server, which therefore sees the plaintext data")
		} else {
			remoteUploader = client
		}
	} else {
		if cmd.Flags().Changed("remote-mode") {
			return New(KindUsage, "", "--remote-mode requires --remote")
		}
		if serverSideCompress {
			return New(KindUsage, "", "--server-side-compress requires --remote")
		}
	}

	resume := getFlagBool(cmd, "resume")
	tempDir := getFlagString(cmd, "temp-dir")
	excludes := getFlagStrings(cmd, "exclude")
	oneFS := getFlagBool(cmd, "one-file-system")
	numOwner := getFlagBool(cmd, "numeric-owner")
	degraded := getFlagBool(cmd, "allow-degraded")
	noMeta := getFlagBool(cmd, "no-metadata")
	dryRun := getFlagBool(cmd, "dry-run")
	jobs := getFlagInt(cmd, "jobs")
	uploadChunk, err := parseSize(getFlagString(cmd, "upload-chunk-size"))
	if err != nil {
		return New(KindUsage, "", "upload-chunk-size: %v", err)
	}

	cfg := backup.Config{
		RootPaths:       append([]string(nil), args...),
		Ref:             ref,
		Compression:     compression,
		Level:           level,
		MaxLayerSize:    maxLayer,
		Jobs:            jobs,
		UploadChunkSize: uploadChunk,
		Version:         buildinfo.Version,
		Encrypt:         encrypt,
		Passphrase:      passFn,
		Recipients:      recipients,
		Dedup:           dedup,
		DedupParams:     dedupParams,
		AgeIdentity:     ageIdentity,
		Exclude:         excludes,
		OneFileSystem:   oneFS,
		NumericOwner:    numOwner,
		AllowDegraded:   degraded,
		NoMetadata:      noMeta,
		Runnable:        runnable,
		Platforms:       platforms,
		TempDir:         tempDir,
		Resume:          resume,
		Keychain:        kc,
		Store:           store,
		DryRun:          dryRun,
		Output:          output,
		OutputPath:      outputPath,
		Remote:          remoteUploader,
		RemoteStream:    remoteStream,
		Created:         getFlagString(cmd, "created"),
		SelfExtract:     embedded.SelfExtract,
	}
	if err := backup.Validate(cfg); err != nil {
		return New(KindUsage, "", "%v", err)
	}

	if cfg.Runnable && compression != "zstd" && compression != "gzip" && compression != "none" {
		// runnable images only allow standard codecs; the plan gates this.
		if compression == "lz4" || compression == "xz" || compression == "store" {
			return New(KindUsage, "", "--runnable=true non ammette il codec %q: usare --runnable=false", compression)
		}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prog := func(msg string) {
		if opts.Quiet {
			return
		}
		progress.WriteLine(cmd.ErrOrStderr(), msg)
	}
	cfg.Progress = prog

	res, err := backup.Run(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return New(KindInterrupted, "", "backup interrotto")
		}
		if errors.Is(err, backup.ErrNoData) {
			return New(KindGeneric, "", "%v", err)
		}
		var remoteErr *backremote.Error
		if errors.As(err, &remoteErr) && remoteErr.Kind >= uint32(KindGeneric) && remoteErr.Kind <= uint32(KindInterrupted) {
			return New(Kind(remoteErr.Kind), remoteErr.Hint, "%s", remoteErr.Message)
		}
		if isNetworkErr(err) || strings.Contains(err.Error(), "registry") || strings.Contains(err.Error(), "push") {
			return New(KindNetwork, "", "%v", err)
		}
		return New(KindGeneric, "", "%v", err)
	}

	if dryRun {
		if err := pr.Result(fmt.Sprintf("dry-run: %d file, %d byte, %d layer, %d chunk (nessuna scrittura)", res.Files, res.BytesRaw, res.Layers, res.Chunks)); err != nil {
			return New(KindGeneric, "", "%v", err)
		}
		return nil
	}
	if opts.JSON {
		return printerResult(pr, res)
	}
	return printerResult(pr, fmt.Sprintf("backup completato: %s\n  digest   %s\n  file     %d\n  byte raw %d\n  byte archiviati %d\n  layer    %d\n  chunk    %d\n  durata   %ds\n  saltati  %d (%d byte)",
		res.Ref, res.Digest, res.Files, res.BytesRaw, res.BytesStored, res.Layers, res.Chunks, res.DurationSeconds, res.SkippedBlobs, res.SkippedBytes)+
		recoveryInstructions(res.Ref, res.Encrypted, runnable))
}

func readDedupParams(cmd *cobra.Command) (chunk.CDCParams, error) {
	var p chunk.CDCParams
	for _, flag := range []struct {
		name string
		dst  *int64
	}{
		{"dedup-chunk-min", &p.Min},
		{"dedup-chunk-avg", &p.Avg},
		{"dedup-chunk-max", &p.Max},
	} {
		value := getFlagString(cmd, flag.name)
		if value == "" {
			continue
		}
		size, err := parseSize(value)
		if err != nil {
			return p, fmt.Errorf("--%s: %w", flag.name, err)
		}
		*flag.dst = size
	}
	if value := getFlagString(cmd, "dedup-polynomial"); value != "" {
		polynomial, err := strconv.ParseUint(value, 0, 64)
		if err != nil {
			return p, fmt.Errorf("--dedup-polynomial: %w", err)
		}
		p.Polynomial = polynomial
	}
	return p, nil
}

// parseSize parses sizes like "512MiB", "1GiB", "1048576".
func isNetworkErr(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		strings.Contains(err.Error(), "SCHEME/HOST") ||
		strings.Contains(err.Error(), "connection refused")
}

func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	tests := []struct {
		suffix string
		mult   int64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
		{"B", 1},
	}
	for _, u := range tests {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			v, err := strconv.ParseInt(num, 10, 64)
			if err != nil || v < 0 || (u.mult != 0 && v > (1<<63-1)/u.mult) {
				return 0, fmt.Errorf("dimensione %q non valida", s)
			}
			return v * u.mult, nil
		}
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("dimensione %q non valida (es. 512MiB, 2GiB)", s)
	}
	return v, nil
}
