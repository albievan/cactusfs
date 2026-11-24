# ADR-0033: One deployment script for every container platform

- **Status:** accepted (2026-11-23, PLAN 10.12; proposed 2026-11-21, PLAN 10.0). Built by
  PLAN 10.1 to 10.14 in `deploy/deploy.sh` and `deploy/lib/`, all done (10.2 run on a
  real Podman, 10.6 on a real Rancher Desktop with its own Kubernetes, 10.14 through
  the gate on the test host); where the build
  differs from or adds to the decision, the section says so under "As built". Rancher
  Desktop's own Kubernetes (`RANCHER_KUBERNETES=yes`, section 9) is handed to the
  Kubernetes path since PLAN 10.6's second part. Run on real engines: Docker and Podman, standalone
  and cluster (`make test-deploy`), Docker CE over SSH (a `CE_HOST` run), k3s in a
  container (`make test-k8s`), and in the acceptance of PLAN 10.13 standalone on
  Docker Desktop 29 for macOS and on a remote k3s server over `K3S_SSH`, and Rancher
  Desktop 1.25 for macOS (moby standalone and cluster, containerd standalone, its
  Kubernetes on both, PLAN 10.6); not yet on a Kubernetes other than k3s. The operator's
  documentation is the README, "Deploying with deploy/deploy.sh". It changes no Raft
  entry schema, on-disk format, crypto design or contract. The points under "Open, for
  the owner" are not answered; their defaults are what was built (the threat model,
  questions 72 to 80).
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
| `rancher` | Rancher Desktop: its moby or containerd engine, or its bundled k3s (not built yet: section 9) |
| `k3s` | k3s, local, over SSH (`K3S_SSH`) or through an import command (`K3S_IMPORT`) |
| `k8s` | any Kubernetes (1.27 or later aimed at; the script does not check the version), through `kubectl` and a context |

Actions: `--install` (run again, it upgrades and keeps the data), `--status`,
`--uninstall` (keeps the data), `--uninstall --purge` (deletes it), with
`--from-source`, `--adopt` and `--engine-replaced` (section 3), `--another-target`
(section 15) and `--dry-run`. `--dry-run` runs nothing that changes state and
prints no secret. A deployment has a name, a DNS label, which names its containers,
volumes, namespace and its entry of `STATE_DIR` (section 3).

As built (PLAN 10.1 to 10.11): the actions are also `--print-namespace` (section 7) and,
with `--install` on Kubernetes, `--take-image` (section 11); `--purge-confirm=<name>`
confirms a purge without a terminal. The exit status is 0 done, 1 a command failed, 2
refused with nothing changed, 3 not built yet; `--status` exits 1 when the deployment is
installed and not ready. `deploy/deploy.sh --help` lists every flag, and the README,
"Deploying with deploy/deploy.sh", is the operator's guide.

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
| Docker targets, cluster | `dev-init` on the operator's machine, into `STATE_DIR` (`--nodes node1,node2,node3,node4`) | the labelled volume `<name>-config`, which the override names as compose's `config` volume (`external`), filled before `up`, `ready` included, so compose's `init` service finds the directory complete and writes nothing (`dev-init` leaves a complete directory alone); the root credentials and the passphrase or token in compose's file secrets, `compose/secrets/` of the entry (section 10) | `CACTUSFS_KMS_KEY_FILE` of `deploy/compose/compose.yaml` |
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
| `ssh-known-hosts` | the lines of the user's known_hosts that accepted `CE_HOST` at the first install, as `ssh-keygen -F` finds them (hashed, `@cert-authority` and `@revoked` lines included): from then on the only known_hosts of every ssh to it (section 16; PLAN 10.7) |

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
10); on Kubernetes the namespace (section 7); the image tags the entry records the
script made, those nothing uses (as built, PLAN 10.11 for k3s and 10.14 for the Docker
engines and Rancher Desktop's Kubernetes, section 5: a tag that cannot be removed is
named and left, and does not stop the purge); and last the deployment's entry
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

**On Kubernetes, as built (PLAN 10.10).** `secrets_where` says `kubernetes`, and
`secrets_install` hands over to `secrets_install_k8s`: the files given are compared with
`STATE_DIR`'s copies first (the root credentials, the passphrase and `kms.json` refused
when they differ; a Vault token or an operator's certificate, key or CA noted as a
rotation); then 10.9's `k8s_secrets_ensure` reads every Secret back and compares it;
what neither copy holds is handed to the one hook `secrets_ensure_k8s`, after the
copy-backs, which refuses half a root pair, `kms.json` without its passphrase and a
configuration that is in part there, and otherwise makes the root credentials and the
passphrase from `/dev/urandom` and the configuration by `dev-init` on the operator's
machine (`--nodes` the chart's node IDs, `<name>-node-0` or `<name>-meta-0..2,data-0`;
in `IMAGE`, the runtime image with the binary of `make image-binaries`, or under
`--from-source` the image `image_native` makes); the Secrets the cluster lacks are
created; only then is the record written, holding the cluster's identity - the UID of
`kube-system`, compared at every install, uninstall and purge as an engine's ID is,
`--engine-replaced` included - and a rotated file replaces `STATE_DIR`'s copy after the
cluster's. A secret given by its `*_FILE` setting is read from that file and not copied
into `STATE_DIR`. The cleanups of the secrets' run directory and of every work
directory of `k8s.sh` are one EXIT trap of the script (`cleanup_add`, `common.sh`):
each command is written into the trap's text, its paths quoted, when it is added;
they run last added first; a subshell that adds its own starts from none. The
read-back, the comparison and the generation run in the script's own shell, not a
subshell on the left of `||`, so `set -e` holds in every step (the review of 10.10).
The entry records the namespace's name (`namespace`) beside its marker; every action
whose settings name another namespace is refused.

**A purge on Kubernetes, as built (PLAN 10.11).** Refused without the record and the
namespace marker in `STATE_DIR` (`--adopt` is not taken), confirmed by the name, on
the recorded cluster only, in the marked namespace only, and while no console
upgrade's lock is fresh. The workload's objects (StatefulSets, Services,
NetworkPolicy, PodDisruptionBudget, by the release's labels) are deleted; then the
release's claims (by the pods' selector, which the StatefulSet controller gives them,
`--wait=false`) - after every Secret about to be deleted was read back and found equal
to its copy in `STATE_DIR` or in the file the settings name, or refused (a value the
cluster alone holds, one that differs, a file that cannot be read, a labelled Secret
the settings do not name: exit 2, nothing removed; an uninstall checks the same before
it deletes the Secrets); a listing proves none is left, asked every 2 seconds up to
`WAIT_TIMEOUT`; the PersistentVolumes are listed until none bound to one of those
claims is left but those a `reclaimPolicy: Retain` keeps `Released`, which are named
with their path and left; the deployment's Secrets (labelled
`io.cactusfs.deploy.name`, `app.kubernetes.io/managed-by=cactusfs-deploy`) and a
listing that proves them gone; the namespace (section 7); the image tags the script
imported into k3s, recorded in the entry's file `images` only when containerd lacked
the name before the import (`k3s ctr -n k8s.io images ls -q name==<name>`), removed
over `K3S_SSH` or here with `k3s ctr -n k8s.io images rm` unless a pod of any namespace
still names it (`get pods -A`: its containers' image, its status's image and imageID),
which is named and kept; those of `K3S_IMPORT` named and left; and last the entry. A
purge whose namespace is gone already removes the images so and then the entry, when
the entry records that namespace's name. Only the workload's objects the script
applied (`metadata.managedFields` holds `cactusfs-deploy` with the operation `Apply`)
are deleted, by an uninstall as by a prune; others carrying the release's labels are
named and left, and while a StatefulSet among them is left an uninstall keeps the
Secrets its pods may use. A labelled Secret the settings do not name (made under
another `DEPLOY_MODE`, `KMS` or `VAULT_CA_FILE`) refuses an uninstall, which names the
way out (run with the settings it was made under, or save and delete it by hand); a
confirmed purge deletes it with the rest, saying so, while a value the cluster alone
holds for a key the settings do name still refuses it. A tag is also kept while the
pod template of a StatefulSet, Deployment, DaemonSet, Job or CronJob of any namespace
names it;
what cannot be listed keeps every tag. An uninstall without `--purge` deletes the workload and the Secrets and
keeps the claims, the namespace and the entry.

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
The image (PLAN 10.4, wired into this install): after the checks above, and still
before anything changes, the install asks `image_check`, so its refusals change
nothing; then the secrets' own checks (`secrets_check`: the files given, the engine's
ID against the record, the secrets' volumes and their labels, and what dev-init needs
- its container's name free, its image there), each a refusal before anything is
built; then `image_make` builds or pulls the image and checks it on the engine it is
made on; then the secrets are made and put (`secrets_install`, under `--from-source`
with dev-init in `RESOLVED_NATIVE_IMAGE`, which `image_native` makes after
`image_make` - the image of the run when it is of this machine's architecture on the
engine here); then `docker_image_ref`, called in the script's own shell, never in a
command substitution, runs `image_deliver` (a load on the target's engine for an image
built on `BUILD_HOST`; nothing to move on the target's own engine) and takes
`RESOLVED_IMAGE`; then the node's volumes, and the node, run from `RESOLVED_IMAGE` with
`--pull never`. The image made before the secrets is not a change of the deployment;
the refusals of the secrets that need the volumes' copies read (two copies that
differ) come after it, as they came after the helper container already. A dry run
asks what `image_check` asks and says what would be built and run. `RESOLVED_IMAGE`,
`RESOLVED_ARCH` and `RESOLVED_NATIVE_IMAGE` are set by the script, never taken from
its environment. Readiness is `/readyz` of the admin port, asked over TLS with the CA of
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

