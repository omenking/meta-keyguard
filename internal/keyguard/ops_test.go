package keyguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeMeta stands in for the extension and dev.meta.ai, including the last-key rule.
type fakeMeta struct {
	keys     []RemoteKey
	nextID   int
	calls    []fakeCall
	fail     map[string]string
	schedule []any
	noSecret bool
}

type fakeCall struct {
	op     string
	params Params
}

func newFakeMeta() *fakeMeta {
	return &fakeMeta{keys: []RemoteKey{{ID: "manual-1", Name: "hand-made"}}, nextID: 100, fail: map[string]string{}}
}

func (m *fakeMeta) Call(op string, params Params, timeout time.Duration) (map[string]any, error) {
	recorded := Params{}
	for k, v := range params {
		if k != "team_id" && k != "project_id" {
			recorded[k] = v
		}
	}
	m.calls = append(m.calls, fakeCall{op, recorded})
	if code, ok := m.fail[op]; ok {
		return nil, NewExtensionError(code)
	}
	switch op {
	case "list":
		keys := make([]any, len(m.keys))
		for i, k := range m.keys {
			keys[i] = map[string]any{"id": k.ID, "name": k.Name}
		}
		return map[string]any{"team": map[string]any{"id": "t1", "name": "Me"}, "keys": keys}, nil
	case "create":
		m.nextID++
		key := RemoteKey{ID: fmt.Sprint(m.nextID), Name: params["name"].(string)}
		m.keys = append(m.keys, key)
		var secret any = fmt.Sprintf("sk-meta-%d-secret", m.nextID)
		if params["discard_secret"] == true || m.noSecret {
			secret = nil
		}
		return map[string]any{"id": key.ID, "name": key.Name, "secret": secret}, nil
	case "delete":
		id := params["id"].(string)
		index := -1
		for i, k := range m.keys {
			if k.ID == id {
				index = i
			}
		}
		if index < 0 {
			return map[string]any{"deleted": false, "already_gone": true}, nil
		}
		if len(m.keys) == 1 {
			return nil, NewExtensionError("last_key")
		}
		m.keys = append(m.keys[:index:index], m.keys[index+1:]...)
		return map[string]any{"deleted": true}, nil
	case "schedule":
		data, _ := json.Marshal(params["keys"])
		m.schedule = nil
		json.Unmarshal(data, &m.schedule)
		return map[string]any{"scheduled": len(m.schedule)}, nil
	case "whoami":
		return map[string]any{"team": map[string]any{"id": "t1", "name": "Me"}, "key_count": len(m.keys), "can_create": true}, nil
	}
	panic(op)
}

func (m *fakeMeta) names() []string {
	var names []string
	for _, k := range m.keys {
		names = append(names, k.Name)
	}
	sort.Strings(names)
	return names
}

func (m *fakeMeta) has(id string) bool {
	for _, k := range m.keys {
		if k.ID == id {
			return true
		}
	}
	return false
}

type fixture struct {
	t     *testing.T
	dir   string
	store *Store
	meta  *fakeMeta
	ops   *Ops
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("META_KEYGUARD_DATA", filepath.Join(dir, "data"))
	store, err := OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	meta := newFakeMeta()
	return &fixture{t, dir, store, meta, &Ops{Store: store, Ext: meta}}
}

func (f *fixture) create(label string, ttl int64, autoRotate bool) *Key {
	f.t.Helper()
	k, err := f.ops.Create(label, ttl, autoRotate, false)
	if err != nil {
		f.t.Fatalf("create %s: %v", label, err)
	}
	return k
}

func (f *fixture) set(key, value string) {
	if err := f.store.SetSetting(key, &value); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) reap(after time.Duration) ReapReport {
	f.t.Helper()
	report, err := f.ops.Reap(now().Add(after))
	if err != nil {
		f.t.Fatal(err)
	}
	return report
}

func (f *fixture) byMetaID(id string) *Key {
	k, err := f.store.ByMetaID(id)
	if err != nil || k == nil {
		f.t.Fatalf("no key %s: %v", id, err)
	}
	return k
}

