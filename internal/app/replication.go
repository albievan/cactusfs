package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Replication between sites, at the destination (ADR-0026). A replica bucket
// takes new versions and delete markers from one source deployment, and from
// nobody else - that is the state's to decide, in apply. What is decided
// here is who may make a bucket a replica or promote it, and who may write a
// copy: s3:ReplicateObject (s3:ReplicateDelete for a marker), which no
// wildcard grants. The protocol of the copy - which headers, signed how - is
// the inbound adapter's.

// ReplicaMode says whether a bucket is the replica of another deployment's.
type ReplicaMode struct {
	// Source is the ID of the deployment the bucket takes its versions
	// from; empty when the bucket is no replica and takes writes.
	Source string
}

// Replica reports whether the bucket is a replica.
func (m ReplicaMode) Replica() bool {
	return m.Source != ""
}

// ReplicationService holds the use cases of replica mode (ADR-0026): reading
// it, and making a bucket a replica of another deployment or promoting it.
// Each passes the single authorizer with an admin: action on the bucket, so
// that a bucket's own administrator, who holds s3:*, can do neither; each
// change carries its audit record in the entry that makes it.
//
// It holds the use cases of a bucket's replication configuration too - the
// source side: reading it (s3:GetReplicationConfiguration), and setting and
// removing it (s3:PutReplicationConfiguration, as S3 has no action of its
// own for removing it), each through the single authorizer and each change
// audited; and the start of a resync (admin:ResyncReplication), which the
// ResyncWorker walks.
type ReplicationService struct {
	buckets ports.BucketStore
	store   ports.ReplicationStore
	targets ports.TargetStore
	authz   ports.Authorizer
	auditor *Auditor
}

// ReplicationServiceConfig lists what a ReplicationService depends on.
type ReplicationServiceConfig struct {
	Buckets ports.BucketStore
	Store   ports.ReplicationStore
	// Targets is where the target a replication configuration names is
	// read; without it every configuration is refused.
	Targets ports.TargetStore
	Authz   ports.Authorizer
	// Auditor builds the records of changes; without one every change is
	// refused.
	Auditor *Auditor
}

// NewReplicationService returns a ReplicationService.
func NewReplicationService(cfg ReplicationServiceConfig) *ReplicationService {
	return &ReplicationService{buckets: cfg.Buckets, store: cfg.Store, targets: cfg.Targets, authz: cfg.Authz, auditor: cfg.Auditor}
}

// ReplicaMode returns whether a bucket is a replica, and of which
// deployment. It needs admin:GetBucketReplicaMode on the bucket. The error
// wraps domain.ErrAccessDenied and domain.ErrBucketNotFound.
func (r *ReplicationService) ReplicaMode(ctx context.Context, principal iam.Principal, bucketName string) (ReplicaMode, error) {
	if err := r.authz.Authorize(ctx, bucketRequest(principal, iam.ActionGetBucketReplicaMode, bucketName)); err != nil {
		return ReplicaMode{}, fmt.Errorf("replication.ReplicaMode: %w", err)
	}
	mode, err := r.read(ctx, bucketName)
	if err != nil {
		return ReplicaMode{}, fmt.Errorf("replication.ReplicaMode: %w", err)
	}
	return mode, nil
}

// SetReplicaMode makes a bucket the replica of the deployment source, or -
// with an empty source - promotes it: from then on it takes writes again,
// and nothing more from its source. It returns the mode the bucket has
// afterwards. It needs admin:PutBucketReplicaMode on the bucket, and the
// change carries its audit record in its entry.
//
// Naming the source the bucket already has, and promoting a bucket that is
// no replica, change nothing and are no error. The error wraps
// domain.ErrAccessDenied; domain.ErrInvalidArgument for a source that is no
// deployment ID (nothing is proposed); domain.ErrBucketNotFound;
// domain.ErrInvalidBucketState when the bucket does not keep versions or has
// a replication configuration of its own; domain.ErrConflict while its usage
// is not exact; domain.ErrBucketNotEmpty when it holds a version; and
// domain.ErrUnavailable until every member of the metadata group knows
// replication, or when there is no audit trail to record the change in.
func (r *ReplicationService) SetReplicaMode(ctx context.Context, principal iam.Principal, bucketName, source string) (ReplicaMode, error) {
	if err := r.authz.Authorize(ctx, bucketRequest(principal, iam.ActionPutBucketReplicaMode, bucketName)); err != nil {
		return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: %w", err)
	}
	if source != "" {
		if err := object.ValidateReplicaSource(source); err != nil {
			return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: %w", err)
		}
	}
	if r.auditor == nil {
		return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: %w: no audit trail to record the change in", domain.ErrUnavailable)
	}
	details := map[string]string{"source": source}
	if source == "" {
		details = map[string]string{"promoted": "true"}
	}
	record := r.auditor.Record(principal, iam.ActionPutBucketReplicaMode, iam.Resource{Bucket: bucketName}, audit.Succeeded, details)
	if err := r.store.SetBucketReplica(ctx, bucketName, source, record); err != nil {
		return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: %w", err)
	}
	// What the bucket is now is read after the change, without a second
	// authorization: the caller may change the mode and need not read it.
	mode, err := r.read(ctx, bucketName)
	if err != nil {
		// The mode was set; a repeat sets the same again.
		return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: the mode was set, but cannot be read: %w", err)
	}
	return mode, nil
}

