package archive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Capability describes one privilege-dependent ability.
type Capability struct {
	Name      string // see BlockingCapability for the list
	Available bool
	Reason    string // why it is unavailable
	Remedy    string // exact command the user should run
	Advisory  bool   // true: missing it degrades metadata, it does not stop the operation
}

// BlockingCapability reports whether a missing capability must stop the
// operation instead of only degrading it. Advisory capabilities (today:
// trusted.* extended attributes) are reported and then skipped at runtime.
func BlockingCapability(c Capability) bool { return !c.Advisory }

// procStatusPath is overridable in tests to inject a synthetic CapEff.
var procStatusPath = "/proc/self/status"

// CapEff bit positions (linux/capability.h).
const (
	capChown           = 0
	capDACReadSearch   = 2
	capSysAdmin        = 21
	capMknod           = 27
	capSetfcap         = 31
	preflightSampleMax = 1000
)

// parseCapEff reads the effective capability bitmask from /proc/self/status
// text. Returns 0 when the field is absent (non-Linux).
func parseCapEff(status string) (uint64, error) {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0, fmt.Errorf("malformed CapEff line: %q", line)
		}
		return strconv.ParseUint(fields[1], 16, 64)
	}
	return 0, nil
}

func readCapEff() uint64 {
	data, err := os.ReadFile(procStatusPath)
	if err != nil {
		return 0
	}
	eff, err := parseCapEff(string(data))
	if err != nil {
		return 0
	}
	return eff
}

func hasCap(eff uint64, bit uint) bool { return eff&(1<<bit) != 0 }

// PreflightBackup inspects the environment and the given roots, and reports
// what will and will not be preserved. Nothing is written.
func PreflightBackup(ctx context.Context, roots []string) ([]Capability, error) {
	if len(roots) == 0 {
		return nil, errors.New("preflight: no roots given")
	}
	eff := readCapEff()
	// Being uid 0 is not the same as holding every capability: a container
	// runs as root with a reduced bounding set (no CAP_SYS_ADMIN unless
	// --privileged). Trust the effective set whenever the kernel exposes it,
	// and fall back to the uid only when /proc is unreadable.
	amRoot := os.Geteuid() == 0 && eff == 0
	capable := func(bit uint) bool { return amRoot || hasCap(eff, bit) }

	// read-all-files: root, CAP_DAC_READ_SEARCH, or a light sample scan.
	unreadable, example, err := countUnreadable(ctx, roots, preflightSampleMax)
	if err != nil {
		return nil, err
	}
	readAll := Capability{Name: "read-all-files",
		Remedy: "run with sudo, or grant capability: sudo setcap cap_dac_read_search+ep $(which backimage)"}
	switch {
	case capable(capDACReadSearch):
		readAll.Available = true
	case unreadable == 0:
		readAll.Available = true
	default:
		readAll.Available = false
		readAll.Reason = fmt.Sprintf("%d unreadable files found (e.g. %s)", unreadable, example)
	}

	chown := Capability{Name: "chown", Remedy: "run as root (sudo backimage)"}
	if capable(capChown) {
		chown.Available = true
	} else {
		chown.Reason = "ownership is not preserved without privileges"
	}

	mknod := Capability{Name: "mknod", Remedy: "run as root (sudo backimage) to restore device nodes"}
	if capable(capMknod) {
		mknod.Available = true
	} else {
		mknod.Reason = "device nodes cannot be created without privileges"
	}

	sec := Capability{Name: "set-security-xattr", Remedy: "sudo setcap cap_setfcap+ep $(which backimage) or run as root"}
	if capable(capSetfcap) {
		sec.Available = true
	} else {
		sec.Reason = "security.capability cannot be written without privileges"
	}

	// Advisory: trusted.* holds overlayfs bookkeeping (trusted.overlay.opaque
	// and friends), which shows up whenever the archived tree contains a
	// nested /var/lib/docker. Writing it needs CAP_SYS_ADMIN, which a
	// container does not get without --privileged; the restore skips those
	// attributes instead of failing.
	trusted := Capability{Name: "set-trusted-xattr", Advisory: true,
		Remedy: "run with --privileged (or --cap-add SYS_ADMIN) to restore trusted.* attributes"}
	if capable(capSysAdmin) {
		trusted.Available = true
	} else {
		trusted.Reason = "trusted.* extended attributes (overlayfs metadata) cannot be written without CAP_SYS_ADMIN"
	}
	return []Capability{readAll, chown, mknod, sec, trusted}, nil
}

// PreflightRestore reports whether a faithful restore is possible into dest.
func PreflightRestore(ctx context.Context, dest string) ([]Capability, error) {
	// The stacked checks are shared with the backup preflight; only the scan
	// target changes.
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, fmt.Errorf("preflight restore %q: %w", dest, err)
	}
	return PreflightBackup(ctx, []string{dest})
}

// countUnreadable inspects at most max entries (Lstat + Open attempt) and
// reports how many cannot be read, with one example path.
func countUnreadable(ctx context.Context, roots []string, max int) (int, string, error) {
	var (
		unreadable int
		example    string
		inspected  int
	)
	for _, root := range roots {
		if err := filepath.Walk(root, func(p string, fi fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if inspected >= max {
				return filepath.SkipAll
			}
			if !fi.Mode().IsRegular() {
				return nil
			}
			inspected++
			f, err := os.Open(p)
			if err != nil {
				unreadable++
				if example == "" {
					example = p
				}
				return nil
			}
			f.Close()
			return nil
		}); err != nil && !errors.Is(err, fs.SkipAll) {
			return 0, "", err
		}
	}
	return unreadable, example, nil
}
