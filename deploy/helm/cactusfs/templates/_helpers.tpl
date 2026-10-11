{{- define "cactusfs.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}{{ .Release.Name | trunc 40 | trimSuffix "-" }}{{- else -}}{{ printf "%s-%s" .Release.Name .Chart.Name | trunc 40 | trimSuffix "-" }}{{- end -}}
{{- end -}}

{{- define "cactusfs.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "cactusfs.selector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Whether a pool's roles include one; empty roles are all of them. */}}
{{- define "cactusfs.hasRole" -}}
{{- if or (not .roles) (has .role (splitList "," (.roles | replace " " ""))) -}}true{{- end -}}
{{- end -}}

{{/* The nodes of the cluster, as a JSON list of {id, pool index}. */}}
{{- define "cactusfs.nodes" -}}
{{- $full := include "cactusfs.fullname" . -}}
{{- $nodes := list -}}
{{- range $p, $pool := .Values.pools -}}
{{- range $i := until (int $pool.replicas) -}}
{{- $nodes = append $nodes (dict "id" (printf "%s-%s-%d" $full $pool.name $i) "pool" $p) -}}
{{- end -}}
{{- end -}}
{{- toJson $nodes -}}
{{- end -}}

{{/* The address of a node's pod inside the cluster. */}}
{{- define "cactusfs.host" -}}
{{- printf "%s-0.%s-nodes.%s.svc.%s" .id (include "cactusfs.fullname" .root) .root.Release.Namespace .root.Values.clusterDomain -}}
{{- end -}}

{{/* What must hold before anything is rendered. */}}
{{- define "cactusfs.validate" -}}
{{- $voters := 0 -}}{{- $stores := 0 -}}{{- $total := 0 -}}
{{- range .Values.pools -}}
{{- $total = add $total (int .replicas) -}}
{{- if include "cactusfs.hasRole" (dict "roles" .roles "role" "meta") -}}{{- $voters = add $voters (int .replicas) -}}{{- end -}}
{{- if include "cactusfs.hasRole" (dict "roles" .roles "role" "storage") -}}{{- $stores = add $stores (int .replicas) -}}{{- end -}}
{{- end -}}
{{- if lt (int $total) 1 -}}{{- fail "pools: the cluster has no node" -}}{{- end -}}
{{- if gt (int $total) 1 -}}
{{- if not (or (eq (int $voters) 3) (eq (int $voters) 5)) -}}{{- fail (printf "pools: %d nodes vote in the metadata group; a cluster has 3 or 5" $voters) -}}{{- end -}}
{{- if lt (int $stores) 4 -}}{{- fail (printf "pools: %d nodes store; a cluster has at least 4" $stores) -}}{{- end -}}
{{- if and (eq .Values.kms.type "local") (not .Values.dev.localKMS) -}}{{- fail "kms.type=local on more than one node shares one key file between them: use Vault, or say dev.localKMS=true for a development cluster" -}}{{- end -}}
{{- end -}}
{{- if not (has .Values.kms.type (list "vault" "local")) -}}{{- fail "kms.type is vault or local" -}}{{- end -}}
{{- if not .Values.secrets.root -}}{{- fail "secrets.root: name the Secret with the root credentials (keys access-key, secret-key)" -}}{{- end -}}
{{- if not .Values.secrets.tls -}}{{- fail "secrets.tls: name the kubernetes.io/tls Secret of the S3 and admin listeners" -}}{{- end -}}
{{- if and (gt (int $total) 1) (not .Values.secrets.nodeCA) -}}{{- fail "secrets.nodeCA: name the Secret with the CA of node certificates (key ca.crt)" -}}{{- end -}}
{{- if eq .Values.kms.type "vault" -}}
{{- if or (not .Values.kms.vault.addr) (not .Values.kms.vault.key) (not .Values.kms.vault.tokenSecret) -}}{{- fail "kms.vault: addr, key and tokenSecret are required" -}}{{- end -}}
{{- else -}}
{{- if or (not .Values.kms.local.keyFileSecret) (not .Values.kms.local.passphraseSecret) -}}{{- fail "kms.local: keyFileSecret and passphraseSecret are required" -}}{{- end -}}
{{- end -}}
{{- if and .Values.localVolumes.enabled (or (not .Values.localVolumes.basePath) (not .Values.localVolumes.nodeName)) -}}{{- fail "localVolumes: basePath and nodeName are required" -}}{{- end -}}
{{- if lt (int .Values.drives.count) 1 -}}{{- fail "drives.count is at least 1" -}}{{- end -}}
{{- end -}}

{{- define "cactusfs.storageClass" -}}
{{- if .Values.localVolumes.enabled -}}{{ include "cactusfs.fullname" . }}-local{{- else -}}{{ .Values.storageClassName }}{{- end -}}
{{- end -}}

{{/* The drain delay: long enough for the readiness probe to have failed. */}}
{{- define "cactusfs.drainSeconds" -}}
{{- add (mul (int .Values.probes.periodSeconds) (int .Values.probes.failureThreshold)) (int .Values.probes.periodSeconds) -}}
{{- end -}}