// read returns the mode of a bucket without asking anyone.
func (r *ReplicationService) read(ctx context.Context, bucketName string) (ReplicaMode, error) {
	b, err := r.buckets.GetBucket(ctx, bucketName)
	if err != nil {
		return ReplicaMode{}, err
	}
	return ReplicaMode{Source: b.ReplicaFrom}, nil
}

// Configuration returns the replication configuration of a bucket: where
// its versions are copied to, and the rules in the order they were put. A
// bucket without one has none (bucket.ReplicationConfig.Empty), which each
// API answers as its own "not found". It needs
// s3:GetReplicationConfiguration on the bucket. The error wraps
// domain.ErrAccessDenied, domain.ErrBucketNotFound, domain.ErrUnavailable for
// a configuration of a later release, and domain.ErrIntegrity for one that
// cannot be read.
func (r *ReplicationService) Configuration(ctx context.Context, principal iam.Principal, bucketName string) (bucket.ReplicationConfig, error) {
	if err := r.authz.Authorize(ctx, bucketRequest(principal, iam.ActionGetReplication, bucketName)); err != nil {
		return bucket.ReplicationConfig{}, fmt.Errorf("replication.Configuration: %w", err)
	}
	raw, err := r.buckets.GetBucketConfig(ctx, bucketName, bucket.ConfigReplication)
	if err != nil {
		return bucket.ReplicationConfig{}, fmt.Errorf("replication.Configuration: %w", err)
	}
	config, err := bucket.ParseReplicationConfig(raw)
	switch {
	case errors.Is(err, domain.ErrUnsupportedFormat):
		return bucket.ReplicationConfig{}, fmt.Errorf("replication.Configuration: %w: the replication configuration of bucket %s is of a later release: %s",
			domain.ErrUnavailable, bucketName, err.Error())
	case err != nil:
		return bucket.ReplicationConfig{}, fmt.Errorf("replication.Configuration: %w: stored replication configuration of bucket %s: %s",
			domain.ErrIntegrity, bucketName, err.Error())
	}
	return config, nil
}

// SetConfiguration replaces the replication configuration of a bucket and
// returns it as it is kept (ADR-0026, "Configuration"). It needs
// s3:PutReplicationConfiguration on the bucket, and the change is audited.
//
// A rule without an ID is given one, "rule-<n>", that no other rule of the
// configuration has. Then the configuration is judged
// (bucket.ReplicationConfig.Validate: one target, one bucket there, 1 to
// bucket.MaxReplicationRules rules) and, in this order, the bucket: it must
// be there, must not be a replica itself, and must keep versions; then the
// target: it must exist, be another deployment (delivery.KindS3) and admit
// the bucket. Which of the three a target fails is not said, so that a
// caller who may configure a bucket learns nothing of targets that are not
// open to it. The store decides all of it again when the change is
// committed, which is what holds.
//
// The error wraps domain.ErrAccessDenied; domain.ErrInvalidArgument for a
// configuration that cannot be kept or a target it may not name (nothing is
// proposed); domain.ErrBucketNotFound; domain.ErrInvalidBucketState for a
// bucket that is a replica; domain.ErrVersioningRequired for one whose
// versioning is not enabled; and domain.ErrUnavailable until every member of
// the metadata group knows replication, or when there is no audit trail to
// record the change in.
func (r *ReplicationService) SetConfiguration(ctx context.Context, principal iam.Principal, bucketName string, config bucket.ReplicationConfig) (bucket.ReplicationConfig, error) {
	if err := r.authz.Authorize(ctx, bucketRequest(principal, iam.ActionPutReplication, bucketName)); err != nil {
		return bucket.ReplicationConfig{}, fmt.Errorf("replication.SetConfiguration: %w", err)
	}
	kept, err := r.setConfiguration(ctx, principal, bucketName, config)
	if err != nil {
		return bucket.ReplicationConfig{}, fmt.Errorf("replication.SetConfiguration: %w", err)
	}
	return kept, nil
}

// DeleteConfiguration removes the replication configuration of a bucket; a
// bucket without one is left as it is. It is asked as setting one is
// (s3:PutReplicationConfiguration, as in S3), and audited. Rows of the
// outbox appended before are sent all the same, and nothing is removed at
// the other deployment. The error wraps domain.ErrAccessDenied,
// domain.ErrBucketNotFound, and domain.ErrUnavailable until every member of
// the metadata group knows replication, or when there is no audit trail to
// record the change in.
func (r *ReplicationService) DeleteConfiguration(ctx context.Context, principal iam.Principal, bucketName string) error {
	if err := r.authz.Authorize(ctx, bucketRequest(principal, iam.ActionPutReplication, bucketName)); err != nil {
		return fmt.Errorf("replication.DeleteConfiguration: %w", err)
	}
	if r.auditor == nil {
		return fmt.Errorf("replication.DeleteConfiguration: %w: no audit trail to record the change in", domain.ErrUnavailable)
	}
	if err := r.buckets.SetBucketConfig(ctx, bucketName, bucket.ConfigReplication, nil); err != nil {
		return fmt.Errorf("replication.DeleteConfiguration: %w", err)
	}
	details := map[string]string{"replication": "removed"}
	r.auditor.Succeeded(ctx, r.auditor.Record(principal, iam.ActionPutReplication, iam.Resource{Bucket: bucketName}, audit.Succeeded, details))
	return nil
}

