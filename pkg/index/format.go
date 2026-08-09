package index

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// FormatLong renders one portable ls-style row. It is shared by the main CLI
// and the self-extracting binary so their output cannot drift.
func FormatLong(e FileEntry) string {
	owner := strconv.Itoa(e.UID) + ":" + strconv.Itoa(e.GID)
	if e.UName != "" || e.GName != "" {
		owner = fallback(e.UName, strconv.Itoa(e.UID)) + ":" + fallback(e.GName, strconv.Itoa(e.GID))
	}
	size := "-"
	if e.Type == TypeRegular {
		size = strconv.FormatInt(e.Size, 10)
	}
	name := e.Path
	if e.Type == TypeSymlink || e.Type == TypeHardlink {
		name += " -> " + e.LinkTarget
	}
	return fmt.Sprintf("%s  %-16s %12s  %s  %s", modeString(e), owner, size, e.MTime.Format("2006-01-02 15:04"), name)
}

func fallback(value, otherwise string) string {
	if value != "" {
		return value
	}
	return otherwise
}

func modeString(e FileEntry) string {
	prefix := map[string]byte{TypeRegular: '-', TypeDir: 'd', TypeSymlink: 'l', TypeHardlink: 'h', TypeChar: 'c', TypeBlock: 'b', TypeFifo: 'p'}[e.Type]
	if prefix == 0 {
		prefix = '?'
	}
	n, err := ParseMode(e.Mode)
	if err != nil {
		return "??????????"
	}
	const bits = "rwxrwxrwx"
	var out [10]byte
	out[0] = prefix
	for i := 0; i < 9; i++ {
		if n&(1<<uint(8-i)) != 0 {
			out[i+1] = bits[i]
		} else {
			out[i+1] = '-'
		}
	}
	if n&0o4000 != 0 {
		if out[3] == 'x' {
			out[3] = 's'
		} else {
			out[3] = 'S'
		}
	}
	if n&0o2000 != 0 {
		if out[6] == 'x' {
			out[6] = 's'
		} else {
			out[6] = 'S'
		}
	}
	if n&0o1000 != 0 {
		if out[9] == 'x' {
			out[9] = 't'
		} else {
			out[9] = 'T'
		}
	}
	return string(out[:])
}

// WriteEntries streams entries in path-only, long, or JSON form.
func WriteEntries(w io.Writer, entries []FileEntry, long, asJSON bool) error {
	if asJSON {
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		for i := range entries {
			if i > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if err := enc.Encode(&entries[i]); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "]\n")
		return err
	}
	for _, e := range entries {
		line := e.Path
		if long {
			line = FormatLong(e)
		}
		if _, err := fmt.Fprintln(w, strings.TrimSpace(line)); err != nil {
			return err
		}
	}
	return nil
}