As built (PLAN 10.4, `deploy/lib/image.sh`, after its two reviews): an install calls
`image_check` before its first change - the secrets included - which decides and
refuses (exit 2) and only reads: the route to the target, the host keys, whether the
target is on another machine, the target's platform, the folder or tree, a tree with
changes, `OFFLINE` (the base image on the building engine for the platform built
for; a given image there). Then `image_make`, which builds, pulls, tags and checks
(a given image that had to be pulled is checked after the pull); `image_resolve` is
the two. `image_deliver` moves the image and checks the copy. For dev-init under
`--from-source`, PLAN 10.5 calls `image_native` after `image_make`: an image of the
architecture of the engine here, on that engine (the image of the run when it is that
one, else one made the same way for it), for `IMAGE_SOURCE=binaries` and `source`
only - with `IMAGE_SOURCE=image` dev-init runs `IMAGE`. `RESOLVED_IMAGE` is the
reference the target runs, `RESOLVED_ARCH` its architecture, `RESOLVED_NATIVE_IMAGE`
dev-init's. The installs run it with `--pull never` on Docker, give the pods
`imagePullPolicy: Never` on the routes import, local and kind, and split `repo@digest` and
`repo:tag` for the chart's values.

The routes: on the engine that runs it (nothing moves; `BUILD_HOST` equal to `CE_HOST`
is one); `save` there and `load` on the target's engine (`CE_HOST` through
`DOCKER_HOST=ssh://`, Podman, a Docker context), the copy then compared there - its
platform, labels, layers (`RootFS.Layers`) and configuration (user, entrypoint,
command, environment, working directory, labels, ports, volumes), not its ID, which
Docker's image store and its containerd store give differently; an import into k3s,
the tar's layers (its configuration's `rootfs.diff_ids`) checked against the image's,
built or given, and its configuration digest (its `manifest.json`) against the one
built - on Docker's containerd store, whose build gives the digest of the image's index
or manifest and no configuration's, the tar must hold that blob and the configuration
must be one it names (PLAN 10.13) - before it is sent and, over `K3S_SSH` or here, against the node's image ID afterwards
(`k3s crictl inspecti -q -o json`, `status.id`; `K3S_IMPORT`'s own words import on a
node the script cannot ask, which is said); the Docker engine here for Rancher
Desktop's Kubernetes and the contexts `docker-desktop` and `rancher-desktop`, whose
API server must be on this machine (`https://` and a dotted-quad address of
127.0.0.0/8, `[::1]`, `localhost`, or Docker Desktop's `kubernetes.docker.internal`;
one node, as for an import); a push to `K8S_REGISTRY` (a repository: a registry's
`host:port` alone is refused), the pods then running `<K8S_REGISTRY>@sha256:<digest>`
(Docker's `RepoDigests`, `podman push --digestfile`); and for `IMAGE_SOURCE=image` on a
cluster without a push or an import - or `IMAGE` already in `K8S_REGISTRY` - the nodes
pulling `IMAGE`, which must be named `@sha256:<64 hex>` and is first present on the
engine here (pulled there when absent) and checked there as any image is.
`K8S_REGISTRY` and `K3S_IMPORT` together are refused, as are nodes of more than one
architecture and a cluster none of the routes reaches.

**Docker Desktop's Kubernetes has two modes (route `kind`, 2026-11-24).** The route
`local` assumed the older one, kubeadm, whose node uses the Docker engine's images. In
kind mode (Docker Desktop 4.x, upstream Kubernetes v1.34.3 on a real engine) the one
node, `desktop-control-plane`, is a container of that engine - hidden from `docker ps`,
reached by `docker exec` - with a containerd of its own (2.2.0, the node's
`containerRuntimeVersion` `containerd://2.2.0`) that does not see the engine's images:
the pods failed with `ErrImageNeverPull`. For `K8S_CONTEXT=docker-desktop` the script
now asks, read-only, before anything changes: the nodes (`get nodes -o json`), and when
there is exactly one whose runtime is `containerd://...`, the Docker engine of the
settings' context for a container of that node's name (`docker inspect --type
container`, which must answer `/<name>`). Both true: the route `kind`; else `local`, as
before. On `kind` the image is made on the Docker engine as for `local`, saved, and
streamed into `docker --context <context> exec -i <node> ctr --namespace k8s.io images
import -` - the tar checked against the image before it is sent, as for k3s; then the
node must list it (`ctr images ls -q name==<name>`), and the target of that name
(`ctr images ls`, read through `ctr content get`: a manifest, or an index of which the
manifests the node holds are read) must name the configuration of the tar sent. The
flags were checked in containerd v2.2.0's `cmd/ctr`: `--namespace` is the global flag;
`images import` without `--all-platforms` imports and unpacks the node's own platform
(the image is built for it), where `--all-platforms` would fail on a tar whose index
names platforms whose content it lacks; `--digests` is not given, since the tar names
the image by its tag and a digest image would be a name no record holds. Two or more
nodes of a containerd runtime on that context are kind mode with several nodes: the
route stays `local`, and the one-node check refuses it, saying so, for `K8S_REGISTRY`.
A tag the node lacked before is recorded in the entry's `images` as `kind <name>
<kube-system UID>/<node>` (the rule of an engine's ID of PLAN 10.14: the node is a
container, made again with the cluster) BEFORE the import - as on the route `import`,
by its name - and the line is taken out only when the import itself fails: an import
whose check fails, or a run killed between the import and its end, leaves a tag the
record names. The tag is removed by a purge through `docker exec
<node> ctr --namespace k8s.io images rm` when the cluster and the node reached then are
the ones the line names and no pod or workload's template of any namespace names the
tag or a digest of its image (its target's, or a configuration that names); else it is
said and left. The tag built on the Docker engine itself is not recorded, as on the
route `local` (PLAN 10.14 records none on the engine of a Kubernetes target other than
Rancher Desktop's).

An image is looked up by its full ID from the moment it is made: Docker builds through
BuildKit's builder named with `--builder` (the context's own, of the docker driver, or
`default` on `BUILD_HOST`), so that no builder chosen in the user's configuration pulls
or loads elsewhere; a stale build tag (`cactusfs:deploy-<name>-build-<arch>`) is
removed first. buildx writes the configuration's digest to `--iidfile` (buildx's
`getImageID`) and the manifest's to `--metadata-file` (`containerimage.digest`); the
first is the image's ID in Docker's image store and in Podman, the second in Docker's
containerd store (moby's `mobyexporter` and `exporter/wrapper.go`). The ID the engine
finds of the two is taken - else the build tag's ID when it is one of them - and the
build tag must name it; else the run fails before anything is tagged. The image is
inspected, tagged, run and pushed by that ID. A saved image carries the name it was
saved by, so the save is by its tag, which is checked to name that ID right before. A
given image is inspected once (ID, labels, `RepoDigests`, layers, configuration) and
tagged by its ID; moved as a tar stream it keeps no registry digest, so it is tagged
`cactusfs:<its version label>-<12 hex of its ID>`. Its labels pass the same validation
as `BUILDINFO` (its commit may be 7 to 40 hex digits), and its `RepoDigests` are
matched with `docker.io/library/` and `docker.io/` taken off, as Podman writes them. An
inspect of more than one line, or with a control character (a label holding one),
fails the run. A tar stream goes through a file of a work directory of its own, under
`TMPDIR` (a failed save is never loaded); the directory is removed by a trap of the
subshell that uses it, whose path is expanded when it is set. That subshell runs in
the foreground, so a Ctrl-C (an INT to the process group) or a TERM to the group
reaches it and the tool it runs at once; a signal sent to the script's process alone
is taken when the step it waits on ends (a shell runs a trap after its foreground
command), and the run then ends the same way. The binary's `version` line is read in a
container without network or privileges, only on an engine of the target's
architecture.

A target is on another machine - where a tree with changes, or of no known commit, is
refused unless `ALLOW_DIRTY=yes` (a warning) - for `BUILD_HOST`, `CE_HOST`, a Docker
context whose endpoint is not a `unix://` or `npipe://` socket (`docker context
inspect`), a Podman whose service is remote (`podman info`'s `Host.ServiceIsRemote`,
true for a Podman machine, as on macOS), `K3S_SSH`, `K3S_IMPORT` (its words may reach
anywhere; a word that is `ssh` is refused: that is `K3S_SSH`), a push, and any cluster
but a k3s here and the desktops' as above. "Uncommitted changes" counts files git does
not track and does not ignore as well (they are in the build context). Without git, or
outside a repository, a build from source is build 0 of no known commit: allowed for a
target here, refused for one on another machine. An image made of binaries for a
target on another machine takes the rest of the image from this tree's Dockerfile, so
HEAD must be `BUILDINFO`'s commit and the Dockerfile without changes (`git status
--porcelain -- Dockerfile`), else the same refusal. On Rancher Desktop's containerd
(PLAN 10.6, section 9) the engine is `nerdctl`, always given its namespace (`default`,
or `k8s.io` for an image its k3s runs, on the route `local`): it builds with
`nerdctl build --iidfile` (no `--metadata-file`; `--pull=false` under `OFFLINE`), and
since `nerdctl` finds an image by its name or by the digest of its target (the
manifest), which its `--iidfile` holds and its `RepoDigests` name, while `image
inspect` says the configuration's digest as `.Id`, the image's ID on `nerdctl` is the
target's digest read from `RepoDigests`; its `image tag` takes a name and not an ID,
so a tag is made from the name checked to name that ID, and checked again after. A
given image named by a tag is tagged by its ID there (`nerdctl` names every image by
a digest, from a registry or not, and runs none by that name). Rancher Desktop's
engine is this machine's (its virtual machine, as Docker Desktop's is).

**Reproducible, and the tags it made, as built (PLAN 10.14).** An image made of
binaries takes the time of the commit `BUILDINFO` names, git's committer time (`git
show -s --format=%ct <commit>^{commit}` in this repository; a `-dirty` suffix is
dropped): `docker buildx build` and `nerdctl build` are given `--build-arg
SOURCE_DATE_EPOCH=<time>` (buildx, and nerdctl's BuildKit, set the image's creation
time and its history's from it), buildx also `--provenance=false` (the default
provenance attestation records the build's own times and invocation, and on Docker's
containerd store the image's ID is the digest of the index holding it), `nerdctl
build` also `--output type=docker,rewrite-timestamp=true` (BuildKit's docker exporter,
which nerdctl loads from in its namespace, clamps every file's time in the layers to
the epoch; BuildKit 0.13 or later), and `podman build` its own `--timestamp <time>`
(Podman 5.4's option: the creation time and every file of the layers; it takes no
`SOURCE_DATE_EPOCH`). So on Podman and nerdctl the same binaries make the same image.
On buildx with Docker's `docker` driver they do not: its `--load` goes through the
engine's own exporter, which on the classic image store keeps an unknown option as
metadata and ignores it (moby's `mobyexporter`) and on the containerd store forces
`unpack=true`, which BuildKit refuses beside `rewrite-timestamp` (`exporter/
containerimage/export.go`, v0.25.0); the docker driver of the classic store has no
`type=docker` exporter to a file either. There the files keep their own times: the
same folder of binaries makes the same image, a copy made anew another (whose tag
the upgrade then removes). The run says which holds. When git does not find the
commit the run says so and the image takes the time of its build. A tag the run makes
on an engine that lacked it (`image inspect` asked first) is recorded, once the entry
is there, in the entry's file `images` (0600) with the ID of its engine: `target <tag>
<engine ID>` on the target's engine - built there, or loaded there from the engine it
was built on - with the deployment's recorded engine ID (`engine-id` of `target`: the
Docker engine's ID, or Podman's host and store; for Rancher Desktop's Kubernetes
`<UID of kube-system>/<engine ID>`, the ID of the engine whose images its k3s sees
added, section 9), and `here:<context> <tag> <engine ID>` on the Docker engine of
this machine when the target's is another machine's (`CE_HOST`), with what `docker
--context <context> info --format '{{.ID}}'` says (a copy whose engine says nothing is
said and not recorded); `BUILD_HOST`'s engine, an `IMAGE`, a tag that was there before
and the engine of a Kubernetes target other than Rancher Desktop's are not recorded.
A line is acted on only when the engine reached now has the ID it names; one of
another engine, or with no ID (an earlier build of the script wrote it), is named and
left. `--engine-replaced` takes the recorded engine as gone with its images: once the
record names the new engine, every line of the target's engine leaves `images`, each
said, and nothing is removed (`here:` lines stay). Once a Docker target's nodes are
ready, the tags the replaced nodes ran (standalone: the container's image; a compose
cluster: each node's) are removed when recorded and no container of that engine names
them (`ps -a --format '{{.Image}}'`, compared without `docker.io/library/`,
`docker.io/` or `localhost/`), and so are the recorded copies of this machine's engine
but the run's; a purge removes every recorded tag no container names, after the
secrets and before the entry. On Rancher Desktop's Kubernetes, once every node is
Ready after an upgrade and at a purge, a tag is also kept while a pod or the template
of a StatefulSet, Deployment, DaemonSet, Job or CronJob of any namespace names it, by
name or by a digest of its image (`image inspect`'s `.Id` and `.RepoDigests` against
the pods' `imageID`): the kubelet names images by ID, and another namespace may run
the same image unrecorded; when either listing fails, or the image cannot be inspected,
it is kept. The engine is asked by its own CLI (`docker --context`, `podman`, `nerdctl`
in its namespace, the Docker CLI over `DOCKER_HOST=ssh://` for `CE_HOST`), which itself
refuses to remove an image a container uses; a removal that fails is said, its line
kept, and the run goes on. A compose cluster's `init` container keeps the first
install's image until an uninstall.

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

**As built (PLAN 10.10).** The render is checked before it is applied: only
StatefulSets, Services, NetworkPolicies and PodDisruptionBudgets, and the StorageClass
and PersistentVolumes of `K8S_STORAGE=hostpath`; any other kind fails the run. It is
applied with `KUBECTL apply --server-side --field-manager=cactusfs-deploy -n <ns> -f -`,
never with `--force-conflicts`; a conflict stops the run naming `--take-image`
(section 11). kubectl's own `--prune` is not used (it prunes by the client-side
annotation a server-side apply does not write): the release's objects of those four
kinds are listed by its labels in the namespace before the apply, and those the render
no longer holds, and that the script applied (`cactusfs-deploy` with the operation
`Apply` among their `managedFields`, read with `--show-managed-fields`), are deleted
after it - but a StatefulSet that is not a node of the
settings (`DEPLOY_MODE` or the name changed) refuses the install before any change,
since it holds a node and its data. The values give `image.pullPolicy: Never` on the
routes `import`, `local` and `kind` of section 5, and the image by digest or by tag,
never both.


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
- the PersistentVolumes (list), at a purge, for the volumes bound to the release's claims;
- the pods, StatefulSets, Deployments, DaemonSets, Jobs and CronJobs of every
  namespace (list), at a
  purge of an image imported into k3s, for the image tags still in use;
