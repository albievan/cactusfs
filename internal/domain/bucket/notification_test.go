package bucket_test

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
)

func notifications() bucket.NotificationConfig {
	return bucket.NotificationConfig{Rules: []bucket.NotificationRule{
		{
			ID: "archives", As: bucket.NotifyAsQueue, Target: "hook",
			// Not in the order of a document: Document puts them in it.
			On:     []delivery.Event{delivery.EventDelete, delivery.EventPut, delivery.EventComplete},
			Prefix: "logs/", Suffix: ".gz",
		},
		{ID: "everything <&>", As: bucket.NotifyAsTopic, Target: "bus", On: []delivery.Event{delivery.EventMarker}},
		{ID: "again", As: bucket.NotifyAsQueue, Target: "bus", On: []delivery.Event{delivery.EventPut}, Suffix: ".jpg"},
	}}
}

func TestNotificationDocument(t *testing.T) {
	t.Parallel()
	config := notifications()
	document, err := config.Document()
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "notification.golden.json", document)
	if got := config.Targets(); !slices.Equal(got, []string{"bus", "hook"}) {
		t.Errorf("Targets() = %v", got)
	}
	back, err := bucket.ParseNotificationConfig(document)
	if err != nil {
		t.Fatal(err)
	}
	// What is read is the rules in their order, each with its kinds of
	// change in the order of a document.
	want := notifications()
	want.Rules[0].On = []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventDelete}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("ParseNotificationConfig = %+v, want %+v", back, want)
	}
	// Encoding it twice is encoding it once.
	again, err := back.Document()
	if err != nil || !bytes.Equal(again, document) {
		t.Errorf("the document of what was read: %s, %v", again, err)
	}
	// Document changes nothing of what it was asked about.
	if config.Rules[0].On[0] != delivery.EventDelete {
		t.Error("Document put the rules it was given in order")
	}
	// No document is no rules.
	if none, err := bucket.ParseNotificationConfig(nil); err != nil || !none.Empty() || config.Empty() {
		t.Errorf("ParseNotificationConfig(nil) = %+v, %v", none, err)
	}
}

