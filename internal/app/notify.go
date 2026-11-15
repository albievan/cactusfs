package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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

// NotificationService holds the use cases of the notification rules of a
// bucket (ADR-0025): reading them, and setting and removing them, each
// through the single authorizer - s3:GetBucketNotification to read,
// s3:PutBucketNotification to change, as S3 has no action of its own for
// removing them - and each change audited.
//
// A rule names a target, never an address: whoever may configure a bucket
// chooses among the targets the administrator opened to it (ADR-0025,
// "Targets are the administrator's"). That is decided here before anything
// is proposed, and again by the metadata store when the change is
// committed, which is what holds.
type NotificationService struct {
	buckets ports.BucketStore
	targets ports.TargetStore
	authz   ports.Authorizer
	auditor *Auditor
}

// NotificationServiceConfig lists what a NotificationService depends on.
type NotificationServiceConfig struct {
	Buckets ports.BucketStore
	Targets ports.TargetStore
	Authz   ports.Authorizer
	// Auditor records every change; without one every change is refused.
	Auditor *Auditor
}

// NewNotificationService returns a NotificationService.
func NewNotificationService(cfg NotificationServiceConfig) *NotificationService {
	return &NotificationService{buckets: cfg.Buckets, targets: cfg.Targets, authz: cfg.Authz, auditor: cfg.Auditor}
}

// Get returns the notification rules of a bucket, in the order they were
// put; a bucket without rules has none (bucket.NotificationConfig.Empty).
// The error wraps domain.ErrAccessDenied, domain.ErrBucketNotFound,
// domain.ErrUnavailable for rules of a later release, and
// domain.ErrIntegrity for rules that cannot be read.
func (s *NotificationService) Get(ctx context.Context, principal iam.Principal, bucketName string) (bucket.NotificationConfig, error) {
	if err := s.authz.Authorize(ctx, bucketRequest(principal, iam.ActionGetBucketNotification, bucketName)); err != nil {
		return bucket.NotificationConfig{}, fmt.Errorf("notify.Get: %w", err)
	}
	raw, err := s.buckets.GetBucketConfig(ctx, bucketName, bucket.ConfigNotification)
	if err != nil {
		return bucket.NotificationConfig{}, fmt.Errorf("notify.Get: %w", err)
	}
	config, err := bucket.ParseNotificationConfig(raw)
	switch {
	case errors.Is(err, domain.ErrUnsupportedFormat):
		return bucket.NotificationConfig{}, fmt.Errorf("notify.Get: %w: the notification rules of bucket %s are of a later release: %s",
			domain.ErrUnavailable, bucketName, err.Error())
	case err != nil:
		return bucket.NotificationConfig{}, fmt.Errorf("notify.Get: %w: stored notification rules of bucket %s: %s",
			domain.ErrIntegrity, bucketName, err.Error())
	}
	return config, nil
}

// Set replaces the notification rules of a bucket and returns them as they
// are kept. No rules removes them, as an empty NotificationConfiguration
// does in S3 (Delete).
//
// A rule without an ID is given one, "rule-<n>", that no other rule of the
// document has; a rule not said to be a queue or a topic is a queue. Then
// the rules are judged (bucket.NotificationConfig.Validate), and every
// target they name must exist, be a webhook or a NATS server and admit the
// bucket. Which of the three a target fails is not said, so that a caller
// who may configure a bucket learns nothing of the targets that are not
// open to it.
//
// The error wraps domain.ErrAccessDenied, domain.ErrBucketNotFound,
// domain.ErrInvalidArgument for rules that cannot be kept (nothing is
// proposed), and domain.ErrUnavailable until every member of the metadata
// group knows notification rules.
func (s *NotificationService) Set(ctx context.Context, principal iam.Principal, bucketName string, config bucket.NotificationConfig) (bucket.NotificationConfig, error) {
	if err := s.authz.Authorize(ctx, bucketRequest(principal, iam.ActionPutBucketNotification, bucketName)); err != nil {
		return bucket.NotificationConfig{}, fmt.Errorf("notify.Set: %w", err)
	}
	if config.Empty() {
		if err := s.remove(ctx, principal, bucketName); err != nil {
			return bucket.NotificationConfig{}, fmt.Errorf("notify.Set: %w", err)
		}
		return bucket.NotificationConfig{}, nil
	}
	kept, err := s.set(ctx, principal, bucketName, config)
	if err != nil {
		return bucket.NotificationConfig{}, fmt.Errorf("notify.Set: %w", err)
	}
	return kept, nil
}

