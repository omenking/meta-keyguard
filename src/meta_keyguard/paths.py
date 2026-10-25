"""Where Keyguard keeps its state. Everything lives in one private data directory."""
import hashlib
import os
from pathlib import Path

# Unix socket paths are limited to 108 bytes on Linux, including the terminating NUL.
MAX_SOCKET_PATH = 100


def data_dir():
    # The native host is launched by Chrome without the user's shell environment, so the
    # installer passes the data directory explicitly through META_KEYGUARD_DATA.
    explicit = os.environ.get('META_KEYGUARD_DATA')
    if explicit:
        return Path(explicit).expanduser()
    return Path(os.environ.get('XDG_DATA_HOME', Path.home() / '.local/share')) / 'meta-keyguard'


def ensure_data_dir():
    path = data_dir()
    path.mkdir(parents=True, mode=0o700, exist_ok=True)
    path.chmod(0o700)
    return path


def db_path():
    return data_dir() / 'keys.sqlite3'


def socket_path():
    preferred = data_dir() / 'bridge.sock'
    if len(str(preferred).encode()) <= MAX_SOCKET_PATH:
        return preferred
    # A deep data directory would overflow the limit, so use a short private directory keyed by it.
    runtime = Path(os.environ.get('XDG_RUNTIME_DIR') or f'/tmp/meta-keyguard-{os.getuid()}')
    if not os.environ.get('XDG_RUNTIME_DIR'):
        runtime.mkdir(mode=0o700, exist_ok=True)
        info = runtime.lstat()
        if info.st_uid != os.getuid() or info.st_mode & 0o077 or runtime.is_symlink():
            raise OSError(f'{runtime} is not a private directory owned by you')
    digest = hashlib.sha256(str(data_dir()).encode()).hexdigest()[:16]
    return runtime / f'meta-keyguard-{digest}.sock'


def lock_path():
    return data_dir() / 'ops.lock'
