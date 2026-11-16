package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Replication between sites, at the source (ADR-0026): the Replicator is the
// Deliverer of the consumers of kind r. For each row of the outbox it reads
// the version the row names, through the ObjectService and as the
// replication service, and sends it - or the later change of its tags,
// retention or legal hold the row tells of - to the other deployment the
// target names, through a ports.SiteClient. What became of the version is
// the status it reports with the acknowledgement: COMPLETED only once the
// other deployment took it or said it had it, FAILED only for what can never
// be sent - a version of a customer's key, or what the other deployment
// refuses for itself. Every other failure holds the target.

// SiteSource gives the ports.SiteClient of a target of kind s3: the other
// deployment it names. It is made by the composition root, which opens the
// target's secret through the KMS for each request (ADR-0026). An error is a
// failure of the class it carries (delivery.ClassOf): one that carries none
// is delivery.ClassInternal, and is tried again.
type SiteSource interface {
	Site(ctx context.Context, target delivery.Target) (ports.SiteClient, error)
}

// SiteSourceFunc is a function that is a SiteSource.
type SiteSourceFunc func(ctx context.Context, target delivery.Target) (ports.SiteClient, error)

// Site implements SiteSource.
func (f SiteSourceFunc) Site(ctx context.Context, target delivery.Target) (ports.SiteClient, error) {
	return f(ctx, target)
}

// ReplicationReader is what the replicator reads a version through; the
// ObjectService is one. Each call names the version by its bucket, its key
// and its reference, reads it again, and is authorized: the replicator reads
// as the replication service (iam.ServiceReplication).
type ReplicationReader interface {
	// ReplicationVersion returns a version named by its ID, a delete
	// marker too.
	ReplicationVersion(ctx context.Context, principal iam.Principal, bucketName, key string, ref object.VersionRef) (object.Version, error)
	// OpenReplicationSpan opens a span of the bytes of a version.
	OpenReplicationSpan(ctx context.Context, principal iam.Principal, bucketName, key string, ref object.VersionRef, span object.Range) (io.ReadCloser, error)
	// ReplicationParts returns up to limit parts of a version of parts,
	// from the one that holds the byte at offset.
	ReplicationParts(ctx context.Context, principal iam.Principal, bucketName, key string, ref object.VersionRef, offset int64, limit int) ([]object.Part, error)
}

// Bounds of the replicator.
const (
	// ReplicationRateWindow is how long the bytes the replicator sends are
	// averaged over against its rate: in any window of that length it sends
	// at most the rate times the window, and one read more for each
	// version sent at once.
	ReplicationRateWindow = 10 * time.Second
	// ReplicationFloorRate is the least pace, in bytes a second, an attempt
	// at a version is given time for (ReplicationAttemptTimeout), unless
	// replication-rate-mib shared among every version that may be sent at
	// once allows less: 64 KiB/s, a link far slower than any a site is
	// joined by.
	ReplicationFloorRate = 64 << 10
	// ReplicationLeastAttempt is the least time an attempt at a version is
	// given, however small: a stalled transfer is ended by the egress
	// client, whose connections may make no progress for longer than
	// delivery-timeout, not by this.
	ReplicationLeastAttempt = 24 * time.Hour
	// siteCheckEvery is how long the answer of the bucket probe of the
	// other deployment is taken as it was: the probe is asked again after
	// it, and after any failure to send there.
	siteCheckEvery = time.Minute
	// replicationPartsPage is how many parts are read from the metadata
	// at once.
	replicationPartsPage = 100
	// replicationReadSize bounds one read of a version's bytes, and so what
	// the rate holds back at once.
	replicationReadSize = 64 << 10
	// replicationAbortTimeout bounds the abort of an upload that will not
	// be completed.
	replicationAbortTimeout = 30 * time.Second
	// maxRememberedUploads bounds the uploads in progress the replicator
	// remembers to go on with: one a version that is being sent, and those
	// held at a failure.
	maxRememberedUploads = 1024
	// maxRememberedParts bounds the parts of those uploads it remembers, all
	// together: the oldest uploads are forgotten - and aborted, as a best
	// effort - beyond it.
	maxRememberedParts = 100_000
)

// ReplicationAttemptTimeout returns the deadline of one attempt at a row of
// replication. A row without bytes - a marker, a later change - has base. A
// version has base and ReplicationLeastAttempt, or the time its size takes
// at ReplicationFloorRate when that is longer - or at rateBytes shared
// among every version that may be sent at once (workers to each of
// delivery.MaxTargets targets), when that is slower still. The bound is
// generous on purpose: a version sent in one PUT cannot go on from where it
// was cut off, and one that never fits its deadline would hold its target
// for good; what ends a transfer that has stopped moving is the egress
// client's no-progress timeout. A version of parts that is cut off goes on
// from the parts that arrived (Replicator).
func ReplicationAttemptTimeout(base time.Duration, rateBytes int64, workers int) func(delivery.Item) time.Duration {
	pace := int64(ReplicationFloorRate)
	if rateBytes > 0 {
		pace = max(min(pace, rateBytes/(int64(max(workers, 1))*delivery.MaxTargets)), 1)
	}
	return func(item delivery.Item) time.Duration {
		if item.Size <= 0 {
			return base
		}
		const most = int64(1<<63-1) / int64(time.Second)
		seconds := max(item.Size/pace+1, int64(ReplicationLeastAttempt/time.Second))
		if seconds > most-int64(base/time.Second) {
			return time.Duration(1<<63 - 1)
		}
		return base + time.Duration(seconds)*time.Second
	}
}

// ReplicatorConfig lists what a Replicator depends on.
type ReplicatorConfig struct {
	// Objects is what versions are read through.
	Objects ReplicationReader
	// Buckets is where the replication configuration of a bucket is read:
	// it names the bucket of the other deployment.
	Buckets ports.BucketStore
	// Sites gives the client of each target.
	Sites SiteSource
	// Source is the ID of this deployment (object.ValidateReplicaSource):
	// what the other deployment's bucket must be a replica of, and the
	// source every copy names.
	Source string
	// RateBytes bounds the bytes sent to every other deployment together,
	// per second, averaged over ReplicationRateWindow; 0 is no bound.
	RateBytes int64
	Clock     ports.Clock
	// Logger, when not nil, is told at Debug of the items that are passed
	// without being sent, by target and reason; never by key. The
	// DeliveryWorker logs the target and the class of every failure
	// already.
	Logger *slog.Logger
}

