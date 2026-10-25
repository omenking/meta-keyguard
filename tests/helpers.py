import os
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'src'))

from meta_keyguard import store  # noqa: E402
from meta_keyguard.extension import ExtensionError  # noqa: E402


class FakeMeta:
    """Stands in for the extension and dev.meta.ai, including the last-key rule."""

    def __init__(self, keys=None):
        self.keys = list(keys or [{'id': 'manual-1', 'name': 'hand-made'}])
        self.next_id = 100
        self.calls = []
        self.fail = {}

    def call(self, op, timeout=90, team_id=None, project_id=None, **params):
        self.calls.append((op, params))
        if op in self.fail:
            raise ExtensionError(self.fail[op])
        if op == 'list':
            return {'team': {'id': 't1', 'name': 'Me'}, 'keys': [dict(k) for k in self.keys]}
        if op == 'create':
            self.next_id += 1
            key = {'id': str(self.next_id), 'name': params['name']}
            self.keys.append(key)
            secret = None if params.get('discard_secret') else f'sk-meta-{self.next_id}-secret'
            return {**key, 'secret': secret}
        if op == 'delete':
            if not any(k['id'] == params['id'] for k in self.keys):
                return {'deleted': False, 'already_gone': True}
            if len(self.keys) == 1:
                raise ExtensionError('last_key')
            self.keys = [k for k in self.keys if k['id'] != params['id']]
            return {'deleted': True}
        if op == 'schedule':
            self.schedule = params['keys']
            return {'scheduled': len(params['keys'])}
        if op == 'whoami':
            return {'team': {'id': 't1', 'name': 'Me'}, 'key_count': len(self.keys), 'can_create': True}
        raise AssertionError(op)

    def names(self):
        return sorted(k['name'] for k in self.keys)


class StoreTestCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.env = {'META_KEYGUARD_DATA': str(Path(self.tmp.name) / 'data')}
        self.saved = {k: os.environ.get(k) for k in self.env}
        os.environ.update(self.env)
        self.db = store.connect()
        self.meta = FakeMeta()

    def tearDown(self):
        self.db.close()
        for k, v in self.saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        self.tmp.cleanup()
