package keyguard

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type flagSpec struct {
	name   string // with dashes, for example --ttl
	value  string // the metavar when the flag takes a value, empty for a switch
	repeat bool
	help   string
}

type commandSpec struct {
	name        string
	args        string // positional arguments, for usage
	required    int    // how many positionals are required
	maximum     int    // how many positionals are allowed
	help        string
	flags       []flagSpec
	passthrough bool // everything after -- is kept for the command
}

var commands = []commandSpec{
	{name: "install-bridge", help: "Register the Chrome native messaging host on Windows"},
	{name: "uninstall-bridge", help: "Remove the native messaging host from Windows"},
	{name: "doctor", help: "Check the bridge, the extension, the dev.meta.ai login, and Cost Center",
		flags: []flagSpec{{name: "--inspect", help: "Show the redacted shape of dev.meta.ai responses"}}},
	{name: "status", help: "Show keys, overdue deletions, failures, and the Cost Center state",
		flags: []flagSpec{{name: "--json", help: "Machine-readable status, without secrets (used by Cost Center)"}}},
	{name: "create", args: "<label>", required: 1, maximum: 1, help: "Create a key on dev.meta.ai and store it",
		flags: []flagSpec{
			{name: "--ttl", value: "DURATION", help: "How long the key lives (default: the default_ttl setting, 12h)"},
			{name: "--auto-rotate", help: "Replace the key shortly before it expires"},
			{name: "--ignore-cost-center", help: "Create even if a Cost Center alert blocks it"}}},
	{name: "list", help: "List stored keys", flags: []flagSpec{
		{name: "--all", help: "Also list keys on dev.meta.ai that Keyguard did not create"},
		{name: "--history", help: "Include revoked keys"}}},
	{name: "get", args: "<label>", required: 1, maximum: 1, help: "Print the secret"},
	{name: "env", args: "<label>", required: 1, maximum: 1, help: "Print an export line for eval",
		flags: []flagSpec{{name: "--var", value: "NAME", help: "Variable name (default: the env_var setting, META_API_KEY)"}}},
	{name: "exec", args: "<label> -- <command>", required: 1, maximum: 1, passthrough: true,
		help:  "Run a command with the key in its environment",
		flags: []flagSpec{{name: "--var", value: "NAME", repeat: true, help: "Variable name; repeat for several (default: META_API_KEY)"}}},
	{name: "rotate", args: "<label>", required: 1, maximum: 1, help: "Replace a key, deleting the old one after a grace period",
		flags: []flagSpec{{name: "--grace", value: "DURATION", help: "How long the old key keeps working (default: 5m)"}}},
	{name: "extend", args: "<label>", required: 1, maximum: 1, help: "Push back a key's expiry",
		flags: []flagSpec{{name: "--ttl", value: "DURATION", help: "New lifetime, measured from now (required)"}}},
	{name: "revoke", args: "<label>", required: 1, maximum: 1,
		help: "Delete a key now: a Keyguard label, or the dev.meta.ai name or ID of a key Keyguard did not create"},
	{name: "kill", help: "Delete every key on dev.meta.ai, leaving only a sentinel nobody holds", flags: []flagSpec{
		{name: "--yes", help: "Do not ask for confirmation"},
		{name: "--reason", value: "TEXT", help: "Recorded in the audit log (default: kill switch)"}}},
	{name: "sync", help: "Reconcile the store with dev.meta.ai"},
	{name: "reap", help: "Enforce TTLs and Cost Center limits now (the bridge does this every minute)"},
	{name: "bridge", help: "Native messaging host; started by Chrome, not by hand"},
	{name: "config", args: "[show|set|unset] [key] [value]", maximum: 3, help: "Show or change settings"},
	{name: "cost-center", args: "on|off|check", required: 1, maximum: 1,
		help: "Turn the Cost Center check on or off, or show what it reports"},
	{name: "log", help: "Show the audit log", flags: []flagSpec{{name: "-n", value: "N", help: "How many events (default: 30)"}}},
}

type parsed struct {
	command     string
	positionals []string
	flags       map[string][]string
	rest        []string
}

func (p parsed) has(name string) bool { _, ok := p.flags[name]; return ok }

