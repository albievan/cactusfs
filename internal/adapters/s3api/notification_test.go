package s3api

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// notificationBody is a configuration of three rules of both kinds, put in
// an order that is not grouped by kind: GET gives it back in that order.
const notificationBody = `<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<QueueConfiguration><Id>uploads</Id><Queue>arn:cactusfs:notify:::hook</Queue><Event>s3:ObjectCreated:*</Event>
		<Filter><S3Key><FilterRule><Name>prefix</Name><Value>logs/</Value></FilterRule><FilterRule><Name>suffix</Name><Value>.gz</Value></FilterRule></S3Key></Filter></QueueConfiguration>
	<TopicConfiguration><Id>removals</Id><Topic>arn:cactusfs:notify:::bus</Topic><Event>s3:ObjectRemoved:Delete</Event><Event>s3:ObjectRemoved:DeleteMarkerCreated</Event></TopicConfiguration>
	<QueueConfiguration><Queue>arn:cactusfs:notify:::hook</Queue><Event>s3:ObjectCreated:CompleteMultipartUpload</Event><Event>s3:ObjectRemoved:Delete</Event>
		<Filter><S3Key><FilterRule><Name>suffix</Name><Value>.tar</Value></FilterRule></S3Key></Filter></QueueConfiguration>
</NotificationConfiguration>`

