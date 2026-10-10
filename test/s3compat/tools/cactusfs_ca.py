"""pytest plugin: trust the certificate authority of the compatibility run.

ceph/s3-tests asks botocore to verify TLS with "verify=True", which botocore
reads as "against the bundle of certifi" - and nothing in the configuration of
the tests, nor an environment variable, can name another bundle. The
compatibility suite serves TLS under a certificate authority it makes for the
run. Rather than switch verification off, this plugin makes "the bundle of
certifi" be that authority's certificate, for this process only.

Loaded with "-p cactusfs_ca"; the file is named by CACTUSFS_CA_BUNDLE.
"""
import os

import botocore.httpsession
import certifi

_BUNDLE = os.environ["CACTUSFS_CA_BUNDLE"]


def _where():
    return _BUNDLE


certifi.where = _where
botocore.httpsession.where = _where
