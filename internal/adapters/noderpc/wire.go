package noderpc

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// NodeInfo is what a node says about itself.
type NodeInfo struct {
	Node cluster.Node
	// Formed reports whether the node knows of a metadata group.
	Formed bool
	// DeploymentID is the deployment the node knows it belongs to, or "".
	DeploymentID string
	Drives       []cluster.DriveState
}

// maxReadLength bounds one ReadShard.
const maxReadLength = 2 << 20

// maxShardIndex is the highest shard index that can be named on the wire;
// erasure sets have at most 16 drives.
const maxShardIndex = 255

// maxChunk bounds one chunk of a WriteShard stream, well below the limit on
// a message.
const maxChunk = 1 << 20

// toWire turns an error into its form on the wire; nil stays nil.
func toWire(err error) *nodev1.Error {
	if err == nil {
		return nil
	}
	return &nodev1.Error{Codes: domain.Codes(err), Message: err.Error()}
}

// fromWire rebuilds the error another node reported. It reads as the node's
// message, prefixed with which node said it.
func fromWire(nodeID string, e *nodev1.Error) error {
	if e == nil {
		return nil
	}
	return domain.FromCodes(e.GetCodes(), fmt.Sprintf("node %s: %s", nodeID, e.GetMessage()))
}

func shardToWire(id object.ShardID) *nodev1.ShardId {
	// An index outside the range no drive accepts is sent as one: the
	// other node refuses it.
	index := uint32(maxShardIndex + 1)
	if id.Index >= 0 && id.Index <= maxShardIndex {
		index = uint32(id.Index)
	}
	return &nodev1.ShardId{DataId: id.DataID[:], Index: index}
}

func shardFromWire(id *nodev1.ShardId) (object.ShardID, error) {
	dataID, err := dataIDFromWire(id.GetDataId())
	if err != nil {
		return object.ShardID{}, err
	}
	if id.GetIndex() > maxShardIndex {
		return object.ShardID{}, fmt.Errorf("%w: shard index %d", domain.ErrInvalidArgument, id.GetIndex())
	}
	return object.ShardID{DataID: dataID, Index: int(id.GetIndex())}, nil
}

func dataIDFromWire(raw []byte) (object.ID, error) {
	var id object.ID
	if len(raw) != len(id) {
		return id, fmt.Errorf("%w: data ID of %d bytes", domain.ErrInvalidID, len(raw))
	}
	copy(id[:], raw)
	return id, nil
}

var errNoHeader = errors.New("the stream did not begin with a header")

// Bounds on what a node says of itself in Settings. Nothing that crosses a
// wire is without one: a node sends no more than this, and takes no more
// from another, whatever that one sends. No deployment comes near them; what
// exceeds them is dropped, and the drop is logged.
const (
	// MaxSettings is the number of settings of one node.
	MaxSettings = 512
	// MaxSettingsDrives is the number of drives of one node.
	MaxSettingsDrives = 256
	// MaxSettingsCertificates is the number of certificates of one node.
	MaxSettingsCertificates = 16
	// MaxSettingsLabels is the number of placement labels, and of roles.
	MaxSettingsLabels = 16
	// MaxCertificateNames is the number of DNS names, and of addresses, of
	// one certificate.
	MaxCertificateNames = 64
	// MaxSettingsNameBytes bounds a name: of a setting, its variable, flag
	// and group, of a node, a drive, a label, a role, a build, a key, the
	// type of a file system, a name in a certificate. The value of a
	// setting, a directory, a device and the subject of a certificate are
	// bounded by cluster.MaxSettingValueBytes.
	MaxSettingsNameBytes = 256
)

// clip returns s as valid UTF-8 of at most limit bytes: the wire format
// refuses anything else, and a directory need not be text. What is cut ends
// with cluster.TruncationMarker.
func clip(s string, limit int) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	if len(s) <= limit {
		return s
	}
	cut := limit - len(cluster.TruncationMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + cluster.TruncationMarker
}

func clipName(s string) string { return clip(s, MaxSettingsNameBytes) }

func clipLong(s string) string { return clip(s, cluster.MaxSettingValueBytes) }

