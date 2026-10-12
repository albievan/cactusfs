package bucket_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func replication() bucket.ReplicationConfig {
	return bucket.ReplicationConfig{Target: "site-b", Bucket: "photos-copy", Rules: []bucket.ReplicationRule{
		{ID: "everything", Enabled: true, Priority: 1},
		{ID: "logs", Enabled: true, Priority: 5, Prefix: "logs/", Markers: true},
		{ID: "kept for later", Priority: 9, Prefix: "logs/private/"},
	}}
}

func TestReplicationDocument(t *testing.T) {
	t.Parallel()
	config := replication()
	document, err := config.Document()
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "replication.golden.json", document)
	back, err := bucket.ParseReplicationConfig(document)
	if err != nil || !reflect.DeepEqual(back, config) {
		t.Fatalf("ParseReplicationConfig = %+v, %v; want %+v", back, err, config)
	}
	again, err := back.Document()
	if err != nil || !bytes.Equal(again, document) {
		t.Errorf("the document of what was read: %s, %v", again, err)
	}
	if got := config.Targets(); !slices.Equal(got, []string{"site-b"}) {
		t.Errorf("Targets() = %v", got)
	}
	none, err := bucket.ParseReplicationConfig(nil)
	if err != nil || !none.Empty() || config.Empty() || none.Targets() != nil {
		t.Errorf("ParseReplicationConfig(nil) = %+v, %v", none, err)
	}
}

