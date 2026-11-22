# ADR-0033: One deployment script for every container platform

- **Status:** proposed (2026-11-21, PLAN 10.0). Nothing of it is built yet: PLAN 10.1 to
  10.11 build it, each with its gate, and PLAN 10.12 is to accept it and say what was
  built where it differs from the text below. It changes no Raft entry schema, on-disk
  format, crypto design or contract. The points under "Open, for the owner" are not
  answered; their defaults are what is to be built (the threat model, questions 72 to
  78).
- **Date:** 2026-11-21
- **Builds on:** ADR-0014 (the default console user), ADR-0015 (the Helm chart),
  ADR-0020 (certificate rotation), ADR-0021 (SBOMs and image signatures), ADR-0029 (how
  a binary was packaged, and upgrades from the console), ADR-0031 (the local KMS)

## Context

Albie's goal (2026-11-21): "write me a deployment script and give me the ability to
deploy to all containers platform i.e. docker desktop, docker ce, podman, k3s, k8s,
rancher docker desktop ... then update the documentation, rebuild binaries and commit
code to github."

CactusFS can be run three ways today, each with its own commands:

- the binary, `cactusfs server --data <dir>...`, standalone;
- `deploy/compose/compose.yaml` (`make up`), a four-node development cluster on one
  machine, built from source, its secrets exported by hand and written into files by
  `scripts/compose-secrets.sh`, its certificates and one shared key file made by
  `cactusfs dev-init`;
- the Helm chart (`deploy/helm/cactusfs`), whose Secrets the operator makes beforehand
  with `kubectl create secret`, from their own PKI.

The platforms Albie names differ in more than their commands. Docker Desktop runs the
engine in a virtual machine and shares only some host directories with it. Docker CE is
usually on a Linux server reached over SSH. Podman runs rootless and, on many hosts,
under SELinux. Rancher Desktop runs either moby or containerd (`nerdctl`), and brings a
k3s of its own that sees images only in containerd's `k8s.io` namespace. A remote k3s
server often has neither helm nor docker installed, and its admin kubeconfig should not
leave it. A Kubernetes cluster may be shared: Albie's requirement for the shared k3s
server is that the deployment has its own namespace and modifies nothing outside it.
The operator's machine is arm64; the servers are x86_64.

Making the secrets is the step most likely to go wrong, and the most expensive:

- on the local KMS, every object is opened by two things together: the sealed key file
  `kms.json` and its passphrase. Neither opens anything alone; either one lost, or made
  again over the old, loses every object;
- a secret on a command line (`--from-literal`, `-e`, an argument to ssh) is readable by
  every user of the machine while it runs, and often lands in shell history;
- a secret in a container's environment is shown by `docker inspect` to anyone who may
  talk to the engine;
- `admin` / `admin` (ADR-0014) is a credential of every new deployment, and a script
  that publishes the admin port by default publishes it.

The tools the script drives choose their engine and cluster from the operator's shell:
`DOCKER_HOST`, `DOCKER_CONTEXT`, `CONTAINER_HOST`, `KUBECONFIG` and the current
context. A script that inherits them deploys to - or purges - whatever the shell last
pointed at.

CLAUDE.md bounds the answer: no secret logged or printed, no default credential beyond
ADR-0014, no runtime fetch from the internet, no tool that must be installed on a host,
nothing copied from or referring to another project, and the same code path for
standalone and cluster.

## Decision

### 1. One script, `deploy/deploy.sh`

One POSIX `sh` script (macOS ships bash 3.2, and every script of `scripts/` is sh) with
its parts in `deploy/lib/`. The target is chosen with `--docker <target>`, else
`DEPLOY_TARGET`, else from what runs (labelled containers, or a marked namespace); the
script refuses when that is ambiguous.

| Target | What it deploys onto |
|---|---|
| `dd` | Docker Desktop on macOS, Windows (through WSL 2) or Linux |
| `ce` | Docker CE, local, or on a server over SSH (`CE_HOST=user@server.example.test`) |
| `podman` | Podman, rootless or not |
| `rancher` | Rancher Desktop: its moby or containerd engine, or its bundled k3s |
| `k3s` | k3s, local, over SSH (`K3S_SSH`) or through an import command (`K3S_IMPORT`) |
| `k8s` | any Kubernetes 1.27 or later, through `kubectl` and a context |

Actions: `--install` (run again, it upgrades and keeps the data), `--status`,
`--uninstall` (keeps the data), `--uninstall --purge` (deletes it), with
`--from-source`, `--adopt` and `--engine-replaced` (section 3), `--another-target`
(section 15) and `--dry-run`. `--dry-run` runs nothing that changes state and
prints no secret. A deployment has a name, a DNS label, which names its containers,
volumes, namespace and its entry of `STATE_DIR` (section 3).

### 2. The env file is data, not code

The settings are read from `deploy/.env` (git ignores it, and it is left out of the
build context), whose example is `deploy/.env.example`.

- It is **parsed, never sourced or evaluated**: one `KEY=VALUE` a line, blank lines and
  `#` comments allowed, the value taken literally - `$(...)`, a backtick or a `;` is
  text, and nothing runs. `~/` is expanded only at the start of a value of a key that
  ends in `_DIR` or `_FILE`.
- A key the script does not know, a lower-case key and a line without `=` are refused,
  with the line number. A typo that is ignored deploys something else.
- Precedence: flag, then the file, then the default. The shell's environment is not
  read by the script, so what is deployed is what the file and the command line say;
  the exceptions are `PATH`, `HOME` and `XDG_STATE_HOME` (the default of `STATE_DIR`),
  and the tool variables of section 15, which are cleared, not read. The tools it runs
  would read the shell's environment - compose interpolates `CACTUSFS_*` from it - so
  they are run under `env -i` with only what they need (sections 10 and 15).
- Every key of the example is read by the script and every key the script reads is in
  the example; a test parses both.
- A `CACTUSFS_*` line is passed to the nodes only when `deploy/cactusfs.env.example`
  names that setting (the settings list has one source, `cactusfs config`), and never
  a secret one (`CACTUSFS_ROOT_*`, `CACTUSFS_KMS_PASSPHRASE*`, a Vault token) or one the
  script manages itself (drives, listeners, certificates, the KMS).
