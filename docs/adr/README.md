# Architecture decision records

An ADR records a decision that is expensive to reverse. Day-to-day decisions go in the
`## Decisions` table of `PLAN.md` instead.

An ADR is **required**, together with a migration and an upgrade test, before changing:

- the on-disk shard or metadata format,
- the Raft entry schema,
- the cryptographic design,
- where the object index lives (for example, a move to sharded Raft groups).

## Format

File name: `NNNN-short-title.md`, numbered in order from `0001`.

```
# NNNN. Title

Date: YYYY-MM-DD
Status: proposed | accepted | superseded by NNNN

## Context
What forces the decision.

## Decision
What we will do.

## Consequences
What becomes easier, what becomes harder, how upgrades and rollbacks work.
```

An accepted ADR is never edited to change its meaning; it is superseded by a new one.

## Index

None yet. The first ADRs are planned with steps 1.3 (on-disk format), 1.4 (encryption)
and 1.6 (metadata).