func equal[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func isUserError(err error) bool {
	var u *UserError
	return errors.As(err, &u)
}

func TestDurations(t *testing.T) {
	for text, want := range map[string]int64{"12h": 43200, "1d12h": 129600, "90s": 90} {
		got, err := ParseDuration(text)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %d, %v", text, got, err)
		}
	}
	for _, bad := range []string{"", "12", "h", "1w", "0m", "-5m", "5 m"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) accepted", bad)
		}
	}
	equal(t, FormatDuration(43200-480), "11h 52m")
	equal(t, FormatDuration(129600), "1d 12h")
}

func TestStoreIsPrivate(t *testing.T) {
	setup(t)
	dir, _ := os.Stat(DataDir())
	db, _ := os.Stat(DBPath())
	equal(t, dir.Mode().Perm(), os.FileMode(0o700))
	equal(t, db.Mode().Perm(), os.FileMode(0o600))
}

func TestSocketPathStaysWithinTheUnixLimit(t *testing.T) {
	f := setup(t)
	t.Setenv("META_KEYGUARD_DATA", filepath.Join(f.dir, strings.Repeat("deep", 30)))
	path, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if len(path) > maxSocketPath {
		t.Fatalf("%s is %d bytes", path, len(path))
	}
}

func TestCreateUsesDefaultTTLAndRejectsBadLabelsAndLongTTLs(t *testing.T) {
	f := setup(t)
	k := f.create("dev", 0, false)
	equal(t, k.TTLSeconds, int64(12*3600))
	if !strings.HasPrefix(k.MetaName, "dev-kg") || !strings.HasPrefix(k.Secret, "sk-meta-") {
		t.Fatalf("unexpected key %+v", k)
	}
	if _, err := f.ops.Create("dev", 0, false, false); !isUserError(err) {
		t.Fatalf("second active key for a label: %v", err)
	}
	for _, bad := range []string{"Dev", "-x", "a b", "keyguard-sentinel-2", strings.Repeat("x", 41)} {
		if _, err := f.ops.Create(bad, 0, false, false); !isUserError(err) {
			t.Errorf("label %q: %v", bad, err)
		}
	}
	eightDays, _ := ParseDuration("8d")
	if _, err := f.ops.Create("long", eightDays, false, false); !isUserError(err) {
		t.Fatalf("long TTL: %v", err)
	}
	f.set("default_ttl", "30m")
	equal(t, f.create("short", 0, false).TTLSeconds, int64(1800))
}