func TestNotificationRefused(t *testing.T) {
	t.Parallel()
	rule := bucket.NotificationRule{ID: "r", As: bucket.NotifyAsQueue, Target: "hook", On: []delivery.Event{delivery.EventPut}}
	with := func(edit func(*bucket.NotificationRule)) bucket.NotificationConfig {
		changed := rule
		changed.On = slices.Clone(rule.On)
		edit(&changed)
		return bucket.NotificationConfig{Rules: []bucket.NotificationRule{changed}}
	}
	many := make([]bucket.NotificationRule, bucket.MaxNotificationRules+1)
	for i := range many {
		many[i] = rule
		many[i].ID = fmt.Sprintf("r%d", i)
	}
	if err := (bucket.NotificationConfig{Rules: many[:bucket.MaxNotificationRules]}).Validate(); err != nil {
		t.Errorf("as many rules as there can be: %v", err)
	}
	long := with(func(r *bucket.NotificationRule) {
		r.Prefix, r.Suffix = strings.Repeat("p", 1024), strings.Repeat("s", 1024)
	})
	if err := long.Validate(); err != nil {
		t.Errorf("a prefix and a suffix as long as they can be: %v", err)
	}
	// As many targets as a document can name, and one more.
	named := make([]bucket.NotificationRule, bucket.MaxDocumentTargets+1)
	for i := range named {
		named[i] = rule
		named[i].ID, named[i].Target = fmt.Sprintf("r%d", i), fmt.Sprintf("hook-%d", i)
	}
	if err := (bucket.NotificationConfig{Rules: named[:bucket.MaxDocumentTargets]}).Validate(); err != nil {
		t.Errorf("as many targets as a document can name: %v", err)
	}
	for name, config := range map[string]bucket.NotificationConfig{
		"no rules":                       {},
		"too many rules":                 {Rules: many},
		"too many targets":               {Rules: named},
		"a rule without an ID":           with(func(r *bucket.NotificationRule) { r.ID = "" }),
		"an ID that is too long":         with(func(r *bucket.NotificationRule) { r.ID = strings.Repeat("i", 256) }),
		"an ID that is not UTF-8":        with(func(r *bucket.NotificationRule) { r.ID = "r\xff" }),
		"an ID twice":                    {Rules: []bucket.NotificationRule{rule, rule}},
		"shown as nothing":               with(func(r *bucket.NotificationRule) { r.As = "" }),
		"shown as a function":            with(func(r *bucket.NotificationRule) { r.As = "function" }),
		"no target":                      with(func(r *bucket.NotificationRule) { r.Target = "" }),
		"a target that is an address":    with(func(r *bucket.NotificationRule) { r.Target = "https://evil.example/hook" }),
		"a target that is an ARN":        with(func(r *bucket.NotificationRule) { r.Target = "arn:cactusfs:notify:::hook" }),
		"for no kind of change":          with(func(r *bucket.NotificationRule) { r.On = nil }),
		"for a kind that cannot be told": with(func(r *bucket.NotificationRule) { r.On = []delivery.Event{delivery.EventTags} }),
		"for a kind there is not":        with(func(r *bucket.NotificationRule) { r.On = []delivery.Event{"s3:ObjectCreated:*"} }),
		"a kind twice":                   with(func(r *bucket.NotificationRule) { r.On = []delivery.Event{delivery.EventPut, delivery.EventPut} }),
		"a prefix that is too long":      with(func(r *bucket.NotificationRule) { r.Prefix = strings.Repeat("p", 1025) }),
		"a suffix that is too long":      with(func(r *bucket.NotificationRule) { r.Suffix = strings.Repeat("s", 1025) }),
		"a prefix that is not UTF-8":     with(func(r *bucket.NotificationRule) { r.Prefix = "\xc3\x28" }),
		"a suffix that is not UTF-8":     with(func(r *bucket.NotificationRule) { r.Suffix = "\xff" }),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := config.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("Validate = %v, want %v", err, domain.ErrInvalidArgument)
			}
			if _, err := config.Document(); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("Document = %v, want %v", err, domain.ErrInvalidArgument)
			}
		})
	}
	// A hundred rules, each as long as a rule can be, are more than a
	// document holds.
	for i := range many {
		many[i].ID = fmt.Sprintf("%0255d", i)
		many[i].Prefix, many[i].Suffix = strings.Repeat("p", 1024), strings.Repeat("s", 1024)
	}
	huge := bucket.NotificationConfig{Rules: many[:bucket.MaxNotificationRules]}
	if err := huge.Validate(); err != nil {
		t.Fatalf("rules that are each within bounds: %v", err)
	}
	if _, err := huge.Document(); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a document beyond its bound: %v", err)
	}

	const document = `{"v":1,"targets":["hook"],"rules":[{"id":"r","as":"queue","target":"hook","on":["put","delete"],"prefix":"logs/"}]}`
	if _, err := bucket.ParseNotificationConfig([]byte(document)); err != nil {
		t.Fatalf("a canonical document: %v", err)
	}
	for name, changed := range map[string]string{
		"not JSON":                       "{",
		"a field too many":               strings.Replace(document, `"prefix"`, `"more":1,"prefix"`, 1),
		"a field twice":                  strings.Replace(document, `"as":"queue"`, `"as":"topic","as":"queue"`, 1),
		"a field in capitals":            strings.Replace(document, `"target":"hook"`, `"Target":"hook"`, 1),
		"white space":                    strings.Replace(document, `"v":1,`, `"v": 1,`, 1),
		"something after":                document + "{}",
		"kinds in another order":         strings.Replace(document, `["put","delete"]`, `["delete","put"]`, 1),
		"an empty prefix written out":    strings.Replace(document, `"prefix":"logs/"`, `"prefix":""`, 1),
		"an empty suffix written out":    strings.Replace(document, `"prefix":"logs/"`, `"prefix":"logs/","suffix":""`, 1),
		"targets the rules do not use":   strings.Replace(document, `["hook"]`, `["bus","hook"]`, 1),
		"a target the list leaves out":   strings.Replace(document, `["hook"]`, `[]`, 1),
		"targets that are null":          strings.Replace(document, `["hook"]`, `null`, 1),
		"rules that are wrong":           strings.Replace(document, `"as":"queue"`, `"as":"function"`, 1),
		"no rules":                       `{"v":1,"targets":[],"rules":[]}`,
		"an escape that is not needed":   strings.Replace(document, `logs/`, `logs\/`, 1),
		"an angle bracket escaped":       strings.Replace(document, `logs/`, `logs\u003c`, 1),
		"a line break as it is":          strings.Replace(document, `logs/`, "logs\n", 1),
		"bytes that are not UTF-8":       strings.Replace(document, `logs/`, "logs\xff", 1),
		"too long":                       `{"v":1,"targets":["hook"],"x":"` + strings.Repeat("a", bucket.MaxConfigBytes) + `"}`,
		"the document of a replication":  `{"v":1,"target":"hook","bucket":"other","rules":[{"id":"r","priority":0}]}`,
		"a number where a name is":       strings.Replace(document, `"id":"r"`, `"id":7`, 1),
		"a rule that is null":            `{"v":1,"targets":[],"rules":[null]}`,
		"an array where the document is": `[` + document + `]`,
	} {
		if _, err := bucket.ParseNotificationConfig([]byte(changed)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: ParseNotificationConfig = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	for _, version := range []string{`"v":2`, `"v":0`} {
		if _, err := bucket.ParseNotificationConfig([]byte(strings.Replace(document, `"v":1`, version, 1))); !errors.Is(err, domain.ErrUnsupportedFormat) {
			t.Errorf("a document with %s: %v, want %v", version, err, domain.ErrUnsupportedFormat)
		}
	}
}

func TestNotificationMatch(t *testing.T) {
	t.Parallel()
	config := notifications()
	hook := delivery.Consumer{Kind: delivery.ConsumerNotification, Target: "hook"}
	bus := delivery.Consumer{Kind: delivery.ConsumerNotification, Target: "bus"}
	tests := []struct {
		name  string
		event delivery.Event
		key   string
		want  []delivery.Consumer
	}{
		{"prefix and suffix", delivery.EventPut, "logs/2026/a.gz", []delivery.Consumer{hook}},
		{"the completion of an upload", delivery.EventComplete, "logs/a.gz", []delivery.Consumer{hook}},
		{"a version removed", delivery.EventDelete, "logs/a.gz", []delivery.Consumer{hook}},
		{"a kind the rule is not for", delivery.EventMarker, "logs/a.gz", []delivery.Consumer{bus}},
		{"the prefix only", delivery.EventPut, "logs/a.txt", nil},
		{"the suffix only", delivery.EventPut, "other/a.gz", nil},
		{"a prefix that only begins alike", delivery.EventPut, "logs-old/a.gz", nil},
		{"the prefix in another case", delivery.EventPut, "Logs/a.gz", nil},
		{"prefix and suffix that overlap in the key", delivery.EventPut, "logs/.gz", []delivery.Consumer{hook}},
		{"a key shorter than both", delivery.EventPut, "logs/", nil},
		{"a rule without a prefix", delivery.EventMarker, "anything", []delivery.Consumer{bus}},
		{"a suffix only", delivery.EventPut, "photos/a.jpg", []delivery.Consumer{bus}},
		{"a kind that is never told", delivery.EventTags, "logs/a.gz", nil},
		{"no kind", "", "logs/a.gz", nil},
	}
	for _, tt := range tests {
		if got := config.Match(tt.event, tt.key); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Match(%s, %q) = %v, want %v", tt.name, tt.event, tt.key, got, tt.want)
		}
	}
	// Two rules to one target are one consumer, and consumers are in the
	// order of their targets whatever the order of the rules.
	both := bucket.NotificationConfig{Rules: []bucket.NotificationRule{
		{ID: "1", As: bucket.NotifyAsQueue, Target: "hook", On: []delivery.Event{delivery.EventPut}},
		{ID: "2", As: bucket.NotifyAsQueue, Target: "bus", On: []delivery.Event{delivery.EventPut}},
		{ID: "3", As: bucket.NotifyAsTopic, Target: "hook", On: []delivery.Event{delivery.EventPut}, Prefix: "a"},
	}}
	if got := both.Match(delivery.EventPut, "a"); !slices.Equal(got, []delivery.Consumer{bus, hook}) {
		t.Errorf("Match with two rules to a target = %v", got)
	}
	if got := (bucket.NotificationConfig{}).Match(delivery.EventPut, "a"); got != nil {
		t.Errorf("no rules match %v", got)
	}
}

