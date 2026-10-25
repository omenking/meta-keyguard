import argparse
import json
import os
import shlex
import sqlite3
import sys
from datetime import datetime
from pathlib import Path

from . import __version__, costcenter, durations, ops, store
from .extension import BridgeClient, ExtensionError


def duration(text):
    try:
        return durations.parse(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(str(exc)) from None


def parser():
    p = argparse.ArgumentParser(prog='keyguard', description='Short-lived, centrally stored Meta Model API keys.')
    p.add_argument('--version', action='version', version=f'keyguard {__version__}')
    sub = p.add_subparsers(dest='command', required=True)
    sub.add_parser('install-bridge', help='Register the Chrome native messaging host on Windows')
    sub.add_parser('uninstall-bridge', help='Remove the native messaging host from Windows')
    d = sub.add_parser('doctor', help='Check the bridge, the extension, the dev.meta.ai login, and Cost Center')
    d.add_argument('--inspect', action='store_true', help='Show the redacted shape of dev.meta.ai responses')
    st = sub.add_parser('status', help='Show keys, overdue deletions, failures, and the Cost Center state')
    st.add_argument('--json', action='store_true', help='Machine-readable status, without secrets (used by Cost Center)')
    c = sub.add_parser('create', help='Create a key on dev.meta.ai and store it')
    c.add_argument('label')
    c.add_argument('--ttl', type=duration, help='How long the key lives (default: the default_ttl setting, 12h)')
    c.add_argument('--auto-rotate', action='store_true', help='Replace the key shortly before it expires')
    c.add_argument('--ignore-cost-center', action='store_true', help='Create even if a Cost Center alert blocks it')
    lst = sub.add_parser('list', help='List stored keys')
    lst.add_argument('--all', action='store_true', help='Also list keys on dev.meta.ai that Keyguard did not create')
    lst.add_argument('--history', action='store_true', help='Include revoked keys')
    for name, text in (('get', 'Print the secret'), ('env', 'Print an export line for eval')):
        g = sub.add_parser(name, help=text)
        g.add_argument('label')
        if name == 'env':
            g.add_argument('--var', help='Variable name (default: the env_var setting, META_API_KEY)')
    e = sub.add_parser('exec', help='Run a command with the key in its environment')
    e.add_argument('label')
    e.add_argument('--var', action='append', help='Variable name; repeat for several (default: META_API_KEY)')
    e.add_argument('argv', nargs=argparse.REMAINDER, metavar='-- command')
    r = sub.add_parser('rotate', help='Replace a key, deleting the old one after a grace period')
    r.add_argument('label')
    r.add_argument('--grace', type=durations.parse, help='How long the old key keeps working (default: 5m)')
    x = sub.add_parser('extend', help='Push back a key\'s expiry')
    x.add_argument('label')
    x.add_argument('--ttl', type=duration, required=True, help='New lifetime, measured from now')
    v = sub.add_parser('revoke', help='Delete a key now')
    v.add_argument('label', help='A Keyguard label, or the dev.meta.ai name or ID of a key Keyguard did not create')
    k = sub.add_parser('kill', help='Delete every key on dev.meta.ai, leaving only a sentinel nobody holds')
    k.add_argument('--yes', action='store_true', help='Do not ask for confirmation')
    k.add_argument('--reason', default='kill switch')
    sub.add_parser('sync', help='Reconcile the store with dev.meta.ai')
    sub.add_parser('reap', help='Enforce TTLs and Cost Center limits now (the bridge does this every minute)')
    sub.add_parser('bridge', help='Native messaging host; started by Chrome, not by hand')
    g = sub.add_parser('config', help='Show or change settings')
    g.add_argument('action', choices=['show', 'set', 'unset'], nargs='?', default='show')
    g.add_argument('key', nargs='?', choices=sorted(store.DEFAULTS))
    g.add_argument('value', nargs='?')
    cc = sub.add_parser('cost-center', help='Turn the Cost Center check on or off, or show what it reports')
    cc.add_argument('action', choices=['on', 'off', 'check'])
    lg = sub.add_parser('log', help='Show the audit log')
    lg.add_argument('-n', type=int, default=30)
    return p


def remaining(row, moment):
    due = store.due_at(row)
    if due is None:
        return '-'
    left = (due - moment).total_seconds()
    return f'OVERDUE {durations.format(-left)}' if left <= 0 else durations.format(left)


def created(value):
    """dev.meta.ai reports creation times as Unix seconds."""
    if isinstance(value, (int, float)):
        return datetime.fromtimestamp(value).strftime('%Y-%m-%d %H:%M')
    return value or ''


def table(rows, headers):
    widths = [max(len(str(h)), *(len(str(r[i])) for r in rows)) if rows else len(h) for i, h in enumerate(headers)]
    lines = ['  '.join(str(h).ljust(w) for h, w in zip(headers, widths))]
    lines += ['  '.join(str(v).ljust(w) for v, w in zip(r, widths)) for r in rows]
    return '\n'.join(lines)


def key_rows(rows, moment):
    return [(r['label'], r['state'], remaining(r, moment), 'yes' if r['auto_rotate'] else '', r['meta_name'] or '',
             r['last_error'] or '') for r in rows]


def usable(db, label):
    row = store.active_key(db, label)
    if not row:
        raise ops.KeyguardError(f'No active key labelled {label}; create one with `keyguard create {label}`')
    if store.is_overdue(row):
        raise ops.KeyguardError(f'The key labelled {label} has expired and is waiting to be deleted; '
                                f'create a new one with `keyguard rotate {label}` or `keyguard create`')
    return row


def show_failure(db):
    failure = store.json_setting(db, 'last_failure')
    if failure:
        print(f'!! Last failure ({failure["at"]}, {failure["op"]}): {failure["code"]}: {failure["message"]}')
    return failure


def print_report(report):
    for kind in ('expired', 'retired', 'rotated', 'cost_center'):
        if report[kind]:
            print(f'{kind.replace("_", " ")}: {", ".join(report[kind])}')
    for name, exc in report['failures']:
        print(f'!! {name}: {exc}', file=sys.stderr)
    if not any(report.values()):
        print('Nothing to do.')


def status(db):
    moment = store.now()
    show_failure(db)
    rows = [r for r in store.live_keys(db)]
    overdue = [r for r in rows if store.is_overdue(r, moment)]
    if overdue:
        print(f'!! {len(overdue)} key(s) are past their expiry but still exist on dev.meta.ai: '
              f'{", ".join(r["label"] for r in overdue)}. Run `keyguard reap` or `keyguard doctor`.')
    print(table(key_rows(rows, moment), ['LABEL', 'STATE', 'EXPIRES IN', 'AUTO', 'META NAME', 'LAST ERROR'])
          if rows else 'No live keys.')
    if costcenter.enabled(db):
        last = store.json_setting(db, 'cost_center_last')
        if not last:
            print('Cost Center: on, not checked yet')
        elif not last['ok']:
            print(f'!! Cost Center check failing ({last["at"]}): {last["error"]}')
        elif last['tripped']:
            for a in last['tripped']:
                print(f'!! Cost Center alert "{a["label"]}" exceeded; revoking keys matching {a["key_match"]}')
        else:
            print(f'Cost Center: on, no limits exceeded (checked {last["at"]})')
    else:
        print('Cost Center: off')
    cost_center_failing = costcenter.enabled(db) and not (store.json_setting(db, 'cost_center_last') or {}).get('ok', True)
    return 1 if overdue or store.json_setting(db, 'last_failure') or cost_center_failing else 0


def status_json(db):
    """What Meta Cost Center shows about Keyguard. It never includes a secret."""
    moment = store.now()
    keys = [{'label': r['label'], 'state': r['state'], 'meta_name': r['meta_name'], 'expires_at': store.iso(store.due_at(r))
             if store.due_at(r) else None, 'overdue': store.is_overdue(r, moment), 'auto_rotate': bool(r['auto_rotate']),
             'last_error': r['last_error']} for r in store.live_keys(db) if r['state'] != 'sentinel']
    found, snapshot_at = ops.targets(db)
    revocations = [dict(json.loads(e['detail'] or '{}'), at=e['at'])
                   for e in store.events(db, 500) if e['kind'] == 'cost-center-revoked']
    return {'keyguard_version': __version__, 'generated_at': store.iso(moment),
            'bridge_running': store.paths.socket_path().exists(),
            'cost_center': {'enabled': costcenter.enabled(db), 'last_check': store.json_setting(db, 'cost_center_last')},
            'last_failure': store.json_setting(db, 'last_failure'),
            'keys': keys, 'targets': found, 'targets_as_of': snapshot_at, 'revocations': revocations[-50:]}


def doctor(db, ext, inspect):
    from . import install
    ok = True

    def report(good, text):
        nonlocal ok
        ok = ok and good
        print(('ok   ' if good else 'FAIL ') + text)

    report(sys.version_info >= (3, 11), f'Python {sys.version.split()[0]}')
    report(bool(os.environ.get('WSL_DISTRO_NAME')), f'WSL distro: {os.environ.get("WSL_DISTRO_NAME") or "not detected"}')
    data = store.paths.data_dir()
    report(data.stat().st_mode & 0o077 == 0, f'Data directory is private: {data}')
    try:
        found = install.check()
        report(found['registered'], 'Native messaging host registered' if found['registered']
               else 'Native messaging host not registered; run `keyguard install-bridge`')
        if found['registered']:
            report(found['extension_current'] is True, 'Installed extension matches this checkout' if
                   found['extension_current'] else 'Installed extension is out of date; run `keyguard install-bridge` '
                   'and reload the extension in chrome://extensions')
            report(found.get('host_command_current') is True, 'Host launcher points at this checkout' if
                   found.get('host_command_current') else 'Host launcher is stale; run `keyguard install-bridge`')
    except Exception as exc:
        report(False, f'Could not inspect the Windows install: {exc}')
    try:
        info = ext.call('bridge-info')
        report(True, f'Bridge running (pid {info["pid"]})')
        # Chrome keeps one bridge process alive, so it does not see code changes until it restarts.
        newest = max(p.stat().st_mtime for p in Path(__file__).parent.glob('*.py'))
        report(info.get('started_at', 0) >= newest, 'Bridge is running the current code' if info.get('started_at', 0) >= newest
               else 'Bridge is running older code; reload the extension in chrome://extensions to restart it')
        report(info['extension_version'] is not None, f'Extension connected (version {info["extension_version"]})')
        who = ops.call(db, ext, 'whoami')
        report(True, f'Signed in to dev.meta.ai; team {who["team"]["name"] or who["team"]["id"]} '
                     f'({who["key_count"]} key(s), can create: {who.get("can_create", "unknown")})')
        if who.get('block_reason'):
            report(False, f'dev.meta.ai blocks key creation: {who["block_reason"]}')
        if inspect:
            print(json.dumps(ops.call(db, ext, 'inspect'), indent=2))
    except ExtensionError as exc:
        report(False, f'{exc.code}: {exc}')
    if costcenter.enabled(db):
        try:
            alerts = costcenter.statuses(db)
            report(True, f'Cost Center answered ({len(alerts)} alert(s), '
                         f'{sum(1 for a in alerts if a.get("revoke_keys"))} revoking keys)')
        except costcenter.CostCenterError as exc:
            report(False, str(exc))
    else:
        print('     Cost Center check is off (`keyguard cost-center on` to enable)')
    return 0 if ok else 1


def config(db, args):
    if args.action == 'show':
        for key in sorted(store.DEFAULTS):
            print(f'{key} = {store.setting(db, key)}')
        return 0
    if not args.key:
        raise ops.KeyguardError('Name a setting')
    if args.action == 'unset':
        store.set_setting(db, args.key, None)
        return 0
    if args.value is None:
        raise ops.KeyguardError('Give a value')
    if args.key in store.DURATION_SETTINGS:
        durations.parse(args.value)
    if args.key == 'cost_center' and args.value not in ('on', 'off'):
        raise ops.KeyguardError('cost_center is on or off')
    store.set_setting(db, args.key, args.value)
    store.log(db, 'config', key=args.key, value=args.value)
    return 0


def main(argv=None):
    args = parser().parse_args(argv)
    if args.command == 'bridge':
        from .bridge import main as bridge_main
        bridge_main()
        return
    if args.command in ('install-bridge', 'uninstall-bridge'):
        from . import install
        try:
            if args.command == 'uninstall-bridge':
                print(f'Removed {install.uninstall()} and the registry entry.')
                return
            where = install.install()
        except install.InstallError as exc:
            print(f'keyguard: {exc}', file=sys.stderr)
            raise SystemExit(1)
        print(f'Installed the native messaging host in {where["directory"]}.\n\n'
              'If the extension is not loaded yet:\n'
              '  1. Open chrome://extensions and turn on Developer mode.\n'
              f'  2. Click "Load unpacked" and choose {where["extension"]}\n'
              'If it is already loaded, click its reload button. Then run `keyguard doctor`.')
        return
    db = None
    ext = BridgeClient()
    try:
        db = store.connect()
        code = run(db, ext, args)
    except ExtensionError as exc:
        print(f'keyguard: {exc.code}: {exc}', file=sys.stderr)
        if exc.detail:
            print(json.dumps(exc.detail, indent=2), file=sys.stderr)
        code = 2
    except (ops.KeyguardError, costcenter.CostCenterError, ValueError, OSError, sqlite3.Error) as exc:
        print(f'keyguard: {exc}', file=sys.stderr)
        code = 1
    except KeyboardInterrupt:
        code = 130
    finally:
        if db is not None:
            db.close()
    raise SystemExit(code or 0)


def run(db, ext, args):
    moment = store.now()
    if args.command == 'status':
        if args.json:
            print(json.dumps(status_json(db), indent=2))
            return 0
        return status(db)
    if args.command == 'doctor':
        return doctor(db, ext, args.inspect)
    if args.command == 'config':
        return config(db, args)
    if args.command == 'log':
        for e in store.events(db, args.n):
            detail = f' {e["detail"]}' if e['detail'] else ''
            print(f'{e["at"]}  {e["kind"]:<18} {e["label"] or "":<24}{detail}')
        return 0
    if args.command == 'cost-center':
        if args.action in ('on', 'off'):
            store.set_setting(db, 'cost_center', args.action)
            store.log(db, 'config', key='cost_center', value=args.action)
            print(f'Cost Center check is {args.action}.')
            if args.action == 'off':
                return 0
        alerts = costcenter.check(db)
        print('No alert is revoking keys.' if not alerts else
              '\n'.join(f'Exceeded: {a.get("label")} -> revokes {a.get("key_match")}' for a in alerts))
        return 0
    if args.command in ('get', 'env', 'exec'):
        row = usable(db, args.label)
        if args.command == 'get':
            print(row['secret'])
        elif args.command == 'env':
            print(f'export {args.var or store.setting(db, "env_var")}={shlex.quote(row["secret"])}')
        else:
            argv = args.argv[1:] if args.argv[:1] == ['--'] else args.argv
            if not argv:
                raise ops.KeyguardError('Give a command after --, for example: keyguard exec dev -- python app.py')
            env = dict(os.environ)
            for name in args.var or [store.setting(db, 'env_var')]:
                env[name] = row['secret']
            db.close()
            os.execvpe(argv[0], argv, env)
        return 0
    if args.command == 'list':
        rows = store.all_keys(db) if args.history else store.live_keys(db)
        print(table(key_rows(rows, moment), ['LABEL', 'STATE', 'EXPIRES IN', 'AUTO', 'META NAME', 'LAST ERROR'])
              if rows else 'No keys stored.')
        if args.all:
            with store.lock():
                unmanaged = ops.sync(db, ext)
            print('\nOn dev.meta.ai but not created by Keyguard:')
            print(table([(k.get('name') or '', k['id'], created(k.get('created_at'))) for k in unmanaged],
                        ['NAME', 'ID', 'CREATED']) if unmanaged else '  none')
        return 0
    if args.command == 'extend':
        row = ops.extend(db, args.label, args.ttl)
        with store.lock():
            ops.push_schedule(db, ext)
        print(f'{args.label} now expires {row["expires_at"]}.')
        return 0
    if args.command == 'kill' and not args.yes:
        if not sys.stdin.isatty():
            raise ops.KeyguardError('kill deletes every key on dev.meta.ai; pass --yes to run it non-interactively')
        if input('This deletes EVERY key on your dev.meta.ai team, including ones Keyguard did not create.\n'
                 'Type kill to continue: ').strip() != 'kill':
            print('Cancelled.')
            return 1
    with store.lock():
        if args.command == 'create':
            row = ops.create(db, ext, args.label, args.ttl, args.auto_rotate, args.ignore_cost_center)
            print(f'Created {row["meta_name"]}; it expires in {remaining(row, store.now())}.\n'
                  f'Use it with: keyguard exec {args.label} -- <command>')
        elif args.command == 'rotate':
            row = ops.rotate(db, ext, args.label, args.grace)
            grace = store.setting(db, 'rotate_grace') if args.grace is None else durations.format(args.grace)
            print(f'Rotated {args.label} to {row["meta_name"]}; the old key is deleted after {grace}.')
        elif args.command == 'revoke':
            try:
                rows = ops.revoke(db, ext, args.label)
                print(f'Revoked {len(rows)} key(s) labelled {args.label}.')
            except ops.KeyguardError:
                if ops.LABEL.fullmatch(args.label) and store.active_key(db, args.label):
                    raise
                target = ops.revoke_remote(db, ext, args.label)
                print(f'Deleted {target.get("name") or target["id"]} from dev.meta.ai.')
        elif args.command == 'kill':
            rows, unmanaged = ops.kill(db, ext, args.reason)
            print(f'Deleted {len(rows)} managed and {len(unmanaged)} unmanaged key(s). '
                  'Only the sentinel key, whose secret nobody has, remains.')
        elif args.command == 'sync':
            unmanaged = ops.sync(db, ext)
            print(f'Store reconciled. {len(unmanaged)} key(s) on dev.meta.ai were not created by Keyguard.')
        elif args.command == 'reap':
            report = ops.reap(db, ext)
            print_report(report)
            return 1 if report['failures'] else 0
        if args.command not in ('sync',):
            ops.push_schedule(db, ext)
    return 0
