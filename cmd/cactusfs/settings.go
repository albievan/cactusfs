package main

import (
	"flag"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// The groups settings are listed under, in the order the documents print
// them.
const (
	groupListeners     = "listeners"
	groupStorage       = "storage"
	groupCluster       = "cluster"
	groupTLS           = "tls"
	groupKMS           = "kms"
	groupBootstrap     = "bootstrap"
	groupSessions      = "sessions"
	groupMaintenance   = "maintenance"
	groupObservability = "observability"
)

// settingGroup is one heading of the settings documents.
type settingGroup struct {
	// Name is the group as a setting names it; Title is the heading.
	Name, Title string
}

// settingGroups returns the groups in the order they are printed in.
func settingGroups() []settingGroup {
	return []settingGroup{
		{Name: groupListeners, Title: "Listeners: where clients reach the node"},
		{Name: groupStorage, Title: "Storage: drives and the metadata directory"},
		{Name: groupCluster, Title: "Cluster: the node among its peers"},
		{Name: groupTLS, Title: "TLS: certificates of the listeners and of the node"},
		{Name: groupKMS, Title: "KMS: the key that wraps data keys"},
		{Name: groupBootstrap, Title: "Bootstrap: the root credentials of a new deployment"},
		{Name: groupSessions, Title: "Console sessions: how long one lasts, and what becomes of it while it uploads"},
		{Name: groupMaintenance, Title: "Maintenance: background work"},
		{Name: groupObservability, Title: "Observability: log, audit export and traces"},
	}
}

// settingTargets is where the settings are parsed into: the configuration,
// and the text of the few that are parsed further once the command line has
// been read.
type settingTargets struct {
	cfg *serverConfig
	// data and driveLabels are lists; a list is taken from the variable
	// when its flag names nothing.
	data, driveLabels stringList
	// roles, peers and logLevel are parsed after the flags, so that what is
	// wrong with one is reported as every other error of the configuration
	// is and not as a mistake of the command line.
	roles, peers, logLevel string
}

// settingSpec declares one setting of "cactusfs server": its flag, its
// variable, its default, what it is for and how much of it may be shown.
// serverSettings lists every one of them, once; the command line, the
// documents and what a node says of its settings are all made from that list.
type settingSpec struct {
	// Name is the name of the setting, which is that of its flag.
	Name string
	// Env is the variable: "CACTUSFS_" and the name in upper case, with
	// underscores.
	Env string
	// Flag reports whether the setting has a flag. A secret has none,
	// because a command line is visible to every user of the machine.
	Flag bool
	// Default is the value the binary has when it is told nothing, as text;
	// empty for a setting without one.
	Default string
	// Usage is what --help says, without the variable. A word in backquotes
	// names the value, as the flag package has it.
	Usage string
	// Doc is what the documents say where they say more than --help; empty,
	// they say Usage.
	Doc string
	// DefaultNote is what the documents say of a setting whose value, when
	// none is given, is worked out or required rather than fixed.
	DefaultNote string
	// Group is the heading the setting is listed under.
	Group string
	Kind  cluster.SettingKind
	Scope cluster.SettingScope
	// bind returns where the setting is kept: a *string, *bool,
	// *time.Duration, *int or *stringList.
	bind func(*settingTargets) any
}

// settingSources records how the value of each setting arrived, by name.
type settingSources map[string]cluster.SettingSource

// durationText writes a duration as it is typed: "24h", not "24h0m0s".
func durationText(d time.Duration) string {
	text := d.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

// serverSettings returns every setting of "cactusfs server", in the order
// the documents list them. --help sorts them by name.
func serverSettings() []settingSpec {
	plain, path, url := cluster.SettingPlain, cluster.SettingPath, cluster.SettingURL
	secret, secretFile := cluster.SettingSecret, cluster.SettingSecretFile
	node, all := cluster.ScopeNode, cluster.ScopeCluster
	return []settingSpec{
		// Listeners.
		{
			Name: "s3-addr", Env: envS3Addr, Flag: true, Default: defaultS3Addr, Group: groupListeners, Kind: plain, Scope: node,
			Usage: "listen `address` of the S3 API",
			bind:  func(t *settingTargets) any { return &t.cfg.S3Addr },
		},
		{
			Name: "admin-addr", Env: envAdminAddr, Flag: true, Default: defaultAdminAddr, Group: groupListeners, Kind: plain, Scope: node,
			Usage: "listen `address` of the admin listener",
			Doc:   "listen address of the admin listener: probes, `/metrics`, the management API, Swagger UI and the console",
			bind:  func(t *settingTargets) any { return &t.cfg.AdminAddr },
		},
		{
			Name: "s3-domain", Env: envS3Domain, Flag: true, Group: groupListeners, Kind: plain, Scope: all,
			Usage: "`domain` for virtual-hosted-style requests, <bucket>.<domain>",
			bind:  func(t *settingTargets) any { return &t.cfg.S3Domain },
		},
		{
			Name: "region", Env: envRegion, Flag: true, Group: groupListeners, Kind: plain, Scope: all, DefaultNote: "`us-east-1`",
			Usage: "`region` name reported to S3 clients; default us-east-1",
			bind:  func(t *settingTargets) any { return &t.cfg.Region },
		},
		{
			Name: "drain-delay", Env: envDrainDelay, Flag: true, Default: "0s", Group: groupListeners, Kind: plain, Scope: node,
			Usage: "on shutdown, keep serving this long with /readyz answering 503 before closing the listeners",
			bind:  func(t *settingTargets) any { return &t.cfg.DrainDelay },
		},
		{
			Name: "shutdown-timeout", Env: envShutdownTimeout, Flag: true, Default: durationText(defaultShutdownTimeout),
			Group: groupListeners, Kind: plain, Scope: node,
			Usage: "time allowed for in-flight requests to finish once the listeners have closed",
			bind:  func(t *settingTargets) any { return &t.cfg.ShutdownTimeout },
		},

		// Storage.
		{
			Name: "data", Env: envData, Flag: true, Group: groupStorage, Kind: path, Scope: node,
			DefaultNote: "required on a node that stores",
			Usage:       "drive `directory`; repeat the flag or separate directories with commas",
			bind:        func(t *settingTargets) any { return &t.data },
		},
		{
			Name: "drive-labels", Env: envDriveLabels, Flag: true, Group: groupStorage, Kind: plain, Scope: node,
			Usage: "what the deployment calls the volume behind each drive directory - a compose volume, a claim - as " +
				"`labels` separated by commas, one for each --data entry in the same order, or none; shown with the " +
				"node's settings and used for nothing else",
			bind: func(t *settingTargets) any { return &t.driveLabels },
		},
		{
			Name: "meta-dir", Env: envMetaDir, Flag: true, Group: groupStorage, Kind: path, Scope: node,
			DefaultNote: "`<first data directory>/" + metaDirName + "`",
			Usage:       "metadata `directory`; default <first data directory>/" + metaDirName,
			bind:        func(t *settingTargets) any { return &t.cfg.MetaDir },
		},
		{
			Name: "allow-drive-replacement", Env: envAllowReplace, Flag: true, Default: "false", Group: groupStorage, Kind: plain, Scope: node,
			Usage: "confirm that one empty drive replaces one missing drive at this start; permanent: the missing drive is " +
				"refused afterwards",
			bind: func(t *settingTargets) any { return &t.cfg.AllowDriveReplacement },
		},

		// Cluster.
		{
			Name: "node-id", Env: envNodeID, Flag: true, Group: groupCluster, Kind: plain, Scope: node,
			DefaultNote: "generated on a node on its own",
			Usage: "`name` of this node: lowercase letters, digits and hyphens; required in a cluster, where it must be " +
				"the name in the node certificate",
			bind: func(t *settingTargets) any { return &t.cfg.NodeID },
		},
		{
			Name: "roles", Env: envRoles, Flag: true, Group: groupCluster, Kind: plain, Scope: node, DefaultNote: "all",
			Usage: "`roles` of this node: gateway, meta, storage, console; default all",
			Doc:   "roles of this node: gateway, meta, storage, console; default all. For members of a cluster only: a node on its own has every role",
			bind:  func(t *settingTargets) any { return &t.roles },
		},
		{
			Name: "peers", Env: envPeers, Flag: true, Group: groupCluster, Kind: plain, Scope: all,
			Usage: "every node of the cluster, this one included, as `id=host:port` of its node RPC, separated by commas; " +
				"makes the node a member of a cluster",
			bind: func(t *settingTargets) any { return &t.peers },
		},
		{
			Name: "rpc-addr", Env: envRPCAddr, Flag: true, Default: defaultRPCAddr, Group: groupCluster, Kind: plain, Scope: node,
			Usage: "listen `address` of node RPC",
			bind:  func(t *settingTargets) any { return &t.cfg.RPCAddr },
		},
		{
			Name: "raft-addr", Env: envRaftAddr, Flag: true, Default: defaultRaftAddr, Group: groupCluster, Kind: plain, Scope: node,
			Usage: "listen `address` of the Raft transport",
			bind:  func(t *settingTargets) any { return &t.cfg.RaftAddr },
		},
		{
			Name: "advertise-rpc", Env: envAdvertiseRPC, Flag: true, Group: groupCluster, Kind: plain, Scope: node,
			DefaultNote: "the node's entry in `--peers`",
			Usage:       "`host:port` at which other nodes reach this node's RPC; default its entry in --peers",
			bind:        func(t *settingTargets) any { return &t.cfg.AdvertiseRPC },
		},
		{
			Name: "advertise-raft", Env: envAdvertiseRaft, Flag: true, Group: groupCluster, Kind: plain, Scope: node,
			DefaultNote: "the host of `--advertise-rpc`, the port of `--raft-addr`",
			Usage: "`host:port` at which other nodes reach this node's Raft transport; default the host of " +
				"--advertise-rpc with the port of --raft-addr",
			bind: func(t *settingTargets) any { return &t.cfg.AdvertiseRaft },
		},
		{
			Name: "topology-region", Env: envTopologyRegion, Flag: true, Group: groupCluster, Kind: plain, Scope: node,
			Usage: "`region` this node is in, for placement",
			bind:  func(t *settingTargets) any { return &t.cfg.Labels.Region },
		},
		{
			Name: "topology-zone", Env: envTopologyZone, Flag: true, Group: groupCluster, Kind: plain, Scope: node,
			Usage: "`zone` this node is in, for placement",
			bind:  func(t *settingTargets) any { return &t.cfg.Labels.Zone },
		},
		{
			Name: "topology-rack", Env: envTopologyRack, Flag: true, Group: groupCluster, Kind: plain, Scope: node,
			Usage: "`rack` this node is in, for placement",
			bind:  func(t *settingTargets) any { return &t.cfg.Labels.Rack },
		},
		{
			Name: "topology-host", Env: envTopologyHost, Flag: true, Group: groupCluster, Kind: plain, Scope: node,
			Usage: "`host` this node runs on, for placement",
			bind:  func(t *settingTargets) any { return &t.cfg.Labels.Host },
		},

		// TLS.
		{
			Name: "tls-cert", Env: envTLSCert, Flag: true, Group: groupTLS, Kind: path, Scope: node,
			DefaultNote: "required, unless `--dev-plaintext`",
			Usage:       "`path` of the PEM certificate chain",
			Doc:         "path of the PEM certificate chain of the S3 and admin listeners",
			bind:        func(t *settingTargets) any { return &t.cfg.TLSCert },
		},
		{
			Name: "tls-key", Env: envTLSKey, Flag: true, Group: groupTLS, Kind: secretFile, Scope: node,
			DefaultNote: "required, unless `--dev-plaintext`",
			Usage:       "`path` of the PEM private key",
			Doc:         "path of the PEM private key of that certificate",
			bind:        func(t *settingTargets) any { return &t.cfg.TLSKey },
		},
		{
			Name: "tls-reload-interval", Env: envTLSReload, Flag: true, Default: durationText(nodetls.DefaultReloadInterval),
			Group: groupTLS, Kind: plain, Scope: node,
			Usage: "how often the certificate and key files, the node's too, are read again, so that a certificate is replaced " +
				"without a restart; at least " + nodetls.MinReloadInterval.String() + ", 0 never reads them again; " +
				"the node CA is read at start only",
			Doc: "how often the certificate and key files, the node's too, are read again (at least `" +
				durationText(nodetls.MinReloadInterval) + "`; `0` never reads them again). A new pair that is valid is used for " +
				"new connections without a restart; one that is not is refused and logged, and the old one stays " +
				"(`cactusfs_tls_reload_pending` in `/metrics` is 1 while it does). The node CA is read at start only: a new CA " +
				"needs a restart. Rotation is not revocation: a replaced certificate and its key remain accepted by peers until " +
				"that certificate expires; for a compromised key, rotate the CA or remove the node. The files must be regular " +
				"files (not pipes)",
			bind: func(t *settingTargets) any { return &t.cfg.TLSReloadInterval },
		},
		{
			Name: "dev-plaintext", Env: envDevPlaintext, Flag: true, Default: "false", Group: groupTLS, Kind: plain, Scope: node,
			Usage: "DEVELOPMENT ONLY: serve HTTP without TLS, on a loopback IP address only",
			bind:  func(t *settingTargets) any { return &t.cfg.DevPlaintext },
		},
		{
			Name: "node-tls-cert", Env: envNodeTLSCert, Flag: true, Group: groupTLS, Kind: path, Scope: node,
			DefaultNote: "required in a cluster",
			Usage:       "`path` of the node certificate for mutual TLS between nodes",
			bind:        func(t *settingTargets) any { return &t.cfg.NodeTLSCert },
		},
		{
			Name: "node-tls-key", Env: envNodeTLSKey, Flag: true, Group: groupTLS, Kind: secretFile, Scope: node,
			DefaultNote: "required in a cluster",
			Usage:       "`path` of the node certificate's private key",
			bind:        func(t *settingTargets) any { return &t.cfg.NodeTLSKey },
		},
		{
			Name: "node-tls-ca", Env: envNodeTLSCA, Flag: true, Group: groupTLS, Kind: path, Scope: node,
			DefaultNote: "required in a cluster",
			Usage:       "`path` of the CA certificate that issues the cluster's node certificates, and nothing else",
			bind:        func(t *settingTargets) any { return &t.cfg.NodeTLSCA },
		},

		// KMS.
		{
			Name: "kms", Env: envKMS, Flag: true, Group: groupKMS, Kind: plain, Scope: all, DefaultNote: "required",
			Usage: "key management `service`: \"" + kmsVault + "\" for HashiCorp Vault Transit, \"" + kmsLocal +
				"\" for a sealed local key file; there is no default",
			Doc: "key management service: `" + kmsVault + "` for HashiCorp Vault Transit, `" + kmsLocal +
				"` for a sealed local key file. There is no default and no unencrypted mode",
			bind: func(t *settingTargets) any { return &t.cfg.KMS },
		},
		{
			Name: "kms-key-file", Env: envKMSKeyFile, Flag: true, Group: groupKMS, Kind: secretFile, Scope: node,
			DefaultNote: "`<metadata directory>/" + kmsKeyFileName + "`; required in a cluster",
			Usage:       "`path` of the local KMS key file; default <metadata directory>/" + kmsKeyFileName,
			bind:        func(t *settingTargets) any { return &t.cfg.KMSKeyFile },
		},
		{
			Name: "kms-passphrase", Env: envKMSPhrase, Group: groupKMS, Kind: secret, Scope: node,
			DefaultNote: "required with the local KMS, or `--kms-passphrase-file`",
			Usage:       "passphrase that opens the local KMS key file",
			bind:        func(t *settingTargets) any { return &t.cfg.secrets.kmsPassphrase },
		},
		{
			Name: "kms-passphrase-file", Env: envKMSPhraseFile, Flag: true, Group: groupKMS, Kind: secretFile, Scope: node,
			Usage: "`path` of a file holding the local KMS passphrase, instead of $" + envKMSPhrase,
			bind:  func(t *settingTargets) any { return &t.cfg.KMSPassphraseFile },
		},
		{
			Name: "vault-addr", Env: envVaultAddr, Flag: true, Group: groupKMS, Kind: url, Scope: all,
			DefaultNote: "required with `--kms " + kmsVault + "`",
			Usage:       "`URL` of Vault, https://host:port",
			bind:        func(t *settingTargets) any { return &t.cfg.VaultAddr },
		},
		{
			Name: "vault-transit-mount", Env: envVaultMount, Flag: true, Group: groupKMS, Kind: plain, Scope: all,
			DefaultNote: "`transit`",
			Usage:       "`path` of Vault's Transit engine; default transit",
			bind:        func(t *settingTargets) any { return &t.cfg.VaultMount },
		},
		{
			Name: "vault-key", Env: envVaultKey, Flag: true, Group: groupKMS, Kind: plain, Scope: all,
			DefaultNote: "required with `--kms " + kmsVault + "`",
			Usage:       "`name` of the Transit key, of an AEAD type such as aes256-gcm96",
			bind:        func(t *settingTargets) any { return &t.cfg.VaultKey },
		},
		{
			Name: "vault-token-file", Env: envVaultAuthFile, Flag: true, Group: groupKMS, Kind: secretFile, Scope: node,
			DefaultNote: "required with `--kms " + kmsVault + "`",
			Usage:       "`path` of a file holding the Vault token; read at every use, so it can be renewed in place",
			bind:        func(t *settingTargets) any { return &t.cfg.VaultTokenFile },
		},
		{
			Name: "vault-ca-cert", Env: envVaultCACert, Flag: true, Group: groupKMS, Kind: path, Scope: node,
			DefaultNote: "the system's CAs",
			Usage:       "`path` of the CA certificate that issued Vault's certificate; default the system's CAs",
			bind:        func(t *settingTargets) any { return &t.cfg.VaultCACert },
		},
		{
			Name: "vault-server-name", Env: envVaultServerName, Flag: true, Group: groupKMS, Kind: plain, Scope: node,
			Usage: "`name` expected in Vault's certificate, when Vault is reached under another",
			bind:  func(t *settingTargets) any { return &t.cfg.VaultServerName },
		},
		{
			Name: "vault-namespace", Env: envVaultNamespace, Flag: true, Group: groupKMS, Kind: plain, Scope: all,
			Usage: "Vault Enterprise `namespace`",
			bind:  func(t *settingTargets) any { return &t.cfg.VaultNamespace },
		},

		// Bootstrap.
		{
			Name: "root-access-key", Env: envRootKeyID, Group: groupBootstrap, Kind: secret, Scope: node,
			DefaultNote: "generated, and shown once, by a new deployment",
			Usage:       "access key ID of the root credentials a new deployment begins with; set it together with $" + envRootKeyValue,
			bind:        func(t *settingTargets) any { return &t.cfg.secrets.rootAccessKey },
		},
		{
			Name: "root-secret-key", Env: envRootKeyValue, Group: groupBootstrap, Kind: secret, Scope: node,
			DefaultNote: "generated, and shown once, by a new deployment",
			Usage:       "secret key of the root credentials a new deployment begins with; set it together with $" + envRootKeyID,
			bind:        func(t *settingTargets) any { return &t.cfg.secrets.rootSecretKey },
		},

		// Console sessions (ADR-0027). The same on every node: any gateway
		// may be asked what a session is.
		{
			Name: "session-lifetime", Env: envSessionLifetime, Flag: true, Default: durationText(app.DefaultSessionLifetime),
			Group: groupSessions, Kind: plain, Scope: all,
			Usage: "how long a console session lasts, from sign-in or from its last unlock, before it reaches its timeout",
			Doc: "how long a console session lasts, from sign-in or from its last unlock, before it reaches its timeout (`" +
				durationText(minSessionLifetime) + "` to `" + durationText(maxSessionLifetime) + "`). Nothing extends it: at the timeout " +
				"the user signs in again, or - while objects are being uploaded with the session - it is locked until the user's password unlocks it. " +
				"It must be the same on every node: any gateway may be asked what a session is",
			bind: func(t *settingTargets) any { return &t.cfg.SessionLifetime },
		},
		{
			Name: "session-upload-grace", Env: envSessionGrace, Flag: true, Default: durationText(app.DefaultSessionUploadGrace),
			Group: groupSessions, Kind: plain, Scope: all,
			Usage: "how long after its last upload a console session that is locked ends",
			Doc: "how long after its last upload a console session that is locked ends (`" + durationText(minSessionGrace) + "` to `" +
				durationText(maxSessionGrace) + "`). While a session uploads, each node it uploads through writes one entry to the metadata log in a third of this time. " +
				"It must be the same on every node",
			bind: func(t *settingTargets) any { return &t.cfg.SessionUploadGrace },
		},
		{
			Name: "session-lock-max", Env: envSessionLockMax, Flag: true, Default: durationText(app.DefaultSessionLockMax),
			Group: groupSessions, Kind: plain, Scope: all,
			Usage: "the longest a console session stays locked, counted from its timeout, whatever is being uploaded; 0: a session never locks",
			Doc: "the longest a console session that reached its timeout while it uploads stays locked - able to store objects and nothing else - " +
				"counted from the timeout, whatever is still being uploaded (at most `" + durationText(maxSessionLockMax) +
				"`); `0`: a session never locks, and ends at its timeout. It must be the same on every node: a gateway with a smaller value " +
				"takes for ended what another takes for locked, and the node that leads the metadata group removes session records by its own",
			bind: func(t *settingTargets) any { return &t.cfg.SessionLockMax },
		},

		// Maintenance.
		{
			Name: "scrub-interval", Env: envScrubInterval, Flag: true, Default: durationText(defaultScrubInterval),
			Group: groupMaintenance, Kind: plain, Scope: all,
			Usage: "pause between two passes of the scrubber, which verifies and heals every object; 0 disables it",
			bind:  func(t *settingTargets) any { return &t.cfg.ScrubInterval },
		},
		{
			Name: "scrub-rate-mib", Env: envScrubRate, Flag: true, Default: strconv.Itoa(defaultScrubRateMiB),
			Group: groupMaintenance, Kind: plain, Scope: all,
			Usage: "limit on how fast the scrubber reads, in MiB per second; 0 is no limit",
			bind:  func(t *settingTargets) any { return &t.cfg.ScrubRateMiB },
		},
		{
			Name: "lifecycle-interval", Env: envLifecycle, Flag: true, Default: durationText(defaultLifecycle),
			Group: groupMaintenance, Kind: plain, Scope: all,
			Usage: "pause between two passes of the worker that carries out the lifecycle rules of buckets; 0 disables it",
			bind:  func(t *settingTargets) any { return &t.cfg.LifecycleInterval },
		},
		{
			Name: "gc-interval", Env: envGCInterval, Flag: true, Default: durationText(defaultGCInterval),
			Group: groupMaintenance, Kind: plain, Scope: node,
			Usage: "pause between two passes of the garbage collector for unreferenced shards; 0 disables it",
			bind:  func(t *settingTargets) any { return &t.cfg.GCInterval },
		},
		{
			Name: "gc-grace", Env: envGCGrace, Flag: true, Default: durationText(defaultGCGrace),
			Group: groupMaintenance, Kind: plain, Scope: all,
			Usage: "minimum age of an unreferenced shard before it is removed",
			Doc:   "minimum age of an unreferenced shard before it is removed (at least `" + durationText(minGCGrace) + "`)",
			bind:  func(t *settingTargets) any { return &t.cfg.GCGrace },
		},
		{
			Name: "access-flush-interval", Env: envAccessFlush, Flag: true, Default: durationText(app.DefaultAccessFlushInterval),
			Group: groupMaintenance, Kind: plain, Scope: node,
			Usage: "how often the downloads this node counted are recorded with the objects, at least " + app.MinAccessFlushInterval.String() +
				"; a node that crashes loses up to this much of the count",
			bind: func(t *settingTargets) any { return &t.cfg.AccessFlushInterval },
		},

		// Observability.
		{
			Name: "log-level", Env: envLogLevel, Flag: true, Default: defaultLogLevel, Group: groupObservability, Kind: plain, Scope: node,
			Usage: "minimum log `level`: debug, info, warn or error",
			bind:  func(t *settingTargets) any { return &t.logLevel },
		},
		{
			Name: "audit-export", Env: envAuditExport, Flag: true, Default: "false", Group: groupObservability, Kind: plain, Scope: node,
			Usage: "write every audit record to standard output as a JSON line with \"stream\":\"audit\"",
			bind:  func(t *settingTargets) any { return &t.cfg.AuditExport },
		},
		{
			Name: "otlp-endpoint", Env: envOTLPEndpoint, Flag: true, Group: groupObservability, Kind: url, Scope: node,
			Usage: "`URL` of an OTLP/HTTP trace collector; traces are not exported when empty",
			Doc: "URL of an OTLP/HTTP trace collector, `https` unless the host is a loopback IP address; traces are not exported " +
				"when empty. A URL with userinfo, a query or a fragment is refused: a credential does not belong in it, and a " +
				"credential for the collector is not a setting of CactusFS",
			bind: func(t *settingTargets) any { return &t.cfg.OTLPEndpoint },
		},
	}
}

// declare gives the setting its value from the variable, or else its
// default, and declares its flag, which wins over both when it is given. It
// returns where the value came from so far. A variable that is set to
// nothing counts as not set. A list is not read from its variable here: the
// variable is its value only when the flag names nothing, which is known
// once the command line has been read (see parseServerSettings).
func (s settingSpec) declare(fs *flag.FlagSet, targets *settingTargets, getenv func(string) string) (cluster.SettingSource, error) {
	raw, source := s.Default, cluster.SourceDefault
	if value := getenv(s.Env); value != "" {
		raw, source = value, cluster.SourceEnvironment
	}
	usage := s.Usage + " ($" + s.Env + ")"
	switch target := s.bind(targets).(type) {
	case *string:
		*target = raw
		if s.Flag {
			fs.StringVar(target, s.Name, raw, usage)
		}
	case *bool:
		value := false
		if raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				return "", fmt.Errorf("%s=%q: want true or false", s.Env, raw)
			}
			value = parsed
		}
		fs.BoolVar(target, s.Name, value, usage)
	case *time.Duration:
		var value time.Duration
		if raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return "", fmt.Errorf("%s=%q: want a duration such as 30s", s.Env, raw)
			}
			value = parsed
		}
		fs.DurationVar(target, s.Name, value, usage)
	case *int:
		value := 0
		if raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				return "", fmt.Errorf("%s=%q: want a whole number", s.Env, raw)
			}
			value = parsed
		}
		fs.IntVar(target, s.Name, value, usage)
	case *stringList:
		fs.Var(target, s.Name, usage)
		return cluster.SourceDefault, nil
	default:
		return "", fmt.Errorf("setting %s: nothing to keep a %T in", s.Name, target)
	}
	return source, nil
}

