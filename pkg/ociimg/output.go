// pkg/ociimg/output.go publishes an assembled image index to one of the
// four supported destinations (phase 04.5).
package ociimg

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Target names an output destination.
type Target string

const (
	TargetRegistry  Target = "registry"
	TargetDaemon    Target = "daemon"
	TargetOCILayout Target = "oci-layout"
	TargetTar       Target = "tar"
)

// Progress reports the outcome of one blob transfer.
type Progress struct {
	Blob      v1.Hash
	Total     int64
	Completed int64
	Layer     int
	Skipped   bool
}

// daemonWrite is the indirection used by the daemon writer; it can be
// swapped in tests.
var daemonWrite = func(tag name.Tag, img v1.Image) (string, error) { return daemon.Write(tag, img) }

// WriterOptions tunes a Writer.
type WriterOptions struct {
	Auth    authn.Authenticator
	Images  map[string]v1.Image // "os/arch" -> image; needed by daemon and tar
	Runtime v1.Platform         // host platform for daemon/tar; defaults to GOOS/GOARCH
}

// Writer publishes an image index to one destination.
type Writer interface {
	// Write publishes idx under ref and reports progress on ch (may be nil).
	Write(ctx context.Context, ref name.Reference, idx v1.ImageIndex, ch chan<- Progress) error
	// Name returns the target name.
	Name() Target
}

type baseWriter struct {
	opts WriterOptions
}

func (b *baseWriter) hostPlatform() v1.Platform {
	if b.opts.Runtime.OS != "" {
		return b.opts.Runtime
	}
	return v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
}

// selectImage picks the image for the host platform from the provided map.
func (b *baseWriter) selectImage() (v1.Image, error) {
	p := b.hostPlatform()
	key := p.OS + "/" + p.Architecture
	if img, ok := b.opts.Images[key]; ok && img != nil {
		return img, nil
	}
	avail := make([]string, 0, len(b.opts.Images))
	for k := range b.opts.Images {
		avail = append(avail, k)
	}
	sort.Strings(avail)
	return nil, fmt.Errorf("no image built for host platform %s (available: %v): pass --platform or build that platform", key, avail)
}

func newBaseWriter(opts WriterOptions) baseWriter {
	if opts.Runtime.OS == "" {
		opts.Runtime = v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	}
	return baseWriter{opts: opts}
}

// NewWriter returns the writer for t. path is used by oci-layout and tar.
func NewWriter(t Target, path string, opts WriterOptions) (Writer, error) {
	switch t {
	case TargetRegistry:
		if opts.Auth == nil {
			opts.Auth = authn.Anonymous
		}
		return &registryWriter{baseWriter: newBaseWriter(opts)}, nil
	case TargetDaemon:
		return &daemonWriter{baseWriter: newBaseWriter(opts)}, nil
	case TargetOCILayout:
		if path == "" {
			return nil, errors.New("oci-layout requires a path")
		}
		return &layoutWriter{baseWriter: newBaseWriter(opts), path: path}, nil
	case TargetTar:
		if path == "" {
			return nil, errors.New("tar requires a path")
		}
		return &tarWriter{baseWriter: newBaseWriter(opts), path: path}, nil
	default:
		return nil, fmt.Errorf("unsupported output target %q", t)
	}
}

type registryWriter struct {
	baseWriter
}

func (w *registryWriter) Name() Target { return TargetRegistry }

func (w *registryWriter) Write(ctx context.Context, ref name.Reference, idx v1.ImageIndex, ch chan<- Progress) error {
	opts := []remote.Option{
		remote.WithAuth(w.opts.Auth),
		remote.WithContext(ctx),
	}
	if ch != nil {
		up := make(chan v1.Update)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range up {
				if u.Error != nil {
					ch <- Progress{Completed: u.Complete, Total: u.Total}
					continue
				}
				ch <- Progress{Total: u.Total, Completed: u.Complete}
			}
		}()
		opts = append(opts, remote.WithProgress(up))
		defer wg.Wait()
		return remote.WriteIndex(ref, idx, opts...)
	}
	return remote.WriteIndex(ref, idx, opts...)
}

type daemonWriter struct {
	baseWriter
}

func (w *daemonWriter) Name() Target { return TargetDaemon }

func (w *daemonWriter) Write(ctx context.Context, ref name.Reference, idx v1.ImageIndex, ch chan<- Progress) (err error) {
	img, err := w.selectImage()
	if err != nil {
		return err
	}
	tag, ok := ref.(name.Tag)
	if !ok {
		return fmt.Errorf("daemon target requires a tag reference, got %s", ref.Name())
	}
	if _, err := daemonWrite(tag, img); err != nil {
		return fmt.Errorf("docker daemon: %w", err)
	}
	return nil
}

type layoutWriter struct {
	baseWriter
	path string
}

func (w *layoutWriter) Name() Target { return TargetOCILayout }

func (w *layoutWriter) Write(ctx context.Context, ref name.Reference, idx v1.ImageIndex, ch chan<- Progress) error {
	if _, err := layout.Write(w.path, idx); err != nil {
		return fmt.Errorf("writing OCI layout at %s: %w", w.path, err)
	}
	return nil
}

type tarWriter struct {
	baseWriter
	path string
}

func (w *tarWriter) Name() Target { return TargetTar }

func (w *tarWriter) Write(ctx context.Context, ref name.Reference, idx v1.ImageIndex, ch chan<- Progress) error {
	img, err := w.selectImage()
	if err != nil {
		return err
	}
	if err := tarball.WriteToFile(w.path, ref, img); err != nil {
		return fmt.Errorf("writing tar at %s: %w", w.path, err)
	}
	return nil
}