func (p parsed) value(name string) string {
	if v := p.flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

type usageError struct {
	message string
	help    bool // asked for with --help, so it goes to stdout with status 0
}

func (e *usageError) Error() string { return e.message }

func usage() string {
	var b strings.Builder
	b.WriteString("usage: keyguard <command> [options]\n\nShort-lived, centrally stored Meta Model API keys.\n\ncommands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-18s %s\n", c.name, c.help)
	}
	b.WriteString("\nRun `keyguard <command> --help` for a command's options.\n")
	return b.String()
}

func commandUsage(c commandSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: keyguard %s", c.name)
	if len(c.flags) > 0 {
		b.WriteString(" [options]")
	}
	if c.args != "" {
		b.WriteString(" " + c.args)
	}
	b.WriteString("\n\n" + c.help + "\n")
	if len(c.flags) > 0 {
		b.WriteString("\noptions:\n")
		for _, f := range c.flags {
			name := f.name
			if f.value != "" {
				name += " " + f.value
			}
			fmt.Fprintf(&b, "  %-24s %s\n", name, f.help)
		}
	}
	return b.String()
}

// parseArgs parses a command line, allowing options before, after, and between positionals.
func parseArgs(args []string) (parsed, *commandSpec, error) {
	if len(args) == 0 {
		return parsed{}, nil, &usageError{message: usage()}
	}
	var spec *commandSpec
	for i := range commands {
		if commands[i].name == args[0] {
			spec = &commands[i]
		}
	}
	if spec == nil {
		return parsed{}, nil, &usageError{message: fmt.Sprintf("keyguard: unknown command %q\n\n%s", args[0], usage())}
	}
	p := parsed{command: spec.name, flags: map[string][]string{}}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if !spec.passthrough {
				p.positionals = append(p.positionals, args[i+1:]...)
				break
			}
			p.rest = args[i+1:]
			break
		}
		if arg == "-h" || arg == "--help" {
			return p, spec, &usageError{commandUsage(*spec), true}
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			p.positionals = append(p.positionals, arg)
			continue
		}
		name, inline, hasInline := strings.Cut(arg, "=")
		var flag *flagSpec
		for j := range spec.flags {
			if spec.flags[j].name == name {
				flag = &spec.flags[j]
			}
		}
		if flag == nil {
			return p, spec, &usageError{message: fmt.Sprintf("keyguard %s: unknown option %s\n\n%s", spec.name, name, commandUsage(*spec))}
		}
		if flag.value == "" {
			if hasInline {
				return p, spec, &usageError{message: fmt.Sprintf("keyguard %s: %s does not take a value", spec.name, name)}
			}
			p.flags[name] = append(p.flags[name], "")
			continue
		}
		if !hasInline {
			if i+1 >= len(args) {
				return p, spec, &usageError{message: fmt.Sprintf("keyguard %s: %s needs a value", spec.name, name)}
			}
			i++
			inline = args[i]
		}
		p.flags[name] = append(p.flags[name], inline)
	}
	if len(p.positionals) < spec.required || len(p.positionals) > spec.maximum {
		return p, spec, &usageError{message: fmt.Sprintf("keyguard %s: expected %s\n\n%s", spec.name, orNothing(spec.args), commandUsage(*spec))}
	}
	return p, spec, nil
}

func orNothing(args string) string {
	if args == "" {
		return "no arguments"
	}
	return args
}

// Main runs the CLI and returns its exit status.
func Main(args []string, extension fs.FS) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		fmt.Println("keyguard " + Version)
		return 0
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Print(usage())
		return 0
	}
	p, _, err := parseArgs(args)
	if err != nil {
		var u *usageError
		if errors.As(err, &u) && u.help {
			fmt.Print(u.message)
			return 0
		}
		fmt.Fprint(os.Stderr, strings.TrimRight(err.Error(), "\n")+"\n")
		return 2
	}
	if p.command == "bridge" {
		if err := RunBridge(); err != nil {
			return 1
		}
		return 0
	}
	store, err := OpenStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "keyguard: %v\n", err)
		return 1
	}
	defer store.Close()
	code, err := run(store, BridgeClient{}, p, extension, os.Stdout)
	if err != nil {
		var extErr *ExtensionError
		if errors.As(err, &extErr) {
			fmt.Fprintf(os.Stderr, "keyguard: %s: %s\n", extErr.Code, extErr.Error())
			if extErr.Detail != nil {
				detail, _ := json.MarshalIndent(extErr.Detail, "", "  ")
				fmt.Fprintln(os.Stderr, string(detail))
			}
			return 2
		}
		fmt.Fprintf(os.Stderr, "keyguard: %v\n", err)
		return 1
	}
	return code
}