// Delete removes the notification rules of a bucket; a bucket without rules
// is left as it is. It is asked as setting them is
// (s3:PutBucketNotification), and audited. Rows of the outbox appended
// before are delivered all the same. The error wraps domain.ErrAccessDenied,
// domain.ErrBucketNotFound, and domain.ErrUnavailable until every member of
// the metadata group knows notification rules.
func (s *NotificationService) Delete(ctx context.Context, principal iam.Principal, bucketName string) error {
	if err := s.authz.Authorize(ctx, bucketRequest(principal, iam.ActionPutBucketNotification, bucketName)); err != nil {
		return fmt.Errorf("notify.Delete: %w", err)
	}
	if err := s.remove(ctx, principal, bucketName); err != nil {
		return fmt.Errorf("notify.Delete: %w", err)
	}
	return nil
}

// set judges the rules, proposes their document and audits the change.
func (s *NotificationService) set(ctx context.Context, principal iam.Principal, bucketName string, config bucket.NotificationConfig) (bucket.NotificationConfig, error) {
	if s.auditor == nil {
		return bucket.NotificationConfig{}, fmt.Errorf("%w: no audit trail to record the change in", domain.ErrUnavailable)
	}
	document, err := completed(config).Document()
	if err != nil {
		return bucket.NotificationConfig{}, err
	}
	// The rules are returned as Get reads them: from the document, with
	// the kinds of change of each in the one order a document has them in.
	kept, err := bucket.ParseNotificationConfig(document)
	if err != nil {
		return bucket.NotificationConfig{}, err
	}
	// A bucket that is not there is said to be so before its rules are
	// judged against the targets.
	if _, err := s.buckets.GetBucket(ctx, bucketName); err != nil {
		return bucket.NotificationConfig{}, err
	}
	names := kept.Targets()
	for _, name := range names {
		if err := s.open(ctx, name, bucketName); err != nil {
			return bucket.NotificationConfig{}, err
		}
	}
	if err := s.buckets.SetBucketConfig(ctx, bucketName, bucket.ConfigNotification, document); err != nil {
		return bucket.NotificationConfig{}, err
	}
	details := map[string]string{"rules": strconv.Itoa(len(kept.Rules)), "targets": strings.Join(names, ",")}
	s.auditor.Succeeded(ctx, s.auditor.Record(principal, iam.ActionPutBucketNotification, iam.Resource{Bucket: bucketName}, audit.Succeeded, details))
	return kept, nil
}

// open reports, with an error that wraps domain.ErrInvalidArgument, a
// target that the rules of the bucket may not name: one that is not there,
// is of kind s3, or does not admit the bucket - in one sentence for all
// three.
func (s *NotificationService) open(ctx context.Context, name, bucketName string) error {
	target, err := s.targets.GetTarget(ctx, name)
	switch {
	case errors.Is(err, domain.ErrEntityNotFound):
	case err != nil:
		return err
	case target.Kind.Serves(delivery.ConsumerNotification) && target.Admits(bucketName):
		return nil
	}
	return fmt.Errorf("%w: target %s is not a webhook or NATS target that is open to bucket %s", domain.ErrInvalidArgument, name, bucketName)
}