// notificationWant is what GET answers for notificationBody: the ID the
// server gave the rule without one, and each rule's events in their one
// order - both removals of the second rule are its wildcard.
const notificationWant = `<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
	`<QueueConfiguration><Id>uploads</Id><Queue>arn:cactusfs:notify:::hook</Queue><Event>s3:ObjectCreated:*</Event>` +
	`<Filter><S3Key><FilterRule><Name>prefix</Name><Value>logs/</Value></FilterRule><FilterRule><Name>suffix</Name><Value>.gz</Value></FilterRule></S3Key></Filter></QueueConfiguration>` +
	`<TopicConfiguration><Id>removals</Id><Topic>arn:cactusfs:notify:::bus</Topic><Event>s3:ObjectRemoved:*</Event></TopicConfiguration>` +
	`<QueueConfiguration><Id>rule-1</Id><Queue>arn:cactusfs:notify:::hook</Queue><Event>s3:ObjectCreated:CompleteMultipartUpload</Event><Event>s3:ObjectRemoved:Delete</Event>` +
	`<Filter><S3Key><FilterRule><Name>suffix</Name><Value>.tar</Value></FilterRule></S3Key></Filter></QueueConfiguration>` +
	`</NotificationConfiguration>`

const emptyNotification = `<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></NotificationConfiguration>`

// queueRule is a configuration of one queue rule whose inside is inner.
func queueRule(inner string) []byte {
	return []byte(`<NotificationConfiguration><QueueConfiguration>` + inner + `</QueueConfiguration></NotificationConfiguration>`)
}

// putTargets defines the targets the tests name: two that admit bkt, of the
// two kinds a rule may name, one that admits another bucket only, and one
// of kind s3.
func putTargets(t *testing.T, s *testServer) {
	t.Helper()
	for _, target := range []delivery.Target{
		{Name: "hook", Kind: delivery.KindWebhook, Endpoint: "https://hooks.example.com", Buckets: []string{"bkt"}},
		{Name: "bus", Kind: delivery.KindNATS, Endpoint: "tls://nats.example.com", Subject: "cactusfs.events", Buckets: []string{"b*"}},
		{Name: "elsewhere", Kind: delivery.KindWebhook, Endpoint: "https://hooks.example.com", Buckets: []string{"other"}},
		{
			Name: "site-b", Kind: delivery.KindS3, Endpoint: "https://s3.example.com", Region: "us-east-1", Buckets: []string{"bkt"},
			// Another deployment proves itself with a key; what is sealed
			// here is never opened.
			Auth:   delivery.Auth{Mode: delivery.AuthSigV4, ID: "site-a"},
			Secret: iam.SealedSecret{KMSKeyID: "test:1", WrappedKey: []byte{1}, Nonce: make([]byte, 12), Ciphertext: []byte{2}},
		},
	} {
		target.CreatedAt, target.UpdatedAt = serverNow, serverNow
		if err := s.meta.PutTarget(context.Background(), target, audit.Record{}); err != nil {
			t.Fatalf("PutTarget %s: %v", target.Name, err)
		}
	}
}

// TestNotificationOverHTTP: the notification rules of a bucket as S3
// clients set and read them, and what is refused - with the code the S3
// reference gives, and without a change of what was set.
func TestNotificationOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	putTargets(t, s)

	// A bucket without rules has an empty configuration.
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?notification"}), http.StatusOK); string(bytes.TrimPrefix(got.body, []byte(xmlHeader))) != emptyNotification {
		t.Errorf("GetBucketNotificationConfiguration of a bucket without rules = %s", got.body)
	}
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?notification", body: []byte(notificationBody)}), http.StatusOK)
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?notification"}), http.StatusOK)
	if body := string(bytes.TrimPrefix(got.body, []byte(xmlHeader))); body != notificationWant {
		t.Errorf("GetBucketNotificationConfiguration =\n%s\nwant\n%s", body, notificationWant)
	}
	if op := s.seen.last().Operation; op != "GetBucketNotificationConfiguration" {
		t.Errorf("the operation is observed as %q", op)
	}
	// What GET gives can be put again, and is then the same.
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?notification", body: bytes.TrimPrefix(got.body, []byte(xmlHeader))}), http.StatusOK)
	if again := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?notification"}), http.StatusOK); !bytes.Equal(again.body, got.body) {
		t.Errorf("what GET gave, put again, reads as\n%s", again.body)
	}

	arn := func(target string) string { return `<Queue>arn:cactusfs:notify:::` + target + `</Queue>` }
	put := arn("hook") + `<Event>s3:ObjectCreated:Put</Event>`
	for name, tt := range map[string]struct {
		body   []byte
		status int
		code   string
	}{
		// What S3 has and is not done here: refused as not implemented,
		// never read without it.
		"a Lambda function":        {[]byte(`<NotificationConfiguration><CloudFunctionConfiguration><CloudFunction>arn:aws:lambda:us-east-1:1:function:f</CloudFunction><Event>s3:ObjectCreated:*</Event></CloudFunctionConfiguration></NotificationConfiguration>`), 501, "NotImplemented"},
		"EventBridge":              {[]byte(`<NotificationConfiguration><EventBridgeConfiguration></EventBridgeConfiguration></NotificationConfiguration>`), 501, "NotImplemented"},
		"EventBridge after a rule": {[]byte(`<NotificationConfiguration><QueueConfiguration>` + put + `</QueueConfiguration><EventBridgeConfiguration/></NotificationConfiguration>`), 501, "NotImplemented"},
		"a copy":                   {queueRule(arn("hook") + `<Event>s3:ObjectCreated:Copy</Event>`), 501, "NotImplemented"},
		"a post":                   {queueRule(arn("hook") + `<Event>s3:ObjectCreated:Post</Event>`), 501, "NotImplemented"},
		"tagging":                  {queueRule(arn("hook") + `<Event>s3:ObjectTagging:*</Event>`), 501, "NotImplemented"},
		"a restore":                {queueRule(arn("hook") + `<Event>s3:ObjectRestore:Completed</Event>`), 501, "NotImplemented"},
		"replication":              {queueRule(arn("hook") + `<Event>s3:Replication:*</Event>`), 501, "NotImplemented"},
		"an expiration":            {queueRule(arn("hook") + `<Event>s3:LifecycleExpiration:Delete</Event>`), 501, "NotImplemented"},
		"one told, one not":        {queueRule(put + `<Event>s3:ObjectAcl:Put</Event>`), 501, "NotImplemented"},
		// What S3 does not have at all.
		"an event S3 does not have":   {queueRule(arn("hook") + `<Event>s3:ObjectCreated:Rename</Event>`), 400, "InvalidArgument"},
		"an event in another case":    {queueRule(arn("hook") + `<Event>s3:objectcreated:put</Event>`), 400, "InvalidArgument"},
		"an event of nothing":         {queueRule(arn("hook") + `<Event></Event>`), 400, "InvalidArgument"},
		"no event":                    {queueRule(arn("hook")), 400, "InvalidArgument"},
		"a filter on something else":  {queueRule(put + `<Filter><S3Key><FilterRule><Name>tag</Name><Value>a</Value></FilterRule></S3Key></Filter>`), 400, "InvalidArgument"},
		"a filter in another case":    {queueRule(put + `<Filter><S3Key><FilterRule><Name>Prefix</Name><Value>a</Value></FilterRule></S3Key></Filter>`), 400, "InvalidArgument"},
		"two prefixes":                {queueRule(put + `<Filter><S3Key><FilterRule><Name>prefix</Name><Value>a</Value></FilterRule><FilterRule><Name>prefix</Name><Value>b</Value></FilterRule></S3Key></Filter>`), 400, "InvalidArgument"},
		"two suffixes":                {queueRule(put + `<Filter><S3Key><FilterRule><Name>suffix</Name><Value>a</Value></FilterRule><FilterRule><Name>suffix</Name><Value>b</Value></FilterRule></S3Key></Filter>`), 400, "InvalidArgument"},
		"an ARN of AWS":               {queueRule(`<Queue>arn:aws:sqs:us-east-1:123456789012:q</Queue><Event>s3:ObjectCreated:Put</Event>`), 400, "InvalidArgument"},
		"an address for an ARN":       {queueRule(`<Queue>https://hooks.example.com/in</Queue><Event>s3:ObjectCreated:Put</Event>`), 400, "InvalidArgument"},
		"an ARN of no name":           {queueRule(arn("") + `<Event>s3:ObjectCreated:Put</Event>`), 400, "InvalidArgument"},
		"an ARN of no target name":    {queueRule(arn("Hook!") + `<Event>s3:ObjectCreated:Put</Event>`), 400, "InvalidArgument"},
		"no ARN":                      {queueRule(`<Event>s3:ObjectCreated:Put</Event>`), 400, "InvalidArgument"},
		"a target that is not there":  {queueRule(arn("nowhere") + `<Event>s3:ObjectCreated:Put</Event>`), 400, "InvalidArgument"},
		"a target closed to bkt":      {queueRule(arn("elsewhere") + `<Event>s3:ObjectCreated:Put</Event>`), 400, "InvalidArgument"},
		"a target of kind s3":         {queueRule(arn("site-b") + `<Event>s3:ObjectCreated:Put</Event>`), 400, "InvalidArgument"},
		"an ID twice":                 {[]byte(`<NotificationConfiguration><QueueConfiguration><Id>a</Id>` + put + `</QueueConfiguration><TopicConfiguration><Id>a</Id><Topic>arn:cactusfs:notify:::hook</Topic><Event>s3:ObjectCreated:Put</Event></TopicConfiguration></NotificationConfiguration>`), 400, "InvalidArgument"},
		"an ID that is too long":      {queueRule(`<Id>` + strings.Repeat("i", 256) + `</Id>` + put), 400, "InvalidArgument"},
		"a prefix that is too long":   {queueRule(put + `<Filter><S3Key><FilterRule><Name>prefix</Name><Value>` + strings.Repeat("p", 1025) + `</Value></FilterRule></S3Key></Filter>`), 400, "InvalidArgument"},
		"too many rules":              {[]byte(`<NotificationConfiguration>` + strings.Repeat(`<QueueConfiguration>`+put+`</QueueConfiguration>`, bucket.MaxNotificationRules+1) + `</NotificationConfiguration>`), 400, "InvalidArgument"},
		"a topic that names a queue":  {[]byte(`<NotificationConfiguration><TopicConfiguration>` + put + `</TopicConfiguration></NotificationConfiguration>`), 400, "MalformedXML"},
		"a queue that names a topic":  {queueRule(`<Topic>arn:cactusfs:notify:::hook</Topic><Event>s3:ObjectCreated:Put</Event>`), 400, "MalformedXML"},
		"two queues":                  {queueRule(put + arn("bus")), 400, "MalformedXML"},
		"two filters":                 {queueRule(put + `<Filter/><Filter/>`), 400, "MalformedXML"},
		"a prefix outside a filter":   {queueRule(put + `<FilterRule><Name>prefix</Name><Value>a</Value></FilterRule>`), 400, "MalformedXML"},
		"a filter rule without S3Key": {queueRule(put + `<Filter><FilterRule><Name>prefix</Name><Value>a</Value></FilterRule></Filter>`), 400, "MalformedXML"},
		"a rule outside the document": {[]byte(`<QueueConfiguration>` + put + `</QueueConfiguration>`), 400, "MalformedXML"},
		"an element not of S3":        {queueRule(put + `<Deliver>always</Deliver>`), 400, "MalformedXML"},
		"an attribute on an element":  {queueRule(`<Queue kind="topic">arn:cactusfs:notify:::hook</Queue><Event>s3:ObjectCreated:Put</Event>`), 400, "MalformedXML"},
		"a document type":             {[]byte(`<!DOCTYPE a>` + string(queueRule(put))), 400, "MalformedXML"},
		// Text where elements belong would be dropped, and with it the
		// filter, or every rule.
		"a filter written as text":     {queueRule(put + `<Filter>prefix</Filter>`), 400, "MalformedXML"},
		"a key filter written as text": {queueRule(put + `<Filter><S3Key>logs/</S3Key></Filter>`), 400, "MalformedXML"},
		"rules written as text":        {[]byte(`<NotificationConfiguration>junk</NotificationConfiguration>`), 400, "MalformedXML"},
		"a rule written as text":       {queueRule(put + `junk`), 400, "MalformedXML"},
		// A filter rule without its value would match every key.
		"a filter rule without a value": {queueRule(put + `<Filter><S3Key><FilterRule><Name>prefix</Name></FilterRule></S3Key></Filter>`), 400, "MalformedXML"},
		"a filter rule without a name":  {queueRule(put + `<Filter><S3Key><FilterRule><Value>a</Value></FilterRule></S3Key></Filter>`), 400, "MalformedXML"},
		"a filter rule of no value":     {queueRule(put + `<Filter><S3Key><FilterRule><Name>suffix</Name><Value></Value></FilterRule></S3Key></Filter>`), 400, "InvalidArgument"},
		// Elements of another namespace are not S3's.
		"a namespace that is not S3's":    {[]byte(`<NotificationConfiguration xmlns="urn:other"><QueueConfiguration>` + put + `</QueueConfiguration></NotificationConfiguration>`), 400, "MalformedXML"},
		"an element of another namespace": {queueRule(`<x:Queue>arn:cactusfs:notify:::hook</x:Queue><Event>s3:ObjectCreated:Put</Event>`), 400, "MalformedXML"},
		"a root of another namespace":     {[]byte(`<x:NotificationConfiguration><QueueConfiguration>` + put + `</QueueConfiguration></x:NotificationConfiguration>`), 400, "MalformedXML"},
		"not XML":                         {[]byte(`notify: hook`), 400, "MalformedXML"},
		"nothing":                         {nil, 400, "MalformedXML"},
	} {
		if r := s.do(call{method: http.MethodPut, path: "/bkt?notification", body: tt.body}); r.status != tt.status || s.errorCode(r) != tt.code {
			t.Errorf("PutBucketNotificationConfiguration with %s = %d %s, want %d %s", name, r.status, s.errorCode(r), tt.status, tt.code)
		}
	}
	// None of them changed what was set.
	if after := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?notification"}), http.StatusOK); !bytes.Equal(after.body, got.body) {
		t.Errorf("a refused configuration changed the rules: %s", after.body)
	}

	// Rules send what happens to a bucket elsewhere: a body the signature
	// does not cover, or no signature, sets none, and none are read
	// without one.
	s.expectError(s.do(call{method: http.MethodPut, path: "/bkt?notification", body: []byte(emptyNotification), payloadHash: "UNSIGNED-PAYLOAD"}),
		http.StatusBadRequest, "InvalidRequest")
	for _, c := range []call{
		{method: http.MethodPut, path: "/bkt?notification", body: []byte(emptyNotification), unsigned: true},
		{method: http.MethodGet, path: "/bkt?notification", unsigned: true},
	} {
		if r := s.do(c); r.status != http.StatusForbidden {
			t.Errorf("%s ?notification without credentials = %d", c.method, r.status)
		}
	}
	s.expectError(s.do(call{method: http.MethodPut, path: "/missing?notification", body: []byte(notificationBody)}), http.StatusNotFound, "NoSuchBucket")
	s.expectError(s.do(call{method: http.MethodGet, path: "/missing?notification"}), http.StatusNotFound, "NoSuchBucket")
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?notification&acl"}), http.StatusNotImplemented, "NotImplemented")
	// S3 has no DELETE of the rules: they are removed by putting none.
	// Neither is an operation of the sub-resource.
	for _, method := range []string{http.MethodDelete, http.MethodPost} {
		s.expectError(s.do(call{method: method, path: "/bkt?notification"}), http.StatusMethodNotAllowed, "MethodNotAllowed")
		if op := s.seen.last().Operation; strings.Contains(op, "Notification") {
			t.Errorf("%s ?notification is observed as %q", method, op)
		}
	}
	// The header that would put the rules without judging their targets
	// is not implemented: they are always judged.
	s.expectError(s.do(call{method: http.MethodPut, path: "/bkt?notification", body: []byte(notificationBody), headers: map[string]string{"x-amz-skip-destination-validation": "true"}}),
		http.StatusNotImplemented, "NotImplemented")

	// An empty configuration removes the rules, and the change is in the
	// trail.
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?notification", body: []byte(`<NotificationConfiguration/>`)}), http.StatusOK)
	if r := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?notification"}), http.StatusOK); string(bytes.TrimPrefix(r.body, []byte(xmlHeader))) != emptyNotification {
		t.Errorf("after an empty configuration, GET = %s", r.body)
	}
	s.auditor.Flush(context.Background())
	records, err := s.iamStore.ListAudit(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, r := range records {
		if r.Action == string(iam.ActionPutBucketNotification) && r.Outcome == audit.Succeeded {
			changes = append(changes, r.Details["rules"]+r.Details["notification"])
		}
	}
	if want := []string{"3", "3", "removed"}; !reflect.DeepEqual(changes, want) {
		t.Errorf("the trail holds the changes %v, want %v", changes, want)
	}
}