- A secret is never a value of the file: it is generated (section 3) or given as a file
  the file names (`ROOT_ACCESS_KEY_FILE`, `ROOT_SECRET_KEY_FILE`, `KMS_PASSPHRASE_FILE`,
  `VAULT_TOKEN_FILE`), so the env file can be read, copied and diffed without
  disclosing anything. `ROOT_ACCESS_KEY`, `ROOT_SECRET_KEY`, `KMS_PASSPHRASE` and
  `VAULT_TOKEN` given as values are refused with the line number, not merely warned
  about.
- `KMS` has no default: a file that does not set it is refused (section 10).
- The confirmation of a purge is never a value of the file: `PURGE_CONFIRM` in it is
  refused with the line number, and the confirmation is given only at a terminal or
  with the flag `--purge-confirm=<name>` (section 3).

### 3. Custody of secrets

**What a deployment begins with.** The root credentials; on the local KMS, the KMS
passphrase **and** the sealed key file `kms.json`, which open the data only together;
on Vault, a token file instead of both; the certificates and their private keys. Each
is a secret of the deployment and is kept as one, `kms.json` included.

**Who makes what.** The root credentials and the KMS passphrase are generated from
`/dev/urandom` when they are not given as files. The certificates and the key file are
made by the product's own `cactusfs dev-init`, the same way on every target: the
script runs it on the operator's machine, in a container of its native architecture,
with the passphrase as a file and its output as a tar stream, and passes it `--hosts`
(the names and addresses clients use for S3, from the env file) and `--validity`. Its
directory - `kms.json`, the certificates and `ready` - is written to `STATE_DIR`
first, and put on the target **before anything there starts**, so no node ever runs,
and stores an object, while the key file it opens exists in one place only:

| Target and mode | `kms.json` is made by | Where it is on the target | How a node finds it |
|---|---|---|---|
| Docker targets, standalone | `dev-init` on the operator's machine, into `STATE_DIR` | two labelled volumes filled with `cp -` before the node is first run: `<name>-config` (dev-init's directory: `kms.json`, the certificates, `ready`) and `<name>-secrets` (the root credentials, and the passphrase or the Vault token) | `CACTUSFS_KMS_KEY_FILE`, a setting the script manages, names the copy in the config volume; the other secrets by their `CACTUSFS_*_FILE` |
| Docker targets, cluster | `dev-init` on the operator's machine, into `STATE_DIR` | compose's `config` volume, filled before `up`, `ready` included, so compose's `init` service finds the directory complete and writes nothing (`dev-init` leaves a complete directory alone) | `CACTUSFS_KMS_KEY_FILE` of `deploy/compose/compose.yaml` |
| Kubernetes, both modes | `dev-init` on the operator's machine, into `STATE_DIR` | a Secret of the namespace, applied before the workload | the chart's `kms.local.keyFileSecret` |

The explicit setting matters on Docker standalone: with no `--kms-key-file`, a
standalone node would make its own key file, `<meta>/kms-local.json`
(`cmd/cactusfs/config.go`, `kmsKeyFileName`), at its first start. `openLocalKMS`
(`cmd/cactusfs/node.go`) opens a key file that exists and makes one only when it is
missing (and refuses when the metadata directory is already in use), so a key file put
in place beforehand is the one used, and none is made on the target.

An operator's own PKI (`TLS_CERT_FILE`, `TLS_KEY_FILE`, `NODE_CA_FILE`,
`NODE_CERT_DIR`) and Vault (`KMS=vault`, `VAULT_*`, `VAULT_TOKEN_FILE`) are used instead
when given.

**Never made again.** A secret present in `STATE_DIR` or on the target is used as it is.
When one copy of a secret is missing - `kms.json` above all - the script puts the other
where it is missing **before any node or `init` starts**, so neither a node (which
makes a key file only when the one it is given is missing) nor `dev-init` (which leaves
a complete directory alone) makes a new one.
When both copies exist and differ, the script refuses and names both; it never chooses.
A secret present on the target with no local state is kept and copied back, and the
script says that the local copy was missing. A put onto the target writes only the
names the target lacks, and only into a copy the same run has read back and compared
(or made): it never writes over what is there, but for a rotation (below). An engine
or cluster that cannot be asked fails the run; an error is never taken for "not there".

