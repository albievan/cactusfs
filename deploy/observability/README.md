# Observability pack

Alerting rules and dashboards for a CactusFS deployment. Nothing here is needed to run
CactusFS, and nothing here fetches anything: the dashboards use panels of Grafana itself
and name no address.

| | |
|---|---|
| `rules/cactusfs.rules.yaml` | Prometheus recording and alerting rules. Every alert has a `severity` (`critical` or `warning`), a `summary` and a `description`; none carries a bucket, a key or a credential |
| `rules/cactusfs.rules.test.yaml` | Their unit tests, for `promtool test rules`: for each alert a series that fires it and one that does not, with the arithmetic in comments |
| `dashboards/cactusfs-overview.json` | Nodes, versions, the S3 API at a glance; the metadata group, readiness, drives and node certificates |
| `dashboards/cactusfs-api.json` | The S3 API and the management API by operation and status |
| `dashboards/cactusfs-storage.json` | Every outbound port; healing, scrubbing, garbage collection, lifecycle rules; the drives; the calls between nodes |

## Scraping

`/metrics` is on the admin port (9001), over TLS, and requires
`Authorization: Bearer <access key ID>:<secret>` of an API key whose owner is allowed
`admin:GetMetrics`. **The rules and dashboards expect the job to be named `cactusfs`.**
Scrape every node, not a load balancer in front of them - and every node whatever its
roles: the gauges of the metadata group and of the drives come from the nodes that vote
and store, which need not be gateways. A node that is still starting refuses its scrape.

```yaml
scrape_configs:
  - job_name: cactusfs
    scheme: https
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/secrets/cactusfs-metrics   # <access key ID>:<secret>
    tls_config:
      ca_file: /etc/prometheus/secrets/cactusfs-ca.crt
    static_configs:
      - targets: [node1.example.test:9001, node2.example.test:9001]
rule_files:
  - cactusfs.rules.yaml
```

On Kubernetes with the Prometheus Operator the Helm chart renders a PodMonitor, which
selects every pod of the release, and a PrometheusRule instead (`podMonitor.enabled`,
`prometheusRule.enabled`; see the chart's README). The chart carries a copy of the rules in
`deploy/helm/cactusfs/files/cactusfs.rules.yaml`: change both files together - the gate
fails when they differ.

## Dashboards

Import the JSON files, or provision them from a directory. Each has a variable for the
Prometheus data source (`DS_PROMETHEUS`) and one for the nodes shown.

## What the alerts cover

| Group | Alerts on |
|---|---|
| `cactusfs.availability` | a node that does not answer, no node at all, restarts, mixed versions |
| `cactusfs.api` | server errors and slowness of the S3 and the management API |
| `cactusfs.storage` | the KMS, shard operations, objects that cannot be rebuilt, shards rebuilt, lifecycle rules, reads not counted |
| `cactusfs.metadata` | no leader of the metadata group, elections that do not stop, a node behind, an upgrade of the schema that is not finished |
| `cactusfs.drives` | a drive that is offline, a drive that is nearly full |
| `cactusfs.nodes` | a node certificate that ends within 14 and within 3 days, a certificate on disk that was refused, refused handshakes, failing calls between nodes, a node that is not ready |
| `cactusfs.audit` | an audit trail that cannot be written |

A drive is named in an alert by its generated ID, never by its directory.

## What is not here yet

There is no alert on a scrubber that has stopped (the count of passes cannot tell a long
pass from a stuck one; it needs a gauge of the last finished pass) and none on requests
refused while a node starts (there is no count of them). The end of the rules file says
so. An alert is not written before its metric exists - a rule on a metric that does not
exist never fires.

The end of the certificate clients are shown (`cactusfs_tls_certificate_not_after_seconds{use="s3"}`)
is exposed and in no alert: the two alerts on a certificate that ends read the node's.
Write one with the margin your own renewal has.

## Certificates and handshakes

Certificates are replaced without a restart (ADR-0020), and three metrics say how that
went, for the node's certificate (`use="node"`) and for the one clients are shown
(`use="s3"`): when the certificate in use ends, whether the files hold something that
was refused (`cactusfs_tls_reload_pending`, what `CactusFSCertificateReloadRefused`
reads), and how often what for (`cactusfs_tls_reload_refused_total`). After a
replacement `cactusfs_node_certificate_not_after_seconds` is the new certificate's end.

`cactusfs_node_handshakes_refused_total` counts every handshake with another node that
this node refused, once: on the node RPC port, on the Raft port - a node that is not
admitted there, and a caller without a certificate, with one of another CA, or one that
was too slow - and on the connections Raft dials, when the node that answers shows a
certificate that is not the one of the node that was meant.

## Checking

```sh
make observability-check
```

runs `promtool check rules` and `promtool test rules`; checks that the chart's copy of
the rules is the same file, that every dashboard is JSON of core panels with no remote
asset, and what the chart renders with its monitoring templates on and off; and runs the
test of `internal/adapters/telemetry` that reads the rules, their tests and the
dashboards and fails on a metric the binary's registry does not have, and the one that
holds the list of metrics in `docs/ARCHITECTURE.md` to the registry.