func TestReplicationRefused(t *testing.T) {
	t.Parallel()
	with := func(edit func(*bucket.ReplicationConfig)) bucket.ReplicationConfig {
		changed := replication()
		edit(&changed)
		return changed
	}
	many := make([]bucket.ReplicationRule, bucket.MaxReplicationRules+1)
	for i := range many {
		many[i] = bucket.ReplicationRule{ID: fmt.Sprint(i), Enabled: true, Priority: i}
	}
	if err := with(func(c *bucket.ReplicationConfig) { c.Rules = many[:bucket.MaxReplicationRules] }).Validate(); err != nil {
		t.Errorf("as many rules as there can be: %v", err)
	}
	for name, config := range map[string]bucket.ReplicationConfig{
		"nothing":                       {},
		"no target":                     with(func(c *bucket.ReplicationConfig) { c.Target = "" }),
		"a target that is an address":   with(func(c *bucket.ReplicationConfig) { c.Target = "https://s3.evil.example" }),
		"no bucket":                     with(func(c *bucket.ReplicationConfig) { c.Bucket = "" }),
		"a bucket that is an ARN":       with(func(c *bucket.ReplicationConfig) { c.Bucket = "arn:aws:s3:::photos-copy" }),
		"a bucket in capitals":          with(func(c *bucket.ReplicationConfig) { c.Bucket = "Photos" }),
		"no rules":                      with(func(c *bucket.ReplicationConfig) { c.Rules = nil }),
		"too many rules":                with(func(c *bucket.ReplicationConfig) { c.Rules = many }),
		"a rule without an ID":          with(func(c *bucket.ReplicationConfig) { c.Rules[1].ID = "" }),
		"an ID that is too long":        with(func(c *bucket.ReplicationConfig) { c.Rules[1].ID = strings.Repeat("i", 256) }),
		"an ID that is not UTF-8":       with(func(c *bucket.ReplicationConfig) { c.Rules[1].ID = "\xff" }),
		"an ID twice":                   with(func(c *bucket.ReplicationConfig) { c.Rules[1].ID = "everything" }),
		"a priority below none":         with(func(c *bucket.ReplicationConfig) { c.Rules[0].Priority = -1 }),
		"a priority beyond the last":    with(func(c *bucket.ReplicationConfig) { c.Rules[0].Priority = math.MaxInt32 + 1 }),
		"a prefix that is too long":     with(func(c *bucket.ReplicationConfig) { c.Rules[0].Prefix = strings.Repeat("p", 1025) }),
		"a prefix that is not UTF-8":    with(func(c *bucket.ReplicationConfig) { c.Rules[0].Prefix = "\xc3\x28" }),
		"a bucket named as an address":  with(func(c *bucket.ReplicationConfig) { c.Bucket = "10.0.0.1" }),
		"a bucket with a reserved name": with(func(c *bucket.ReplicationConfig) { c.Bucket = "xn--photos" }),
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
	// A hundred rules as long as rules can be are more than a document
	// holds.
	for i := range many {
		many[i].ID, many[i].Prefix = fmt.Sprintf("%0255d", i), strings.Repeat("p", 1024)
	}
	huge := with(func(c *bucket.ReplicationConfig) { c.Rules = many[:bucket.MaxReplicationRules] })
	if err := huge.Validate(); err != nil {
		t.Fatalf("rules that are each within bounds: %v", err)
	}
	if _, err := huge.Document(); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a document beyond its bound: %v", err)
	}

	const document = `{"v":1,"target":"site-b","bucket":"photos-copy","rules":[{"id":"r","enabled":true,"priority":1,"prefix":"logs/","markers":true}]}`
	if _, err := bucket.ParseReplicationConfig([]byte(document)); err != nil {
		t.Fatalf("a canonical document: %v", err)
	}
	for name, changed := range map[string]string{
		"not JSON":                        "{",
		"a field too many":                strings.Replace(document, `"priority"`, `"storage_class":"x","priority"`, 1),
		"a field twice":                   strings.Replace(document, `"target":"site-b"`, `"target":"other","target":"site-b"`, 1),
		"a field in capitals":             strings.Replace(document, `"bucket"`, `"Bucket"`, 1),
		"white space":                     strings.Replace(document, `"v":1,`, `"v":1, `, 1),
		"something after":                 document + " ",
		"another order":                   strings.Replace(document, `"enabled":true,"priority":1`, `"priority":1,"enabled":true`, 1),
		"false written out":               strings.Replace(document, `"markers":true`, `"markers":false`, 1),
		"an empty prefix written out":     strings.Replace(document, `"prefix":"logs/"`, `"prefix":""`, 1),
		"no priority":                     strings.Replace(document, `"priority":1,`, ``, 1),
		"a priority with a point":         strings.Replace(document, `"priority":1`, `"priority":1.0`, 1),
		"a priority as text":              strings.Replace(document, `"priority":1`, `"priority":"1"`, 1),
		"a priority beyond every number":  strings.Replace(document, `"priority":1`, `"priority":99999999999999999999`, 1),
		"configuration that is wrong":     strings.Replace(document, `"target":"site-b"`, `"target":"Site B"`, 1),
		"no rules":                        `{"v":1,"target":"site-b","bucket":"photos-copy","rules":[]}`,
		"rules that are null":             `{"v":1,"target":"site-b","bucket":"photos-copy","rules":null}`,
		"too long":                        `{"v":1,"target":"site-b","x":"` + strings.Repeat("a", bucket.MaxConfigBytes) + `"}`,
		"the document of notifications":   `{"v":1,"targets":["hook"],"rules":[{"id":"r","as":"queue","target":"hook","on":["put"]}]}`,
		"a second destination":            strings.Replace(document, `"rules"`, `"bucket2":"other","rules"`, 1),
		"bytes that are not UTF-8":        strings.Replace(document, `logs/`, "logs\xff", 1),
		"an escape that is not needed":    strings.Replace(document, `logs/`, `logs\/`, 1),
		"an ampersand escaped":            strings.Replace(document, `logs/`, `logs\u0026`, 1),
		"a priority with a zero first":    strings.Replace(document, `"priority":1`, `"priority":01`, 1),
		"a priority below nothing":        strings.Replace(document, `"priority":1`, `"priority":-1`, 1),
		"a priority as an exponent":       strings.Replace(document, `"priority":1`, `"priority":1e0`, 1),
		"true as a number":                strings.Replace(document, `"enabled":true`, `"enabled":1`, 1),
		"an object where the rules are":   `{"v":1,"target":"site-b","bucket":"photos-copy","rules":{}}`,
		"a document that is only a brace": `{}x`,
	} {
		if _, err := bucket.ParseReplicationConfig([]byte(changed)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: ParseReplicationConfig = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	for _, version := range []string{`"v":2`, `"v":0`} {
		if _, err := bucket.ParseReplicationConfig([]byte(strings.Replace(document, `"v":1`, version, 1))); !errors.Is(err, domain.ErrUnsupportedFormat) {
			t.Errorf("a document with %s: %v, want %v", version, err, domain.ErrUnsupportedFormat)
		}
	}
}

func TestReplicationMatch(t *testing.T) {
	t.Parallel()
	config := replication()
	site := []delivery.Consumer{{Kind: delivery.ConsumerReplication, Target: "site-b"}}
	tests := []struct {
		name    string
		event   delivery.Event
		key     string
		decides string
		want    []delivery.Consumer
	}{
		{"a version under the rule for everything", delivery.EventPut, "photos/a.jpg", "everything", site},
		{"the completion of an upload", delivery.EventComplete, "photos/a.jpg", "everything", site},
		{"a marker where markers are not copied", delivery.EventMarker, "photos/a.jpg", "everything", nil},
		{"a marker where they are", delivery.EventMarker, "logs/a", "logs", site},
		{"the greater priority decides", delivery.EventPut, "logs/a", "logs", site},
		{"a rule that is not enabled decides nothing", delivery.EventMarker, "logs/private/a", "logs", site},
		{"a version removed for good, never", delivery.EventDelete, "logs/a", "logs", nil},
		{"tags of a version", delivery.EventTags, "photos/a.jpg", "everything", site},
		{"retention of a version", delivery.EventRetention, "photos/a.jpg", "everything", site},
		{"legal hold of a version", delivery.EventLegalHold, "logs/a", "logs", site},
		{"no kind of change", "", "photos/a.jpg", "everything", nil},
	}
	for _, tt := range tests {
		rule, ok := config.Decides(tt.key)
		if !ok || rule.ID != tt.decides {
			t.Errorf("%s: Decides(%q) = %q, %v; want %q", tt.name, tt.key, rule.ID, ok, tt.decides)
		}
		if got := config.Match(tt.event, tt.key); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Match(%s, %q) = %v, want %v", tt.name, tt.event, tt.key, got, tt.want)
		}
	}

	// No rule is about the key: nothing is sent.
	narrow := bucket.ReplicationConfig{Target: "site-b", Bucket: "photos-copy", Rules: []bucket.ReplicationRule{
		{ID: "a", Enabled: true, Prefix: "logs/"}, {ID: "off", Prefix: ""},
	}}
	if _, ok := narrow.Decides("photos/a"); ok || narrow.Match(delivery.EventPut, "photos/a") != nil {
		t.Error("a key no enabled rule is about is replicated")
	}
	// Of two rules of one priority the first decides.
	tie := bucket.ReplicationConfig{Target: "site-b", Bucket: "photos-copy", Rules: []bucket.ReplicationRule{
		{ID: "first", Enabled: true, Priority: 3}, {ID: "second", Enabled: true, Priority: 3, Markers: true},
	}}
	if rule, _ := tie.Decides("k"); rule.ID != "first" || tie.Match(delivery.EventMarker, "k") != nil {
		t.Errorf("of two rules of one priority %q decides", rule.ID)
	}
	if (bucket.ReplicationConfig{}).Match(delivery.EventPut, "k") != nil {
		t.Error("no configuration replicates")
	}
}

func TestReplicates(t *testing.T) {
	t.Parallel()
	config := replication()
	tests := []struct {
		name    string
		event   delivery.Event
		key     string
		by      string
		replica bool
		status  object.ReplicationStatus
		want    bool
	}{
		{"a put", delivery.EventPut, "logs/a", "user/alice", false, "", true},
		{"a put by nobody on record", delivery.EventPut, "logs/a", "", false, "", true},
		{"a version that arrived as a replica", delivery.EventPut, "logs/a", "key/AKID", true, "", false},
		{"a marker that arrived as a replica", delivery.EventMarker, "logs/a", "key/AKID", true, "", false},
		{"a marker of a caller", delivery.EventMarker, "logs/a", "user/alice", false, "", true},
		{"a marker of the lifecycle service", delivery.EventMarker, "logs/a", bucket.LifecycleActor, false, "", false},
		{"a put by the lifecycle service, which makes none", delivery.EventPut, "logs/a", bucket.LifecycleActor, false, "", true},
		{"a marker where markers are not copied", delivery.EventMarker, "photos/a", "user/alice", false, "", false},
		{"tags of a version that is pending", delivery.EventTags, "logs/a", "user/alice", false, object.ReplicationPending, true},
		{"tags of a version that was replicated", delivery.EventTags, "logs/a", "user/alice", false, object.ReplicationCompleted, true},
		{"tags of a version that failed", delivery.EventTags, "logs/a", "user/alice", false, object.ReplicationFailed, true},
		{"tags of a version no rule was about", delivery.EventTags, "logs/a", "user/alice", false, "", false},
		{"retention of a version no rule was about", delivery.EventRetention, "logs/a", "user/alice", false, "", false},
		{"legal hold of a version no rule was about", delivery.EventLegalHold, "logs/a", "user/alice", false, "", false},
		{"tags of a replica", delivery.EventTags, "logs/a", "user/alice", false, object.ReplicationReplica, false},
		{"retention of a replica", delivery.EventRetention, "logs/a", "user/alice", false, object.ReplicationReplica, false},
		{"a version removed for good", delivery.EventDelete, "logs/a", "user/alice", false, object.ReplicationCompleted, false},
	}
	for _, tt := range tests {
		got := config.Replicates(tt.event, tt.key, tt.by, tt.replica, tt.status)
		if (len(got) == 1) != tt.want || (tt.want && got[0] != delivery.Consumer{Kind: delivery.ConsumerReplication, Target: "site-b"}) {
			t.Errorf("%s: Replicates = %v, want one: %v", tt.name, got, tt.want)
		}
	}
}

// TestReplicationDecidesAsWrittenByHand: the rule that decides is the one a
// reader of the ADR would pick - enabled, about the key, of the greatest
// priority, the first of them.
func TestReplicationDecidesAsWrittenByHand(t *testing.T) {
	t.Parallel()
	property := func(seeds []uint16, key uint16) bool {
		prefixes := []string{"", "a", "a/", "a/b", "b"}
		keys := []string{"a", "a/", "a/b/c", "b/a", "c", "ab"}
		config := bucket.ReplicationConfig{Target: "site-b", Bucket: "photos-copy"}
		for i, seed := range seeds {
			if i == 10 {
				break
			}
			config.Rules = append(config.Rules, bucket.ReplicationRule{
				ID: fmt.Sprint(i), Enabled: seed&1 != 0, Markers: seed&2 != 0, Priority: int(seed>>2) % 4, Prefix: prefixes[int(seed>>4)%len(prefixes)],
			})
		}
		name := keys[int(key)%len(keys)]
		best := -1
		for i, rule := range config.Rules {
			about := rule.Enabled && len(name) >= len(rule.Prefix) && name[:len(rule.Prefix)] == rule.Prefix
			if about && (best < 0 || rule.Priority > config.Rules[best].Priority) {
				best = i
			}
		}
		rule, ok := config.Decides(name)
		if ok != (best >= 0) || (ok && rule != config.Rules[best]) {
			t.Logf("Decides(%q) of %+v = %+v, %v; by hand rule %d", name, config.Rules, rule, ok, best)
			return false
		}
		for _, event := range []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete, delivery.EventTags} {
			want := ok && event != delivery.EventDelete && (event != delivery.EventMarker || rule.Markers)
			if got := config.Match(event, name); (len(got) == 1) != want {
				return false
			}
		}
		if len(config.Rules) == 0 {
			return true
		}
		// The same answer from the configuration as it is stored.
		document, err := config.Document()
		if err != nil {
			t.Logf("Document: %v", err)
			return false
		}
		stored, err := bucket.ParseReplicationConfig(document)
		again, okAgain := stored.Decides(name)
		return err == nil && okAgain == ok && again == rule
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
}

func FuzzParseReplicationConfig(f *testing.F) {
	f.Add(goldenFile(f, "replication.golden.json"), "logs/private/a")
	f.Add([]byte(`{"v":1,"target":"site-b","bucket":"photos-copy","rules":[{"id":"r","enabled":true,"priority":1,"prefix":"logs/","markers":true}]}`), "logs/a")
	f.Add([]byte(`{"v":1,"target":"site-b","bucket":"photos-copy","rules":[{"id":"r","priority":0}]}`), "")
	f.Add([]byte(`{"v":3}`), "k")
	f.Add([]byte(`[`), "k")
	f.Add(goldenFile(f, "replication-text.golden.json"), everyClass)
	f.Fuzz(func(t *testing.T, document []byte, key string) {
		config, err := bucket.ParseReplicationConfig(document)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("ParseReplicationConfig: %v", err)
			}
			return
		}
		if len(document) == 0 {
			return
		}
		again, err := config.Document()
		if err != nil || !bytes.Equal(again, document) {
			t.Fatalf("a document that was read is written as %s, %v", again, err)
		}
		rule, ok := config.Decides(key)
		if ok && (!rule.Enabled || !strings.HasPrefix(key, rule.Prefix)) {
			t.Fatalf("Decides(%q) = %+v", key, rule)
		}
		for _, event := range []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete, delivery.EventTags, delivery.EventRetention, delivery.EventLegalHold, ""} {
			consumers := config.Match(event, key)
			switch {
			case len(consumers) > 1, len(consumers) == 1 && consumers[0] != delivery.Consumer{Kind: delivery.ConsumerReplication, Target: config.Target}:
				t.Fatalf("Match(%s, %q) = %v", event, key, consumers)
			case len(consumers) == 1 && (!ok || event == delivery.EventDelete || !event.Valid() || (event == delivery.EventMarker && !rule.Markers)):
				t.Fatalf("Match(%s, %q) sends what is not to be sent", event, key)
			}
			// What Match does not send, Replicates does not either.
			if sent := config.Replicates(event, key, "user/x", false, object.ReplicationPending); len(sent) > len(consumers) {
				t.Fatalf("Replicates(%s, %q) = %v, Match %v", event, key, sent, consumers)
			}
			if sent := config.Replicates(event, key, "user/x", true, ""); sent != nil {
				t.Fatalf("a replica is sent on: %v", sent)
			}
		}
	})
}

