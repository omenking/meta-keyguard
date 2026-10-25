package keyguard

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var durationPart = regexp.MustCompile(`(\d+)([smhd])`)
var durationUnits = map[string]int64{"s": 1, "m": 60, "h": 3600, "d": 86400}

// ParseDuration reads durations such as 30m, 12h, 1d, or 1d12h, in seconds.
func ParseDuration(text string) (int64, error) {
	value := strings.ToLower(strings.TrimSpace(text))
	if value == "" || durationPart.ReplaceAllString(value, "") != "" {
		return 0, fmt.Errorf("invalid duration %q; use forms such as 30m, 12h, or 1d", text)
	}
	var seconds int64
	for _, m := range durationPart.FindAllStringSubmatch(value, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", text)
		}
		seconds += n * durationUnits[m[2]]
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("duration must be greater than zero")
	}
	return seconds, nil
}

// FormatDuration shows the two largest units, for example 11h 52m.
func FormatDuration(seconds int64) string {
	if seconds < 0 {
		return "-" + FormatDuration(-seconds)
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	days, rest := seconds/86400, seconds%86400
	hours, minutes := rest/3600, rest%3600/60
	var parts []string
	for _, p := range []struct {
		v    int64
		unit string
	}{{days, "d"}, {hours, "h"}, {minutes, "m"}} {
		if p.v > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", p.v, p.unit))
		}
	}
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, " ")
}
