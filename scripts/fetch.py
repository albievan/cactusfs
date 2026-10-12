#!/usr/bin/env python3
"""Download an archive, check it against a SHA-256, and unpack it.

    fetch.py <url> <sha256> <directory> [<prefix>]
    fetch.py --file <url> <sha256> <path>

Used when the builder image is built, for test tools that are released as
archives. Nothing is unpacked unless the checksum matches. File modes are
kept, which the zipfile module does not do by itself.

With a prefix, only the members whose names begin with it are unpacked - one
directory of a large archive - and it is an error when there is none. The
archive is kept in a temporary file, not in memory, while it is checked.

With --file, what is downloaded is not an archive: it is written to the path
as it is, mode 0644, and there is no file at the path unless the checksum
matches.
"""
import hashlib
import os
import sys
import tarfile
import tempfile
import urllib.request
import zipfile


def download(url: str, data) -> str:
    """Writes what is at the URL to data and returns its SHA-256."""
    digest = hashlib.sha256()
    with urllib.request.urlopen(url, timeout=300) as response:
        while True:
            block = response.read(1 << 20)
            if not block:
                break
            digest.update(block)
            data.write(block)
    return digest.hexdigest()


def fetch_file(url: str, want: str, path: str) -> int:
    """Downloads one file to the path, which exists only if the checksum matches."""
    directory = os.path.dirname(path) or "."
    os.makedirs(directory, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=directory, delete=False) as data:
        try:
            got = download(url, data)
            if got != want:
                print(f"fetch: {url}: sha256 is {got}, want {want}", file=sys.stderr)
                return 1
            data.flush()
            os.chmod(data.name, 0o644)
            os.replace(data.name, path)
        finally:
            if os.path.exists(data.name):
                os.unlink(data.name)
    return 0


def main() -> int:
    if len(sys.argv) == 5 and sys.argv[1] == "--file":
        return fetch_file(*sys.argv[2:])
    if len(sys.argv) not in (4, 5) or sys.argv[1].startswith("--"):
        print(__doc__, file=sys.stderr)
        return 2
    url, want, directory = sys.argv[1:4]
    prefix = sys.argv[4] if len(sys.argv) == 5 else ""
    with tempfile.TemporaryFile() as data:
        got = download(url, data)
        if got != want:
            print(f"fetch: {url}: sha256 is {got}, want {want}", file=sys.stderr)
            return 1
        data.seek(0)
        os.makedirs(directory, exist_ok=True)
        unpacked = 0
        if url.endswith(".zip"):
            with zipfile.ZipFile(data) as archive:
                for member in archive.infolist():
                    if not member.filename.startswith(prefix):
                        continue
                    path = archive.extract(member, directory)
                    mode = member.external_attr >> 16
                    if mode:
                        os.chmod(path, mode & 0o777)
                    unpacked += 1
        else:
            with tarfile.open(fileobj=data) as archive:
                wanted = []
                for member in archive:
                    # "./name" and "name" are the same member.
                    name = member.name[2:] if member.name.startswith("./") else member.name
                    if name.startswith(prefix):
                        wanted.append(member)
                archive.extractall(directory, members=wanted, filter="data")
                unpacked = len(wanted)
        if not unpacked:
            print(f"fetch: {url}: nothing in the archive begins with {prefix!r}", file=sys.stderr)
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