// Resync starts the walk that sends the other deployment what it lacks of a
// bucket (ADR-0026, "Resync"), or starts it again from the beginning, and
// returns the record it stored: running, nothing looked at yet. The walk is
// the ResyncWorker's, on the node that leads; this only stores where it
// begins, through Raft, so that any node may be asked.
//
// It needs admin:ResyncReplication on the bucket - a load on the link and on
// the other deployment that a bucket's own administrator, who holds s3:*,
// cannot start - and is audited. The walk is of the target the bucket's
// replication configuration names now; that target must still be another
// deployment open to the bucket.
//
// The error wraps domain.ErrAccessDenied; domain.ErrBucketNotFound;
// domain.ErrEntityNotFound for a bucket without a replication configuration;
// domain.ErrInvalidArgument when its target is no longer another deployment
// open to the bucket; domain.ErrIntegrity for a configuration that cannot be
// read; and domain.ErrUnavailable for one of a later release, until every
// member of the metadata group knows replication, or when there is no audit
// trail to record the start in.
func (r *ReplicationService) Resync(ctx context.Context, principal iam.Principal, bucketName string) (bucket.Resync, error) {
	if err := r.authz.Authorize(ctx, bucketRequest(principal, iam.ActionResyncReplication, bucketName)); err != nil {
		return bucket.Resync{}, fmt.Errorf("replication.Resync: %w", err)
	}
	record, err := r.resync(ctx, principal, bucketName)
	if err != nil {
		return bucket.Resync{}, fmt.Errorf("replication.Resync: %w", err)
	}
	return record, nil
}

// resync judges the configuration and its target, stores the start of the
// walk and audits it.
func (r *ReplicationService) resync(ctx context.Context, principal iam.Principal, bucketName string) (bucket.Resync, error) {
	if r.auditor == nil {
		return bucket.Resync{}, fmt.Errorf("%w: no audit trail to record the start in", domain.ErrUnavailable)
	}
	if r.targets == nil {
		return bucket.Resync{}, fmt.Errorf("%w: no targets to send versions to on this node", domain.ErrUnavailable)
	}
	raw, err := r.buckets.GetBucketConfig(ctx, bucketName, bucket.ConfigReplication)
	if err != nil {
		return bucket.Resync{}, err
	}
	config, err := bucket.ParseReplicationConfig(raw)
	switch {
	case errors.Is(err, domain.ErrUnsupportedFormat):
		return bucket.Resync{}, fmt.Errorf("%w: the replication configuration of bucket %s is of a later release: %s", domain.ErrUnavailable, bucketName, err.Error())
	case err != nil:
		return bucket.Resync{}, fmt.Errorf("%w: stored replication configuration of bucket %s: %s", domain.ErrIntegrity, bucketName, err.Error())
	case config.Empty():
		return bucket.Resync{}, fmt.Errorf("%w: bucket %s has no replication configuration", domain.ErrEntityNotFound, bucketName)
	}
	if _, err := r.open(ctx, config.Target, bucketName); err != nil {
		return bucket.Resync{}, err
	}
	record := bucket.Resync{Bucket: bucketName, Target: config.Target, State: bucket.ResyncRunning}
	if err := r.store.PutResync(ctx, record); err != nil {
		return bucket.Resync{}, err
	}
	details := map[string]string{"target": config.Target, "resync": "started"}
	r.auditor.Succeeded(ctx, r.auditor.Record(principal, iam.ActionResyncReplication, iam.Resource{Bucket: bucketName}, audit.Succeeded, details))
	return record, nil
}

