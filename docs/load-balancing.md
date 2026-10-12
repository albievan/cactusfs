# Putting a load balancer in front of CactusFS

For an operator who knows load balancers and does not know CactusFS yet. It covers the
development cluster of compose (Docker Desktop, Podman), a cluster of the single binary
on virtual machines, and the Helm chart on k3s or Kubernetes.

Example configurations are in
[`deploy/examples/loadbalancer/`](../deploy/examples/loadbalancer/README.md). They are
examples: none of them is run by the project's gates, every name and address in them is
a placeholder, and each is to be adapted and tested before it carries traffic.

## Contents

1. [What a balancer must and must not do](#1-what-a-balancer-must-and-must-not-do)
2. [Docker Desktop (compose)](#2-docker-desktop-compose)
3. [A cluster on virtual machines](#3-a-cluster-on-virtual-machines)
4. [k3s and Kubernetes](#4-k3s-and-kubernetes)
5. [Verifying](#5-verifying)
6. [Limits and gaps today](#6-limits-and-gaps-today)

## 1. What a balancer must and must not do

### What it is in front of

Every node is the same binary; `--roles` says what it does. Only two of its four ports
are for clients.

| Port | Setting | Who uses it | Behind a balancer |
|---|---|---|---|
| 9000 | `--s3-addr` | S3 clients. Open on nodes with the `gateway` role only | Yes |
| 9001 | `--admin-addr` | Probes (`/healthz`, `/readyz`) and `/metrics` on **every** node; the management API (`/api/v1`) and Swagger UI (`/docs`) on nodes with the `gateway` role; the console (`/`) on nodes that also have the `console` role | Yes, apart from S3 and restricted |
| 9002 | `--rpc-addr` | Node to node: gRPC over mutual TLS 1.3 | **Never** |
| 9003 | `--raft-addr` | Node to node: the Raft transport, mutual TLS 1.3 | **Never** |

Ports 9002 and 9003 are addressed node by node (`--peers`, `--advertise-rpc`,
`--advertise-raft`) and authenticated by each node's own certificate. A balancer in
front of them breaks the cluster; a firewall that lets anybody but the nodes reach them
is a hole. All four port numbers are settings; the tables here use the defaults.

Gateways hold no state a balancer has to respect:

| What | Where it lives | Consequence |
|---|---|---|
| Console sessions and their CSRF tokens | The replicated metadata (Raft); reads are linearizable on every node ([ADR-0005](adr/0005-cluster.md)) | **No sticky sessions.** A cookie issued through one node is accepted by every other, at once |
| `Idempotency-Key` records of the management API | The replicated metadata | A repeated request may land on another node |
| Multipart uploads: the upload and its parts | The replicated metadata ([ADR-0012](adr/0012-multipart-uploads.md)) | Parts of one upload may go to different nodes, and so may its completion |
| Download counters | Each node's memory, written to the metadata every `--access-flush-interval` (30 s) | Nothing to do; the counts add up |
| Bucket usage figures of the console | Each node's memory, for 30 s | Two reloads through a balancer can show counts up to 30 s apart |
| The limit on failed console sign-ins | Each node's memory, **by source address** | See [the source address](#the-source-address-the-nodes-see) |

### The checklist

1. **Balance 9000 and 9001 only**, as two endpoints. Never 9002 or 9003.
2. **Send S3 to nodes with the `gateway` role**, and the admin endpoint to nodes with
   `gateway` and `console`. A node without `gateway` has no S3 listener.
3. **Never speak plaintext to a node.** The listeners serve TLS 1.2 or later and
   nothing else; `--dev-plaintext` binds to a loopback address only and is for
   development. There are two sound designs:
   - **TLS passthrough (layer 4).** The balancer forwards TCP; the node ends the
     client's TLS session with its own certificate. Simplest, nothing is decrypted on
     the way, and nothing of a request can be altered. Prefer it.
   - **TLS ended at the balancer and begun again (layer 7).** The balancer holds the
     public certificate and connects to the nodes over TLS, **verifying** their
     certificate against its CA. Only when the balancer has to hold the certificate.
4. **Leave the request as the client signed it.** SigV4 covers the `Host` header, the
   path and the query string, in headers and in presigned URLs alike:
   - pass `Host` through unchanged, port included (`s3.example.com:9443` is not
     `s3.example.com`);
   - do not rewrite, decode, re-encode or normalise the path or the query (no merging
     of slashes, no path prefix, no added parameters);
   - add no header whose name begins with `x-amz-`, and no `Content-MD5`: a request
     that carries one the client did not sign is refused.

   A passthrough balancer cannot break any of this. A layer 7 one can.
5. **Do not buffer bodies, and do not cap their size.** A single `PUT` and a single
   part may be 5 GiB (the server refuses more); clients send `Expect: 100-continue`
   and streaming uploads (`aws-chunked`, with signed chunks or a trailing checksum)
   whose framing must arrive byte for byte.
6. **No short timeouts.** The S3 listener puts no limit on how long a transfer takes.
   Idle timeouts of the balancer are measured in minutes, not seconds: a client waits
   in silence while a large multipart upload is completed.
7. **Health check: `GET /readyz` over HTTPS on port 9001**, without credentials, and
   expect 200. **Port 9000 has no health endpoint** - a balancer for S3 checks 9001 of
   the same node. See [health checks](#health-checks-and-draining).
8. **Check faster than the nodes drain**: interval x failures-to-mark-down must be
   shorter than `--drain-delay`, whose default is `0s`. Set it.
9. **No stickiness**, for S3 or for the console (the table above).
10. **Prefer least-connections to round-robin.** A 5 GiB upload and a `HEAD` are both
    one request; counting connections spreads the long ones.
11. **Names and certificates.** One DNS name for S3, pointing at the balancer; for
    virtual-hosted-style requests also the wildcard `*.<name>`, and `--s3-domain <name>`
    on every node. With passthrough, the certificate **on the nodes** carries those
    names; all nodes can share one certificate.
12. **Keep the admin endpoint apart and closed.** A new deployment has the console
    user `admin` with the password `admin`
    ([ADR-0014](adr/0014-default-console-user.md)). Change the password or delete the
    user **before** a balancer makes port 9001 reachable, and let only the
    administrators' network reach it afterwards.
13. **Know what the audit trail will say**: the balancer's address, unless the balancer
    preserves the client's at layer 4. Next section.

### The source address the nodes see

A node takes the address of the TCP connection as the client's. It does **not** read
`X-Forwarded-For`, `Forwarded` or `X-Real-IP`, has no setting to trust a proxy, and does
not speak the PROXY protocol. Behind a balancer that opens its own connections - every
layer 7 balancer, and HAProxy or nginx in TCP mode - the nodes therefore see the
balancer. That address is used in four places:

| Where | Effect behind such a balancer |
|---|---|
| Audit records (failed authentication, denials, admin changes) | Every record names the balancer. Keep the balancer's own log, with timestamps, to tell clients apart |
| `aws:SourceIp` conditions in policies | They test the balancer's address: a policy that allows a client network no longer matches, and one that allows the balancer allows everybody behind it |
| The limit on failed console sign-ins (10 in 5 minutes per address, per node) | All users share one allowance: a few people mistyping, or one person guessing, makes everybody wait (`429`). It also means the limit does not tell attackers apart |
| The node's log (`remote` of a failed authentication) | The balancer |

Do not enable the PROXY protocol towards the nodes (`send-proxy` in HAProxy,
`proxy_protocol on` in nginx): the node reads the first bytes as a TLS handshake and
every connection fails.

What does keep the client's address is a balancer that forwards packets without
opening a connection of its own: a cloud network load balancer with client address
preservation, a Kubernetes Service with `externalTrafficPolicy: Local`, direct server
return. Where the address matters - `aws:SourceIp` policies above all - use one of
those, and test that the audit trail shows what you expect.

`aws:SecureTransport` is true in both designs: the node is always reached over TLS.

### Health checks and draining

| Endpoint | Port | Answers |
|---|---|---|
| `GET /healthz` | 9001, HTTPS, no credentials | 200 `{"status":"ok"}` whenever the process can answer HTTP. For liveness; a balancer has no use for it |
| `GET /readyz` | 9001, HTTPS, no credentials | 200 when the node should get traffic. 503 `{"status":"unavailable"}` while it starts or waits for the other members, while the metadata group has no leader this node knows of (a node cut off from the others), and from the moment it is told to stop |

The answer never says why; the node's log does. While a node starts, its listeners are
already open: port 9000 answers every request with an S3 `503` and port 9001 serves the
probes and refuses the rest. A check that only connects to the port would call that
node healthy; ask `/readyz`.

Stopping (SIGTERM, or an interrupt):

1. `/readyz` answers 503 at once.
2. The node **keeps serving** on both ports for `--drain-delay`.
3. The listeners close; requests in flight get `--shutdown-timeout` (default `30s`) to
   finish, then their connections are closed.

Step 2 exists for the balancer, and is worth nothing unless the balancer notices in
time:

```
check interval x failures to mark down (+ the check's timeout)  <  --drain-delay
```

| Where | Drain delay | Check | Takes a node out after |
|---|---|---|---|
| The binary, as it comes | `0s` - **no drain at all** | - | set `--drain-delay` |
| The examples for virtual machines | `--drain-delay 10s` | every 2 s, 2 failures, 2 s timeout | at most 6 s |
| Compose cluster | `2s` (`CACTUSFS_DRAIN_DELAY`) | every 500 ms, 2 failures | about 1 s |
| Helm chart | 20 s, worked out: `probes.periodSeconds` x `probes.failureThreshold` + one period = 5 x 3 + 5 | the kubelet's readiness probe, every 5 s, 3 failures | at most 15 s |

A transfer that is still running when the shutdown timeout ends is cut, and the client
retries it. If your objects take longer than 30 s to move, raise `--shutdown-timeout`
(and, in the chart, `shutdownTimeoutSeconds`).

### Protocols, limits and timeouts of the nodes

| | Value | What it means for a balancer |
|---|---|---|
| TLS | 1.2 and 1.3; with 1.2 only ECDHE suites with AES-GCM or ChaCha20-Poly1305 | A balancer that begins TLS towards the nodes must offer one of them |
| HTTP | HTTP/2 and HTTP/1.1, chosen by ALPN (`h2`, `http/1.1`) | With passthrough the client negotiates with the node. A layer 7 balancer may use either towards the nodes |
| Request headers | 16 KiB, read within 5 s | - |
| Idle connections | Closed by the node after 60 s | A layer 7 balancer that keeps connections to the nodes open should drop idle ones sooner |
| S3, a whole request | No limit | Do not add one |
| Admin port, a whole request | 15 s, except the uploads and downloads of objects through the management API (the console's object browser), which have no limit | Do not add a shorter one; do not cap console uploads |
| One `PUT`, one part | 5 GiB | No body limit below it |
| A multipart object | 5 TiB, 10 000 parts, 5 MiB a part except the last | - |

Nothing the nodes send depends on the scheme, the name or the port the client used:
there are no absolute redirects, and `Location` headers are paths. The console's
session cookie is `__Host-cactusfs-session`: `Secure`, `HttpOnly`, `SameSite=Strict`,
`Path=/`, no `Domain`. A browser accepts it only over HTTPS, so the console must be
reached over HTTPS at the balancer too, and it belongs at the root of a host name of
its own - not under a path prefix.

## 2. Docker Desktop (compose)

The development cluster of [`deploy/compose/compose.yaml`](../deploy/compose/compose.yaml)
runs four nodes, all with the `gateway` and `console` roles, and already publishes each
of them:

| Node | S3 | Admin port, on `127.0.0.1` only |
|---|---|---|
| node1 | `https://localhost:9000` | `https://localhost:9001` |
| node2 | `https://localhost:9010` | `https://localhost:9011` |
| node3 | `https://localhost:9020` | `https://localhost:9021` |
| node4 | `https://localhost:9030` | `https://localhost:9031` |

So a client can be pointed at any node with no balancer at all. To have **one** entry
point that survives a node being stopped, add a balancer with an override file. The
project's compose file is not edited:

```sh
export CACTUSFS_KMS_PASSPHRASE=...     # as for the cluster itself
docker compose -f deploy/compose/compose.yaml \
               -f deploy/examples/loadbalancer/compose.haproxy.yaml up -d
```

(`podman compose` takes the same arguments.) That adds a service `lb` - HAProxy in TCP
mode, configured by
[`haproxy.compose.cfg`](../deploy/examples/loadbalancer/haproxy.compose.cfg) - to the
same project, and so to its default network (`cactusfs_default`), where the nodes
answer to `node1` to `node4`:

| Through the balancer | Address |
|---|---|
| S3 | `https://localhost:9443` |
| Console, Swagger UI, management API | `https://localhost:9444`, on `127.0.0.1` only |

What the example does, and why:

- **TLS passthrough.** HAProxy forwards TCP on both ports. The certificate a client
  sees is the cluster's own (`s3.pem`, written by `cactusfs dev-init`), which names
  `localhost`, `127.0.0.1`, `::1` and `node1` to `node4`. `https://localhost:9443`
  therefore verifies against `s3-ca.pem` exactly as `https://localhost:9000` does. To
  reach the balancer under another name - the machine's, from a second machine - the
  certificate must carry that name: `dev-init --hosts <name>` adds names, but only
  when it writes a new configuration volume, and it never rewrites one that exists;
  the `init` service of the compose file does not pass it, so that is a change to make
  in an override of your own, before the first start.
- **The health check asks `/readyz` on port 9001** of each node, over TLS, also for the
  S3 backend. It checks every 500 ms and takes a node out after two failures, inside
  the 2 s the compose cluster drains for.
- **The check does not verify the certificate - in this example only.** The
  development CA is in a volume that also holds every node's private key, and the
  balancer has no business mounting it. The check carries no credential and reads no
  data. To verify, copy the CA out and mount that one file; the configuration file
  says how. Outside development, always verify.
- **No privileged port.** 9443 and 9444 work on Docker Desktop and with rootless
  Podman. Change them with `CACTUSFS_LB_S3_PORT` and `CACTUSFS_LB_ADMIN_PORT`.
- **The admin port stays on `127.0.0.1`**, as the project's own compose file has it,
  because of the console user `admin` / `admin`.
- **The image is named by a version tag** (`docker.io/library/haproxy:3.0`); the file
  could not carry a digest that was looked up. A deployment pins the digest.
- **Paths.** Compose reads the paths of every `-f` file relative to the directory of
  the first one. The override names its configuration file accordingly; with another
  `--project-directory`, adapt it.

The client's signature covers the host and the port: a client that talks to
`localhost:9443` signs `localhost:9443`, and passthrough delivers exactly that.

[`nginx-stream.conf`](../deploy/examples/loadbalancer/nginx-stream.conf) is the same
balancer written for nginx's `stream` module, with `proxy_timeout` in place of
HAProxy's timeouts. Open-source nginx has **no active health checks in `stream`**
(`health_check` is NGINX Plus): it sets a node aside only after connections to it have
failed (`max_fails`, `fail_timeout`). It never asks `/readyz`, so it keeps sending new
connections to a node that is draining or starting. Use it where that is acceptable.

## 3. A cluster on virtual machines

### Topology

- At least four nodes that store; three or five of them vote on the metadata (`meta`).
- The balancer needs the nodes with the `gateway` role. The simplest cluster gives
  every node every role, and the balancer lists them all. Larger ones add nodes with
  `--roles gateway,console` and nothing else - they store nothing, need a
  `--meta-dir` of their own, and can be added later - and list only those.
- Every node that the admin endpoint lists has `gateway` and `console`.
- Every gateway runs with `--drain-delay` set (the examples assume `10s`), and with
  `--s3-domain` if virtual-hosted-style requests are wanted.
- Keep balancer and nodes on one site. A cluster is not stretched across a WAN, and
  neither is the path from its balancer.

```
            clients                       administrators
               |  443                          |  8443
        +------+-------------------------------+------+
        |   virtual address 10.0.0.10 (VRRP)          |
        |   lb1 10.0.0.5          lb2 10.0.0.6        |   HAProxy + keepalived
        +------+-------------------------------+------+
               |  9000 (S3)     9001 (admin, /readyz)
   +-----------+-----------+-----------+-----------+
   | node1     | node2     | node3     | node4     |      10.0.0.11 to .14
   +-----------+-----------+-----------+-----------+
        9002 (node RPC) and 9003 (Raft) between the nodes only
```

### Two balancers and one address: HAProxy and keepalived

[`haproxy.cfg`](../deploy/examples/loadbalancer/haproxy.cfg) goes on both balancer
machines unchanged. It has:

- a frontend `s3` on 443, to port 9000 of the nodes, and a frontend `admin` on 8443, to
  port 9001, that rejects every source outside the administrators' network;
- TCP mode - TLS passthrough - for both, and, commented out, the variant that ends TLS
  at the balancer and begins it again towards the nodes, with the nodes' certificate
  verified (`ssl verify required ca-file ...`). That variant sets no header: the nodes
  read none of the `X-Forwarded-*` family, and the `Host` header is passed as it came;
- `balance leastconn`;
- the health check `GET /readyz` on port 9001 over TLS, expecting 200, **with the
  certificate verified**, every 2 s, down after 2 failures and up after 2 successes -
  out of rotation within 6 s, inside a drain delay of 10 s;
- inactivity timeouts of 15 minutes, and `maxconn` at each level - set those to what
  your machines carry.

[`keepalived.conf`](../deploy/examples/loadbalancer/keepalived.conf) moves the virtual
address between the two machines by VRRP, in unicast, and gives it up when HAProxy is
no longer running. Both machines start as `BACKUP` with `nopreempt`: the address moves
once for a failure and not again for the repair. Connections that are open when it
moves are lost, and clients retry.

### DNS and certificates

| Record | Points at | For |
|---|---|---|
| `s3.example.com` | the virtual address | S3, path-style: `https://s3.example.com/<bucket>/<key>` |
| `*.s3.example.com` | the virtual address | S3, virtual-hosted-style: `https://<bucket>.s3.example.com/<key>`. Needs `--s3-domain s3.example.com` on every gateway |
| `console.example.com` | the virtual address | Console, Swagger UI and management API on 8443 |

With passthrough, the certificate clients see is the one **on the nodes** (`--tls-cert`,
`--tls-key`): it must carry `s3.example.com`, `*.s3.example.com` if used, and
`console.example.com`. One certificate and key may be installed on every node. The AWS
CLI and boto3 use path-style addresses with an endpoint of your own unless told
otherwise; several other SDKs default to virtual-hosted style and need either the
wildcard or their "force path style" option. The certificate of the listeners is
not the node certificate (`--node-tls-cert`), which names the node, comes from a CA
that issues nothing else and never leaves the cluster.

To renew, replace the two files on each node. A node reads them again every
`--tls-reload-interval` (default one minute) and uses a new pair for new connections,
without a restart; a pair that is not valid is refused and the old one stays
([ADR-0020](adr/0020-certificate-rotation.md)). The balancer needs nothing: it holds no
certificate. If it verifies its health checks against a CA that changes, give it the
new CA first.

With the variant that ends TLS at the balancer, the public certificate is the
balancer's, and the nodes' certificate needs only a name the balancer checks for and a
CA the balancer trusts. A private CA is the natural choice.

### Firewall

| From | To | Port | Why |
|---|---|---|---|
| Clients | Virtual address | 443 | S3 |
| Administrators' network | Virtual address | 8443 | Console, Swagger UI, management API |
| Balancers | Gateway nodes | 9000 | S3 |
| Balancers | Gateway nodes | 9001 | Health checks of both backends, and the admin endpoint |
| Monitoring | Every node | 9001 | `/metrics`, node by node, with a credential - not through the balancer: each node has its own figures |
| Nodes | Nodes | 9002, 9003 | Node RPC and Raft |
| Balancer | Balancer | VRRP (IP protocol 112) | keepalived |
| Nodes | Vault | as configured | With `--kms vault` |

Nothing else. In particular nobody but the nodes reaches 9002 and 9003, and clients do
not reach 9000 or 9001 of a node directly - otherwise the restriction on the admin
frontend is worth nothing.

### Restarting nodes one at a time

1. Check that the cluster is whole: `/readyz` is 200 on every node.
2. Send the node SIGTERM (stop its service). `/readyz` turns 503; the node serves on.
3. Within 6 s the balancers stop sending it new connections. Connections that are open
   stay open.
4. After `--drain-delay` the node closes its listeners, lets requests in flight finish
   within `--shutdown-timeout`, and stops.
5. Start it. Its ports open at once and `/readyz` says 503 until it has joined; two
   good checks later it is back in rotation.
6. Wait for that before the next node. One node at a time: the cluster reads and writes
   with one node away. Take the nodes that do not vote first, then the voters, the one
   that leads the metadata group last.

### In a cloud

VRRP needs a network that lets a machine announce an address that is not its own; most
public clouds do not. Use the provider's layer 4 balancer in place of both HAProxy
machines, or in front of them. What it needs is the same everywhere: TCP listeners (no
TLS on the balancer), an HTTPS health check on `/readyz` of port 9001 with thresholds
inside the drain delay, and cross-zone balancing if the gateways are spread.

- **AWS**: a Network Load Balancer with TCP listeners; target group health check
  HTTPS, path `/readyz`, port 9001. Not an Application Load Balancer, which ends TLS
  and rewrites.
- **Azure**: a Standard Load Balancer with TCP rules and an HTTPS health probe on
  `/readyz`, port 9001.
- **Google Cloud**: a passthrough Network Load Balancer with an HTTPS health check on
  `/readyz`, port 9001.

These forward packets rather than opening connections of their own, so the nodes can
see the clients' addresses; whether they do depends on options of each product
(target type, client address preservation). Check the audit trail after setting one
up. Health checks come from the provider's own address ranges, which the nodes'
firewall must let reach 9001.

### Without a balancer

- **DNS round-robin**: one name with an A record for each gateway. No machine to run,
  and it works with TLS and SigV4 as they are (the certificate names the shared name).
  But DNS knows nothing of health or draining: a node that is down keeps being handed
  out until you change the zone, and resolvers and clients cache answers past their
  TTL. Clients that retry on another address ride through it; those that do not, fail
  for the share of requests that reach the dead node. Acceptable for a few
  well-behaved clients, not as an availability measure.
- **Several endpoints in the client**: the S3 SDKs and the AWS CLI take one endpoint
  URL. Spreading over several is then your application's work - a list, a health
  check, a retry on the next. It suits a single batch job; it does not suit many
  clients or anything a browser uses.

## 4. k3s and Kubernetes

The chart ([`deploy/helm/cactusfs`](../deploy/helm/cactusfs/README.md),
[ADR-0015](adr/0015-helm-chart.md)) already balances inside the cluster:

| Service | Port | Selects | Default type |
|---|---|---|---|
| `<release>` | 9000 (`service.port`) | Pods labelled `cactusfs.io/gateway: "true"` - the nodes whose pool has the `gateway` role - that are ready | `ClusterIP` |
| `<release>-admin` | 9001 (`adminService.port`) | The same pods | `ClusterIP` |
| `<release>-nodes` | 9002, 9003 | Every pod, ready or not; headless | Never publish it |

Readiness is the kubelet's probe of `/readyz` on port 9001; a pod that is not ready is
taken out of both Services. Inside the cluster clients use
`https://<release>.<namespace>.svc:9000`; the certificate of `secrets.tls` must carry
whatever name they use.

The admin Service selects by `gateway` alone. If it is to serve the console, give every
pool with `gateway` the `console` role as well; a gateway without it answers the
management API and Swagger UI and has no console.

> **The most common mistake: the NetworkPolicy.** The chart installs a default-deny
> NetworkPolicy. Until `networkPolicy.clientsFrom` (port 9000) and
> `networkPolicy.adminFrom` (port 9001) name who may come in, **nobody does** - and a
> balancer or ingress controller in front changes who that is. Each is a list of
> NetworkPolicy peers: `podSelector`, `namespaceSelector`, `ipBlock`. Traffic that is
> not named is dropped without a word: the symptom is a timeout, not a refusal.
>
> What to name depends on what the pods see as the source:
>
> | In front | The pods see | Name in `clientsFrom` |
> |---|---|---|
> | An ingress controller or Gateway in the cluster | The controller's pods | Its namespace, and its pod labels if the namespace holds more: `namespaceSelector` (+ `podSelector`) |
> | `LoadBalancer` or `NodePort`, `externalTrafficPolicy: Cluster` (the default) | Usually an address of a cluster machine, sometimes the client's: it depends on the network plugin and on which machine the packet arrived | `ipBlock` for the machines' network, and for the clients' |
> | `LoadBalancer` or `NodePort`, `externalTrafficPolicy: Local` | The client's address | `ipBlock` for the clients' networks |
> | k3s ServiceLB | The ServiceLB pods in `kube-system`, or a machine address | Test it; see (i) below |
>
> To allow every address, write it as its two halves - `0.0.0.0/1` and `128.0.0.0/1` -
> since some policy controllers cannot hold a block of length 0 (the project's own k3s
> test does this, `test/k8s/testdata/values.yaml`). A NetworkPolicy binds only where
> the network plugin enforces it; k3s enforces it as it comes.

### (i) A Service of type LoadBalancer

```yaml
service:
  type: LoadBalancer
  port: 443                      # what clients connect to; the pods stay on 9000
  annotations: {}                # passed to the Service as they are
networkPolicy:
  clientsFrom:
    - ipBlock: {cidr: 198.51.100.0/24}
```

- **A cloud**: the provider's controller creates a balancer. Its options go in
  `service.annotations` - they are the provider's, not the chart's. Ask for a layer 4
  (network) balancer; one that ends TLS needs the re-encryption rules of section 1.
- **k3s as it comes**: the built-in ServiceLB (klipper-lb) answers for the Service by
  binding `service.port` on every machine of the cluster. There is no separate
  address: clients use the machines', and a DNS name with one record per machine has
  the limits of DNS round-robin. The port must be free on every machine (Traefik, if
  enabled, already holds 443). Connections reach the CactusFS pods from the `svclb-...`
  pods ServiceLB runs in `kube-system`, or from a machine address, depending on the
  version of k3s and the traffic policy: find out which with a test before you narrow
  `clientsFrom`.
- **Bare metal**: MetalLB gives the Service an address of its own.
  [`metallb-pool.yaml`](../deploy/examples/loadbalancer/metallb-pool.yaml) is a pool
  and its layer 2 advertisement; the Service asks for the pool with an annotation. On
  k3s, disable ServiceLB first. In layer 2 mode one machine at a time answers for the
  address, and kube-proxy spreads the connections from there.

**`externalTrafficPolicy`.** `Local` keeps the client's address - the one arrangement
here in which the audit trail, `aws:SourceIp` and the sign-in limit see real clients -
and saves a hop, but only machines that run a ready gateway pod take traffic. With
MetalLB in layer 2 mode that means all traffic goes to the gateway pods of the one
machine that holds the address. `Cluster`, the default, spreads over every gateway and
hides the client.

The chart has **no value** for `externalTrafficPolicy` (nor for
`loadBalancerSourceRanges`, `loadBalancerClass` or a fixed address); the Service
template sets `type`, the port, `nodePort` and annotations only. Until it has, set the
field on the live Service and check it after every `helm upgrade`, or add it with a
Helm post-renderer:

```sh
kubectl -n cactusfs patch service cactusfs -p '{"spec":{"externalTrafficPolicy":"Local"}}'
```

### (ii) NodePort and a balancer outside the cluster

```yaml
service:
  type: NodePort
  nodePort: 30900
networkPolicy:
  clientsFrom:
    - ipBlock: {cidr: 10.0.0.0/24}     # see the callout: usually the machines' network
```

Point the HAProxy of section 3 at `<machine>:30900` of every cluster machine, still in
TCP mode. Two things differ from the virtual machines:

- **The health check is a plain TCP check of the node port** (`check`, without
  `option httpchk` and `check port`). `/readyz` is on the admin port, which this
  Service does not publish, and it is not needed here: Kubernetes already sends only
  to ready pods. The outer check answers one question - is this machine reachable.
- A NodePort answers on every address of the machine, on many hosts past the host's
  own firewall. Do not make `adminService` a NodePort for convenience.

### (iii) An ingress controller or the Gateway API

Use **TLS passthrough**: the controller routes by the server name of the TLS handshake
and the pod ends TLS. Nothing of the request can be changed, no body limit or timeout
of an HTTP proxy applies, and the public certificate is the one in `secrets.tls`.

| Controller | Example | Needs |
|---|---|---|
| Traefik - **what k3s installs** | [`traefik-ingressroutetcp.yaml`](../deploy/examples/loadbalancer/traefik-ingressroutetcp.yaml): `IngressRouteTCP` with `tls.passthrough: true` | The matcher syntax of your Traefik version (2 or 3); the file gives both |
| Gateway API | [`gateway-api-tlsroute.yaml`](../deploy/examples/loadbalancer/gateway-api-tlsroute.yaml): a `TLS` listener in mode `Passthrough` and a `TLSRoute` | An implementation that supports `TLSRoute`, and the API version your cluster serves |
| ingress-nginx | [`ingress-nginx-passthrough.yaml`](../deploy/examples/loadbalancer/ingress-nginx-passthrough.yaml), first document: the `ssl-passthrough` annotation | The controller started with `--enable-ssl-passthrough`; without it the annotation does nothing |

If the controller must end TLS itself, it has to begin it again towards the pods,
verify their certificate, and keep to section 1. With an HTTP proxy in the path its
defaults are wrong for S3 and **must** be raised - for ingress-nginx (second document
of the same file):

| Annotation (`nginx.ingress.kubernetes.io/`) | Value | Otherwise |
|---|---|---|
| `backend-protocol` | `"HTTPS"` | The controller speaks plaintext to a TLS port: every request fails |
| `proxy-ssl-verify`, `proxy-ssl-secret`, `proxy-ssl-name` | `"on"`, the CA, a name in the pods' certificate | The pods' certificate is not checked |
| `proxy-body-size` | `"0"` | Uploads over 1 MiB are refused with 413 |
| `proxy-request-buffering` | `"off"` | Every upload is written to the controller's disk before the pod sees a byte |
| `proxy-read-timeout`, `proxy-send-timeout` | minutes, in seconds | Long operations are cut after 60 s |

Other controllers have the same four concerns under other names: TLS to the backend,
body size, buffering, timeouts. The `Host` header must reach the pod as the client sent
it; no rewrite of the path.

Whichever controller: name it in `networkPolicy.clientsFrom` (the callout above; each
example file has the stanza), and have DNS point `s3.example.com` - and
`*.s3.example.com` with `s3.domain` - at the controller.

### The admin Service

It is `ClusterIP` by default and meant to stay so. Reach it with

```sh
kubectl -n cactusfs port-forward service/cactusfs-admin 9001:9001
```

and `https://localhost:9001` (the certificate must name `localhost`, or the browser
will ask). If it has to be published, do it apart from S3 - its own Service type, its
own passthrough route under its own name, `networkPolicy.adminFrom` naming the
administrators' network or the controller - and only after the console user `admin` has
been deleted or given another password. Remember that `adminFrom` opens port 9001 of
every pod, `/metrics` included.

### Restarts

`helm upgrade` restarts nothing (`updateStrategy: OnDelete`). Delete one pod at a time
and wait for it to be ready again, as the
[chart's README](../deploy/helm/cactusfs/README.md) describes. A pod that is told to
stop fails its readiness probe, is taken out of the Services, and serves on for the
drain delay (20 s with the default probes) so that kube-proxy, the controller and any
balancer in front have stopped sending to it. The chart works the delay out from
`probes.periodSeconds` and `probes.failureThreshold`, and `make helm-lint` asserts that
it is at least their product. An outer balancer that keeps its own list of machines
must take a machine out within the same time.

## 5. Verifying

`<s3>` is the S3 endpoint through the balancer and `<admin>` the admin endpoint
(`https://localhost:9443` and `https://localhost:9444` for the compose example); `ca.pem`
is the CA of the certificate clients see (`s3-ca.pem` in development).

**The probes answer through the balancer.** `/readyz` is on the admin port only; on the
S3 port the same path is read as a request for a bucket of that name and answers with
an S3 error in XML.

```sh
curl --cacert ca.pem -s -o /dev/null -w '%{http_code}\n' <admin>/readyz      # 200
curl --cacert ca.pem -s <s3>/readyz | head -c 300                            # an S3 error in XML, from a node
```

**An S3 round trip, path-style.** A signature that survives proves that host, path and
query arrived as they were signed.

```sh
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url <s3> --ca-bundle ca.pem s3 mb s3://lb-check
echo hello > small.txt
aws --endpoint-url <s3> --ca-bundle ca.pem s3 cp small.txt s3://lb-check/a/b%20c/small.txt
aws --endpoint-url <s3> --ca-bundle ca.pem s3 ls s3://lb-check --recursive
```

The key with `%20` in it is deliberate: a balancer that decodes or normalises paths
breaks this one with `SignatureDoesNotMatch`. Whenever a step here fails, repeat it
against one node directly: only a difference between the two is the balancer's doing.

**Virtual-hosted style**, if `--s3-domain` is set and the wildcard name resolves:

```sh
aws configure set default.s3.addressing_style virtual
aws --endpoint-url <s3> --ca-bundle ca.pem s3 ls s3://lb-check
aws configure set default.s3.addressing_style path
```

**A presigned URL**, made against the public name and fetched through the balancer:

```sh
url=$(aws --endpoint-url <s3> s3 presign s3://lb-check/a/b%20c/small.txt --expires-in 300)
curl --cacert ca.pem -s "$url"                                               # hello
```

**A multipart upload**, larger than the part size (8 MiB in the AWS CLI), and back:

```sh
head -c 67108864 /dev/urandom > big.bin
aws --endpoint-url <s3> --ca-bundle ca.pem s3 cp big.bin s3://lb-check/big.bin
aws --endpoint-url <s3> --ca-bundle ca.pem s3 cp s3://lb-check/big.bin big.back
cmp big.bin big.back && echo same
```

The parts go out over several connections, which a balancer without stickiness spreads
over the nodes. Repeat it with a file larger than any body limit you suspect on the
path.

**A node stopped, requests continuing.** Run the loop, then stop one node in another
terminal (`docker compose -f deploy/compose/compose.yaml stop node2`, or SIGTERM to the
service of one machine, or `kubectl delete pod`). No line should be anything but `200`;
start the node again and watch it return in the balancer's log.

```sh
while true; do
  aws --endpoint-url <s3> --ca-bundle ca.pem s3api head-object --bucket lb-check --key big.bin \
    --query 'ContentLength' --output text >/dev/null && echo 200 || echo FAILED
  sleep 0.5
done
```

One node at a time: the compose cluster writes with one node away and not with two.

**The console without stickiness.** In a browser: sign in at `<admin>/`, stop the node
that served you or simply reload a few times, and stay signed in. With curl, where
every call is a new connection and so, with least-connections, another node:

```sh
curl --cacert ca.pem -s -c jar -H 'Content-Type: application/json' \
     -d '{"username":"<user>","password":"<password>"}' <admin>/api/v1/session >/dev/null
for i in 1 2 3 4 5 6; do
  curl --cacert ca.pem -s -b jar -o /dev/null -w '%{http_code}\n' <admin>/api/v1/session
done                                                                         # 200, six times
```

HAProxy's log names the node each connection went to
(`docker compose ... logs lb` for the compose example).

**What the audit trail says.** Make a failed sign-in or a request with a wrong secret
through the balancer, then read the record in the console's audit log: the source
address is the one section 1 told you to expect. If you rely on `aws:SourceIp`, test a
policy that uses it now.

## 6. Limits and gaps today

What this guide works around, said plainly. None of it is hidden by the examples.

| Gap | Effect | Until it is closed |
|---|---|---|
| No PROXY protocol on the listeners | A TCP balancer cannot hand the client's address on | Use a balancer that preserves the address at layer 4, or accept the balancer's address |
| No trusted-proxy setting; `X-Forwarded-For` and `Forwarded` are not read | Behind any balancer that opens its own connections, audit records, `aws:SourceIp` and the log name the balancer | Keep the balancer's logs; do not write `aws:SourceIp` policies for clients behind such a balancer |
| The sign-in limit counts by source address, in each node's memory | Behind such a balancer all console users share one allowance of failed attempts, and the allowance is multiplied by the number of gateways | Restrict who reaches the admin endpoint; watch the audit trail for failed sign-ins |
| `/readyz` is on port 9001 only | A health check of the S3 port must be pointed at another port; balancers that cannot do that fall back to a TCP check, which a starting node passes | Use a balancer that can (`check port` in HAProxy, a health check port in cloud balancers) |
| `--drain-delay` defaults to `0s` in the binary | A node on a virtual machine stops with no warning to the balancer unless the operator sets it | Set it; compose and the chart do |
| No plaintext listener for a trusted network | A balancer that ends TLS must begin it again; some appliances make that awkward | By design: encryption in transit is not optional. Use passthrough |
| The chart has no value for `externalTrafficPolicy`, `loadBalancerSourceRanges`, `loadBalancerClass` or a fixed balancer address | They are set outside the chart | `kubectl patch`, or a post-renderer |
| The chart renders no Ingress, `IngressRouteTCP` or `TLSRoute` | The route is yours to write | The example manifests |
| The admin Service selects pods by `gateway` only | A gateway pool without the `console` role is behind the console's Service and has no console | Give gateway pools the `console` role too |
| The examples are not run by the project's gates | A change in the product could outdate them unnoticed | Section 5, after every change of balancer or release |
| Nothing in the responses names the node that answered | Spreading is proved from the balancer's log, not from the client | The balancer's log |