- the kinds the API offers (`api-resources`), at a purge, to find the namespace empty.

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
| ClusterRole `cactusfs-deploy-<ns>` | `""` | `nodes`, `services`, `persistentvolumes` (and `pods` when the image is imported into k3s) | | `list` (PersistentVolumes for the purge's check that none bound to the claims is left, pods for its check of the image tags still in use, PLAN 10.11) |
| | `apps` | `statefulsets`, `deployments`, `daemonsets` (only when the image is imported into k3s) | | `list` |
| | `batch` | `jobs`, `cronjobs` (only when the image is imported into k3s) | | `list` |
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

**As built (PLAN 10.10, 10.11).** The namespace is checked (read, its marker
compared, a Helm release looked for) before anything changes, and made after the
image; a namespace confirmed at a terminal is listed for the release's objects like
one of its own. At a purge it is read again: a namespace that no longer carries the
marker is left; every kind the API offers to list in a namespace
(`api-resources --verbs=list --namespaced -o name`, events but) is listed in it, and
one that holds anything but `configmap/kube-root-ca.crt`, `serviceaccount/default` and
the deployment's own objects on their way out (its pods, Services, StatefulSets, their
Endpoints, EndpointSlices and ControllerRevisions) is named and left; a listing that
fails, or a delete the context may not make (Forbidden), leaves it to the
administrator; otherwise it is deleted by `delete --raw /api/v1/namespaces/<ns> -f -`
with `{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":...,
"resourceVersion":...}}` of that read, and a precondition that fails keeps the entry.
The preconditions guard the Namespace object itself, not what it holds: an object made
in it after it was found empty is deleted with it. When the image is
imported into k3s, the ClusterRole of `--print-namespace` also lists pods,
StatefulSets, Deployments and DaemonSets cluster-wide, for the purge's check of the
image tags; otherwise it does not.
`K8S_STORAGE=hostpath`'s directories are not made by the script: the install names
them (`<K8S_HOSTPATH>/<ns>/<node ID>/<volume>`, owned by 65532) and the operator makes
them. Besides the reads listed above, the script reads the API server of a named
context from the kubeconfig here (`config view --minify`), which asks the cluster
nothing.


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
  server's loopback, and `k3s kubectl port-forward` on the server. On Docker Desktop's
  Kubernetes in kind mode (v1.34.3, run on a real engine on 2026-11-24) the round trip
  reached S3 through `kubectl port-forward service/<name> <port>:9000`; whether a
  NodePort is reached from this machine in kind mode, as in kubeadm mode, remains
  unchecked.