// secretPlaceholder stands for the value of a secret wherever settings are
// handled as text: what is known of a secret outside the configuration is
// whether it is set.
const secretPlaceholder = "(set)"

// text returns the value the setting has in targets, as it is typed. Of a
// secret, and of the path of a file that holds one, it returns
// secretPlaceholder when there is a value and never the value.
func (s settingSpec) text(targets *settingTargets) string {
	var text string
	switch target := s.bind(targets).(type) {
	case *string:
		text = *target
	case *bool:
		text = strconv.FormatBool(*target)
	case *time.Duration:
		text = durationText(*target)
	case *int:
		text = strconv.Itoa(*target)
	case *stringList:
		text = target.String()
	}
	if text != "" && (s.Kind == cluster.SettingSecret || s.Kind == cluster.SettingSecretFile) {
		return secretPlaceholder
	}
	return text
}

// settingTexts returns the value of every setting of a configuration, by
// name, as settingSpec.text gives it.
func settingTexts(cfg serverConfig, specs []settingSpec) map[string]string {
	targets := settingTargets{
		cfg: &cfg, data: cfg.Data, driveLabels: cfg.DriveLabels,
		roles: cfg.Roles.String(), peers: peersText(cfg.Peers), logLevel: strings.ToLower(cfg.LogLevel.String()),
	}
	texts := make(map[string]string, len(specs))
	for _, spec := range specs {
		texts[spec.Name] = spec.text(&targets)
	}
	return texts
}

