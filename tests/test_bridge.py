import os
import subprocess
import sys
import threading
import time
import unittest

from helpers import ROOT, FakeMeta, StoreTestCase

from meta_keyguard import nativemsg, paths, store
from meta_keyguard.extension import ExtensionError


class FakeChrome:
    """Plays the extension's side of native messaging against a real `keyguard bridge` process."""

    def __init__(self, env):
        self.meta = FakeMeta()
        self.received = []
        self.process = subprocess.Popen([sys.executable, str(ROOT / 'keyguard'), 'bridge'], stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, env={**os.environ, **env})
        nativemsg.write_message(self.process.stdin, {'type': 'hello', 'version': 'test'})
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()
        deadline = time.time() + 10
        while not paths.socket_path().exists():
            if time.time() > deadline:
                raise AssertionError('bridge did not open its socket')
            time.sleep(0.05)

    def serve(self):
        while True:
            message = nativemsg.read_message(self.process.stdout)
            if message is None:
                return
            self.received.append(dict(message))
            if 'op' in message:
                request_id, op = message['request_id'], message['op']
                try:
                    reply = {'request_id': request_id, 'ok': True, 'result': self.meta.call(op, **message['params'])}
                except ExtensionError as exc:
                    reply = {'request_id': request_id, 'ok': False, 'error': {'code': exc.code, 'message': str(exc)}}
                nativemsg.write_message(self.process.stdin, reply)

    def send(self, message):
        nativemsg.write_message(self.process.stdin, message)

    def close(self):
        self.process.stdin.close()
        self.process.wait(10)
        self.process.stdout.close()


class BridgeTests(StoreTestCase):
    def setUp(self):
        super().setUp()
        self.chrome = FakeChrome(self.env)

    def tearDown(self):
        self.chrome.close()
        super().tearDown()

    def cli(self, *args, check=True):
        done = subprocess.run([sys.executable, str(ROOT / 'keyguard'), *args], capture_output=True, text=True,
                              env={**os.environ, **self.env}, timeout=30)
        if check and done.returncode != 0:
            raise AssertionError(f'keyguard {" ".join(args)} failed ({done.returncode}): {done.stderr}')
        return done

    def test_cli_round_trip_through_the_bridge(self):
        self.assertIn('Created dev-kg', self.cli('create', 'dev', '--ttl', '2h').stdout)
        secret = self.cli('get', 'dev').stdout.strip()
        self.assertTrue(secret.startswith('sk-meta-'))
        self.assertEqual(self.cli('exec', 'dev', '--', sys.executable, '-c',
                                  'import os; print(os.environ["META_API_KEY"])').stdout.strip(), secret)
        self.assertIn(f'export MUSE_API_KEY={secret}', self.cli('env', 'dev', '--var', 'MUSE_API_KEY').stdout)
        listing = self.cli('list', '--all').stdout
        self.assertIn('hand-made', listing)
        self.assertIn('1h 59m', listing)
        # The bridge pushed the schedule to the extension after the create.
        schedules = [m['params'] for m in self.chrome.received if m.get('op') == 'schedule']
        self.assertEqual(schedules[-1]['keys'][0]['label'], 'dev')
        refused = self.cli('kill', check=False)
        self.assertEqual(refused.returncode, 1)
        self.assertIn('--yes', refused.stderr)
        self.cli('kill', '--yes')
        self.assertEqual(self.chrome.meta.names(), ['keyguard-sentinel'])
        self.assertEqual(self.cli('get', 'dev', check=False).returncode, 1)

    def test_extension_failures_reach_the_cli(self):
        self.chrome.meta.fail['create'] = 'not_logged_in'
        done = self.cli('create', 'dev', check=False)
        self.assertEqual(done.returncode, 2)
        self.assertIn('not_logged_in', done.stderr)
        self.assertIn('Sign in at https://dev.meta.ai/', done.stderr)
        status = self.cli('status', check=False)
        self.assertEqual(status.returncode, 1)
        self.assertIn('Last failure', status.stdout)

    def test_events_from_the_extension_update_the_store(self):
        self.cli('create', 'dev')
        meta_id = store.active_key(self.db, 'dev')['meta_id']
        self.chrome.send({'type': 'event', 'event': {'event_id': 'e1', 'kind': 'deleted', 'meta_id': meta_id,
                                                     'reason': 'expired (deleted by the extension)'}})
        deadline = time.time() + 10
        while not any(m.get('type') == 'event_ack' for m in self.chrome.received):
            self.assertLess(time.time(), deadline, 'no acknowledgement')
            time.sleep(0.05)
        row = store.by_meta_id(self.db, meta_id)
        self.assertEqual((row['state'], row['secret']), ('revoked', None))

    def test_requests_fail_fast_once_chrome_disconnects(self):
        self.chrome.close()
        done = self.cli('list', '--all', check=False)
        self.assertEqual(done.returncode, 2)
        self.assertIn('bridge_offline', done.stderr)
        self.assertFalse(paths.socket_path().exists())


if __name__ == '__main__':
    unittest.main()
