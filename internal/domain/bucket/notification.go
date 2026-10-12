package bucket

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// ConfigNotification is the kind of bucket configuration that holds the
// notification rules of a bucket (ADR-0025), and ConfigReplication the kind
// that says where its versions are copied to (ADR-0026). Both exist from
// entry schema 5 on (ADR-0023); ValidConfigKind names neither, and the
// metadata store accepts them only where every member of the cluster knows
// them.
const (
	ConfigNotification = "notification"
	ConfigReplication  = "replication"
)

// Limits of the rules of the two documents.
const (
	// MaxNotificationRules is how many notification rules a bucket can
	// have.
	MaxNotificationRules = 100
	// MaxReplicationRules is how many replication rules a bucket can have.
	MaxReplicationRules = 100
	maxRuleID           = 255
	maxRuleAffix        = 1024
)

// How the S3 API shows a notification rule. It changes nothing of what is
// delivered.
const (
	// NotifyAsQueue shows the rule as a QueueConfiguration.
	NotifyAsQueue = "queue"
	// NotifyAsTopic shows the rule as a TopicConfiguration.
	NotifyAsTopic = "topic"
)

// configVersion is the version of the two stored documents.
const configVersion = 1

// NotificationRule says which changes, of which keys, are told to which
// target.
type NotificationRule struct {
	// ID names the rule; it is unique in the bucket.
	ID string
	// As is NotifyAsQueue or NotifyAsTopic.
	As string
	// Target names the target, which is a webhook or a NATS server.
	Target string
	// On are the kinds of change the rule is for, of those
	// delivery.Event.Notified names.
	On []delivery.Event
	// Prefix and Suffix, when not empty, confine the rule to keys that
	// begin and end with them, byte for byte.
	Prefix string
	Suffix string
}

// NotificationConfig is the notification rules of a bucket, in the order
// they were put.
type NotificationConfig struct {
	Rules []NotificationRule
}