func TestResync(t *testing.T) {
	t.Parallel()
	version := object.VersionRef{ID: object.ID{7}}.String()
	valid := []bucket.Resync{
		{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning},
		{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "a/b", Version: version, Scanned: 500, Sent: 20},
		{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "a/b", Version: object.NullVersion, Scanned: 1, Sent: 1},
		{Bucket: "photos", Target: "site-b", State: bucket.ResyncDone, Key: "z", Version: version, Scanned: 1200, Sent: 0},
	}
	for _, r := range valid {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	base := valid[1]
	for name, edit := range map[string]func(*bucket.Resync){
		"no bucket":                   func(r *bucket.Resync) { r.Bucket = "" },
		"no target":                   func(r *bucket.Resync) { r.Target = "" },
		"a target that is an address": func(r *bucket.Resync) { r.Target = "https://x" },
		"no state":                    func(r *bucket.Resync) { r.State = "" },
		"a state there is not":        func(r *bucket.Resync) { r.State = "paused" },
		"more sent than looked at":    func(r *bucket.Resync) { r.Sent = r.Scanned + 1 },
		"a key without a version":     func(r *bucket.Resync) { r.Version = "" },
		"a version without a key":     func(r *bucket.Resync) { r.Key = "" },
		"a key that is none":          func(r *bucket.Resync) { r.Key = "\xff" },
		"a version that is none":      func(r *bucket.Resync) { r.Version = "latest" },
	} {
		changed := base
		edit(&changed)
		if err := changed.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
	if err := (bucket.Resync{}).Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("no record: %v", err)
	}
}
