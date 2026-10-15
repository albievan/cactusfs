# Test data of the metadata store

Golden files and evidence that pin what the replicated state machine writes. A file here
is the bytes some release wrote: a test that fails against one has found that two releases
would disagree, and the answer is in the code or in a new entry schema, not in the file.

## Frozen: the bytes of entry schema 5

`schema5-entries.golden.txt` and `schema5-records.golden.txt` are **frozen from PLAN
8.0.10 on**, the step in which this release announced entry schema 5
(`MetadataVersion = 5`, ADR-0023). From then on members of two releases write and read
the entries and the stored records of schema 5, so neither file may change:

| File | What it pins | SHA-256 |
|---|---|---|
| `schema5-entries.golden.txt` | every command of schema 5, and every old command with what schema 5 lets it carry, as it is logged | `4f4222da822e819a4421f590aefc260141948ef2b86b6303f296a8704e952107` |
| `schema5-records.golden.txt` | every record schema 5 added to the state, and every older record with what schema 5 added to it | `a5a082a02a100b8313b35234594281bc9c00f4a93a27911ab531088c385f6c2d` |

`TestTheEntriesOfSchema5ArePinned` and `TestTheRecordsOfSchema5ArePinned` hold the code to
the files; `TestTheGoldenFilesOfSchema5AreFrozen` holds the files to these sums. A change
that makes any of the three fail needs a schema 6 - an ADR, a migration and an upgrade
test (CLAUDE.md, section 10) - and never a new file or a new sum. The writer
`TestWriteSchema5Golden` (`schema5_golden_gen_test.go`) is kept to show how the files
came to be and must not be run to replace them.

The three `bucket-usage-record*.golden.json` files pin the usage record, which the domain
encodes (`internal/domain/bucket/testdata/`); they are frozen with it.

## Evidence of earlier releases

| File | Written by | Held by |
|---|---|---|
| `schema3-release.log.hex`, `schema3-release.state.json` | the code of the release of schema 3, in an export of its commit | `schema3_release_test.go` |
| `schema4-release.sha256`, `schema4-entries.golden.txt` | the code of commit ddbfe43, the last tree without anything of schema 5 | `schema4_release_test.go` |
| `access-record.golden.json`, `bucket-activity-record.golden.json` | pinned when schema 3 and schema 4 were announced | `schema3_test.go`, `schema4_test.go` |

None of these is written again by a later tree: each is what THAT code made.
