# ADR-0027: A console session that locks while it uploads

- **Status:** accepted (2026-10-15, PLAN L.11): the server side (PLAN L.0 to L.8) and
  the console (PLAN L.9, L.10) have landed in commit edf32af, and the gates have seen
  both - `make check` and `make test-rolling`, the rolling case below included. Proposed
  on 2026-10-13
- **Date:** 2026-10-13
- **Revised:** 2026-10-13, to the findings of the first review: a locked session is
  given no stored answer; the keeper writes at every tick; a change of password ends
  the sessions of the user; ended records are removed by the node that made them.
  2026-10-15, to the findings of the second review: a session begins when its password
  is about to be checked; a margin of five seconds against the mark; a mark that cannot
  be written is audited; the console gives an unlock up after twenty seconds, never
  holds a sign-out back, and does not take a 401 to a request sent before an unlock for
  the end of the session
- **Supersedes in part:** ADR-0006, "sessions are fixed-length"
- **Refines:** ADR-0007, "Authentication: an API key as a bearer credential, or a console
  session"

## Context

The owner, 2026-10-13: "Do not time out the session while busy uploading objects; rather
disable all functionality and show a dialog box to type the user's password to enable
all functionality - unless all objects were uploaded and the session timed out: then log
off from the system."

What the code did before this ADR:

- A session lasted 30 minutes from sign-in (`app.DefaultSessionLifetime`), absolutely:
  `SessionService.Authenticate` refused a record once the node's clock had reached its
  `ExpiresAt`, and nothing renewed one. The timeout is not an idle timeout, and uploads
  did not keep a session alive.
- A request is authenticated once, when it begins. An upload that was running at minute
  30 was finished by the server. The console gave it up all the same: the next request
  answered 401, the console took that as "the session is gone" on the status alone, and
  it aborted every upload it had in flight. A batch longer than half an hour could not
  be uploaded through the console.
- Sessions are replicated state: a record `{token_hash, user, csrf_hash, created_at,
  expires_at}` in the `sessions` bucket, written by `session.put`, `session.delete` and
  `session.expire`. The bytes of entry schema 5 are fixed (ADR-0023); a field or a
  command more is a schema 6.
- Gateways are stateless and no load balancer is asked for sticky sessions
  (`docs/load-balancing.md`): whatever decides whether a session lives must decide alike
  on every node.

## Decision

### Three states, derived and not stored

A session with the stored record `R` is, at the time `now` of the node that is asked:

- **active** while `now < R.ExpiresAt`;
- **locked** while `R.ExpiresAt <= now < R.ExpiresAt + session-lock-max` **and** an
  upload lease `L` of the session exists with `now < L.ExpiresAt`;
- **ended** otherwise - and, whatever the above says, when the password of its user was
  changed since the session began ("A change of password", below).

The timeout that locks is the session's `ExpiresAt`: absolute from sign-in or from the
last unlock, as it was. No idle timeout is introduced, and **an upload never extends
full authority**: it only decides whether the session that timed out is locked or ended.

Nothing is stored that says "locked". The lock is a function of `R`, of `L` and of the
clock, as expiry always was.

### The upload lease

The lease is an ordinary session record, written with the `session.put` that exists:

| Field | Value |
|---|---|
| key (`token_hash`) | `upload-lease:` followed by the SHA-256, in hex, of the user's name, a line feed and `R.CreatedAt` as RFC 3339 with nanoseconds, in UTC |
| `user` | `R.User` |
| `csrf_hash` | empty |
| `created_at` | `R.CreatedAt` |
| `expires_at` | while an upload is in flight: the writer's `now` + `session-upload-grace`; when none is: the end of the last upload + `session-upload-grace` |

- **It is no credential.** A session is looked up under the SHA-256 of the token a
  request presents, which is 64 hexadecimal digits whatever the token is. The key of a
  lease is 77 characters and begins with a word and a colon: no token of any length
  hashes to it, on this release or on any release before it. A request that sends the
  key itself as its token is looked up under the hash of the key and finds nothing.
  `SessionService` refuses a record whose key has the form of a lease besides, should
  one ever be handed to it. The credential stays `R`.
