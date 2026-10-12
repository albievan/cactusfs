{{/* The release name, as it is: node IDs, Secrets and Services are named by it. */}}
{{- define "cactusfs.fullname" -}}
{{- .Release.Name -}}
{{- end -}}

{{/* The version that is deployed: the tag of the image - <version>-<build> -
     or the chart's appVersion when no tag is given; as a label value, so what
     a label may not hold is replaced and it is at most 63 characters. */}}
{{- define "cactusfs.version" -}}
{{- regexReplaceAll "[^A-Za-z0-9_.-]+" (.Values.image.tag | default .Chart.AppVersion | toString) "_" | trunc 63 | trimAll "-_." -}}
{{- end -}}

{{- define "cactusfs.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ include "cactusfs.version" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Labels of pods: nothing that changes when only the chart does, or every
     node would be restarted by an upgrade that changes nothing of theirs.
     These select the pods; a pod also says the version it runs (see the
     StatefulSet), which changes when its image does and never selects. */}}
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
{{- $root := . -}}
{{- if or (gt (len .Release.Name) 40) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .Release.Name)) -}}{{- fail (printf "the release name %q names every node: lower-case letters, digits and hyphens, at most 40 characters" .Release.Name) -}}{{- end -}}
{{- $voters := 0 -}}{{- $stores := 0 -}}{{- $total := 0 -}}{{- $names := dict -}}
{{- range .Values.pools -}}
{{- if not (kindIs "string" (.roles | default "")) -}}{{- fail (printf "pools: roles of pool %q is a string of names separated by commas" .name) -}}{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" (.name | default "")) -}}{{- fail (printf "pools: the name %q is not lower-case letters, digits and hyphens" (.name | default "")) -}}{{- end -}}
{{- if hasKey $names .name -}}{{- fail (printf "pools: two pools are named %q" .name) -}}{{- end -}}
{{- $_ := set $names .name true -}}
{{- if lt (int .replicas) 0 -}}{{- fail (printf "pools: pool %q has a negative number of replicas" .name) -}}{{- end -}}
{{- if gt (len (printf "%s-%s-%d" $root.Release.Name .name (int .replicas))) 52 -}}{{- fail (printf "pools: the node names of pool %q are longer than 52 characters" .name) -}}{{- end -}}
{{- range (splitList "," ((.roles | default "") | replace " " "")) -}}
{{- if and . (not (has . (list "gateway" "meta" "storage" "console"))) -}}{{- fail (printf "pools: %q is not a role (gateway, meta, storage, console)" .) -}}{{- end -}}
{{- end -}}
{{- $total = add $total (int .replicas) -}}
{{- if include "cactusfs.hasRole" (dict "roles" .roles "role" "meta") -}}{{- $voters = add $voters (int .replicas) -}}{{- end -}}
{{- if include "cactusfs.hasRole" (dict "roles" .roles "role" "storage") -}}{{- $stores = add $stores (int .replicas) -}}{{- end -}}
{{- end -}}
{{- if lt (int $total) 1 -}}{{- fail "pools: the cluster has no node" -}}{{- end -}}
{{- if eq (int $total) 1 -}}
{{- range .Values.pools -}}{{- if and (gt (int .replicas) 0) .roles -}}{{- fail "pools: a node on its own has every role; leave its roles empty" -}}{{- end -}}{{- end -}}
{{- else -}}
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
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.egressTo) -}}{{- fail "networkPolicy.egressTo: the nodes keep their keys in Vault and the policy lets them reach nothing: say where Vault is" -}}{{- end -}}
{{- else -}}
{{- if or (not .Values.kms.local.keyFileSecret) (not .Values.kms.local.passphraseSecret) -}}{{- fail "kms.local: keyFileSecret and passphraseSecret are required" -}}{{- end -}}
{{- end -}}
{{- if and .Values.localVolumes.enabled (or (not .Values.localVolumes.basePath) (not .Values.localVolumes.nodeName)) -}}{{- fail "localVolumes: basePath and nodeName are required" -}}{{- end -}}
{{- if lt (int .Values.drives.count) 1 -}}{{- fail "drives.count is at least 1" -}}{{- end -}}
{{- end -}}

{{- define "cactusfs.storageClass" -}}
{{- if .Values.localVolumes.enabled -}}{{ .Release.Namespace }}-{{ include "cactusfs.fullname" . }}-local{{- else -}}{{ .Values.storageClassName }}{{- end -}}
{{- end -}}

{{/* The drain delay: long enough for the readiness probe to have failed. */}}
{{- define "cactusfs.drainSeconds" -}}
{{- add (mul (int .Values.probes.periodSeconds) (int .Values.probes.failureThreshold)) (int .Values.probes.periodSeconds) -}}
{{- end -}}

{{- define "cactusfs.image" -}}
{{- if .Values.image.digest -}}{{ .Values.image.repository }}@{{ .Values.image.digest }}{{- else -}}{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}{{- end -}}
{{- end -}}