func remaining(k Key, moment time.Time) string {
	due, ok := k.DueAt()
	if !ok {
		return "-"
	}
	left := int64(due.Sub(moment).Seconds())
	if left <= 0 {
		return "OVERDUE " + FormatDuration(-left)
	}
	return FormatDuration(left)
}

// created formats dev.meta.ai's creation times, which are Unix seconds.
func created(value any) string {
	switch v := value.(type) {
	case float64:
		return time.Unix(int64(v), 0).Format("2006-01-02 15:04")
	case string:
		return v
	}
	return ""
}

func table(rows [][]string, headers []string) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
		for _, r := range rows {
			widths[i] = max(widths[i], len(r[i]))
		}
	}
	line := func(cells []string) string {
		padded := make([]string, len(cells))
		for i, c := range cells {
			padded[i] = c + strings.Repeat(" ", widths[i]-len(c))
		}
		return strings.Join(padded, "  ")
	}
	lines := []string{line(headers)}
	for _, r := range rows {
		lines = append(lines, line(r))
	}
	return strings.Join(lines, "\n")
}

var keyHeaders = []string{"LABEL", "STATE", "EXPIRES IN", "AUTO", "META NAME", "LAST ERROR"}

func keyRows(keys []Key, moment time.Time) [][]string {
	rows := make([][]string, len(keys))
	for i, k := range keys {
		auto := ""
		if k.AutoRotate {
			auto = "yes"
		}
		rows[i] = []string{k.Label, k.State, remaining(k, moment), auto, k.MetaName, k.LastError}
	}
	return rows
}

func usable(store *Store, label string) (*Key, error) {
	k, err := store.ActiveKey(label)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, userErrorf("No active key labelled %s; create one with `keyguard create %s`", label, label)
	}
	if k.Overdue() {
		return nil, userErrorf("The key labelled %s has expired and is waiting to be deleted; "+
			"create a new one with `keyguard rotate %s` or `keyguard create`", label, label)
	}
	return k, nil
}

var shellSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func printReport(out io.Writer, report ReapReport) {
	for _, group := range []struct {
		name  string
		names []string
	}{{"expired", report.Expired}, {"retired", report.Retired}, {"rotated", report.Rotated}, {"cost center", report.CostCenter}} {
		if len(group.names) > 0 {
			fmt.Fprintf(out, "%s: %s\n", group.name, strings.Join(group.names, ", "))
		}
	}
	for _, f := range report.Failures {
		fmt.Fprintf(os.Stderr, "!! %s: %v\n", f.Name, f.Err)
	}
	if report.Empty() {
		fmt.Fprintln(out, "Nothing to do.")
	}
}

func status(store *Store, out io.Writer) (int, error) {
	moment := now()
	failure := store.LastFailure()
	if failure != nil {
		fmt.Fprintf(out, "!! Last failure (%s, %s): %s: %s\n", failure.At, failure.Op, failure.Code, failure.Message)
	}
	keys, err := store.LiveKeys()
	if err != nil {
		return 1, err
	}
	var overdue []string
	for _, k := range keys {
		if k.OverdueAt(moment) {
			overdue = append(overdue, k.Label)
		}
	}
	if len(overdue) > 0 {
		fmt.Fprintf(out, "!! %d key(s) are past their expiry but still exist on dev.meta.ai: %s. "+
			"Run `keyguard reap` or `keyguard doctor`.\n", len(overdue), strings.Join(overdue, ", "))
	}
	if len(keys) > 0 {
		fmt.Fprintln(out, table(keyRows(keys, moment), keyHeaders))
	} else {
		fmt.Fprintln(out, "No live keys.")
	}
	costCenterFailing := false
	if store.CostCenterEnabled() {
		last := store.LastCostCenterCheck()
		switch {
		case last == nil:
			fmt.Fprintln(out, "Cost Center: on, not checked yet")
		case !last.OK:
			costCenterFailing = true
			fmt.Fprintf(out, "!! Cost Center check failing (%s): %s\n", last.At, last.Error)
		case len(last.Tripped) > 0:
			for _, a := range last.Tripped {
				fmt.Fprintf(out, "!! Cost Center alert %q exceeded; revoking keys matching %v\n", a["label"], a["key_match"])
			}
		default:
			fmt.Fprintf(out, "Cost Center: on, no limits exceeded (checked %s)\n", last.At)
		}
	} else {
		fmt.Fprintln(out, "Cost Center: off")
	}
	if len(overdue) > 0 || failure != nil || costCenterFailing {
		return 1, nil
	}
	return 0, nil
}

