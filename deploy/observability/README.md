# Observability pack

Alerting rules and dashboards for a CactusFS deployment. Nothing here is needed to run
CactusFS, and nothing here fetches anything: the dashboards use panels of Grafana itself
and name no address.

| | |
|---|---|
| `rules/cactusfs.rules.yaml` | Prometheus recording and alerting rules. Every alert has a `severity` (`critical` or `warning`), a `summary` and a `description`; none carries a bucket, a key or a credential |
| `rules/cactusfs.rules.test.yaml` | Their unit tests, for `promtool test rules`: for each alert a series that fires it and one that does not, with the arithmetic in comments |
| `dashboards/cactusfs-overview.json` | Nodes, versions, the S3 API at a glance |
| `dashboards/cactusfs-api.json` | The S3 API and the management API by operation and status |
| `dashboards/cactusfs-storage.json` | KMS, shard stores and metadata; healing, scrubbing, garbage collection, lifecycle rules |

## Scraping

`/metrics` is on the admin port (9001), over TLS, and requires
`Authorization: Bearer <access key ID>:<secret>` of an API key whose owner is allowed
`admin:GetMetrics`. **The rules and dashboards expect the job to be named `cactusfs`.**
Scrape every node, not a load balancer in front of them.

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

On Kubernetes with the Prometheus Operator the Helm chart renders a ServiceMonitor and a
PrometheusRule instead (`serviceMonitor.enabled`, `prometheusRule.enabled`; see the
chart's README). The chart carries a copy of the rules in
`deploy/helm/cactusfs/files/cactusfs.rules.yaml`: change both files together - the gate
fails when they differ.

## Dashboards

Import the JSON files, or provision them from a directory. Each has a variable for the
Prometheus data source (`DS_PROMETHEUS`) and one for the nodes shown.

## What is not here yet

There is no alert on the state of the metadata group (no leader, a voter behind), on
drives (offline, nearly full), on the expiry of node certificates, on calls between
nodes or on readiness: the binary has no metric for them yet (PLAN 7.8). The end of the
rules file lists them. An alert is not written before its metric exists - a rule on a
metric that does not exist never fires.

## Checking

```sh
make observability-check
```

runs `promtool check rules` and `promtool test rules`; checks that the chart's copy of
the rules is the same file, that every dashboard is JSON of core panels with no remote
asset, and what the chart renders with its monitoring templates on and off; and runs the
test of `internal/adapters/telemetry` that reads the rules, their tests and the
dashboards and fails on a metric the binary's registry does not have.
