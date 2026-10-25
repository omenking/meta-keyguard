package keyguard

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const SentinelName = "keyguard-sentinel"

var labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// Operations that reach dev.meta.ai; a success proves an earlier failure has cleared.
var meetsMeta = map[string]bool{"list": true, "create": true, "delete": true, "whoami": true}

const snapshotMaxAge = 10 * time.Minute

// UserError is a problem with what the user asked for, reported without a stack of codes.
type UserError struct{ Message string }

func (e *UserError) Error() string { return e.Message }

func userErrorf(format string, args ...any) error { return &UserError{fmt.Sprintf(format, args...)} }

// RemoteKey is a key as dev.meta.ai lists it.
type RemoteKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt any    `json:"created_at,omitempty"`
}

func (k RemoteKey) IsSentinel() bool { return strings.HasPrefix(k.Name, SentinelName) }

// Ops performs key operations against the store and an extension.
type Ops struct {
	Store *Store
	Ext   Extension
}

// Call asks the extension to do something, recording any failure so `status` and `doctor` report it.
func (o *Ops) Call(op string, params Params) (map[string]any, error) {
	full := Params{"team_id": optional(o.Store.Setting("team_id")), "project_id": optional(o.Store.Setting("project_id"))}
	for k, v := range params {
		full[k] = v
	}
	result, err := o.Ext.Call(op, full, defaultTimeout)
	if err != nil {
		var extErr *ExtensionError
		if errors.As(err, &extErr) {
			o.Store.RecordFailure(op, extErr.Code, extErr.Error())
		}
		return nil, err
	}
	if meetsMeta[op] {
		o.Store.ClearFailure()
	}
	return result, nil
}

func optional(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func CheckLabel(label string) error {
	if !labelPattern.MatchString(label) || strings.HasPrefix(label, SentinelName) {
		return userErrorf("Labels use lowercase letters, digits, - and _, start with a letter or digit, " +
			"are at most 40 characters, and cannot start with " + SentinelName)
	}
	return nil
}

// MetaName keeps the label readable on dev.meta.ai; the suffix keeps rotated keys distinct.
func MetaName(label string, moment time.Time) string {
	return label + "-kg" + moment.UTC().Format("20060102150405")
}

func stringField(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	}
	return ""
}

// RemoteKeys lists the team's keys, keeping a names-only snapshot for Cost Center's key-match preview.
func (o *Ops) RemoteKeys() ([]RemoteKey, error) {
	result, err := o.Call("list", nil)
	if err != nil {
		return nil, err
	}
	items, _ := result["keys"].([]any)
	keys := make([]RemoteKey, 0, len(items))
	for _, item := range items {
		m, _ := item.(map[string]any)
		keys = append(keys, RemoteKey{ID: stringField(m, "id"), Name: stringField(m, "name"), CreatedAt: m["created_at"]})
	}
	snapshot := map[string]any{"at": ISO(now()), "keys": keys}
	o.Store.SetJSON("remote_snapshot", snapshot)
	return keys, nil
}

type remoteSnapshot struct {
	At   string      `json:"at"`
	Keys []RemoteKey `json:"keys"`
}

// Target is a key a Cost Center key match can select: a managed label or an unmanaged dev.meta.ai name.
type Target struct {
	Name    string `json:"name"`
	Managed bool   `json:"managed"`
}

func (o *Ops) Targets() ([]Target, string, error) {
	live, err := o.Store.LiveKeys()
	if err != nil {
		return nil, "", err
	}
	all, err := o.Store.AllKeys()
	if err != nil {
		return nil, "", err
	}
	// Any key the store knows about, including revoked ones the snapshot may still list, is not unmanaged.
	known := map[string]bool{}
	for _, k := range all {
		known[k.MetaID] = true
	}
	targets := []Target{}
	seen := map[string]bool{}
	for _, k := range live {
		if k.State != StateSentinel && !seen[k.Label] {
			seen[k.Label] = true
			targets = append(targets, Target{k.Label, true})
		}
	}
	var snapshot remoteSnapshot
	o.Store.JSONSetting("remote_snapshot", &snapshot)
	for _, k := range snapshot.Keys {
		if !known[k.ID] && !k.IsSentinel() {
			targets = append(targets, Target{k.Name, false})
		}
	}
	return targets, snapshot.At, nil
}