**Given as files.** A secret the operator gives as a file must equal every copy there
is when it is one that opens the data or begins the deployment - the root credentials,
the passphrase, `kms.json` - and one that differs is refused: a new one would open
nothing, and the script never replaces them. A Vault token or a certificate, key or CA
of the operator's own (`VAULT_TOKEN_FILE`, `TLS_CERT_FILE`, `TLS_KEY_FILE`,
`TLS_CA_FILE`) that differs is a **rotation**: no data depends on the old one, so the
file given replaces both copies - the target's first, then the local one, so that a
run cut short in between leaves the target on the new file and the same files finish
it - and the run says what it rotated. A local copy that differs from the target's
otherwise is refused as two copies that differ. A certificate rotated while a CA is kept
(`s3-ca.pem`, dev-init's or an earlier one) needs its own CA in `TLS_CA_FILE`, or the
run is refused: the CA of the certificate it replaces is never kept for clients.

**The record.** The entry's file `target` records the engine or cluster as the
settings name it (section 15), its own ID - the engine's (`docker info`; Podman's host
and store root; the server's for `CE_HOST`), or the cluster's `kube-system` UID - and
the KMS the deployment was made with. Every install compares them. Another ID behind
the same settings is always refused - the secrets of one deployment are never given to
a second engine, so two live deployments never share them - with one exception said by
the flag `--engine-replaced` (with `--install` only, never a setting): the recorded
engine is gone, as after a factory reset of Docker Desktop, and this one takes its
place; the run says that the old engine's deployment is treated as gone and that its
secrets now belong to this engine, and the record is written again with the new ID.
Another KMS is refused. The ID is kept in the record, not in the entry's path, so no
entry moves.

**Adopting.** Labelled copies on the target of which `STATE_DIR` holds no record - the
deployment was made from another machine or another `STATE_DIR`, or the record was
lost - are taken into `STATE_DIR` only when the operator says `--adopt` (a flag, never
a setting); otherwise the run is refused before anything is written.

**Two copies.** A local copy in `STATE_DIR`, and the copy in use on the target: Secrets
in the deployment's namespace, or labelled volumes on a Docker target (the compose
cluster's exception is in section 10). The nodes read every secret from a file
(`CACTUSFS_*_FILE`, `CACTUSFS_KMS_KEY_FILE`), never from their environment.

`STATE_DIR` defaults to `${XDG_STATE_HOME:-~/.local/state}/cactusfs-deploy` and is laid
out as `STATE_DIR/<target-id>/<name>/`: one entry for each deployment on each target.
The target ID in the path is the target and the context it is reached through
(`dd-default`, `podman-default`, `k8s-<context>`), and the ssh host for a server
reached through `CE_HOST` or `K3S_SSH`; a byte that is not a letter, digit, `.`, `_` or
`-` is written as `_`. The engine's own ID, or the cluster's `kube-system` UID, is not
part of the path: it is kept in the entry's record and compared (above), so an entry
never moves when an engine is replaced. Directories are of mode 0700 and files of
0600. A `STATE_DIR` inside the repository is refused; one inside a folder a known sync
client keeps (iCloud Drive, Dropbox, OneDrive, Google Drive) is warned about, since a
synced copy is a copy the operator does not control. The README (PLAN 10.12) says how
to back the directory up encrypted. Besides the secrets, an entry holds the record of
the resolved target (section 15), the namespace marker's UID (section 7) and the pinned
ssh host key (section 16). An entry is laid out as follows (PLAN 10.3 writes the
secrets and the record, 10.9 the marker):

| Path in the entry | What it holds |
|---|---|
| `secrets/root-access-key`, `secrets/root-secret-key` | the root credentials, when generated |
| `secrets/kms-passphrase` | the KMS passphrase, when generated (`KMS=local`) |
| `secrets/vault-token` | a copy of `VAULT_TOKEN_FILE` (`KMS=vault`) |
| `config/` | the directory of `cactusfs dev-init`, in its own layout: `kms.json`, `s3.pem`, `s3.key`, `s3-ca.pem`, `node-ca.pem`, `nodes/<node ID>.{pem,key}`, `ready` |
| `target` | the record of the resolved engine or cluster (section 15) |
| `namespace-uid` | the namespace marker (section 7; Kubernetes) |

A secret the env file names by its `*_FILE` setting is read from that file instead.

**On Kubernetes the cluster's copy is never overwritten from here.** Before anything is
written, every Secret of the deployment is read back by its name (a read that fails,
other than not-found, stops the run) and each value compared with the local copy, file
to file (decoded into a 0700 directory of the entry, compared with `cmp`; never a
variable or an argument). Equal: nothing is done. Different: refused, naming both -
except a certificate or key given by `TLS_CERT_FILE`/`TLS_KEY_FILE`, or a token or CA
given by `VAULT_TOKEN_FILE`/`VAULT_CA_FILE`, which is a rotation: the cluster's copy is
replaced by the file (`kubectl replace` of that Secret alone, at the `resourceVersion` read back, so a change made meanwhile fails it; never forced), with a message. In the
cluster only: copied back into the entry with a warning. Missing in the cluster: the
Secret is created (`create`, never `apply`, with the field manager `cactusfs-deploy`).
Missing in both: PLAN 10.3 makes it. A Secret of the deployment's name without the
label `io.cactusfs.deploy.name=<name>` is somebody else's, and refused. The root
credentials, the passphrase, `kms.json`, the node CA and the node certificates and keys
are never rotated by the script.

**Never on a command line, never in a variable, never printed.** Secrets travel on
standard input only (`cp -`, `kubectl apply -f -`, a tar stream through ssh), streamed
from their file by a redirection, so no argv of any process, local or remote, holds
one. The script never assigns a secret to a shell variable, so neither `sh -x`, nor a
dump of the variables, nor an error message can show one. They are not in the output
of `--dry-run`, `--status` or an error. The harness of PLAN 10.3 searches every
recorded argv, all output, and the standard error of a run under `sh -x`, for every
generated value.

**Purge deletes them by design, last.** `--uninstall --purge` deletes the deployment's
data and, with it, both copies of its secrets, in this order: the data (volumes, or
claims); a check that the data is gone (no volume or claim with the deployment's label
is left); the target's secrets - the labelled volumes or Secrets, and for a cluster on
`CE_HOST` the server's `CE_STATE_DIR/<target-id>/<name>/compose/secrets/` (section
10); on Kubernetes the namespace (section 7); and last the deployment's entry
`STATE_DIR/<target-id>/<name>/`, and nothing else of `STATE_DIR`. A step that fails
stops the purge, and the local copy is kept while anything it opens may remain. Two
things the script leaves, names in its output, and does not count as a failure: with
`K8S_STORAGE=hostpath` the host directories (it never deletes a host directory), and
on a StorageClass with `reclaimPolicy: Retain` the PersistentVolumes left `Released`
by the deleted claims (it never deletes a cluster-scoped object). The purge goes on to
the keys; what is left in those directories and volumes is ciphertext that nothing
opens once the keys are purged, and removing it is the cluster's administrator's. A
purge must be confirmed by typing the deployment's name at a terminal, or with
`--purge-confirm=<name>`; never from the env file.

On a Docker engine, standalone (PLAN 10.5), a purge is refused unless the engine's ID
is the one recorded for the deployment (an uninstall too); a deployment of which
`STATE_DIR` holds no record is purged only with `--adopt`, which asks the engine's ID,
and writes the record naming it once the container, the node an upgrade kept and the
labelled volumes are found and checked, before the first is removed (so a purge cut
short leaves it); an engine that holds nothing labelled with the name refuses it (the
settings may reach another engine), and before anything is deleted a volume carrying the deployment's name with another target's
label refuses it. Then: the node's container is stopped and removed; every volume
labelled with the deployment's name but the two of its secrets is deleted; a listing
by that label proves none of them is left; `<name>-secrets` and `<name>-config` are
deleted; a second listing proves no volume of the deployment is left; and last
`STATE_DIR/<target-id>/<name>/` is removed, with its parent only when that is left
empty. A listing or an inspect that fails, or a volume of another target that
appears in one, ends the run there with the entry kept (exit 1: something was deleted
by then): nothing is taken for "no
volume left" that the engine did not say (the review of 10.5 found a failed listing
read as an empty one inside a command substitution; every function that can refuse
or fail is now called plainly and answers in variables). Volumes are found by the
label only, so an unlabelled volume of the same name is never touched.

`dev-init` keeps no CA private key, so a development deployment's certificates cannot
be renewed or reissued by the script: they are valid for `--validity` (a year by
default), `--status` shows their expiry, and the names and addresses of the S3
certificate are fixed when it is made - a client address added later needs a new
deployment or the operator's own PKI.

