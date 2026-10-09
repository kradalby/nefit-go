"""Private output for research recordings, which hold device credentials.

A shared or pre-created directory would let another local user read captures
or plant files; outputs never follow or reuse an existing path.
"""
import io
import os
from pathlib import Path
import stat


def default():
    state = os.environ.get('XDG_STATE_HOME') or Path.home() / '.local' / 'state'
    return Path(state) / 'nefit-go' / 'research'


def directory(path):
    """Create path 0700, or accept it only if this user owns it privately."""
    path = Path(path)
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise SystemExit(f'{path}: must be a real directory owned by uid {os.geteuid()} with mode 0700')
    return path


class _File(io.FileIO):
    """Unbuffered, but a short write is finished or raises: recordings are
    indexed by byte offset."""

    def write(self, data):
        view = memoryview(data).cast('B')
        while view:
            view = view[os.write(self.fileno(), view):]
        return len(data)


def create(path):
    """Open a new 0600 file for unbuffered writing."""
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
    return _File(fd, 'wb')