// Replicator is the Deliverer of the consumers of kind r (ADR-0026). For an
// item of the outbox it
//
//   - reads the replication configuration of the item's bucket: an item is
//     sent only while it names the target (and the target admits the
//     bucket), to the bucket of the other deployment it names; an item of a
//     bucket whose configuration was removed, or points elsewhere now, is
//     passed with delivery.ClassRejected and no status;
//   - reads the version through the ObjectService as the replication
//     service: one that is gone is passed (delivery.ClassRejected, no
//     status), and one that arrived as a replica is never sent on;
//   - gives a version of a customer's key FAILED (delivery.ClassSSEC)
//     without reading a byte of it;
//   - sends nothing to a bucket until its probe says it keeps versions and
//     is a replica of this deployment (delivery.ClassVersioning,
//     delivery.ClassNotReplica otherwise);
//   - asks whether the other deployment has the version already: with the
//     entity tag sent it is COMPLETED and nothing is sent; with another it
//     is FAILED (delivery.ClassRejected) - the other deployment holds other
//     bytes under the version's ID and will take no others;
//   - sends a version in one PUT, a version of parts as an upload of the
//     same parts, and a delete marker as a marker - only when the deciding
//     rule copies markers and the lifecycle service did not make it;
//   - carries a later change of a version (PLAN 8.1.5) - its tags, its
//     retention, its legal hold, as the version has them now - to the
//     version there; a change the other deployment refuses for itself, a
//     refusal by Object Lock among them, is FAILED, and one it takes
//     reports no status: a change never makes a version COMPLETED;
//   - reports COMPLETED once the other deployment took a version, FAILED
//     when it refused it for itself (delivery.ClassRejected, a 400 or 413),
//     and nothing with any other failure, which holds the target.
//
// A version of parts goes on from where an attempt was cut off: the
// upload begun for it is remembered, within this process, by target,
// bucket there and version; the next attempt asks the other deployment for
// the parts that arrived and sends the others. Other uploads this deployment
// left of the key are aborted first. An upload is forgotten when it is
// completed, when it is given up (a refusal, then aborted), and the oldest
// once maxRememberedUploads are remembered; a version that never finishes
// holds its target.
//
// The bytes it sends, to every target together, are held to RateBytes.
type Replicator struct {
	objects ReplicationReader
	buckets ports.BucketStore
	sites   SiteSource
	source  string
	clock   ports.Clock
	logger  *slog.Logger
	rate    *byteRate

	mu sync.Mutex
	// conns are the targets as last sent to, by name, each with the
	// generation of its connection; next is the next generation.
	conns map[string]siteConn
	next  uint64
	// checked is what the bucket of the other deployment was last found to
	// be, and when, by connection and bucket.
	checked map[siteBucket]probed
	// uploads are the uploads in progress at the other deployment, by the
	// version they are of; order is the order they were begun in, and parts
	// how many parts they remember all together.
	uploads map[siteVersion]*siteUpload
	order   []siteVersion
	parts   int
	// maxUploads and maxParts are maxRememberedUploads and
	// maxRememberedParts, but in a test.
	maxUploads, maxParts int
	// sending are the objects at the other deployments that a version or a
	// change is being sent to now, by ObjectOf's name. The delivery worker's
	// waves never hand over two items of one object at once; this keeps a
	// resync (resync.go) and the outbox apart too.
	sending map[string]*sendingObject
	// preempted counts, by object there and version, the resync sends the
	// outbox ended; at ResyncPreemptions the outbox waits for the next one
	// instead. A version's count goes once its send ends by itself; all of
	// them go beyond maxRememberedUploads versions.
	preempted map[string]int
}

// sendingObject is a send to an object there in progress: done is closed
// when it ends; cancel, for a resync's send, ends it - the outbox does not
// wait for a resync, but for a version it has ended the send of
// ResyncPreemptions times. version names the version a resync sends.
type sendingObject struct {
	done    chan struct{}
	cancel  context.CancelCauseFunc
	version string
}

// errPreempted is why a resync's send was ended: the outbox has the object
// to send.
var errPreempted = errors.New("replicator: the outbox sends the object")

// siteConn is a target as it was last sent to, and the generation of its
// connection: a new one whenever the target was made again under its name,
// or its connection changed (delivery.Target.SameConnection).
type siteConn struct {
	target     delivery.Target
	generation uint64
}

// siteBucket names a bucket of the other deployment a target is: by the
// target's name and the generation of its connection, so that a target
// made again, or pointed elsewhere, is probed again and nothing is resumed
// at the endpoint it had.
type siteBucket struct {
	target     string
	generation uint64
	bucket     string
}

// probed is the answer of a bucket probe, and when it was given.
type probed struct {
	at     time.Time
	answer ports.SiteBucket
}

// siteVersion names a version at the other deployment.
type siteVersion struct {
	siteBucket
	key     string
	version string
}

// siteUpload is an upload in progress at the other deployment: its ID, the
// entity tag each part sent was given there, and the client it was begun
// through, which aborts it should it be forgotten.
type siteUpload struct {
	id          string
	sent        map[int]string
	site        ports.SiteClient
	bucket, key string
	// pinned counts the attempts that are sending through the upload now;
	// a pinned upload is never forgotten for the bounds, nor aborted.
	pinned int
}

// NewReplicator returns a Replicator.
func NewReplicator(cfg ReplicatorConfig) *Replicator {
	r := &Replicator{
		objects: cfg.Objects, buckets: cfg.Buckets, sites: cfg.Sites, source: cfg.Source, clock: cfg.Clock, logger: cfg.Logger,
		conns: map[string]siteConn{}, checked: map[siteBucket]probed{}, uploads: map[siteVersion]*siteUpload{},
		maxUploads: maxRememberedUploads, maxParts: maxRememberedParts, sending: map[string]*sendingObject{}, preempted: map[string]int{},
	}
	if r.logger == nil {
		r.logger = slog.New(slog.DiscardHandler)
	}
	if cfg.RateBytes > 0 {
		r.rate = &byteRate{perSecond: float64(cfg.RateBytes), clock: cfg.Clock}
	}
	return r
}

// replicationPrincipal is who the replicator reads as.
func replicationPrincipal() iam.Principal {
	return iam.Principal{Service: iam.ServiceReplication}
}

// Errors of the replicator; each is the cause of a delivery.Error.
var (
	// errNotSent is the cause of an item that is passed without being
	// sent: its words say why, and never name the key.
	errNotSent = errors.New("replicator: the item is not sent")
	// errNoReplicator is the cause of the failure of a Replicator that
	// was given no reader, store, sites or source.
	errNoReplicator = errors.New("replicator: nothing to read or send through on this node")
	// errSiteBucket is the cause of a bucket of the other deployment that
	// does not take the copies of this one.
	errSiteBucket = errors.New("replicator: the bucket of the other deployment does not take copies of this one")
	// errOtherBytes is the cause of a version the other deployment holds
	// with another entity tag.
	errOtherBytes = errors.New("replicator: the other deployment holds other bytes under the version's ID")
	// errParts is the cause of the parts of a version that are not as its
	// record says.
	errParts = errors.New("replicator: the parts of the version are not as its record says")
)