func rawSetting(store *Store, key string) any {
	if value := store.Setting(key); value != "" && json.Valid([]byte(value)) {
		return json.RawMessage(value)
	}
	return nil
}

// StatusJSON is what Meta Cost Center shows about Keyguard. It never includes a secret.
func StatusJSON(store *Store) (map[string]any, error) {
	moment := now()
	live, err := store.LiveKeys()
	if err != nil {
		return nil, err
	}
	keys := []map[string]any{}
	for _, k := range live {
		if k.State == StateSentinel {
			continue
		}
		var expires any
		if due, ok := k.DueAt(); ok {
			expires = ISO(due)
		}
		keys = append(keys, map[string]any{"label": k.Label, "state": k.State, "meta_name": nullable(k.MetaName),
			"expires_at": expires, "overdue": k.OverdueAt(moment), "auto_rotate": k.AutoRotate, "last_error": nullable(k.LastError)})
	}
	targets, snapshotAt, err := (&Ops{Store: store}).Targets()
	if err != nil {
		return nil, err
	}
	revocations := []map[string]any{}
	for _, e := range store.Events(500) {
		if e.Kind != "cost-center-revoked" {
			continue
		}
		detail := map[string]any{}
		json.Unmarshal([]byte(e.Detail), &detail)
		detail["at"] = e.At
		revocations = append(revocations, detail)
	}
	if len(revocations) > 50 {
		revocations = revocations[len(revocations)-50:]
	}
	bridgeRunning := false
	if path, err := SocketPath(); err == nil {
		_, statErr := os.Stat(path)
		bridgeRunning = statErr == nil
	}
	return map[string]any{"keyguard_version": Version, "generated_at": ISO(moment), "bridge_running": bridgeRunning,
		"cost_center":  map[string]any{"enabled": store.CostCenterEnabled(), "last_check": rawSetting(store, "cost_center_last")},
		"last_failure": rawSetting(store, "last_failure"), "keys": keys, "targets": targets,
		"targets_as_of": nullable(snapshotAt), "revocations": revocations}, nil
}

func doctor(store *Store, ext Extension, extension fs.FS, inspect bool, out io.Writer) int {
	ok := true
	report := func(good bool, text string) {
		ok = ok && good
		if good {
			fmt.Fprintln(out, "ok   "+text)
		} else {
			fmt.Fprintln(out, "FAIL "+text)
		}
	}
	report(true, fmt.Sprintf("keyguard %s (%s)", Version, runtime.Version()))
	distro := os.Getenv("WSL_DISTRO_NAME")
	if distro == "" {
		distro = "not detected"
	}
	report(os.Getenv("WSL_DISTRO_NAME") != "", "WSL distro: "+distro)
	info, err := os.Stat(DataDir())
	report(err == nil && info.Mode().Perm()&0o077 == 0, "Data directory is private: "+DataDir())
	found, err := CheckInstall(extension)
	if err != nil {
		report(false, fmt.Sprintf("Could not inspect the Windows install: %v", err))
	} else if !found.Registered {
		report(false, "Native messaging host not registered; run `keyguard install-bridge`")
	} else {
		report(true, "Native messaging host registered")
		if found.ExtensionCurrent != nil && *found.ExtensionCurrent {
			report(true, "Installed extension matches this keyguard binary")
		} else {
			report(false, "Installed extension is out of date; run `keyguard install-bridge` and reload the extension in chrome://extensions")
		}
		if found.HostCommandCurrent != nil && *found.HostCommandCurrent {
			report(true, "Host launcher points at this keyguard binary")
		} else {
			report(false, "Host launcher is stale; run `keyguard install-bridge`")
		}
	}
	ops := &Ops{Store: store, Ext: ext}
	if err := doctorBridge(ops, ext, inspect, report, out); err != nil {
		var extErr *ExtensionError
		if errors.As(err, &extErr) {
			report(false, extErr.Code+": "+extErr.Error())
		} else {
			report(false, err.Error())
		}
	}
	if store.CostCenterEnabled() {
		alerts, err := store.CostCenterStatuses(2 * time.Minute)
		if err != nil {
			report(false, err.Error())
		} else {
			revoking := 0
			for _, a := range alerts {
				if a.Bool("revoke_keys") {
					revoking++
				}
			}
			report(true, fmt.Sprintf("Cost Center answered (%d alert(s), %d revoking keys)", len(alerts), revoking))
		}
	} else {
		fmt.Fprintln(out, "     Cost Center check is off (`keyguard cost-center on` to enable)")
	}
	if ok {
		return 0
	}
	return 1
}