// setConfiguration judges the configuration, the bucket and the target,
// proposes the document and audits the change.
func (r *ReplicationService) setConfiguration(ctx context.Context, principal iam.Principal, bucketName string, config bucket.ReplicationConfig) (bucket.ReplicationConfig, error) {
	if r.auditor == nil {
		return bucket.ReplicationConfig{}, fmt.Errorf("%w: no audit trail to record the change in", domain.ErrUnavailable)
	}
	if r.targets == nil {
		return bucket.ReplicationConfig{}, fmt.Errorf("%w: no targets to send versions to on this node", domain.ErrUnavailable)
	}
	config = withRuleIDs(config)
	if err := refuseTies(config.Rules); err != nil {
		return bucket.ReplicationConfig{}, err
	}
	document, err := config.Document()
	if err != nil {
		return bucket.ReplicationConfig{}, err
	}
	// The configuration is returned as Configuration reads it: from the
	// document.
	kept, err := bucket.ParseReplicationConfig(document)
	if err != nil {
		return bucket.ReplicationConfig{}, err
	}
	b, err := r.buckets.GetBucket(ctx, bucketName)
	if err != nil {
		return bucket.ReplicationConfig{}, err
	}
	switch {
	case b.ReplicaFrom != "":
		// A replica bucket is never the source of another: one hop.
		return bucket.ReplicationConfig{}, fmt.Errorf("%w: bucket %s is a replica and is not replicated again", domain.ErrInvalidBucketState, bucketName)
	case b.Versioning != bucket.VersioningEnabled:
		return bucket.ReplicationConfig{}, fmt.Errorf("%w: bucket %s", domain.ErrVersioningRequired, bucketName)
	}
	target, err := r.open(ctx, kept.Target, bucketName)
	if err != nil {
		return bucket.ReplicationConfig{}, err
	}
	if err := r.refuseShared(ctx, bucketName, target, kept.Bucket); err != nil {
		return bucket.ReplicationConfig{}, err
	}
	if err := r.buckets.SetBucketConfig(ctx, bucketName, bucket.ConfigReplication, document); err != nil {
		return bucket.ReplicationConfig{}, err
	}
	details := map[string]string{"target": kept.Target, "destination": kept.Bucket, "rules": strconv.Itoa(len(kept.Rules))}
	r.auditor.Succeeded(ctx, r.auditor.Record(principal, iam.ActionPutReplication, iam.Resource{Bucket: bucketName}, audit.Succeeded, details))
	return kept, nil
}

// open reports, with an error that wraps domain.ErrInvalidArgument, a
// target that the replication configuration of the bucket may not name: one
// that is not there, is no other deployment, or does not admit the bucket -
// in one sentence for all three.
func (r *ReplicationService) open(ctx context.Context, name, bucketName string) (delivery.Target, error) {
	target, err := r.targets.GetTarget(ctx, name)
	switch {
	case errors.Is(err, domain.ErrEntityNotFound):
	case err != nil:
		return delivery.Target{}, err
	case target.Kind.Serves(delivery.ConsumerReplication) && target.Admits(bucketName):
		return target, nil
	}
	return delivery.Target{}, fmt.Errorf("%w: target %s is not another deployment that is open to bucket %s", domain.ErrInvalidArgument, name, bucketName)
}

// refuseShared refuses, with an error that wraps domain.ErrInvalidArgument,
// a bucket of another deployment that the replication configuration of
// another bucket here already sends to through the same target: two source
// buckets in one replica bucket would interleave two unrelated sequences of
// versions there, and the replicator would send versions of one key of both
// side by side (ADR-0026, "One destination a source bucket"). It is told in
// the one sentence every refusal of a target has (open), which says nothing
// of which other bucket names the pair, or that one does.
//
// The other configurations are read only when the target counts a document
// besides the bucket's own (delivery.Target.Refs), and the walk ends once it
// has found as many as it counts: an index of the pairs that are named would
// be a field more in the replicated state, so a schema change, and is not
// made (PLAN, Decisions "8.1.4 re-review"). It is the use case's rule, not
// the store's: two configurations put at the same moment may each pass it
// (Decisions "8.1.4 review"); what every member applies is unchanged.
func (r *ReplicationService) refuseShared(ctx context.Context, bucketName string, target delivery.Target, destination string) error {
	mine, err := r.buckets.GetBucketConfig(ctx, bucketName, bucket.ConfigReplication)
	if err != nil {
		return err
	}
	others := target.Refs
	if config, err := bucket.ParseReplicationConfig(mine); err == nil && config.Target == target.Name {
		others--
	}
	if others <= 0 {
		return nil
	}
	buckets, err := r.buckets.ListBuckets(ctx)
	if err != nil {
		return err
	}
	for _, b := range buckets {
		if others <= 0 {
			// Every other document that names the target was read.
			return nil
		}
		if b.Name == bucketName {
			continue
		}
		raw, err := r.buckets.GetBucketConfig(ctx, b.Name, bucket.ConfigReplication)
		switch {
		case errors.Is(err, domain.ErrBucketNotFound):
			continue
		case err != nil:
			return err
		}
		config, err := bucket.ParseReplicationConfig(raw)
		if err != nil {
			// A document this release cannot read could name the pair:
			// the stricter reading refuses rather than guesses.
			return fmt.Errorf("%w: the replication configuration of another bucket cannot be read", domain.ErrUnavailable)
		}
		if config.Target != target.Name {
			continue
		}
		others--
		if config.Bucket == destination {
			return fmt.Errorf("%w: target %s is not another deployment that is open to bucket %s", domain.ErrInvalidArgument, target.Name, bucketName)
		}
	}
	return nil
}

// refuseTies refuses, with an error that wraps domain.ErrInvalidArgument, two
// enabled rules of one priority of which one's prefix begins the other's:
// some key would match both, and which of them decides for it would be
// settled by the order they were put in and by nothing the caller said. In
// the first form of S3's schema every rule has the priority 0, so there any
// two enabled rules that overlap are refused. It is the use case's rule, not
// the store's: bucket.ReplicationConfig.Validate, and with it what every
// member applies, is unchanged (Decides still settles a tie, for documents
// stored before).
func refuseTies(rules []bucket.ReplicationRule) error {
	for i, a := range rules {
		for _, b := range rules[i+1:] {
			if !a.Enabled || !b.Enabled || a.Priority != b.Priority {
				continue
			}
			if strings.HasPrefix(a.Prefix, b.Prefix) || strings.HasPrefix(b.Prefix, a.Prefix) {
				return fmt.Errorf("%w: replication: rules %s and %s both match some keys at the priority %d: give them different priorities or prefixes that do not overlap",
					domain.ErrInvalidArgument, a.ID, b.ID, a.Priority)
			}
		}
	}
	return nil
}

