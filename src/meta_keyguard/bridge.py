"""The native messaging host. Chrome starts it through wsl.exe and keeps its stdin/stdout open.

The bridge relays requests from CLI processes (over a Unix socket) to the extension (over
native messaging), applies events the extension reports on its own, and runs the reaper
once a minute for as long as Chrome keeps it alive.
"""
import json
import os
import socket
import sys
import threading
import time
import traceback
import uuid

from . import __version__, nativemsg, ops, paths, store
from .extension import ExtensionError, from_reply

REAP_INTERVAL = 60
STARTED_AT = time.time()


class Bridge:
    def __init__(self, inp, out, sock_path=None):
        self.inp, self.out = inp, out
        self.sock_path = sock_path or paths.socket_path()
        self.write_lock = threading.Lock()
        self.pending = {}
        self.stopping = threading.Event()
        self.extension_version = None

    # Extension side -----------------------------------------------------------------------

    def send(self, message):
        with self.write_lock:
            nativemsg.write_message(self.out, message)

    def call(self, op, timeout=90, **params):
        """Ask the extension to do something and wait for its answer."""
        request_id = uuid.uuid4().hex
        slot = {'done': threading.Event()}
        self.pending[request_id] = slot
        try:
            self.send({'request_id': request_id, 'op': op, 'params': params})
            if not slot['done'].wait(timeout):
                raise ExtensionError('timeout')
            return from_reply(slot['reply'])
        finally:
            self.pending.pop(request_id, None)

    def read_loop(self):
        while not self.stopping.is_set():
            message = nativemsg.read_message(self.inp)
            if message is None:
                break
            slot = self.pending.get(message.get('request_id')) if 'request_id' in message else None
            if slot is not None:
                slot['reply'] = message
                slot['done'].set()
            elif message.get('type') == 'hello':
                self.extension_version = message.get('version')
                threading.Thread(target=self.on_hello, daemon=True).start()
            elif message.get('type') == 'event':
                self.apply_event(message.get('event') or {})
        self.stopping.set()
        # Chrome has gone, so nothing will answer the requests still waiting.
        for slot in list(self.pending.values()):
            slot['reply'] = {'ok': False, 'error': {'code': 'bridge_offline', 'message': None}}
            slot['done'].set()

    def on_hello(self):
        """A (re)connected extension gets the current schedule straight away."""
        db = store.connect()
        try:
            ops.push_schedule(db, self)
        except ExtensionError:
            pass
        finally:
            db.close()

    def apply_event(self, event):
        """Record what the extension did while acting on its own, then acknowledge it."""
        db = store.connect()
        try:
            kind = event.get('kind')
            if kind == 'deleted':
                row = store.by_meta_id(db, event.get('meta_id'))
                if row and row['state'] != 'revoked':
                    store.mark_revoked(db, row, event.get('reason') or 'deleted by the extension')
            elif kind == 'sentinel-created' and not store.by_meta_id(db, event.get('meta_id')):
                store.insert_key(db, label=ops.SENTINEL_NAME, meta_id=event.get('meta_id'),
                                 meta_name=event.get('name'), secret=None, state='sentinel',
                                 created_at=event.get('at') or store.iso(store.now()))
            elif kind == 'failure':
                store.record_failure(db, event.get('op') or 'extension', event.get('code') or 'unknown',
                                     event.get('message') or '')
                row = store.by_meta_id(db, event.get('meta_id')) if event.get('meta_id') else None
                if row:
                    store.update_key(db, row['id'], last_error=f'{event.get("code")}: {event.get("message")}')
        finally:
            db.close()
        if event.get('event_id'):
            self.send({'type': 'event_ack', 'event_id': event['event_id']})

    # CLI side -----------------------------------------------------------------------------

    def serve(self):
        paths.ensure_data_dir()
        try:
            self.sock_path.unlink()
        except FileNotFoundError:
            pass
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        old_umask = os.umask(0o177)
        try:
            server.bind(str(self.sock_path))
        finally:
            os.umask(old_umask)
        self.sock_inode = self.sock_path.stat().st_ino
        server.listen(8)
        server.settimeout(1)
        self.server = server
        while not self.stopping.is_set():
            try:
                conn, _ = server.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            threading.Thread(target=self.handle_client, args=(conn,), daemon=True).start()

    def handle_client(self, conn):
        with conn:
            data = b''
            while not data.endswith(b'\n'):
                chunk = conn.recv(65536)
                if not chunk:
                    return
                data += chunk
            request = json.loads(data)
            op = request.pop('op')
            timeout = request.pop('timeout', 90)
            try:
                if self.stopping.is_set():
                    raise ExtensionError('bridge_offline')
                if op == 'bridge-info':
                    result = {'bridge_version': __version__, 'extension_version': self.extension_version,
                              'pid': os.getpid(), 'started_at': STARTED_AT}
                else:
                    result = self.call(op, timeout=timeout, **request)
                reply = {'ok': True, 'result': result}
            except ExtensionError as exc:
                reply = {'ok': False, 'error': {'code': exc.code, 'message': str(exc), 'detail': exc.detail}}
            conn.sendall(json.dumps(reply).encode() + b'\n')

    # Reaper -------------------------------------------------------------------------------

    def reap_loop(self):
        while not self.stopping.wait(REAP_INTERVAL):
            db = store.connect()
            try:
                with store.lock():
                    ops.reap(db, self)
            except Exception:
                # A failure here is already recorded by ops.call; keep the loop alive regardless.
                traceback.print_exc(file=sys.stderr)
            finally:
                db.close()

    def run(self):
        threading.Thread(target=self.serve, daemon=True).start()
        threading.Thread(target=self.reap_loop, daemon=True).start()
        try:
            self.read_loop()
        finally:
            self.stopping.set()
            # A second Chrome profile may have started a newer bridge on the same path; leave it alone.
            try:
                if self.sock_path.stat().st_ino == getattr(self, 'sock_inode', None):
                    self.sock_path.unlink()
            except FileNotFoundError:
                pass


def main():
    # Anything printed to stdout would corrupt the native messaging stream, so diagnostics go
    # to a log file in the data directory instead (Chrome discards the host's stderr).
    paths.ensure_data_dir()
    log = open(paths.data_dir() / 'bridge.log', 'a', buffering=1)
    os.chmod(log.name, 0o600)
    sys.stderr = log
    Bridge(sys.stdin.buffer, sys.stdout.buffer).run()