func TestSecretsNeverReachTheAuditLog(t *testing.T) {
	f := setup(t)
	k := f.create("dev", 0, false)
	if _, err := f.ops.Rotate("dev", 0, "rotated"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.ops.Kill("kill switch"); err != nil {
		t.Fatal(err)
	}
	log, _ := json.Marshal(f.store.Events(1000))
	if strings.Contains(string(log), k.Secret) || strings.Contains(string(log), "sk-meta-") {
		t.Fatal("a secret reached the audit log")
	}
}

func TestRotateKeepsTheOldKeyForTheGracePeriod(t *testing.T) {
	f := setup(t)
	old := f.create("dev", 3600, false)
	fresh, err := f.ops.Rotate("dev", 300, "rotated")
	if err != nil {
		t.Fatal(err)
	}
	if old.Secret == fresh.Secret {
		t.Fatal("rotation kept the secret")
	}
	active, _ := f.store.ActiveKey("dev")
	equal(t, active.ID, fresh.ID)
	equal(t, fresh.TTLSeconds, int64(3600))
	equal(t, f.byMetaID(old.MetaID).State, StateRetiring)
	if !f.meta.has(old.MetaID) {
		t.Fatal("old key deleted before the grace period")
	}
	equal(t, f.reap(301*time.Second).Retired, []string{"dev"})
	if f.meta.has(old.MetaID) {
		t.Fatal("old key not deleted after the grace period")
	}
	equal(t, f.byMetaID(old.MetaID).Secret, "")
}

func TestReapDeletesExpiredKeysAndAutoRotates(t *testing.T) {
	f := setup(t)
	f.create("plain", 600, false)
	f.create("rolling", 1200, true)
	f.set("rotate_grace", "20m")
	// Five minutes before 'rolling' expires it is inside rotate_before (10m); 'plain' has expired.
	report := f.reap(900 * time.Second)
	equal(t, report.Expired, []string{"plain"})
	equal(t, report.Rotated, []string{"rolling"})
	if k, _ := f.store.ActiveKey("plain"); k != nil {
		t.Fatal("plain is still active")
	}
	rolling := 0
	for _, k := range f.meta.keys {
		if strings.HasPrefix(k.Name, "rolling-") {
			rolling++
		}
	}
	equal(t, rolling, 2)
	labels := map[string]bool{}
	for _, entry := range f.meta.schedule {
		labels[entry.(map[string]any)["label"].(string)] = true
	}
	equal(t, labels, map[string]bool{"rolling": true})
}

func TestShortAutoRotatingKeysAreNotRotatedEveryMinute(t *testing.T) {
	f := setup(t)
	k := f.create("quick", 120, true)
	// Well inside the default 10m rotate_before, but not within a quarter of the 2m TTL.
	equal(t, f.reap(60*time.Second).Rotated, []string(nil))
	equal(t, f.reap(95*time.Second).Rotated, []string{"quick"})
	equal(t, f.byMetaID(k.MetaID).State, StateRetiring)
}

func TestDeletingTheLastKeyCreatesASentinel(t *testing.T) {
	f := setup(t)
	f.meta.keys = nil
	k := f.create("only", 0, false)
	if _, err := f.ops.Revoke("only", "revoked by user"); err != nil {
		t.Fatal(err)
	}
	equal(t, f.meta.names(), []string{SentinelName})
	live, _ := f.store.LiveKeys()
	equal(t, len(live), 1)
	equal(t, live[0].State, StateSentinel)
	equal(t, live[0].Secret, "")
	found := false
	for _, c := range f.meta.calls {
		found = found || (c.op == "create" && reflect.DeepEqual(c.params, Params{"name": SentinelName, "discard_secret": true}))
	}
	if !found {
		t.Fatal("no sentinel created with discard_secret")
	}
	equal(t, f.byMetaID(k.MetaID).State, StateRevoked)
}

func TestKillDeletesEveryKeyIncludingUnmanagedOnes(t *testing.T) {
	f := setup(t)
	f.create("a", 0, false)
	f.create("b", 0, false)
	keys, unmanaged, err := f.ops.Kill("test")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, []string{keys[0].Label, keys[1].Label}, []string{"a", "b"})
	equal(t, []string{unmanaged[0].Name}, []string{"hand-made"})
	equal(t, f.meta.names(), []string{SentinelName})
	live, _ := f.store.LiveKeys()
	equal(t, len(live), 1)
	equal(t, live[0].State, StateSentinel)
	// A second kill has nothing left to do and creates no further sentinel.
	if _, _, err := f.ops.Kill("again"); err != nil {
		t.Fatal(err)
	}
	equal(t, f.meta.names(), []string{SentinelName})
}

func TestFailuresAreRecordedAndKeysKeptForRetry(t *testing.T) {
	f := setup(t)
	f.create("dev", 60, false)
	f.meta.fail["delete"] = "not_logged_in"
	report := f.reap(61 * time.Second)
	equal(t, len(report.Failures), 1)
	var extErr *ExtensionError
	if report.Failures[0].Name != "dev" || !errors.As(report.Failures[0].Err, &extErr) || extErr.Code != "not_logged_in" {
		t.Fatalf("failures: %+v", report.Failures)
	}
	k, _ := f.store.ActiveKey("dev")
	if !k.OverdueAt(now().Add(61*time.Second)) || !strings.Contains(k.LastError, "not_logged_in") {
		t.Fatalf("key after a failed delete: %+v", k)
	}
	equal(t, f.store.LastFailure().Code, "not_logged_in")
	delete(f.meta.fail, "delete")
	f.reap(61 * time.Second)
	if k, _ := f.store.ActiveKey("dev"); k != nil {
		t.Fatal("dev survived the retry")
	}
	if f.store.LastFailure() != nil {
		t.Fatal("the failure was not cleared by a successful delete")
	}
}

