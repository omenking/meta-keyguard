"""Durations such as 12h, 30m, 1d12h, or 90s."""
import re

UNITS = {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}
PART = re.compile(r'(\d+)([smhd])')


def parse(text):
    """Return a duration in seconds, rejecting anything that is not whole units."""
    value = str(text).strip().lower()
    if not value or PART.sub('', value):
        raise ValueError(f'Invalid duration {text!r}; use forms such as 30m, 12h, or 1d')
    seconds = sum(int(n) * UNITS[u] for n, u in PART.findall(value))
    if seconds <= 0:
        raise ValueError('Duration must be greater than zero')
    return seconds


def format(seconds):
    seconds = int(seconds)
    if seconds < 0:
        return '-' + format(-seconds)
    if seconds < 60:
        return f'{seconds}s'
    days, rest = divmod(seconds, 86400)
    hours, rest = divmod(rest, 3600)
    minutes = rest // 60
    parts = [f'{v}{u}' for v, u in ((days, 'd'), (hours, 'h'), (minutes, 'm')) if v]
    return ' '.join(parts[:2])