// TestNotificationIsAuthorized: who may read or set the rules of a bucket
// is the authorizer's to say - a user who may read objects may do neither,
// and the refusal changes nothing.
func TestNotificationIsAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	putTargets(t, s)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?notification", body: []byte(notificationBody)}), http.StatusOK)
	root := s.principal()
	if _, err := s.iam.CreateUser(ctx, root, app.UserInput{Name: "reader", Roles: []string{iam.RoleReadOnly}}); err != nil {
		t.Fatal(err)
	}
	creds, _, err := s.iam.CreateAPIKey(ctx, root, app.APIKeyInput{Owner: "reader"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []call{
		{method: http.MethodGet, path: "/bkt?notification"},
		{method: http.MethodPut, path: "/bkt?notification", body: []byte(emptyNotification)},
	} {
		c.keyID, c.key = creds.AccessKeyID, []byte(creds.SecretKey)
		s.expectError(s.do(c), http.StatusForbidden, "AccessDenied")
	}
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?notification"}), http.StatusOK); string(bytes.TrimPrefix(got.body, []byte(xmlHeader))) != notificationWant {
		t.Errorf("a refused change changed the rules: %s", got.body)
	}
}

// TestNotificationWithoutTheUseCase: a node that was not given the use case
// answers 501, and reads no body for it.
func TestNotificationWithoutTheUseCase(t *testing.T) {
	t.Parallel()
	s := startTestServer(t, "", false, func(c *Config) { c.Notifications = nil })
	s.createBucket("bkt")
	for _, c := range []call{
		{method: http.MethodGet, path: "/bkt?notification"},
		{method: http.MethodPut, path: "/bkt?notification", body: []byte(notificationBody)},
	} {
		s.expectError(s.do(c), http.StatusNotImplemented, "NotImplemented")
	}
}

