# ADR-0014: A default console user

- **Status:** accepted and implemented (PLAN A.1)
- **Date:** 2026-10-11
- **Decided by:** Albie, against the recommendation recorded below
- **Changes:** CLAUDE.md §3 "No default credentials", §10, §11; ADR-0006 (identity)

## Context

Until now a deployment had no built-in account. Its first start required or generated
root credentials (an API key), shown once; a console user had to be made with that key
before anybody could sign in to the console. Albie asked for "a default user admin and
password admin". Asked how, with the options of a development-only account, a generated
password, or none, he chose: always on.

## Decision

- A new deployment is given the console user `admin` with the password `admin` and the
  role `cluster-admin`.
- It is made **once**, by the node that stores the root credential, right after that.
  A deployment whose operator changed the password, changed the roles or deleted the
  user keeps what the operator did: the user is never made again and the password is
  never set back. Deployments that exist before this change do not get the user.
- The password is stored as every console password is: an Argon2id hash. The rule that
  a password has at least 12 characters still holds for every password that is set
  afterwards, this user's own included: `admin` cannot be chosen again.
- The user is a console user only. It has no API key; the S3 API and the bearer
  credential of the management API still need a key, and the root credential is still
  generated or supplied as before. There is still no default API key.
- Making the user is an audit record (`system:BootstrapAdmin`), and the node logs a
  warning naming the user when it makes it. The password is in neither.

## Consequences

- Whoever can reach the admin port of a new deployment before its operator changes the
  password has every right a policy can give, including reading every object through
  the console and making API keys. Object Lock in compliance mode still holds against
  that account, as against any other.
- The sign-in rate limit and the audit trail apply to this user as to any other, but a
  password that is public is not guessed: the first attempt succeeds.
- Operators must delete the user, or change the password, before the admin listener is
  reachable by anybody else. README, the threat model, the start-up log and the notice
  on standard error say so. The compose cluster publishes the admin port on loopback.
- Changing the password is the weaker remedy. It does not end a session that is
  already open (30 minutes at most), and whatever was made through such a session - an
  API key for `admin`, another user, a role, a policy - stays. Deleting the user ends
  its sessions and removes its keys. "Console user only, no API key" describes the user
  as it is made, not what somebody who signs in as it can then do.
- The name `admin` is taken on every new deployment: provisioning that creates a user
  of that name gets 409 until the default one is deleted.

## How "once" can fail, and which way

The user is a second entry in the metadata log, after the one that stores the root
credential, and "this node began the deployment" is known only in memory, to the node
that stored that credential. So:

- If the node stops between the two entries, or the second fails, the deployment has
  **no default user**, and never gets one. The safe direction; the operator makes a
  console user with the root key, as before this ADR.
- If the outcome of storing a **supplied** root credential is unknown (leadership lost
  while it was being applied) the node cannot tell whether it or another node given the
  same credential stored it, and makes no user. With a generated credential it can
  tell, and does.
- If the outcome of making the user is unknown, the node looks: when the user is there
  with the default password it warns exactly as if it had seen the entry applied. A
  deployment never has the default user without the warning.

## Alternatives

- **Only with an explicit development flag, loopback only** — recommended, not chosen.
- **The user `admin` with a generated password shown once** — offered, not chosen.
- **No default user** — the rule as it stood.

## Not done, and could be

- Forcing a change of password at the first sign-in.
- A setting that leaves the user out (`CACTUSFS_NO_DEFAULT_ADMIN`), for deployments
  made by automation.
- A warning in the console and a readiness note for as long as the password is `admin`.
