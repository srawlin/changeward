"""Download (once), verify and exec the Changegate binary for this platform."""

import hashlib
import io
import os
import platform
import sys
import tarfile
import urllib.request

from . import __version__

BASE = os.environ.get("CHANGEGATE_DOWNLOAD_BASE", f"https://github.com/srawlin/changegate/releases/download/v{__version__}")


def _fail(msg):
    sys.stderr.write(f"changegate launcher: {msg}\n")
    sys.exit(1)


def _get(url):
    if url.startswith("file://"):
        with open(url[7:], "rb") as f:
            return f.read()
    req = urllib.request.Request(url, headers={"User-Agent": "changegate-pypi"})
    with urllib.request.urlopen(req, timeout=60) as r:  # noqa: S310 (fixed https base)
        return r.read()


def binary_path():
    if os.environ.get("CHANGEGATE_BINARY"):
        return os.environ["CHANGEGATE_BINARY"]
    system = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
    arch = {"x86_64": "amd64", "AMD64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(platform.machine())
    if not system or not arch:
        _fail(f"unsupported platform {platform.system()}/{platform.machine()}")
    cache = os.environ.get("CHANGEGATE_CACHE") or os.path.join(os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache"), "changegate")
    target_dir = os.path.join(cache, __version__)
    target = os.path.join(target_dir, "changegate")
    if os.path.exists(target):
        return target
    asset = f"changegate_{system}_{arch}.tar.gz"
    tgz = _get(f"{BASE}/{asset}")
    sums = _get(f"{BASE}/checksums.txt").decode()
    want = next((p[0] for p in (line.split() for line in sums.splitlines()) if len(p) == 2 and p[1] == asset), None)
    if not want:
        _fail(f"no checksum for {asset}")
    got = hashlib.sha256(tgz).hexdigest()
    if got != want:
        _fail(f"checksum mismatch for {asset}: got {got}, want {want}")
    with tarfile.open(fileobj=io.BytesIO(tgz), mode="r:gz") as tf:
        member = next((m for m in tf.getmembers() if m.name in ("changegate", "./changegate") and m.isfile()), None)
        if member is None:
            _fail(f"changegate not found in {asset}")
        data = tf.extractfile(member).read()
    os.makedirs(target_dir, exist_ok=True)
    tmp = f"{target}.{os.getpid()}.tmp"
    with open(tmp, "wb") as f:
        f.write(data)
    os.chmod(tmp, 0o755)
    os.replace(tmp, target)
    return target


def main():
    path = binary_path()
    os.execv(path, [path] + sys.argv[1:])


if __name__ == "__main__":
    main()
