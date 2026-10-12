# ADR-0020: Certificate rotation without restart

- **Status:** accepted; implemented (PLAN 7.5)
- **Date:** 2026-10-12
- **Milestone:** 7
- **Builds on:** ADR-0005 (cluster, node identity, mutual TLS), ADR-0015 (the Helm chart)

## Context

Certificates were read once, at start. A certificate that is about to expire meant a
restart of every node, and for the voters a change of leader each time. cert-manager and
Vault issue short-lived certificates and replace the files - in Kubernetes by swapping
the `..data` link of a Secret volume, which the kubelet does up to a minute or more
after the Secret changed.

Two kinds of certificate are in use: the **node certificate** (node RPC and Raft, both
ends of mutual TLS 1.3, ADR-0005), which *is* the node's identity, and the
**client-facing certificate** of the S3 and admin listeners.

## Decision

- **A reloader in `internal/adapters/nodetls`** holds the certificate in use and serves
  it to every handshake through `tls.Config.GetCertificate` (accepting) and
  `GetClientCertificate` (dialling). A handshake that begins after a replacement uses
  the new pair; a connection that is open is not touched, so Raft's streams and gRPC's
  connections go on and are replaced as they would be anyway. The same reloader, with
  other checks, serves the client-facing listeners: `nodetls` already issues the
  development certificate for them.
- **Polling, not file notifications (fsnotify).** Every `--tls-reload-interval`
  (`CACTUSFS_TLS_RELOAD_INTERVAL`, default `1m`, at least `5s`, `0` turns it off) the
  certificate and key files are read and a SHA-256 of their content is compared with
  that of the pair in use. Why polling: it needs no new dependency and behaves the same
  on Linux, macOS and Windows; a swapped link, a rewritten file and a file replaced by
  rename are all the same to it, where a watch on the file misses the swap of a link in
  its parent directory - which is how a Secret volume is updated - and a watch on a
  network file system may say nothing at all; and a rotation that is noticed a minute
  late costs nothing, since certificates are renewed days before they expire.
  Modification times are not trusted.
- **How the files are read.** Both kinds must be **regular files** of at most 1 MiB. The
  file is opened without waiting (`O_NONBLOCK` where there is such a thing), the open
  file is asked what it is, and a pipe, a device or a directory is refused
  (`unreadable`) before a byte is read: a read of a pipe nobody writes to never ends.
  - *The node's pair* is opened within its own directory through `os.Root`, as it was
    before this ADR: a link inside the directory is followed (a Secret volume is made of
    them), one that leaves it is not.
  - *The client-facing pair* is read by a plain bounded read that follows links wherever
    they lead, as `tls.LoadX509KeyPair` did before this ADR: certbot's
    `live/<host>/fullchain.pem` is a link into `../../archive`, and a file under `/etc`
    is often a link to another place. Confining it would have stopped deployments that
    start today. It is the operator's path either way; `os.Root` buys nothing for a
    path the operator names, and for the node's pair it is kept only because it was
    already the rule.
- **A look at the files holds nothing up.** One look happens at a time. The files are
  read and the candidate is checked outside the lock that guards the reloader's state,
  which is held only to compare and to assign; what is asked of the reloader (`NotAfter`,
  `Refused`, `Replaced`, `Pending`) and every handshake are answered while a read is in
  progress. The read is done by a goroutine of its own, so the look ends when its
  context is cancelled even if the read does not - a mount that hangs. That goroutine
  is then left behind, and no further look begins until its read has ended. A node
  that stops waits for the loop at most 2 seconds, then logs at WARN that it did not
  stop and goes on with its shutdown.
- **A candidate is checked before it is used.** It replaces the pair in use only if all
  of this holds; otherwise it is refused, counted by reason once for that content,
  logged, and **the pair in use stays**:
  1. both files can be read, parse, and the key is the certificate's (reasons
     `unreadable`, `pair`; logged at WARN: a pair that is half written is the ordinary
     moment between two writes, and the next poll tries again);
  2. the certificate is valid now, by the node's clock (`not_yet_valid`, `expired`);
  3. *node certificate:* it carries exactly one DNS name and no IP address, and the name
     is **this node's ID** (`identity`) - the check a peer makes of it - and it verifies
     under the node CA **as loaded at start**, for server and for client authentication
     (`untrusted`);
  4. *client-facing certificate:* it may be used to authenticate a server: no extended
     key usage at all, or one that includes server authentication (`usage`).
  Refusals of kinds 2 to 4 are logged at ERROR. The log names the reason, the file of
  the certificate, and the candidate's subject, serial number and expiry; never the key.
  A refusal is logged when the content is first found and **again every hour** by the
  node's clock for as long as that content stays on disk, at the level it had and with
  the time it was first refused: one line, said once, leaves the log with the files
  that are rotated out. A reminder is not counted as a new refusal.
- **What is not checked of the client-facing certificate**, and why: that it chains to
  any CA (the node cannot know which roots its clients trust), and which names it
  carries (it cannot know every name it is reached under). It is not a node certificate
  and need not come from the node CA - it should not.
- **A candidate that expires sooner than the one in use is accepted** when it is valid:
  an operator may shorten a lifetime, or take back a certificate, on purpose. It is
  logged at WARN.