// first returns at most limit elements of list, each through clean, and how
// many it left out.
func first[T any](list []T, limit int, clean func(T) T) (kept []T, dropped int) {
	if len(list) > limit {
		list, dropped = list[:limit], len(list)-limit
	}
	if len(list) == 0 {
		return nil, dropped
	}
	kept = make([]T, len(list))
	for i, element := range list {
		kept[i] = clean(element)
	}
	return kept, dropped
}

func boundVolume(v cluster.Volume) cluster.Volume {
	v.Directory, v.Label = clipLong(v.Directory), clipName(v.Label)
	v.Filesystem = cluster.Filesystem{
		Type: clipName(v.Filesystem.Type), MountPoint: clipLong(v.Filesystem.MountPoint),
		// What is mounted can name a user and a password. The adapter that
		// read the table of mounts has taken them out; it is done again
		// here, on both sides of the wire.
		Device: clipLong(cluster.RedactDevice(v.Filesystem.Device)),
	}
	return v
}

// boundSettings returns n within the bounds of this file, and how many
// settings, drives, certificates, labels, roles and names it dropped to get
// there. It shares nothing with n. It does not redact - Normalized does -
// and it cannot undo a redaction: it never touches the value of a setting,
// and a kind it cuts was not a kind that is shown, nor is what is left of
// it.
func boundSettings(n cluster.NodeSettings) (out cluster.NodeSettings, dropped int) {
	count := func(d int) { dropped += d }
	var d int
	out = cluster.NodeSettings{
		NodeID: clipName(n.NodeID), StartedAt: n.StartedAt, KMSKeyID: clipName(n.KMSKeyID), Metadata: boundVolume(n.Metadata),
		Build: cluster.Build{Version: clipName(n.Build.Version), Build: clipName(n.Build.Build), Commit: clipName(n.Build.Commit)},
	}
	out.Roles, d = first(n.Roles, MaxSettingsLabels, clipName)
	count(d)

	// Of too many labels, the first by name are kept: which is the same
	// whoever cuts.
	labels := make([]string, 0, len(n.Labels))
	for label := range n.Labels {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	if len(labels) > MaxSettingsLabels {
		count(len(labels) - MaxSettingsLabels)
		labels = labels[:MaxSettingsLabels]
	}
	if n.Labels != nil {
		out.Labels = make(map[string]string, len(labels))
	}
	for _, label := range labels {
		out.Labels[clipName(label)] = clipName(n.Labels[label])
	}

	out.Settings, d = first(n.Settings, MaxSettings, func(s cluster.Setting) cluster.Setting {
		s.Name, s.Env, s.Flag, s.Group = clipName(s.Name), clipName(s.Env), clipName(s.Flag), clipName(s.Group)
		s.Kind, s.Scope = cluster.SettingKind(clipName(string(s.Kind))), cluster.SettingScope(clipName(string(s.Scope)))
		s.Source = cluster.SettingSource(clipName(string(s.Source)))
		return s
	})
	count(d)
	out.Drives, d = first(n.Drives, MaxSettingsDrives, func(drive cluster.DriveSettings) cluster.DriveSettings {
		drive.ID, drive.Volume = clipName(drive.ID), boundVolume(drive.Volume)
		return drive
	})
	count(d)
	out.Certificates, d = first(n.Certificates, MaxSettingsCertificates, func(c cluster.Certificate) cluster.Certificate {
		c.Use, c.Subject, c.Issuer = clipName(c.Use), clipLong(c.Subject), clipLong(c.Issuer)
		var names, addresses int
		c.DNSNames, names = first(c.DNSNames, MaxCertificateNames, clipName)
		c.IPAddresses, addresses = first(c.IPAddresses, MaxCertificateNames, clipName)
		count(names + addresses)
		return c
	})
	count(d)
	return out, dropped
}

// unixOf returns a time as seconds since the Unix epoch, and no time as 0.
func unixOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOf(unix int64) time.Time {
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
}

func volumeToWire(v cluster.Volume) *nodev1.Volume {
	if v == (cluster.Volume{}) {
		return nil
	}
	out := &nodev1.Volume{Directory: v.Directory, Label: v.Label, TotalBytes: v.TotalBytes, FreeBytes: v.FreeBytes}
	if fs := v.Filesystem; fs != (cluster.Filesystem{}) {
		out.Filesystem = &nodev1.Filesystem{Type: fs.Type, Device: fs.Device, MountPoint: fs.MountPoint}
	}
	return out
}

func volumeFromWire(v *nodev1.Volume) cluster.Volume {
	fs := v.GetFilesystem()
	return cluster.Volume{
		Directory: v.GetDirectory(), Label: v.GetLabel(), TotalBytes: v.GetTotalBytes(), FreeBytes: v.GetFreeBytes(),
		Filesystem: cluster.Filesystem{Type: fs.GetType(), Device: fs.GetDevice(), MountPoint: fs.GetMountPoint()},
	}
}

// settingsToWire puts what a node says of itself on the wire. What it is
// given has been normalized and bounded (Server.Settings): nothing here
// decides what may be sent.
func settingsToWire(n cluster.NodeSettings) *nodev1.SettingsResponse {
	resp := &nodev1.SettingsResponse{
		NodeId: n.NodeID, StartedAtUnix: unixOf(n.StartedAt), KmsKeyId: n.KMSKeyID,
		Build: &nodev1.Build{Version: n.Build.Version, Build: n.Build.Build, Commit: n.Build.Commit},
		Roles: n.Roles, Labels: n.Labels, Metadata: volumeToWire(n.Metadata),
	}
	for _, s := range n.Settings {
		resp.Settings = append(resp.Settings, &nodev1.Setting{
			Name: s.Name, Env: s.Env, Flag: s.Flag, Group: s.Group,
			Kind: string(s.Kind), Scope: string(s.Scope), Source: string(s.Source), Set: s.Set, Value: s.Value,
		})
	}
	for _, drive := range n.Drives {
		volume := volumeToWire(drive.Volume)
		resp.Drives = append(resp.Drives, &nodev1.DriveSettings{Id: drive.ID, Volume: volume, Online: drive.Online})
	}
	for _, c := range n.Certificates {
		resp.Certificates = append(resp.Certificates, &nodev1.Certificate{
			Use: c.Use, Subject: c.Subject, Issuer: c.Issuer, DnsNames: c.DNSNames, IpAddresses: c.IPAddresses,
			NotBeforeUnix: unixOf(c.NotBefore), NotAfterUnix: unixOf(c.NotAfter),
		})
	}
	return resp
}

// settingsFromWire reads what another node sent, as it sent it. The caller
// bounds and normalizes it (Client.NodeSettings): nothing here is trusted.
// Which erasure set a drive is in is not on the wire.
func settingsFromWire(resp *nodev1.SettingsResponse) cluster.NodeSettings {
	build := resp.GetBuild()
	n := cluster.NodeSettings{
		NodeID: resp.GetNodeId(), StartedAt: timeOf(resp.GetStartedAtUnix()), KMSKeyID: resp.GetKmsKeyId(),
		Build: cluster.Build{Version: build.GetVersion(), Build: build.GetBuild(), Commit: build.GetCommit()},
		Roles: resp.GetRoles(), Labels: resp.GetLabels(), Metadata: volumeFromWire(resp.GetMetadata()),
	}
	for _, s := range resp.GetSettings() {
		n.Settings = append(n.Settings, cluster.Setting{
			Name: s.GetName(), Env: s.GetEnv(), Flag: s.GetFlag(), Group: s.GetGroup(),
			Kind: cluster.SettingKind(s.GetKind()), Scope: cluster.SettingScope(s.GetScope()), Source: cluster.SettingSource(s.GetSource()),
			Set: s.GetSet(), Value: s.GetValue(),
		})
	}
	for _, drive := range resp.GetDrives() {
		n.Drives = append(n.Drives, cluster.DriveSettings{
			ID: drive.GetId(), Volume: volumeFromWire(drive.GetVolume()), Online: drive.GetOnline(), ErasureSet: cluster.ErasureSetUnknown,
		})
	}
	for _, c := range resp.GetCertificates() {
		n.Certificates = append(n.Certificates, cluster.Certificate{
			Use: c.GetUse(), Subject: c.GetSubject(), Issuer: c.GetIssuer(), DNSNames: c.GetDnsNames(), IPAddresses: c.GetIpAddresses(),
			NotBefore: timeOf(c.GetNotBeforeUnix()), NotAfter: timeOf(c.GetNotAfterUnix()),
		})
	}
	return n
}