// TestNotificationEventNames: the event names a rule is shown with, for
// every set of kinds of change a rule can have, and back: what is shown
// reads as the same kinds.
func TestNotificationEventNames(t *testing.T) {
	t.Parallel()
	all := []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete}
	for _, tt := range []struct {
		on   []delivery.Event
		want []string
	}{
		{[]delivery.Event{delivery.EventPut}, []string{"s3:ObjectCreated:Put"}},
		{[]delivery.Event{delivery.EventComplete}, []string{"s3:ObjectCreated:CompleteMultipartUpload"}},
		{[]delivery.Event{delivery.EventMarker}, []string{"s3:ObjectRemoved:DeleteMarkerCreated"}},
		{[]delivery.Event{delivery.EventDelete}, []string{"s3:ObjectRemoved:Delete"}},
		{[]delivery.Event{delivery.EventPut, delivery.EventComplete}, []string{"s3:ObjectCreated:*"}},
		{[]delivery.Event{delivery.EventMarker, delivery.EventDelete}, []string{"s3:ObjectRemoved:*"}},
		{all, []string{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"}},
		{[]delivery.Event{delivery.EventComplete, delivery.EventDelete}, []string{"s3:ObjectCreated:CompleteMultipartUpload", "s3:ObjectRemoved:Delete"}},
		{[]delivery.Event{delivery.EventPut, delivery.EventMarker}, []string{"s3:ObjectCreated:Put", "s3:ObjectRemoved:DeleteMarkerCreated"}},
	} {
		got := notificationEventNames(tt.on)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("notificationEventNames(%v) = %v, want %v", tt.on, got, tt.want)
		}
		config, err := notificationRules(notificationConfiguration{Rules: []notificationRuleXML{{
			XMLName: xml.Name{Local: queueConfiguration}, Queue: new("arn:cactusfs:notify:::hook"), Events: got,
		}}})
		if err != nil {
			t.Fatalf("%v read back: %v", got, err)
		}
		for _, event := range all {
			if slices.Contains(config.Rules[0].On, event) != slices.Contains(tt.on, event) {
				t.Errorf("%v read back as %v", tt.on, config.Rules[0].On)
			}
		}
	}
	// Every name told is in neither list twice, and none that is told is
	// said not to be.
	for name := range notifiedEvents() {
		for _, untold := range untoldEvents() {
			if name == untold {
				t.Errorf("%s is both told and not", name)
			}
		}
	}
}

