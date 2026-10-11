# ADR-0018: Tag rules, lower-case tags, and a catalogue of tags

- **Status:** accepted; being implemented (PLAN D.1-D.6)
- **Date:** 2026-10-12
- **Builds on:** ADR-0011 (tags of buckets and objects), ADR-0016 (entry schema 3),
  ADR-0017 (sub-buckets)

## Context

Objects and buckets have had S3's key-value tags since milestone 6, through the S3 API
only. Albie wants them in the console; rules ("if an object is uploaded I want to be
able to select tags that must be supplied"); all tags and values in lower case; and
tags he can define for users to pick.

S3 keeps the case of tags, and the compatibility baseline has tests that say so
(`test_set_bucket_tagging`, `test_put_max_kvsize_tags`). CLAUDE.md allows no regression
of that baseline.

## Decision

- **Tag rules are a configuration of a bucket** (kind `tagrules`, schema 3):
  `lowercase`, `catalogue_only`, and at most 10 required tags, each a key, optionally
  the values allowed, optionally confined to one sub-bucket.
- **Lower-casing is one of the rules.** Where it is on, keys and values are stored in
  lower case whatever case they arrive in, by every way a tag arrives - S3 included;
  two keys that become one are refused, not merged. It is **on for every bucket made
  through the management API or the console, and off for a bucket made with S3's
  `CreateBucket`** until its rules switch it on. Tags stored before are not rewritten;
  they are lower-cased when next written. The catalogue is always lower case.
  Deployment-wide lower-casing would be Albie's to choose, at the price of two entries
  of the compatibility baseline.
- **Rules are enforced in the use case, for every way an object comes to have tags:**
  PutObject, CreateMultipartUpload (early) and CompleteMultipartUpload, CopyObject (on
  the tags the copy ends with), the management API's upload, PutObjectTagging; and
  DeleteObjectTagging is refused where a tag is required. Tags on
  CreateMultipartUpload, 501 until now, are implemented: a bucket that requires tags
  would otherwise take no multipart upload. Objects that exist are left as they are.
- A violation is `domain.ErrTagRule`, an `ErrInvalidTag`: S3 answers 400 `InvalidTag`,
  the management API a problem with code `tag_rule_violated`. The message names the
  rule, never a value.
- Rules are read once a request, like the policy. A put that races a change of rules
  may be judged by the rules before.
- **The catalogue** is deployment-wide: definitions of a key, a description and
  optionally the values allowed (at most 200 definitions, 50 values each), replicated
  by `tag.definition.put` and `tag.definition.delete` and audited like changes to
  identities. The console offers them when tagging and when writing rules. Tags outside
  the catalogue stay allowed unless a bucket's rules say `catalogue_only`. A definition
  that a rule names cannot be deleted.
- **Actions:** `s3:GetBucketTagRules`, `s3:PutBucketTagRules`;
  `admin:{List,Get,Put,Delete}TagDefinition`. Every built-in role can list the
  catalogue; `cluster-admin` changes it.
- The management API gains tags of a bucket and of an object, tag rules, the catalogue,
  and tags on its upload; the console gains editors for all of it and asks, at upload,
  for the tags the bucket requires.

## Consequences

- An S3 client that sends no tags is refused by a bucket that requires them: that is
  the rule working.
- In a bucket that lower-cases, a client that writes `Stage=Draft` reads back
  `stage=draft`.

## Alternatives

- **Lower-casing everywhere, always** - Albie's words taken alone; it breaks the
  compatibility contract he also set.
- **Rules in the console only** - a rule that S3 clients can ignore is not a rule.