// byHand says what Match must, written out without the helpers of strings:
// a rule is for a change when it lists its kind and the key begins with the
// prefix and ends with the suffix, byte for byte.
func byHand(rules []bucket.NotificationRule, event delivery.Event, key string) []string {
	var targets []string
	for _, rule := range rules {
		listed := false
		for _, on := range rule.On {
			listed = listed || on == event
		}
		fits := len(key) >= len(rule.Prefix) && len(key) >= len(rule.Suffix)
		for i := 0; fits && i < len(rule.Prefix); i++ {
			fits = key[i] == rule.Prefix[i]
		}
		for i := 0; fits && i < len(rule.Suffix); i++ {
			fits = key[len(key)-len(rule.Suffix)+i] == rule.Suffix[i]
		}
		if listed && fits && !slices.Contains(targets, rule.Target) {
			targets = append(targets, rule.Target)
		}
	}
	slices.Sort(targets)
	return targets
}

// TestNotificationMatchEqualsACheckByHand: over rules and keys made of few
// letters, so that prefixes and suffixes do fit now and then.
func TestNotificationMatchEqualsACheckByHand(t *testing.T) {
	t.Parallel()
	letters := func(seed []byte, limit int) string {
		out := make([]byte, 0, limit)
		for _, b := range seed[:min(len(seed), limit)] {
			out = append(out, "ab/"[int(b)%3])
		}
		return string(out)
	}
	property := func(seeds [][]byte, kinds []uint8, key []byte, kind uint8) bool {
		events := []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete, delivery.EventTags}
		var config bucket.NotificationConfig
		for i, seed := range seeds {
			if i == 8 {
				break
			}
			half := len(seed) / 2
			rule := bucket.NotificationRule{
				ID: fmt.Sprint(i), As: bucket.NotifyAsQueue, Target: fmt.Sprintf("t%d", len(seed)%3),
				Prefix: letters(seed[:half], 3), Suffix: letters(seed[half:], 3),
			}
			for n, event := range events[:4] {
				if i < len(kinds) && kinds[i]&(1<<n) != 0 {
					rule.On = append(rule.On, event)
				}
			}
			if len(rule.On) == 0 {
				rule.On = []delivery.Event{delivery.EventPut}
			}
			config.Rules = append(config.Rules, rule)
		}
		event, name := events[int(kind)%len(events)], letters(key, 8)
		got := config.Match(event, name)
		want := byHand(config.Rules, event, name)
		if len(got) != len(want) {
			t.Logf("Match(%s, %q) of %+v = %v, want %v", event, name, config.Rules, got, want)
			return false
		}
		for i, consumer := range got {
			if consumer.Kind != delivery.ConsumerNotification || consumer.Target != want[i] {
				return false
			}
		}
		// The same answer from the rules as they are stored.
		if len(config.Rules) == 0 {
			return true
		}
		document, err := config.Document()
		if err != nil {
			t.Logf("Document: %v", err)
			return false
		}
		stored, err := bucket.ParseNotificationConfig(document)
		return err == nil && slices.Equal(stored.Match(event, name), got)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
}

