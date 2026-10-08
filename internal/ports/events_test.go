package ports_test

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/ports"
)

// contract reads api/asyncapi/events.yaml.
func contract(t *testing.T) string {
	t.Helper()
	root, err := os.OpenRoot("../../api/asyncapi")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile("events.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// schemaProperties returns the property names of a schema under
// components/schemas, and which of them are required. The document is
// validated as AsyncAPI by `make contracts`; here its layout is relied on.
func schemaProperties(t *testing.T, doc, schema string) (properties, required []string) {
	t.Helper()
	_, rest, found := strings.Cut(doc, "\n    "+schema+":\n")
	if !found {
		t.Fatalf("schema %s is not in the contract", schema)
	}
	inProperties := false
	for line := range strings.SplitSeq(rest, "\n") {
		switch {
		case strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     "):
			return properties, required // the next schema
		case strings.HasPrefix(line, "      required: ["):
			list := strings.TrimSuffix(strings.TrimPrefix(line, "      required: ["), "]")
			for name := range strings.SplitSeq(list, ",") {
				required = append(required, strings.TrimSpace(name))
			}
		case line == "      properties:":
			inProperties = true
		case inProperties && strings.HasPrefix(line, "        ") && !strings.HasPrefix(line, "         "):
			properties = append(properties, strings.TrimSuffix(strings.TrimSpace(line), ":"))
		}
	}
	return properties, required
}

// jsonFields returns the JSON names of a struct's fields, and which of them
// are always present.
func jsonFields(v any) (all, always []string) {
	typ := reflect.TypeOf(v)
	for i := range typ.NumField() {
		name, options, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		all = append(all, name)
		if !strings.Contains(options, "omit") {
			always = append(always, name)
		}
	}
	return all, always
}

// TestEventsMatchTheContract: the event names and payloads in this package
// are the ones api/asyncapi/events.yaml defines - no more, no fewer, with the
// same fields.
func TestEventsMatchTheContract(t *testing.T) {
	t.Parallel()
	doc := contract(t)

	events := map[string]struct {
		payload any
		schema  string
	}{
		ports.EventBucketCreated:             {ports.BucketEvent{}, "Bucket"},
		ports.EventBucketDeleted:             {ports.BucketEvent{}, "Bucket"},
		ports.EventObjectCreated:             {ports.ObjectEvent{}, "Object"},
		ports.EventObjectDeleted:             {ports.ObjectEvent{}, "Object"},
		ports.EventRootKeySet:                {ports.RootCredentialEvent{}, "RootCredential"},
		ports.EventTopologyChanged:           {ports.TopologyEvent{}, "Topology"},
		ports.EventNodeRegistered:            {ports.NodeEvent{}, "Node"},
		ports.EventNodeRemoved:               {ports.NodeEvent{}, "Node"},
		ports.EventAuditRecorded:             {ports.AuditEvent{}, "Audit"},
		ports.EventBucketVersioningChanged:   {ports.BucketVersioningEvent{}, "BucketVersioning"},
		ports.EventObjectDeleteMarkerCreated: {ports.ObjectEvent{}, "Object"},
		ports.EventObjectRetentionChanged:    {ports.ObjectEvent{}, "Object"},
		ports.EventObjectLegalHoldChanged:    {ports.ObjectEvent{}, "Object"},
	}

	var addresses []string
	for _, match := range regexp.MustCompile(`(?m)^    address: (\S+)$`).FindAllStringSubmatch(doc, -1) {
		addresses = append(addresses, match[1])
	}
	if len(addresses) != len(events) {
		t.Errorf("the contract has %d channels, the code %d events", len(addresses), len(events))
	}
	name := regexp.MustCompile(`^cactusfs\.[a-z]+\.[a-z]+(-[a-z]+)*\.v[0-9]+$`)
	for event, want := range events {
		if !name.MatchString(event) {
			t.Errorf("event name %q is not cactusfs.<aggregate>.<event>.v<major>", event)
		}
		if !slices.Contains(addresses, event) {
			t.Errorf("event %s has no channel in the contract", event)
		}
		// The message of that name carries the schema the code sends.
		_, message, found := strings.Cut(doc, "      name: "+event+"\n")
		if !found {
			t.Errorf("event %s has no message in the contract", event)
			continue
		}
		block := message
		if next := regexp.MustCompile(`\n {2,4}\S`).FindStringIndex(message); next != nil {
			block = message[:next[0]+1]
		}
		if !strings.Contains(block, "const: "+event+"\n") || !strings.Contains(block, "$ref: '#/components/schemas/"+want.schema+"'") {
			t.Errorf("message %s does not carry type %s and schema %s", event, event, want.schema)
		}

		properties, required := schemaProperties(t, doc, want.schema)
		fields, always := jsonFields(want.payload)
		slices.Sort(properties)
		slices.Sort(required)
		slices.Sort(fields)
		slices.Sort(always)
		if !slices.Equal(properties, fields) {
			t.Errorf("%s: the contract has properties %v, the payload has fields %v", want.schema, properties, fields)
		}
		if !slices.Equal(required, always) {
			t.Errorf("%s: the contract requires %v, the payload always sends %v", want.schema, required, always)
		}
	}

	// The envelope.
	properties, required := schemaProperties(t, doc, "Envelope")
	fields, always := jsonFields(ports.Event{})
	slices.Sort(properties)
	slices.Sort(required)
	slices.Sort(fields)
	slices.Sort(always)
	if !slices.Equal(properties, fields) || !slices.Equal(required, always) {
		t.Errorf("envelope: contract %v (required %v), code %v (always %v)", properties, required, fields, always)
	}
}

// TestEventEncoding: an event serialises as the contract describes, and a
// change without a time has no time field rather than a zero time.
func TestEventEncoding(t *testing.T) {
	t.Parallel()
	event := ports.Event{
		ID: "000000000000002a-0", Type: ports.EventObjectCreated, Sequence: 42,
		Data: ports.ObjectEvent{Bucket: "b", Key: "k", VersionID: "v", Size: 7, ETag: "e"},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"000000000000002a-0","type":"cactusfs.object.created.v1","sequence":42,` +
		`"data":{"bucket":"b","key":"k","version_id":"v","size":7,"etag":"e"}}`
	if string(encoded) != want {
		t.Errorf("event = %s\nwant    %s", encoded, want)
	}
	event.Time = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	encoded, _ = json.Marshal(event)
	if !strings.Contains(string(encoded), `"time":"2026-10-07T12:00:00Z"`) {
		t.Errorf("event with a time = %s", encoded)
	}
	removed, _ := json.Marshal(ports.NodeEvent{NodeID: "n1"})
	if string(removed) != `{"node_id":"n1"}` {
		t.Errorf("node-removed payload = %s", removed)
	}
}
