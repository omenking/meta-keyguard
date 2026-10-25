"""Key operations. Every function takes the store and an extension (anything with `call`)."""
import json
import re
from datetime import timedelta

from . import costcenter, store
from .extension import ExtensionError

SENTINEL_NAME = 'keyguard-sentinel'
LABEL = re.compile(r'[a-z0-9][a-z0-9_-]{0,39}')
MEETS_META = ('list', 'create', 'delete', 'whoami')


class KeyguardError(Exception):
    pass


def call(db, ext, op, **params):
    """Call the extension, recording any failure so `status` and `doctor` report it."""
    try:
        result = ext.call(op, team_id=store.setting(db, 'team_id') or None,
                          project_id=store.setting(db, 'project_id') or None, **params)
    except ExtensionError as exc:
        store.record_failure(db, op, exc.code, str(exc))
        raise
    if op in MEETS_META:
        # A request that reached dev.meta.ai proves the earlier failure has cleared.
        store.clear_failure(db)
    return result


def check_label(label):
    if not LABEL.fullmatch(label or '') or label.startswith(SENTINEL_NAME):
        raise KeyguardError('Labels use lowercase letters, digits, - and _, start with a letter or digit, '
                            f'are at most 40 characters, and cannot start with {SENTINEL_NAME}')
    return label


def meta_name(label, moment):
    # The label stays readable on dev.meta.ai, and the suffix keeps rotated keys distinct.
    return f'{label}-kg{moment:%Y%m%d%H%M%S}'


def is_sentinel(remote_key):
    return (remote_key.get('name') or '').startswith(SENTINEL_NAME)


SNAPSHOT_MAX_AGE = timedelta(minutes=10)


def remote_keys(db, ext):
    keys = call(db, ext, 'list')['keys']
    # Names and IDs only, so Cost Center can preview which keys an alert's key match covers.
    store.set_setting(db, 'remote_snapshot', json.dumps({
        'at': store.iso(store.now()), 'keys': [{'id': k['id'], 'name': k.get('name') or ''} for k in keys]}))
    return keys


def targets(db):
    """Every key a Cost Center key match can select: managed labels and unmanaged dev.meta.ai names."""
    live = [r for r in store.live_keys(db) if r['state'] != 'sentinel']
    # Any key the store knows about, including revoked ones the snapshot may still list, is not unmanaged.
    managed_ids = {r['meta_id'] for r in store.all_keys(db)}
    snapshot = store.json_setting(db, 'remote_snapshot') or {'at': None, 'keys': []}
    names = list(dict.fromkeys(r['label'] for r in live))
    unmanaged = [k['name'] for k in snapshot['keys'] if k['id'] not in managed_ids and not is_sentinel(k)]
    return ([{'name': n, 'managed': True} for n in names] + [{'name': n, 'managed': False} for n in unmanaged],
            snapshot['at'])


def capped_ttl(db, ttl):
    maximum = store.duration_setting(db, 'max_ttl')
    if ttl > maximum:
        raise KeyguardError(f'TTL is longer than the maximum of {store.setting(db, "max_ttl")}; '
                            'raise it with `keyguard config set max_ttl` if you really need to')
    return ttl


def blocking_alert(db, label):
    """The tripped Cost Center alert that covers this label, if the check is on."""
    if not costcenter.enabled(db):
        return None
    try:
        alerts = costcenter.check(db)
    except costcenter.CostCenterError as exc:
        # Fail closed: when the spending check cannot run, do not hand out new keys.
        raise KeyguardError(f'{exc}. Keyguard will not create keys while the Cost Center check is failing; '
                            'pass --ignore-cost-center to override.') from None
    return next((a for a in alerts if costcenter.matches(a, label)), None)


def create(db, ext, label, ttl=None, auto_rotate=False, ignore_cost_center=False):
    check_label(label)
    if store.active_key(db, label):
        raise KeyguardError(f'{label} already has an active key; use `keyguard rotate {label}` to replace it')
    ttl = capped_ttl(db, ttl or store.duration_setting(db, 'default_ttl'))
    if not ignore_cost_center:
        alert = blocking_alert(db, label)
        if alert:
            raise KeyguardError(f'Cost Center alert "{alert.get("label")}" has passed its limit and revokes keys '
                                f'matching {alert.get("key_match")}, so Keyguard will not create {label}')
    return _create(db, ext, label, ttl, auto_rotate)