func doctorBridge(ops *Ops, ext Extension, inspect bool, report func(bool, string), out io.Writer) error {
	info, err := ext.Call("bridge-info", nil, defaultTimeout)
	if err != nil {
		return err
	}
	pid, _ := info["pid"].(float64)
	report(true, fmt.Sprintf("Bridge running (pid %d)", int64(pid)))
	// Chrome keeps one bridge process alive, so it does not see a new binary until it restarts.
	startedAt, _ := info["started_at"].(float64)
	if exe, err := Executable(); err == nil {
		if stat, err := os.Stat(exe); err == nil {
			if startedAt >= float64(stat.ModTime().UnixNano())/1e9 {
				report(true, "Bridge is running the current binary")
			} else {
				report(false, "Bridge is running an older binary; reload the extension in chrome://extensions to restart it")
			}
		}
	}
	report(info["extension_version"] != nil, fmt.Sprintf("Extension connected (version %v)", info["extension_version"]))
	who, err := ops.Call("whoami", nil)
	if err != nil {
		return err
	}
	team, _ := who["team"].(map[string]any)
	name := stringField(team, "name")
	if name == "" {
		name = stringField(team, "id")
	}
	canCreate := who["can_create"]
	if canCreate == nil {
		canCreate = "unknown"
	}
	report(true, fmt.Sprintf("Signed in to dev.meta.ai; team %s (%v key(s), can create: %v)", name, who["key_count"], canCreate))
	if reason, _ := who["block_reason"].(string); reason != "" {
		report(false, "dev.meta.ai blocks key creation: "+reason)
	}
	if inspect {
		shape, err := ops.Call("inspect", nil)
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(shape, "", "  ")
		fmt.Fprintln(out, string(data))
	}
	return nil
}

func config(store *Store, p parsed, out io.Writer) error {
	action, key, value := "show", "", ""
	hasValue := false
	if len(p.positionals) > 0 {
		action = p.positionals[0]
	}
	if len(p.positionals) > 1 {
		key = p.positionals[1]
	}
	if len(p.positionals) > 2 {
		value, hasValue = p.positionals[2], true
	}
	switch action {
	case "show":
		for _, name := range SettingNames() {
			fmt.Fprintf(out, "%s = %s\n", name, store.Setting(name))
		}
		return nil
	case "set", "unset":
	default:
		return userErrorf("config takes show, set, or unset")
	}
	if key == "" {
		return userErrorf("Name a setting: %s", strings.Join(SettingNames(), ", "))
	}
	if _, known := Defaults[key]; !known {
		return userErrorf("Unknown setting %s; choose from %s", key, strings.Join(SettingNames(), ", "))
	}
	if action == "unset" {
		return store.SetSetting(key, nil)
	}
	if !hasValue {
		return userErrorf("Give a value")
	}
	for _, name := range DurationSettings {
		if name == key {
			if _, err := ParseDuration(value); err != nil {
				return err
			}
		}
	}
	if key == "cost_center" && value != "on" && value != "off" {
		return userErrorf("cost_center is on or off")
	}
	if err := store.SetSetting(key, &value); err != nil {
		return err
	}
	store.Log("config", "", "", map[string]any{"key": key, "value": value})
	return nil
}

func durationFlag(p parsed, name string) (int64, error) {
	if !p.has(name) {
		return 0, nil
	}
	seconds, err := ParseDuration(p.value(name))
	if err != nil {
		return 0, &usageError{message: fmt.Sprintf("keyguard %s: %s: %v", p.command, name, err)}
	}
	return seconds, nil
}