### 4. Docker targets: no bind mounts

No container the script runs mounts a host path, with the one exception of cluster
mode (section 10). Secrets and certificates are put into labelled named volumes with
`cp -` (a tar stream on standard input); helm and `dev-init` take their input and give
their output as tar streams. Every container and volume carries the label
`io.cactusfs.deploy.name=<name>` (and `io.cactusfs.deploy.target=<target>`, by which a
run without `--docker` finds what is deployed), and the script touches nothing without
it: an
unlabelled container or volume of the same name is refused, not taken over.

The same commands then work with a remote daemon (`DOCKER_HOST=ssh://`), rootless
Podman, `nerdctl`, SELinux (nothing to relabel) and Docker Desktop's file sharing
(nothing to share). A node runs with `--read-only --cap-drop ALL --security-opt
no-new-privileges` and a restart policy that survives a reboot (`unless-stopped` on Docker,
`always` on Podman, below); S3 is published on `BIND`, the admin port
on `ADMIN_BIND`, which is the loopback address by default; a port already in use is
refused before anything changes.

As built for a standalone deployment on Docker Desktop, Docker CE on the operator's
machine and Podman (PLAN 10.5, `deploy/lib/docker.sh`): the node is one container named
`<name>`, run from the image as `65532:65532` with `--pull never` and a stop timeout of
40 s, with the labelled volumes `<name>-data1` to `<name>-data<DRIVES>` and
`<name>-meta` mounted on the image's own directories (owned by 65532, so a new volume
takes that owner), and `<name>-secrets` and `<name>-config` mounted read-only at
`/var/lib/cactusfs/secrets` and `/var/lib/cactusfs/config`. Its environment names
files only: `CACTUSFS_KMS_KEY_FILE` the config volume's `kms.json`, the passphrase or
the Vault token and the root credentials by their `CACTUSFS_*_FILE`, the certificate
and key by `CACTUSFS_TLS_CERT`/`_KEY`; the console's upgrade is off
(`CACTUSFS_UPGRADE_MODE=off`: the script upgrades a container). Before anything
changes, an install asks: a container or volume of the deployment's names without its
two labels (name and target) is refused; once the node has run (its metadata volume
is there) a number of drives other than `DRIVES` is refused, as is a drive beyond it;
data volumes with no record in `STATE_DIR` need `--adopt`, and data volumes with
neither copy of the key file are refused; a port is refused when another container
of the engine publishes it, or when a connection to it on the bind address (the
loopback address for `0.0.0.0` and `::`) is answered (curl's `%{num_connects}`,
never through a proxy) - the node's own ports excepted on an upgrade, read from `port
<name>` (one line a port), since `ps` groups consecutive ports into a range
(`127.0.0.1:9000-9001->9000-9001/tcp`), which is read as a range for the other
containers. The node listens inside on the port numbers it is published on
(`CACTUSFS_S3_ADDR=:<S3_PORT>`, `CACTUSFS_ADMIN_ADDR=:<ADMIN_PORT>`), so each port has one
number wherever it is read; the image's `EXPOSE` is metadata only and it has no
`HEALTHCHECK`. What remains: a
process listening on another address of the machine alone is not seen, and the
engine's own refusal of the port then fails the run after the secrets are in place.
`VAULT_CA_FILE` is refused on these targets for now: the volumes do not carry it.
The image comes from one function, `docker_image_ref`, called in the script's own
shell, never in a command substitution: when PLAN 10.4's `image_resolve` is there it
runs, then `image_deliver`, and the image is what they leave in `RESOLVED_IMAGE`;
without them `IMAGE_SOURCE=image` takes `IMAGE` as given, and another source stops as
not built yet. Readiness is `/readyz` of the admin port, asked over TLS with the CA of
`STATE_DIR` (by the name `localhost`, which dev-init's certificate holds, or the first
DNS name of `TLS_HOSTS`), every 2 s until `WAIT_TIMEOUT` seconds have passed (by the
clock, `date +%s`). Every install ends by saying that a new deployment has the
console user `admin` with the password `admin` (ADR-0014) and that it must be changed
or deleted before the admin port is reachable from beyond the machine, and warns
when `ADMIN_BIND` is not a loopback address. A reboot: Docker starts a container of the
policy `unless-stopped` again unless it was stopped, and the node has it. Podman starts
containers after a reboot only through its `podman-restart` service, which runs
`podman start --all --filter restart-policy=always` (its unit, Podman 5.4): a
container of the policy `unless-stopped` is not among them, so on Podman the node runs
with `--restart always`, and the install says how to enable the service
(`systemctl --user enable podman-restart.service`, and `loginctl enable-linger` for a
rootless user). During an upgrade the old node, stopped and renamed `<name>-previous`,
would be started by that service at a reboot and hold the ports: its policy is set to
`no` with `podman update --restart no` (an option of Podman 5.1 and later, in its
release notes). An older Podman refuses the option: the run warns and goes on - the old
node is removed as soon as the new one is ready, and when the new one is not, the run
says that a reboot would start the old one. `--status` says the node's
state, image and version, the volumes, where it is reached, the files of the CA and
root credentials, the S3 certificate's `notAfter` (read from `STATE_DIR` with openssl
when it is installed, else said to need it), and `/readyz`.

### 5. The image: binaries from `make image-binaries`

An image the script builds from a folder of binaries uses the binaries exported from
the image build itself (`make image-binaries`, PLAN 10.2): `linux-amd64` and
`linux-arm64`, with `SHA256SUMS` and `BUILDINFO` (version, build, commit,
`packaging=image`). The script checks both and refuses a binary whose packaging is not
`image`. A binary of `make build-all` reports packaging `binary`, and inside a container
it would offer to replace itself (ADR-0029; `cmd/cactusfs/updates.go`, `config.go`); the
guard `TestOnlyTheImageIsBuiltAsAnImage` stays as it is.

`--from-source` builds with the `Dockerfile`; `IMAGE_SOURCE=image` deploys a given
reference by its digest. The image is built for the target's architecture, read from
the target (the engine's `info`, or the nodes' `kubernetes.io/arch`), without emulation
- the Go stage cross-compiles and the final stage has no `RUN` - optionally on a
`BUILD_HOST`. Its tag is `<version>-<build>-<12 hex of the image ID>`, so a new content
is a new tag and `IfNotPresent` rolls. Before it is shipped, its revision label (and
its `version` when it can run natively) is checked against what was meant, and a tree
with uncommitted changes is refused for a remote target unless `ALLOW_DIRTY=yes`.
Images reach a target without a registry: `save` and `load` over ssh, `k3s ctr -n
k8s.io images import -` over `K3S_SSH` or through `K3S_IMPORT`; or pushed to
`K8S_REGISTRY` when one is given. An import reaches one node's containerd only, so on
a cluster of more than one node (counted from the Nodes) the script refuses to deliver
by import and requires `K8S_REGISTRY`, and says why; it does not import on every node
or pin the pods to one. What of this reaches the network is in section 12.

### 6. Kubernetes: render and apply, no Helm release

The chart is rendered with `helm template` in the pinned `HELM_IMAGE` - the reference,
with its digest, is read from the `Dockerfile`, which already pins it for `make
helm-lint` - with the chart and the generated values on standard input. An operator
without that image, or offline, names a mirrored or preloaded image with
`HELM_IMAGE` (a reference with a digest) or a helm binary of their own with `HELM`
(a path); section 12. The render is applied with `KUBECTL apply --server-side
--field-manager=cactusfs-deploy -f -` and pruned by the instance label within the
namespace, among the chart's kinds only. `KUBECTL` is `kubectl --context K8S_CONTEXT`
(never the current context, section 15), `k3s kubectl` on a local k3s, or `ssh K3S_SSH
k3s kubectl`, so the cluster's admin kubeconfig stays on its server and no tool is
installed on any host.

There is **no Helm release**: no `helm history` or `helm rollback` for a deployment the
script made. A namespace that holds a Helm release of the same name is refused, so the
script and Helm never manage the same objects. The chart has no `lookup` and no hooks,
so its render is exact; a change of the chart that adds either must revisit this
decision.

### 7. Namespace isolation on shared clusters

The script writes only namespaced objects, in its own namespace. It creates the
namespace with a create (never an apply) whose object already carries the marker
annotation - a random UID, also written into the deployment's `STATE_DIR` entry - and
the label `pod-security.kubernetes.io/enforce=restricted`, so there is no moment at
which a namespace of that name is ours but unmarked. A namespace that exists already
is not ours: it is refused unless its marker equals the UID in `STATE_DIR`. (One whose
marker is there but whose local record is lost is used only after the operator confirms
it at a terminal, and the UID is recorded again; it is never purged without the record.)
It is removed only when it carries the marker, is found empty (nothing but what
Kubernetes puts in every namespace) when checked again just before, and then by a
delete whose `preconditions` name the namespace's `uid` and the `resourceVersion` it
was found empty at, sent as a `DeleteOptions` body with `KUBECTL delete --raw
/api/v1/namespaces/<ns> -f -`. Those preconditions guard the Namespace object itself -
that it is the one marked, with the marker and labels as read - not what it contains.
What remains: an object created in the namespace between the check and the deletion,
by somebody with rights in it, is deleted with it.

Without `hostpath`, the only cluster-scoped objects that come of it are the
PersistentVolumes the cluster's provisioner makes for its claims. What it reads outside
the namespace, it only reads:

- the Services of every namespace (list), so that a NodePort collision is refused
  before anything is applied;
- the Nodes (list): their `kubernetes.io/arch`, their number (the spread of a
  one-node cluster) and their addresses for `--status`;
- the StorageClasses (list), to find the default class or check `K8S_STORAGE_CLASS`;
- the `kube-system` namespace (get), whose UID is the target's ID.

Helm's release records are Secrets inside the namespace (`owner=helm`), and are read
there. The identity it acts as is what `KUBECTL` gives it: through `ssh K3S_SSH k3s
kubectl` that is cluster-admin, which can do far more than the script does - the
boundary is the script's code, not RBAC. An operator who wants RBAC to be the boundary
gives `K8S_CONTEXT` a context bound to a Role in the namespace, with a ClusterRole that
only lists Services, Nodes, StorageClasses and PersistentVolumes and gets the
namespace itself and `kube-system` by name; the namespace is then made beforehand by the cluster's
administrator from the manifests `deploy.sh --print-namespace` writes - the Namespace,
marker included, the Role and the ClusterRole - and bound to the operator's identity
with the two commands it prints on standard error (`kubectl create rolebinding
cactusfs-deploy -n <ns> --role=cactusfs-deploy --user=<identity>` and `kubectl create
clusterrolebinding cactusfs-deploy-<ns> --clusterrole=cactusfs-deploy-<ns>
--user=<identity>`). The rules, exactly:

| Object | apiGroups | resources | resourceNames | verbs |
|---|---|---|---|---|
| Role `cactusfs-deploy` in `<ns>` | `""` | `secrets`, `services`, `persistentvolumeclaims`, `serviceaccounts` | | `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` |
| | `""` | `pods` | | `get`, `list`, `watch`, `delete` |
| | `""` | `pods/portforward` | | `create` |
| | `apps` | `statefulsets` | | `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` |
| | `networking.k8s.io` | `networkpolicies` | | the same |
| | `policy` | `poddisruptionbudgets` | | the same |
| ClusterRole `cactusfs-deploy-<ns>` | `""` | `nodes`, `services`, `persistentvolumes` | | `list` (PersistentVolumes for the purge's check that none bound to the claims is left, PLAN 10.11) |
| | `storage.k8s.io` | `storageclasses` | | `list` |
| | `""` | `namespaces` | `<ns>`, `kube-system` | `get` |

A read the context may not make fails, naming the permission it lacks (Forbidden).
Such a context cannot delete its namespace: a purge then leaves the empty namespace to
the administrator (PLAN 10.11).

`K8S_STORAGE=hostpath` (the chart's `localVolumes`: a StorageClass and
PersistentVolumes; and the directories they name, made on the host with their SELinux
label) is **opt-in only**, announced as cluster-scoped before it is applied, and not
for shared servers. The default is `K8S_STORAGE=pvc` on the default class or
`K8S_STORAGE_CLASS`.

### 8. Reaching a deployment

- **Kubernetes.** The S3 Service is a NodePort by default. With NodePort or
  LoadBalancer, `K8S_S3_ALLOW_FROM` (address ranges, or `any`) is required and becomes
  the NetworkPolicy's `clientsFrom`; an empty one is refused rather than opening S3 to
  nobody without saying why. Such a Service is rendered with
  `externalTrafficPolicy: Local`: with `Cluster`, the default, a node forwards with
  source NAT and the pods see a node's address, so an allow-list of client ranges would
  admit whatever reaches any node (`docs/load-balancing.md`). The chart has no value for
  it today; PLAN 10.9 adds `service.externalTrafficPolicy` (and the admin Service's),
  unset by default so that existing renders do not change. What remains: with `Local`
  only nodes that run a ready pod take traffic, and k3s's ServiceLB may still show the
  pods the address of its own `svclb` pods, depending on its version - the operator
  tests which before narrowing the list; `--status` lists a NodePort only on the
  addresses of nodes that run a ready pod. The admin Service stays ClusterIP and closed
  unless both `K8S_ADMIN_SERVICE_TYPE` and `K8S_ADMIN_ALLOW_FROM` are set. `--status`
  prints the `kubectl port-forward` command for the admin port; with `K3S_SSH` it
  prints the pair that reaches it from the operator's machine: `ssh -L` to the
  server's loopback, and `k3s kubectl port-forward` on the server.
- **Docker.** S3 on `BIND`; the admin port on the loopback address. For `CE_HOST`,
  `--status` prints the `ssh -L` command that reaches it.

So `admin` / `admin` is never published by a default the script chose.

### 9. `rancher` is a target of its own

Rancher Desktop differs from Docker Desktop in its CLI, in how its Kubernetes shares
images, and in its context name; a user should not have to know that. `--docker
rancher` runs over the code of `dd` and `k8s`: `RANCHER_ENGINE=auto` picks moby (the
`rancher-desktop` Docker context) or containerd, where every engine command is
`nerdctl` and images meant for its k3s are built with `--namespace k8s.io`;
`RANCHER_KUBERNETES=yes` hands over to the Kubernetes path with the local image and the
context `rancher-desktop`.

### 10. Standalone by default, and the KMS opted into

Standalone - one node, 4 drives, 2 data + 2 parity - is the default mode on every
target. `DEPLOY_MODE=cluster` gives 3 voters and one more node that stores; on Docker
targets through `deploy/compose/compose.yaml` itself, with a generated override (the
image, the labels, and the `init` service's `--hosts` and `--validity`), so there is no
second compose file to keep in step. An override can only add to or replace what it
names: it cannot remove `build:`, and ports are additive. So compose is run with `up
--no-build`, and cluster mode publishes S3 as `compose.yaml` does - on every address
of the engine's host, at the ports `CACTUSFS_PORT_BASE_1` to `_4` choose - with the
admin ports on the loopback address; a `BIND` other than the default is refused in
cluster mode rather than silently not applied.

**Compose reads the operator's shell.** `compose.yaml` interpolates `CACTUSFS_*`
variables (the image, the build, the port bases and the tunable settings) from the
environment of whoever runs compose, and from a `.env` beside it. The script therefore
runs every compose command under `env -i`, keeping only `PATH`, `HOME` and the one
engine variable the target needs (section 15), with a generated `--env-file` in the
deployment's entry that holds every value compose interpolates; a `CACTUSFS_*`
variable of the operator's shell reaches no node.

**Cluster mode on Docker is the exception to section 4, stated as it is.** The compose
file hands the nodes their secrets as compose file secrets, and those are bind mounts
of host files. An override cannot take them away cleanly (removing a key needs
`!reset`, which not every compose implementation reads), and replacing them would make
a second compose file in all but name. So:

- The files are a third copy, `STATE_DIR/<target-id>/<name>/compose/secrets/`, written
  by the script from the copies of section 3: the directory 0700, the files **0444**,
  because the nodes run as user 65532 and a bind-mounted file keeps its host owner and
  mode. They are readable through the mount alone; on the host the 0700 directory keeps
  them the operator's (and root's). This is the 0444 of `scripts/compose-secrets.sh`,
  and the only files of `STATE_DIR` that are not 0600.
- Under SELinux (Podman on Fedora or the RHEL family) a bind-mounted file needs a
  container label; the script relabels that directory with `chcon -R -t
  container_file_t` when SELinux is enforcing, as the files' owner.
- The engine must see those paths, so a remote engine is reached only through
  `CE_HOST`, with compose run on the server over ssh, in `CE_STATE_DIR` there (the
  same layout and modes, the files sent as a tar stream on standard input); a
  `DOCKER_HOST` engine elsewhere is refused for cluster mode. The compose file and the
  override are copied into the entry and compose runs from it, so no `.env` of the
  repository is read.
- The `config` volume is filled from `STATE_DIR` - the output of `dev-init` on the
  operator's machine, `kms.json` and `ready` included - before `up`, so compose's
  `init` service finds the directory complete and writes nothing (section 3), and the
  nodes, which start as soon as `init` completes, open the key that `STATE_DIR` already
  holds.

There is no default KMS. `KMS=local` (a sealed key file) is written in the example and
must be stated; `KMS=vault` uses Vault Transit; a file without `KMS` is refused. A
cluster on one shared local key file is refused without `ALLOW_SHARED_LOCAL_KMS=yes`:
it is the development arrangement of the compose file and the chart's `dev.localKMS`,
and CLAUDE.md asks Vault of a cluster in production.

### 11. Upgrades one node at a time

`--install` over a running deployment replaces one node at a time - the nodes that do
not vote, then the voters, the leader last when the admin API names it - and waits for
each to be Ready on the new image before the next; it stops at the first that is not,
within `WAIT_TIMEOUT`, and exits non-zero. A standalone node on Docker is stopped and kept, renamed
`<name>-previous`, while the new one is run with the same volumes; the old one is removed
only once the new one is ready, and when it is not the run says that the node is down and
how to go back to the old one (rename it back and start it, or install its image again).
A `<name>-previous` found by an install is an upgrade that did not finish, and is refused
with both ways out. This is the chart's `OnDelete` order (ADR-0015; PLAN,
Decisions, the review of the chart of 2026-10-11) and the roll-out rule recorded in
PLAN's Decisions on 2026-11-21 from the roll-outs to the owner's k3s host (an image
built from a fresh worktree, its `version` read before it is imported, one pod at a
time, stopping at the first that is not Ready), and relies on a release running
alongside the next.

On Kubernetes an upgrade from the console (ADR-0029) holds the annotation
`cactusfs.io/upgrade` on the StatefulSet of the first node by name. The script's values
never enable `upgrade.fromConsole`, but an operator may have; so the script reads that
annotation before it moves anything, and again before each node, and refuses to start,
or stops, while a lock with a fresh heartbeat (younger than twice a node's time, as
ADR-0029 judges staleness) is present. It only reads the lock: it never writes, takes
over or removes it, since the lock's record, its take-over and its settling belong to
the nodes (a lock written by anything else would be taken over as one that does not
parse, or settled and audited as a run nobody started). The server-side apply never
uses `--force-conflicts`, so a StatefulSet whose image a console upgrade patched is a
conflict the script reports and stops at, not a field it silently takes back. What
remains: a console run started between two of the script's checks is not excluded;
the conflict, or the next check, stops the script. Once a console upgrade has finished,
the image field stays owned by the nodes' field manager, so every later `--install`
meets that conflict; `--take-image` (a flag, never a setting) takes the field back: it
is refused while a lock is held, asks for the deployment's name as `--purge` does,
applies `--force-conflicts` to the workloads' image fields only, and says so in its
output. Without it the deployment stays with console upgrades.

### 12. Every network fetch, and `OFFLINE`

Nothing at run time of a node reaches the internet. The script itself fetches only
what is listed here, and each fetch is one an operator can avoid:

| Fetch | When | From | Avoided by |
|---|---|---|---|
| `HELM_IMAGE` (`alpine/helm`, pinned by digest in the `Dockerfile`) | every Kubernetes install, upgrade and uninstall, unless present | Docker Hub | a preloaded or mirrored image named with `HELM_IMAGE` (with a digest), or `HELM=<path>` to a helm binary |
| `RUNTIME_IMAGE` (distroless, by digest) | `IMAGE_SOURCE=binaries`, the base of the `from-binaries` stage, unless present | its registry | preloading it |
| `RUNTIME_IMAGE` (distroless, by digest) | every install of a standalone deployment on a Docker engine - this machine's, and the server's for `CE_HOST` - unless present: the never-started helper containers that read and fill the secrets' volumes, and dev-init (`IMAGE_SOURCE=binaries`), are made from it. With `IMAGE_SOURCE=image` they are made from `IMAGE` instead (the same base and user), which the target needs anyway, so nothing more is pulled | its registry | preloading it on both engines; `OFFLINE=yes` refuses the pull |
| The base images (Go, Node, distroless), the Go modules and the npm packages | `--from-source` | their registries, the Go module proxy, the npm registry | not using `--from-source` |
| The image named | `IMAGE_SOURCE=image`, unless present | its registry | preloading it |
| Docker CE and its repository key | `CE_INSTALL_DOCKER=yes`, on the server | Docker's own package repository | installing the engine otherwise |
| Pushing to `K8S_REGISTRY` | when it is given | the operator's registry | not giving it |

`OFFLINE=yes` refuses every pull and fetch from a public source: every image, base
images included, must be present (checked with `image inspect` first), builds run
with `docker build --pull=false` or `podman build --pull=never` (the flags differ by
engine), and `--from-source`, `IMAGE_SOURCE=image` for an image not present and
`CE_INSTALL_DOCKER` are refused. A push to `K8S_REGISTRY` stays allowed: it is the
operator's own registry, often the way into an air-gapped cluster. An air-gapped site
sets it.

`CE_INSTALL_DOCKER=yes` installs Docker CE from Docker's own repository on Debian,
Ubuntu and the RHEL family, and refuses an unknown distribution, a host that runs k3s
or Kubernetes (Docker sets the iptables `FORWARD` policy to `DROP`, which breaks pod
networking) and one with `podman-docker`, unless `CE_INSTALL_DOCKER_FORCE=yes`.

### 13. Windows through WSL 2

On Windows the script runs in WSL 2 with Docker Desktop's WSL integration. There is no
PowerShell entry point, and Git Bash is not supported.

### 14. How it is tested

`make deploy-check` (in `make check`, in the builder) runs `go tool shfmt -ln=posix`,
`dash -n` and `bash -n` on every file of `deploy/`, a standard-library Go harness in
`test/deploy` (tag `deployscript`) that runs the real script against recording fakes of
`docker`, `podman`, `nerdctl`, `kubectl` and `ssh`, and kubeconform on the render of
every profile. `make test-deploy` runs the lifecycle against a real engine (CI matrix of
docker and podman), and the k3s lifecycle runs in `make test-k8s`. The lifecycle
(`test/deploy/lifecycle.sh`, PLAN 10.5) also proves on the engine what the fakes
cannot: that every file of `<name>-secrets` and `<name>-config`, and their roots, as
PLAN 10.3's never-started helper fills them with `cp -a`, is readable by the node's
user 65532 (the owner and mode of each entry as the engine reads them out), and that
`kms.json` of `STATE_DIR` is the one the node uses. `shfmt` is a new build
tool (BSD-3-Clause), pinned with a `tool` directive; ShellCheck is GPL-3.0 and outside
the allow-list.

### 15. Which engine and which cluster

The script clears `DOCKER_HOST`, `DOCKER_CONTEXT`, `CONTAINER_HOST` and `KUBECONFIG`
from the environment of every tool it runs - each runs under `env -i` with `PATH`,
`HOME`, `LC_ALL=C` and what it needs (`SSH_AUTH_SOCK` for ssh and an engine over ssh,
`XDG_RUNTIME_DIR` for rootless Podman's socket; neither is a setting), plus only the
variables `TOOL_ENV_PASS` names (names only; engine, kubeconfig, loader, shell start-up,
`CACTUSFS_*`, the script's own and secret-like names refused) for kubectl exec plugins and
proxies - and sets what the target needs: the Docker CLI always gets an explicit `--context`
(`ENGINE_CONTEXT`, else `rancher-desktop` for `rancher`, else `default`, the local
engine), `DOCKER_HOST=ssh://` for `CE_HOST` and `BUILD_HOST`, or `--context K8S_CONTEXT`.
The namespace marker is the annotation `deploy.cactusfs.io/owner-uid`. `k8s` requires `K8S_CONTEXT`; the current
context is never used. Before it changes anything it prints what it resolved - the
engine's endpoint and ID, or the context, its API server and the `kube-system` UID -
and records it in the deployment's `STATE_DIR` entry. A later run that resolves the same
name differently - another engine or cluster than recorded, or the name recorded under
another target and not under this one - is refused, naming both; deploying the same
name onto a second target ID is said with `--another-target`, which makes a second
deployment with secrets of its own and never lets one entry's secrets reach a second
engine. Behind the same target ID, the engine's own ID in the record decides (section
3): another one is refused, and only `--engine-replaced` lets a new engine take the
place of one that is gone. Podman reports no ID of its own, so its identity is its
host's name and the root of its store (`{{.Host.Hostname}}:{{.Store.GraphRoot}}`): a
weak identifier, which mainly tells another store apart (rootless against rootful,
another user, a new Podman machine whose store moved) and is not unique across
machines of one name.

