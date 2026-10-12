# ADR-0022: Settings - declared once, shown for every node

- **Status:** accepted (2026-10-12)
- **Builds on:** ADR-0005 (node RPC over mutual TLS), ADR-0015 (the chart), ADR-0020
  (certificates that are reloaded)

## Context

A node is configured by flags and by `CACTUSFS_*` variables. What they are was written
by hand in three places - the parser, the README and the architecture document - and
the three had begun to differ. What a node was configured with was known to its own
process and to nobody else: the console could not show it, and an operator who wanted
to know which volume a drive was on, or whether every node used the same key service,
had to read each node's environment. The owner asked for the settings of an `.env` file
to be defined, and for a screen that shows every node's settings and the cluster's.

## Decision

**One registry.** `serverSettings()` in `cmd/cactusfs` declares every setting once: its
name, variable, flag, default, group, kind (`plain`, `path`, `url`, `secret`,
`secret-file`) and scope (`node`, or `cluster` for what every node must agree on). The
parser is one loop over it and records where each value came from: `default`,
`environment`, `flag`, or `derived`. `cactusfs server --help` is held to a golden file.

**Documents are printed, not kept.** `cactusfs config --format env|markdown` prints
`deploy/cactusfs.env.example` and the tables of the README and the architecture
document; a test fails when a committed copy differs. The binary reads no `.env` file:
an engine's `--env-file`, compose's interpolation and systemd's `EnvironmentFile=`
already do that, and a reader of our own would add a third precedence and a file that
invites secrets. `.env`, `.env.*` and `*.env` are ignored by git and kept out of the
build context; the example files are not.

**A live view, asked of each node.** `GET /api/v1/settings` is answered by one node,
which asks every other over node RPC (`NodeService.Settings`) - eight at a time, four
seconds each, a second more than a connection is waited for, so that a node that cannot
be reached is said to be that and not slow. Nothing is stored or replicated: no schema
rises. A node that does not answer is named with a reason - `unreachable`, `timeout`,
`unsupported` (an older release), `failed` - and never with its error's words.

**What is shown.** Of the cluster: the deployment, the level the metadata store writes
at, the leader, the count of nodes and voters, each erasure set with its quorums, and
the settings of scope `cluster` on which nodes disagree. Of each node: its build, its
roles and labels, every setting with its source, each drive with the directory as the
node sees it, the file system behind it (Linux: type, device, mount point - never the
options of the mount), its size and what is free, and the label the deployment gave it
(`CACTUSFS_DRIVE_LABELS`: the compose volume, the claim of the chart - a process cannot
know the name of its volume, so it is told), and the certificates it serves by subject,
names, issuer and validity.

**What is never shown.** The value of a `secret` or a `secret-file` - only whether it is
set; the path of a secret file (the stricter reading); the userinfo, query and fragment
of a URL; a key. Redaction is in the domain (`cluster.NewSetting`) and is done in the
node that owns the value, before anything is sent; the node that receives redacts
again and bounds what it takes. A kind it does not know is treated as a secret. The
collector's URL may carry no credential at all: one that does is refused at start and
printed only without it.

**Who may see it.** `admin:GetSettings`. The built-in `cluster-admin` and `auditor` hold
it by their wildcards, and so does every policy that says `admin:Get*`. A refusal is
audited; a reading is not.

## Consequences

- A new setting is one row of the registry; forgetting to say that it is a secret is
  what the classification test and the registry-driven scan are there to catch.
- The screen costs a call to every node each time it is opened. It is not polled.
- Any holder of a node certificate can read its peers' redacted settings.
- A standalone node says one node and one voter; it has no record of itself.
- File system facts exist on Linux only.

## Alternatives rejected

- **Settings replicated in the node's record.** Stale by design, and a rise of the
  entry schema for what a question answers.
- **The console asking every node.** A question of reachability and of CORS, and a
  second way in.
- **`env_file:` in compose, or a reader in the binary.** See above.

## Open, for the owner

Whether an `auditor` should read directories and addresses; whether the path of a secret
file may be shown; whether the binary should read an environment file for installs
without a container engine.