- **Start-up stays permissive, but loud.** The node certificate is checked as before
  and a bad one - expired, of another CA, naming another node - stops the node, as
  before. The client-facing pair must parse and match, as before, and is then held to
  the checks of time and usage that a replacement gets; when it fails one, the node
  **logs it at ERROR with the reason and starts all the same**. Clients reject an
  expired certificate without the node's help, and a node that does not start cannot be
  given a better certificate by rotation - only by a restart. No deployment that starts
  today stops starting, with one exception that is deliberate: a certificate or key
  given as a pipe (a FIFO, a shell's process substitution) started before and does not
  now, for either kind of pair. Such a file can be read once and never again, and
  reading it again is what blocked the loop and the shutdown.
- **The dialling side presents its certificate unconditionally.**
  `GetClientCertificate` returns the certificate in use whatever the other end asked
  for. With a static `Certificates` list, crypto/tls would first compare the list with
  the CAs the server names in its request and send no certificate if none fits. What is
  lost is that silent fallback, and in practice nothing: there is one node CA, the
  listener demands a certificate (`RequireAndVerifyClientCert`), so a handshake without
  one fails just the same, and under TLS 1.3 the server verifies what it is sent. The
  signature algorithm is still negotiated by the library.
- **The CA of node certificates is not reloaded. Trust is fixed until restart.**
  Replacing trust roots under a running `tls.Config` needs the dialling side to turn
  the library's verification off (`InsecureSkipVerify`) and do it again by hand in
  `VerifyConnection` - gRPC keeps one configuration for the life of a client
  connection. That is the pattern in which verification gets lost, for an event that
  happens once in years. A CA is rotated with restarts instead:
  1. give every node a CA file that holds **both** roots, old and new, and restart the
     nodes one at a time;
  2. replace every node certificate by one of the new CA - no restart, this ADR;
  3. give every node a CA file with the new root only and restart them one at a time.
  A candidate certificate of a CA the node did not load at start is refused
  (`untrusted`), so step 2 before step 1 changes nothing and breaks nothing.
- **TLS versions and cipher suites are unchanged**: mutual TLS 1.3 between nodes; 1.2
  and up with ECDHE and AEAD for clients.
- **Metrics are fed through accessors, read when the node is scraped.** The package
  counts and exposes nothing itself; the composition root hands its accessors to the
  telemetry adapter, and neither adapter knows the other. None of them waits for a file.
  - `Reloader.NotAfter` (when the certificate in use expires) is
    `cactusfs_tls_certificate_not_after_seconds{use}` - `use` is `node`, or `s3` for
    the pair clients are shown - and, for the node's,
    `cactusfs_node_certificate_not_after_seconds` as before: both follow a replacement.
  - `Reloader.Pending` (why the files on disk are not in use, if they are not) is
    `cactusfs_tls_reload_pending{use}`, 1 or 0. The alert
    `CactusFSCertificateReloadRefused` fires when it has been 1 for thirty minutes.
  - `Reloader.Refused(reason)` (candidates refused, by reason) is
    `cactusfs_tls_reload_refused_total{use,reason}`, every reason at 0 from the start.
  - `Identity.PeersRefused(reason)` (peers whose handshake was refused where this
    package does the handshake - the Raft listener and Raft's dialling) is added to
    `cactusfs_node_handshakes_refused_total{reason}`, which has four reasons that node
    RPC's handshakes are counted by as well: `no_certificate`, `untrusted` and
    `identity` are its `certificate`; `not_admitted` and `timeout` are themselves;
    `handshake` - a connection closed half-way, a caller that speaks no TLS 1.3 - is its
    `other`. A node the Raft listener does not admit is counted here and nowhere else.
  - `Reloader.Replaced` (candidates put to use) is in no metric: a replacement is
    logged, and shows as the step of the certificate's end.
  A caller without a certificate is told from other failed handshakes by the text of
  the error of `crypto/tls`, which has no type for it; should the text change such a
  caller is counted as `handshake`, and a test fails.

## Consequences

- A renewed certificate is in use at most one interval after its files are complete -
  plus, in Kubernetes, the kubelet's delay. Renew with more margin than both.
- **Rotation is not revocation.** A certificate that was replaced, and its key, are
  accepted by peers until that certificate expires: there is still no revocation list.
  Connections opened with it stay open. For a key that is compromised, rotate the CA
  (above) or remove the node from the cluster.
- A certificate or key file that is a pipe, a device or a link out of the directory (the
  node's pair only) is `unreadable`: the node keeps what it has, and does not start
  with it.
- The key of a node certificate should be replaced with it. The same key under a new
  certificate is accepted.
- A node whose replacement is refused keeps serving with what it has and says so; if
  nobody reads the log or the counter, the old certificate expires and the node drops
  out as it would have without this ADR. The pair on disk is what the next start reads:
  a refused node certificate on disk stops that start.
- gRPC's handshakes for node RPC are verified by the same configuration but are not
  counted in `Identity.PeersRefused`: gRPC does them, and the node RPC server reports
  each to the same metric. No handshake is counted twice.
- The end of the certificate clients are shown is exposed and in no alert: the two
  alerts on a certificate that ends read the node's.
- No format on disk, no Raft entry and no cipher changes: no migration.

## Alternatives

- **File notifications (fsnotify)** - a dependency, per-OS behaviour, and a Secret
  volume swaps a link in the parent directory, which watchers of the file miss.
- **SIGHUP** - not on Windows, and nothing sends it in Kubernetes.
- **Reloading the CA as well** - see above; `GetConfigForClient` would do it for the
  accepting side only.
- **Closing connections on rotation** - would cost Raft its leader for nothing: the old
  certificate is still valid.

## Open question for the owner

- **Should start-up refuse a client-facing certificate that is expired, not yet valid or
  not for server authentication?** Decided here: no - it is logged at ERROR and served
  (see "Start-up stays permissive, but loud"). The stricter reading would stop the node,
  as a bad node certificate does. It is a one-line change in `nodetls.LoadServer`;
  the cost is that a deployment with an expired certificate that restarts today, and
  is repaired by putting a new pair on disk, would instead stay down until someone
  does.
- **Should a certificate or key given as a pipe still start a node** that has reloading
  turned off (`--tls-reload-interval 0`)? Decided here: no, for one rule instead of two.
