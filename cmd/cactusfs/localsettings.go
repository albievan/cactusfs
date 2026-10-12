package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/disk"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/ports"
)

// volumeProbeTimeout is how long the drives and the metadata directory are
// waited for when the node is asked for its settings: all of them against
// this one deadline, as the gauges of the drives are (telemetry.Config
// .StateTimeout). A directory that does not answer - a mount that hangs - is
// shown as it is known, without its room, and holds nobody longer than this.
const volumeProbeTimeout = 2 * time.Second

// volumeFacts is what the operating system says of a directory.
type volumeFacts struct {
	// reached reports whether the directory answered.
	reached    bool
	filesystem cluster.Filesystem
	total      uint64
	free       uint64
}

// volumeProbe asks the operating system about one directory, with one
// question out at a time: whoever asks while a question is out waits for that
// one, each no longer than it may. A directory that hangs costs one goroutine,
// however many ask, and is asked again only after it has answered.
type volumeProbe struct {
	ask func() volumeFacts

	mu sync.Mutex
	// out is the question that is out; nil when none is.
	out *volumeQuestion
}

// volumeQuestion is one asking of a directory, shared by everybody who asked
// while it was out.
type volumeQuestion struct {
	// answered is closed when the directory has answered; facts is written
	// before that and never after.
	answered chan struct{}
	facts    volumeFacts
}

// start puts the question unless one is out already, and returns the question
// to wait for. It does not wait.
func (p *volumeProbe) start() *volumeQuestion {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.out != nil {
		return p.out
	}
	q := &volumeQuestion{answered: make(chan struct{})}
	p.out = q
	ask := p.ask
	go func() {
		q.facts = ask()
		p.mu.Lock()
		p.out = nil
		p.mu.Unlock()
		close(q.answered)
	}()
	return q
}

// wait returns what the directory answered, and false when it has not by the
// time expired is done: nothing is known of it then, and that is no answer of
// the directory's. An answer that comes at the same moment counts.
func (q *volumeQuestion) wait(expired context.Context) (volumeFacts, bool) {
	select {
	case <-q.answered:
		return q.facts, true
	case <-expired.Done():
	}
	select {
	case <-q.answered:
		return q.facts, true
	default:
		return volumeFacts{}, false
	}
}

// bytesOf turns the room the operating system reports into whole bytes.
func bytesOf(room float64) uint64 {
	if math.IsNaN(room) || room <= 0 {
		return 0
	}
	if room >= math.MaxUint64 {
		return math.MaxUint64
	}
	return uint64(room)
}

// factsOf puts what was asked of a directory together. The file system is
// said where it can be told; where it cannot, the directory is still reached.
func factsOf(usage disk.Usage, usageErr error, filesystem cluster.Filesystem) volumeFacts {
	if usageErr != nil {
		return volumeFacts{filesystem: filesystem}
	}
	return volumeFacts{reached: true, filesystem: filesystem, total: bytesOf(usage.TotalBytes), free: bytesOf(usage.FreeBytes)}
}

// localDrive is one of this node's drives as its settings show it.
type localDrive struct {
	id, dir, label string
	probe          volumeProbe
}

// servedCertificates holds what keeps the node's certificates current, by
// use. The node sets them while it starts; node RPC reads them from the
// first call on.
type servedCertificates struct {
	node, s3 atomic.Pointer[nodetls.Reloader]
}

// localSettings is how this node was configured, as it says it when asked:
// by the management API of this node, and over node RPC by the node that
// answers the management API elsewhere. What cannot change while the node
// runs is worked out once, when it starts; the room on its volumes and the
// certificates it serves are read at each question.
//
// Redaction happens here, in the node that owns the values, and only here
// are the values known: every setting is made by cluster.NewSetting, which
// is never handed a secret (shownSettings).
type localSettings struct {
	// nodeID is known at once on a member of a cluster; a node on its own
	// learns its name from its metadata store.
	nodeID    atomic.Pointer[string]
	roles     []string
	labels    map[string]string
	build     cluster.Build
	startedAt time.Time
	kmsKeyID  string
	settings  []cluster.Setting
	drives    []*localDrive
	metaDir   string
	meta      volumeProbe
	served    servedCertificates
}

// newLocalSettings works out what the node says of itself from its
// configuration, the drives it opened - in the order of --data, which is the
// order of the labels - and the name of its KMS key.
func newLocalSettings(cfg serverConfig, drives []*disk.Drive, kmsKeyID string, startedAt time.Time) *localSettings {
	build := buildinfo.Get()
	local := &localSettings{
		labels:    map[string]string{},
		build:     cluster.Build{Version: build.Version, Build: build.Build, Commit: build.Commit},
		startedAt: startedAt,
		kmsKeyID:  kmsKeyID,
		settings:  shownSettings(cfg),
		metaDir:   cfg.MetaDir,
	}
	local.setNodeID(cfg.NodeID)
	for _, role := range cfg.Roles {
		local.roles = append(local.roles, string(role))
	}
	for name, value := range map[string]string{
		"region": cfg.Labels.Region, "zone": cfg.Labels.Zone, "rack": cfg.Labels.Rack, "host": cfg.Labels.Host,
	} {
		if value != "" {
			local.labels[name] = value
		}
	}
	for i, drive := range drives {
		shown := &localDrive{id: drive.DriveID(), dir: drive.Dir()}
		// The labels were counted against the directories when the
		// configuration was read; a node whose drive does not open does
		// not start, so no label is ever another drive's.
		if i < len(cfg.DriveLabels) {
			shown.label = cfg.DriveLabels[i]
		}
		shown.probe.ask = func() volumeFacts {
			usage, usageErr := drive.Usage()
			// Where the file system cannot be told it is left out.
			filesystem, _ := drive.Filesystem()
			return factsOf(usage, usageErr, filesystem)
		}
		local.drives = append(local.drives, shown)
	}
	local.meta.ask = func() volumeFacts {
		usage, usageErr := disk.UsageOf(cfg.MetaDir)
		filesystem, _ := disk.FilesystemOf(cfg.MetaDir)
		return factsOf(usage, usageErr, filesystem)
	}
	return local
}

