package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// durationUnitsHelp documents the accepted syntax once, so every duration flag
// says the same thing.
const durationUnitsHelp = "units: s, m, h, d (days), w (weeks); e.g. 90m, 12h, 3d, 2w"

// parseHumanDuration parses a retention duration. It extends
// time.ParseDuration with the day and week suffixes an operator expects from a
// backup tool: "3d" is unambiguous where "72h" needs mental arithmetic.
// A bare number is rejected: the unit must be explicit.
func parseHumanDuration(value string) (time.Duration, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return 0, fmt.Errorf("empty duration (%s)", durationUnitsHelp)
	}
	negative := strings.HasPrefix(raw, "-")
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "-"), "+")
	if raw == "0" {
		return 0, nil
	}

	var total time.Duration
	rest := raw
	consumed := false
	for {
		// A "3d12h" style prefix is peeled off one day/week term at a time;
		// what remains is handed to time.ParseDuration, which owns h/m/s.
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 || i >= len(rest) || (rest[i] != 'd' && rest[i] != 'w') {
			break
		}
		n, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (%s)", value, durationUnitsHelp)
		}
		unit := 24 * time.Hour
		if rest[i] == 'w' {
			unit = 7 * 24 * time.Hour
		}
		total += time.Duration(n * float64(unit))
		rest = rest[i+1:]
		consumed = true
	}
	if rest != "" {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (%s)", value, durationUnitsHelp)
		}
		if d < 0 {
			return 0, fmt.Errorf("invalid duration %q: mixed signs", value)
		}
		total += d
	} else if !consumed {
		return 0, fmt.Errorf("invalid duration %q (%s)", value, durationUnitsHelp)
	}
	if negative {
		total = -total
	}
	return total, nil
}

// humanDuration is the pflag value behind every duration flag: it accepts the
// Go syntax plus the day and week suffixes.
type humanDuration struct{ d *time.Duration }

func newHumanDuration(target *time.Duration) *humanDuration { return &humanDuration{d: target} }

func (h *humanDuration) Set(value string) error {
	d, err := parseHumanDuration(value)
	if err != nil {
		return err
	}
	*h.d = d
	return nil
}

func (h *humanDuration) Type() string { return "duration" }

func (h *humanDuration) String() string {
	if h.d == nil || *h.d == 0 {
		return "0"
	}
	return formatHumanDuration(*h.d)
}

// formatHumanDuration renders a duration the way the flags accept it, so a
// value echoed back can be pasted into the next command.
func formatHumanDuration(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	if d%(24*time.Hour) == 0 {
		days := int64(d / (24 * time.Hour))
		if days%7 == 0 {
			return strconv.FormatInt(days/7, 10) + "w"
		}
		return strconv.FormatInt(days, 10) + "d"
	}
	return d.String()
}