- **Who writes it.** The gateway that serves `PUT /buckets/{bucket}/object` for a
  request authenticated by a session. That is the server's own knowledge of an upload in
  flight - not a claim of the console, which a stolen cookie could make as well. The
  adapter tells the session use case that an upload began and when it ended
  (`SessionService.UploadBegun`); that is plumbing, and decides nothing.
- **When.** `SessionService.Run` ticks every third of a grace. At every tick - whenever
  it comes; nothing is compared with the tick - it writes the lease of each session that
  has an upload in flight on the node, or had one begin or end since the tick before.
  And when an upload begins for a session whose lease is not there, or lasts less than
  two thirds of a grace more, `UploadBegun` wakes the keeper, which writes that lease at
  once: an upload never begins under a lease that could lapse before the second tick
  from now. The upload itself waits for nothing; the lease is written off the request's
  own time, as the last use of an API key is. How long a lease lasts is said when it is
  written, and one pass of the keeper has the time of one tick for all its writes.
- **What counts as an upload.** The adapter tells of one when the request has been
  authenticated and before the use case decides whether its user may store the object:
  a request that is then denied counted as an upload for as long as it lasted. It gains
  its session nothing but the lock, in which the same denial awaits it.
- **What it costs.** One Raft entry per third of the grace, per session that uploads,
  per gateway - with the defaults one entry in 30 seconds - and at times one more, when
  an upload begins late in the life of a lease: never more than two in a third of a
  grace, and never an entry per request. A thousand small files in a minute are two
  entries.
- **How long a session stays locked after its last upload.** A lease lasts one grace
  from when it was written while an upload is in flight, and one grace from the end of
  the last upload when none is - not from the keeper's pass, which comes later. So, for
  as long as ticks come and the writes succeed, a locked session ends **one grace after
  its last upload ended**. Sooner only when the lease could not be written again after
  that upload ended - the node stopped, the metadata group had no leader: then at the
  lapse of the lease written while the upload was in flight, which is two thirds of a
  grace after its end at the least, less what the last tick was late by.
- **When a lease cannot be written at all** - no leader for longer than a grace - the
  session of an upload in flight is ended, not locked, on every gateway that can still
  be asked; the console signs out and gives the upload up. A lock is lost in an outage
  of the metadata group; it is never gained in one.
- **It is replicated**, so every gateway decides alike and no sticky session is needed.
- **It is keyed by sign-in, not by token**: by the user and the time of sign-in, which
  an unlock carries over into the new record. An upload that began before an unlock
  still counts after it; a key by token would end the lock of a 50 GB upload at the
  second timeout. Two sessions of one user have two sign-in times and two leases.
- **It goes with the user** (`iam.user.delete` removes every session record of the
  user), and the sweep removes it when it has lapsed.

### What a locked session may do

- `SessionService.Authenticate` returns the principal with `LockedSession` set.
- `iam.Decide` - the one decision - refuses a locked principal every action but
  `s3:PutObject` with `domain.ErrSessionLocked`, before a policy is read. The sentinel
  does not wrap `ErrAccessDenied`, so that no use case can take it for a denial and
  answer "not found" or an empty list. `s3:PutObject` is then judged by the user's
  policies and the bucket's, exactly as for the same user unlocked.
