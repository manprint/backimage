package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/fpierri/backimage/pkg/archive"
	"github.com/fpierri/backimage/pkg/cpu"
	dockerd "github.com/fpierri/backimage/pkg/docker"
	"github.com/fpierri/backimage/pkg/index"
	"github.com/fpierri/backimage/pkg/recovery"
)

var stdoutIsTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
var removeDockerImage = dockerd.RemoveLocalImage

func cmdInfo(_ context.Context, args []string) error {
	var common commonOptions
	fs := newFlagSet("info", &common)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(common.root, "manifest.json"))
	if err != nil {
		return fmt.Errorf("questa immagine non è un backup backimage: %w", err)
	}
	m, err := index.ReadManifest(f)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(m)
	}
	encryption := "disattiva"
	if m.Encryption.Enabled {
		encryption = "attiva (" + m.Encryption.AEAD + ")"
	}
	fmt.Fprintf(stdout, "backup backimage %s\n", m.Tool.Version)
	fmt.Fprintf(stdout, "  creato        %s\n", m.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(stdout, "  origine       %v\n", m.Sources)
	fmt.Fprintf(stdout, "  contenuto     %d file, %d directory, %d byte\n", m.Totals.Files, m.Totals.Dirs, m.Totals.BytesRaw)
	fmt.Fprintf(stdout, "  archivio      %s, %s livello %d -> %d byte\n", m.Archive.Format, m.Archive.Compression, m.Archive.CompressionLevel, m.Totals.BytesStored)
	fmt.Fprintf(stdout, "  cifratura     %s\n", encryption)
	return nil
}

