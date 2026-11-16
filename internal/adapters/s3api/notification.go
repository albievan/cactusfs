package s3api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// Notification rules: the ?notification sub-resource of a bucket
// (PutBucketNotificationConfiguration and GetBucketNotificationConfiguration
// of the S3 API reference; ADR-0025). A rule names a target of the
// deployment by an ARN, never an address; which targets a bucket may name,
// and who may set its rules, is the use case's to decide.

// Notifications is the use case of the notification rules of buckets.
// *app.NotificationService implements it.
type Notifications interface {
	Get(ctx context.Context, principal iam.Principal, bucketName string) (bucket.NotificationConfig, error)
	Set(ctx context.Context, principal iam.Principal, bucketName string, config bucket.NotificationConfig) (bucket.NotificationConfig, error)
}

// notifyARNPrefix is what the ARN of a target begins with; the name of the
// target follows it.
const notifyARNPrefix = "arn:cactusfs:notify:::"

// The two kinds of rule S3 has that name a target. They mean the same here;
// which one a rule was put as is kept, and shown again.
const (
	queueConfiguration = "QueueConfiguration"
	topicConfiguration = "TopicConfiguration"
)

// notificationConfiguration is S3's NotificationConfiguration. Its rules are
// read as one list, whatever their kind, so that the order they were put in
// is the order they are kept and shown in.
type notificationConfiguration struct {
	XMLName xml.Name              `xml:"NotificationConfiguration"`
	XMLNS   string                `xml:"xmlns,attr,omitempty"`
	Rules   []notificationRuleXML `xml:",any"`
}

// notificationRuleXML is a QueueConfiguration or a TopicConfiguration, as
// XMLName says. The shape lets a queue have a Queue and a topic a Topic,
// and neither the other's.
type notificationRuleXML struct {
	XMLName xml.Name
	ID      string                 `xml:"Id,omitempty"`
	Queue   *string                `xml:"Queue,omitempty"`
	Topic   *string                `xml:"Topic,omitempty"`
	Events  []string               `xml:"Event"`
	Filter  *notificationFilterXML `xml:"Filter,omitempty"`
}

type notificationFilterXML struct {
	Rules []filterRuleXML `xml:"S3Key>FilterRule"`
}

// filterRuleXML is one FilterRule. Name and Value are pointers so that one
// that is left out is told from one that is empty.
type filterRuleXML struct {
	Name  *string `xml:"Name"`
	Value *string `xml:"Value"`
}

// notificationShape is where each element of a notification configuration
// belongs.
func notificationShape() xmlShape {
	return xmlShape{
		"":                          {"NotificationConfiguration"},
		"NotificationConfiguration": {queueConfiguration + "*", topicConfiguration + "*"},
		queueConfiguration:          {"Id", "Queue", "Event*", "Filter"},
		topicConfiguration:          {"Id", "Topic", "Event*", "Filter"},
		"Filter":                    {"S3Key"},
		"S3Key":                     {"FilterRule*"},
		"FilterRule":                {"Name", "Value"},
	}
}

// unsupportedNotification are the kinds of rule S3 has whose destinations
// are AWS's own: a Lambda function and EventBridge. A configuration that has
// one is refused as not implemented, never read without it.
func unsupportedNotification() []string {
	return []string{"CloudFunctionConfiguration", "EventBridgeConfiguration"}
}

// notifiedEvents are the event names of S3 that are told, and the kinds of
// change each stands for. A copy is committed as a put and told as one,
// which is why s3:ObjectCreated:Copy is not here (ADR-0025).
func notifiedEvents() map[string][]delivery.Event {
	return map[string][]delivery.Event{
		"s3:ObjectCreated:*":                       {delivery.EventPut, delivery.EventComplete},
		"s3:ObjectCreated:Put":                     {delivery.EventPut},
		"s3:ObjectCreated:CompleteMultipartUpload": {delivery.EventComplete},
		"s3:ObjectRemoved:*":                       {delivery.EventMarker, delivery.EventDelete},
		"s3:ObjectRemoved:Delete":                  {delivery.EventDelete},
		"s3:ObjectRemoved:DeleteMarkerCreated":     {delivery.EventMarker},
	}
}