// withRuleIDs returns a copy of the configuration in which every rule put
// without an ID has one, as S3 makes one: "rule-<n>", the least n that no
// rule of the configuration has taken. The IDs given are deterministic, so
// that a request made again keeps the same rules.
func withRuleIDs(config bucket.ReplicationConfig) bucket.ReplicationConfig {
	rules := slices.Clone(config.Rules)
	taken := map[string]bool{}
	for _, rule := range rules {
		taken[rule.ID] = true
	}
	next := 1
	for i := range rules {
		if rules[i].ID != "" {
			continue
		}
		id := generatedRuleID + strconv.Itoa(next)
		for taken[id] {
			next++
			id = generatedRuleID + strconv.Itoa(next)
		}
		rules[i].ID, taken[id] = id, true
		next++
	}
	config.Rules = rules
	return config
}

// ReplicaWrite says that a write is the copy of a version or of a delete
// marker of another deployment (ADR-0026): what the source says of it. The
// inbound adapter reads it from the request; this package decides whether
// the caller may write it and whether the bucket takes it.
type ReplicaWrite struct {
	// Source is the ID of the deployment the version comes from.
	Source string
	// Version is the ID the version has there, or the null version.
	Version object.VersionRef
	// Seq is the sequence the version has there, from 1 to
	// object.MaxReplicaSeq.
	Seq uint64
	// ModifiedAt is when the version was made there. It is shown, never
	// decided on.
	ModifiedAt time.Time
	// ETag is the entity tag the version has there, without quotes: the
	// one the destination computes must be the same. Empty for a delete
	// marker, which has none.
	ETag string
}

// replica is what the write says of itself to the store.
func (w ReplicaWrite) replica() *object.Replica {
	return &object.Replica{From: w.Source, Seq: w.Seq, Null: w.Version.Null}
}

// validate reports whether the write names a source, a version and a
// sequence a replica can have, and - but for a marker - an entity tag. The
// error wraps domain.ErrInvalidArgument.
func (w ReplicaWrite) validate(marker bool) error {
	if err := w.replica().Validate(); err != nil {
		return err
	}
	switch {
	case !w.Version.Null && w.Version.ID.IsZero():
		return fmt.Errorf("%w: a replica names the version it is the copy of", domain.ErrInvalidArgument)
	case w.ModifiedAt.IsZero():
		return fmt.Errorf("%w: a replica says when its version was made", domain.ErrInvalidArgument)
	case marker && w.ETag != "":
		return fmt.Errorf("%w: a delete marker has no entity tag", domain.ErrInvalidArgument)
	case !marker && w.ETag == "":
		return fmt.Errorf("%w: a replica says the entity tag of its version", domain.ErrInvalidArgument)
	}
	return nil
}

// verify wraps the verification of a copy's content: the entity tag the
// destination computes must be the one the source says the version has,
// and a copy whose content is another is refused before anything of it is
// durable. The error wraps domain.ErrBadDigest.
func (w ReplicaWrite) verify(next func() (string, error)) func() (string, error) {
	return func() (string, error) {
		if next == nil {
			return "", fmt.Errorf("%w: the content of a replica cannot be verified", domain.ErrBadDigest)
		}
		etag, err := next()
		if err != nil {
			return "", err
		}
		if etag != w.ETag {
			return "", fmt.Errorf("%w: the content of the replica is not the version its source has", domain.ErrBadDigest)
		}
		return etag, nil
	}
}

// errReplicaSSE refuses a copy that asks for an encryption of its own -
// SSE-KMS or SSE-C; SSE-S3 is the default and asks for nothing: no key
// crosses between deployments, and a copy is encrypted under the
// destination's default (ADR-0026).
var errReplicaSSE = fmt.Errorf("%w: a replica is encrypted as this deployment encrypts, and asks for no encryption of its own", domain.ErrInvalidArgument)

// replicaAction is the action a copy needs: s3:ReplicateDelete for a delete
// marker, s3:ReplicateObject for everything else.
func replicaAction(marker bool) iam.Action {
	if marker {
		return iam.ActionReplicateDelete
	}
	return iam.ActionReplicateObject
}

// admitSource decides, before anything is written, whether the bucket takes
// copies from source: it must be the replica of exactly that deployment. The
// store decides again when the copy is committed; this spares the shards of
// a copy that cannot be committed. The error wraps domain.ErrBucketNotFound
// and domain.ErrInvalidBucketState.
func (s *ObjectService) admitSource(ctx context.Context, bucketName, source string) error {
	b, err := s.buckets.GetBucket(ctx, bucketName)
	if err != nil {
		return err
	}
	if b.ReplicaFrom != source {
		// Whether the bucket is the replica of another deployment, or of
		// none, is not told: either way it takes nothing from this one.
		return fmt.Errorf("%w: bucket %s is no replica of that deployment", domain.ErrInvalidBucketState, bucketName)
	}
	return nil
}

