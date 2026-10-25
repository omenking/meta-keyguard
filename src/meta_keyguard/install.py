"""Registering the native messaging host with Chrome on the Windows side of WSL."""
import hashlib
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

from . import paths

HOST_NAME = 'com.metakeyguard.bridge'
# Fixed by the public key in extension/manifest.json, so the host manifest can name it in advance.
EXTENSION_ID = 'admjfgnffhpgjnnbnkogihlnpeeeenio'
REGISTRY_KEY = rf'HKCU\Software\Google\Chrome\NativeMessagingHosts\{HOST_NAME}'
ROOT = Path(__file__).resolve().parents[2]
EXTENSION_SOURCE = ROOT / 'extension'
LAUNCHER = ROOT / 'keyguard'


class InstallError(Exception):
    pass


def windows(*command, check=True):
    """Run a Windows program from WSL. A Windows working directory avoids UNC path warnings."""
    try:
        done = subprocess.run(command, cwd='/mnt/c', capture_output=True, text=True, timeout=30)
    except FileNotFoundError:
        raise InstallError(f'{command[0]} is not available; Keyguard needs WSL with Windows interop enabled') from None
    if check and done.returncode != 0:
        raise InstallError(f'{" ".join(command)} failed: {(done.stderr or done.stdout).strip()}')
    return done


def to_linux(windows_path):
    return Path(subprocess.run(['wslpath', '-u', windows_path], capture_output=True, text=True, check=True).stdout.strip())


def to_windows(linux_path):
    return subprocess.run(['wslpath', '-w', str(linux_path)], capture_output=True, text=True, check=True).stdout.strip()


def install_dir():
    local = windows('cmd.exe', '/c', 'echo %LOCALAPPDATA%').stdout.strip()
    if not local or '%' in local:
        raise InstallError('Could not find %LOCALAPPDATA% on Windows')
    return to_linux(local) / 'MetaKeyguard'


def extension_digest(folder):
    """A hash of the extension files, used to tell whether the installed copy is current."""
    digest = hashlib.sha256()
    for path in sorted(p for p in folder.rglob('*') if p.is_file() and 'tests' not in p.relative_to(folder).parts):
        digest.update(str(path.relative_to(folder)).encode() + b'\0' + path.read_bytes())
    return digest.hexdigest()


def host_command():
    """The command Chrome runs: Python inside this distro, with the data directory pinned."""
    distro = os.environ.get('WSL_DISTRO_NAME')
    if not distro:
        raise InstallError('WSL_DISTRO_NAME is not set; run install-bridge from inside WSL')
    # wsl.exe takes quotes around the -d value literally, so the distro name must stay bare.
    if any(c in distro for c in ' "%&|<>^'):
        raise InstallError(f'Unsupported characters in the WSL distro name {distro!r}')
    return (f'"%SystemRoot%\\System32\\wsl.exe" -d {distro} --exec /usr/bin/env '
            f'"META_KEYGUARD_DATA={paths.data_dir()}" "{sys.executable}" "{LAUNCHER}" bridge')


def install():
    target = install_dir()
    target.mkdir(parents=True, exist_ok=True)
    extension = target / 'extension'
    if extension.exists():
        shutil.rmtree(extension)
    shutil.copytree(EXTENSION_SOURCE, extension, ignore=shutil.ignore_patterns('tests', '*.test.js'))
    # cmd.exe needs CRLF line endings, and @echo off keeps the batch file from writing to the
    # stdout that carries native messages.
    (target / 'keyguard-host.bat').write_bytes(f'@echo off\r\n{host_command()}\r\n'.encode())
    manifest = {'name': HOST_NAME, 'description': 'Meta Keyguard bridge to WSL', 'path': 'keyguard-host.bat',
                'type': 'stdio', 'allowed_origins': [f'chrome-extension://{EXTENSION_ID}/']}
    (target / f'{HOST_NAME}.json').write_text(json.dumps(manifest, indent=2))
    windows('reg.exe', 'add', REGISTRY_KEY, '/ve', '/t', 'REG_SZ', '/d', to_windows(target / f'{HOST_NAME}.json'), '/f')
    paths.ensure_data_dir()
    return {'directory': to_windows(target), 'extension': to_windows(extension)}


def uninstall():
    windows('reg.exe', 'delete', REGISTRY_KEY, '/f', check=False)
    target = install_dir()
    if target.exists():
        shutil.rmtree(target)
    return to_windows(target)


def check():
    """What doctor reports about the installation."""
    result = {'registered': False, 'manifest': None, 'extension_current': None}
    done = windows('reg.exe', 'query', REGISTRY_KEY, '/ve', check=False)
    if done.returncode != 0:
        return result
    result['registered'] = True
    value = done.stdout.strip().splitlines()[-1].split('REG_SZ', 1)[-1].strip()
    result['manifest'] = value
    manifest = to_linux(value)
    installed = manifest.parent / 'extension'
    if manifest.exists() and installed.exists():
        result['extension_current'] = extension_digest(installed) == extension_digest(EXTENSION_SOURCE)
        bat = (manifest.parent / 'keyguard-host.bat').read_text(errors='replace')
        result['host_command_current'] = host_command() in bat
    return result