// peersText writes the peers as --peers takes them, by node ID.
func peersText(peers map[string]string) string {
	items := make([]string, 0, len(peers))
	for _, id := range slices.Sorted(maps.Keys(peers)) {
		items = append(items, id+"="+peers[id])
	}
	return strings.Join(items, ",")
}

// shownSettings returns the settings of a configuration as they may be
// shown, each through cluster.NewSetting, which redacts it by its kind. The
// value of a secret, and the path of a file that holds one, never reach it:
// what it is given for those is secretPlaceholder (settingSpec.text).
//
// A configuration that was not parsed - one a test built by hand - has no
// record of where its values came from; they are then said to be defaults.
func shownSettings(cfg serverConfig) []cluster.Setting {
	specs := serverSettings()
	texts := settingTexts(cfg, specs)
	shown := make([]cluster.Setting, 0, len(specs))
	for _, spec := range specs {
		source := cfg.sources[spec.Name]
		if !source.Valid() {
			source = cluster.SourceDefault
		}
		flagName := ""
		if spec.Flag {
			flagName = spec.Name
		}
		shown = append(shown, cluster.NewSetting(spec.Name, spec.Env, flagName, spec.Group, spec.Kind, spec.Scope, source, texts[spec.Name]))
	}
	return shown
}

// maxDriveLabel bounds the label of a drive, in bytes.
const maxDriveLabel = 128

// validDriveLabel reports whether label can name a volume: 1 to 128 letters,
// digits, dots, underscores, slashes and hyphens - what the names of compose
// volumes and of claims are made of, and nothing a table or a log could be
// misled by.
func validDriveLabel(label string) bool {
	if label == "" || len(label) > maxDriveLabel {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !letter && c != '.' && c != '_' && c != '/' && c != '-' {
			return false
		}
	}
	return true
}