func (o *Ops) cappedTTL(ttl int64) (int64, error) {
	maximum, err := o.Store.DurationSetting("max_ttl")
	if err != nil {
		return 0, err
	}
	if ttl > maximum {
		return 0, userErrorf("TTL is longer than the maximum of %s; raise it with `meta-keyguard config set max_ttl` "+
			"if you really need to", o.Store.Setting("max_ttl"))
	}
	return ttl, nil
}

// BlockingAlert is the tripped Cost Center alert that covers this label, if the check is on.
func (o *Ops) BlockingAlert(label string) (Alert, error) {
	if !o.Store.CostCenterEnabled() {
		return nil, nil
	}
	alerts, err := o.Store.CheckCostCenter()
	if err != nil {
		// Fail closed: when the spending check cannot run, do not hand out new keys.
		return nil, userErrorf("%s. Keyguard will not create keys while the Cost Center check is failing; "+
			"pass --ignore-cost-center to override.", err)
	}
	for _, a := range alerts {
		if a.Matches(label) {
			return a, nil
		}
	}
	return nil, nil
}

func (o *Ops) Create(label string, ttl int64, autoRotate, ignoreCostCenter bool) (*Key, error) {
	if err := CheckLabel(label); err != nil {
		return nil, err
	}
	if existing, err := o.Store.ActiveKey(label); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, userErrorf("%s already has an active key; use `meta-keyguard rotate %s` to replace it", label, label)
	}
	if ttl == 0 {
		var err error
		if ttl, err = o.Store.DurationSetting("default_ttl"); err != nil {
			return nil, err
		}
	}
	ttl, err := o.cappedTTL(ttl)
	if err != nil {
		return nil, err
	}
	if !ignoreCostCenter {
		alert, err := o.BlockingAlert(label)
		if err != nil {
			return nil, err
		}
		if alert != nil {
			return nil, userErrorf("Cost Center alert %q has passed its limit and revokes keys matching %s, so "+
				"Keyguard will not create %s", alert.String("label"), alert.String("key_match"), label)
		}
	}
	return o.create(label, ttl, autoRotate)
}

func (o *Ops) create(label string, ttl int64, autoRotate bool) (*Key, error) {
	moment := now()
	created, err := o.Call("create", Params{"name": MetaName(label, moment)})
	if err != nil {
		return nil, err
	}
	secret := stringField(created, "secret")
	if secret == "" {
		return nil, NewExtensionError("secret_not_found")
	}
	id, err := o.Store.InsertKey(Key{Label: label, MetaID: stringField(created, "id"), MetaName: stringField(created, "name"),
		Secret: secret, State: StateActive, CreatedAt: ISO(moment), ExpiresAt: ISO(moment.Add(time.Duration(ttl) * time.Second)),
		TTLSeconds: ttl, AutoRotate: autoRotate})
	if err != nil {
		return nil, err
	}
	o.Store.Log("created", label, stringField(created, "id"), map[string]any{"ttl": ttl, "auto_rotate": autoRotate})
	return o.Store.ByID(id)
}

// EnsureSentinel makes sure a key nobody holds exists, so every other key can be deleted.
func (o *Ops) EnsureSentinel(remote []RemoteKey) ([]RemoteKey, error) {
	for _, k := range remote {
		if k.IsSentinel() {
			return remote, nil
		}
	}
	created, err := o.Call("create", Params{"name": SentinelName, "discard_secret": true})
	if err != nil {
		return nil, err
	}
	sentinel := RemoteKey{ID: stringField(created, "id"), Name: stringField(created, "name")}
	if _, err := o.Store.InsertKey(Key{Label: SentinelName, MetaID: sentinel.ID, MetaName: sentinel.Name,
		State: StateSentinel, CreatedAt: ISO(now())}); err != nil {
		return nil, err
	}
	o.Store.Log("sentinel-created", SentinelName, sentinel.ID, nil)
	return append(remote, sentinel), nil
}

// DeleteRemote deletes one key on dev.meta.ai, creating a sentinel first if it is the last one.
func (o *Ops) DeleteRemote(metaID string, remote []RemoteKey) ([]RemoteKey, error) {
	present := false
	for _, k := range remote {
		present = present || k.ID == metaID
	}
	if !present {
		return remote, nil
	}
	if len(remote) == 1 {
		var err error
		if remote, err = o.EnsureSentinel(remote); err != nil {
			return nil, err
		}
	}
	if _, err := o.Call("delete", Params{"id": metaID}); err != nil {
		return nil, err
	}
	kept := remote[:0:0]
	for _, k := range remote {
		if k.ID != metaID {
			kept = append(kept, k)
		}
	}
	return kept, nil
}

