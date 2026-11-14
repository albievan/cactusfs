# ADR-0018: Tag rules, lower-case tags, and a catalogue of tags

- **Status:** accepted; the domain rules and actions, the port, the replicated store
  (PLAN D.1, S.3), the management API's contract (PLAN D.0), the use cases, which
  enforce the rules on S3's every way an object comes to have tags (PLAN D.2-D.3), and
  the management API's handlers - tags, tag rules, the catalogue, an upload's tags,
  and a bucket made through the API lower-casing its tags (PLAN D.4) - are implemented.
  The four-node test, `TestTagRulesAcrossTheCluster`, and an S3 compatibility test of
  tags on a multipart upload pass (PLAN D.5). The console's editors - a bucket's tags
  and tag rules, an object's tags, the catalogue, and tags on the upload form - are
  implemented (PLAN D.6). The ADR is implemented
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
  An upload is judged again at completion, by the rules of then and with the tags it
  was begun with: one begun before `lowercase` was set, or before a stricter rule, whose
  tags those rules do not allow, is refused at completion and must be begun again -
  the completion entry does not carry tags, so they cannot be lower-cased there
  (that would be entry schema 7).
- A violation is `domain.ErrTagRule`, an `ErrInvalidTag`: S3 answers 400 `InvalidTag`,
  the management API a problem with code `TagRuleViolated` (PascalCase, as every code
  of that API; PLAN D.0). The message names the
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
  `admin:ListTagDefinitions`, `admin:GetTagDefinition`, `admin:PutTagDefinition`,
  `admin:DeleteTagDefinition` (the list in the plural, as `admin:ListUsers` and
  `admin:ListTargets` are; PLAN D.1). Every built-in role can list and read the
  catalogue; `cluster-admin` changes it. The tag-rule actions are held as a bucket's
  other configurations are: by `s3:*`, so by `cluster-admin` and `bucket-admin`; removing
  the rules is `s3:PutBucketTagRules`, as removing CORS or lifecycle rules is the action
  that sets them.
- The management API gains tags of a bucket and of an object, tag rules, the catalogue,
  and tags on its upload; the console gains editors for all of it and asks, at upload,
  for the tags the bucket requires.

## Consequences

- An S3 client that sends no tags is refused by a bucket that requires them: that is
  the rule working.
- In a bucket that lower-cases, a client that writes `Stage=Draft` reads back
  `stage=draft`.
- **Rolling upgrades.** Rules are kept by the nodes of a release that enforces them
  (PLAN D.2b on). Rules can be set as soon as every member knows entry schema 3, which
  every release since S.3 does, though none before D.2b enforces them: while a cluster
  is being upgraded, a node of an older release stores what it is sent without checking
  it - an S3 `PutObject` without the required tags, say - and an older gateway's
  management upload ignores `X-CactusFS-Tagging` and stores the object without tags.
  Such objects are left as they are, as those stored before the rules were set. Gating
  `putBucketTagRules` until every member runs an enforcing release is the alternative,
  open for Albie (PLAN Decisions, 2026-11-14); it would need its own ADR.
