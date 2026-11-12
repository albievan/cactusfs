# Version 1 wrapped key

`kms.json` is a key file, and `wrapped.json` a data key wrapped by it, both written by
the version 1 code of this package on 2026-11-12, before ADR-0031 changed the format of
a wrapped key. The passphrase is the one of `testPassphrase` in `local_test.go`; the key
and the data key protect nothing but this test.

`TestWrappedKeysOfVersion1StillUnwrap` opens them with the current code. Never
regenerate them: their worth is that they were made by the old code.