func openBackup(ctx context.Context, common commonOptions, required bool) (*recovery.Backup, error) {
	b, err := recovery.OpenLocal(ctx, common.root)
	if err != nil {
		return nil, err
	}
	if err := common.unlock(ctx, b, required); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func cmdList(ctx context.Context, args []string) error {
	var common commonOptions
	var includes, excludes multiFlag
	fs := newFlagSet("list", &common)
	long := fs.Bool("long", false, "long listing")
	fs.BoolVar(long, "l", false, "long listing")
	fs.Var(&includes, "include", "include glob (repeatable)")
	fs.Var(&excludes, "exclude", "exclude glob (repeatable)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	b, err := openBackup(ctx, common, true)
	if err != nil {
		return err
	}
	defer b.Close()
	idx, err := b.Index(ctx)
	if err != nil {
		return err
	}
	entries, err := index.EntriesMatching(idx, includes, excludes)
	if err != nil {
		return usageErrorf("%v", err)
	}
	return index.WriteEntries(stdout, entries, *long, *asJSON)
}

func cmdTar(ctx context.Context, args []string) error {
	var common commonOptions
	fs := newFlagSet("tar", &common)
	cpus := fs.Int("cpus", cpu.Default(), "maximum CPUs used during tar extraction (default: half available CPUs)")
	noVerify := fs.Bool("no-verify", false, "skip plaintext digest verification (last resort)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	restoreCPUs, err := cpu.Apply(*cpus)
	if err != nil {
		return usageErrorf("--cpus: %v", err)
	}
	defer restoreCPUs()
	if stdoutIsTerminal() {
		return usageErrorf("tar scrive dati binari: reindirizza l'output, es. `docker run --rm -i IMAGE tar > backup.tar`")
	}
	b, err := openBackup(ctx, common, true)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := b.StreamTar(ctx, stdout, !*noVerify); err != nil {
		if exitCode(err) == exitPassphrase {
			return err
		}
		return withCode(exitIntegrity, err)
	}
	return nil
}

func cmdExtract(ctx context.Context, args []string) error {
	var common commonOptions
	var includes, excludes multiFlag
	fs := newFlagSet("extract", &common)
	out := fs.String("out", "", "destination directory")
	fs.Var(&includes, "include", "include glob (repeatable)")
	fs.Var(&excludes, "exclude", "exclude glob (repeatable)")
	cpus := fs.Int("cpus", cpu.Default(), "maximum CPUs used during extraction (default: half available CPUs)")
	noOwner := fs.Bool("no-preserve-owner", false, "do not restore owner")
	removeLocalImage := fs.Bool("remove-local-image", false, "remove the local Docker image after a successful extraction")
	overwrite := fs.Bool("overwrite", false, "replace existing files")
	strip := fs.Int("strip-components", 0, "remove leading path components")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *out == "" {
		return usageErrorf("--out è obbligatorio")
	}
	if *strip < 0 {
		return usageErrorf("--strip-components non può essere negativo")
	}
	restoreCPUs, err := cpu.Apply(*cpus)
	if err != nil {
		return usageErrorf("--cpus: %v", err)
	}
	defer restoreCPUs()
	if !*overwrite {
		if entries, err := os.ReadDir(*out); err == nil && len(entries) > 0 {
			return usageErrorf("destinazione %s non vuota; usa --overwrite", *out)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	b, err := openBackup(ctx, common, true)
	if err != nil {
		return err
	}
	defer b.Close()
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
	}
	pr, pw := io.Pipe()
	streamErr := make(chan error, 1)
	go func() {
		var err error
		if idx != nil {
			err = b.StreamSelectedTar(ctx, idx, selected, pw, true)
		} else {
			err = b.StreamTar(ctx, pw, true)
		}
		_ = pw.CloseWithError(err)
		streamErr <- err
	}()
	xIncludes, xExcludes := []string(includes), []string(excludes)
	if idx != nil {
		xIncludes, xExcludes = nil, nil // the tar stream is already filtered
	}
	x := archive.NewExtractor(archive.ExtractOptions{
		PreserveOwner: !*noOwner, PreserveXattrs: true, Overwrite: *overwrite,
		Includes: xIncludes, Excludes: xExcludes, StripComponents: *strip, Strict: true,
	})
	stats, extractErr := x.Extract(ctx, pr, *out)
	if extractErr != nil {
		_ = pr.CloseWithError(extractErr)
	}
	producerErr := <-streamErr
	if extractErr != nil {
		return extractErr
	}
	if producerErr != nil {
		return withCode(exitIntegrity, producerErr)
	}
	if *removeLocalImage {
		ref := strings.TrimSpace(os.Getenv("BACKIMAGE_IMAGE_REF"))
		if ref == "" {
			return usageErrorf("--remove-local-image richiede BACKIMAGE_IMAGE_REF")
		}
		if err := removeDockerImage(ctx, ref); err != nil {
			return fmt.Errorf("rimozione immagine locale fallita: %w", err)
		}
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(stats)
	}
	fmt.Fprintf(stdout, "estratti: %d file, %d directory, %d byte\n", stats.Files, stats.Dirs, stats.BytesRaw)
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	var common commonOptions
	fs := newFlagSet("verify", &common)
	keepGoing := fs.Bool("continue", false, "report all integrity errors")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	b, err := recovery.OpenLocal(ctx, common.root)
	if err != nil {
		return err
	}
	defer b.Close()
	full := !b.Manifest.Encryption.Enabled
	if b.Manifest.Encryption.Enabled && common.hasCredential() {
		if err := common.unlock(ctx, b, false); err != nil {
			return err
		}
		full = true
	}
	res, verifyErr := b.Verify(ctx, full, *keepGoing)
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			return err
		}
	} else if verifyErr != nil {
		for _, message := range res.Errors {
			fmt.Fprintln(stderr, "  "+message)
		}
	} else if full {
		fmt.Fprintf(stdout, "ok: il backup è integro (%d chunk, %d voci)\n", res.Chunks, res.Entries)
	} else {
		fmt.Fprintf(stdout, "ok: verifica parziale (%d chunk memorizzati); fornisci la passphrase per verificare plaintext e indice\n", res.Chunks)
	}
	if verifyErr != nil {
		return withCode(exitIntegrity, verifyErr)
	}
	return nil
}