// isChange reports whether an event is a later change of a version.
func isChange(event delivery.Event) bool {
	return event == delivery.EventTags || event == delivery.EventRetention || event == delivery.EventLegalHold
}

// Deliver implements Deliverer: it sends the version an item names, or the
// later change of it, to the other deployment the target is, and returns the
// status of replication it gives.
func (r *Replicator) Deliver(ctx context.Context, target delivery.Target, item delivery.Item) (object.ReplicationStatus, error) {
	status, _, err := r.deliver(ctx, target, item, false)
	return status, err
}

// Resend implements ResyncSender: it sends a version a resync walks (PLAN
// 8.1.6) as the outbox would send the row of its commit - the same reads,
// classes, probe, locks, uploads that go on, rate and statuses - and asks
// first whether the other deployment has it, a delete marker too. It reports
// whether anything was sent: false when the version was there already, and
// for a version that is passed or FAILED without a byte sent.
//
// Unlike a row it never waits and never passes what it cannot send now: an
// object the outbox is sending is ErrResyncObjectBusy at once, and the
// outbox ends a resync's send to an object it comes to send (also
// ErrResyncObjectBusy); a configuration that no longer sends the bucket to
// the target, or a target that no longer admits it, is
// ErrResyncConfiguration. A version that is FAILED is neither sent nor
// given another status: a resync never lifts FAILED.
func (r *Replicator) Resend(ctx context.Context, target delivery.Target, v object.Version) (object.ReplicationStatus, bool, error) {
	event := delivery.EventPut
	switch {
	case v.DeleteMarker:
		event = delivery.EventMarker
	case v.Multipart():
		event = delivery.EventComplete
	}
	item := delivery.Item{
		Event: event, Bucket: v.Bucket, Key: v.Key, Version: v.Ref(), Size: v.Size, ETag: v.ETag, At: v.ModifiedAt,
		To: []delivery.Consumer{{Kind: delivery.ConsumerReplication, Target: target.Name}},
	}
	return r.deliver(ctx, target, item, true)
}

// deliver is Deliver and Resend: resync says the item is a version a resync
// walks, not a row of the outbox. sent reports whether anything was sent.
func (r *Replicator) deliver(ctx context.Context, target delivery.Target, item delivery.Item, resync bool) (object.ReplicationStatus, bool, error) {
	if r.objects == nil || r.buckets == nil || r.sites == nil || object.ValidateReplicaSource(r.source) != nil {
		return "", false, delivery.Failed(delivery.ClassInternal, errNoReplicator)
	}
	if resync && !target.Admits(item.Bucket) {
		return "", false, fmt.Errorf("replicator.Resend: %w: the target no longer admits the bucket", ErrResyncConfiguration)
	}
	if class, err := heldOrPassed(target, item); err != nil {
		return "", false, r.notSent(ctx, target, class, err)
	}
	config, err := r.configuration(ctx, item.Bucket)
	switch {
	case gone(err):
		return "", false, r.notSent(ctx, target, delivery.ClassRejected, fmt.Errorf("%w: the bucket is gone", errNotSent))
	case err != nil:
		return "", false, replicaFailure(err)
	case config.Target != target.Name && resync:
		// A row of the outbox is passed; a resync waits rather than move
		// its cursor over what it did not send.
		return "", false, fmt.Errorf("replicator.Resend: %w", ErrResyncConfiguration)
	case config.Target != target.Name:
		return "", false, r.notSent(ctx, target, delivery.ClassRejected,
			fmt.Errorf("%w: the bucket's replication configuration no longer names the target", errNotSent))
	}
	// One send to an object there at a time, whoever sends: a row of the
	// outbox and a version a resync walks may name the same object. The
	// outbox waits for another row, and ends a resync's send - but for a
	// version whose send it ended ResyncPreemptions times, which it waits
	// for; a resync waits for nobody.
	name := objectThere(target, config, item.Key)
	var release func()
	if resync {
		ctx, release, err = r.tryExclusive(ctx, name, item.Version.String())
	} else {
		release, err = r.exclusive(ctx, name)
	}
	if err != nil {
		if errors.Is(err, ErrResyncObjectBusy) {
			return "", false, err
		}
		return "", false, replicaFailure(err)
	}
	defer release()
	status, sent, err := r.sendRead(ctx, target, item, config, resync)
	if resync {
		if errors.Is(context.Cause(ctx), errPreempted) {
			// The outbox has the object now; what this send did is found
			// there when the walk comes back to it.
			return "", false, fmt.Errorf("replicator.Resend: %w", ErrResyncPreempted)
		}
		r.mu.Lock()
		delete(r.preempted, preemptionOf(name, item.Version.String()))
		r.mu.Unlock()
	}
	return status, sent, err
}

// sendRead reads the version an item names and sends it.
func (r *Replicator) sendRead(
	ctx context.Context, target delivery.Target, item delivery.Item, config bucket.ReplicationConfig, resync bool,
) (object.ReplicationStatus, bool, error) {
	v, err := r.objects.ReplicationVersion(ctx, replicationPrincipal(), item.Bucket, item.Key, item.Version)
	switch {
	case gone(err):
		return "", false, r.notSent(ctx, target, delivery.ClassRejected, fmt.Errorf("%w: the version is gone", errNotSent))
	case err != nil:
		return "", false, replicaFailure(err)
	case resync && v.Replication == object.ReplicationFailed:
		// FAILED is final: a resync neither sends the version nor gives it
		// another status, whatever the other deployment holds.
		return "", false, nil
	}
	status, sent, err := r.send(ctx, target, item, config, v, resync)
	if err != nil {
		r.forget(target, config.Bucket)
		if !delivery.ClassOf(err).Passes() {
			return "", false, err
		}
	}
	return status, sent, err
}

// objectThere names the object at the other deployment an item is about:
// the target, the bucket there the configuration names, and the key.
func objectThere(target delivery.Target, config bucket.ReplicationConfig, key string) string {
	return target.Name + "\x00" + config.Bucket + "\x00" + key
}

// preemptionOf names a version at an object there, as preempted counts it.
func preemptionOf(name, version string) string {
	return name + "\x00" + version
}

// exclusive is the outbox's: it waits until no other send to the object
// named is in progress, or ctx ends, and marks one as in progress; the
// function it returns ends it. A resync's send to the object is ended at
// once rather than waited for to its end - but for a version whose send it
// ended ResyncPreemptions times already: that one it waits for, so that a
// key written to again and again does not keep a large old version from
// ever crossing. A wait that outlasts ctx is a failure that holds, never
// FAILED.
func (r *Replicator) exclusive(ctx context.Context, name string) (func(), error) {
	for {
		r.mu.Lock()
		busy, ok := r.sending[name]
		if !ok {
			release := r.holdLocked(name, nil, "")
			r.mu.Unlock()
			return release, nil
		}
		cancel := busy.cancel
		if cancel != nil {
			key := preemptionOf(name, busy.version)
			if r.preempted[key] < ResyncPreemptions {
				if len(r.preempted) >= maxRememberedUploads {
					clear(r.preempted)
				}
				r.preempted[key]++
				// Ended once: the next look waits for it to end.
				busy.cancel = nil
			} else {
				cancel = nil
			}
		}
		r.mu.Unlock()
		if cancel != nil {
			cancel(errPreempted)
		}
		select {
		case <-busy.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("replicator: waiting for another send to the object: %w", ctx.Err())
		}
	}
}