// admitReplica decides whether a copy may be written - by whom, of what,
// into which bucket - before anything is written: the caller holds the
// replicate action on the object, the write says what a copy must say, and
// the bucket is the replica of its source.
func (s *ObjectService) admitReplica(ctx context.Context, principal iam.Principal, resource iam.Resource, w ReplicaWrite, marker bool) error {
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: replicaAction(marker), Resource: resource}); err != nil {
		return err
	}
	if err := w.validate(marker); err != nil {
		return err
	}
	return s.admitSource(ctx, resource.Bucket, w.Source)
}

// admitReplicaUpload decides whether a multipart upload may be begun as the
// copy of a version of source - or a question of that source answered: the
// caller holds s3:ReplicateObject on the resource, source is a deployment
// ID, and the bucket is its replica.
func (s *ObjectService) admitReplicaUpload(ctx context.Context, principal iam.Principal, resource iam.Resource, source string) error {
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionReplicateObject, Resource: resource}); err != nil {
		return err
	}
	if err := object.ValidateReplicaSource(source); err != nil {
		return err
	}
	return s.admitSource(ctx, resource.Bucket, source)
}

// authorizeUpload is the gate of what is done to a multipart upload in
// progress - a part sent, the upload aborted, its parts listed - asked
// before the upload is read, which would tell whoever asks whether the
// bucket is there. An upload that is a copy (object.Upload.ReplicaFrom) is
// its source's: each of these needs s3:ReplicateObject, and the ordinary
// action (s3:PutObject, s3:AbortMultipartUpload,
// s3:ListMultipartUploadParts) does not do; any other upload needs the
// ordinary action. Which of the two the caller is asked for first is chosen
// by whether it holds the replicate action - Authorizer.Allowed, a question
// that is not recorded; the gate is Authorize either way, and
// authorizeUploadOf asks for the other action too when the upload turns
// out to need it. replicates says which was asked.
func (s *ObjectService) authorizeUpload(ctx context.Context, principal iam.Principal, resource iam.Resource, ordinary iam.Action) (replicates bool, err error) {
	replicates, err = s.authz.Allowed(ctx, iam.Request{Principal: principal, Action: iam.ActionReplicateObject, Resource: resource})
	if err != nil {
		return false, err
	}
	action := ordinary
	if replicates {
		action = iam.ActionReplicateObject
	}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: action, Resource: resource}); err != nil {
		return false, err
	}
	return replicates, nil
}

// authorizeUploadOf completes the gate of authorizeUpload once the upload is
// known: a copy is touched only by who replicates, any other upload only by
// who holds the ordinary action. A caller that holds both is asked for both.
func (s *ObjectService) authorizeUploadOf(
	ctx context.Context, principal iam.Principal, resource iam.Resource, upload object.Upload, ordinary iam.Action, replicates bool,
) error {
	copied := upload.ReplicaFrom != ""
	switch {
	case copied == replicates:
		return nil
	case copied:
		return s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionReplicateObject, Resource: resource})
	default:
		return s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: ordinary, Resource: resource})
	}
}

// admitPart decides, once the upload is known, whether this part may join
// it. A part of a copy is sent by its source and by nothing else: its payload
// must be covered by the request's signature (refused 403, and audited, as
// the adapter refuses an unsigned copy), and a copy of another object - an
// UploadPartCopy - is never one. A part of an upload that is no copy is not
// taken in a bucket that has become a replica since the upload was begun:
// it could never be completed there.
func (s *ObjectService) admitPart(ctx context.Context, principal iam.Principal, resource iam.Resource, upload object.Upload, in UploadPartInput) error {
	if upload.ReplicaFrom != "" {
		if in.copied {
			return fmt.Errorf("%w: the part of a replica upload is sent by its source, never copied", domain.ErrInvalidArgument)
		}
		if !in.PayloadSigned {
			if s.auditor != nil {
				s.auditor.Denied(ctx, iam.Request{Principal: principal, Action: iam.ActionReplicateObject, Resource: resource}, "ReplicaPayloadNotSigned")
			}
			return fmt.Errorf("%w: the payload of a part of a replica upload is not signed", domain.ErrAccessDenied)
		}
		return nil
	}
	b, err := s.buckets.GetBucket(ctx, resource.Bucket)
	if err != nil {
		return err
	}
	if b.ReplicaFrom != "" {
		return fmt.Errorf("%w: bucket %s is a replica and takes parts of its source's uploads only", domain.ErrInvalidBucketState, resource.Bucket)
	}
	return nil
}