// Failure is one key that could not be deleted.
type KeyFailure struct {
	Name string
	Err  error
}

// RevokeKeys deletes stored keys, continuing past failures and returning them together.
func (o *Ops) RevokeKeys(keys []Key, reason string, remote []RemoteKey) ([]RemoteKey, []KeyFailure, error) {
	if remote == nil {
		var err error
		if remote, err = o.RemoteKeys(); err != nil {
			return nil, nil, err
		}
	}
	var failures []KeyFailure
	for _, k := range keys {
		next, err := o.DeleteRemote(k.MetaID, remote)
		if err != nil {
			var extErr *ExtensionError
			if !errors.As(err, &extErr) {
				return nil, nil, err
			}
			o.Store.UpdateKey(k.ID, map[string]string{"last_error": extErr.Code + ": " + extErr.Error()})
			failures = append(failures, KeyFailure{k.Label, err})
			continue
		}
		remote = next
		if err := o.Store.MarkRevoked(k, reason); err != nil {
			return nil, nil, err
		}
	}
	return remote, failures, nil
}

func failuresError(failures []KeyFailure, action string) error {
	if len(failures) == 0 {
		return nil
	}
	names := make([]string, len(failures))
	for i, f := range failures {
		code := "error"
		var extErr *ExtensionError
		if errors.As(f.Err, &extErr) {
			code = extErr.Code
		}
		names[i] = f.Name + ": " + code
	}
	detail := f0Detail(failures[0].Err)
	return userErrorf("Could not %s %d key(s) (%s). %s", action, len(failures), strings.Join(names, "; "), detail)
}

func f0Detail(err error) string {
	var extErr *ExtensionError
	if errors.As(err, &extErr) && extErr.Hint() != "" {
		return extErr.Hint()
	}
	return err.Error()
}

func (o *Ops) find(label string) ([]Key, error) {
	live, err := o.Store.LiveKeys()
	if err != nil {
		return nil, err
	}
	var found []Key
	for _, k := range live {
		if k.Label == label && k.State != StateSentinel {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		return nil, userErrorf("No live key labelled %s", label)
	}
	return found, nil
}

func (o *Ops) Revoke(label, reason string) ([]Key, error) {
	keys, err := o.find(label)
	if err != nil {
		return nil, err
	}
	_, failures, err := o.RevokeKeys(keys, reason, nil)
	if err != nil {
		return nil, err
	}
	return keys, failuresError(failures, "revoke")
}

// RevokeRemote deletes a key that Keyguard did not create, by its dev.meta.ai name or ID.
func (o *Ops) RevokeRemote(nameOrID, reason string) (*RemoteKey, error) {
	remote, err := o.RemoteKeys()
	if err != nil {
		return nil, err
	}
	for _, k := range remote {
		if (k.ID == nameOrID || k.Name == nameOrID) && !k.IsSentinel() {
			if _, err := o.DeleteRemote(k.ID, remote); err != nil {
				return nil, err
			}
			o.Store.Log("revoked-unmanaged", k.Name, k.ID, map[string]any{"reason": reason})
			return &k, nil
		}
	}
	return nil, userErrorf("No key named or with ID %s on dev.meta.ai", nameOrID)
}

func (o *Ops) Rotate(label string, grace int64, reason string) (*Key, error) {
	old, err := o.Store.ActiveKey(label)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, userErrorf("No active key labelled %s", label)
	}
	if grace < 0 {
		if grace, err = o.Store.DurationSetting("rotate_grace"); err != nil {
			return nil, err
		}
	}
	ttl := old.TTLSeconds
	if ttl == 0 {
		if ttl, err = o.Store.DurationSetting("default_ttl"); err != nil {
			return nil, err
		}
	}
	fresh, err := o.create(label, ttl, old.AutoRotate)
	if err != nil {
		return nil, err
	}
	deleteAfter := ISO(now().Add(time.Duration(grace) * time.Second))
	if err := o.Store.UpdateKey(old.ID, map[string]string{"state": StateRetiring, "delete_after": deleteAfter}); err != nil {
		return nil, err
	}
	o.Store.Log("rotated", label, old.MetaID, map[string]any{"replaced_by": fresh.MetaID, "grace": grace, "reason": reason})
	if grace == 0 {
		retiring := *old
		retiring.State = StateRetiring
		_, failures, err := o.RevokeKeys([]Key{retiring}, reason, nil)
		if err != nil {
			return nil, err
		}
		if err := failuresError(failures, "delete the old"); err != nil {
			return nil, err
		}
	}
	return fresh, nil
}