// tryExclusive is a resync's: it marks a send of the version to the object
// named as in progress, under a context the outbox may end (errPreempted),
// and returns ErrResyncObjectBusy at once when another send to it is in
// progress.
func (r *Replicator) tryExclusive(ctx context.Context, name, version string) (context.Context, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sending[name]; ok {
		return nil, nil, fmt.Errorf("replicator.Resend: %w", ErrResyncObjectBusy)
	}
	sendCtx, cancel := context.WithCancelCause(ctx)
	release := r.holdLocked(name, cancel, version)
	return sendCtx, func() {
		release()
		cancel(nil)
	}, nil
}

// holdLocked marks a send to the object named as in progress and returns
// the function that ends it; r.mu is held.
func (r *Replicator) holdLocked(name string, cancel context.CancelCauseFunc, version string) func() {
	held := &sendingObject{done: make(chan struct{}), cancel: cancel, version: version}
	r.sending[name] = held
	return func() {
		r.mu.Lock()
		if r.sending[name] == held {
			delete(r.sending, name)
		}
		r.mu.Unlock()
		close(held.done)
	}
}

// ObjectOf implements ObjectNamer: the object an item is about at the other
// deployment - the target, the bucket there the replication configuration
// names, and the key - so that the DeliveryWorker never hands over two items
// of one object there at once, whichever buckets here they came from. An
// item whose bucket's configuration cannot be read, or does not name the
// target, is named by nothing (false), and goes alone.
func (r *Replicator) ObjectOf(ctx context.Context, target delivery.Target, item delivery.Item) (string, bool) {
	if r.buckets == nil {
		return "", false
	}
	config, err := r.configuration(ctx, item.Bucket)
	if err != nil || config.Target != target.Name {
		return "", false
	}
	return objectThere(target, config, item.Key), true
}

// heldOrPassed returns the failure of an item that is not to be sent, and
// its class: an item that is not the target's as replication, or of a target
// that is not another deployment, is held (delivery.ClassInternal), as the
// DeliveryWorker holds an item nothing delivers. The removal of a version
// for good, which is never replicated, and a bucket the target no longer
// admits, are passed. No failure is nil.
func heldOrPassed(target delivery.Target, item delivery.Item) (delivery.Class, error) {
	kind, ok := item.For(target.Name)
	switch {
	case !ok || kind != delivery.ConsumerReplication:
		return delivery.ClassInternal, fmt.Errorf("%w: the item is not for the target as replication", errNotSent)
	case target.Kind != delivery.KindS3:
		return delivery.ClassInternal, fmt.Errorf("%w: the target is not another deployment", errNotSent)
	case item.Event != delivery.EventPut && item.Event != delivery.EventComplete && item.Event != delivery.EventMarker && !isChange(item.Event):
		return delivery.ClassRejected, fmt.Errorf("%w: a kind of change that is never replicated", errNotSent)
	case !target.Admits(item.Bucket):
		return delivery.ClassRejected, fmt.Errorf("%w: the target no longer admits the bucket", errNotSent)
	}
	return delivery.ClassNone, nil
}

// notSent returns the failure of an item that is not sent, of its class, and
// logs why at Debug: by target and reason, never by key.
func (r *Replicator) notSent(ctx context.Context, target delivery.Target, class delivery.Class, cause error) error {
	r.logger.DebugContext(ctx, "a version is not replicated",
		slog.String("target", target.Name), slog.String("class", string(class)), slog.String("reason", cause.Error()))
	return delivery.Failed(class, fmt.Errorf("replicator.Deliver: %w", cause))
}

// gone reports whether a read found its bucket, object or version not there.
func gone(err error) bool {
	return errors.Is(err, domain.ErrBucketNotFound) || errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrVersionNotFound)
}

// replicaFailure returns a failure of this side as a delivery.Error: of the
// class it carries, delivery.ClassTimeout when that is the end of the
// attempt's deadline, delivery.ClassInternal otherwise. It is tried again.
func replicaFailure(err error) error {
	class := delivery.ClassOf(err)
	if class == delivery.ClassInternal && errors.Is(err, context.DeadlineExceeded) {
		class = delivery.ClassTimeout
	}
	return delivery.Failed(class, fmt.Errorf("replicator.Deliver: %w", err))
}

// configuration reads the replication configuration of a bucket. One that
// cannot be read is a failure of this side; none is the empty one.
func (r *Replicator) configuration(ctx context.Context, bucketName string) (bucket.ReplicationConfig, error) {
	raw, err := r.buckets.GetBucketConfig(ctx, bucketName, bucket.ConfigReplication)
	if err != nil {
		return bucket.ReplicationConfig{}, err
	}
	config, err := bucket.ParseReplicationConfig(raw)
	if err != nil {
		return bucket.ReplicationConfig{}, fmt.Errorf("%w: the stored replication configuration of a bucket: %s", domain.ErrIntegrity, err.Error())
	}
	return config, nil
}