- **A pod that cannot start (2026-11-24).** The wait for a node's pod - after an
  install, and for each node of an upgrade - reads its containers' (init containers'
  included) waiting reason. `ErrImageNeverPull` (the image is not in the node's runtime
  under `imagePullPolicy: Never`) and `InvalidImageName` cannot clear by themselves
  here: the kubelet syncs the pod again, but the image was delivered to the cluster - and
  on an import checked on the node - before the apply, and the pod's reference is the
  one applied, so nothing the run waits for changes the answer. The run fails at once,
  on every route, naming the pod, the reason and the kubelet's message, the image, and
  the route it took to the cluster (in an upgrade, with the note that the nodes after
  it were not moved and the image to go back to), rather than waiting out
  `WAIT_TIMEOUT`. Only a pod of the deployment on its new UID that runs the image of
  the run counts: the old pod of an upgrade, or a pod on another image, does not.
  Other waiting reasons (`ImagePullBackOff`, `CrashLoopBackOff`) can clear by themselves
  and are waited for as before.
- **Docker.** S3 on `BIND`; the admin port on the loopback address. For `CE_HOST`,
  `--status` prints the `ssh -L` command that reaches it. There the loopback address
  is the server's, so whether a port is free and whether the node is ready are asked
  on the server, by its own curl over ssh (PLAN 10.7): a port by curl's exit status
  (7, nothing answered, is free; anything else is in use), `/readyz` by the status
  line of its headers (`-D -`), over TLS with the deployment's CA streamed on ssh's
  standard input (`--cacert /dev/stdin`), never `-k`, never through a proxy, the
  address given with `--resolve` (an IPv6 address needs no brackets there, which
  would not be plain), and `-q` first so no `.curlrc` is read (the script's curls on
  this machine too). An ssh that fails while `/readyz` is asked reads as `000`, and
  the wait goes on. That is simpler than an `ssh -L` forward with a free local port, a
  background process and a trap, and reads nothing back but a status line; the CA is
  a certificate, not a secret. S3 is said to be at the HostName `ssh -G` gives for
  `CE_HOST` (the target's, behind a ProxyJump).

**`--status` on Kubernetes, as built (PLAN 10.11).** Read-only: the namespace (not
installed, or not the deployment's, said), each node's pod (its machine, readiness,
image and version), a console upgrade's lock, the S3 and admin Services - a NodePort
listed on the InternalIP and ExternalIP of each machine that runs a ready pod of a
gateway, a load balancer by its ingress, a ClusterIP as the Service's name in the
cluster - `/healthz` and `/readyz` of a published admin port over TLS with the
deployment's CA (`TLS_CA_FILE`, else `STATE_DIR`'s) through `curl --connect-to`, the
certificates' expiry (the S3 certificate, and in cluster mode each node's), and the
port-forward commands when no admin port answered 200. It exits 1 when a node of the
settings has no pod or one that is not Ready.


So `admin` / `admin` is never published by a default the script chose.

### 9. `rancher` is a target of its own

Rancher Desktop differs from Docker Desktop in its CLI, in how its Kubernetes shares
images, and in its context name; a user should not have to know that. `--docker
rancher` runs over the code of `dd` and `k8s`: `RANCHER_ENGINE=auto` picks moby (the
`rancher-desktop` Docker context) or containerd, where every engine command is
`nerdctl` and images meant for its k3s are built with `--namespace k8s.io`;
`RANCHER_KUBERNETES=yes` hands over to the Kubernetes path with the local image and the
context `rancher-desktop`.