- `Authorizer.Authorize` writes the refusal to the audit trail (reason "the session is
  locked") and to the log. `Authorizer.Allowed`, which filters lists, returns it as an
  error: a locked session is never answered an empty 200. Because that error ends the
  operation that asked - for a search of all buckets it is the only refusal there is -
  `Allowed` records it too, which it does of no other refusal; the auditor counts the
  repeats of one caller, so a filter writes one record.
- A use case that hides why it refuses - `IAMService.DeleteAPIKey` says "access denied"
  whatever the authorizer said, so as not to tell whose a key is - passes the lock on
  as the lock, and says as little of the key.
- Three operations ask the authorizer nothing, as before, and are what else a locked
  session can do: read its own state (`getSession`), unlock (`unlockSession`) and sign
  out (`signOut`).
- **What is answered before anybody is asked what the caller may do** is answered to a
  locked session as to any other, and is not 423: 415 for a body that is not
  `application/json`, 400 for a request the contract does not allow or an operation
  refuses as malformed, 404 for a path that is none, 405, and 501 for an operation that
  is not written yet. None of them tells more than the public contract does. 413 and
  the answers of the idempotency layer for a malformed key are of the same kind.
- **No stored answer.** A request that carries an `Idempotency-Key` it was sent with
  before is given the answer it was given then, without its use case being asked again
  (ADR-0007). To a locked session that would be a way round the lock - the answer of
  `createAPIKey`, made while the session was active, holds the secret of the key. So
  `IdempotencyService.Begin` is told who asks, and for a locked principal and anything
  but an upload asks the authorizer first (`auth:RepeatRequest`, which `iam.Decide`
  refuses a locked session like every action that is not `s3:PutObject`): the answer is
  423, the refusal is audited, no stored answer is looked for and no key is reserved -
  a locked session's request costs the replicated log nothing before it is refused. The
  upload of a locked session keeps its idempotency: the retry of a `PUT` is answered
  from the record, as the upload engine expects.
- No route is judged in an adapter. The S3 API is not touched: a session is not a
  credential there, so a locked principal cannot arise; its error table maps the
  sentinel to `AccessDenied` all the same. API keys are never locked.

### 423 and 403, and why not 401

- A locked session is answered **423** with the problem code `SessionLocked`, and no
  `WWW-Authenticate`.
- A wrong password at unlock is answered **403** `AccessDenied`.

Neither is 401, though both are about authentication. The console - and every console
bundle that is still open in a browser while a deployment is upgraded - takes a 401 as
"the session is gone" on the status alone, before a body is read, and aborts its
uploads: the very thing this ADR is to prevent. 401 keeps one meaning, "sign in again".
The two are told by their `code`, as `QuotaExceeded` is (ADR-0024).

### Unlock

`POST /api/v1/session/unlock`, body `{"password": "..."}`, with the session cookie and
the CSRF token. It takes no user name: it is the password of the session's own user, so
it is no oracle for any other account.

1. The attempt is counted against the caller's address by the limiter of sign-in, before
   the password is looked at (ADR-0007). Over the limit: 429 `TooManyAttempts`; the
   session is kept.
2. The password is checked through `ports.IdentityProvider` for the session's user: the
   same Argon2id path, the same decoy and the same check of a disabled user as sign-in.
3. Not accepted: an audit record `auth:UnlockSession`, failed, and 403.
4. **The fifth wrong password for one session ends it** (counted per node, in memory, as
   the limiter is): the record is deleted, with an audit record, and the answer is 401
   with the cookie cleared. Nothing is lost by that that "Sign out" would not lose.
   This is the stricter reading of "password guessing"; it is an open question for the
   owner. The use case says "not signed in" - of this, and of a session that had ended
   before - with `domain.ErrCredentialNotFound`, the sentinel both inbound adapters
   already answer as a credential that is no longer valid: a wrong password is
   `ErrAccessDenied`, and the adapter tells the two apart by the sentinel alone.
   **Only this ending clears the cookie** - the use case names `ErrTooManyAttempts`
   beside the other sentinel, and by that the adapter knows it. An unlock that finds its
   session ended already is answered 401 and leaves the cookie alone: it may be the very
   request that unlocked, sent a second time, and the cookie the browser holds by then
   is the new one. The console must send an unlock once; a network may still send it
   twice.
5. Accepted: a new record is written - **a new session token and a new CSRF token**, the
   old `CreatedAt`, `ExpiresAt = now + session-lifetime` - together with an audit record
   `auth:UnlockSession`, and then the old record is deleted. The new one first: an
   orphan new record is unusable (its token was told to nobody), a missing old one would
   end the uploads. The old token is refused from then on: a stolen cookie does not get
   full authority back because its owner unlocked.

It is accepted on an active session too, and rotates alike. That removes the race
between the console's timer, the clocks of two gateways and the boundary of the lock.

The operation is **exempt from `Idempotency-Key`**: the header is accepted and has no
effect, as at sign-in. A stored idempotency record would keep a SHA-256 of a body that
holds a password - which can be guessed against offline - and an answer that holds a
CSRF token, for a day.

For the same header on every other operation: **a 423 is not kept as the answer of an
idempotent request.** It is of the moment. The repeat that comes with the same key once
the session is unlocked is carried out, and is not told "locked" from a record for a
day.

### Bounds and settings

| Setting | Default | Range | Meaning |
|---|---|---|---|
| `session-lifetime` | 30m | 10s to 12h | from sign-in, or from the last unlock, to the timeout |
| `session-upload-grace` | 90s | 5s to 10m | how long a lease lasts |
| `session-lock-max` | 12h | 0 to 24h | the longest a session stays locked, counted from the timeout; 0: a session never locks |

- The grace outlasts the longest pause the console's upload engine makes between two
  requests (15 s) and its poll.
- The cap is counted from the timeout and not from sign-in, so that an unlock at hour 11
  is not followed by a forced end at hour 12.
- A request in flight when a session ends is finished by the server, as before.
- **The three must be the same on every node.** Any gateway may be asked what a session
  is: one with a smaller `session-lock-max`, or with `0`, takes for ended what another
  takes for locked. And the horizon of the sweep is the leader's own. `GET /settings`
  shows a difference between nodes as a mismatch (ADR-0022).

### A change of password

When the password of a user is changed - by the user or by an administrator, through
`IAMService.UpdateUser` - **every session of that user that exists ends at once, on
every gateway**: the active ones and the locked ones, with whatever they are uploading.
Whoever holds one proved a password that is no longer the user's. The upload in flight
is finished by the server, as one is whose session ends in any other way.

The store cannot be asked for the sessions of a user, and nothing new is added to it to
ask. So, as the lock is, the ending is derived:

- the change leaves a **mark**, a record among the sessions written with `session.put`:
  key `password-change:` and the SHA-256 of the user's name, `user`, `created_at` = the
  time of the change, `expires_at` = that time + 48 hours. Like a lease it is the hash
  of no token and signs nobody in;
- a session of the user whose `created_at` is not later than the mark's plus
  **`PasswordChangeMargin` (5 seconds)** has ended. An unlock carries `created_at` over,
  so a session that was unlocked since is still one from before the change;
- a session's `created_at` is the moment **its password was about to be checked**, not
  the moment the check was over: a check that read the hash of the old password began
  before the new one was written, and so before the mark. Taken after the check, a
  sign-in with the old password whose check (Argon2id) straddled the change (its Raft
  commit) would be dated after the mark and survive it - and successful sign-ins are
  not limited, so whoever holds the old password could race a reset. An unlock that
  straddles a change keeps the old `created_at`, and the session it renews is ended by
  the mark as the one before was;
- the mark is written after the password, so that no session begun with the old
  password comes after it. Should the second write fail, the caller is told that the
  password is changed and the sessions are not ended, and **the audit trail says so**
  (`auth:EndSessions`, failed, with the reason and the store's error - never the
  password); the same request again ends them;
- a mark that cannot be read ends the session that asked: the request is refused with
  the store's error, never let through;
- 48 hours is longer than any session there at the change can last, locked included
  (the longest lifetime, the longest lock and a grace a node accepts are 36 hours and
  10 minutes; a test of `cmd/cactusfs` holds the two against each other). After that the
  sweep removes the mark;
- it costs one entry at the change, and one more reading at every request of a session
  (see "Consequences").

**Clocks.** The mark is dated by the clock of the gateway that changed the password, a
session by the clock of the gateway that began it. Both directions, as they are:

- a session that began **before** the change, through a gateway whose clock is ahead
  of the changing gateway's by up to 5 seconds, is dated up to 5 seconds after the mark
  and is ended all the same - that is what the margin is for. **Ahead by more than
  5 seconds, it survives the change** and lives to its timeout (or its lock's end): the
  remedy, as for a member of the previous release, is to disable the user. Gateways
  are to keep their clocks within a second or two (NTP); the lock assumes that already
  (see "The sweep");
- a session that began **after** the change - with the new password - within 5 seconds
  of the mark by the two clocks, through any gateway, or later through a gateway whose
  clock is behind by the difference, counts as one from before and is ended: its user
  is signed in, and the next request answers 401. The user signs in once more. That is
  the price of the margin, and the stricter side of the trade;
- two changes through two gateways with different clocks: the second mark replaces the
  first, and a later mark may carry an earlier time. A session begun between the two
  with the first new password is ended by the second as long as the second gateway's
  clock is not behind by more than the margin.

What it does not do: a member of the previous release does not read the mark, and goes
on serving the session until its timeout, as that release did (ADR-0006). **Disabling a
user does not end the user's sessions:** it refuses them while the user is disabled -
every request of a session looks the user up - and re-enabling the user brings them
back, locked or active as their times say, as before this ADR. Deleting a user removes
them.

### The cookie

The session cookie keeps its attributes (`__Host-`, `HttpOnly`, `Secure`,
`SameSite=Strict`, `Path=/`). Its `Expires` becomes `ExpiresAt + session-lock-max`, the
latest moment the session can be locked: a browser would otherwise drop it at the
timeout and could not unlock. The server stays the authority; the cookie of an ended
session is a string that signs nobody in.

### The console and an unlock on its way

An unlock replaces both tokens; the console has the new ones a moment after the API
refuses the old. So, in `web/src/api/client.ts`:

- while an unlock is on its way nothing else is sent: requests and uploads wait for it,
  and go with the tokens it left. **For 20 seconds at the most** (`UNLOCK_TIMEOUT_MS`):
  then the unlock is given up - its request aborted - what waited is sent, and the lock
  says that the server could not be reached and can be tried again;
- **signing out never waits** for an unlock. Its `DELETE /session` carries the tokens
  the unlock may be about to replace; so the unlock on its way is abandoned: should the
  API carry it out after all, its tokens are not kept, and the session it renewed is
  ended with them by a second `DELETE /session`;
- every request is stamped, as it is sent, with the **generation** of the tokens it
  carries, which an unlock raises when it keeps the new CSRF token. A 401 to a request
  of an older generation - sent before an unlock, answered after it - is not "the
  session has ended": the caller gets its error. An upload of that kind fails as one
  whose connection failed (status 0), and the upload engine makes it again with the
  same `Idempotency-Key` - nothing was stored under it - and the new tokens;
- a 401 answered **while an unlock is still on its way** - the server has rotated the
  tokens, the console has not heard yet - is judged when the unlock is over, by the
  same rule: if the unlock kept new tokens it was a 401 of the old ones, otherwise the
  session has ended. The unlock's own 401 and the sign-out's are judged at once, so
  signing out still never waits. An unlock answered after its 20 seconds is treated as
  abandoned: its tokens are not kept and the session it renewed is ended (Goal M,
  2026-10-15, where "Delete all" asks for the password through this unlock while other
  requests are on their way);
- an `api` call answered a 401 of the old tokens is marked so (`ApiError.staleTokens`);
  the deletions of Goal M send such a `DELETE`, or such a page of the listing, once more
  with the new tokens - it never reached the authorizer - and never a third time: a
  second answer of that kind counts the object as not deleted, or stops the walk, with
  a reason that says the password was checked again meanwhile, not that the session
  ended. An upload whose 401 waited for an unlock that then failed, and an unlock
  answered after its 20 seconds, are told to the console as the end of the session,
  which signs out;
- the sign-in screen says why a session ended as far as the console can tell, which
  the API does not say: "your session timed out" for a 401 to its own question of how
  the session stands (at the timeout, or while locked); "the password was wrong too
  many times" for a 401 to an unlock after four wrong passwords; otherwise only "You
  were signed out".

### The sweep

`SessionService.Sweep`, which the leader runs every ten minutes, asks the store to
remove the records that ended before `now - session-lock-max`: a record past its timeout
may still be the credential of a locked session, and the store cannot be asked which
are - it removes by time alone (`session.expire`).

Left at that, every session there ever was would be kept for `session-lock-max` after
its timeout: with the defaults some twenty-five times as many records as before. So
**the node that began a session removes its record**, with `session.delete`, a tick
after it has ended - at once past the timeout for the sessions that upload nothing,
which are nearly all, and when its lock has ended for one that was locked - and the
lease of that session with it when that has lapsed. A node remembers the sessions it
began (65 536 at the most) in memory.

**Clocks.** A node judges "ended" by its own clock; another gateway may still judge the
session locked or active. The lease written at every tick lasts a grace from its
writing, so by the writer's clock it always has two thirds of a grace left: a node
whose clock is ahead of the writer's by **more than about two thirds of a grace**
(60 seconds by default) sees a live lease as lapsed, takes the locked session for
ended - and, if it began the session, deletes its record, which ends the lock on every
gateway and the console's uploads with it. A node behind by more than a third of a
grace deletes nothing early but answers "active" for a moment after the timeout. Either
way the session gets less, never more, than its bounds; the gateways' clocks are to
agree within seconds.

What then stays until the sweep's horizon: the sessions of a node that was restarted,
or that began more than it remembers; the lease of a session whose record was removed
by sign-out or unlock, until `session-lock-max` after its lapse; the mark of a change
of password, for its 48 hours and the horizon. Each is a record of some two hundred
bytes. The bound is the sign-ins of `session-lifetime + session-lock-max` - what it was
without the removal - and in a deployment whose nodes stay up it is the sessions that
are alive, as before this ADR, and the leases of the last `session-lock-max`.

A leader that could list the sessions would do this for all of them; that needs a
reading the port and its adapter do not have, and is left for a later step.

## The replicated state

No entry, no record, no command and no file of `internal/adapters/raftmeta` changes; the
golden files of schema 5 stay as they are. What is new is how the commands that exist
are used:

| New use | A member of this release | A member of the previous release |
|---|---|---|
| A record in `sessions` under `upload-lease:...` | reads it as a lease | never reads it: its `Authenticate` looks up `sha256(token)` in hex and nothing else, so no request finds it; `iam.user.delete` removes it with the user (by its `user` field); its sweep removes it once it has lapsed |
| A record past `ExpiresAt` that is kept and still used | locked while a lease is live | refuses it: `if !s.clock.Now().Before(stored.ExpiresAt) { return refused }` - 401. When it leads, its sweep (`session.expire` at `now`) deletes the record, and the session is then ended on every node |
| `CreatedAt` carried over an unlock | the key of the lease, and what a change of password is held against | reads `CreatedAt` nowhere |
| A record in `sessions` under `password-change:...` | ends the sessions of the user that are not younger | never reads it, for the same reason as the lease; its sessions go on to their timeout, as they always did there |
| A session record deleted (`session.delete`) by the node that made it, once ended | - | a deletion like that of a sign-out |

So during a rolling upgrade a lock degrades to what a timeout was before - a sign-out -
and never to more authority: there is no reading of the new state by the old code that
admits a request the new code refuses. For the same reason this is not a rise of the
entry schema: the bytes are those of schema 5, and every member applies them alike.

Roll-back: a node that is rolled back ignores leases and marks, and its sweep removes
them and the records that were kept past their timeout.

`TestRollingUpgradeFromSchema3ToSchema4SessionLock` (`test/integration`, with the
rolling harness) holds this with the binaries: three voters of the release before and a
node of this release; a session locked through the one is ended through the others and
gets nothing there; the leader's sweep ends it for all; nothing stops. It passed in the
gate of PLAN L.10. It does **not** cover a change of password in the mixed deployment
(the mark, which the previous release does not read - argued above), nor a leader of
this release with gateways of the previous one. `make test-rolling` takes `ROLLING_TEST`
as the beginning of a test's name, so it runs with the upgrade from schema 3 to 4, as CI
runs it (`make test-rolling ROLLING_FROM_COMMIT=0b7d825
ROLLING_TEST=TestRollingUpgradeFromSchema3ToSchema4`), or by itself with its full name -
its comment says how.

Why not extend `ExpiresAt` while an upload runs: a member of the previous release would
read that as a session with full authority.

## Alternatives considered

| Alternative | Why not |
|---|---|
| A lock in the console only | Cosmetic: the server would go on answering. |
| A `locked_at` field, or a command of its own | Entry schema 6. |
| Asking every gateway over node RPC "have you an upload of this session" | Sound, and larger: a proto, a port, a client, a server and a broadcast at rotation. |
| A heartbeat of the console | A claim a stolen cookie can make as well as the owner's browser. |
| Extending `ExpiresAt` by upload activity | Full authority for as long as something uploads, and read as such by the previous release. |
| A lease keyed by the token | Lost at unlock; and a keeper could bring the lease of a rotated token back. |
| 401 for a locked session or a wrong unlock password | Aborts the uploads in every console that is open (above). |

## Security

- **An unattended console.** At the timeout the server locks the session: every
  operation but storing an object, reading the session, unlocking and signing out
  answers 423 and is audited. Full authority comes back only with the user's password.
- **A stolen cookie that keeps itself alive by uploading**, a slow `PUT` included. It
  gains `s3:PutObject` where the user has it, until `session-lock-max` after the timeout
  at the latest (12 hours by default) and within one grace of its last upload - nothing
  it did not hold while the session was live, and nothing it can read. It loses even
  that when the owner unlocks. `session-lock-max=0` turns locking off.
- **Password guessing at the unlock.** The same Argon2id path and limiter as sign-in,
  every failure audited, five failures end the session. Both counts are per node and in
  memory: a deployment allows as many as it has gateways, and a restart forgets them.
  The limiter is the one of sign-in, by the address of the peer: behind one proxy or
  NAT everybody shares it, and ten wrong passwords of anybody - at sign-in or at an
  unlock - make all of them wait five minutes.
- **A changed password.** It ends the sessions of the user, locked ones too (above).
  Not on a member of the previous release during a rolling upgrade, where a session
  lives to its timeout as it did; there, and whenever a session must end at once for
  any other reason, the operator's remedy is to disable the user, which every release
  honours with the next request, or to sign out from the owner's browser.
- **A stored answer** is not given to a locked session (above).
- **The upload as the one authority of a locked session.** It cannot read, list, delete,
  tag or administer. It can overwrite a key the user may write: recoverable in a
  versioned bucket, not in an unversioned one, refused where Object Lock forbids.
- **The lease.** Whoever can write the replicated state can write a lease - and a
  session, a user and a policy as well. A lease alone admits nobody (above).
- The password is never logged, never stored and never fingerprinted.

## Consequences

- A session is no longer strictly `session-lifetime` long: its record lives up to
  `session-lock-max` longer, with the authority to store objects only.
- One Raft entry per third of the grace per uploading session per gateway, at times
  two. A keeper writes the lease for as long as an upload is in flight on its node,
  also when the session has ended meanwhile: the server finishes that request as
  before, and the lease then admits nobody.
- One entry more per session: its record is deleted by the node that made it. One
  reading more per request of a session: the mark of a change of password - and for a
  session past its timeout one more, its lease. The session, its lease and the mark are
  three reads of the store, each of the kind the session itself always was: the port
  (`ports.SessionStore`) reads one record at a time, and one consistent read of the
  three would need a method of the port and of `raftmeta`, which this ADR does not
  change. The cost is recorded, and left.
- **Locks are lost in an outage of the metadata group** longer than a grace, and the
  uploads of the console with them (above); so they are when the one gateway that
  serves a session's uploads stops.
- **Retained records**: see "The sweep".
- Changing one's own password signs one out, everywhere.
- Three settings, shown by `GET /settings`.
- **OIDC** has no provider yet. The ADR of the first one must say how its users unlock;
  until then a session whose user has no password cannot be unlocked and is signed out
  when its uploads are done, or by "Sign out".
- More than one tab: one works at a time, as before (ADR-0007).
- The lock itself is no audit event - it is derived from time, as expiry is. Refusals of
  a locked session, failed and successful unlocks and a session ended for wrong
  passwords are.
- Clock skew between gateways moves the boundary of the lock by the skew. Harmless:
  unlock is accepted in both states. Beyond about two thirds of a grace it can end a
  lock early ("The sweep"), and beyond five seconds a session from before a change of
  password can survive it ("A change of password").
- A reverse proxy's own timeout may still cut a long upload (`docs/load-balancing.md`).

## Tests

| Claim | Test |
|---|---|
| A locked principal is refused everything but `s3:PutObject`, which is judged as unlocked | `TestALockedSessionMayOnlyStoreObjects` (every action `iam.go` declares), `TestLockedSessionProperty`, `FuzzPolicyDecision` (`internal/domain/iam`) |
| The sentinel is no denial and has an answer in both APIs | `TestEverySentinelIsInOneOfTheTwoLists` (`internal/domain`), `TestEverySentinelHasAnAnswer` (`adminapi`, `s3api`) |
| The authorizer records it; `Allowed` never says "no" without an error; no policy is read | `TestAuthorizerOnALockedSession` (`internal/app`) |
| The three states, the bounds, the sweep | `TestSessionLock` (`internal/app`) |
| A lease alone signs nobody in; its key is the hash of no token | `TestAnUploadLeaseSignsNobodyIn`, `TestUploadLeaseKeyIsNoTokenHash` |
| The keeper's cost, its retries (of a write that failed while an upload was in flight, and of the write after the last one ended) and its end | `TestUploadLeaseKeeper`, `TestSessionKeeperEndsWithItsContext` |
| A pass of the keeper has a deadline | `TestUploadLeaseKeeperPassHasADeadline` |
| An upload in flight has a live lease at every moment, whenever the ticks come; on a real clock a lease is written at every tick | `TestUploadLeaseNeverLapsesUnderAnUpload`, `TestUploadLeaseCadenceOnARealClock` |
| A lease that names another user locks nothing | `TestALeaseOfAnotherUserLocksNothing` |
| A change of password ends every session of the user | `TestAPasswordChangeEndsTheSessions` (`internal/app`), `TestAPasswordChangeEndsEverySession` (`adminapi`), `TestSessionBoundsFitTheHorizonOfAPasswordChange` (`cmd/cactusfs`), `TestSessionLock` "a change of password ends the active and the locked sessions" (`test/integration`) |
| A sign-in or an unlock whose check straddles the change is ended; the margin, both ways; the mark written after the password; a mark that cannot be read refuses; one that cannot be written is audited | `TestAPasswordChangeRaces` (`internal/app`) |
| The node gives the IAM use case the store of sessions, and the idempotency use case the authorizer | `TestANodeWiresWhatEndsAndRefusesSessions` (`cmd/cactusfs`) |
| Ended records are removed by the node that made them, one an unlock renewed included | `TestEndedSessionsAreRemoved` |
| The console: an unlock given up after 20 s; sign-out never held back; a 401 of an older generation; why a session ended | `web/src/api/client.test.ts`, `web/src/lock.test.tsx` |
| A locked session is given no stored answer and reserves no key; an upload is repeated | `TestIdempotencyOfALockedSession` (`internal/app`), `TestALockedSessionOnlyUploads` |
| Unlock: rotation, the limiter, five failures, the audit trail, no password in log or trail | `TestSessionUnlock` |
| Every operation of the contract answers a locked session 423 and leaves a record; uploads; an API key is never locked | `TestALockedSessionOnlyUploads`, `TestALockedSessionWithoutTheRight` (`internal/adapters/adminapi`) |
| The cookie; CSRF; no idempotency record; 403, 429, 401 | `TestUnlockSession`, `TestUnlockAttemptsAreLimited` (`internal/adapters/adminapi`) |
| The settings and their ranges | `TestParseServerConfig`, `TestSettingsDocumentsInSync` (`cmd/cactusfs`) |
| The binary: a timeout of ten seconds and an upload slower than that | `TestSessionLock` (`test/integration`) |
| A member of the previous release | argued above from its code, and `TestRollingUpgradeFromSchema3ToSchema4SessionLock` (with `make test-rolling`, outside `make check`; not a change of password in a mixed deployment, nor a leader of this release with old gateways) |