// untoldEvents are the event names of the S3 API reference that are not
// told. Asking for one is not implemented; a name that is in neither list
// is no event of S3's and is an invalid argument.
func untoldEvents() []string {
	return []string{
		"s3:ObjectCreated:Post", "s3:ObjectCreated:Copy", "s3:ReducedRedundancyLostObject",
		"s3:ObjectRestore:*", "s3:ObjectRestore:Post", "s3:ObjectRestore:Completed", "s3:ObjectRestore:Delete",
		"s3:Replication:*", "s3:Replication:OperationFailedReplication", "s3:Replication:OperationNotTracked",
		"s3:Replication:OperationMissedThreshold", "s3:Replication:OperationReplicatedAfterThreshold",
		"s3:LifecycleTransition", "s3:IntelligentTiering", "s3:ObjectAcl:Put",
		"s3:LifecycleExpiration:*", "s3:LifecycleExpiration:Delete", "s3:LifecycleExpiration:DeleteMarkerCreated",
		"s3:ObjectTagging:*", "s3:ObjectTagging:Put", "s3:ObjectTagging:Delete",
		"s3:ObjectAnnotation:*", "s3:ObjectAnnotation:Put", "s3:ObjectAnnotation:Delete", "s3:ObjectRetention:Put",
	}
}

