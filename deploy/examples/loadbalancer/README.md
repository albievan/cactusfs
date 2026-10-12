# Load balancer examples

Example configurations for [`docs/load-balancing.md`](../../../docs/load-balancing.md),
which explains every choice made in them. Read that first.

**These are examples.** None of them is run by the project's gates (`make check` does
not look at this directory), every name and address in them is a placeholder
(`example.com`, RFC 5737 and RFC 1918 addresses), and container images are named by a
version tag where a deployment pins a digest. Adapt each one, then prove it with the
section "Verifying" of the guide before it carries traffic.

All of them keep to the same rules: only ports 9000 (S3) and 9001 (console, Swagger UI,
management API) are balanced, never 9002 or 9003; TLS is passed through to the nodes,
or ended and begun again with the nodes' certificate verified, never plaintext; the
health check is `GET /readyz` over HTTPS on port 9001; nothing is sticky.

| File | For | What it is | How to use it |
|---|---|---|---|
| `compose.haproxy.yaml` | Docker Desktop, Podman | A compose override that adds one service, `lb` (HAProxy 3.0, TLS passthrough), to the development cluster. S3 on `https://localhost:9443`, the admin port on `https://localhost:9444` (this machine only) | `docker compose -f deploy/compose/compose.yaml -f deploy/examples/loadbalancer/compose.haproxy.yaml up -d` |
| `haproxy.compose.cfg` | the same | The HAProxy configuration that service mounts | Used by the override. Check: `haproxy -c -f haproxy.compose.cfg` |
| `nginx-stream.conf` | the same | The same balancer for nginx's `stream` module. No active health checks: read its header | Name the nginx image and this file in a copy of the override; its header says how |
| `haproxy.cfg` | Virtual machines | HAProxy 3.0 (2.8 or later) for two balancer machines: `s3` on 443 to the nodes' 9000, `admin` on 8443 to 9001 for one network only; TLS passthrough, and commented out the variant that ends TLS and begins it again | Replace names, addresses and the CA file; `/etc/haproxy/haproxy.cfg` on both machines. Check: `haproxy -c -f haproxy.cfg` |
| `keepalived.conf` | Virtual machines | keepalived 2.x: one virtual address between the two balancer machines, unicast VRRP, given up when HAProxy is not running | Replace addresses and the interface; swap the two addresses and lower `priority` on the second machine. Check: `keepalived -t -f keepalived.conf` |
| `metallb-pool.yaml` | k3s, Kubernetes on bare metal | An `IPAddressPool` and `L2Advertisement` for MetalLB, for `service.type: LoadBalancer` | `kubectl apply -f metallb-pool.yaml`, then the chart values in its header |
| `traefik-ingressroutetcp.yaml` | k3s (Traefik is what it installs), Kubernetes with Traefik | `IngressRouteTCP` with TLS passthrough to the S3 Service | Replace names; `kubectl apply -f`; add the `networkPolicy.clientsFrom` of its header to the chart's values |
| `gateway-api-tlsroute.yaml` | Kubernetes with a Gateway API implementation | A `Gateway` with a TLS listener in mode `Passthrough`, and a `TLSRoute` | Replace names, the GatewayClass and the API version your cluster serves; `kubectl apply -f`; `clientsFrom` as in its header |
| `ingress-nginx-passthrough.yaml` | Kubernetes with ingress-nginx | Two documents, of which one is applied: an Ingress with `ssl-passthrough`, and one that ends TLS at the controller and begins it again, with the annotations S3 needs | Replace names; apply the one you want; `clientsFrom` as in its header |

The Kubernetes examples assume the Helm release `cactusfs` in the namespace `cactusfs`,
so that the S3 Service is `cactusfs` on port 9000 and the admin Service
`cactusfs-admin` on 9001. They publish S3 only. The admin Service stays `ClusterIP`;
the guide says how to reach it and what to do before publishing it.

## What was and was not tested

These files were written from the code of CactusFS and from the documentation of the
tools they configure. No gate of the project runs them. What was run, once, by hand
(2026-10-12, Docker on Linux amd64):

- `compose.haproxy.yaml` with `haproxy.compose.cfg` (HAProxy 3.0): `haproxy -c` accepts
  the file; the four-node compose cluster started with the balancer in front; all four
  nodes passed its HTTPS check of `/readyz`; through the balancer's admin port a console
  sign-in, a bucket, an upload of 8 MiB and eight downloads of it with the one session
  cookie - each intact, spread over the nodes; with one node stopped, six more intact;
  an S3 request without a credential through its S3 port was answered by a node (403).
- `nginx-stream.conf` (nginx 1.28): `nginx -t` accepts the file. It was not run in front
  of a cluster.

Not run at all: `haproxy.cfg` and `keepalived.conf` for virtual machines, and the four
Kubernetes manifests - they need machines, a virtual address, or controllers with their
own resource kinds. The YAML files parse; that is all that was checked of them. Before
relying on any file, run the tool's own check (`haproxy -c`, `nginx -t`,
`keepalived -t`, `kubectl apply --dry-run=server`) with the version you deploy, and then
the guide's "Verifying".