// writtenRule is a rule as the token stream of a document has it: the text
// of its Queue or Topic, decoded, and whether it has a Filter and which
// filter rules are under it, by name.
type writtenRule struct {
	arn     string
	filter  bool
	filters map[string]string
}

// writtenNotification reads a document token by token, as the decoder
// does: the local names of its elements, whether it has a directive, and
// its rules in order.
func writtenNotification(data []byte) (names []string, directive bool, rules []writtenRule) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	var open []string
	var name, value string
	for {
		token, err := decoder.Token()
		if err != nil {
			return names, directive, rules
		}
		switch t := token.(type) {
		case xml.Directive:
			directive = true
		case xml.StartElement:
			names = append(names, t.Name.Local)
			open = append(open, t.Name.Local)
			switch {
			case len(open) == 2:
				rules = append(rules, writtenRule{filters: map[string]string{}})
			case len(open) == 3 && t.Name.Local == "Filter" && len(rules) > 0:
				rules[len(rules)-1].filter = true
			case t.Name.Local == "FilterRule":
				name, value = "", ""
			}
		case xml.EndElement:
			if t.Name.Local == "FilterRule" && len(rules) > 0 {
				rules[len(rules)-1].filters[name] = value
			}
			open = open[:len(open)-1]
		case xml.CharData:
			if len(open) == 0 || len(rules) == 0 {
				continue
			}
			switch inner := open[len(open)-1]; {
			case len(open) == 3 && (inner == "Queue" || inner == "Topic"):
				rules[len(rules)-1].arn += string(t)
			case inner == "Name":
				name += string(t)
			case inner == "Value":
				value += string(t)
			}
		}
	}
}