- A required key or value, and a key or value of the catalogue, must be one a tag can
  have (S3's characters and lengths, `object.ValidateTagKey`/`ValidateTagValue`):
  otherwise a rule could refuse every object, and a definition could never be picked.
  This is checked by the proposer's domain types; the state machine keeps checking bytes
  only, so the entry schema does not change.
- **A bucket made through the management API is two entries** - the bucket, then its
  rule `lowercase` (one entry would be a new entry schema). When the second fails the
  bucket is deleted again, if it is still only what was made (no configuration,
  sub-bucket or upload in progress; its record as written), and the caller is told the
  error; the attempt is audited as failed (`tag_rules=failed`, `undone=true`). When it
  cannot be deleted again - something else is in it by now, the store cannot say, or the
  deletion fails - the bucket is there without the rule: the caller is answered 201, so
  that a repeat with the same Idempotency-Key agrees, the creation is audited as
  succeeded with `tag_rules=not_set`, and the log says so at ERROR. Buckets made through
  the API therefore lower-case their tags except after that failure, which the trail and
  the log name; their rules can be set then. Rules found written after all (an outcome
  that was not known) make the creation a success. A bucket another request deleted
  before its rules were made is not taken back by anybody: the creation is answered 409
  `Conflict`, kept for its Idempotency-Key so that a repeat does not make again what
  another caller deleted, and audited as failed (`undone=false`,
  `removed_by_other=true`). A deletion whose outcome is not known is decided by reading
  the bucket again. The answer of a creation is the bucket as written - nothing is read
  or authorized again - so a caller who may create buckets and not list them is answered
  201. Left for later, as each needs a conditional deletion or one entry (an ADR and an
  entry schema): a write between the look and the deletion is not seen (and a bucket
  made again by someone in that moment would be deleted), and a creation whose outcome
  is not known but was committed leaves a bucket that no record names (as `Create` has
  always done).
- While a member of the metadata group does not know entry schema 3, every bucket made
  through the management API is made and then deleted again, answered 503, for as long
  as the rolling upgrade lasts. Every release since S.3 knows it, so this holds only for
  an upgrade from an older one. S3's `CreateBucket` is not affected.
- **A required tag shuts out whoever may store and not tag** (X.1 review, 2026-11-14;
  a decision for Albie). Tagging is a permission of its own: an upload that carries
  tags needs `s3:PutObjectTagging` as well as `s3:PutObject` (`admitTags`,
  `internal/app/tagrules.go`), as in S3, and one that carries none breaks a rule that
  requires a tag (`bucket.TagRules.Check`). So once a bucket requires any tag, a caller
  who holds `s3:PutObject` and not `s3:PutObjectTagging` cannot store an object there
  at all, with tags or without - the built-in `read-write` role among them
  (`internal/domain/iam/builtin.go`), and a user whose sub-bucket or bucket policy grants
  `s3:PutObject` alone. The same holds for a copy and a multipart upload. Kept, as the
  stricter reading, and documented; an administrator who wants such users to upload
  grants them `s3:PutObjectTagging` on the bucket (an identity policy, the bucket's or a
  sub-bucket's policy). Open for Albie: (a) give `read-write` `s3:PutObjectTagging`;
  (b) let the tags a rule requires pass under `s3:PutObject` alone; (c) keep it.
- **Rules no object can satisfy are accepted.** `SetTagRules` judges the rules by
  themselves and never reads the catalogue: under `catalogue_only` a required key the
  catalogue does not define, a required value it does not list, or a key in mixed case
  where the bucket does not lower-case (the catalogue is lower case) is accepted, and
  the bucket then refuses every upload the rule covers. A definition can also be changed
  later (`TagDefinitionService.Put`) so that it no longer lists the values a rule
  requires. The guard on deleting a definition (`EntityInUse`) is narrow: it looks only
  for a required tag with exactly that key, so it does not protect a definition that a
  `catalogue_only` bucket relies on for tags it does not require, nor match a required
  key in another case. Accepted, deferred: rules are set by administrators and audited,
  and the refusals name the rule. Cross-checking rules and catalogue in both directions
  is open for Albie.
- **A rule confined to a sub-bucket is a test of the key's prefix** (`sub_bucket`), not a
  link to the registered sub-bucket. It may name a sub-bucket that does not exist, and
  deleting a sub-bucket (`SubBucketService.Delete`) neither consults nor changes the
  rules that name it: such a rule keeps applying to keys under `<name>/` with no
  sub-bucket there, and to a sub-bucket created later under that name. Accepted,
  deferred (ADR-0017, "Consequences").

## Alternatives

- **Lower-casing everywhere, always** - Albie's words taken alone; it breaks the
  compatibility contract he also set.
- **Rules in the console only** - a rule that S3 clients can ignore is not a rule.
