"""Talking to the Chrome extension through the bridge's Unix socket."""
import json
import socket

from . import paths

# What to tell the user for each failure the extension or bridge can report. Failures are
# never silent: every one reaches the CLI as an ExtensionError carrying one of these codes.
HINTS = {
    'bridge_offline': 'Chrome is not running, or the Meta Keyguard extension is not connected. Open Chrome, '
                      'check the extension is enabled, and run `keyguard doctor`.',
    'timeout': 'The extension did not answer in time. Check that Chrome is responsive and that dev.meta.ai loads.',
    'not_logged_in': 'Sign in at https://dev.meta.ai/ in Chrome, then try again.',
    'reauth_required': 'dev.meta.ai wants you to confirm your identity. Open https://dev.meta.ai/api-keys in Chrome, '
                       'complete the prompt, then try again.',
    'forbidden': 'dev.meta.ai refused the request for this account.',
    'create_blocked': 'dev.meta.ai will not create keys for this account (for example, no payment method).',
    'endpoint_changed': 'dev.meta.ai answered in a way Keyguard does not recognise; its internal API may have '
                        'changed. Run `keyguard doctor --inspect` and update extension/adapter.js.',
    'secret_not_found': 'A key was created but Keyguard could not find its secret in the response, so it deleted '
                        'that key again. The internal API has probably changed; see `keyguard doctor --inspect`.',
    'multiple_teams': 'Your account has several teams. Choose one with `keyguard config set team_id <id>`.',
    'multiple_projects': 'This team has several projects. Choose one with `keyguard config set project_id <id>` '
                         '(the details below list them).',
    'project_unknown': 'Keyguard could not find which project to create keys in. Open https://dev.meta.ai/api-keys '
                       'in Chrome and pick the project, or set it with `keyguard config set project_id <id>`; '
                       'the project ID appears as ?project_id= in the dashboard URL.',
    'rate_limited': 'dev.meta.ai is rate limiting requests. Wait a minute and try again.',
    'meta_unavailable': 'dev.meta.ai returned a server error. Try again later.',
    'network': 'Chrome could not reach dev.meta.ai.',
    'last_key': 'dev.meta.ai will not delete the last key on a team, and Keyguard could not create a sentinel key.',
    'tab_failed': 'The extension could not open or use a dev.meta.ai tab.',
}


class ExtensionError(Exception):
    def __init__(self, code, message=None, detail=None):
        self.code = code
        self.detail = detail
        super().__init__(message or HINTS.get(code, code))

    @property
    def hint(self):
        return HINTS.get(self.code)


def from_reply(reply):
    """Turn a bridge reply into its result, or raise the error it carries."""
    if reply.get('ok'):
        return reply.get('result')
    error = reply.get('error') or {}
    raise ExtensionError(error.get('code', 'unknown'), error.get('message'), error.get('detail'))


class BridgeClient:
    """Sends one request per connection to the running `keyguard bridge`."""

    def __init__(self, path=None):
        self.path = path or paths.socket_path()

    def call(self, op, timeout=90, **params):
        if not self.path.exists():
            raise ExtensionError('bridge_offline')
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(timeout + 5)
        try:
            conn.connect(str(self.path))
            conn.sendall(json.dumps({'op': op, 'timeout': timeout, **params}).encode() + b'\n')
            data = b''
            while not data.endswith(b'\n'):
                chunk = conn.recv(65536)
                if not chunk:
                    break
                data += chunk
        except (ConnectionRefusedError, FileNotFoundError):
            raise ExtensionError('bridge_offline') from None
        except socket.timeout:
            raise ExtensionError('timeout') from None
        finally:
            conn.close()
        if not data:
            raise ExtensionError('bridge_offline', 'The bridge closed the connection without answering.')
        return from_reply(json.loads(data))