// remove proposes no rules for the bucket and audits the change.
func (s *NotificationService) remove(ctx context.Context, principal iam.Principal, bucketName string) error {
	if s.auditor == nil {
		return fmt.Errorf("%w: no audit trail to record the change in", domain.ErrUnavailable)
	}
	if err := s.buckets.SetBucketConfig(ctx, bucketName, bucket.ConfigNotification, nil); err != nil {
		return err
	}
	details := map[string]string{"notification": "removed"}
	s.auditor.Succeeded(ctx, s.auditor.Record(principal, iam.ActionPutBucketNotification, iam.Resource{Bucket: bucketName}, audit.Succeeded, details))
	return nil
}

// generatedRuleID is the beginning of the ID a rule is given that was put
// without one.
const generatedRuleID = "rule-"

// completed returns a copy of the rules with what the caller may leave out
// filled in: an ID, as S3 makes one, and the queue a rule is shown as. The
// IDs given are deterministic, so that a request made again keeps the same
// rules.
func completed(config bucket.NotificationConfig) bucket.NotificationConfig {
	rules := make([]bucket.NotificationRule, len(config.Rules))
	taken := map[string]bool{}
	for i, rule := range config.Rules {
		rule.On = slices.Clone(rule.On)
		rules[i] = rule
		taken[rule.ID] = true
	}
	next := 1
	for i := range rules {
		if rules[i].As == "" {
			rules[i].As = bucket.NotifyAsQueue
		}
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
	return bucket.NotificationConfig{Rules: rules}
}

// The constants of the record (ADR-0025, "The record").
const (
	// NotificationEventVersion is the version of S3's event notification
	// the record follows.
	NotificationEventVersion = "2.1"
	// NotificationEventSource is where the record says it comes from: not
	// aws:s3, so that a receiver can tell.
	NotificationEventSource = "cactusfs:s3"
)

// notificationRecords is the body of a notification: S3's event
// notification, of one record. The order of the fields is the order of the
// body.
type notificationRecords struct {
	Records []notificationRecord `json:"Records"`
}

type notificationRecord struct {
	EventVersion string            `json:"eventVersion"`
	EventSource  string            `json:"eventSource"`
	EventTime    string            `json:"eventTime"`
	EventName    string            `json:"eventName"`
	UserIdentity *notificationUser `json:"userIdentity,omitempty"`
	S3           notificationS3    `json:"s3"`
}

type notificationUser struct {
	PrincipalID string `json:"principalId"`
}

type notificationS3 struct {
	Bucket notificationBucket `json:"bucket"`
	Object notificationObject `json:"object"`
}

type notificationBucket struct {
	Name string `json:"name"`
}

type notificationObject struct {
	Key       string `json:"key"`
	Size      *int64 `json:"size,omitempty"`
	ETag      string `json:"eTag,omitempty"`
	VersionID string `json:"versionId,omitempty"`
	Sequencer string `json:"sequencer"`
}

// NotificationEventName returns the eventName of the record of a kind of
// change, as S3 names it without the "s3:" a configuration writes, and
// false for a kind of change that is not told.
func NotificationEventName(event delivery.Event) (string, bool) {
	switch event {
	case delivery.EventPut:
		return "ObjectCreated:Put", true
	case delivery.EventComplete:
		return "ObjectCreated:CompleteMultipartUpload", true
	case delivery.EventMarker:
		return "ObjectRemoved:DeleteMarkerCreated", true
	case delivery.EventDelete:
		return "ObjectRemoved:Delete", true
	}
	return "", false
}

// NotificationKey returns a key as S3 writes it in a notification: encoded
// as a form value is - a space is "+", every byte that is not a letter, a
// digit or one of "-_.~" is "%XX" - but for "/", which is left as it is.
// url.QueryUnescape gives the key back, byte for byte.
func NotificationKey(key string) string {
	return strings.ReplaceAll(url.QueryEscape(key), "%2F", "/")
}

// NotificationRecord returns the body of the notification of an item of the
// outbox (ADR-0025, "The record"): S3's event notification, of one record,
// as compact JSON.
//
// What it tells is the item's and nothing else: the time of the change, its
// kind, who made it (left out when that was not recorded), the bucket, the
// key URL-encoded, the size and entity tag of the version (left out for a
// delete marker, and for a removal that carries no entity tag: a delete
// marker removed for good - and a version whose entity tag was longer than
// delivery.MaxItemETag, which its row does not carry, or that holds a quote
// once its own quotes are trimmed, so that such a removal is told without
// its size too), the version (left out for the null version, of a
// bucket that keeps none) and the sequencer, the place of the row. Never the
// address of a client - there is no requestParameters - and never anything
// of the object's contents, metadata or tags, which an item does not hold.
//
// The error wraps domain.ErrInvalidArgument for a kind of change that is not
// told; it names no key.
func NotificationRecord(item delivery.Item) ([]byte, error) {
	name, ok := NotificationEventName(item.Event)
	if !ok {
		return nil, fmt.Errorf("notify.Record: %w: a kind of change that is not told", domain.ErrInvalidArgument)
	}
	record := notificationRecord{
		EventVersion: NotificationEventVersion,
		EventSource:  NotificationEventSource,
		EventTime:    item.At.UTC().Format(time.RFC3339Nano),
		EventName:    name,
		S3: notificationS3{
			Bucket: notificationBucket{Name: item.Bucket},
			Object: notificationObject{Key: NotificationKey(item.Key), Sequencer: item.Seq.String()},
		},
	}
	if item.By != "" {
		record.UserIdentity = &notificationUser{PrincipalID: item.By}
	}
	// The entity tag is told without its quotes; one that holds a quote
	// even so is no entity tag, and is not told.
	etag := strings.Trim(item.ETag, `"`)
	if strings.Contains(etag, `"`) {
		etag = ""
	}
	if item.Event != delivery.EventMarker && (item.Event != delivery.EventDelete || etag != "") {
		size := item.Size
		record.S3.Object.Size, record.S3.Object.ETag = &size, etag
	}
	if !item.Version.Null && !item.Version.ID.IsZero() {
		record.S3.Object.VersionID = item.Version.ID.String()
	}
	body, err := json.Marshal(notificationRecords{Records: []notificationRecord{record}})
	if err != nil {
		return nil, fmt.Errorf("notify.Record: %w", err)
	}
	return body, nil
}

// PublisherSource gives the ports.EventPublisher that sends to a target: a
// webhook or a NATS server. It is made by the composition root, which
// unseals the target's credentials through the KMS when it first has
// something to send (ADR-0025, "Delivery"). An error is a failure of the
// class it carries (delivery.ClassOf): one that carries none - a credential
// that could not be opened - is delivery.ClassInternal, and is tried again.
type PublisherSource interface {
	Publisher(ctx context.Context, target delivery.Target) (ports.EventPublisher, error)
}

// PublisherSourceFunc is a function that is a PublisherSource.
type PublisherSourceFunc func(ctx context.Context, target delivery.Target) (ports.EventPublisher, error)

// Publisher implements PublisherSource.
func (f PublisherSourceFunc) Publisher(ctx context.Context, target delivery.Target) (ports.EventPublisher, error) {
	return f(ctx, target)
}

// NotifierConfig lists what a Notifier depends on.
type NotifierConfig struct {
	// Publishers gives the publisher of each target.
	Publishers PublisherSource
	// Logger, when not nil, is told at Debug of the items that are not
	// sent, by target and reason; never by key. The DeliveryWorker logs the
	// target and the class of every failure already.
	Logger *slog.Logger
}

// Notifier is the Deliverer of the consumers of kind n (ADR-0025): it
// builds the notification of an item (NotificationRecord) and publishes it
// through the target's publisher as the message
// ports.MessageNotificationObject, with the ID of the item - the same every
// time it is sent, so that a receiver deduplicates by it.
//
// An item is sent only to a webhook or a NATS server that is a consumer of
// it as a notification target and that admits its bucket now. A row that
// was appended while the bucket's rules named the target, and that waited
// while its rules were removed and the target was closed to the bucket, is
// passed with delivery.ClassRejected and is not sent: the list of buckets a
// target admits is what keeps tenants apart. An item that is not for the
// target as a notification is a fault of whoever handed it over: it is held,
// with delivery.ClassInternal, as the DeliveryWorker holds an item of a kind
// nothing delivers.
type Notifier struct {
	publishers PublisherSource
	logger     *slog.Logger
}

// NewNotifier returns a Notifier.
func NewNotifier(cfg NotifierConfig) *Notifier {
	n := &Notifier{publishers: cfg.Publishers, logger: cfg.Logger}
	if n.logger == nil {
		n.logger = slog.New(slog.DiscardHandler)
	}
	return n
}

// errNoPublishers is the cause of a failure of a Notifier that was given
// nothing to publish through.
var errNoPublishers = errors.New("no publishers of notifications on this node")

// Deliver implements Deliverer: it publishes the notification of item to
// target, and returns what the publisher returns. It never reports a status
// of replication.
func (n *Notifier) Deliver(ctx context.Context, target delivery.Target, item delivery.Item) (object.ReplicationStatus, error) {
	if class, reason := notSent(target, item); reason != "" {
		n.logger.DebugContext(ctx, "a notification is not sent",
			slog.String("target", target.Name), slog.String("class", string(class)), slog.String("reason", reason))
		return "", delivery.Failed(class, fmt.Errorf("notify.Deliver: %w: %s", domain.ErrInvalidArgument, reason))
	}
	body, err := NotificationRecord(item)
	if err != nil {
		return "", delivery.Failed(delivery.ClassRejected, fmt.Errorf("notify.Deliver: %w", err))
	}
	if n.publishers == nil {
		return "", delivery.Failed(delivery.ClassInternal, errNoPublishers)
	}
	publisher, err := n.publishers.Publisher(ctx, target)
	switch {
	case err != nil:
		return "", classed(err)
	case publisher == nil:
		return "", delivery.Failed(delivery.ClassInternal, errNoPublishers)
	}
	event := ports.Event{
		ID: item.ID(), Type: ports.MessageNotificationObject, Sequence: item.Seq.Index, Time: item.At,
		Data: json.RawMessage(body),
	}
	if err := publisher.Publish(ctx, event); err != nil {
		return "", classed(err)
	}
	return "", nil
}

// classed returns a failure of a publisher as a delivery.Error of its class
// - delivery.ClassInternal when it carries none, delivery.ClassTimeout when
// that is the end of the attempt's deadline - whose text is the class and
// nothing else: the words of the cause may name an endpoint, and are for the
// log of the node that leads only.
func classed(err error) error {
	class := delivery.ClassOf(err)
	if class == delivery.ClassInternal && errors.Is(err, context.DeadlineExceeded) {
		class = delivery.ClassTimeout
	}
	return delivery.Failed(class, fmt.Errorf("notify.Deliver: %w", err))
}

// notSent returns why an item is not to be sent to a target, and the class
// of that failure, or no reason when it is to be sent. An item that is not
// the target's as a notification is held (delivery.ClassInternal); one the
// target may not be told of is passed (delivery.ClassRejected). The reason
// names neither the key nor the endpoint.
func notSent(target delivery.Target, item delivery.Item) (delivery.Class, string) {
	kind, ok := item.For(target.Name)
	switch {
	case !ok || kind != delivery.ConsumerNotification:
		return delivery.ClassInternal, "the item is not for the target as a notification"
	case !target.Kind.Serves(delivery.ConsumerNotification):
		return delivery.ClassRejected, "the target is not a webhook or a NATS server"
	case !item.Event.Notified():
		return delivery.ClassRejected, "a kind of change that is not told"
	case !target.Admits(item.Bucket):
		return delivery.ClassRejected, "the target no longer admits the bucket"
	}
	return delivery.ClassNone, ""
}