def _create(db, ext, label, ttl, auto_rotate):
    moment = store.now()
    created = call(db, ext, 'create', name=meta_name(label, moment))
    if not created.get('secret'):
        raise ExtensionError('secret_not_found')
    row_id = store.insert_key(db, label=label, meta_id=created['id'], meta_name=created['name'],
                              secret=created['secret'], state='active', created_at=store.iso(moment),
                              expires_at=store.iso(moment + timedelta(seconds=ttl)), ttl_seconds=ttl,
                              auto_rotate=1 if auto_rotate else 0)
    store.log(db, 'created', label, created['id'], ttl=ttl, auto_rotate=bool(auto_rotate))
    return dict(db.execute('SELECT * FROM keys WHERE id=?', (row_id,)).fetchone())


def ensure_sentinel(db, ext, remote):
    """Make sure a key nobody holds exists, so every other key can be deleted."""
    if any(is_sentinel(k) for k in remote):
        return remote
    created = call(db, ext, 'create', name=SENTINEL_NAME, discard_secret=True)
    store.insert_key(db, label=SENTINEL_NAME, meta_id=created['id'], meta_name=created['name'], secret=None,
                     state='sentinel', created_at=store.iso(store.now()))
    store.log(db, 'sentinel-created', SENTINEL_NAME, created['id'])
    return remote + [{'id': created['id'], 'name': created['name']}]


def delete_remote(db, ext, meta_id, remote):
    """Delete one key on dev.meta.ai, creating a sentinel first if it is the last one."""
    if not any(k['id'] == meta_id for k in remote):
        return remote
    if len(remote) == 1:
        remote = ensure_sentinel(db, ext, remote)
    call(db, ext, 'delete', id=meta_id)
    return [k for k in remote if k['id'] != meta_id]


def revoke_rows(db, ext, rows, reason, remote=None):
    """Delete stored keys, continuing past failures and reporting them together at the end."""
    remote = remote if remote is not None else remote_keys(db, ext)
    failures = []
    for row in rows:
        try:
            remote = delete_remote(db, ext, row['meta_id'], remote)
        except ExtensionError as exc:
            store.update_key(db, row['id'], last_error=f'{exc.code}: {exc}')
            failures.append((row['label'], exc))
            continue
        store.mark_revoked(db, row, reason)
    return remote, failures


def raise_failures(failures, action):
    if failures:
        lines = '; '.join(f'{name}: {exc.code}' for name, exc in failures)
        first = failures[0][1]
        raise KeyguardError(f'Could not {action} {len(failures)} key(s) ({lines}). {first.hint or first}')


def find(db, label):
    rows = [r for r in store.live_keys(db) if r['label'] == label and r['state'] != 'sentinel']
    if not rows:
        raise KeyguardError(f'No live key labelled {label}')
    return rows


def revoke(db, ext, label, reason='revoked by user'):
    rows = find(db, label)
    _, failures = revoke_rows(db, ext, rows, reason)
    raise_failures(failures, 'revoke')
    return rows


def revoke_remote(db, ext, name_or_id, reason='revoked by user'):
    """Delete a key that Keyguard did not create, by its dev.meta.ai name or ID."""
    remote = remote_keys(db, ext)
    target = next((k for k in remote if name_or_id in (k['id'], k.get('name')) and not is_sentinel(k)), None)
    if not target:
        raise KeyguardError(f'No key named or with ID {name_or_id} on dev.meta.ai')
    delete_remote(db, ext, target['id'], remote)
    store.log(db, 'revoked-unmanaged', target.get('name'), target['id'], reason=reason)
    return target


def rotate(db, ext, label, grace=None, reason='rotated'):
    old = store.active_key(db, label)
    if not old:
        raise KeyguardError(f'No active key labelled {label}')
    grace = store.duration_setting(db, 'rotate_grace') if grace is None else grace
    new = _create(db, ext, label, old['ttl_seconds'] or store.duration_setting(db, 'default_ttl'),
                  old['auto_rotate'])
    store.update_key(db, old['id'], state='retiring', delete_after=store.iso(store.now() + timedelta(seconds=grace)))
    store.log(db, 'rotated', label, old['meta_id'], replaced_by=new['meta_id'], grace=grace, reason=reason)
    if grace == 0:
        _, failures = revoke_rows(db, ext, [dict(old, state='retiring')], reason)
        raise_failures(failures, 'delete the old')
    return new


def extend(db, label, ttl):
    row = store.active_key(db, label)
    if not row:
        raise KeyguardError(f'No active key labelled {label}')
    ttl = capped_ttl(db, ttl)
    expires = store.now() + timedelta(seconds=ttl)
    store.update_key(db, row['id'], expires_at=store.iso(expires))
    store.log(db, 'extended', label, row['meta_id'], ttl=ttl)
    return dict(row, expires_at=store.iso(expires))