### 16. SSH and commands from the env file

ssh runs with `-o BatchMode=yes -o StrictHostKeyChecking=yes`, and the host is given
after `--`. A host whose key is not already known is refused; the README (PLAN 10.12)
says how to add it after checking its fingerprint out of band. Before a remote engine is
used through `DOCKER_HOST=ssh://` - whose ssh the Docker CLI starts with the user's own
configuration - the same check is made with the script's own ssh, so an unknown key is
refused first. The key seen at the first install is pinned in the deployment's
`STATE_DIR` entry, and a later run that sees another is refused even if `known_hosts`
was changed.

`CE_HOST`, `K3S_SSH` and `BUILD_HOST` are plain values (the `plain()` rule of
`scripts/remote.sh`: letters, digits and `_./=:@+,-`, not empty, no leading `-`), and
ssh is never asked to run a value that is not plain. `K3S_IMPORT` is the one value that
is a command: it is split into words at spaces, without `eval`; every word must be
plain except that words after the first may begin with `-`; a shell metacharacter
anywhere refuses it; the words are run as they are, with the image as a tar stream on
standard input.

## Consequences

- One set of commands for six targets, and a second `--install` is an upgrade; the
  README's instructions for compose and the chart remain, for those who want them.
- Every secret is kept in two places, so losing one copy of a secret loses nothing as
  long as the other copy of that secret is there; the script puts it back. Losing both
  copies of the passphrase, or both copies of `kms.json`, loses every object of a
  deployment on the local KMS. The README must tell the operator to back up `STATE_DIR`,
  encrypted (PLAN 10.12).