func run(store *Store, ext Extension, p parsed, extension fs.FS, out io.Writer) (int, error) {
	ops := &Ops{Store: store, Ext: ext}
	moment := now()
	switch p.command {
	case "install-bridge":
		where, err := Install(extension, store)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Installed the native messaging host in %s.\n\n"+
			"If the extension is not loaded yet:\n"+
			"  1. Open chrome://extensions and turn on Developer mode.\n"+
			"  2. Click \"Load unpacked\" and choose %s\n"+
			"If it is already loaded, click its reload button. Then run `keyguard doctor`.\n", where.Directory, where.Extension)
		return 0, nil
	case "uninstall-bridge":
		where, err := Uninstall()
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Removed %s and the registry entry.\n", where)
		return 0, nil
	case "status":
		if p.has("--json") {
			result, err := StatusJSON(store)
			if err != nil {
				return 1, err
			}
			data, _ := json.MarshalIndent(result, "", "  ")
			fmt.Fprintln(out, string(data))
			return 0, nil
		}
		return status(store, out)
	case "doctor":
		return doctor(store, ext, extension, p.has("--inspect"), out), nil
	case "config":
		return 0, config(store, p, out)
	case "log":
		limit := 30
		if p.has("-n") {
			n, err := strconv.Atoi(p.value("-n"))
			if err != nil || n < 1 {
				return 1, userErrorf("-n takes a positive number")
			}
			limit = n
		}
		for _, e := range store.Events(limit) {
			detail := ""
			if e.Detail != "" {
				detail = " " + e.Detail
			}
			fmt.Fprintf(out, "%s  %-18s %-24s%s\n", e.At, e.Kind, e.Label, detail)
		}
		return 0, nil
	case "cost-center":
		action := p.positionals[0]
		switch action {
		case "on", "off":
			if err := store.SetSetting("cost_center", &action); err != nil {
				return 1, err
			}
			store.Log("config", "", "", map[string]any{"key": "cost_center", "value": action})
			fmt.Fprintf(out, "Cost Center check is %s.\n", action)
			if action == "off" {
				return 0, nil
			}
		case "check":
		default:
			return 1, userErrorf("cost-center takes on, off, or check")
		}
		alerts, err := store.CheckCostCenter()
		if err != nil {
			return 1, err
		}
		if len(alerts) == 0 {
			fmt.Fprintln(out, "No alert is revoking keys.")
		}
		for _, a := range alerts {
			fmt.Fprintf(out, "Exceeded: %s -> revokes %s\n", a.String("label"), a.String("key_match"))
		}
		return 0, nil
	case "get", "env", "exec":
		k, err := usable(store, p.positionals[0])
		if err != nil {
			return 1, err
		}
		switch p.command {
		case "get":
			fmt.Fprintln(out, k.Secret)
		case "env":
			name := p.value("--var")
			if name == "" {
				name = store.Setting("env_var")
			}
			fmt.Fprintf(out, "export %s=%s\n", name, shellQuote(k.Secret))
		case "exec":
			return 1, execWithKey(store, k, p)
		}
		return 0, nil
	case "list":
		var keys []Key
		var err error
		if p.has("--history") {
			keys, err = store.AllKeys()
		} else {
			keys, err = store.LiveKeys()
		}
		if err != nil {
			return 1, err
		}
		if len(keys) > 0 {
			fmt.Fprintln(out, table(keyRows(keys, moment), keyHeaders))
		} else {
			fmt.Fprintln(out, "No keys stored.")
		}
		if p.has("--all") {
			unlock, err := Lock()
			if err != nil {
				return 1, err
			}
			unmanaged, err := ops.Sync()
			unlock()
			if err != nil {
				return 1, err
			}
			fmt.Fprintln(out, "\nOn dev.meta.ai but not created by Keyguard:")
			if len(unmanaged) == 0 {
				fmt.Fprintln(out, "  none")
			} else {
				rows := make([][]string, len(unmanaged))
				for i, k := range unmanaged {
					rows[i] = []string{k.Name, k.ID, created(k.CreatedAt)}
				}
				fmt.Fprintln(out, table(rows, []string{"NAME", "ID", "CREATED"}))
			}
		}
		return 0, nil
	case "extend":
		if !p.has("--ttl") {
			return 1, &usageError{message: "keyguard extend: --ttl is required"}
		}
		ttl, err := durationFlag(p, "--ttl")
		if err != nil {
			return 1, err
		}
		k, err := ops.Extend(p.positionals[0], ttl)
		if err != nil {
			return 1, err
		}
		unlock, err := Lock()
		if err != nil {
			return 1, err
		}
		defer unlock()
		if err := ops.PushSchedule(); err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "%s now expires %s.\n", p.positionals[0], k.ExpiresAt)
		return 0, nil
	case "kill":
		if !p.has("--yes") {
			if !isTerminal(os.Stdin) {
				return 1, userErrorf("kill deletes every key on dev.meta.ai; pass --yes to run it non-interactively")
			}
			fmt.Fprint(out, "This deletes EVERY key on your dev.meta.ai team, including ones Keyguard did not create.\n"+
				"Type kill to continue: ")
			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(answer) != "kill" {
				fmt.Fprintln(out, "Cancelled.")
				return 1, nil
			}
		}
	}
	return locked(ops, p, out)
}