def revoke_matching(db, ext, match, reason, remote=None):
    """Delete every key, managed or not, whose label or dev.meta.ai name `match` accepts."""
    remote = remote if remote is not None else remote_keys(db, ext)
    managed = {r['meta_id']: r for r in store.live_keys(db)}
    rows = [r for r in managed.values() if r['state'] != 'sentinel' and match(r['label'])]
    unmanaged = [k for k in remote if k['id'] not in managed and not is_sentinel(k) and match(k.get('name') or '')]
    if rows or unmanaged:
        # Deleting everything leaves the account at its last key, so make the sentinel first.
        remote = ensure_sentinel(db, ext, remote)
    remote, failures = revoke_rows(db, ext, rows, reason, remote)
    for key in unmanaged:
        try:
            remote = delete_remote(db, ext, key['id'], remote)
            store.log(db, 'revoked-unmanaged', key.get('name'), key['id'], reason=reason)
        except ExtensionError as exc:
            failures.append((key.get('name') or key['id'], exc))
    return rows, unmanaged, failures


def kill(db, ext, reason='kill switch'):
    """Delete every key on the team except a sentinel whose secret nobody has."""
    store.log(db, 'kill', reason=reason)
    rows, unmanaged, failures = revoke_matching(db, ext, lambda name: True, reason)
    raise_failures(failures, 'delete')
    return rows, unmanaged


def sync(db, ext):
    """Reconcile the store with dev.meta.ai. Returns the keys Keyguard does not manage."""
    remote = remote_keys(db, ext)
    present = {k['id'] for k in remote}
    for row in store.live_keys(db):
        if row['meta_id'] not in present:
            store.mark_revoked(db, row, 'missing on dev.meta.ai')
    managed = {r['meta_id'] for r in store.live_keys(db)}
    return [k for k in remote if k['id'] not in managed]


def schedule(db):
    """What the extension should delete, and when, if the CLI is not around to do it."""
    return [{'id': r['meta_id'], 'name': r['meta_name'], 'label': r['label'], 'delete_at': store.iso(store.due_at(r))}
            for r in store.live_keys(db) if store.due_at(r)]


def push_schedule(db, ext):
    call(db, ext, 'schedule', keys=schedule(db))


def rotation_lead(db, row):
    """How long before expiry to replace an auto-rotating key. Capped at a quarter of its TTL, so a
    key whose whole lifetime is shorter than rotate_before is not replaced again on every reap."""
    lead = store.duration_setting(db, 'rotate_before')
    return timedelta(seconds=min(lead, (row['ttl_seconds'] or lead * 4) // 4))


def reap(db, ext, moment=None):
    """Enforce TTLs, finish rotations, and apply Cost Center limits. Run regularly by the bridge."""
    moment = moment or store.now()
    report = {'rotated': [], 'expired': [], 'retired': [], 'cost_center': [], 'failures': []}
    for row in store.live_keys(db):
        due = store.due_at(row)
        if row['state'] == 'active' and row['auto_rotate'] and due and moment < due <= moment + rotation_lead(db, row):
            try:
                rotate(db, ext, row['label'], reason='auto-rotate before expiry')
                report['rotated'].append(row['label'])
            except (ExtensionError, KeyguardError) as exc:
                report['failures'].append((row['label'], exc))
    due_rows = [r for r in store.live_keys(db) if store.is_overdue(r, moment)]
    if due_rows:
        _, failures = revoke_rows(db, ext, due_rows, 'expired')
        report['failures'] += failures
        failed = {name for name, _ in failures}
        for r in due_rows:
            if r['label'] not in failed:
                report['retired' if r['state'] == 'retiring' else 'expired'].append(r['label'])
    if costcenter.enabled(db):
        snapshot = store.json_setting(db, 'remote_snapshot')
        if not snapshot or moment - store.parse_time(snapshot['at']) > SNAPSHOT_MAX_AGE:
            try:
                remote_keys(db, ext)
            except ExtensionError as exc:
                report['failures'].append(('list', exc))
        try:
            alerts = costcenter.check(db)
        except costcenter.CostCenterError as exc:
            # costcenter.check keeps this failure in cost_center_last, which `status` reports.
            store.log(db, 'failure', op='cost-center', message=str(exc))
            report['failures'].append(('cost-center', exc))
            alerts = []
        for alert in alerts:
            reason = f'cost center alert {alert.get("id")}: {alert.get("label")}'
            rows, unmanaged, failures = revoke_matching(db, ext, lambda name, a=alert: costcenter.matches(a, name),
                                                        reason)
            if rows or unmanaged:
                store.log(db, 'cost-center-revoked', alert_id=alert.get('id'), key_match=alert.get('key_match'),
                          keys=[r['label'] for r in rows] + [k.get('name') for k in unmanaged])
                report['cost_center'] += [r['label'] for r in rows] + [k.get('name') for k in unmanaged]
            report['failures'] += failures
    push_schedule(db, ext)
    return report
