import json
import os
import stat
import sys
import unittest
from datetime import timedelta
from pathlib import Path

from helpers import StoreTestCase

from meta_keyguard import costcenter, durations, ops, paths, store
from meta_keyguard.extension import ExtensionError


class DurationTests(unittest.TestCase):
    def test_parse_and_format(self):
        self.assertEqual(durations.parse('12h'), 43200)
        self.assertEqual(durations.parse('1d12h'), 129600)
        self.assertEqual(durations.parse('90s'), 90)
        for bad in ('', '12', 'h', '1w', '0m', '-5m', '5 m'):
            with self.assertRaises(ValueError):
                durations.parse(bad)
        self.assertEqual(durations.format(43200 - 480), '11h 52m')
        self.assertEqual(durations.format(129600), '1d 12h')


class OperationTests(StoreTestCase):
    def test_store_is_private(self):
        self.assertEqual(stat.S_IMODE(paths.data_dir().stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(paths.db_path().stat().st_mode), 0o600)

    def test_socket_path_stays_within_the_unix_limit(self):
        os.environ['META_KEYGUARD_DATA'] = str(Path(self.tmp.name) / ('deep' * 30))
        try:
            self.assertLessEqual(len(str(paths.socket_path()).encode()), paths.MAX_SOCKET_PATH)
        finally:
            os.environ.update(self.env)

    def test_create_uses_default_ttl_and_rejects_bad_labels_and_long_ttls(self):
        row = ops.create(self.db, self.meta, 'dev')
        self.assertEqual(row['ttl_seconds'], 12 * 3600)
        self.assertTrue(row['meta_name'].startswith('dev-kg'))
        self.assertTrue(row['secret'].startswith('sk-meta-'))
        with self.assertRaises(ops.KeyguardError):
            ops.create(self.db, self.meta, 'dev')  # one active key per label
        for bad in ('Dev', '-x', 'a b', 'keyguard-sentinel-2', 'x' * 41):
            with self.assertRaises(ops.KeyguardError):
                ops.create(self.db, self.meta, bad)
        with self.assertRaises(ops.KeyguardError):
            ops.create(self.db, self.meta, 'long', ttl=durations.parse('8d'))
        store.set_setting(self.db, 'default_ttl', '30m')
        self.assertEqual(ops.create(self.db, self.meta, 'short')['ttl_seconds'], 1800)

    def test_secrets_never_reach_the_audit_log(self):
        row = ops.create(self.db, self.meta, 'dev')
        ops.rotate(self.db, self.meta, 'dev', grace=0)
        ops.kill(self.db, self.meta)
        log = json.dumps(store.events(self.db, 1000))
        self.assertNotIn(row['secret'], log)
        self.assertNotIn('sk-meta-', log)

    def test_rotate_keeps_the_old_key_for_the_grace_period(self):
        old = ops.create(self.db, self.meta, 'dev', ttl=3600)
        new = ops.rotate(self.db, self.meta, 'dev', grace=300)
        self.assertNotEqual(old['secret'], new['secret'])
        self.assertEqual(store.active_key(self.db, 'dev')['id'], new['id'])
        self.assertEqual(new['ttl_seconds'], 3600)
        retiring = store.by_meta_id(self.db, old['meta_id'])
        self.assertEqual(retiring['state'], 'retiring')
        self.assertIn(old['meta_id'], [k['id'] for k in self.meta.keys])
        report = ops.reap(self.db, self.meta, store.now() + timedelta(seconds=301))
        self.assertEqual(report['retired'], ['dev'])
        self.assertNotIn(old['meta_id'], [k['id'] for k in self.meta.keys])
        self.assertIsNone(store.by_meta_id(self.db, old['meta_id'])['secret'])

    def test_reap_deletes_expired_keys_and_auto_rotates(self):
        ops.create(self.db, self.meta, 'plain', ttl=600)
        ops.create(self.db, self.meta, 'rolling', ttl=1200, auto_rotate=True)
        store.set_setting(self.db, 'rotate_grace', '20m')
        # Five minutes before 'rolling' expires it is inside rotate_before (10m); 'plain' has expired.
        report = ops.reap(self.db, self.meta, store.now() + timedelta(seconds=900))
        self.assertEqual(report['expired'], ['plain'])
        self.assertEqual(report['rotated'], ['rolling'])
        self.assertIsNone(store.active_key(self.db, 'plain'))
        self.assertEqual(len([k for k in self.meta.keys if k['name'].startswith('rolling-')]), 2)
        self.assertEqual({k['label'] for k in self.meta.schedule}, {'rolling'})

    def test_short_auto_rotating_keys_are_not_rotated_every_minute(self):
        row = ops.create(self.db, self.meta, 'quick', ttl=120, auto_rotate=True)
        # Well inside the default 10m rotate_before, but not within a quarter of the 2m TTL.
        self.assertEqual(ops.reap(self.db, self.meta, store.now() + timedelta(seconds=60))['rotated'], [])
        self.assertEqual(ops.reap(self.db, self.meta, store.now() + timedelta(seconds=95))['rotated'], ['quick'])
        self.assertEqual(store.by_meta_id(self.db, row['meta_id'])['state'], 'retiring')

    def test_deleting_the_last_key_creates_a_sentinel(self):
        self.meta.keys = []
        row = ops.create(self.db, self.meta, 'only')
        ops.revoke(self.db, self.meta, 'only')
        self.assertEqual(self.meta.names(), ['keyguard-sentinel'])
        sentinel = [r for r in store.live_keys(self.db) if r['state'] == 'sentinel']
        self.assertEqual(len(sentinel), 1)
        self.assertIsNone(sentinel[0]['secret'])
        self.assertTrue(('create', {'name': 'keyguard-sentinel', 'discard_secret': True}) in self.meta.calls)
        self.assertEqual(store.by_meta_id(self.db, row['meta_id'])['state'], 'revoked')

    def test_kill_deletes_every_key_including_unmanaged_ones(self):
        ops.create(self.db, self.meta, 'a')
        ops.create(self.db, self.meta, 'b')
        rows, unmanaged = ops.kill(self.db, self.meta, 'test')
        self.assertEqual(({r['label'] for r in rows}, [k['name'] for k in unmanaged]), ({'a', 'b'}, ['hand-made']))
        self.assertEqual(self.meta.names(), ['keyguard-sentinel'])
        self.assertEqual([r['state'] for r in store.live_keys(self.db)], ['sentinel'])
        # A second kill has nothing left to do and creates no further sentinel.
        ops.kill(self.db, self.meta)
        self.assertEqual(self.meta.names(), ['keyguard-sentinel'])

    def test_failures_are_recorded_and_keys_kept_for_retry(self):
        ops.create(self.db, self.meta, 'dev', ttl=60)
        self.meta.fail['delete'] = 'not_logged_in'
        report = ops.reap(self.db, self.meta, store.now() + timedelta(seconds=61))
        self.assertEqual([(n, e.code) for n, e in report['failures']], [('dev', 'not_logged_in')])
        row = store.active_key(self.db, 'dev')
        self.assertTrue(store.is_overdue(row, store.now() + timedelta(seconds=61)))
        self.assertIn('not_logged_in', row['last_error'])
        self.assertEqual(store.json_setting(self.db, 'last_failure')['code'], 'not_logged_in')
        del self.meta.fail['delete']
        ops.reap(self.db, self.meta, store.now() + timedelta(seconds=61))
        self.assertIsNone(store.active_key(self.db, 'dev'))
        self.assertIsNone(store.json_setting(self.db, 'last_failure'))

    def test_sync_marks_missing_keys_and_reports_unmanaged(self):
        row = ops.create(self.db, self.meta, 'dev')
        self.meta.keys = [k for k in self.meta.keys if k['id'] != row['meta_id']]
        unmanaged = ops.sync(self.db, self.meta)
        self.assertEqual([k['name'] for k in unmanaged], ['hand-made'])
        self.assertEqual(store.by_meta_id(self.db, row['meta_id'])['revoke_reason'], 'missing on dev.meta.ai')

    def test_missing_secret_is_an_error(self):
        original = self.meta.call
        self.meta.call = lambda op, **p: {**original(op, **p), 'secret': None} if op == 'create' else original(op, **p)
        with self.assertRaises(ExtensionError) as caught:
            ops.create(self.db, self.meta, 'dev')
        self.assertEqual(caught.exception.code, 'secret_not_found')


class CostCenterTests(StoreTestCase):
    def fake_cost_center(self, alerts, code=0):
        script = Path(self.tmp.name) / 'muse-cost'
        script.write_text(f'#!{sys.executable}\nimport sys\nprint({json.dumps(json.dumps(alerts))})\nsys.exit({code})\n')
        script.chmod(0o700)
        store.set_setting(self.db, 'cost_center_command', str(script))

    def test_python_cost_center_runs_with_keyguards_python(self):
        # A shebang naming a Python that does not exist, as `env python3` can resolve to an unusable one.
        self.fake_cost_center([{'id': 1, 'level': 'ok', 'revoke_keys': True, 'key_match': '*'}])
        script = Path(store.setting(self.db, 'cost_center_command'))
        script.write_text('#!/nonexistent/python3\n' + script.read_text().split('\n', 1)[1])
        self.assertEqual(costcenter.statuses(self.db)[0]['id'], 1)

    def test_off_by_default_and_never_consulted(self):
        self.fake_cost_center([], code=1)
        ops.create(self.db, self.meta, 'dev')
        self.assertEqual(ops.reap(self.db, self.meta)['failures'], [])

    def test_exceeded_alert_revokes_matching_keys_only(self):
        ops.create(self.db, self.meta, 'bootcamp-api')
        ops.create(self.db, self.meta, 'bootcamp-web')
        ops.create(self.db, self.meta, 'other')
        self.meta.keys.append({'id': 'manual-2', 'name': 'bootcamp-manual'})
        self.fake_cost_center([
            {'id': 1, 'label': 'Daily', 'level': 'exceeded', 'enabled': True, 'revoke_keys': True, 'key_match': 'bootcamp-*'},
            {'id': 2, 'label': 'Warn only', 'level': 'exceeded', 'enabled': True, 'revoke_keys': False, 'key_match': None},
            {'id': 3, 'label': 'Not yet', 'level': 'warning', 'enabled': True, 'revoke_keys': True, 'key_match': '*'}])
        store.set_setting(self.db, 'cost_center', 'on')
        report = ops.reap(self.db, self.meta)
        self.assertEqual(sorted(report['cost_center']), ['bootcamp-api', 'bootcamp-manual', 'bootcamp-web'])
        self.assertEqual([n for n in self.meta.names() if not n.startswith('other-')], ['hand-made', 'keyguard-sentinel'])
        self.assertIsNotNone(store.active_key(self.db, 'other'))
        # New keys under a tripped alert are refused until the alert clears or is overridden.
        with self.assertRaises(ops.KeyguardError):
            ops.create(self.db, self.meta, 'bootcamp-api')
        ops.create(self.db, self.meta, 'bootcamp-api', ignore_cost_center=True)
        ops.create(self.db, self.meta, 'unrelated')

    def test_status_json_for_cost_center_has_targets_revocations_and_no_secrets(self):
        import subprocess
        from helpers import ROOT
        row = ops.create(self.db, self.meta, 'bootcamp-api')
        ops.create(self.db, self.meta, 'other')
        self.fake_cost_center([{'id': 7, 'label': 'Daily', 'level': 'exceeded', 'enabled': True,
                                'revoke_keys': True, 'key_match': 'bootcamp-*'}])
        store.set_setting(self.db, 'cost_center', 'on')
        ops.reap(self.db, self.meta)  # refreshes the remote snapshot, then revokes bootcamp-api
        done = subprocess.run([sys.executable, str(ROOT / 'keyguard'), 'status', '--json'], capture_output=True,
                              text=True, env={**os.environ, **self.env}, check=True)
        self.assertNotIn('sk-meta-', done.stdout)
        status = json.loads(done.stdout)
        self.assertTrue(status['cost_center']['enabled'])
        self.assertEqual([k['label'] for k in status['keys']], ['other'])
        self.assertEqual(status['targets'], [{'name': 'other', 'managed': True}, {'name': 'hand-made', 'managed': False}])
        self.assertEqual([(r['alert_id'], r['keys']) for r in status['revocations']], [(7, ['bootcamp-api'])])
        self.assertIsNotNone(status['targets_as_of'])
        self.assertIsNone(store.active_key(self.db, 'bootcamp-api'))
        self.assertEqual(store.by_meta_id(self.db, row['meta_id'])['state'], 'revoked')

    def test_specific_names_and_star(self):
        alert = {'key_match': 'key1,key2'}
        self.assertTrue(costcenter.matches(alert, 'key2'))
        self.assertFalse(costcenter.matches(alert, 'key3'))
        self.assertTrue(costcenter.matches({'key_match': '*'}, 'anything'))
        self.assertFalse(costcenter.matches({'key_match': None}, 'anything'))

    def test_a_failing_check_is_reported_and_blocks_creation(self):
        self.fake_cost_center([], code=1)
        store.set_setting(self.db, 'cost_center', 'on')
        report = ops.reap(self.db, self.meta)
        self.assertEqual(report['failures'][0][0], 'cost-center')
        self.assertFalse(store.json_setting(self.db, 'cost_center_last')['ok'])
        with self.assertRaises(ops.KeyguardError):
            ops.create(self.db, self.meta, 'dev')


if __name__ == '__main__':
    unittest.main()