func TestSyncMarksMissingKeysAndReportsUnmanaged(t *testing.T) {
	f := setup(t)
	k := f.create("dev", 0, false)
	f.meta.keys = f.meta.keys[:1]
	unmanaged, err := f.ops.Sync()
	if err != nil {
		t.Fatal(err)
	}
	equal(t, []string{unmanaged[0].Name}, []string{"hand-made"})
	equal(t, f.byMetaID(k.MetaID).RevokeReason, "missing on dev.meta.ai")
}

func TestMissingSecretIsAnError(t *testing.T) {
	f := setup(t)
	f.meta.noSecret = true
	_, err := f.ops.Create("dev", 0, false, false)
	var extErr *ExtensionError
	if !errors.As(err, &extErr) || extErr.Code != "secret_not_found" {
		t.Fatalf("got %v", err)
	}
}

// fakeCostCenter installs a script that prints the alerts as `muse-cost alert status --json` would.
func (f *fixture) fakeCostCenter(alerts []map[string]any, code int) {
	f.t.Helper()
	data, _ := json.Marshal(alerts)
	script := filepath.Join(f.dir, "muse-cost")
	body := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n%s\nEOF\nexit %d\n", data, code)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		f.t.Fatal(err)
	}
	f.set("cost_center_command", script)
}

func TestCostCenterOffByDefaultAndNeverConsulted(t *testing.T) {
	f := setup(t)
	f.fakeCostCenter(nil, 1)
	f.create("dev", 0, false)
	equal(t, len(f.reap(0).Failures), 0)
}

func TestExceededAlertRevokesMatchingKeysOnly(t *testing.T) {
	f := setup(t)
	f.create("bootcamp-api", 0, false)
	f.create("bootcamp-web", 0, false)
	f.create("other", 0, false)
	f.meta.keys = append(f.meta.keys, RemoteKey{ID: "manual-2", Name: "bootcamp-manual"})
	f.fakeCostCenter([]map[string]any{
		{"id": 1, "label": "Daily", "level": "exceeded", "enabled": true, "revoke_keys": true, "key_match": "bootcamp-*"},
		{"id": 2, "label": "Warn only", "level": "exceeded", "enabled": true, "revoke_keys": false, "key_match": nil},
		{"id": 3, "label": "Not yet", "level": "warning", "enabled": true, "revoke_keys": true, "key_match": "*"}}, 0)
	f.set("cost_center", "on")
	report := f.reap(0)
	sort.Strings(report.CostCenter)
	equal(t, report.CostCenter, []string{"bootcamp-api", "bootcamp-manual", "bootcamp-web"})
	var left []string
	for _, name := range f.meta.names() {
		if !strings.HasPrefix(name, "other-") {
			left = append(left, name)
		}
	}
	equal(t, left, []string{"hand-made", SentinelName})
	if k, _ := f.store.ActiveKey("other"); k == nil {
		t.Fatal("other was revoked")
	}
	// New keys under a tripped alert are refused until the alert clears or is overridden.
	if _, err := f.ops.Create("bootcamp-api", 0, false, false); !isUserError(err) {
		t.Fatalf("create under a tripped alert: %v", err)
	}
	if _, err := f.ops.Create("bootcamp-api", 0, false, true); err != nil {
		t.Fatal(err)
	}
	f.create("unrelated", 0, false)
}

