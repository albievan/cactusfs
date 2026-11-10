# The CactusFS Helm chart

Runs a CactusFS cluster, or one node on its own, on Kubernetes (1.27 or later) and k3s.
The chart depends on no ingress controller, storage driver or cloud. The reasons for its
shape are in [`docs/adr/0015-helm-chart.md`](../../../docs/adr/0015-helm-chart.md).

**No release has been published yet**: there is no image at `ghcr.io/albievan/cactusfs`
and no chart at `oci://ghcr.io/albievan/charts` to pull. Until the first release, install
the chart from this directory with an image you built ([Installing](#installing)).

## What it installs

| Resource | Name | What it is for |
|---|---|---|
| StatefulSet, one for every node | `<release>-<pool>-<index>` | A node is one pod, for life. Its name is its node ID and the one name in its certificate. A claim for its metadata, and one for each drive when it stores |
| Service, headless | `<release>-nodes` | How the nodes find each other on 9002 (node RPC) and 9003 (Raft). It publishes addresses that are not ready: a node must reach its peers in order to become ready |
| Service | `<release>` | The S3 API (9000) of the nodes with the gateway role |
| Service | `<release>-admin` | The management API, Swagger UI and the console (9001), apart from S3 |
| PodDisruptionBudget | `<release>` | One node at a time may be evicted. Not rendered for a node on its own |
| NetworkPolicy | `<release>` | Default deny for every pod, in and out, with the exceptions below |
| StorageClass, PersistentVolumes | | Only with `localVolumes.enabled`: volumes on one named machine's disks, for a cluster with no storage driver. Not for production |
| ServiceAccount, Role, RoleBinding | `<release>-upgrade` | Only with `upgrade.fromConsole.enabled`: the identity with which the nodes move themselves to a newer image ([Upgrading from the console](#upgrading-from-the-console)) |

The chart makes no Secret and no Ingress, and no ServiceAccount unless upgrades from
the console are turned on. Pods run as user 65532 with a read-only root filesystem,
every capability dropped, the runtime's default seccomp profile and no service-account
token (with upgrades from the console, the cactusfs container alone is given one of its
own, below), and are admitted under the `restricted` Pod Security Standard.

The release name is used exactly as given: `helm install store ...` makes the nodes
`store-meta-0`, `store-meta-1`, `store-meta-2` and `store-data-0` with the default
pools. Across all pools 3 or 5 nodes vote (their `roles` are empty, which is every
role, or name `meta`) and at least 4 store - or there is exactly one node, with every
role.

## The image

| Value | |
|---|---|
| `image.repository` | `ghcr.io/albievan/cactusfs`, where releases will be published (none is yet). An image you built (`make image`) and pushed to a registry of your own, or loaded into the cluster, is named here |
| `image.tag` | The build to run, `<version>-<build>`. Empty: the chart's `appVersion`, the release |
| `image.digest` | `sha256:...`, to run exactly that image; it wins over the tag. Verify a release's digest and deploy that digest (the README of the repository, "Verifying an image") |

Pods are labelled `app.kubernetes.io/version` with the tag, or with the `appVersion`
when there is none.

## Values that must be set

Nothing renders until the Secrets the nodes need are named. You make them; cert-manager
can issue the certificates, and nothing requires it.

| Value | Secret |
|---|---|
| `secrets.root` | Root credentials: keys `access-key` and `secret-key` |
| `secrets.tls` | The certificate of the S3 and admin listeners (`kubernetes.io/tls`: keys `tls.crt` and `tls.key`) |
| `secrets.nodeCA` | The CA of node certificates: key `ca.crt`. Not for a node on its own |
| `<node ID>-node-tls` | One for every node (`kubernetes.io/tls`); the suffix is `secrets.nodeTLSSuffix`. The certificate carries one DNS name, the node ID, and no IP address. A pod mounts its own and no other node's. Not for a node on its own |

The root credentials are an access key ID of at least 3 characters and a secret key of
at least 16. They are what the deployment begins with: once it exists, a pair that
differs from the stored one stops the node.

## Installing

With the default pools and a release named `store`, the nodes are `store-meta-0`,
`store-meta-1`, `store-meta-2` and `store-data-0`. The names of the Secrets are yours
to choose; the files are your own PKI's.

```sh
kubectl create namespace cactusfs
kubectl label namespace cactusfs pod-security.kubernetes.io/enforce=restricted   # optional: the pods pass it

kubectl -n cactusfs create secret generic cactusfs-root \
  --from-literal=access-key='<access key ID>' --from-literal=secret-key='<secret key>'
kubectl -n cactusfs create secret tls cactusfs-tls --cert=s3.pem --key=s3.key
kubectl -n cactusfs create secret generic cactusfs-node-ca --from-file=ca.crt=node-ca.pem
for node in store-meta-0 store-meta-1 store-meta-2 store-data-0; do
  kubectl -n cactusfs create secret tls "$node-node-tls" --cert="$node.pem" --key="$node.key"
done
kubectl -n cactusfs create secret generic cactusfs-vault-token --from-file=token=vault-token   # with kms.type=vault

helm install store deploy/helm/cactusfs -n cactusfs -f my-values.yaml \
  --set image.repository=registry.example.test/cactusfs --set image.tag=<version>-<build>
kubectl -n cactusfs get pods        # alive at once, ready when the cluster has formed
```

`my-values.yaml` names those Secrets and says where the keys are; `ci/cluster-values.yaml`
is the least it can hold. From the first release on, the chart can be installed from the
registry instead, with the published image by its digest (the README of the repository,
"Releases"):

```sh
helm install store oci://ghcr.io/albievan/charts/cactusfs --version <version> \
  -n cactusfs -f my-values.yaml --set image.digest=sha256:<digest of the manifest list>
```

A new deployment has the console user `admin` with the password `admin`
([ADR-0014](../../../docs/adr/0014-default-console-user.md)); see "Who may reach it"
before you open the admin port.

**Rotating certificates.** Update the Secret - `secrets.tls`, or a node's
`<node ID>-node-tls` - and do nothing else: no pod restarts (ADR-0020). The kubelet
brings the new files into the pod, which can take a minute or more; the node reads its
certificate files again every minute (`CACTUSFS_TLS_RELOAD_INTERVAL`) and uses a new
pair for new connections. Connections that are open stay open, and the metadata group
keeps its leader. A node certificate must name the same node and come from the CA in
`secrets.nodeCA`; a pair that is not valid is refused, the node logs why at `ERROR`
(`the certificate on disk is refused; the one in use stays`) and goes on with the old
one - so renew with more margin than those two delays, and watch the log or the alert
`CactusFSCertificateReloadRefused` (`cactusfs_tls_reload_pending` has been 1 for thirty
minutes). The Secret is
also what a pod reads when it next starts: a refused node certificate left in it stops
that start.

Rotation is not revocation: a replaced certificate and its key remain accepted by peers
until that certificate expires; for a compromised key, rotate the CA (below) or remove
the node.

The CA in `secrets.nodeCA` is **not** read again: trust changes with a restart. To
replace the CA, (1) put both CA certificates, old and new, in `ca.crt` and restart the
pods one StatefulSet at a time; (2) replace every node certificate by one of the new
CA, as above, without restarts; (3) put the new CA alone in `ca.crt` and restart once
more.

Keys are kept in a KMS; there is no unencrypted fallback.

| Value | |
|---|---|
| `kms.type: vault` | What a cluster uses. `kms.vault.addr`, `kms.vault.key` and `kms.vault.tokenSecret` (key `token`) are required, and `networkPolicy.egressTo` must say where Vault is, or the nodes could not reach it. `kms.vault.caSecret` (key `ca.crt`) when Vault's CA is not a public one |
| `kms.type: local` | A sealed key file, for a node on its own: `kms.local.keyFileSecret` (key `kms.json`) and `kms.local.passphraseSecret` (key `passphrase`). Several nodes sharing one file is a development arrangement and needs `dev.localKMS: true` |

The chart refuses to render what must not run: a number of voters other than 3 or 5,
fewer than four nodes that store, a shared local key file without `dev.localKMS`, Vault
with a NetworkPolicy that lets the nodes reach nothing, a missing Secret name.

`ci/` has value sets to start from: a cluster with Vault (`cluster-values.yaml`), five
voters (`five-voters-values.yaml`), one node (`standalone-values.yaml`), a development
cluster on one machine's disks (`single-machine-values.yaml`), a cluster opened to
its clients (`exposed-values.yaml`), and a cluster the console may upgrade
(`upgrade-values.yaml`). `ci/refused/` has what the chart refuses, each with
the words of the refusal.

## Other values

| Value | |
|---|---|
| `pools` | The nodes: `name`, `replicas`, `roles` (empty for every role; or names separated by commas - `gateway`, `meta`, `storage`, `console`), `topology` (`region`, `zone`, `rack`, for the placement of shards), `nodeSelector`, `tolerations` |
| `drives.count`, `drives.size` | The drives of every node that stores: one claim each (default 2 of `100Gi`) |
| `metadata.size` | The claim of a node's metadata (default `10Gi`) |
| `storageClassName` | The storage class of the claims. Empty: the cluster's default |
| `localVolumes` | `enabled`, `basePath`, `nodeName`: volumes on the disks of one named machine, for a cluster without a storage driver. The directories `<basePath>/<node ID>/{meta,data1,...}` must exist and belong to user 65532. Not for production |
| `s3.domain`, `s3.region` | The domain of virtual-hosted-style requests (empty: path-style only); the region reported to S3 clients (`us-east-1`) |
| `resources` | Requests and limits of a node (default: 500m and 1Gi requested, 4 CPUs and 4Gi at most) |
| `probes`, `shutdownTimeoutSeconds` | The period, threshold and timeout of the probes, and how long a node may take to start answering (`startupSeconds`); how long requests in flight get after the drain delay |
| `spread` | `topologyKey` and `whenUnsatisfiable`: pods are kept apart by host; `ScheduleAnyway` lets them share one machine |
| `podDisruptionBudget.maxUnavailable` | 1: one node at a time |
| `logLevel`, `otlpEndpoint`, `extraEnv` | The log level; the URL of a trace collector (name it in `networkPolicy.egressTo` as well); further `CACTUSFS_*` variables for every node |
| `podAnnotations`, `podLabels`, `imagePullSecrets`, `clusterDomain` | As their names say |

Every setting of a node that the chart does not set is listed in the README of the
repository ("Settings"); `extraEnv` passes one.

## Who may reach it

Both Services are `ClusterIP` unless you say otherwise (`service.type`,
`adminService.type`, with `nodePort` and `annotations` for each); how they are
published is yours to choose. The NetworkPolicy then decides who gets through, and by
default nobody does:

| Value | Opens |
|---|---|
| `networkPolicy.clientsFrom` | Port 9000 (S3) to the peers listed: `podSelector`, `namespaceSelector`, `ipBlock`. Empty: nobody |
| `networkPolicy.adminFrom` | Port 9001 (admin API, console) to the peers listed, apart from S3. Empty: nobody |
| `networkPolicy.dns` | Where the pods may ask for names (port 53) |
| `networkPolicy.egressTo` | What else the pods may reach, as peers and ports: Vault, a collector of traces (`otlpEndpoint`) |

Ports 9002 and 9003 are open between the chart's own pods and to nobody else. Three
things to know:

- A new deployment has the console user `admin` with the password `admin`. Change the
  password or delete the user before `adminFrom` or `adminService` opens the admin port
  to anybody.
- A Service of type NodePort answers on every address of the machine, and on many
  hosts past the host's own firewall. The admin port has a Service of its own so that
  S3 can be published without it.
- A NetworkPolicy binds only where the cluster's network plugin enforces it.

## Monitoring

Two resources for the Prometheus Operator, both off by default; the chart needs no
operator, and with them off renders nothing of its kinds.

| Value | Renders |
|---|---|
| `podMonitor.enabled` | A PodMonitor that scrapes `/metrics` of every pod of the release, over HTTPS on the admin port, every `podMonitor.interval` (30s) with a `scrapeTimeout` of 10s. The scrape job is named `cactusfs`, whatever the release is called |
| `prometheusRule.enabled` | A PrometheusRule with the alerting and recording rules of `deploy/observability/rules/`, which the chart carries in `files/`. They look for the job `cactusfs` |

`labels` on each is for the labels your Prometheus selects by. What the PodMonitor
needs, and the chart refuses to render it without:

- **A credential.** `/metrics` answers nobody without one. Make an API key for a user
  who is allowed `admin:GetMetrics` and nothing else, and a Secret in the release's
  namespace with one key whose value is `<access key ID>:<secret>`; name them in
  `podMonitor.bearerTokenSecret.name` and `.key`. The chart writes the name of the
  Secret into the PodMonitor, never the credential.
- **A name to check the certificate against.** Prometheus scrapes pods by their
  addresses, which no certificate names: `podMonitor.tlsConfig.serverName` is a
  name the certificate of `secrets.tls` carries. `tlsConfig.caSecret` (`name`, `key`)
  is the CA that signed it, when Prometheus does not trust it already.
  The certificate is always checked: the chart has no value that scrapes without, since
  the credential would then go to whoever answers, and refuses a
  `tlsConfig.insecureSkipVerify` it is given.
- **A way in.** The NetworkPolicy lets nobody reach port 9001 until
  `networkPolicy.adminFrom` names them, and Prometheus is one of them:

  ```yaml
  networkPolicy:
    adminFrom:
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: monitoring}
        podSelector:
          matchLabels: {app.kubernetes.io/name: prometheus}
  ```

  That opens the whole admin port to Prometheus's pods, the console included; there is
  no rule for `/metrics` alone, since a NetworkPolicy knows ports and not paths.

Every node serves `/metrics` on its admin port, and every node is scraped: the pods are
selected, not a Service. The admin Service is in front of the nodes with the gateway
role only, and the nodes that vote in the metadata group and hold the drives need not
be gateways - their gauges (the Raft leader and term, the drives, the node
certificate) are what most of the alerts read. No Service changes for it, and the
NetworkPolicy does not either: `adminFrom` already governs port 9001 of every pod.

A pod is scraped from the moment it runs. Until a node has started - a member waits
for the others of its cluster - it refuses everything but its probes, `/metrics`
included, since nobody can be authenticated yet: Prometheus shows it as not answering
(`up == 0`), which is what it is.

Dashboards for Grafana are in `deploy/observability/dashboards/`; the chart does not
install them. `make observability-check` renders `ci/monitoring/` - a cluster whose
voters are no gateways - and asserts what this section says; `make helm-lint` does not see that directory, because it validates every
kind against the schemas of Kubernetes itself and these two are the operator's.

## How nodes roll

`helm upgrade` changes what the nodes are to be and restarts none of them: every
StatefulSet has `updateStrategy: OnDelete`. Each node is a StatefulSet of its own, so
nothing orders their restarts, and a rolling update of each would be all of them at
once - the metadata group would lose its quorum and the erasure sets their drives. A
change reaches a node when its pod is deleted. Delete one pod at a time - the nodes
that do not vote first, then the voters, the one that leads the metadata group last -
and wait for each to be ready again before the next:

```sh
kubectl -n <namespace> delete pod <node ID>-0
kubectl -n <namespace> wait --for=condition=Ready pod/<node ID>-0 --timeout=5m
```

A pod that is told to stop keeps serving for the drain delay - a little longer than
the readiness probe takes to fail (`probes.periodSeconds` x `probes.failureThreshold`),
so that the Services have stopped sending to it - then lets requests in flight finish
within `shutdownTimeoutSeconds`. The pod's termination grace period is longer than the
two together. There is no `preStop` hook: the node does this itself.

**Going back after an upgrade.** While nodes of two releases run side by side, no node
writes a metadata entry newer than the oldest of them understands, and a node can be
taken back to the release before. When the last node runs a release that raises the
entry schema, the cluster begins to write the new schema by itself, and the first such
write ends the way back: nodes of the release before refuse that state and do not
start. From schema 3 that write is, in practice, the first upload, the first flushed
count of downloads or the first bucket created; from schema 4 to 5 (this release) it is
the first bucket created after the last node was upgraded. Nothing is lost, but it is an
outage until they are rolled forward again
([ADR-0016](../../../docs/adr/0016-object-access-and-schema-3.md),
[ADR-0019](../../../docs/adr/0019-bucket-activity-and-schema-4.md),
[ADR-0023](../../../docs/adr/0023-schema-5-usage-outbox-targets.md)). To keep a way
back, hold one node at the release before until you are sure, and take a backup first.
Once every node runs this release, go back - while no bucket has been created since -
only by stopping **every** node first and then starting all of them on the release
before: never a node at a time. A node taken back alone is still counted at schema 5
until it registers again, and one bucket created meanwhile stops it at that entry and
ends the way back for the whole cluster; it recovers when it is started on this release
again. The [README](../../../README.md#on-kubernetes) says more.
`kubectl -n <namespace> get pods -L app.kubernetes.io/version` shows which node runs
what; the alert `CactusFSMetadataUpgradeUnfinished` fires when, for six hours, the
cluster has written an older schema than its nodes support - an upgrade that was begun
and not finished.

## Upgrading from the console

Off by default, and two opt-ins (ADR-0029,
[`docs/adr/0029-updates-and-upgrades.md`](../../../docs/adr/0029-updates-and-upgrades.md)):
the nodes check for releases only once `CACTUSFS_UPDATE_SOURCE` is set, and they install
one only with `upgrade.fromConsole.enabled`. Turned on, the console of a `cluster-admin`
who types their password again may move every node to a newer release's image - only a
release signed by this project's release workflow for that version, and only a newer
patch of the running minor or the newest patch of the next one. Whoever may do so
chooses the code the cluster runs; turn it on only where that is wanted, and read
[what the token can reach](#what-the-token-can-reach) first.

**The update source.** The chart has no value of its own for it: set it through
`extraEnv`, and let the pods reach it with a rule of `networkPolicy.egressTo` on 443 - a
NetworkPolicy names addresses, not hosts. `extraEnv` and `egressTo` are lists, so give
the whole list (with Vault's rule, if `egressTo` has one), in a values file:

```yaml
extraEnv:
  - {name: CACTUSFS_UPDATE_SOURCE, value: "https://releases.example.internal/repos/albievan/cactusfs/releases"}
networkPolicy:
  egressTo:
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: vault}}}]
      ports: [8200]
    - to: [{ipBlock: {cidr: 192.0.2.30/32}}]   # the mirror's address
      ports: [443]
upgrade:
  fromConsole:
    enabled: true
    apiServer: {cidrs: [192.0.2.10/32], port: 6443}
```

```sh
helm upgrade store deploy/helm/cactusfs -n cactusfs --reuse-values -f updates.yaml
```

GitHub's own source is `https://api.github.com/repos/albievan/cactusfs/releases`; its
files are fetched from `github.com` and redirected to GitHub's storage, on addresses
GitHub changes, so a rule for it is a wide range that must be kept current
(`https://api.github.com/meta` publishes GitHub's ranges). A mirror inside the network,
with `update-ca` if its certificate is of a private CA, is the dependable choice (the
[README](../../../README.md#checking-for-releases-and-upgrading) says how): put the
CA's certificate in a Secret under the key `ca.crt` and name it in
`updates.caSecret`. `ci/upgrade-values.yaml` is such a set of values, and
`helm-assert` holds that the source's port has a rule.

| Value | |
|---|---|
| `updates.caSecret` | A Secret with the key `ca.crt`: the CAs the source's certificate is verified against (`update-ca`), mounted read-only into the cactusfs container. Empty: the system's CAs. Only with `CACTUSFS_UPDATE_SOURCE` in `extraEnv`; the chart refuses to render without, as a node would refuse to start, and refuses `CACTUSFS_UPDATE_CA` in `extraEnv` beside it |
| `updates.trustedRootSecret` | A Secret with the key `trusted_root.json`: the Sigstore trusted root releases are verified against instead of the binary's (`update-trusted-root`), for a deployment that was offline across a rotation of Sigstore's keys. Empty: the binary's. Only with `CACTUSFS_UPDATE_SOURCE`, as above, and not beside `CACTUSFS_UPDATE_TRUSTED_ROOT` in `extraEnv` |
| `upgrade.fromConsole.enabled` | `false`. Renders what follows, and sets `CACTUSFS_UPGRADE_MODE=kubernetes` on every node |
| `upgrade.fromConsole.apiServer.cidrs` | The addresses at which the pods reach the API server, for the NetworkPolicy: the endpoints of the Service `kubernetes` in the namespace `default`, as `kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes` lists them, each as `<address>/32`. A NetworkPolicy sees the address after the Service's translation, so the Service's own address does not do. Required when `networkPolicy.enabled`; the chart refuses to render without |
| `upgrade.fromConsole.apiServer.port` | Their port: `6443` (k3s, kubeadm) |

What it renders, and nothing more:

- **A ServiceAccount, a Role and a RoleBinding**, `<release>-upgrade`. The Role may
  `get` and `patch` the nodes' StatefulSets, `get` their pods and `create` their
  evictions - each by name (`resourceNames`). It names no other resource; but see
  [what the token can reach](#what-the-token-can-reach): patching a StatefulSet is
  more than it looks.
- **The token on the cactusfs container only.** The pods name the ServiceAccount but
  keep `automountServiceAccountToken: false`; a projected volume, `kube-api`, mounted
  read-only at `/var/run/secrets/cactusfs.io/kubernetes` into the cactusfs container
  alone, holds a token that expires within the hour (the kubelet renews it), the API
  server's CA (`kube-root-ca.crt`) and the namespace. The node reads the token again
  for every request, trusts that CA alone, and never logs it.
- **A way to the API server**: one egress rule of the NetworkPolicy, to the addresses
  and port above.

How a node is moved: its StatefulSet's image becomes `<the repository it runs>@<the
digest of the release's image list>` and its pods' `app.kubernetes.io/version` the
release; its pod is evicted, so the PodDisruptionBudget is respected; and the next node
is not touched until the pod is ready on the new image, the node reports the release,
and the cluster is ready. The nodes go one at a time, the node that runs the upgrade
last. The first node that does not come up is put back to the image it ran and evicted
again, and the upgrade stops there. The upgrade is held by the annotation
`cactusfs.io/upgrade` on the StatefulSet of the first node by name, and its outcome is
`cactusfs.io/upgrade-result`; each moved StatefulSet keeps what it ran before in
`cactusfs.io/upgrade-previous`. The node that runs the upgrade is moved last, so its own
pod is evicted before it could write the outcome: once every node runs the release,
that node, restarted, replaces the lock with the outcome `done` as it starts (or when
the console reads the progress from it), under the StatefulSet's `resourceVersion`;
any node does so once the lock is stale. Another node reading the progress meanwhile
says `done` and writes nothing. A lock whose node stops answering is taken over
after twice a node's time (10 minutes each); a run that finds its lock taken over
stops and puts nothing back. An upgrade that was cut off is resumed by starting it
again: the nodes already moved are skipped.

**Helm drift.** The console changes the StatefulSets behind Helm's back. The next
`helm upgrade` renders the image of its own values again and, with `OnDelete`, puts it
in the StatefulSets, where it waits until a pod is next deleted - which may take a node
back to the release before. After an upgrade from the console, every `helm upgrade`
must name the installed digest, or a newer one:

```sh
helm upgrade store deploy/helm/cactusfs -n cactusfs --reuse-values \
  --set image.digest=sha256:<the digest the console installed>
```

The console shows that command with the digest it installed, and the audit trail
records it.

### What the token can reach

`patch` of a StatefulSet changes its pod template, and Kubernetes then makes the pod
that template says. That is as good as creating **any pod in the namespace**: a pod that
mounts every Secret of the namespace - the Vault token, the root credentials, the TLS
keys and the node CA's key - that runs as any ServiceAccount of the namespace, with any
image. `resourceNames` limits which StatefulSets may be patched, not what their template
may say; the `restricted` Pod Security Standard limits a pod's privileges, not which
Secrets or ServiceAccounts it uses. So whoever holds the token - someone who has taken
over a gateway, or read the token from its pod - holds the namespace and its secrets.

- Install the chart in **a namespace of its own**, with no other workload's Secrets or
  ServiceAccounts in it.
- What would close it is a ValidatingAdmissionPolicy that lets this ServiceAccount's
  patches change only the cactusfs container's image (to an `@sha256:` digest), the
  label `app.kubernetes.io/version` and annotations `cactusfs.io/*`, and nothing else.
  The chart does not render one: it is a residual risk, open question 10 of ADR-0029.

## Checking the chart

```sh
make helm-lint
```

runs, in the builder image: `helm lint --strict`; a render of every value set of
`ci/`; `kubeconform -strict` on each render, against the Kubernetes schemas the builder
image carries - it is given no other place to look, so it reads nothing from the
network, and a kind without a schema fails; `scripts/helm-assert.sh` on each render, which
asserts what this document promises (the Services, the claims, the security context,
the probes, the placement, every rule of the NetworkPolicy, the drain delay, the image
and the version label, and the identity of upgrades from the console: exactly its
rules and the token on the cactusfs container with it, nothing of it without); and a
render of every file of `ci/refused/`, which must fail with the words its first line
names. A new value set in `ci/` is picked up by its name. Without a chart, without a
value set or without a refusal to ask for, the gate fails.

`make test-k8s` (`test/k8s`) installs the chart on k3s in one privileged container, with
`ci/single-machine-values.yaml` and the image the tree builds, in a namespace that enforces
the `restricted` standard, and holds the running cluster to this document - an upgrade from
the console included: turned on with `helm upgrade`, four nodes moved to a second image one
at a time, and an image that cannot be pulled put back. It is a CI job of its own and no
part of `make check`.
