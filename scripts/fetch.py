#!/usr/bin/env python3
"""Download an archive, check it against a SHA-256, and unpack it.

    fetch.py <url> <sha256> <directory>

Used when the builder image is built, for test tools that are released as
archives. Nothing is unpacked unless the checksum matches. File modes are
kept, which the zipfile module does not do by itself.
"""
import hashlib
import io
import os
import sys
import tarfile
import urllib.request
import zipfile


def main() -> int:
    if len(sys.argv) != 4:
        print(__doc__, file=sys.stderr)
        return 2
    url, want, directory = sys.argv[1:]
    with urllib.request.urlopen(url, timeout=300) as response:
        data = response.read()
    got = hashlib.sha256(data).hexdigest()
    if got != want:
        print(f"fetch: {url}: sha256 is {got}, want {want}", file=sys.stderr)
        return 1
    os.makedirs(directory, exist_ok=True)
    if url.endswith(".zip"):
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            for member in archive.infolist():
                path = archive.extract(member, directory)
                mode = member.external_attr >> 16
                if mode:
                    os.chmod(path, mode & 0o777)
    else:
        with tarfile.open(fileobj=io.BytesIO(data)) as archive:
            archive.extractall(directory, filter="data")
    return 0


if __name__ == "__main__":
    sys.exit(main())