func TestStatusJSONHasTargetsRevocationsAndNoSecrets(t *testing.T) {
	f := setup(t)
	k := f.create("bootcamp-api", 0, false)
	f.create("other", 0, false)
	f.fakeCostCenter([]map[string]any{
		{"id": 7, "label": "Daily", "level": "exceeded", "enabled": true, "revoke_keys": true, "key_match": "bootcamp-*"}}, 0)
	f.set("cost_center", "on")
	f.reap(0) // refreshes the remote snapshot, then revokes bootcamp-api
	result, err := StatusJSON(f.store)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "sk-meta-") {
		t.Fatal("status --json contains a secret")
	}
	var status struct {
		CostCenter struct {
			Enabled   bool `json:"enabled"`
			LastCheck struct {
				OK bool `json:"ok"`
			} `json:"last_check"`
		} `json:"cost_center"`
		Keys        []struct{ Label string } `json:"keys"`
		Targets     []Target                 `json:"targets"`
		TargetsAsOf *string                  `json:"targets_as_of"`
		Revocations []struct {
			AlertID float64  `json:"alert_id"`
			Keys    []string `json:"keys"`
			At      string   `json:"at"`
		} `json:"revocations"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	if !status.CostCenter.Enabled || !status.CostCenter.LastCheck.OK || status.TargetsAsOf == nil {
		t.Fatalf("status: %s", data)
	}
	equal(t, len(status.Keys), 1)
	equal(t, status.Keys[0].Label, "other")
	equal(t, status.Targets, []Target{{"other", true}, {"hand-made", false}})
	equal(t, len(status.Revocations), 1)
	equal(t, status.Revocations[0].AlertID, 7.0)
	equal(t, status.Revocations[0].Keys, []string{"bootcamp-api"})
	equal(t, f.byMetaID(k.MetaID).State, StateRevoked)
}

func TestKeyMatchNamesAndStar(t *testing.T) {
	alert := Alert{"key_match": "key1,key2"}
	if !alert.Matches("key2") || alert.Matches("key3") {
		t.Fatal("comma-separated names")
	}
	if !(Alert{"key_match": "*"}).Matches("anything") || (Alert{"key_match": nil}).Matches("anything") {
		t.Fatal("star and empty")
	}
}

func TestAFailingCheckIsReportedAndBlocksCreation(t *testing.T) {
	f := setup(t)
	f.fakeCostCenter(nil, 1)
	f.set("cost_center", "on")
	report := f.reap(0)
	equal(t, report.Failures[0].Name, "cost-center")
	if f.store.LastCostCenterCheck().OK {
		t.Fatal("check recorded as ok")
	}
	if _, err := f.ops.Create("dev", 0, false, false); !isUserError(err) {
		t.Fatalf("create while the check fails: %v", err)
	}
}

func TestMissingCostCenterCommandIsReported(t *testing.T) {
	f := setup(t)
	f.set("cost_center_command", filepath.Join(f.dir, "nowhere", "muse-cost"))
	_, err := f.store.CostCenterStatuses(time.Minute)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
}

func TestShellQuote(t *testing.T) {
	equal(t, shellQuote("sk-meta-1"), "sk-meta-1")
	equal(t, shellQuote("a b'c"), `'a b'"'"'c'`)
}

func TestParseArgsAllowsOptionsAnywhere(t *testing.T) {
	p, _, err := parseArgs([]string{"create", "--ttl", "1h", "dev", "--auto-rotate"})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, p.positionals, []string{"dev"})
	equal(t, p.value("--ttl"), "1h")
	if !p.has("--auto-rotate") {
		t.Fatal("--auto-rotate lost")
	}
	p, _, err = parseArgs([]string{"exec", "dev", "--var=A", "--var", "B", "--", "env", "--var", "x"})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, p.flags["--var"], []string{"A", "B"})
	equal(t, p.rest, []string{"env", "--var", "x"})
	if _, _, err := parseArgs([]string{"create"}); err == nil {
		t.Fatal("create without a label")
	}
	if _, _, err := parseArgs([]string{"create", "dev", "--bogus"}); err == nil {
		t.Fatal("unknown option accepted")
	}
}
