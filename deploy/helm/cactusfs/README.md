# The CactusFS Helm chart

Runs a CactusFS cluster, or one node on its own, on Kubernetes and k3s. The chart
depends on no ingress controller, storage driver or cloud. The reasons for its shape
are in `docs/adr/0015-helm-chart.md`.

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

The chart makes no Secret, no ServiceAccount and no Ingress. Pods run as user 65532
with a read-only root filesystem, every capability dropped, the runtime's default
seccomp profile and no service-account token, and are admitted under the `restricted`
Pod Security Standard.

The release name is used exactly as given: `helm install store ...` makes the nodes
`store-meta-0`, `store-meta-1`, `store-meta-2` and `store-data-0` with the default
pools. Across all pools 3 or 5 nodes vote (their `roles` are empty) and at least 4
store - or there is exactly one node, with every role.

## Values that must be set

Nothing renders until the Secrets the nodes need are named. You make them; cert-manager
can issue the certificates, and nothing requires it.

| Value | Secret |
|---|---|
| `secrets.root` | Root credentials: keys `access-key` and `secret-key` |
| `secrets.tls` | The certificate of the S3 and admin listeners (`kubernetes.io/tls`) |
| `secrets.nodeCA` | The CA of node certificates: key `ca.crt`. Not for a node on its own |
| `<node ID>-node-tls` | One for every node (`kubernetes.io/tls`); the suffix is `secrets.nodeTLSSuffix`. The certificate carries one DNS name, the node ID. A pod mounts its own and no other node's |

Keys are kept in a KMS; there is no unencrypted fallback.

| Value | |
|---|---|
| `kms.type: vault` | What a cluster uses. `kms.vault.addr`, `kms.vault.key` and `kms.vault.tokenSecret` (key `token`) are required, and `networkPolicy.egressTo` must say where Vault is, or the nodes could not reach it. `kms.vault.caSecret` (key `ca.crt`) when Vault's CA is not a public one |
| `kms.type: local` | A sealed key file, for a node on its own: `kms.local.keyFileSecret` (key `kms.json`) and `kms.local.passphraseSecret` (key `passphrase`). Several nodes sharing one file is a development arrangement and needs `dev.localKMS: true` |

The chart refuses to render what must not run: a number of voters other than 3 or 5,
fewer than four nodes that store, a shared local key file without `dev.localKMS`, Vault
with a NetworkPolicy that lets the nodes reach nothing, a missing Secret name.

`ci/` has value sets to start from: a cluster with Vault, five voters, one node, a
development cluster on one machine's disks, and a cluster opened to its clients.
`ci/refused/` has what the chart refuses, each with the words of the refusal.

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
| `serviceMonitor.enabled` | A ServiceMonitor that scrapes `/metrics` of the pods behind `<release>-admin`, over HTTPS on the admin port. The scrape job is named `cactusfs`, whatever the release is called |
| `prometheusRule.enabled` | A PrometheusRule with the alerting and recording rules of `deploy/observability/rules/`, which the chart carries in `files/`. They look for the job `cactusfs` |

`labels` on each is for the labels your Prometheus selects by. What a ServiceMonitor
needs, and the chart refuses to render it without:

- **A credential.** `/metrics` answers nobody without one. Make an API key for a user
  who is allowed `admin:GetMetrics` and nothing else, and a Secret in the release's
  namespace with one key whose value is `<access key ID>:<secret>`; name them in
  `serviceMonitor.bearerTokenSecret.name` and `.key`. The chart writes the name of the
  Secret into the ServiceMonitor, never the credential.
- **A name to check the certificate against.** Prometheus scrapes pods by their
  addresses, which no certificate names: `serviceMonitor.tlsConfig.serverName` is a
  name the certificate of `secrets.tls` carries. `tlsConfig.caSecret` (`name`, `key`)
  is the CA that signed it, when Prometheus does not trust it already.
  `tlsConfig.insecureSkipVerify: true` scrapes without checking the certificate, and
  hands the credential to whoever answers: for a test bench only.
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

Every node serves `/metrics`, but the admin Service is in front of the nodes with the
gateway role only: a pool whose `roles` leave out `gateway` is not scraped by this
ServiceMonitor.

Dashboards for Grafana are in `deploy/observability/dashboards/`; the chart does not
install them. `make observability-check` renders `ci/monitoring/` and asserts what this
section says; `make helm-lint` does not see that directory, because it validates every
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

## Checking the chart

```sh
make helm-lint
```

runs, in the builder image: `helm lint --strict`; a render of every value set of
`ci/`; `kubeconform -strict` on each render, against the Kubernetes schemas the builder
image carries - it is given no other place to look, so it reads nothing from the
network, and a kind without a schema fails; `scripts/helm-assert.sh` on each render, which
asserts what this document promises (the Services, the claims, the security context,
the probes, the placement, every rule of the NetworkPolicy, the drain delay); and a
render of every file of `ci/refused/`, which must fail with the words its first line
names. A new value set in `ci/` is picked up by its name.