// setNodeID names the node.
func (l *localSettings) setNodeID(id string) {
	l.nodeID.Store(&id)
}

// serve says which reloader keeps the certificate of a use current.
func (l *localSettings) serve(use string, reloader *nodetls.Reloader) {
	switch use {
	case cluster.CertificateNode:
		l.served.node.Store(reloader)
	case cluster.CertificateS3:
		l.served.s3.Store(reloader)
	}
}

// Local implements ports.LocalSettings.
func (l *localSettings) Local(ctx context.Context) (cluster.NodeSettings, error) {
	settings := cluster.NodeSettings{
		NodeID: *l.nodeID.Load(), Roles: l.roles, Labels: l.labels, Build: l.build, StartedAt: l.startedAt,
		KMSKeyID: l.kmsKeyID, Settings: l.settings,
		Drives:   make([]cluster.DriveSettings, len(l.drives)),
		Metadata: cluster.Volume{Directory: l.metaDir},
	}
	// Every volume is asked first and all are waited for against one
	// deadline, which is this caller's own: a question another caller put is
	// waited for as long as one put here. A volume that has not answered by
	// then is shown as not reached, without its room.
	answers := make([]*volumeQuestion, len(l.drives))
	for i, drive := range l.drives {
		answers[i] = drive.probe.start()
	}
	meta := l.meta.start()
	expired, cancel := context.WithTimeout(ctx, volumeProbeTimeout)
	defer cancel()
	for i, drive := range l.drives {
		facts, _ := answers[i].wait(expired)
		settings.Drives[i] = cluster.DriveSettings{
			ID: drive.id, Online: facts.reached, ErasureSet: cluster.ErasureSetUnknown,
			Volume: cluster.Volume{
				Directory: drive.dir, Label: drive.label, Filesystem: facts.filesystem,
				TotalBytes: facts.total, FreeBytes: facts.free,
			},
		}
	}
	facts, _ := meta.wait(expired)
	settings.Metadata.Filesystem, settings.Metadata.TotalBytes, settings.Metadata.FreeBytes = facts.filesystem, facts.total, facts.free
	if err := ctx.Err(); err != nil {
		return cluster.NodeSettings{}, fmt.Errorf("settings.Local: %w", err)
	}

	for _, served := range []struct {
		use      string
		reloader *nodetls.Reloader
	}{
		{cluster.CertificateNode, l.served.node.Load()},
		{cluster.CertificateS3, l.served.s3.Load()},
	} {
		if served.reloader == nil {
			continue
		}
		certificate, err := servedCertificate(served.use, served.reloader)
		if err != nil {
			return cluster.NodeSettings{}, fmt.Errorf("settings.Local: %w", err)
		}
		settings.Certificates = append(settings.Certificates, certificate)
	}
	return settings, nil
}

var _ ports.LocalSettings = (*localSettings)(nil).Local

// servedCertificate returns what may be said of the certificate a reloader
// has in use at this moment - the one a handshake that begins now is shown:
// who it names, who issued it and for how long. Nothing of its key.
func servedCertificate(use string, reloader *nodetls.Reloader) (cluster.Certificate, error) {
	// The reloader takes no notice of who asks.
	held, err := reloader.GetCertificate(nil)
	if err != nil {
		return cluster.Certificate{}, fmt.Errorf("the %s certificate: %w", use, err)
	}
	if held == nil {
		return cluster.Certificate{}, fmt.Errorf("the %s certificate: %w", use, errNoCertificate)
	}
	leaf := held.Leaf
	if leaf == nil {
		if len(held.Certificate) == 0 {
			return cluster.Certificate{}, fmt.Errorf("the %s certificate: %w", use, errNoCertificate)
		}
		if leaf, err = x509.ParseCertificate(held.Certificate[0]); err != nil {
			return cluster.Certificate{}, fmt.Errorf("the %s certificate: %w", use, err)
		}
	}
	shown := cluster.Certificate{
		Use: use, Subject: leaf.Subject.String(), Issuer: leaf.Issuer.String(),
		DNSNames: slices.Clone(leaf.DNSNames), NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
	}
	for _, address := range leaf.IPAddresses {
		shown.IPAddresses = append(shown.IPAddresses, address.String())
	}
	return shown, nil
}

// errNoCertificate is a reloader that holds no certificate, which none does.
var errNoCertificate = errors.New("none is in use")
