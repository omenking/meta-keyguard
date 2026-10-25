package keyguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path"
	"strings"
	"time"
)

// CostCenterError means Meta Cost Center could not be run or answered unexpectedly.
type CostCenterError struct{ Message string }

func (e *CostCenterError) Error() string { return e.Message }

// Alert is one Cost Center alert, as printed by `muse-cost alert status --json`.
type Alert map[string]any

func (a Alert) String(key string) string {
	v, _ := a[key].(string)
	return v
}

func (a Alert) Bool(key string) bool {
	v, _ := a[key].(bool)
	return v
}

func (a Alert) ID() any { return a["id"] }

func (a Alert) Patterns() []string {
	var patterns []string
	for _, p := range strings.Split(a.String("key_match"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}

// Matches reports whether a Keyguard label or dev.meta.ai name is selected by the alert's key match.
// Cost Center only allows letters, digits, '.', '_', '-', '*' and '?', so path.Match behaves like fnmatch here.
func (a Alert) Matches(name string) bool {
	for _, p := range a.Patterns() {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func (s *Store) CostCenterEnabled() bool { return s.Setting("cost_center") == "on" }

// splitCommand splits a command line on spaces, honouring single and double quotes.
func splitCommand(line string) []string {
	var words []string
	var current strings.Builder
	var quote rune
	inWord := false
	for _, r := range line {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '\'' || r == '"'):
			quote, inWord = r, true
		case quote == 0 && (r == ' ' || r == '\t'):
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, current.String())
	}
	return words
}

// CostCenterStatuses runs `muse-cost alert status --json` and returns its alerts.
func (s *Store) CostCenterStatuses(timeout time.Duration) ([]Alert, error) {
	argv := append(splitCommand(s.Setting("cost_center_command")), "alert", "status", "--json")
	if len(argv) == 3 {
		return nil, &CostCenterError{"The cost_center_command setting is empty"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return nil, &CostCenterError{fmt.Sprintf("Cost Center command not found: %s. Set it with "+
			"`meta-keyguard config set cost_center_command /path/to/muse-cost`.", argv[0])}
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, &CostCenterError{"Cost Center did not answer within two minutes"}
	}
	if err != nil {
		output := strings.TrimSpace(stderr.String())
		if output == "" {
			output = strings.TrimSpace(stdout.String())
		}
		lines := strings.Split(output, "\n")
		return nil, &CostCenterError{"Cost Center failed: " + lines[len(lines)-1]}
	}
	var alerts []Alert
	if err := json.Unmarshal(stdout.Bytes(), &alerts); err != nil {
		return nil, &CostCenterError{"Cost Center did not print a JSON list of alerts; update it to a version with `alert status`"}
	}
	return alerts, nil
}

// Tripped are the alerts that have passed their limit and are set to revoke keys.
func Tripped(alerts []Alert) []Alert {
	var tripped []Alert
	for _, a := range alerts {
		enabled, present := a["enabled"].(bool)
		if a.Bool("revoke_keys") && (enabled || !present) && a.String("level") == "exceeded" {
			tripped = append(tripped, a)
		}
	}
	return tripped
}

// CostCenterCheck is the result of the last check, shown by `status` and by Cost Center itself.
type CostCenterCheck struct {
	At      string           `json:"at"`
	OK      bool             `json:"ok"`
	Error   string           `json:"error,omitempty"`
	Tripped []map[string]any `json:"tripped"`
}

// CheckCostCenter returns the tripped alerts, recording the result so `status` can show it.
func (s *Store) CheckCostCenter() ([]Alert, error) {
	alerts, err := s.CostCenterStatuses(2 * time.Minute)
	if err != nil {
		s.SetJSON("cost_center_last", CostCenterCheck{At: ISO(now()), OK: false, Error: err.Error()})
		return nil, err
	}
	tripped := Tripped(alerts)
	summary := []map[string]any{}
	for _, a := range tripped {
		summary = append(summary, map[string]any{"id": a.ID(), "label": a.String("label"), "key_match": a.String("key_match")})
	}
	s.SetJSON("cost_center_last", CostCenterCheck{At: ISO(now()), OK: true, Tripped: summary})
	return tripped, nil
}

func (s *Store) LastCostCenterCheck() *CostCenterCheck {
	var check CostCenterCheck
	if s.JSONSetting("cost_center_last", &check) {
		return &check
	}
	return nil
}