- With no bind mounts, a standalone deployment on Docker needs nothing of the host's
  filesystem but the engine's socket; the price is that secrets are copied into
  volumes, and reading them back takes a container. Cluster mode on Docker keeps the
  compose file's bind-mounted secrets, with their 0444 files, SELinux label and the rule
  that a remote engine is reached only through `CE_HOST`, and publishes S3 on every
  address of the host, as `compose.yaml` does.
- On a Kubernetes cluster of more than one node, images are delivered only through
  `K8S_REGISTRY`.
- An upgrade from the console and one from the script are kept apart by reading the
  console's lock and by apply conflicts, not by a lock of the script's own.
- With no Helm release, `helm list` does not show a deployment the script made, and
  there is no `helm rollback`: going back is `--install` with the earlier image. A
  deployment made by `helm install` cannot be taken over by the script.
- The script depends on the chart staying free of `lookup` and hooks, on the
  `Dockerfile` naming `HELM_IMAGE` with its digest, and on the chart's
  `service.externalTrafficPolicy` value (PLAN 10.9).
- Clusters made by the script on the local KMS are development clusters, as the compose
  cluster is; a production cluster needs Vault.
- The development certificates expire after `--validity` and cannot be renewed or
  given new names by the script.
- Deploying to Kubernetes pulls the helm image at deploy time, unless it is present;
  an air-gapped site preloads it, or names a helm binary, and sets `OFFLINE=yes`.