// locked runs the commands that change keys, holding the lock the bridge's reaper also takes.
func locked(ops *Ops, p parsed, out io.Writer) (int, error) {
	unlock, err := Lock()
	if err != nil {
		return 1, err
	}
	defer unlock()
	store := ops.Store
	switch p.command {
	case "create":
		ttl, err := durationFlag(p, "--ttl")
		if err != nil {
			return 1, err
		}
		label := p.positionals[0]
		k, err := ops.Create(label, ttl, p.has("--auto-rotate"), p.has("--ignore-cost-center"))
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Created %s; it expires in %s.\nUse it with: keyguard exec %s -- <command>\n",
			k.MetaName, remaining(*k, now()), label)
	case "rotate":
		grace := int64(-1)
		if p.has("--grace") {
			var err error
			if grace, err = durationFlag(p, "--grace"); err != nil {
				return 1, err
			}
		}
		k, err := ops.Rotate(p.positionals[0], grace, "rotated")
		if err != nil {
			return 1, err
		}
		after := store.Setting("rotate_grace")
		if grace >= 0 {
			after = FormatDuration(grace)
		}
		fmt.Fprintf(out, "Rotated %s to %s; the old key is deleted after %s.\n", p.positionals[0], k.MetaName, after)
	case "revoke":
		label := p.positionals[0]
		keys, err := ops.Revoke(label, "revoked by user")
		var userErr *UserError
		switch {
		case err == nil:
			fmt.Fprintf(out, "Revoked %d key(s) labelled %s.\n", len(keys), label)
		case errors.As(err, &userErr) && keys == nil:
			// Not a live Keyguard label, so it may name a key Keyguard did not create.
			target, err := ops.RevokeRemote(label, "revoked by user")
			if err != nil {
				return 1, err
			}
			name := target.Name
			if name == "" {
				name = target.ID
			}
			fmt.Fprintf(out, "Deleted %s from dev.meta.ai.\n", name)
		default:
			return 1, err
		}
	case "kill":
		reason := p.value("--reason")
		if reason == "" {
			reason = "kill switch"
		}
		keys, unmanaged, err := ops.Kill(reason)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Deleted %d managed and %d unmanaged key(s). Only the sentinel key, whose secret nobody has, remains.\n",
			len(keys), len(unmanaged))
	case "sync":
		unmanaged, err := ops.Sync()
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Store reconciled. %d key(s) on dev.meta.ai were not created by Keyguard.\n", len(unmanaged))
		return 0, nil
	case "reap":
		report, err := ops.Reap(now())
		if err != nil {
			return 1, err
		}
		printReport(out, report)
		if len(report.Failures) > 0 {
			return 1, nil
		}
		return 0, nil
	}
	return 0, ops.PushSchedule()
}

// execWithKey replaces this process with the command, the key in its environment. It returns only on failure.
func execWithKey(store *Store, k *Key, p parsed) error {
	argv := p.rest
	if len(argv) == 0 {
		return userErrorf("Give a command after --, for example: keyguard exec dev -- python app.py")
	}
	names := p.flags["--var"]
	if len(names) == 0 {
		names = []string{store.Setting("env_var")}
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	replaced := map[string]bool{}
	for _, name := range names {
		replaced[name] = true
	}
	// getenv returns the first match, so drop any existing value rather than appending a second.
	var env []string
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); !replaced[name] {
			env = append(env, entry)
		}
	}
	for _, name := range names {
		env = append(env, name+"="+k.Secret)
	}
	store.Close()
	return syscall.Exec(path, argv, env)
}