func invalidNotification(format string, args ...any) error {
	return fmt.Errorf("%w: notification configuration: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// notificationRules reads a NotificationConfiguration as the rules it says.
// The ID, the queue or topic, and whether the target may be named are the
// use case's to judge; what is read here is what the document says and
// nothing less: an event that is not told is not implemented, an event or a
// filter that S3 does not have, a filter twice or an ARN of another form is
// an invalid argument.
func notificationRules(in notificationConfiguration) (bucket.NotificationConfig, error) {
	var config bucket.NotificationConfig
	told := notifiedEvents()
	for i, r := range in.Rules {
		rule := bucket.NotificationRule{ID: r.ID}
		var arn *string
		switch r.XMLName.Local {
		case queueConfiguration:
			rule.As, arn = bucket.NotifyAsQueue, r.Queue
		case topicConfiguration:
			rule.As, arn = bucket.NotifyAsTopic, r.Topic
		default:
			// The shape lets nothing else through.
			return bucket.NotificationConfig{}, errMalformedXML
		}
		if arn == nil {
			return bucket.NotificationConfig{}, invalidNotification("rule %d names no target", i)
		}
		target, ok := strings.CutPrefix(*arn, notifyARNPrefix)
		if !ok {
			return bucket.NotificationConfig{}, invalidNotification("rule %d: a target is named %s<target>", i, notifyARNPrefix)
		}
		rule.Target = target
		for _, name := range r.Events {
			events, ok := told[name]
			switch {
			case !ok && slices.Contains(untoldEvents(), name):
				return bucket.NotificationConfig{}, errNotImplemented()
			case !ok:
				return bucket.NotificationConfig{}, invalidNotification("rule %d: an event S3 does not have", i)
			}
			for _, event := range events {
				if !slices.Contains(rule.On, event) {
					rule.On = append(rule.On, event)
				}
			}
		}
		if r.Filter != nil {
			var prefix, suffix bool
			for _, f := range r.Filter.Rules {
				// A filter rule without its value would match every key.
				if f.Name == nil || f.Value == nil {
					return bucket.NotificationConfig{}, errMalformedXML
				}
				switch {
				case *f.Value == "":
					return bucket.NotificationConfig{}, invalidNotification("rule %d: a filter rule of no value", i)
				case *f.Name == "prefix" && !prefix:
					rule.Prefix, prefix = *f.Value, true
				case *f.Name == "suffix" && !suffix:
					rule.Suffix, suffix = *f.Value, true
				default:
					return bucket.NotificationConfig{}, invalidNotification("rule %d: a filter rule is named prefix or suffix, each once", i)
				}
			}
		}
		config.Rules = append(config.Rules, rule)
	}
	return config, nil
}

// notificationEventNames returns the event names of S3 that stand for the
// kinds of change of a rule, in the one order a rule keeps them in: both
// kinds of a creation, or of a removal, are the wildcard of it.
func notificationEventNames(on []delivery.Event) []string {
	var names []string
	put, complete := slices.Contains(on, delivery.EventPut), slices.Contains(on, delivery.EventComplete)
	marker, remove := slices.Contains(on, delivery.EventMarker), slices.Contains(on, delivery.EventDelete)
	switch {
	case put && complete:
		names = append(names, "s3:ObjectCreated:*")
	case put:
		names = append(names, "s3:ObjectCreated:Put")
	case complete:
		names = append(names, "s3:ObjectCreated:CompleteMultipartUpload")
	}
	switch {
	case marker && remove:
		names = append(names, "s3:ObjectRemoved:*")
	case marker:
		names = append(names, "s3:ObjectRemoved:DeleteMarkerCreated")
	case remove:
		names = append(names, "s3:ObjectRemoved:Delete")
	}
	return names
}

// notificationConfigurationOf is the rules of a bucket as S3 shows them, in
// the order they were put, each as the kind it was put as.
func notificationConfigurationOf(config bucket.NotificationConfig) notificationConfiguration {
	out := notificationConfiguration{XMLNS: s3Namespace, Rules: []notificationRuleXML{}}
	for _, r := range config.Rules {
		arn := notifyARNPrefix + r.Target
		rule := notificationRuleXML{ID: r.ID, Events: notificationEventNames(r.On)}
		if r.As == bucket.NotifyAsTopic {
			rule.XMLName, rule.Topic = xml.Name{Local: topicConfiguration}, &arn
		} else {
			rule.XMLName, rule.Queue = xml.Name{Local: queueConfiguration}, &arn
		}
		if r.Prefix != "" || r.Suffix != "" {
			rule.Filter = &notificationFilterXML{}
			if r.Prefix != "" {
				rule.Filter.Rules = append(rule.Filter.Rules, filterRuleXML{Name: new("prefix"), Value: new(r.Prefix)})
			}
			if r.Suffix != "" {
				rule.Filter.Rules = append(rule.Filter.Rules, filterRuleXML{Name: new("suffix"), Value: new(r.Suffix)})
			}
		}
		out.Rules = append(out.Rules, rule)
	}
	return out
}

// notifications is the use case of notification rules, or the answer of a
// node that was not given it.
func (h *Handler) notifications() (Notifications, error) {
	if h.cfg.Notifications == nil {
		return nil, errNotImplemented()
	}
	return h.cfg.Notifications, nil
}

func (h *Handler) putBucketNotification(ctx context.Context, req *request) error {
	notifications, err := h.notifications()
	if err != nil {
		return err
	}
	raw, err := h.configBody(req)
	if err != nil {
		return err
	}
	var in notificationConfiguration
	if err := decodeShapedXML(bytes.NewReader(raw), maxConfigBody, &in, notificationShape(), unsupportedNotification()...); err != nil {
		return err
	}
	config, err := notificationRules(in)
	if err != nil {
		return err
	}
	// No rules removes them, as an empty configuration does in S3.
	if _, err := notifications.Set(ctx, req.principal, req.bucket, config); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getBucketNotification(ctx context.Context, req *request) error {
	notifications, err := h.notifications()
	if err != nil {
		return err
	}
	config, err := notifications.Get(ctx, req.principal, req.bucket)
	if err != nil {
		return err
	}
	// A bucket without rules has an empty configuration, not none.
	h.writeXML(ctx, req, http.StatusOK, notificationConfigurationOf(config))
	return nil
}