// send sends a version that was read, or a later change of it, and reports
// whether anything was sent. For a resync a delete marker is asked for
// before it is sent, as a version always is.
func (r *Replicator) send(
	ctx context.Context, target delivery.Target, item delivery.Item, config bucket.ReplicationConfig, v object.Version, resync bool,
) (object.ReplicationStatus, bool, error) {
	change := isChange(item.Event)
	switch {
	case v.Replication == object.ReplicationReplica:
		return "", false, r.notSent(ctx, target, delivery.ClassRejected, fmt.Errorf("%w: a replica is never sent on", errNotSent))
	case v.DeleteMarker != (item.Event == delivery.EventMarker):
		// A version ID names one version, which does not change: the
		// row and the version disagree, and nothing is decided on either.
		// A delete marker has no tags, retention or legal hold.
		return "", false, delivery.Failed(delivery.ClassInternal, fmt.Errorf("replicator.Deliver: %w: the row and the version are not of one kind", errNotSent))
	case v.DeleteMarker && len(config.Replicates(delivery.EventMarker, v.Key, item.By, false, "")) == 0:
		// A marker crosses only while the deciding rule copies markers,
		// and never one the lifecycle service made: one that crosses
		// can hide a version at the destination.
		return "", false, r.notSent(ctx, target, delivery.ClassRejected, fmt.Errorf("%w: no rule copies the marker", errNotSent))
	case change && v.Encryption.Mode == object.SSEC:
		// The version itself never crossed, and is FAILED already.
		return "", false, r.notSent(ctx, target, delivery.ClassSSEC, fmt.Errorf("%w: a change of a version of a customer's key", errNotSent))
	case !v.DeleteMarker && v.Encryption.Mode == object.SSEC:
		return object.ReplicationFailed, false, delivery.Failed(delivery.ClassSSEC,
			fmt.Errorf("replicator.Deliver: %w: encrypted with a key of the customer's", errNotSent))
	case v.Seq > object.MaxReplicaSeq && !change:
		// A bucket promoted above a replica at the bound (ADR-0026,
		// "Order"): no deployment takes a copy at such a sequence.
		return object.ReplicationFailed, false, delivery.Failed(delivery.ClassRejected,
			fmt.Errorf("replicator.Deliver: %w: a sequence beyond the bound of a replica", errNotSent))
	}
	site, err := r.sites.Site(ctx, target)
	if err != nil {
		return "", false, replicaFailure(err)
	}
	generation := r.generation(target)
	answer, err := r.check(ctx, site, target.Name, generation, config.Bucket)
	if err != nil {
		return "", false, err
	}
	if change {
		status, err := r.sendChange(ctx, target, site, item, config.Bucket, v, answer)
		return status, false, err
	}
	if !v.DeleteMarker && r.locked(v.Lock) && !answer.ObjectLock {
		// The bucket there could keep neither the retention nor the legal
		// hold: an operator gives it Object Lock, or the rule here
		// another bucket.
		return "", false, delivery.Failed(delivery.ClassObjectLock,
			fmt.Errorf("replicator.Deliver: %w: it has no Object Lock, and the version carries a lock", errSiteBucket))
	}
	copied := v
	copied.Bucket = config.Bucket
	// A null version is sent at its own sequence; the null version of
	// before versions, at sequence 0 here, at 1 - below every version its
	// bucket made since (ADR-0026, "Open", 6).
	copied.Replica = &object.Replica{From: r.source, Seq: max(v.Seq, 1), Null: !v.Versioned}
	var sent bool
	if v.DeleteMarker {
		sent, err = r.sendMarker(ctx, site, item, copied, resync)
	} else {
		sent, err = r.sendVersion(ctx, target.Name, generation, site, item, copied)
	}
	switch class := delivery.ClassOf(err); {
	case err == nil:
		return object.ReplicationCompleted, sent, nil
	case class.Passes():
		return object.ReplicationFailed, false, fmt.Errorf("replicator.Deliver: %w", err)
	}
	return "", false, replicaFailure(err)
}

// sendMarker sends a delete marker, and reports whether it was sent. A row
// of the outbox sends it as it is - a marker sent twice is taken the second
// time as there - but a resync asks first (HeadVersion), so that what it
// counts as sent is what the other deployment lacked: a marker there is
// there with no entity tag, and anything there with one under the marker's
// ID holds other bytes (delivery.ClassRejected).
func (r *Replicator) sendMarker(ctx context.Context, site ports.SiteClient, item delivery.Item, copied object.Version, resync bool) (bool, error) {
	if resync {
		etag, found, err := site.HeadVersion(ctx, copied.Bucket, copied.Key, item.Version)
		switch {
		case err != nil:
			return false, err
		case found && etag == "":
			return false, nil
		case found:
			return false, delivery.Failed(delivery.ClassRejected, errOtherBytes)
		}
	}
	if err := site.PutMarker(ctx, copied); err != nil {
		return false, err
	}
	return true, nil
}

// locked reports whether a lock is one to send: a legal hold, or a
// retention that ends more than object.RetentionMargin after this side's
// clock - one that ends sooner is left out of a copy, and is no change to
// carry (ADR-0026, "Open", 7).
func (r *Replicator) locked(lock object.Lock) bool {
	return lock.LegalHold || (lock.Retention != nil && lock.Retention.Until.After(r.clock.Now().Add(object.RetentionMargin)))
}

// sendChange carries a later change of a version to the version there (PLAN
// 8.1.5): its tags, its retention or its legal hold, as the version has them
// now - so that changes sent in the order of the outbox leave the version
// there as it is here.
//
// The version there is asked for first (HeadVersion): a change is made only
// to the version this deployment sent, with its entity tag. One that is not
// there, or holds other bytes under the version's ID, is FAILED and nothing
// is sent - so a retention or a legal hold is never put on a version that is
// not this one's. The question is asked rather than the version's status
// read here, because a status is acknowledged in batches and may not say
// yet what became of the version.
//
// A retention that ends within object.RetentionMargin of this side's clock
// is nothing to carry, as it is left out of a copy, and is passed with no
// status. In a bucket there without Object Lock a lock to set holds the
// target (delivery.ClassObjectLock), and one to lift is nothing to carry. A
// change the other deployment refuses for itself - a refusal by Object Lock
// (403 ObjectLocked), a version not there, a date it will not take - is
// FAILED, and the target goes on; a change it takes reports no status: the
// version there is as it was said to be when it was sent, which only its
// own row says.
func (r *Replicator) sendChange(
	ctx context.Context, target delivery.Target, site ports.SiteClient, item delivery.Item, destination string, v object.Version, answer ports.SiteBucket,
) (object.ReplicationStatus, error) {
	lockChange := item.Event != delivery.EventTags
	if item.Event == delivery.EventRetention && v.Lock.Retention != nil && !r.locked(object.Lock{Retention: v.Lock.Retention}) {
		return "", r.notSent(ctx, target, delivery.ClassRejected, fmt.Errorf("%w: a retention that ends within the margin", errNotSent))
	}
	if lockChange && !answer.ObjectLock {
		if (item.Event == delivery.EventRetention && v.Lock.Retention != nil) || (item.Event == delivery.EventLegalHold && v.Lock.LegalHold) {
			return "", delivery.Failed(delivery.ClassObjectLock,
				fmt.Errorf("replicator.Deliver: %w: it has no Object Lock, and the change sets a lock", errSiteBucket))
		}
		return "", r.notSent(ctx, target, delivery.ClassRejected, fmt.Errorf("%w: no lock to lift in a bucket without Object Lock", errNotSent))
	}
	etag, found, err := site.HeadVersion(ctx, destination, v.Key, item.Version)
	switch {
	case err != nil:
		return "", replicaFailure(err)
	case !found:
		return object.ReplicationFailed, delivery.Failed(delivery.ClassRejected,
			fmt.Errorf("replicator.Deliver: %w: the version is not there", errNotSent))
	case etag != v.ETag:
		return object.ReplicationFailed, delivery.Failed(delivery.ClassRejected, fmt.Errorf("replicator.Deliver: %w", errOtherBytes))
	}
	switch item.Event {
	case delivery.EventTags:
		var tags map[string]string
		if len(v.Tags) > 0 {
			tags = v.Tags
		}
		err = site.PutTags(ctx, destination, v.Key, item.Version, tags)
	case delivery.EventRetention:
		err = site.PutRetention(ctx, destination, v.Key, item.Version, v.Lock.Retention)
	default:
		err = site.PutLegalHold(ctx, destination, v.Key, item.Version, v.Lock.LegalHold)
	}
	switch class := delivery.ClassOf(err); {
	case err == nil:
		return "", nil
	case class.Passes():
		return object.ReplicationFailed, fmt.Errorf("replicator.Deliver: %w", err)
	}
	return "", replicaFailure(err)
}