As built (PLAN 10.6, `deploy/lib/rancher.sh`): `auto` asks, read-only and before the
engine is named or recorded, `docker --context rancher-desktop info --format
'{{.ID}}'` (`ENGINE_CONTEXT` names another context) and takes moby when it answers an
ID, else `nerdctl --namespace default info --format '{{.ID}}'` and takes containerd
when that answers; neither is refused, as is `RANCHER_ENGINE=moby` without `docker`
and `containerd` without `nerdctl`. The engine is part of the record's first line
(`Rancher Desktop, engine moby, docker --context rancher-desktop`, or `Rancher Desktop,
engine containerd, nerdctl --namespace default`), so an entry made on one engine is
refused on the other. On moby a deployment is Docker Desktop's: standalone
(`docker.sh`) or a compose cluster (`compose.sh`) through `docker --context
rancher-desktop`. On containerd a standalone deployment is `docker.sh`'s with every
engine command `nerdctl --namespace default` (`build`, `run`, `create`, `cp`,
`volume`, `ps`, `port`, `info`, `image`), and these differences of `nerdctl`, each read
in its source (v2.4.2): its `cp` has no `-a` and extracts as root keeping the owner a
stream says, so every tar stream the script puts into a container there (the secrets'
volumes, dev-init's input) is made owned by 65532 by number with no names (`--owner
--group --numeric-owner`, which GNU tar and bsdtar both take); and `cp -` (a stream on
standard input) is not in every release Rancher Desktop bundles - the `nerdctl` v2.2.2
of Rancher Desktop 1.25.0 answers "support for reading a tar archive from stdin is not
implemented yet" although its usage names `-` (found on a real Rancher Desktop,
2026-11-23), and `cp <container>:<path> -` (a stream to standard output) answers
"support for writing a tar archive to stdout is not implemented yet" - so the script
uses neither on any `nerdctl`. Rancher Desktop's `nerdctl`
is a wrapper, `rdctl shell sudo nerdctl ...`: `nerdctl` runs as root in Rancher
Desktop's VM, and a path it is given is the VM's. And its `cp` into or out of a
container that never ran sees only the container's snapshot, not the volumes mounted
into it ("destination parent does not exist", "source does not exist" on v2.2.2, though
v2.4.2's `cp_resolve_linux.go` reads the mounts from the spec): so on `nerdctl` the
secrets' volumes have no helper container and no `cp` (below), and only dev-init's
container, whose `/tmp` is its snapshot's, is copied into and out of. A stream therefore
reaches dev-init's container in four steps (`nerdctl_put`): `rdctl shell sudo mktemp -d
/dev/shm/cactusfs-deploy.XXXXXX` makes a private directory on a tmpfs (root's, `0700`;
nothing reaches the VM's disk) - not `/tmp`, which on the real VM is the disk (ext4 on
`/dev/vda1`, mounted at `/mnt/data`; only `/`, `/run` and `/dev/shm` are tmpfs, found by
the review of 2026-11-23). mktemp's answer must be exactly the template with its X's
made letters and digits (one line), else the run fails and nothing is written to or
removed from it; once its removal is in the EXIT trap, `rdctl shell sudo stat -f -c %T
<dir>` must answer `tmpfs`, else the run fails before anything is written there. The
directory is then given to 65532 (`chown 65532:65532`, as the
volume's root and dev-init's `/tmp` then are); `rdctl shell sudo tar -x --same-owner -p
-C <dir> -f -` extracts the stream there with it on standard input, keeping its owners
and modes (a `.` entry, when the stream has one, sets the directory's own); `nerdctl
cp <dir>/. <container>:<dir>` copies it in - `nerdctl` archives the directory and
extracts it into the container as root, keeping owners and modes
(`pkg/containerutil/cp_linux.go`); and `rdctl shell sudo rm -rf <dir>` removes it. The
removal is in the run's one EXIT trap from the moment the directory is made, so a
step that fails, or a run stopped, still removes it; an answer of `mktemp` that is not
a directory of that template fails the run and is never removed. A read out of
dev-init's container (its output) is the mirror (`nerdctl_get`): a private directory made the same way and in the trap at once, `nerdctl
cp <container>:<dir> <vmdir>/` (the directory copied in under its own name), `rdctl
shell sudo tar -c -C <vmdir> -f - <name>` to standard output into a file of the run's own
directory of the entry - the layout of `docker cp <container>:<dir> -`, every entry under
the directory's name, so the readers are those of every engine, which extract it
without owners and check it holds only files and directories - and the directory
removed. No dry run reaches either: dev-init runs, a change, and `nerdctl_get` fails as
an error of the script if one does; the harness counts the VM's directory as a change
and its archive as a read. A secrets' volume is read and filled in place, in the VM:
`nerdctl --namespace <ns> volume inspect --format '{{.Mountpoint}}' <volume>` (a read)
names its directory, which must be exactly `/var/lib/nerdctl/<hex>/volumes/<ns>/<volume>/_data`
(nerdctl's data root, the digest of containerd's address, the namespace and that
volume's own name; one line, nothing else) - any other answer fails the run as an error
of the script, and nothing is read from or written to it. A put (`nerdctl_volume_put`, a
change) is `rdctl shell sudo tar -x --same-owner -p -C <mountpoint> -f -` with the
owner-stamped stream on standard input: owners and modes kept, and the stream's `.`
entry, in a volume made in the run, makes the volume's root 65532 and `0700`, as `cp -a`
does on Docker - and before every put the volume's root is given to 65532 and made `0700`
(`chown`, `chmod` on the validated mount point), as a volume made by an earlier run and
never filled keeps nerdctl's `root:root 0755`; a rotation is the same put. A fetch (`nerdctl_volume_get`, a read) is
`rdctl shell sudo tar -c -C <the mountpoint's parent> -f - --transform=s,^_data,<unit>, _data`
into a file of the run's directory of the entry: every entry under the unit's name,
the layout `docker cp <helper>:/var/lib/cactusfs/<unit> -` gives, so `secrets_fetch`'s
reader is that of every engine. The stream's first entry, the volume's root, must be a
directory owned by 65532:65532 with the mode `0700` (read from its ustar header, as
Podman's `volume_root_check` does); otherwise a volume that holds anything is refused
(exit 2) with the `chown` and `chmod` that fix it, and an empty one is left to the put,
which sets its root. Nothing is made in the VM for a volume, so there is
nothing to clean up; the volumes are still made by `nerdctl volume create` with the
deployment's labels, and those labels are still checked before anything is read. No secret is in an
argument, an environment or the output: it is only ever on a standard input. `rdctl`
is the one on `PATH`, else the one beside `nerdctl` (Rancher Desktop installs both in
`~/.rd/bin`); without one, an install there is refused before anything is changed. The
helper containers are in the same trap from their creation (`helper_later`):
`<name>-dev-init` on every engine, which holds the passphrase once its input is
copied in, and `<name>-volumes` on every engine but `nerdctl` (which has none) are removed, by the engine and context that made them,
when the run fails after making them (one left by an earlier run is removed at the
next only with the deployment's labels, as before, and refused without). It copies into and out
of a container that never ran by mounting its snapshot, so dev-init's container is
never started for a copy, as on Docker; its `volume inspect` gives `Labels` as
a pointer, nil without labels, read through `with`; its `ps` has no `.State` and a
container holds none of its image's labels, so `--status` reads the version label of
the image; the node's restart policy is `always`, the one every containerd restart
plugin has - and under it containerd's restart monitor starts again a container
`nerdctl stop` stopped (v2.2.2 on the real Rancher Desktop: after the stop and rename
of an upgrade the old node showed "Restarting", held its ports, and the new node's
`run` failed with "port is already allocated"). So a node the script stops has its
policy set to `no` with `nerdctl update --restart no` BEFORE the stop - the old node of
an upgrade, and the node an uninstall stops and removes; an update that fails stops the
run before anything is stopped. Whenever the old node is started again - its stop or
its removal failing, and the way back from an upgrade an earlier build left half way -
`update --restart always` gives its policy back (`docker_restart_policy`). And a stopped
container keeps the host ports it published reserved until it is removed (v2.2.2;
Docker frees them at the stop: the new node's `run` failed with "port is already
allocated"), so on `nerdctl` the standalone upgrade keeps no `<name>-previous`
(`docker_upgrade_nerdctl`). Before anything is changed the old node's image is pinned:
its tag (its `.Config.Image`, by which the script ran it) must be on the engine, the
digest of its target - nerdctl's ID of an image - is read from its `RepoDigests`, and
the container's `.Image` (the digest it was made from, in nerdctl releases that record
it, else the tag) must name the same image, else nothing is stopped. Then its policy is
cleared, it is stopped and removed - never its volumes - and the new node is run and
waited for; the old tag stays recorded, and is removed only once the new node is ready.
A new node that cannot run (`run` fails: it never started, so the metadata is as the old
node left it) is removed and the old image is run again by its digest with the same
volumes and settings and waited for (`docker_rollback`): the run then fails (exit 1)
saying it rolled back and nothing was upgraded - or, when the old image does not come up
either, that the node is DOWN, with the command that runs it again (`<cli> rm -f
<name>`, then `deploy/deploy.sh --install` with `IMAGE_SOURCE=image IMAGE=<old tag>`).
A new node that runs but is not ready within `WAIT_TIMEOUT` is NOT rolled back (Albie's
decision, 2026-11-23): once it ran it may have changed the metadata already, which the
old image may not read, so the operator decides, as on Docker, where the old node is
kept - it is left running and the run fails with its log's command and the way back.
From the removal until a node runs again the run's EXIT trap says the way back should
the run be stopped (a signal), also during the rollback's own run.
Docker and Podman keep their upgrade: the old node stopped and renamed `<name>-previous`
until the new one is ready. A compose
cluster on containerd is refused before anything is changed (with
`RANCHER_ENGINE=containerd`, before the engine is asked anything): `nerdctl compose` has
no `up --no-deps`, with which a cluster is upgraded one node at a time, and no `compose
ls`. An `ENGINE_CONTEXT` on moby must reach Rancher Desktop: `docker info`'s `.Name`
is `lima-0` (its Lima instance is `0`, `lima.InstanceName` in its `rdctl`; Lima names an
instance's host `lima-<instance>`; `lima-rancher-desktop` is taken too) or its
`.OperatingSystem` names Rancher Desktop (its WSL distribution), else the run is
refused; the header says the context and what its engine said. A deployment found
by its namespace in Rancher Desktop's Kubernetes, and not on its engine, is refused
unless `RANCHER_KUBERNETES=yes` says so. On `nerdctl` an `IMAGE` named
`<repository>@sha256:<hex>` is inspected by its bare digest - `nerdctl image inspect` of
a repository with a digest takes only an image tagged `latest` - and its repository
must be among its `RepoDigests`; the configuration's digest (`.Id`) of an image built
there is kept, against which a saved tar is checked. With `RANCHER_KUBERNETES=yes` the engine is asked only by an install (at its
start, for its ID when a tag is recorded, and after an upgrade with a recorded tag to
remove) and by a purge with image tags to remove, and the secrets are the namespace's
(10.9, 10.10), not volumes. What only a real Rancher Desktop proves is listed in PLAN
10.6's rows of Decisions.

**`RANCHER_KUBERNETES=yes`, as built (PLAN 10.6, after 10.10 and 10.11).** Install,
upgrade, `--status`, uninstall and purge are those of k3s and k8s (`k8s.sh`) through
`kubectl --context rancher-desktop` (`K8S_CONTEXT` names another); the record's first
line is that command, and the target ID `rancher-rancher-desktop`. The image is made on
the engine `rancher_resolve` finds - asked, read-only, at the start of the install, in the namespace `k8s.io`
on containerd - by `nerdctl --namespace k8s.io build`, or `docker --context
rancher-desktop buildx build` on moby, whose images Rancher Desktop's k3s sees: the
route `local` of section 5, whose API server must be on this machine, and the pods run
it with `imagePullPolicy: Never`. dev-init runs on that engine; the Secrets go through
10.9's path (`secrets_install` dispatches to `k8s_secrets_ensure`). The chart is
rendered in the helm image on that engine too: `nerdctl --namespace default` on
containerd, where there may be no Docker engine. The tags built there are recorded
in the entry's `images` as `target <tag> <cluster UID>/<engine ID>`: the UID of the
cluster's `kube-system` (the deployment's `engine-id`), and the ID of the engine that
holds the tag - `docker --context rancher-desktop info --format '{{.ID}}'` on moby,
`nerdctl --namespace k8s.io info --format '{{.ID}}'` on containerd (nerdctl's
Docker-compatible `info` gives containerd's introspection UUID as `.ID`, v2.4.2
`pkg/infoutil`), asked once a run. The cluster's UID alone would not do: Rancher
Desktop's runtime can be switched between moby and containerd under the same cluster,
and the engine that held a tag is then another. A line is acted on only when both
parts match what is reached now; a line of the other runtime, a line an earlier
build wrote with the cluster's UID alone, and every line while the engine does not
say its ID, are said and left, and nothing is removed through them. After an upgrade
there, once every node is Ready, the tag a replaced pod ran is removed when it is
recorded and nothing names it; a purge removes every recorded tag nothing names.
Nothing names a tag when no container of that engine (its k3s's among them) runs it
and no pod or workload's template of any namespace names it or its image's digest
(`k8s_images_used`), each listed before anything is removed; the removal is by the
engine's own CLI. A removal that fails is said, the line stays, and the run goes on.

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

**As built (PLAN 10.8, `deploy/lib/compose.sh`).** The compose project is the
deployment's name. Its two copies of the secrets are PLAN 10.3's, with the same
guarantees and code (`secrets_install`): the config unit is the labelled volume
`<name>-config`, which the override names as compose's `config` volume (`external:
true`, so compose neither makes nor labels it), and the secrets unit is the directory
of compose's file secrets - read back and put as tar streams, compared file by file
with `STATE_DIR`'s, put only where missing, a difference refused - on `CE_HOST` over
ssh (`test -d`, `tar -c`/`tar -x -o` on standard input and output, `chmod`), never as
an argument. Every install sets its files 0444 and the directory 0700 again, and
relabels it under enforcing SELinux (`getenforce`; on `CE_HOST`, a missing one, exit
127, is no SELinux). The record of a cluster ends in `mode=cluster` (a standalone
record has no such line, as every record of 10.3 and 10.5): a deployment is never
installed or removed in the other mode. The override sets, for every service, the
image with `pull_policy: never` and the labels; for `init`, `--validity` and `--hosts`
(with Vault, `init` runs `version` - there is no key file to seal - and the
passphrase secret of `compose.yaml` is an empty file of `compose/none/`); for each
node, `CACTUSFS_DRIVE_LABELS` of compose's volume names, `CACTUSFS_UPGRADE_MODE=off`,
the restart policy of section 4 (`unless-stopped`, `always` on Podman), Vault's
settings and token secret, and the env file's `CACTUSFS_*` lines that `compose.yaml`
does not interpolate; labels on compose's volumes and its default network.
`compose.env` holds every variable `compose.yaml` interpolates, read from the file
itself - the image, `CACTUSFS_PORT_BASE_1` to `_4` (`S3_PORT / 100`), the env file's
value of a node setting or nothing - and a variable the script does not know fails
the run; a value with a character compose's reader of env files may take as quoting
or a comment is refused. In cluster mode `S3_PORT` ends in `00`, `ADMIN_PORT` is
`S3_PORT + 1`, and `ADMIN_BIND` and `DRIVES` stay at their defaults. Before anything
changes: the operator's node certificates and a Vault CA are refused (not carried
yet); the Docker context must reach a local socket (`unix://`, `npipe://`) and Podman
must not be a remote service (but on macOS, whose Podman machine shares the home
directory); compose must answer; the deployment's containers (by label) must be of
this target and compose's (a standalone node of the name is refused), and no
container of a compose project of the name may lack the label; the volumes labelled
with the name must be of this target and compose's names or `<name>-config`, and none
of compose's names may be there unlabelled; data without a record needs `--adopt`,
and data without the key file is refused; every port of the four nodes must be free
or the deployment's own (another container's binding refused by name; then a probe,
on `CE_HOST` by the server's curl, where only exit 7 is free). compose runs `up -d
--no-build`, from the entry's files, under `env -i` (on `CE_HOST`, `env -i` with a
fixed `PATH` and the user's `HOME`, asked with `printenv HOME`); each node is ready
when `/readyz` of its admin port answers 200 (on `CE_HOST`, the server's curl with
the CA sent beside the compose files). An upgrade rewrites the override and runs `up
-d --no-build --no-deps <service>` for node4, then the voters, the leader last when
`GET /api/v1/cluster` names it (asked with the root credentials as the bearer API key
on curl's standard input, `-H @-`), each ready before the next; the first that is not
within `WAIT_TIMEOUT` ends the run (exit 1), naming the nodes upgraded and those not
touched. One node that is not ready before an install - failed on a new image or
secret, stopped, or without its container - is made first, and the readiness of each
node before the next keeps a second from going down: that is how a failed upgrade is
rolled back or retried; two or more not ready are refused, naming them and the way out
(`--uninstall`, then `--install`). A rotation (a Vault token, the operator's
certificate, key or CA) on a running cluster makes every node again with
`--force-recreate`, in the same order and with the same readiness stop: a rotated file
is a new file, and a bind mount of a single file keeps the one the container was made
with. The rotation is marked pending (`compose/recreate-pending` of the local entry)
before the target's copy is replaced, every install makes every node again by force
while the marker is there, and it is removed only once all four are made again and
ready - a run that stops part way, after both copies were replaced, is finished by the
next, and its failure says which nodes hold the new files and which the old. On `CE_HOST` the docker
compose runs with there must report the engine ID that `DOCKER_HOST=ssh://` reaches,
and the record names the server's compose directory (`compose-dir=`), which a later
`CE_STATE_DIR` must keep. In cluster mode the env file may not set `CACTUSFS_TOPOLOGY_*`
or any key `compose.yaml` sets per node, and on macOS a remote Podman service is taken
only when its default connection is a Podman machine of the machine (127.0.0.1 or
localhost). Uninstall stops and removes the deployment's containers and its labelled
network by label (not `compose down`, which acts on compose's project label alone)
and keeps the volumes and the files; a purge (section 3) first refuses any volume of the
name that is not compose's 12 or `<name>-config`, then deletes compose's volumes,
proves none left, deletes `<name>-config`, proves none left, then on `CE_HOST` the
server's `compose/secrets/` and the rest of its directory (checked gone), and last
the entry. What remains: `podman compose` with `podman-compose` as its provider may
merge an override's list of secrets differently from `docker compose` (only Vault's
token adds one); a remote engine behind a local socket (an ssh tunnel) is not seen.
Merged onto PLAN 10.4 and 10.7: the cluster's install takes the image as a
standalone install does (`image_check` with the refusals, the image made before the
secrets, delivered after them), and on `CE_HOST` its ssh is held to the pin of section
16 - checked first (`ce_begin`), written at the first install, given alone to every
later ssh, the Docker CLI's included; `CE_INSTALL_DOCKER=yes` installs Docker CE with
its compose plugin after the cluster's own refusals. Before anything changes, compose must
reach the engine as well as answer: `compose ls` (not asked of podman-compose, which has
no `ls`); Podman's docker-compose provider needs `podman.socket`, which the refusal names.

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

**On Kubernetes, as built (PLAN 10.11).** After the apply, for each node in the order
`<name>-data-0`, then `<name>-meta-0` to `-2` (standalone: its one node): the lock is
read; the StatefulSet is read until its controller has seen the apply
(`observedGeneration` equal to `generation`, within `WAIT_TIMEOUT`); a pod already of
its `updateRevision` and image is left; otherwise it is deleted (`--wait=false`) and the
new pod - another UID, `Ready`, of that revision, running the image - is waited for up
to `WAIT_TIMEOUT`, else the run stops (exit 1) naming the image the node ran before
and the nodes after it untouched. The leader is not asked of the admin API, which needs
a credential and none is put on a command line: the voters go in the order of their
index (a decision of PLAN 10.11). Before each pod is deleted the release's pods are
listed: unless every other node's pod is Ready (and not being deleted) the run stops
(exit 1); an upgrade does not begin (exit 2) while more than one is not Ready, and one
that is not is replaced first. The lock is read on the first node by name, as the
nodes choose it (`<name>-data-0` in cluster mode). A lock is fresh when its heartbeat
(RFC 3339, read from the annotation) is at most 1500 s old by this machine's clock -
twice the nodes' `DefaultNodeTimeout` of 10 minutes, and 300 s for a clock here ahead
of the cluster's (the heartbeat is the node's time); a lock whose heartbeat cannot be
read is taken as fresh (the stricter reading). `--take-image` (or `--take-image=<name>`
without a terminal) first applies the whole render with `--dry-run=server`: no
conflict, nothing is forced. Its answer is read by allowlist, line by line, as the
API server and kubectl write a conflict (apimachinery v0.37.0 `getConflictMessage`,
kubectl v0.37.0 `apply.go` and `MultipleErrors`): a one-conflict header, whose path
follows its last `: `; a several-conflicts header or a further manager's `conflicts
with` line; a `- <path>` line; one of kubectl's fixed advice lines; a `Warning:`
line; or an empty line. Any other line (an error that is no conflict, a connection
refused), a number of paths other than the headers count, or a path but the cactusfs
container's image and the pod template's `app.kubernetes.io/version` label (the two an
upgrade from the console patches; a top-level field such as a StorageClass's
`.reclaimPolicy` included) refuses it (exit 2) before the workload changes; otherwise the render is applied
**once**, with `--force-conflicts`, by the script's own manager `cactusfs-deploy`, which
server-side apply then makes the owner of exactly those conflicting fields. There is
no second field manager, no put-back and no release step (the review of 10.10/10.11:
a multi-step take could orphan the image field, or stop part way through the
StatefulSets). The upgrade then rolls the pods one at a time as usual. What remains: a
conflict that appears between the dry run and the apply is forced with them.

The check that every other node is Ready is a listing, and the pod is deleted after
it: a node that goes away between the two is not seen, and the run then has two away
until the next node's check stops it. The pod is deleted, not evicted, so the
PodDisruptionBudget is not consulted - the script's own check takes its place; an
eviction would need the `pods/eviction` permission in the Role, which the script does
not ask for.

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
sets it. As built (PLAN 10.4): the image is built with `docker buildx build
--builder <name> --load` (BuildKit, as `make image-from-binaries` builds the same
stage), so the flag is `docker buildx build --pull=false`; the base image of an image
made of binaries (`RUNTIME_IMAGE`) is asked for with `image inspect` on the engine that
builds, and an answer other than "no such image" fails the run rather than counting
as absent; it must be there for the platform built for, because BuildKit resolves a
base of another platform from the registry - Docker is asked with `image inspect
--platform` when its CLI has it (an image store may hold several platforms), else the
one platform shown must be it, and a cross-architecture OFFLINE build on a Docker
that cannot be asked is refused with that reason. `IMAGE_SOURCE=image` for the nodes to pull is refused under `OFFLINE=yes`
unless `IMAGE` is in `K8S_REGISTRY`, the operator's own registry: the cluster would
otherwise fetch it from wherever `IMAGE` points, and an air-gapped cluster pulls only
from the operator's registry. Either way it is present and checked on the engine here
first.

`CE_INSTALL_DOCKER=yes` installs Docker CE from Docker's own repository on Debian,
Ubuntu and the RHEL family, and refuses an unknown distribution, a host that runs k3s
or Kubernetes (Docker sets the iptables `FORWARD` policy to `DROP`, which breaks pod
networking) and one with `podman-docker`, unless `CE_INSTALL_DOCKER_FORCE=yes`.
As built (PLAN 10.7): a server whose `docker --version` says `Docker version` is left
as it is. Otherwise the distribution is chosen by `ID` of `/etc/os-release` alone -
`debian` and `ubuntu` (by `VERSION_CODENAME`), `rhel`, and `rocky`, `almalinux` and
`centos` (Docker's `centos` repository); a derivative that only says `ID_LIKE` is
refused, and `CE_INSTALL_DOCKER_FORCE` overrides only the Kubernetes and
`podman-docker` refusals (an unknown distribution has no repository to install
from). Kubernetes counts as there when any of `/etc/rancher`, `/var/lib/rancher`,
`/etc/kubernetes`, `/var/lib/kubelet`, `/snap/microk8s`, `/var/snap/microk8s`,
`/var/lib/k0s`, `/etc/k0s` or a `k3s`, `k0s` or `kubelet` binary is (installed, the
stricter reading of "runs"), or `ps -e -o comm=` shows `kubelet`, `kubelite`, `k3s*`
or `k0s*` running (one plain question; `pgrep -x a|b` would not be plain, and a
server without `ps` is warned about and judged by its files); `podman-docker` when
its package is installed or `docker --version` names another program. The user is
root, or has `sudo -n` (asked with `sudo -n true`; refused otherwise); a user other
than root also needs `CE_DOCKER_GROUP=yes` (below). Every question comes before the
first change, the server's ports among them (`curl` there; a Debian or Ubuntu server
without curl yet is said, as the install brings it, and asked again by the install's
port check; on the RHEL family it is refused). What needs the engine - the
deployment's containers and volumes, other containers' ports, the engine's ID - can
only be asked once it is installed, and is, before anything of the deployment is
made. Docker's signing key is fetched over HTTPS by the server's curl (`-q
--proto =https --tlsv1.2`, never `-k`), as the user, into a directory of its own
(`mktemp -d`), and read there by the server's gpg with that directory as its home:
one primary key whose fingerprint is `9DC8 5822 9FC7 DD38 854A E2D8 8D81 803C 0EBF
CD88` for Debian and Ubuntu, `060A 61C5 1B55 8A7F 742B 77AA C52F EB6B 621E 9F35` for
the RHEL family, as Docker publishes them (confirmed against the keys
download.docker.com serves, 2026-11-22). Only then is it installed where the package
manager trusts it (`install -m 0644`, as root); another key is never put there - its
directory is removed and the run fails. The repository's definition is written by
`dd` from ssh's standard input, as Docker's pages write it: the deb822
`/etc/apt/sources.list.d/docker.sources` (Types, URIs, Suites, Components,
Architectures from `dpkg --print-architecture`, Signed-By), or the `docker-ce-stable`
repository of Docker's `docker-ce.repo` as `/etc/yum.repos.d/docker-ce.repo` with
`gpgkey=file://` the key checked (Docker's names the URL it was fetched from); then
`docker-ce`, `docker-ce-cli`, `containerd.io`, `docker-buildx-plugin` and
`docker-compose-plugin` are installed and the service enabled. `DOCKER_HOST=ssh://`
reaches the engine as the ssh user, which needs the group `docker` - root on that
server: a user other than root is added to it only with `CE_DOCKER_GROUP=yes`;
without it the install is refused before any change, naming the `usermod` command
(threat model, question 80).

### 13. Windows through WSL 2

On Windows the script runs in WSL 2 with Docker Desktop's WSL integration. There is no
PowerShell entry point, and Git Bash is not supported.

### 14. How it is tested

`make test-deploy DEPLOY_MODE=cluster` runs the lifecycle of a compose cluster (PLAN
10.8): install, the round trip, `kms.json` and compose's secret files checked, an
upgrade with the object read from whichever node answers all through it, `--status`,
uninstall, reinstall and the purges.
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
`kms.json` of `STATE_DIR` is the one the node uses; with `LIFECYCLE_SOURCE=binaries` it
deploys an image the script builds from `make image-binaries`, whose helper is the bare
runtime image, so the roots are proven on that path too (on Podman the secrets'
volumes are made owned by 65532, `--opt o=uid=65532,gid=65532`: Podman's `cp` into a
never-started container sets no owner on a volume's root). `shfmt` is a new build
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
was changed. Every ssh of the script also runs with `-o ConnectTimeout=20 -o
ControlPath=none` (no multiplexing: each connection is checked on its own).

As built for `CE_HOST` (PLAN 10.7), the pin is enforced by ssh itself, never read from
its messages (which the server can write to: the review of 10.7 found a pin parsed
from `ssh -v` forgeable by the remote command's standard error). At the first
contact the script's ssh runs `true` there against the user's known_hosts; `ssh -G`
says the name ssh looks the server up by (its `HostKeyAlias`, else its `HostName`,
with a port other than 22 as `[host]:port`) and which known_hosts files it reads;
`ssh-keygen -F <name> -f <file>` copies the lines that name it - hashed lines,
`@cert-authority` lines (host certificates) and `@revoked` lines as they are - into a
known_hosts of the run, and ssh must accept the server with that file alone
(`-o UserKnownHostsFile=<it> -o GlobalKnownHostsFile=none -o
StrictHostKeyChecking=yes`) before anything else runs there. The install keeps the
file as `STATE/ssh-known-hosts` (0600), written once `secrets_install` has made the
entry; every later run copies it into its own directory and every ssh to the server
is given it alone, so a key it does not hold is refused even when known_hosts accepts
it. To that copy each run adds the `@revoked` lines `ssh-keygen -F` finds for the
server in the user's known_hosts files, so a key revoked since the first install is
refused - and nothing else of them: a new key they accept is never taken, and the pin
in `STATE` is not changed. A pinned ssh is also given `-o VerifyHostKeyDNS=no -o
UpdateHostKeys=no -o CheckHostIP=no -o IgnoreUnknown=KnownHostsCommand -o
KnownHostsCommand=none` (the review of 10.7: a KnownHostsCommand or SSHFP records of
the user's configuration could otherwise accept a key the pin does not hold;
`IgnoreUnknown` keeps an ssh older than 8.5 from refusing the option), all of them
before any argument of its caller. The run's directory is refused when its path holds
a `:`, as it begins a PATH. The Docker CLI's ssh is held to the same: for `CE_HOST`, the PATH `tool` gives it
begins with the run's directory (0700, in `TMPDIR`, a plain path, removed by the
EXIT trap, which secrets.sh's trap carries while it is set) holding an `ssh` shim that
runs the real ssh - found by its absolute path before PATH is changed - with those
options and `ConnectTimeout=20 -o ControlPath=none` first, which win over the Docker
CLI's options and the user's configuration. A ProxyJump's own hop is checked by the
user's configuration; the key of the target behind it is the pinned one. `CE_HOST`
names no port: the Docker CLI reads it as the URL `ssh://<CE_HOST>`, so a `:` is
refused and a port goes in `~/.ssh/config`.

`CE_HOST`, `K3S_SSH` and `BUILD_HOST` are plain values (the `plain()` rule of
`scripts/remote.sh`: letters, digits and `_./=:@+,-`, not empty, no leading `-`), and
ssh is never asked to run a value that is not plain. `K3S_IMPORT` is the one value that
is a command: it is split into words at spaces, without `eval`; every word must be
plain except that words after the first may begin with `-`; a shell metacharacter
anywhere refuses it; the words are run as they are, with the image as a tar stream on
standard input. As built (PLAN 10.4): the first word may not hold `=` either - `env`,
under which every tool runs, would take it for a variable to set - and no word may be
`ssh` (by its last path component; `sudo ssh ...` too): a server reached over ssh is
`K3S_SSH`, whose host key the script checks first.

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

Defaults are applied until answered; the threat model has each, numbered 72 to 80.

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
9. **The group `docker` on a server Docker CE is installed on** (80, PLAN 10.7).
   Default: `CE_INSTALL_DOCKER=yes` with a user other than root is refused unless
   `CE_DOCKER_GROUP=yes` says to add that user to the group, which is root there.
