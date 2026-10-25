"""Reading Meta Cost Center alerts that ask Keyguard to revoke keys."""
import json
import shlex
import shutil
import subprocess
import sys
from fnmatch import fnmatchcase
from pathlib import Path

from . import store


class CostCenterError(Exception):
    pass


def enabled(db):
    return store.setting(db, 'cost_center') == 'on'


def with_interpreter(argv):
    """Run a Python script with this Python rather than whatever `env python3` finds.

    Chrome starts the bridge through wsl.exe with a minimal PATH, where `python3` is often the
    distro's system Python, which may be too old for Cost Center (it needs 3.11 for tomllib).
    """
    found = shutil.which(argv[0])
    if not found:
        return argv
    try:
        with open(found, 'rb') as handle:
            first = handle.readline(200)
    except OSError:
        return argv
    if first.startswith(b'#!') and b'python' in first:
        return [sys.executable, str(Path(found).resolve())] + argv[1:]
    return argv


def statuses(db, timeout=120):
    """Run `muse-cost alert status --json` and return its alerts."""
    command = with_interpreter(shlex.split(store.setting(db, 'cost_center_command'))) + ['alert', 'status', '--json']
    try:
        done = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
    except FileNotFoundError:
        raise CostCenterError(f'Cost Center command not found: {command[0]}. '
                              'Set it with `keyguard config set cost_center_command /path/to/muse-cost`.') from None
    except subprocess.TimeoutExpired:
        raise CostCenterError('Cost Center did not answer within two minutes') from None
    if done.returncode != 0:
        message = (done.stderr or done.stdout).strip().splitlines()
        raise CostCenterError(f'Cost Center failed: {message[-1] if message else done.returncode}')
    try:
        alerts = json.loads(done.stdout)
    except json.JSONDecodeError:
        raise CostCenterError('Cost Center did not print JSON; update it to a version with `alert status`') from None
    if not isinstance(alerts, list):
        raise CostCenterError('Cost Center printed an unexpected alert status')
    return alerts


def tripped(alerts):
    """Alerts that have passed their limit and are set to revoke keys."""
    return [a for a in alerts if a.get('revoke_keys') and a.get('enabled', True) and a.get('level') == 'exceeded']


def patterns(alert):
    return [p.strip() for p in (alert.get('key_match') or '').split(',') if p.strip()]


def matches(alert, name):
    return any(fnmatchcase(name, p) for p in patterns(alert))


def check(db):
    """Return the tripped alerts, recording the result so `status` can show it."""
    try:
        alerts = tripped(statuses(db))
    except CostCenterError as exc:
        store.set_setting(db, 'cost_center_last', json.dumps({'at': store.iso(store.now()), 'ok': False,
                                                             'error': str(exc)}))
        raise
    store.set_setting(db, 'cost_center_last', json.dumps({
        'at': store.iso(store.now()), 'ok': True,
        'tripped': [{'id': a.get('id'), 'label': a.get('label'), 'key_match': a.get('key_match')} for a in alerts]}))
    return alerts