func invalidNotification(format string, args ...any) error {
	return fmt.Errorf("%w: notification rules: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// notifiedEvents are the kinds of change a rule can ask for, in the order a
// document lists them.
func notifiedEvents() []delivery.Event {
	return []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete}
}

// validRuleText reports whether s can be the ID, the prefix or the suffix of
// a rule: at most limit bytes of valid UTF-8, so that it is stored as it is.
func validRuleText(s string, limit int) bool {
	return len(s) <= limit && utf8.ValidString(s)
}

// Validate reports whether the rules can be kept. Which targets there are,
// and whether they admit the bucket, is for the store to say. The error
// wraps domain.ErrInvalidArgument.
func (c NotificationConfig) Validate() error {
	if len(c.Rules) == 0 || len(c.Rules) > MaxNotificationRules {
		return invalidNotification("needs 1 to %d rules", MaxNotificationRules)
	}
	ids := map[string]bool{}
	for i, rule := range c.Rules {
		switch {
		case rule.ID == "" || !validRuleText(rule.ID, maxRuleID):
			return invalidNotification("rule %d: an ID of 1 to %d bytes", i, maxRuleID)
		case ids[rule.ID]:
			return invalidNotification("rule %d: its ID is used twice", i)
		case rule.As != NotifyAsQueue && rule.As != NotifyAsTopic:
			return invalidNotification("rule %d: shown as a queue or as a topic", i)
		case !validRuleText(rule.Prefix, maxRuleAffix) || !validRuleText(rule.Suffix, maxRuleAffix):
			return invalidNotification("rule %d: a prefix or a suffix of more than %d bytes", i, maxRuleAffix)
		case len(rule.On) == 0:
			return invalidNotification("rule %d: is for no kind of change", i)
		}
		ids[rule.ID] = true
		if err := delivery.ValidateTargetName(rule.Target); err != nil {
			return invalidNotification("rule %d: %v", i, err)
		}
		for n, event := range rule.On {
			if !event.Notified() || slices.Contains(rule.On[:n], event) {
				return invalidNotification("rule %d: a kind of change that cannot be told, or that is there twice", i)
			}
		}
	}
	return nil
}

// Targets returns the names of the targets the rules use, sorted, each once.
func (c NotificationConfig) Targets() []string {
	names := make([]string, 0, len(c.Rules))
	for _, rule := range c.Rules {
		names = append(names, rule.Target)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Empty reports whether there are no rules.
func (c NotificationConfig) Empty() bool {
	return len(c.Rules) == 0
}

// Document returns the rules as the document that is stored and that every
// member of a cluster matches by, in the canonical form (ADR-0023):
//
//	{"v":1,"targets":[name],"rules":[{"id","as","target","on":[event],"prefix","suffix"}]}
//
// the targets the rules use, sorted; the rules in their order, each with its
// kinds of change in the one order a document has them in, and with "prefix"
// and "suffix" left out when empty. The same rules are always the same
// bytes, whatever release of Go wrote them.
func (c NotificationConfig) Document() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	w := &canonicalWriter{}
	w.raw(`{"v":`)
	w.num(configVersion)
	w.raw(`,"targets":[`)
	for i, target := range c.Targets() {
		if i > 0 {
			w.raw(",")
		}
		w.str(target)
	}
	w.raw(`],"rules":[`)
	for i, rule := range c.Rules {
		if i > 0 {
			w.raw(",")
		}
		w.raw(`{"id":`)
		w.str(rule.ID)
		w.raw(`,"as":`)
		w.str(rule.As)
		w.raw(`,"target":`)
		w.str(rule.Target)
		w.raw(`,"on":[`)
		first := true
		for _, event := range notifiedEvents() {
			if !slices.Contains(rule.On, event) {
				continue
			}
			if !first {
				w.raw(",")
			}
			first = false
			w.str(string(event))
		}
		w.raw("]")
		if rule.Prefix != "" {
			w.raw(`,"prefix":`)
			w.str(rule.Prefix)
		}
		if rule.Suffix != "" {
			w.raw(`,"suffix":`)
			w.str(rule.Suffix)
		}
		w.raw("}")
	}
	w.raw("]}")
	if w.bad {
		return nil, invalidNotification("the document cannot be written")
	}
	if len(w.buf) > MaxConfigBytes {
		return nil, invalidNotification("the document is longer than %d bytes", MaxConfigBytes)
	}
	return w.buf, nil
}

// ParseNotificationConfig reads a stored document. An empty document is no
// rules. It takes the bytes Document writes and no others - a field too
// many, a field twice, kinds of change in another order, a list of targets
// that is not the rules', white space, a string escaped another way - so
// that a document means to every member what it reads as. The error wraps
// domain.ErrInvalidArgument, or domain.ErrUnsupportedFormat for a document
// of a version this code does not know.
func ParseNotificationConfig(document []byte) (NotificationConfig, error) {
	if len(document) == 0 {
		return NotificationConfig{}, nil
	}
	if len(document) > MaxConfigBytes {
		return NotificationConfig{}, invalidNotification("the document is longer than %d bytes", MaxConfigBytes)
	}
	r := &canonicalReader{data: document}
	r.lit(`{"v":`)
	version := r.small()
	if r.bad {
		return NotificationConfig{}, invalidNotification("the document is not in its canonical form")
	}
	if version != configVersion {
		return NotificationConfig{}, fmt.Errorf("%w: notification rules of version %d", domain.ErrUnsupportedFormat, version)
	}
	// The list of targets is read and thrown away: it is the rules', and
	// whether it is, is seen when the document is written again.
	r.lit(`,"targets":`)
	r.list(func() { _ = r.str() })
	var config NotificationConfig
	r.lit(`,"rules":`)
	r.list(func() {
		var rule NotificationRule
		r.lit(`{"id":`)
		rule.ID = r.str()
		r.lit(`,"as":`)
		rule.As = r.str()
		r.lit(`,"target":`)
		rule.Target = r.str()
		r.lit(`,"on":`)
		r.list(func() { rule.On = append(rule.On, delivery.Event(r.str())) })
		if r.has(`,"prefix":`) {
			rule.Prefix = r.str()
		}
		if r.has(`,"suffix":`) {
			rule.Suffix = r.str()
		}
		r.lit("}")
		config.Rules = append(config.Rules, rule)
	})
	r.lit("}")
	if !r.end() {
		return NotificationConfig{}, invalidNotification("the document is not in its canonical form")
	}
	again, err := config.Document()
	if err != nil {
		return NotificationConfig{}, err
	}
	if !bytes.Equal(again, document) {
		return NotificationConfig{}, invalidNotification("the document is not in its canonical form")
	}
	return config, nil
}

// Match returns the consumers a change of the kind event to the key is for:
// one for every target that a rule for that kind of change names whose
// prefix the key begins with and whose suffix it ends with, compared byte
// for byte. They are sorted by target, each once.
//
// It is part of the replicated format: the state machine of every member
// appends the rows of the outbox by it.
func (c NotificationConfig) Match(event delivery.Event, key string) []delivery.Consumer {
	var targets []string
	for _, rule := range c.Rules {
		if slices.Contains(rule.On, event) && strings.HasPrefix(key, rule.Prefix) && strings.HasSuffix(key, rule.Suffix) {
			targets = append(targets, rule.Target)
		}
	}
	slices.Sort(targets)
	targets = slices.Compact(targets)
	if len(targets) == 0 {
		return nil
	}
	consumers := make([]delivery.Consumer, len(targets))
	for i, target := range targets {
		consumers[i] = delivery.Consumer{Kind: delivery.ConsumerNotification, Target: target}
	}
	return consumers
}