// FuzzNotificationConfiguration: what is read as notification rules is
// what the document says, as its token stream has it - a rule for every
// rule element, none of a Lambda function or EventBridge, no directive, the
// target of every rule from its ARN, and for every rule with a filter the
// prefix and suffix its filter rules name, and none without. Rules that
// can be kept, written as GET writes them, read as the same rules.
func FuzzNotificationConfiguration(f *testing.F) {
	f.Add([]byte(notificationBody))
	f.Add([]byte(notificationWant))
	f.Add([]byte(emptyNotification))
	f.Add(queueRule(`<Queue>arn:cactusfs:notify:::hook</Queue><Event>s3:ObjectCreated:Put</Event><FilterRule><Name>prefix</Name><Value>a</Value></FilterRule>`))
	f.Add([]byte(`<NotificationConfiguration><CloudFunctionConfiguration><Event>s3:ObjectCreated:*</Event></CloudFunctionConfiguration></NotificationConfiguration>`))
	f.Add([]byte(`<!DOCTYPE a [<!ENTITY b "c">]><NotificationConfiguration/>`))
	f.Add(queueRule(`<x:Queue xmlns:x="urn:x">arn:cactusfs:notify:::hook</x:Queue><Event>s3:ObjectCreated:Put</Event>`))
	f.Add(queueRule(`<Queue>arn:cactusfs:notify:::hook</Queue><Event>s3:ObjectCreated:Put</Event><Filter><S3Key>logs/</S3Key></Filter>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var in notificationConfiguration
		if err := decodeShapedXML(bytes.NewReader(data), maxConfigBody, &in, notificationShape(), unsupportedNotification()...); err != nil {
			return
		}
		names, directive, written := writtenNotification(data)
		if directive || slices.Contains(names, "CloudFunctionConfiguration") || slices.Contains(names, "EventBridgeConfiguration") {
			t.Fatalf("decoded %q", data)
		}
		if len(in.Rules) != len(written) {
			t.Fatalf("%d rules decoded from %d rule elements: %q", len(in.Rules), len(written), data)
		}
		config, err := notificationRules(in)
		if err != nil {
			return
		}
		for i, r := range config.Rules {
			if target, ok := strings.CutPrefix(written[i].arn, notifyARNPrefix); !ok || target != r.Target {
				t.Fatalf("rule %d: target %q read from the ARN %q", i, r.Target, written[i].arn)
			}
			read := map[string]string{}
			if r.Prefix != "" {
				read["prefix"] = r.Prefix
			}
			if r.Suffix != "" {
				read["suffix"] = r.Suffix
			}
			if !written[i].filter {
				if len(read) != 0 {
					t.Fatalf("rule %d has no filter and was read with %v: %q", i, read, data)
				}
				continue
			}
			if !reflect.DeepEqual(read, written[i].filters) {
				t.Fatalf("rule %d: filter rules %v read as %v: %q", i, written[i].filters, read, data)
			}
		}
		if config.Validate() != nil {
			return
		}
		document, err := config.Document()
		if err != nil {
			t.Fatalf("rules that validate have no document: %v", err)
		}
		kept, err := bucket.ParseNotificationConfig(document)
		if err != nil {
			t.Fatalf("the document of rules does not parse: %v", err)
		}
		out, err := xml.Marshal(notificationConfigurationOf(kept))
		if err != nil {
			t.Fatal(err)
		}
		var again notificationConfiguration
		if err := decodeShapedXML(bytes.NewReader(out), maxConfigBody, &again, notificationShape(), unsupportedNotification()...); err != nil {
			t.Fatalf("what GET writes does not read: %v\n%s", err, out)
		}
		back, err := notificationRules(again)
		if err != nil {
			t.Fatalf("what GET writes is refused: %v\n%s", err, out)
		}
		if !reflect.DeepEqual(back, kept) {
			t.Fatalf("written and read again\n%+v\nwant\n%+v", back, kept)
		}
	})
}