// auditReplica writes into the audit trail that a copy was committed: a
// version or a marker with an ID, a time and a sequence of the caller's
// choosing is something only a replicate action allows, and each is said,
// as a retention that was lifted is. Nothing of the object but its name is
// in the record.
func (s *ObjectService) auditReplica(ctx context.Context, principal iam.Principal, resource iam.Resource, operation string, w ReplicaWrite, marker bool) {
	if s.auditor == nil {
		return
	}
	s.auditor.Succeeded(ctx, s.auditor.Record(principal, replicaAction(marker), resource, audit.Succeeded, map[string]string{
		"operation": operation, "source": w.Source, "version": w.Version.String(), "seq": strconv.FormatUint(w.Seq, 10),
	}))
}

// The questions a source asks before it sends (ADR-0026, "What the source
// asks before it sends, and what it is told"): whether the bucket is its
// replica and keeps versions, whether a version is there and with which
// entity tag, which replica uploads of a key it left behind, and the abort
// of one of them. Each is judged as s3:ReplicateObject - no read, listing or
// abort action is asked, and none is enough - and refused, as a copy is,
// unless the bucket is the replica of exactly that source. Each answers no
// more than the source needs: never a byte of an object, nothing of a
// version but its entity tag, never an upload that is not that source's.

// ReplicaBucket answers the bucket probe of a source: the versioning of a
// bucket that is the replica of that source, and whether it has Object Lock. That it answers at all says
// the bucket is that source's replica; nothing of what the bucket holds is
// told. It needs s3:ReplicateObject on the bucket. The error wraps
// domain.ErrAccessDenied, domain.ErrInvalidArgument for a source that is no
// deployment ID, domain.ErrBucketNotFound, and domain.ErrInvalidBucketState
// for a bucket that is no replica of that source; any other conflict is
// domain.ErrUnavailable, never domain.ErrConflict (questionFailed).
func (s *ObjectService) ReplicaBucket(ctx context.Context, principal iam.Principal, bucketName, source string) (ReplicaBucketState, error) {
	if err := s.admitReplicaUpload(ctx, principal, iam.Resource{Bucket: bucketName}, source); err != nil {
		return ReplicaBucketState{}, fmt.Errorf("object.ReplicaBucket: %w", questionFailed(err))
	}
	b, err := s.buckets.GetBucket(ctx, bucketName)
	if err != nil {
		return ReplicaBucketState{}, fmt.Errorf("object.ReplicaBucket: %w", questionFailed(err))
	}
	return ReplicaBucketState{Versioning: b.Versioning, ObjectLock: b.ObjectLock}, nil
}

// ReplicaBucketState is what the bucket probe of a source is told of a
// replica bucket: whether it keeps versions, and whether it has Object Lock
// - so that a source holds a version that carries a lock rather than send
// it where no lock is kept (PLAN 8.1.4 re-review).
type ReplicaBucketState struct {
	Versioning bucket.Versioning
	ObjectLock bool
}

// questionFailed keeps the one conflict a question is refused with -
// domain.ErrInvalidBucketState, a bucket that is no replica of the source -
// apart from every other: the source reads a 409 to a HEAD, which has no
// body to name its code, as that one. Any other domain.ErrConflict is
// domain.ErrUnavailable, which the source asks again, and keeps nothing of
// the conflict but its text.
func questionFailed(err error) error {
	if errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrInvalidBucketState) {
		return fmt.Errorf("%w: a conflict while the question was answered (%v)", domain.ErrUnavailable, err.Error())
	}
	return err
}

// ReplicaVersionInput names the version a source asks about.
type ReplicaVersionInput struct {
	Bucket string
	Key    string
	// Version is the ID the version has at the source - which its copy
	// has here - or the null version.
	Version object.VersionRef
	// Source is the deployment that asks.
	Source string
}

// ReplicaVersion answers whether the copy of a version is there: its entity
// tag when it is, and nothing else of it. It needs s3:ReplicateObject on the
// object; s3:GetObjectVersion, which would let the version be read, is
// neither asked nor enough. The error wraps domain.ErrAccessDenied,
// domain.ErrInvalidArgument, domain.ErrBucketNotFound,
// domain.ErrInvalidBucketState for a bucket that is no replica of that
// source, domain.ErrVersionNotFound or domain.ErrObjectNotFound for a
// version that is not there, and domain.ErrMethodNotAllowed for a delete
// marker, which is there and has no entity tag. Any other conflict is
// domain.ErrUnavailable, never domain.ErrConflict (questionFailed).
func (s *ObjectService) ReplicaVersion(ctx context.Context, principal iam.Principal, in ReplicaVersionInput) (string, error) {
	etag, err := s.replicaVersion(ctx, principal, in)
	if err != nil {
		return "", fmt.Errorf("object.ReplicaVersion: %w", questionFailed(err))
	}
	return etag, nil
}

// replicaVersion is ReplicaVersion, its errors not yet kept apart.
func (s *ObjectService) replicaVersion(ctx context.Context, principal iam.Principal, in ReplicaVersionInput) (string, error) {
	if err := s.admitReplicaUpload(ctx, principal, iam.Resource{Bucket: in.Bucket, Key: in.Key}, in.Source); err != nil {
		return "", err
	}
	if err := object.ValidateKey(in.Key); err != nil {
		return "", err
	}
	if !in.Version.Null && in.Version.ID.IsZero() {
		return "", fmt.Errorf("%w: no version is named", domain.ErrInvalidArgument)
	}
	version, err := s.index.GetVersion(ctx, in.Bucket, in.Key, in.Version)
	if err != nil {
		return "", err
	}
	if version.DeleteMarker {
		return "", fmt.Errorf("%w: the version is a delete marker", domain.ErrMethodNotAllowed)
	}
	return version.ETag, nil
}