// generation returns the generation of a target's connection. A target made
// again under its name, or whose connection changed, has a new one: what
// was probed and the uploads begun at the endpoint it had are forgotten -
// the uploads are not aborted, as the client of the target as it is now does
// not reach them; the other deployment's own lifecycle rules for uploads
// clear them.
//
// A target older than the one known - by its time of definition, then by
// when it was last changed - is that of an attempt that began before the
// change, or of the loop of a target deleted since: it is given a
// generation of its own, which nothing is kept under, and changes nothing of
// what is known of the target as it is.
func (r *Replicator) generation(target delivery.Target) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	known, ok := r.conns[target.Name]
	switch {
	case ok && olderTarget(target, known.target):
		r.next++
		return r.next
	case ok && known.target.SameConnection(target) && known.target.CreatedAt.Equal(target.CreatedAt):
		// The same connection, perhaps changed otherwise since: the newest
		// is what an older one is judged against.
		known.target = target.Clone()
		r.conns[target.Name] = known
		return known.generation
	}
	r.next++
	r.conns[target.Name] = siteConn{target: target.Clone(), generation: r.next}
	r.dropLocked(func(name string) bool { return name == target.Name })
	return r.next
}

// olderTarget reports whether a is an older state of the target b is: made
// before it, or made with it and changed before it.
func olderTarget(a, b delivery.Target) bool {
	return a.CreatedAt.Before(b.CreatedAt) || (a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Before(b.UpdatedAt))
}

// dropLocked forgets what was probed of the targets gone says, and the
// uploads remembered of them that no attempt is sending through; the
// uploads are not aborted, as the client of a target as it is now does not
// reach the endpoint it had. r.mu is held.
func (r *Replicator) dropLocked(gone func(name string) bool) {
	for key := range r.checked {
		if gone(key.target) {
			delete(r.checked, key)
		}
	}
	kept := r.order[:0]
	for _, name := range r.order {
		if upload := r.uploads[name]; gone(name.target) && upload.pinned == 0 {
			r.parts -= len(upload.sent)
			delete(r.uploads, name)
			continue
		}
		kept = append(kept, name)
	}
	r.order = kept
}

// KeepTargets implements TargetKeeper: what is known of the targets that are
// not named - the generation of their connection, their probes, their idle
// uploads - is forgotten.
func (r *Replicator) KeepTargets(names []string) {
	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.conns {
		if !present[name] {
			delete(r.conns, name)
		}
	}
	r.dropLocked(func(name string) bool { return !present[name] })
}

// check asks the other deployment whether its bucket keeps versions and is a
// replica of this deployment, and whether it has Object Lock, unless it was
// told within siteCheckEvery; it returns the answer.
func (r *Replicator) check(ctx context.Context, site ports.SiteClient, target string, generation uint64, bucketName string) (ports.SiteBucket, error) {
	key := siteBucket{target: target, generation: generation, bucket: bucketName}
	now := r.clock.Now()
	r.mu.Lock()
	last, ok := r.checked[key]
	r.mu.Unlock()
	if ok && now.Sub(last.at) < siteCheckEvery && !now.Before(last.at) {
		return last.answer, nil
	}
	answer, err := site.Check(ctx, bucketName)
	switch {
	case err != nil:
		return ports.SiteBucket{}, replicaFailure(err)
	case answer.Versioning != bucket.VersioningEnabled:
		return ports.SiteBucket{}, delivery.Failed(delivery.ClassVersioning, fmt.Errorf("replicator.Deliver: %w: it does not keep versions", errSiteBucket))
	case answer.ReplicaOf != r.source:
		return ports.SiteBucket{}, delivery.Failed(delivery.ClassNotReplica, fmt.Errorf("replicator.Deliver: %w: it is no replica of this deployment", errSiteBucket))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checked[key] = probed{at: now, answer: answer}
	return answer, nil
}

// forget makes the next item to a bucket of the other deployment probe it
// again: something failed there.
func (r *Replicator) forget(target delivery.Target, bucketName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.checked {
		if key.target == target.Name && key.bucket == bucketName {
			delete(r.checked, key)
		}
	}
}

// sendVersion sends a version that has bytes, unless the other deployment
// has it already: in one PUT, or as an upload of the same parts. It reports
// whether it was sent.
func (r *Replicator) sendVersion(ctx context.Context, target string, generation uint64, site ports.SiteClient, item delivery.Item, copied object.Version) (bool, error) {
	etag, found, err := site.HeadVersion(ctx, copied.Bucket, copied.Key, item.Version)
	switch {
	case err != nil:
		return false, err
	case found && etag == copied.ETag:
		// An attempt before completed it, and its answer was lost: what
		// was remembered of its upload is of no more use.
		r.forgetUpload(uploadOf(target, generation, item, copied))
		return false, nil
	case found:
		return false, delivery.Failed(delivery.ClassRejected, errOtherBytes)
	}
	if !copied.Multipart() {
		body, err := r.open(ctx, item, object.Range{Offset: 0, Length: copied.Size})
		if err != nil {
			return false, err
		}
		defer func() { _ = body.Close() }()
		return true, site.PutReplica(ctx, copied, body)
	}
	return true, r.sendParts(ctx, target, generation, site, item, copied)
}

// sendParts sends a version of parts as an upload of the same parts: the
// entity tag is then the same by construction. It goes on with the upload
// an attempt before began, when there is one, from the parts that arrived;
// the other uploads this deployment left of the key are aborted first. An
// upload that is refused for good is aborted and forgotten; one that fails
// otherwise is kept for the next attempt.
func (r *Replicator) sendParts(ctx context.Context, target string, generation uint64, site ports.SiteClient, item delivery.Item, copied object.Version) error {
	name := uploadOf(target, generation, item, copied)
	// The upload is pinned (resumable, remember) while this attempt may
	// send through it, and idle once it returns, however it returns.
	var pinned *siteUpload
	defer func() {
		if pinned != nil {
			r.unpin(pinned)
		}
	}()
	upload, there, err := r.resumable(ctx, site, name)
	if err != nil {
		return err
	}
	pinned = upload
	stale, err := site.ListReplicaUploads(ctx, copied.Bucket, copied.Key)
	if err != nil {
		return err
	}
	for _, id := range stale {
		if upload != nil && id == upload.id {
			continue
		}
		if err := site.AbortUpload(ctx, copied.Bucket, copied.Key, id); err != nil {
			return err
		}
	}
	if upload == nil {
		id, err := site.CreateUpload(ctx, copied)
		if err != nil {
			return err
		}
		upload = r.remember(ctx, name, &siteUpload{id: id, sent: map[int]string{}, site: site, bucket: copied.Bucket, key: copied.Key})
		pinned = upload
	}
	parts, err := r.sendEachPart(ctx, name, site, item, copied, upload, there)
	if err == nil {
		err = site.CompleteReplica(ctx, copied, upload.id, parts)
	}
	switch {
	case err == nil:
		r.forgetUpload(name)
	case delivery.ClassOf(err).Passes():
		// Never completed: aborted, so that it holds no space there.
		r.forgetUpload(name)
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replicationAbortTimeout)
		_ = site.AbortUpload(abortCtx, copied.Bucket, copied.Key, upload.id)
		cancel()
	}
	return err
}