func (o *Ops) Extend(label string, ttl int64) (*Key, error) {
	k, err := o.Store.ActiveKey(label)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, userErrorf("No active key labelled %s", label)
	}
	if ttl, err = o.cappedTTL(ttl); err != nil {
		return nil, err
	}
	k.ExpiresAt = ISO(now().Add(time.Duration(ttl) * time.Second))
	if err := o.Store.UpdateKey(k.ID, map[string]string{"expires_at": k.ExpiresAt}); err != nil {
		return nil, err
	}
	o.Store.Log("extended", label, k.MetaID, map[string]any{"ttl": ttl})
	return k, nil
}

// RevokeMatching deletes every key, managed or not, whose label or dev.meta.ai name match accepts.
func (o *Ops) RevokeMatching(match func(string) bool, reason string) ([]Key, []RemoteKey, []KeyFailure, error) {
	remote, err := o.RemoteKeys()
	if err != nil {
		return nil, nil, nil, err
	}
	live, err := o.Store.LiveKeys()
	if err != nil {
		return nil, nil, nil, err
	}
	managed := map[string]bool{}
	var keys []Key
	for _, k := range live {
		managed[k.MetaID] = true
		if k.State != StateSentinel && match(k.Label) {
			keys = append(keys, k)
		}
	}
	var unmanaged []RemoteKey
	for _, k := range remote {
		if !managed[k.ID] && !k.IsSentinel() && match(k.Name) {
			unmanaged = append(unmanaged, k)
		}
	}
	if len(keys) > 0 || len(unmanaged) > 0 {
		// Deleting everything leaves the account at its last key, so make the sentinel first.
		if remote, err = o.EnsureSentinel(remote); err != nil {
			return nil, nil, nil, err
		}
	}
	remote, failures, err := o.RevokeKeys(keys, reason, remote)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, k := range unmanaged {
		next, err := o.DeleteRemote(k.ID, remote)
		if err != nil {
			failures = append(failures, KeyFailure{k.Name, err})
			continue
		}
		remote = next
		o.Store.Log("revoked-unmanaged", k.Name, k.ID, map[string]any{"reason": reason})
	}
	return keys, unmanaged, failures, nil
}

// Kill deletes every key on the team except a sentinel whose secret nobody has.
func (o *Ops) Kill(reason string) ([]Key, []RemoteKey, error) {
	o.Store.Log("kill", "", "", map[string]any{"reason": reason})
	keys, unmanaged, failures, err := o.RevokeMatching(func(string) bool { return true }, reason)
	if err != nil {
		return nil, nil, err
	}
	return keys, unmanaged, failuresError(failures, "delete")
}

// Sync reconciles the store with dev.meta.ai and returns the keys Keyguard does not manage.
func (o *Ops) Sync() ([]RemoteKey, error) {
	remote, err := o.RemoteKeys()
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, k := range remote {
		present[k.ID] = true
	}
	live, err := o.Store.LiveKeys()
	if err != nil {
		return nil, err
	}
	managed := map[string]bool{}
	for _, k := range live {
		if !present[k.MetaID] {
			if err := o.Store.MarkRevoked(k, "missing on dev.meta.ai"); err != nil {
				return nil, err
			}
			continue
		}
		managed[k.MetaID] = true
	}
	var unmanaged []RemoteKey
	for _, k := range remote {
		if !managed[k.ID] {
			unmanaged = append(unmanaged, k)
		}
	}
	return unmanaged, nil
}

// Schedule is what the extension should delete, and when, if the CLI is not around to do it.
func (o *Ops) Schedule() ([]map[string]any, error) {
	live, err := o.Store.LiveKeys()
	if err != nil {
		return nil, err
	}
	schedule := []map[string]any{}
	for _, k := range live {
		if due, ok := k.DueAt(); ok {
			schedule = append(schedule, map[string]any{"id": k.MetaID, "name": k.MetaName, "label": k.Label, "delete_at": ISO(due)})
		}
	}
	return schedule, nil
}

func (o *Ops) PushSchedule() error {
	schedule, err := o.Schedule()
	if err != nil {
		return err
	}
	_, err = o.Call("schedule", Params{"keys": schedule})
	return err
}

