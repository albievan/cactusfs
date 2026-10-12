# ADR-0015: The Helm chart - a StatefulSet for every node

- **Status:** accepted; implemented (PLAN 7.1, 7.2, 7.6, 7.10)
- **Date:** 2026-10-11
- **Milestone:** 7
- **Builds on:** ADR-0005 (cluster, node identity), ADR-0006 (Vault), ADR-0014 (the
  default console user)

## Context

A CactusFS node has an identity for life: its ID, the certificate that carries that ID
as its one DNS name, its drives, and for three or five of the nodes a vote in the
metadata group. Kubernetes' StatefulSet gives pods stable names and volumes, but one
StatefulSet gives all its pods the same environment and the same mounted Secrets. Two
things do not fit that: "four nodes, three of them voters" (roles differ), and a node's
private key, which no other node should hold.

## Decision

- **Every node is a StatefulSet of one pod**, named `<release>-<pool>-<index>` with the
  release name exactly as given. That name is the node ID, the name in the node's
  certificate, and the prefix of its Secret (`<node ID>-node-tls`). A pod mounts its own
  key and nobody else's. Pools group nodes that share roles, topology labels and
  placement.
- **The chart makes no Secret.** Root credentials, the listener certificate, the CA of
  node certificates, each node's certificate and the Vault token are the operator's,
  named in values; nothing renders until they are. cert-manager can issue them; nothing
  requires it. The chart depends on no ingress controller, storage driver or cloud.
- **The chart refuses what must not run**: voters other than 3 or 5; fewer than four
  storing nodes; a sealed local key file shared by several nodes unless
  `dev.localKMS=true`; Vault with a NetworkPolicy that lets the nodes reach nothing;
  names Kubernetes or the node-ID rules would refuse.
- **Upgrades do not restart nodes** (`updateStrategy: OnDelete`). With a StatefulSet per
  node nothing orders their restarts, and a rolling update of each would be all of them
  at once. The operator deletes pods one at a time, in the order the README gives.
- **Default deny.** One NetworkPolicy selects every pod, for both directions: nodes
  reach each other on 9002 and 9003 and DNS; S3 (9000) and the admin port (9001) are
  opened separately, to peers the operator names, to nobody by default; anything else
  the nodes must reach (Vault, a trace collector) is named too.
- **The admin port has its own Service**, ClusterIP by default, so that S3 can be
  published without the console - on a new deployment the console has a user whose
  password is public (ADR-0014).
- **Probes** are HTTPS `httpGet` on `/healthz` and `/readyz` of the admin port, which
  every role serves and which open before the node has joined (PLAN 7.0): a waiting
  member is alive and not ready. The drain delay is set to outlast the readiness probe.
- **`localVolumes`** offers static volumes on one named machine's disks, for a cluster
  with no storage driver. Not for production: all drives are then on one machine.

## Consequences

- Scaling a pool adds StatefulSets, not replicas; the cluster's voters and storage are
  in any case fixed when it forms (ARCHITECTURE).
- An upgrade is a procedure, not a command. Until a controller does it (it needs access
  to the Kubernetes API, which the pods deliberately do not have) the order is the
  operator's to keep.
- The PodDisruptionBudget (one node at a time) governs evictions - drains of machines -
  across all the StatefulSets.

## Proved, and not

Deployed by hand on a single-machine k3s under the `restricted` Pod Security Standard:
formation, an S3 round trip, a pod deleted and returned under load, the policy refusing
another pod on the node ports. Not yet: validation of the rendered manifests against the
Kubernetes schemas in `make helm-lint`; a test in the repository that installs the chart
(PLAN 7.10); Vault under the NetworkPolicy; more than one machine.

Since then (2026-10-12): the first two exist - `make helm-lint` validates every render
with `kubeconform`, and `make test-k8s` installs the chart on k3s (PLAN 7.1, 7.10).
Vault under the NetworkPolicy and more than one machine are still not proved by a test
of the repository.

## Alternatives

- **One StatefulSet per pool, one Secret with every node's key, mounted by `subPath`** -
  fewer objects, but every pod's volume holds every key on the kubelet, and `subPath`
  mounts are never refreshed, which rules out rotating certificates without a restart.
- **An operator** - the right home for ordered upgrades and for issuing certificates;
  much more to build and to trust, and not needed to run a cluster.