- On a cluster reached through `k3s kubectl` the script acts as cluster-admin; only its
  code keeps it in its namespace, unless the operator gives it a namespaced context.
- Rolling upgrades take as long as every node needs to become Ready, one after
  another.
- A new target is a file of `deploy/lib/` and its transcripts in the harness, not a
  new script.

## Alternatives

- **A Helm release** (`helm upgrade --install`). It needs helm on the machine that
  holds the kubeconfig - on a remote k3s, the server - and keeps release state in
  Secrets of the namespace; the chart has neither hooks nor `lookup`, so a release would
  add only history and rollback, which `--install` with the earlier image gives.
- **Sourcing the env file** (`. deploy/.env`). Sourcing runs whatever the file holds,
  with the operator's rights; a value with `$(...)` would run it. A typo becomes a
  variable nobody reads.
- **Bind mounts** for secrets and certificates. They fail with a remote daemon, need
  file sharing on Docker Desktop, relabelling under SELinux and matching user IDs under
  rootless Podman, and leave secrets in host paths the script would have to manage.
  Cluster mode on Docker pays exactly that price, because the compose file does.
- **Secrets of the compose cluster from a named volume**, through the generated
  override. Taking the compose file's `secrets:` away needs `!reset`, which not every
  compose implementation reads, and what remained would be a second compose file in all
  but name.