func FuzzParseNotificationConfig(f *testing.F) {
	f.Add(goldenFile(f, "notification.golden.json"), "logs/2026/a.gz")
	f.Add([]byte(`{"v":1,"targets":["hook"],"rules":[{"id":"r","as":"queue","target":"hook","on":["put","delete"],"prefix":"logs/"}]}`), "logs/a")
	f.Add([]byte(`{"v":1,"targets":["hook"],"rules":[{"id":"r","as":"topic","target":"hook","on":["delete","put"]}]}`), "")
	f.Add([]byte(`{"v":2,"targets":[],"rules":[]}`), "k")
	f.Add([]byte(`{`), "k")
	f.Add(goldenFile(f, "notification-text.golden.json"), everyClass)
	f.Fuzz(func(t *testing.T, document []byte, key string) {
		config, err := bucket.ParseNotificationConfig(document)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("ParseNotificationConfig: %v", err)
			}
			return
		}
		if len(document) == 0 {
			return
		}
		// What is read is exactly what would be written: a document has
		// one form.
		again, err := config.Document()
		if err != nil || !bytes.Equal(again, document) {
			t.Fatalf("a document that was read is written as %s, %v", again, err)
		}
		// Whatever matches is a target the document lists, once, in
		// order.
		for _, event := range []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete, delivery.EventTags, ""} {
			consumers := config.Match(event, key)
			targets := make([]string, 0, len(consumers))
			for _, consumer := range consumers {
				if consumer.Kind != delivery.ConsumerNotification || !slices.Contains(config.Targets(), consumer.Target) || !event.Notified() {
					t.Fatalf("Match(%s, %q) = %v", event, key, consumers)
				}
				targets = append(targets, consumer.Target)
			}
			if !slices.Equal(targets, byHand(config.Rules, event, key)) {
				t.Fatalf("Match(%s, %q) = %v, by hand %v", event, key, targets, byHand(config.Rules, event, key))
			}
		}
	})
}