// uploadOf names the version an upload there is of.
func uploadOf(target string, generation uint64, item delivery.Item, copied object.Version) siteVersion {
	return siteVersion{
		siteBucket: siteBucket{target: target, generation: generation, bucket: copied.Bucket},
		key:        copied.Key, version: item.Version.String(),
	}
}

// resumable returns the upload an attempt before began for a version, and
// the parts of it that are there - each with the entity tag it was given
// when it was sent; nil when there is none to go on with. An upload the
// other deployment no longer has is forgotten.
func (r *Replicator) resumable(ctx context.Context, site ports.SiteClient, name siteVersion) (*siteUpload, map[int]ports.SitePart, error) {
	r.mu.Lock()
	upload := r.uploads[name]
	if upload != nil {
		// Pinned at once, under the lock: nothing forgets it between here
		// and the attempt's end (unpin).
		upload.pinned++
	}
	r.mu.Unlock()
	if upload == nil {
		return nil, nil, nil
	}
	listed, found, err := site.ListParts(ctx, name.bucket, name.key, upload.id)
	if err != nil {
		r.unpin(upload)
		return nil, nil, err
	}
	if !found {
		r.unpin(upload)
		r.forgetUpload(name)
		return nil, nil, nil
	}
	there := map[int]ports.SitePart{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, part := range listed {
		if etag, ok := upload.sent[part.Number]; ok && etag == part.ETag {
			there[part.Number] = part
		}
	}
	return upload, there, nil
}

// remember keeps an upload begun for a version, pinned for the attempt that
// began it, and forgets the oldest idle uploads beyond the bounds.
func (r *Replicator) remember(ctx context.Context, name siteVersion, upload *siteUpload) *siteUpload {
	r.mu.Lock()
	upload.pinned++
	r.uploads[name] = upload
	r.order = append(r.order, name)
	evicted := r.evictLocked()
	r.mu.Unlock()
	r.abort(ctx, evicted)
	return upload
}

// unpin ends the use of an upload by an attempt.
func (r *Replicator) unpin(upload *siteUpload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	upload.pinned--
}

// evictLocked forgets the oldest idle uploads - those no attempt is sending
// through, kept after a failure to go on with - while more than
// maxRememberedUploads are remembered or they remember more than
// maxRememberedParts parts, and returns them to be aborted; r.mu is held.
//
// An upload that is pinned is never forgotten or aborted: when every upload
// is in use the bounds are exceeded for as long as that lasts - at most
// replication-workers uploads a target - rather than abort work in flight,
// which another attempt would only begin again (ADR-0026).
func (r *Replicator) evictLocked() []*siteUpload {
	var evicted []*siteUpload
	for i := 0; i < len(r.order) && (len(r.uploads) > r.maxUploads || r.parts > r.maxParts); {
		name := r.order[i]
		if r.uploads[name].pinned > 0 {
			i++
			continue
		}
		upload := r.uploads[name]
		r.parts -= len(upload.sent)
		delete(r.uploads, name)
		r.order = append(r.order[:i:i], r.order[i+1:]...)
		evicted = append(evicted, upload)
	}
	return evicted
}

// abort aborts uploads that are forgotten, as a best effort, each through
// the client it was begun through: left, they would hold space there until
// the other deployment's own rules clear them.
func (r *Replicator) abort(ctx context.Context, uploads []*siteUpload) {
	for _, upload := range uploads {
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replicationAbortTimeout)
		_ = upload.site.AbortUpload(abortCtx, upload.bucket, upload.key, upload.id)
		cancel()
	}
}

// forgetUpload forgets the upload of a version.
func (r *Replicator) forgetUpload(name siteVersion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	upload, ok := r.uploads[name]
	if !ok {
		return
	}
	r.parts -= len(upload.sent)
	delete(r.uploads, name)
	for i, kept := range r.order {
		if kept == name {
			r.order = append(r.order[:i:i], r.order[i+1:]...)
			break
		}
	}
}

// sent records the entity tag a part was given there, and forgets the
// oldest other uploads beyond the bound of parts.
func (r *Replicator) sent(ctx context.Context, name siteVersion, upload *siteUpload, number int, etag string) {
	r.mu.Lock()
	if _, ok := upload.sent[number]; !ok && r.uploads[name] == upload {
		r.parts++
	}
	upload.sent[number] = etag
	evicted := r.evictLocked()
	r.mu.Unlock()
	r.abort(ctx, evicted)
}

// sendEachPart sends the parts of a version in order, each with its own
// number and size, but those that are there already with the size they have
// here, and returns them all as the completion names them.
func (r *Replicator) sendEachPart(
	ctx context.Context, name siteVersion, site ports.SiteClient, item delivery.Item, copied object.Version, upload *siteUpload, there map[int]ports.SitePart,
) ([]ports.SitePart, error) {
	sent := make([]ports.SitePart, 0, min(copied.Parts, object.MaxParts))
	offset := int64(0)
	for len(sent) < copied.Parts {
		parts, err := r.objects.ReplicationParts(ctx, replicationPrincipal(), item.Bucket, item.Key, item.Version, offset, replicationPartsPage)
		if err != nil {
			return nil, err
		}
		before := len(sent)
		for _, part := range parts {
			if part.Index < len(sent) {
				continue
			}
			if part.Index != len(sent) || part.Size < 0 || len(sent) == copied.Parts {
				return nil, fmt.Errorf("%w: %w: part %d", domain.ErrIntegrity, errParts, len(sent)+1)
			}
			offset = part.Offset + part.Size
			if arrived, ok := there[part.Number]; ok && arrived.Size == part.Size {
				sent = append(sent, ports.SitePart{Number: part.Number, ETag: arrived.ETag})
				continue
			}
			body, err := r.open(ctx, item, object.Range{Offset: part.Offset, Length: part.Size})
			if err != nil {
				return nil, err
			}
			etag, err := site.PutPart(ctx, copied.Bucket, copied.Key, upload.id, part.Number, part.Size, body)
			_ = body.Close()
			if err != nil {
				return nil, err
			}
			r.sent(ctx, name, upload, part.Number, etag)
			sent = append(sent, ports.SitePart{Number: part.Number, ETag: etag})
		}
		if len(sent) == before {
			return nil, fmt.Errorf("%w: %w: %d of %d parts", domain.ErrIntegrity, errParts, len(sent), copied.Parts)
		}
	}
	return sent, nil
}