// ReplicaUploadsInput asks for the replica uploads of one key that a source
// began and has neither completed nor aborted.
type ReplicaUploadsInput struct {
	Bucket string
	Key    string
	Source string
	// KeyMarker and UploadIDMarker continue a listing as in
	// ListUploadsInput: after the upload with that ID of that key, or,
	// without an ID, after every upload of that key. A marker before the
	// key lists from its first upload; one after it lists nothing.
	KeyMarker      string
	UploadIDMarker object.ID
	// Max is the greatest number of uploads to return; at most
	// object.MaxListKeys.
	Max int
}

// ReplicaUploadsOutput is one page of the replica uploads of a key.
type ReplicaUploadsOutput struct {
	// Uploads are the uploads of the key whose ReplicaFrom is the source,
	// in order of ID.
	Uploads []object.Upload
	// Truncated reports that there are more; the key as the key marker
	// and NextUploadIDMarker continue the listing.
	Truncated          bool
	NextUploadIDMarker object.ID
}

// ListReplicaUploads lists the uploads in progress of one key that the
// source began as copies, and no other: neither the uploads of other keys
// nor those that are no copy or are another source's. It needs
// s3:ReplicateObject on the object; s3:ListBucketMultipartUploads is
// neither asked nor enough. The error wraps domain.ErrAccessDenied,
// domain.ErrInvalidArgument, domain.ErrBucketNotFound and
// domain.ErrInvalidBucketState for a bucket that is no replica of that
// source.
func (s *ObjectService) ListReplicaUploads(ctx context.Context, principal iam.Principal, in ReplicaUploadsInput) (ReplicaUploadsOutput, error) {
	if err := s.admitReplicaUpload(ctx, principal, iam.Resource{Bucket: in.Bucket, Key: in.Key}, in.Source); err != nil {
		return ReplicaUploadsOutput{}, fmt.Errorf("object.ListReplicaUploads: %w", err)
	}
	if err := object.ValidateKey(in.Key); err != nil {
		return ReplicaUploadsOutput{}, fmt.Errorf("object.ListReplicaUploads: %w", err)
	}
	limit := min(max(in.Max, 0), object.MaxListKeys)
	start := ports.UploadCursor{Key: in.Key}
	switch {
	case limit == 0, in.KeyMarker > in.Key, in.KeyMarker == in.Key && in.UploadIDMarker.IsZero():
		// Nothing is asked for, or the listing is past every upload of
		// the key.
		return ReplicaUploadsOutput{}, nil
	case in.KeyMarker == in.Key:
		start.After = in.UploadIDMarker
	}
	var out ReplicaUploadsOutput
	err := s.uploads.ScanUploads(ctx, in.Bucket, start, func(u object.Upload) ports.ScanControl {
		switch {
		case u.Key != in.Key:
			// Keys are ordered: past the key nothing is of it.
			return ports.ScanControl{Stop: true}
		case u.ReplicaFrom != in.Source:
			return ports.ScanControl{}
		case len(out.Uploads) == limit:
			out.Truncated = true
			return ports.ScanControl{Stop: true}
		}
		out.Uploads = append(out.Uploads, u)
		return ports.ScanControl{}
	})
	if err != nil {
		return ReplicaUploadsOutput{}, fmt.Errorf("object.ListReplicaUploads: %w", err)
	}
	if out.Truncated {
		out.NextUploadIDMarker = out.Uploads[len(out.Uploads)-1].ID
	}
	return out, nil
}

// AbortReplicaUpload gives up an upload that the source began as a copy,
// and no other: an upload of the key that is no copy, or is another
// source's, is to this source not there (domain.ErrUploadNotFound), so the
// answer tells it nothing of uploads that are not its own. It needs
// s3:ReplicateObject on the object. The error wraps domain.ErrAccessDenied,
// domain.ErrInvalidArgument, domain.ErrBucketNotFound,
// domain.ErrInvalidBucketState for a bucket that is no replica of that
// source, and domain.ErrUploadNotFound.
func (s *ObjectService) AbortReplicaUpload(ctx context.Context, principal iam.Principal, in UploadRef, source string) error {
	if err := s.admitReplicaUpload(ctx, principal, iam.Resource{Bucket: in.Bucket, Key: in.Key}, source); err != nil {
		return fmt.Errorf("object.AbortReplicaUpload: %w", err)
	}
	upload, err := s.uploads.GetUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return fmt.Errorf("object.AbortReplicaUpload: %w", err)
	}
	if upload.ReplicaFrom != source {
		return fmt.Errorf("object.AbortReplicaUpload: %w: no upload of that source", domain.ErrUploadNotFound)
	}
	parts, err := s.uploads.AbortUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return fmt.Errorf("object.AbortReplicaUpload: %w", err)
	}
	s.discard(ctx, nil, parts)
	return nil
}