- **A PowerShell script** for Windows. A second implementation to keep correct; Docker
  Desktop on Windows runs on WSL 2 anyway.
- **Cluster by default.** A cluster on one machine needs either Vault or a shared local
  key file, which is a development arrangement; one node with four drives already
  erasure-codes, and is a cluster of one.

## Open, for the owner

Defaults are applied until answered; the threat model has each, numbered 72 to 79.

1. **The local KMS** (72). Default: opted into through the example (`KMS=local` must
   be stated); a cluster on one shared key file needs `ALLOW_SHARED_LOCAL_KMS=yes`.
2. **An empty S3 allow-list on Kubernetes** (73). Default: refused for NodePort and
   LoadBalancer; `any` must be written to open S3 to everyone; such a Service is
   rendered with `externalTrafficPolicy: Local`.
3. **Windows** (74). Default: through WSL 2 only.
4. **Install steps on the console's sign-in page** (75). Default: not in milestone 10;
   the README holds them.
5. **Which targets are tried on real engines** (76). Default: podman in the CI matrix;
   `rancher` and `CE_INSTALL_DOCKER` by the harness only.
6. **The image binaries in releases** (77). Default: published from the next release.
7. **The acceptance hosts** (78). Default: standalone left running on both; purge tried
   on a second deployment name only.
8. **Vault by a host name on Kubernetes** (79, PLAN 10.9). Default: `VAULT_ADDR` must
   name Vault by its IPv4 address there, which becomes the NetworkPolicy's one egress
   rule; a host name is refused.