// RotationLead is how long before expiry to replace an auto-rotating key. It is capped at a quarter
// of the key's TTL, so a key whose whole lifetime is shorter than rotate_before is not replaced on every reap.
func (o *Ops) RotationLead(k Key) time.Duration {
	lead, err := o.Store.DurationSetting("rotate_before")
	if err != nil {
		lead = 600
	}
	ttl := k.TTLSeconds
	if ttl == 0 {
		ttl = lead * 4
	}
	return time.Duration(min(lead, ttl/4)) * time.Second
}

// ReapReport says what one reaper pass did.
type ReapReport struct {
	Rotated, Expired, Retired, CostCenter []string
	Failures                              []KeyFailure
}

func (r ReapReport) Empty() bool {
	return len(r.Rotated)+len(r.Expired)+len(r.Retired)+len(r.CostCenter)+len(r.Failures) == 0
}

// Reap enforces TTLs, finishes rotations, and applies Cost Center limits. The bridge runs it every minute.
func (o *Ops) Reap(moment time.Time) (ReapReport, error) {
	var report ReapReport
	live, err := o.Store.LiveKeys()
	if err != nil {
		return report, err
	}
	for _, k := range live {
		due, ok := k.DueAt()
		if k.State == StateActive && k.AutoRotate && ok && moment.Before(due) && !due.After(moment.Add(o.RotationLead(k))) {
			if _, err := o.Rotate(k.Label, -1, "auto-rotate before expiry"); err != nil {
				report.Failures = append(report.Failures, KeyFailure{k.Label, err})
			} else {
				report.Rotated = append(report.Rotated, k.Label)
			}
		}
	}
	if live, err = o.Store.LiveKeys(); err != nil {
		return report, err
	}
	var due []Key
	for _, k := range live {
		if k.OverdueAt(moment) {
			due = append(due, k)
		}
	}
	if len(due) > 0 {
		_, failures, err := o.RevokeKeys(due, "expired", nil)
		if err != nil {
			report.Failures = append(report.Failures, KeyFailure{"list", err})
		}
		report.Failures = append(report.Failures, failures...)
		failed := map[string]bool{}
		for _, f := range failures {
			failed[f.Name] = true
		}
		for _, k := range due {
			if err != nil || failed[k.Label] {
				continue
			}
			if k.State == StateRetiring {
				report.Retired = append(report.Retired, k.Label)
			} else {
				report.Expired = append(report.Expired, k.Label)
			}
		}
	}
	if o.Store.CostCenterEnabled() {
		o.reapCostCenter(moment, &report)
	}
	if err := o.PushSchedule(); err != nil {
		report.Failures = append(report.Failures, KeyFailure{"schedule", err})
	}
	return report, nil
}

func (o *Ops) reapCostCenter(moment time.Time, report *ReapReport) {
	var snapshot remoteSnapshot
	fresh := false
	if o.Store.JSONSetting("remote_snapshot", &snapshot) {
		at, ok := ParseTime(snapshot.At)
		fresh = ok && moment.Sub(at) <= snapshotMaxAge
	}
	if !fresh {
		if _, err := o.RemoteKeys(); err != nil {
			report.Failures = append(report.Failures, KeyFailure{"list", err})
		}
	}
	alerts, err := o.Store.CheckCostCenter()
	if err != nil {
		// CheckCostCenter keeps this failure in cost_center_last, which `status` reports.
		o.Store.Log("failure", "", "", map[string]any{"op": "cost-center", "message": err.Error()})
		report.Failures = append(report.Failures, KeyFailure{"cost-center", err})
		return
	}
	for _, alert := range alerts {
		reason := fmt.Sprintf("cost center alert %v: %s", alert.ID(), alert.String("label"))
		keys, unmanaged, failures, err := o.RevokeMatching(alert.Matches, reason)
		if err != nil {
			report.Failures = append(report.Failures, KeyFailure{"cost-center", err})
			continue
		}
		var names []string
		for _, k := range keys {
			names = append(names, k.Label)
		}
		for _, k := range unmanaged {
			names = append(names, k.Name)
		}
		if len(names) > 0 {
			o.Store.Log("cost-center-revoked", "", "", map[string]any{"alert_id": alert.ID(), "key_match": alert.String("key_match"), "keys": names})
			report.CostCenter = append(report.CostCenter, names...)
		}
		report.Failures = append(report.Failures, failures...)
	}
}