// open opens a span of the version of an item, read at the rate of the
// replicator.
func (r *Replicator) open(ctx context.Context, item delivery.Item, span object.Range) (io.ReadCloser, error) {
	body, err := r.objects.OpenReplicationSpan(ctx, replicationPrincipal(), item.Bucket, item.Key, item.Version, span)
	if err != nil {
		return nil, err
	}
	if r.rate == nil {
		return body, nil
	}
	return &ratedBody{ReadCloser: body, ctx: ctx, rate: r.rate}, nil
}

// byteRate holds the bytes read to be sent to a rate: each read is charged
// at the rate, and a read that would go beyond it waits until it does not.
// The bytes of any window of time are then at most the rate times the
// window, and one read more for each reader. It is shared by every version
// being sent.
type byteRate struct {
	perSecond float64
	clock     ports.Clock

	mu sync.Mutex
	// due is when the bytes charged so far have been sent at the rate.
	due time.Time
}

// charge charges n bytes and waits until they are within the rate, or ctx
// ends.
func (b *byteRate) charge(ctx context.Context, n int) error {
	now := b.clock.Now()
	b.mu.Lock()
	if b.due.Before(now) {
		b.due = now
	}
	start := b.due
	b.due = b.due.Add(time.Duration(float64(n) / b.perSecond * float64(time.Second)))
	b.mu.Unlock()
	if wait := start.Sub(now); wait > 0 && !pause(ctx, wait) {
		return ctx.Err()
	}
	return nil
}

// ratedBody is a body of a version read at the rate of the replicator.
type ratedBody struct {
	io.ReadCloser
	ctx  context.Context
	rate *byteRate
}

// Read implements io.Reader: at most replicationReadSize bytes at once,
// each read waiting its turn at the rate before it is handed on.
func (b *ratedBody) Read(p []byte) (int, error) {
	if len(p) > replicationReadSize {
		p = p[:replicationReadSize]
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if waitErr := b.rate.charge(b.ctx, n); waitErr != nil {
			return 0, waitErr
		}
	}
	return n, err
}

// ReplicationVersion returns a version named by its ID as the replicator
// reads it to send it to another deployment (ADR-0026): a delete marker
// too, which HeadVersion answers with domain.ErrMethodNotAllowed. It needs
// s3:GetObjectVersion on the object - the replication service
// (iam.ServiceReplication) holds it and nothing else - and is not counted
// as a download: nothing the deployment reads for itself is. The error
// wraps domain.ErrAccessDenied, domain.ErrBucketNotFound,
// domain.ErrObjectNotFound and domain.ErrVersionNotFound.
func (s *ObjectService) ReplicationVersion(ctx context.Context, principal iam.Principal, bucketName, key string, ref object.VersionRef) (object.Version, error) {
	v, err := s.replicationVersion(ctx, principal, bucketName, key, ref)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.ReplicationVersion: %w", err)
	}
	return v, nil
}

// replicationVersion authorizes the read of a version by its ID and reads
// it.
func (s *ObjectService) replicationVersion(ctx context.Context, principal iam.Principal, bucketName, key string, ref object.VersionRef) (object.Version, error) {
	resource := iam.Resource{Bucket: bucketName, Key: key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionGetObjectVersion, Resource: resource}); err != nil {
		return object.Version{}, err
	}
	if err := object.ValidateKey(key); err != nil {
		return object.Version{}, err
	}
	return s.index.GetVersion(ctx, bucketName, key, ref)
}

// OpenReplicationSpan opens a span of the bytes of a version named by its
// bucket, its key and its reference, decrypted, every byte authenticated
// before it is handed on. It is authorized and read as ReplicationVersion
// is, and reads no version of a customer's key
// (domain.ErrCustomerKeyRequired) and no delete marker
// (domain.ErrMethodNotAllowed); a span beyond the version is
// domain.ErrInvalidRange. The caller closes the body.
func (s *ObjectService) OpenReplicationSpan(
	ctx context.Context, principal iam.Principal, bucketName, key string, ref object.VersionRef, span object.Range,
) (io.ReadCloser, error) {
	v, err := s.replicationVersion(ctx, principal, bucketName, key, ref)
	if err != nil {
		return nil, fmt.Errorf("object.OpenReplicationSpan: %w", err)
	}
	switch {
	case v.DeleteMarker:
		return nil, fmt.Errorf("object.OpenReplicationSpan: %w: a delete marker has no bytes", domain.ErrMethodNotAllowed)
	case span.Offset < 0 || span.Length < 0 || span.Offset > v.Size || span.Length > v.Size-span.Offset:
		return nil, fmt.Errorf("object.OpenReplicationSpan: %w: %d bytes from %d of %d", domain.ErrInvalidRange, span.Length, span.Offset, v.Size)
	}
	body, err := s.openSpan(ctx, v, span, nil)
	if err != nil {
		return nil, fmt.Errorf("object.OpenReplicationSpan: %w", err)
	}
	return body, nil
}

// ReplicationParts returns up to limit parts of a version of parts named by
// its bucket, its key and its reference, in order, from the one that holds
// the byte at offset. It is authorized and read as ReplicationVersion is; a
// version that is not made of parts is domain.ErrInvalidArgument.
func (s *ObjectService) ReplicationParts(
	ctx context.Context, principal iam.Principal, bucketName, key string, ref object.VersionRef, offset int64, limit int,
) ([]object.Part, error) {
	v, err := s.replicationVersion(ctx, principal, bucketName, key, ref)
	if err != nil {
		return nil, fmt.Errorf("object.ReplicationParts: %w", err)
	}
	if !v.Multipart() {
		return nil, fmt.Errorf("object.ReplicationParts: %w: the version is not made of parts", domain.ErrInvalidArgument)
	}
	parts, err := s.index.ObjectParts(ctx, v.Bucket, v.DataID, offset, limit)
	if err != nil {
		return nil, fmt.Errorf("object.ReplicationParts: %w", err)
	}
	return parts, nil
}

// ReplicationRateBytes returns a rate of replication-rate-mib, in MiB per
// second, as the bytes per second ReplicatorConfig.RateBytes takes; 0 and
// less are no bound.
func ReplicationRateBytes(mib int) int64 {
	if mib <= 0 {
		return 0
	}
	return int64(mib) << 20
}
