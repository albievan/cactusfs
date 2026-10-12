package cluster

import (
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// SettingKind says what the value of a setting is, and with that how much of
// it may be shown: redaction goes by the kind and by nothing else.
type SettingKind string

// The kinds of a setting.
const (
	// SettingPlain is a value that may be shown as it is: an address to
	// listen on, an interval, a name.
	SettingPlain SettingKind = "plain"
	// SettingPath is a file or a directory that holds nothing secret: a
	// drive, a certificate.
	SettingPath SettingKind = "path"
	// SettingURL is an address of another service. It is shown without
	// userinfo, query and fragment (RedactURL), which is where a credential
	// travels in a URL.
	SettingURL SettingKind = "url"
	// SettingSecret is a secret: a passphrase, a key. Only whether it is set
	// is ever known outside the process that read it.
	SettingSecret SettingKind = "secret"
	// SettingSecretFile is the path of a file that holds a secret. The path
	// is not shown either: it says where to look, and whoever reads the
	// settings has no need of that.
	SettingSecretFile SettingKind = "secret-file"
)

// Valid reports whether k is one of the kinds.
func (k SettingKind) Valid() bool {
	switch k {
	case SettingPlain, SettingPath, SettingURL, SettingSecret, SettingSecretFile:
		return true
	}
	return false
}

// shows reports whether a value of this kind may be shown at all. A kind
// that is not known is treated as a secret: a setting declared by a newer
// release, or by mistake, must not be the one that leaks.
func (k SettingKind) shows() bool {
	return k == SettingPlain || k == SettingPath || k == SettingURL
}

// SettingScope says whose a setting is.
type SettingScope string

// The scopes of a setting.
const (
	// ScopeNode is a setting each node has for itself: its drives, its
	// addresses, its labels.
	ScopeNode SettingScope = "node"
	// ScopeCluster is a setting that must be the same on every node: the
	// region, the KMS. Nodes that differ in one are reported
	// (SettingMismatches).
	ScopeCluster SettingScope = "cluster"
)

// Valid reports whether s is one of the scopes.
func (s SettingScope) Valid() bool {
	return s == ScopeNode || s == ScopeCluster
}

// SettingSource says how a value arrived.
type SettingSource string

// The sources of a setting.
const (
	// SourceDefault is the value the binary has when it is told nothing.
	SourceDefault SettingSource = "default"
	// SourceEnvironment is a CACTUSFS_* variable.
	SourceEnvironment SettingSource = "environment"
	// SourceFlag is the command line, which wins over the variable.
	SourceFlag SettingSource = "flag"
	// SourceDerived is a value worked out from other settings: the metadata
	// directory inside the first drive, the address a node advertises.
	SourceDerived SettingSource = "derived"
)

// Valid reports whether s is one of the sources.
func (s SettingSource) Valid() bool {
	switch s {
	case SourceDefault, SourceEnvironment, SourceFlag, SourceDerived:
		return true
	}
	return false
}

// MaxSettingValueBytes bounds the value of a setting, in bytes. Settings are
// given by the operator, but they are sent from node to node and to the
// console, and nothing that crosses a wire is without a bound. No setting
// CactusFS has comes near it; the longest are lists of peers and of drives.
const MaxSettingValueBytes = 4096

// TruncationMarker ends a value that was longer than MaxSettingValueBytes,
// so that a cut value is not taken for the whole. It holds none of the
// characters RedactURL looks for, which keeps redaction idempotent.
const TruncationMarker = "...[truncated]"

// Setting is one setting of one node as it may be shown: its value is
// redacted according to its kind.
//
// NewSetting is the only constructor. A Setting built any other way - a
// literal, a copy with its fields changed - is a programming error, because
// nothing has redacted it. Since that cannot be ruled out by the compiler,
// whatever sends settings on applies Redacted once more (NodeSettings does in
// Normalized): redaction is enforced where the value is read and again at
// each wire.
type Setting struct {
	// Name is the name of the setting, which is that of its flag where it
	// has one: "s3-addr".
	Name string
	// Env is the environment variable: "CACTUSFS_S3_ADDR".
	Env string
	// Flag is the flag, without dashes; empty for a secret, which has none
	// because a command line is visible to every user of the machine.
	Flag string
	// Group is the heading the setting is listed under.
	Group  string
	Kind   SettingKind
	Scope  SettingScope
	Source SettingSource
	// Set reports whether the setting has a value. For a secret it is all
	// that is known.
	Set bool
	// Value is the redacted value: empty for the secret kinds, without
	// userinfo, query and fragment for a URL, at most MaxSettingValueBytes
	// of valid UTF-8 in any case.
	Value string
}

// NewSetting returns the setting with raw redacted according to kind:
//
//   - SettingSecret and SettingSecretFile keep nothing of raw;
//   - SettingURL keeps RedactURL(raw);
//   - SettingPlain and SettingPath keep raw;
//   - a kind that is not one of these is treated as a secret.
//
// What is kept is made valid UTF-8 (a byte that is not becomes U+FFFD, since
// the wire formats refuse it) and, when longer than MaxSettingValueBytes, cut
// at a rune boundary and ended with TruncationMarker. Set reports whether raw
// was given, whatever became of it.
func NewSetting(name, env, flag, group string, kind SettingKind, scope SettingScope, source SettingSource, raw string) Setting {
	return Setting{
		Name: name, Env: env, Flag: flag, Group: group,
		Kind: kind, Scope: scope, Source: source,
		Set: raw != "", Value: raw,
	}.Redacted()
}

// Redacted returns the setting with the rule of NewSetting applied to the
// value it holds. On a setting that NewSetting made it changes nothing; on
// one made by hand it removes what must not be shown. A setting that holds a
// value is set, whatever it said of itself.
func (s Setting) Redacted() Setting {
	s.Set = s.Set || s.Value != ""
	switch {
	case !s.Kind.shows():
		s.Value = ""
	case s.Kind == SettingURL:
		s.Value = bounded(RedactURL(s.Value))
	default:
		s.Value = bounded(s.Value)
	}
	return s
}

// The words a secret is shown as.
const (
	shownSet    = "set"
	shownNotSet = "not set"
)

// shown returns what is compared and listed of a setting: its redacted
// value, or for a secret whether it is set.
func (s Setting) shown() string {
	s = s.Redacted()
	switch {
	case s.Kind.shows():
		return s.Value
	case s.Set:
		return shownSet
	}
	return shownNotSet
}

// explicit reports whether somebody chose the value: it is set, and not by
// the default of the binary.
func (s Setting) explicit() bool {
	return s.Set && s.Source != SourceDefault
}

// bounded returns value as valid UTF-8 of at most MaxSettingValueBytes.
func bounded(value string) string {
	value = strings.ToValidUTF8(value, string(utf8.RuneError))
	if len(value) <= MaxSettingValueBytes {
		return value
	}
	cut := MaxSettingValueBytes - len(TruncationMarker)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + TruncationMarker
}

// RedactURL returns raw without the parts of a URL that carry credentials:
// userinfo, query and fragment. It takes anything - a URL, "host:port" as an
// OTLP endpoint is often given, or what is neither - and decides by
// characters alone, in this order:
//
//  1. If raw has "://" and what stands before the first is a scheme (a
//     letter, then letters, digits, "+", "-" and "."), the scheme and "://"
//     are kept and the rules apply to what follows. Otherwise they apply to
//     all of raw.
//  2. Everything from the first "?" or "#" on is dropped.
//  3. Of what is left, everything up to and including the last "@" is
//     dropped.
//  4. If what rule 2 dropped held an "@", nothing is left but the scheme.
//
// So the result is raw with pieces removed, never longer, and outside the
// scheme it has no "@", "?" or "#"; redacting it again changes nothing.
//
// Rule 3 takes the last "@" and not the last before the first "/": a
// password that was not percent-encoded may hold a "/", and a path shown
// from its last "@" on shows nothing secret. Rule 4 is for what cannot be
// told apart: an "@" after a "?" or "#" is either in a query, or the end of
// a userinfo whose password holds a "?" or "#". Each reading makes a secret
// of what the other would show, so neither is shown.
//
// The price is that a URL with "@" in its path loses what stands before it,
// and one with "@" in its query or fragment is shown as its scheme alone:
// less than could be, and nothing that was secret.
//
// A URL whose path is itself the secret is not helped by this: such a
// setting is of kind SettingSecret.
func RedactURL(raw string) string {
	scheme, rest := "", raw
	if i := strings.Index(raw, "://"); i >= 0 && isScheme(raw[:i]) {
		scheme, rest = raw[:i+len("://")], raw[i+len("://"):]
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		if strings.Contains(rest[i:], "@") {
			return scheme
		}
		rest = rest[:i]
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	return scheme + rest
}

// isScheme reports whether s is the scheme of a URL as RFC 3986 spells one.
func isScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		other := (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
		if !letter && (!other || i == 0) {
			return false
		}
	}
	return s != ""
}

// RedactDevice returns the source of a mount as it may be shown. A device, a
// label or a word - "/dev/sda1", "UUID=...", "overlay", "tmpfs", an export
// "host:/path" - is returned as it is. What could name a credential goes
// through RedactURL: anything with "://", anything with an "@" (the user of
// "user@host:/path"), and a share, "//host/share", which keeps its two
// slashes and is otherwise redacted like the rest.
//
// The result has no "@" outside a scheme, and redacting it again changes
// nothing. What a mount was given as options is not part of its source and
// is never shown at all.
func RedactDevice(source string) string {
	share := strings.HasPrefix(source, "//")
	if !share && !strings.Contains(source, "://") && !strings.Contains(source, "@") {
		return source
	}
	out := RedactURL(strings.TrimLeft(source, "/"))
	if !share {
		out = RedactURL(source)
	}
	// What is, or has become, a share has exactly its two slashes: so that
	// what was redacted once reads the same when redacted again.
	if share || strings.HasPrefix(out, "//") {
		out = "//" + strings.TrimLeft(out, "/")
	}
	return out
}

// Filesystem is the file system behind a directory, where the node can tell:
// on Linux, from the table of mounts. All of it is empty elsewhere.
type Filesystem struct {
	// Type is the kind of file system: "ext4", "xfs", "overlay".
	Type string
	// Device is what is mounted.
	Device string
	// MountPoint is where, as the node sees it.
	MountPoint string
}

// Volume is a directory a node stores in, and what is known of what lies
// behind it.
type Volume struct {
	// Directory is the directory as the node sees it: inside a container,
	// the path in the container.
	Directory string
	// Label is what the deployment calls the volume - the compose volume,
	// the claim. A process cannot know that name; it is shown as it was
	// told, and is empty when it was told nothing.
	Label      string
	Filesystem Filesystem
	TotalBytes uint64
	FreeBytes  uint64
}

// ErasureSetUnknown is the erasure set of a drive whose set is not known:
// the drive is not in the topology, or nobody looked.
const ErasureSetUnknown = -1

// DriveSettings is one drive of a node: where it is and what it belongs to.
type DriveSettings struct {
	// ID is the drive's generated identifier.
	ID string
	Volume
	// Online reports whether the node can use the drive.
	Online bool
	// ErasureSet is the index of the erasure set the drive is in, or
	// ErasureSetUnknown.
	ErasureSet int
}

// The uses of a certificate.
const (
	// CertificateS3 is the certificate of the listeners clients reach: the
	// S3 API, the admin API and the console.
	CertificateS3 = "s3"
	// CertificateNode is the certificate a node shows other nodes, for node
	// RPC and Raft.
	CertificateNode = "node"
)

// Certificate is what may be said of a certificate a node serves: who it
// names and until when. Nothing of the key.
type Certificate struct {
	// Use is CertificateS3 or CertificateNode.
	Use         string
	Subject     string
	Issuer      string
	DNSNames    []string
	IPAddresses []string
	NotBefore   time.Time
	NotAfter    time.Time
}

// Build names the build of a binary.
type Build struct {
	// Version is the release: "1.4.0".
	Version string
	// Build is the number of the build.
	Build string
	// Commit is the commit it was built from.
	Commit string
}

// NodeSettings is what a node says of itself when asked: how it was
// configured, what it stores on and what it shows its peers. It is read from
// the node each time and stored nowhere.
type NodeSettings struct {
	NodeID string
	// Roles are the roles the node runs, by name.
	Roles []string
	// Labels are the placement labels that are set: region, zone, rack,
	// host.
	Labels map[string]string
	Build  Build
	// StartedAt is when the process started.
	StartedAt time.Time
	// KMSKeyID names the key that wraps data keys. It is a name, not key
	// material.
	KMSKeyID     string
	Settings     []Setting
	Drives       []DriveSettings
	Metadata     Volume
	Certificates []Certificate
}

func settingOrder(a, b Setting) int {
	if c := strings.Compare(a.Name, b.Name); c != 0 {
		return c
	}
	if c := strings.Compare(a.Env, b.Env); c != 0 {
		return c
	}
	return strings.Compare(a.Flag, b.Flag)
}

func certificateOrder(a, b Certificate) int {
	if c := strings.Compare(a.Use, b.Use); c != 0 {
		return c
	}
	if c := strings.Compare(a.Subject, b.Subject); c != 0 {
		return c
	}
	return a.NotAfter.Compare(b.NotAfter)
}

// Normalized returns the settings in the one form they are sent and compared
// in: settings by name, drives by ID, certificates by use and then subject,
// and every setting redacted once more (Setting.Redacted). The result shares
// no slice and no map with the receiver.
func (n NodeSettings) Normalized() NodeSettings {
	out := n
	out.Roles = slices.Clone(n.Roles)
	out.Labels = maps.Clone(n.Labels)

	out.Settings = slices.Clone(n.Settings)
	for i, setting := range out.Settings {
		out.Settings[i] = setting.Redacted()
	}
	slices.SortStableFunc(out.Settings, settingOrder)

	out.Drives = slices.Clone(n.Drives)
	slices.SortStableFunc(out.Drives, func(a, b DriveSettings) int { return strings.Compare(a.ID, b.ID) })

	out.Certificates = slices.Clone(n.Certificates)
	for i, cert := range out.Certificates {
		out.Certificates[i].DNSNames = slices.Clone(cert.DNSNames)
		out.Certificates[i].IPAddresses = slices.Clone(cert.IPAddresses)
	}
	slices.SortStableFunc(out.Certificates, certificateOrder)
	return out
}

// AnswerReason says why a node did not give its settings.
type AnswerReason string

// The reasons a node did not answer.
const (
	// ReasonNone is the reason of a node that answered.
	ReasonNone AnswerReason = ""
	// ReasonUnreachable is a node that could not be reached.
	ReasonUnreachable AnswerReason = "unreachable"
	// ReasonTimeout is a node that did not answer in time.
	ReasonTimeout AnswerReason = "timeout"
	// ReasonUnsupported is a node of a release that cannot be asked.
	ReasonUnsupported AnswerReason = "unsupported"
	// ReasonFailed is a node that was reached and could not answer.
	ReasonFailed AnswerReason = "failed"
)

// Valid reports whether r is one of the reasons, ReasonNone included.
func (r AnswerReason) Valid() bool {
	switch r {
	case ReasonNone, ReasonUnreachable, ReasonTimeout, ReasonUnsupported, ReasonFailed:
		return true
	}
	return false
}

// NodeAnswer is what came of asking one node for its settings.
type NodeAnswer struct {
	NodeID string
	// Answered reports whether Settings holds the node's answer.
	Answered bool
	// Reason says why not; ReasonNone when it answered.
	Reason   AnswerReason
	Settings NodeSettings
}

// The names of what SettingMismatches compares besides the settings. A
// setting of either name is left out of the comparison.
const (
	// SettingKMSKeyID is the key that wraps data keys (NodeSettings.KMSKeyID):
	// nodes that wrap with different keys are one misconfigured cluster.
	SettingKMSKeyID = "kms-key-id"
	// SettingVersion is the version and build number of the binary, joined
	// with "-": it differs during a rolling upgrade, and should not after.
	SettingVersion = "version"
)

// MismatchValue is what one node has of a setting nodes differ in.
type MismatchValue struct {
	NodeID string
	// Value is the redacted value; "set" or "not set" for a secret; empty,
	// or "not set" for a secret, on a node that does not have the setting.
	Value string
}

// Mismatch is a setting that should be the same on every node and is not.
type Mismatch struct {
	// Setting is the name of the setting, or SettingKMSKeyID or
	// SettingVersion.
	Setting string
	// Values has what each node that answered has, by node ID.
	Values []MismatchValue
}

// SettingMismatches returns the settings of scope ScopeCluster in which the
// nodes that answered differ, by name, each with the value of every such
// node by node ID. Nodes that did not answer are left out: nothing is known
// of them. Fewer than two answers differ in nothing.
//
// Values are compared as they are shown: redacted, and for the secret kinds
// only by whether they are set - two nodes with different passphrases cannot
// be told apart here, and must not be.
//
// A node that does not have a setting is compared as though it had it unset,
// and counts as different only when a node that has it holds a value
// somebody chose (Set, not from SourceDefault, and not shown as nothing). A node of an older
// release does not know a setting a newer one added: while all the others
// run on the default there is nothing to report but the upgrade, which
// SettingVersion reports already; once one of them is given a value, the
// older node really does behave differently.
//
// SettingKMSKeyID and SettingVersion are compared the same way, as plain
// values every node has.
//
// The result depends on the answers only, not on their order.
func SettingMismatches(answers []NodeAnswer) []Mismatch {
	var nodes []NodeAnswer
	for _, answer := range answers {
		if answer.Answered {
			nodes = append(nodes, answer)
		}
	}
	if len(nodes) < 2 {
		return nil
	}
	slices.SortStableFunc(nodes, func(a, b NodeAnswer) int { return strings.Compare(a.NodeID, b.NodeID) })

	// The settings of scope cluster of each node, by name. Of two with one
	// name the first in the order of Normalized counts.
	held := make([]map[string]Setting, len(nodes))
	var names []string
	for i, node := range nodes {
		held[i] = map[string]Setting{}
		for _, setting := range node.Settings.Normalized().Settings {
			if setting.Scope != ScopeCluster || setting.Name == SettingKMSKeyID || setting.Name == SettingVersion {
				continue
			}
			if _, twice := held[i][setting.Name]; !twice {
				held[i][setting.Name] = setting
				names = append(names, setting.Name)
			}
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)

	var out []Mismatch
	for _, name := range names {
		if values, differ := settingValues(name, nodes, held); differ {
			out = append(out, Mismatch{Setting: name, Values: values})
		}
	}
	for _, pseudo := range []struct {
		name string
		of   func(NodeSettings) string
	}{
		{SettingKMSKeyID, func(n NodeSettings) string { return n.KMSKeyID }},
		{SettingVersion, func(n NodeSettings) string { return n.Build.Version + "-" + n.Build.Build }},
	} {
		values := make([]MismatchValue, len(nodes))
		differ := false
		for i, node := range nodes {
			values[i] = MismatchValue{NodeID: node.NodeID, Value: bounded(pseudo.of(node.Settings))}
			differ = differ || values[i].Value != values[0].Value
		}
		if differ {
			out = append(out, Mismatch{Setting: pseudo.name, Values: values})
		}
	}
	slices.SortFunc(out, func(a, b Mismatch) int { return strings.Compare(a.Setting, b.Setting) })
	return out
}

// settingValues returns what each node has of the setting called name, and
// whether they differ by the rule of SettingMismatches. At least one node
// has it.
func settingValues(name string, nodes []NodeAnswer, held []map[string]Setting) ([]MismatchValue, bool) {
	// What a node without the setting is shown as goes by the kind the
	// others give it.
	absent := ""
	for i := range nodes {
		if setting, ok := held[i][name]; ok && !setting.Kind.shows() {
			absent = shownNotSet
		}
	}

	values := make([]MismatchValue, len(nodes))
	first := -1
	differ, missing, chosen := false, false, false
	for i, node := range nodes {
		setting, ok := held[i][name]
		values[i] = MismatchValue{NodeID: node.NodeID, Value: absent}
		if !ok {
			missing = true
			continue
		}
		values[i].Value = setting.shown()
		if first < 0 {
			first = i
		}
		differ = differ || values[i].Value != values[first].Value
		// A value somebody chose that shows as nothing - a URL that was
		// all query - is not told apart from no value.
		chosen = chosen || (setting.explicit() && values[i].Value != absent)
	}
	return values, differ || (missing && chosen)
}
