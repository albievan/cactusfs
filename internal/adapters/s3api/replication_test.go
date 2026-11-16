package s3api

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// replicationBody is a configuration of two rules of the later form of the
// schema - the second without an ID or a priority.
const replicationBody = `<ReplicationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Role>arn:cactusfs:replication:::site-b</Role>
	<Rule><ID>logs</ID><Status>Enabled</Status><Priority>2</Priority><Filter><Prefix>logs/</Prefix></Filter>
		<DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication><Destination><Bucket>arn:aws:s3:::copy</Bucket></Destination></Rule>
	<Rule><Status>Disabled</Status><Filter><Prefix>tmp/</Prefix></Filter>
		<DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication><Destination><Bucket>arn:aws:s3:::copy</Bucket></Destination></Rule>
</ReplicationConfiguration>`

// firstFormBody is a configuration of two rules of the first form of the
// schema: a prefix in the rule, no filter and no priority - the first
// without an ID or a word on delete markers.
const firstFormBody = `<ReplicationConfiguration>
	<Role>arn:cactusfs:replication:::site-b</Role>
	<Rule><Status>Enabled</Status><Prefix>logs/</Prefix><Destination><Bucket>arn:aws:s3:::copy</Bucket></Destination></Rule>
	<Rule><ID>tmp</ID><Status>Disabled</Status><Prefix>tmp/</Prefix>
		<DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication><Destination><Bucket>arn:aws:s3:::copy</Bucket></Destination></Rule>
</ReplicationConfiguration>`

// firstFormWant is what GET answers for firstFormBody: in the later form,
// the delete markers of the rule that said nothing of them not copied.
const firstFormWant = `<ReplicationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
	`<Role>arn:cactusfs:replication:::site-b</Role>` +
	`<Rule><ID>rule-1</ID><Status>Enabled</Status><Priority>0</Priority><Filter><Prefix>logs/</Prefix></Filter>` +
	`<DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication><Destination><Bucket>arn:aws:s3:::copy</Bucket></Destination></Rule>` +
	`<Rule><ID>tmp</ID><Status>Disabled</Status><Priority>0</Priority><Filter><Prefix>tmp/</Prefix></Filter>` +
	`<DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication><Destination><Bucket>arn:aws:s3:::copy</Bucket></Destination></Rule>` +
	`</ReplicationConfiguration>`

// replicationWant is what GET answers for replicationBody: the ID the
// server gave the rule without one, and every rule in the later form, with
// its filter, its priority and its delete markers said.
const replicationWant = `<ReplicationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
	`<Role>arn:cactusfs:replication:::site-b</Role>` +
	`<Rule><ID>logs</ID><Status>Enabled</Status><Priority>2</Priority><Filter><Prefix>logs/</Prefix></Filter>` +
	`<DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication><Destination><Bucket>arn:aws:s3:::copy</Bucket></Destination></Rule>` +
	`<Rule><ID>rule-1</ID><Status>Disabled</Status><Priority>0</Priority><Filter><Prefix>tmp/</Prefix></Filter>` +
	`<DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication><Destination><Bucket>arn:aws:s3:::copy</Bucket></Destination></Rule>` +
	`</ReplicationConfiguration>`

// replicationOf is a configuration through the role given whose rules are
// inner.
func replicationOf(role, inner string) []byte {
	return []byte(`<ReplicationConfiguration><Role>` + role + `</Role>` + inner + `</ReplicationConfiguration>`)
}

// replicationRule is a configuration through site-b of one rule whose
// inside is inner.
func replicationRule(inner string) []byte {
	return replicationOf("arn:cactusfs:replication:::site-b", `<Rule>`+inner+`</Rule>`)
}

// toCopy is the destination of every rule of the tests.
const toCopy = `<Destination><Bucket>arn:aws:s3:::copy</Bucket></Destination>`

// putReplicationBuckets makes the buckets of the tests: bkt keeps versions,
// plain never did, suspended does no longer, copies is the replica of
// another deployment. The targets are putTargets' and site-c, another
// deployment open to the bucket "other" only.
func putReplicationBuckets(t *testing.T, s *testServer) {
	t.Helper()
	ctx := context.Background()
	for name, versioning := range map[string]string{"bkt": "Enabled", "plain": "", "suspended": "Suspended", "copies": "Enabled"} {
		s.createBucket(name)
		if versioning != "" {
			s.mustStatus(s.do(call{
				method: http.MethodPut, path: "/" + name + "?versioning",
				body: []byte(`<VersioningConfiguration><Status>` + versioning + `</Status></VersioningConfiguration>`),
			}), http.StatusOK)
		}
	}
	if err := s.meta.SetBucketReplica(ctx, "copies", "site-z", audit.Record{}); err != nil {
		t.Fatalf("SetBucketReplica: %v", err)
	}
	putTargets(t, s)
	site := delivery.Target{
		Name: "site-c", Kind: delivery.KindS3, Endpoint: "https://s3.example.com", Region: "us-east-1", Buckets: []string{"other"},
		Auth:   delivery.Auth{Mode: delivery.AuthSigV4, ID: "site-a"},
		Secret: iam.SealedSecret{KMSKeyID: "test:1", WrappedKey: []byte{1}, Nonce: make([]byte, 12), Ciphertext: []byte{2}},
	}
	site.CreatedAt, site.UpdatedAt = serverNow, serverNow
	if err := s.meta.PutTarget(ctx, site, audit.Record{}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
}

// TestReplicationOverHTTP: the replication configuration of a bucket as S3
// clients set, read and remove it (ADR-0026, "Configuration"), and what is
// refused - with the status and code the ADR gives, and without a change of
// what was set.
func TestReplicationOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	putReplicationBuckets(t, s)

	// A bucket without a configuration has none, as S3 says it.
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?replication"}), http.StatusNotFound, "ReplicationConfigurationNotFoundError")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?replication", body: []byte(replicationBody)}), http.StatusOK)
	if op := s.seen.last().Operation; op != "PutBucketReplication" {
		t.Errorf("the put is observed as %q", op)
	}
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?replication"}), http.StatusOK)
	if body := string(bytes.TrimPrefix(got.body, []byte(xmlHeader))); body != replicationWant {
		t.Errorf("GetBucketReplication =\n%s\nwant\n%s", body, replicationWant)
	}
	if op := s.seen.last().Operation; op != "GetBucketReplication" {
		t.Errorf("the read is observed as %q", op)
	}
	// What GET gives can be put again, and is then the same.
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?replication", body: bytes.TrimPrefix(got.body, []byte(xmlHeader))}), http.StatusOK)
	if again := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?replication"}), http.StatusOK); !bytes.Equal(again.body, got.body) {
		t.Errorf("what GET gave, put again, reads as\n%s", again.body)
	}

	enabled := `<Status>Enabled</Status>`
	markers := `<DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication>`
	every := `<Filter></Filter>`
	// bare is a rule of the later form without its filter; rule is one
	// for every key.
	bare := enabled + toCopy + markers
	rule := bare + every
	site := "arn:cactusfs:replication:::site-b"
	rules := func(inner ...string) []byte {
		return replicationOf(site, `<Rule>`+strings.Join(inner, `</Rule><Rule>`)+`</Rule>`)
	}
	tooMany := make([]string, bucket.MaxReplicationRules+1)
	for i := range tooMany {
		tooMany[i] = rule + `<Priority>` + strconv.Itoa(i) + `</Priority>`
	}
	for name, tt := range map[string]struct {
		bucket string
		body   []byte
		status int
		code   string
	}{
		// What S3 has and is not done here: refused as not implemented,
		// never read without it.
		"a filter on a tag":              {body: replicationRule(bare + `<Filter><Tag><Key>a</Key><Value>b</Value></Tag></Filter>`), status: 501, code: "NotImplemented"},
		"a filter of several conditions": {body: replicationRule(bare + `<Filter><And><Prefix>a</Prefix></And></Filter>`), status: 501, code: "NotImplemented"},
		"objects encrypted with AWS KMS": {body: replicationRule(rule + `<SourceSelectionCriteria><SseKmsEncryptedObjects><Status>Enabled</Status></SseKmsEncryptedObjects></SourceSelectionCriteria>`), status: 501, code: "NotImplemented"},
		"the objects already there":      {body: replicationRule(rule + `<ExistingObjectReplication><Status>Enabled</Status></ExistingObjectReplication>`), status: 501, code: "NotImplemented"},
		"a storage class":                {body: replicationRule(enabled + markers + every + `<Destination><Bucket>arn:aws:s3:::copy</Bucket><StorageClass>GLACIER</StorageClass></Destination>`), status: 501, code: "NotImplemented"},
		"encryption at the destination":  {body: replicationRule(enabled + markers + every + `<Destination><Bucket>arn:aws:s3:::copy</Bucket><EncryptionConfiguration><ReplicaKmsKeyID>k</ReplicaKmsKeyID></EncryptionConfiguration></Destination>`), status: 501, code: "NotImplemented"},
		"a time of replication":          {body: replicationRule(enabled + markers + every + `<Destination><Bucket>arn:aws:s3:::copy</Bucket><ReplicationTime><Status>Enabled</Status></ReplicationTime></Destination>`), status: 501, code: "NotImplemented"},
		"metrics":                        {body: replicationRule(enabled + markers + every + `<Destination><Bucket>arn:aws:s3:::copy</Bucket><Metrics><Status>Enabled</Status></Metrics></Destination>`), status: 501, code: "NotImplemented"},
		"an account":                     {body: replicationRule(enabled + markers + every + `<Destination><Bucket>arn:aws:s3:::copy</Bucket><Account>123456789012</Account></Destination>`), status: 501, code: "NotImplemented"},
		"an owner at the destination":    {body: replicationRule(enabled + markers + every + `<Destination><Bucket>arn:aws:s3:::copy</Bucket><AccessControlTranslation><Owner>Destination</Owner></AccessControlTranslation></Destination>`), status: 501, code: "NotImplemented"},
		"two destination buckets": {
			body: rules(rule, enabled+markers+`<Filter><Prefix>a</Prefix></Filter><Priority>1</Priority><Destination><Bucket>arn:aws:s3:::other</Bucket></Destination>`), status: 501, code: "NotImplemented",
		},
		"a tag in a later rule": {body: rules(rule, bare+`<Filter><Tag/></Filter>`), status: 501, code: "NotImplemented"},
		// The role names a target; the destination a bucket.
		"a role of AWS":                  {body: replicationOf("arn:aws:iam::123456789012:role/replication", `<Rule>`+rule+`</Rule>`), status: 400, code: "InvalidArgument"},
		"a role of no target name":       {body: replicationOf("arn:cactusfs:replication:::", `<Rule>`+rule+`</Rule>`), status: 400, code: "InvalidArgument"},
		"a role of a name that is none":  {body: replicationOf("arn:cactusfs:replication:::Site!", `<Rule>`+rule+`</Rule>`), status: 400, code: "InvalidArgument"},
		"a role of notifications":        {body: replicationOf("arn:cactusfs:notify:::site-b", `<Rule>`+rule+`</Rule>`), status: 400, code: "InvalidArgument"},
		"a target that is not there":     {body: replicationOf("arn:cactusfs:replication:::nowhere", `<Rule>`+rule+`</Rule>`), status: 400, code: "InvalidArgument"},
		"a target closed to the bucket":  {body: replicationOf("arn:cactusfs:replication:::site-c", `<Rule>`+rule+`</Rule>`), status: 400, code: "InvalidArgument"},
		"a webhook":                      {body: replicationOf("arn:cactusfs:replication:::hook", `<Rule>`+rule+`</Rule>`), status: 400, code: "InvalidArgument"},
		"a destination that is no ARN":   {body: replicationRule(enabled + markers + every + `<Destination><Bucket>copy</Bucket></Destination>`), status: 400, code: "InvalidArgument"},
		"a destination of no name":       {body: replicationRule(enabled + markers + every + `<Destination><Bucket>arn:aws:s3:::No_Bucket</Bucket></Destination>`), status: 400, code: "InvalidArgument"},
		"a destination of nothing":       {body: replicationRule(enabled + markers + every + `<Destination><Bucket>arn:aws:s3:::</Bucket></Destination>`), status: 400, code: "InvalidArgument"},
		"a prefix and a filter":          {body: replicationRule(bare + `<Prefix>a</Prefix><Filter><Prefix>a</Prefix></Filter>`), status: 400, code: "InvalidArgument"},
		"a negative priority":            {body: replicationRule(rule + `<Priority>-1</Priority>`), status: 400, code: "InvalidArgument"},
		"a priority with a sign":         {body: replicationRule(rule + `<Priority>+1</Priority>`), status: 400, code: "InvalidArgument"},
		"a priority with a leading zero": {body: replicationRule(rule + `<Priority>01</Priority>`), status: 400, code: "InvalidArgument"},
		"a priority with a space":        {body: replicationRule(rule + `<Priority> 1</Priority>`), status: 400, code: "InvalidArgument"},
		"a priority of nothing":          {body: replicationRule(rule + `<Priority></Priority>`), status: 400, code: "InvalidArgument"},
		"a priority that is no number":   {body: replicationRule(rule + `<Priority>first</Priority>`), status: 400, code: "InvalidArgument"},
		"a priority past its range":      {body: replicationRule(rule + `<Priority>2147483648</Priority>`), status: 400, code: "InvalidArgument"},
		"a priority far past its range":  {body: replicationRule(rule + `<Priority>99999999999999999999</Priority>`), status: 400, code: "InvalidArgument"},
		"an ID of nothing":               {body: replicationRule(rule + `<ID></ID>`), status: 400, code: "InvalidArgument"},
		"an ID that is too long":         {body: replicationRule(rule + `<ID>` + strings.Repeat("i", 256) + `</ID>`), status: 400, code: "InvalidArgument"},
		"an ID twice": {
			body: rules(`<ID>a</ID><Priority>1</Priority>`+rule, `<ID>a</ID><Priority>2</Priority>`+rule), status: 400, code: "InvalidArgument",
		},
		"a prefix that is too long": {body: replicationRule(bare + `<Filter><Prefix>` + strings.Repeat("p", 1025) + `</Prefix></Filter>`), status: 400, code: "InvalidArgument"},
		"too many rules":            {body: rules(tooMany...), status: 400, code: "InvalidArgument"},
		// Two enabled rules of one priority that some key matches both.
		"two rules of the first form that overlap": {
			body: rules(enabled+toCopy+`<Prefix>logs/</Prefix>`, enabled+toCopy+`<Prefix>logs/2026/</Prefix>`), status: 400, code: "InvalidArgument",
		},
		"every key and some keys at one priority": {
			body: rules(rule+`<Priority>1</Priority>`, bare+`<Filter><Prefix>a</Prefix></Filter><Priority>1</Priority>`), status: 400, code: "InvalidArgument",
		},
		// What the bucket is decides.
		"a bucket that never kept versions": {bucket: "plain", body: []byte(replicationBody), status: 400, code: "InvalidRequest"},
		"a bucket whose versions stopped":   {bucket: "suspended", body: []byte(replicationBody), status: 400, code: "InvalidRequest"},
		"a replica":                         {bucket: "copies", body: []byte(replicationBody), status: 409, code: "InvalidBucketState"},
		"a bucket that is not there":        {bucket: "missing", body: []byte(replicationBody), status: 404, code: "NoSuchBucket"},
		// What the schema requires, and what it does not have.
		"no role":                      {body: []byte(`<ReplicationConfiguration><Rule>` + rule + `</Rule></ReplicationConfiguration>`), status: 400, code: "MalformedXML"},
		"no rule":                      {body: replicationOf(site, ""), status: 400, code: "MalformedXML"},
		"two roles":                    {body: replicationOf(site+`</Role><Role>`+site, `<Rule>`+rule+`</Rule>`), status: 400, code: "MalformedXML"},
		"no status":                    {body: replicationRule(toCopy + markers + every), status: 400, code: "MalformedXML"},
		"a status in another case":     {body: replicationRule(`<Status>enabled</Status>` + toCopy + markers + every), status: 400, code: "MalformedXML"},
		"a status of nothing":          {body: replicationRule(`<Status></Status>` + toCopy + markers + every), status: 400, code: "MalformedXML"},
		"two statuses":                 {body: replicationRule(rule + enabled), status: 400, code: "MalformedXML"},
		"markers neither on nor off":   {body: replicationRule(enabled + toCopy + every + `<DeleteMarkerReplication><Status>On</Status></DeleteMarkerReplication>`), status: 400, code: "MalformedXML"},
		"markers of no status":         {body: replicationRule(enabled + toCopy + every + `<DeleteMarkerReplication></DeleteMarkerReplication>`), status: 400, code: "MalformedXML"},
		"no destination":               {body: replicationRule(enabled + markers + every), status: 400, code: "MalformedXML"},
		"a destination without bucket": {body: replicationRule(enabled + markers + every + `<Destination></Destination>`), status: 400, code: "MalformedXML"},
		"two destinations":             {body: replicationRule(rule + toCopy), status: 400, code: "MalformedXML"},
		// Which keys a rule is for is said, in one form of the schema for
		// every rule; the later form says what becomes of delete markers.
		"neither a prefix nor a filter":   {body: replicationRule(bare), status: 400, code: "MalformedXML"},
		"the later form without markers":  {body: replicationRule(enabled + toCopy + every), status: 400, code: "MalformedXML"},
		"the first form after the later":  {body: rules(rule, enabled+toCopy+`<Prefix>logs/</Prefix>`), status: 400, code: "MalformedXML"},
		"the later form after the first":  {body: rules(enabled+toCopy+`<Prefix>logs/</Prefix>`, rule), status: 400, code: "MalformedXML"},
		"a bucket in the filter":          {body: replicationRule(bare + `<Filter><Bucket>arn:aws:s3:::copy</Bucket></Filter>`), status: 400, code: "MalformedXML"},
		"a prefix in the destination":     {body: replicationRule(enabled + markers + every + `<Destination><Bucket>arn:aws:s3:::copy</Bucket><Prefix>a</Prefix></Destination>`), status: 400, code: "MalformedXML"},
		"a prefix outside a rule":         {body: replicationOf(site, `<Prefix>logs/</Prefix><Rule>`+rule+`</Rule>`), status: 400, code: "MalformedXML"},
		"a rule outside the document":     {body: []byte(`<Rule>` + rule + `</Rule>`), status: 400, code: "MalformedXML"},
		"an element not of S3":            {body: replicationRule(rule + `<Deliver>always</Deliver>`), status: 400, code: "MalformedXML"},
		"an attribute on an element":      {body: replicationRule(`<Status kind="x">Enabled</Status>` + toCopy + markers + every), status: 400, code: "MalformedXML"},
		"a document type":                 {body: []byte(`<!DOCTYPE a>` + string(replicationRule(rule))), status: 400, code: "MalformedXML"},
		"a filter written as text":        {body: replicationRule(bare + `<Filter>logs/</Filter>`), status: 400, code: "MalformedXML"},
		"a destination written as text":   {body: replicationRule(enabled + markers + every + `<Destination>arn:aws:s3:::copy</Destination>`), status: 400, code: "MalformedXML"},
		"rules written as text":           {body: replicationOf(site, `junk`), status: 400, code: "MalformedXML"},
		"a namespace that is not S3's":    {body: []byte(`<ReplicationConfiguration xmlns="urn:other"><Role>` + site + `</Role><Rule>` + rule + `</Rule></ReplicationConfiguration>`), status: 400, code: "MalformedXML"},
		"an element of another namespace": {body: replicationRule(`<x:Status xmlns:x="urn:x">Enabled</x:Status>` + toCopy + markers + every), status: 400, code: "MalformedXML"},
		"not XML":                         {body: []byte(`replicate: everything`), status: 400, code: "MalformedXML"},
		"nothing":                         {body: nil, status: 400, code: "MalformedXML"},
	} {
		if tt.bucket == "" {
			tt.bucket = "bkt"
		}
		if r := s.do(call{method: http.MethodPut, path: "/" + tt.bucket + "?replication", body: tt.body}); r.status != tt.status || s.errorCode(r) != tt.code {
			t.Errorf("PutBucketReplication with %s = %d %s, want %d %s", name, r.status, s.errorCode(r), tt.status, tt.code)
		}
	}
	// None of them changed what was set, nor set one where there was none.
	if after := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?replication"}), http.StatusOK); !bytes.Equal(after.body, got.body) {
		t.Errorf("a refused configuration changed it: %s", after.body)
	}
	for _, name := range []string{"plain", "suspended", "copies"} {
		s.expectError(s.do(call{method: http.MethodGet, path: "/" + name + "?replication"}), http.StatusNotFound, "ReplicationConfigurationNotFoundError")
	}

	// While it is there, the bucket keeps versions.
	s.expectError(s.do(call{
		method: http.MethodPut, path: "/bkt?versioning", body: []byte(`<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`),
	}), http.StatusConflict, "InvalidBucketState")

	// A configuration sends versions elsewhere: a body the signature does
	// not cover, or no signature, sets none, and none is read or removed
	// without one.
	s.expectError(s.do(call{method: http.MethodPut, path: "/bkt?replication", body: []byte(replicationBody), payloadHash: "UNSIGNED-PAYLOAD"}),
		http.StatusBadRequest, "InvalidRequest")
	for _, c := range []call{
		{method: http.MethodPut, path: "/bkt?replication", body: []byte(replicationBody), unsigned: true},
		{method: http.MethodGet, path: "/bkt?replication", unsigned: true},
		{method: http.MethodDelete, path: "/bkt?replication", unsigned: true},
	} {
		if r := s.do(c); r.status != http.StatusForbidden {
			t.Errorf("%s ?replication without credentials = %d", c.method, r.status)
		}
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/missing?replication"}), http.StatusNotFound, "NoSuchBucket")
	s.expectError(s.do(call{method: http.MethodDelete, path: "/missing?replication"}), http.StatusNotFound, "NoSuchBucket")
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?replication&acl"}), http.StatusNotImplemented, "NotImplemented")
	// The token of a bucket with Object Lock is not implemented.
	s.expectError(s.do(call{method: http.MethodPut, path: "/bkt?replication", body: []byte(replicationBody), headers: map[string]string{"x-amz-bucket-object-lock-token": "t"}}),
		http.StatusNotImplemented, "NotImplemented")
	s.expectError(s.do(call{method: http.MethodPost, path: "/bkt?replication"}), http.StatusMethodNotAllowed, "MethodNotAllowed")
	if op := s.seen.last().Operation; strings.Contains(op, "Replication") {
		t.Errorf("POST ?replication is observed as %q", op)
	}

	// A configuration of the first form is read as S3 reads it, and shown
	// in the later form.
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?replication", body: []byte(firstFormBody)}), http.StatusOK)
	if r := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?replication"}), http.StatusOK); string(bytes.TrimPrefix(r.body, []byte(xmlHeader))) != firstFormWant {
		t.Errorf("GetBucketReplication of the first form =\n%s\nwant\n%s", r.body, firstFormWant)
	}

	// DELETE removes it, and removing none is no error.
	for range 2 {
		s.mustStatus(s.do(call{method: http.MethodDelete, path: "/bkt?replication"}), http.StatusNoContent)
		if op := s.seen.last().Operation; op != "DeleteBucketReplication" {
			t.Errorf("the removal is observed as %q", op)
		}
		s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?replication"}), http.StatusNotFound, "ReplicationConfigurationNotFoundError")
	}

	// Each change is in the trail, with what it named.
	s.auditor.Flush(context.Background())
	records, err := s.iamStore.ListAudit(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, r := range records {
		if r.Action == string(iam.ActionPutReplication) && r.Outcome == audit.Succeeded {
			changes = append(changes, r.Details["target"]+r.Details["destination"]+r.Details["rules"]+r.Details["replication"])
		}
	}
	if want := []string{"site-bcopy2", "site-bcopy2", "site-bcopy2", "removed", "removed"}; !reflect.DeepEqual(changes, want) {
		t.Errorf("the trail holds the changes %v, want %v", changes, want)
	}
}

// TestReplicationIsAuthorized: who may read, set or remove the
// configuration of a bucket is the authorizer's to say - a user who may
// read objects may do none of it, and the refusal changes nothing.
func TestReplicationIsAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestServer(t, "")
	putReplicationBuckets(t, s)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?replication", body: []byte(replicationBody)}), http.StatusOK)
	root := s.principal()
	if _, err := s.iam.CreateUser(ctx, root, app.UserInput{Name: "reader", Roles: []string{iam.RoleReadOnly}}); err != nil {
		t.Fatal(err)
	}
	creds, _, err := s.iam.CreateAPIKey(ctx, root, app.APIKeyInput{Owner: "reader"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []call{
		{method: http.MethodGet, path: "/bkt?replication"},
		{method: http.MethodPut, path: "/bkt?replication", body: []byte(replicationBody)},
		{method: http.MethodDelete, path: "/bkt?replication"},
	} {
		c.keyID, c.key = creds.AccessKeyID, []byte(creds.SecretKey)
		s.expectError(s.do(c), http.StatusForbidden, "AccessDenied")
	}
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?replication"}), http.StatusOK); string(bytes.TrimPrefix(got.body, []byte(xmlHeader))) != replicationWant {
		t.Errorf("a refused change changed the configuration: %s", got.body)
	}
	s.auditor.Flush(ctx)
	records, err := s.iamStore.ListAudit(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	denied := map[string]int{}
	for _, r := range records {
		if r.Outcome == audit.Denied && r.Actor == "user/reader" {
			denied[r.Action]++
		}
	}
	if want := map[string]int{string(iam.ActionGetReplication): 1, string(iam.ActionPutReplication): 2}; !reflect.DeepEqual(denied, want) {
		t.Errorf("the trail holds the denials %v, want %v", denied, want)
	}
}

// TestReplicationWithoutTheUseCase: a node that was not given the use case
// answers 501, and reads no body for it.
func TestReplicationWithoutTheUseCase(t *testing.T) {
	t.Parallel()
	s := startTestServer(t, "", false, func(c *Config) { c.Replication = nil })
	s.createBucket("bkt")
	for _, c := range []call{
		{method: http.MethodGet, path: "/bkt?replication"},
		{method: http.MethodPut, path: "/bkt?replication", body: []byte(replicationBody)},
		{method: http.MethodDelete, path: "/bkt?replication"},
	} {
		s.expectError(s.do(c), http.StatusNotImplemented, "NotImplemented")
	}
}

// TestReplicationPriority: a priority is decimal digits without a sign, a
// space or a leading zero, from 0 to 2^31 - 1.
func TestReplicationPriority(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]int{"0": 0, "1": 1, "10": 10, "2147483647": 2147483647} {
		if got, ok := replicationPriority(text); !ok || got != want {
			t.Errorf("replicationPriority(%q) = %d, %t", text, got, ok)
		}
	}
	for _, text := range []string{"", "-0", "-1", "+1", "01", "00", " 1", "1 ", "1.0", "1e3", "2147483648", "4294967296", "99999999999", "٣", "0x10"} {
		if got, ok := replicationPriority(text); ok {
			t.Errorf("replicationPriority(%q) = %d, want refused", text, got)
		}
	}
}

// writtenReplication is a configuration as the token stream of a document
// has it: the decoded text of its Role, whether there is one, and for each
// rule the decoded text of each leaf by its path under the rule
// ("Filter/Prefix", "Destination/Bucket", ...) and the paths of its
// elements.
type writtenReplication struct {
	role     string
	hasRole  bool
	rules    []map[string]string
	elements []map[string]bool
}

// readReplicationTokens reads a document token by token, as the decoder
// does: the local names of its elements, whether it has a directive, and
// what it writes.
func readReplicationTokens(data []byte) (names []string, directive bool, w writtenReplication) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	var open []string
	for {
		token, err := decoder.Token()
		if err != nil {
			return names, directive, w
		}
		switch t := token.(type) {
		case xml.Directive:
			directive = true
		case xml.StartElement:
			names = append(names, t.Name.Local)
			open = append(open, t.Name.Local)
			switch {
			case len(open) == 2 && t.Name.Local == "Rule":
				w.rules = append(w.rules, map[string]string{})
				w.elements = append(w.elements, map[string]bool{})
			case len(open) == 2 && t.Name.Local == "Role":
				w.hasRole = true
			case len(open) > 2 && len(w.rules) > 0:
				w.elements[len(w.elements)-1][strings.Join(open[2:], "/")] = true
			}
		case xml.EndElement:
			open = open[:len(open)-1]
		case xml.CharData:
			switch {
			case len(open) == 2 && open[1] == "Role":
				w.role += string(t)
			case len(open) > 2 && len(w.rules) > 0:
				w.rules[len(w.rules)-1][strings.Join(open[2:], "/")] += string(t)
			}
		}
	}
}

// FuzzReplicationConfiguration: what is read as a replication configuration
// is what the document says, as its token stream has it - no directive and
// none of the elements that are not implemented; a rule for every rule
// element; the target from the role's ARN; each rule's status, delete
// markers, ID, priority and destination bucket from their elements, and its
// prefix from the one place it is written - and nothing from nowhere. A
// configuration that can be kept, written as GET writes it, reads as the
// same configuration.
func FuzzReplicationConfiguration(f *testing.F) {
	f.Add([]byte(replicationBody))
	f.Add([]byte(replicationWant))
	f.Add(replicationRule(`<Status>Enabled</Status>` + toCopy + `<Filter><Tag/></Filter>`))
	f.Add(replicationRule(`<Status>Enabled</Status>` + toCopy + `<Prefix>a</Prefix><Filter><Prefix>b</Prefix></Filter>`))
	f.Add(replicationRule(`<Status>Enabled</Status>` + toCopy + `<Priority>2147483647</Priority><ID>x</ID>`))
	f.Add(replicationRule(`<Status>Enabled</Status><Destination>arn:aws:s3:::copy</Destination>`))
	f.Add([]byte(`<!DOCTYPE a [<!ENTITY b "c">]><ReplicationConfiguration/>`))
	f.Add(replicationRule(`<x:Status xmlns:x="urn:x">Enabled</x:Status>` + toCopy))
	f.Add(replicationOf("arn:cactusfs:replication:::site-b", `<Rule><Status>Enabled</Status>`+toCopy+`</Rule><Rule><Status>Disabled</Status><Destination><Bucket>arn:aws:s3:::other</Bucket></Destination></Rule>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var in replicationConfiguration
		if err := decodeShapedXML(bytes.NewReader(data), maxConfigBody, &in, replicationShape(), unsupportedReplication()...); err != nil {
			return
		}
		names, directive, written := readReplicationTokens(data)
		if directive {
			t.Fatalf("decoded a document with a directive: %q", data)
		}
		for _, name := range unsupportedReplication() {
			if slices.Contains(names, name) {
				t.Fatalf("decoded a document with %s: %q", name, data)
			}
		}
		if len(in.Rules) != len(written.rules) {
			t.Fatalf("%d rules decoded from %d rule elements: %q", len(in.Rules), len(written.rules), data)
		}
		config, err := replicationRules(in)
		if err != nil {
			return
		}
		if target, ok := strings.CutPrefix(written.role, replicationRolePrefix); !written.hasRole || !ok || target != config.Target {
			t.Fatalf("target %q read from the role %q: %q", config.Target, written.role, data)
		}
		for i, elements := range written.elements {
			if elements["Prefix"] == elements["Filter"] || elements["Filter"] != written.elements[0]["Filter"] ||
				(elements["Filter"] && !elements["DeleteMarkerReplication"]) {
				t.Fatalf("rule %d is of no form, or not of the form of the first, or of the later without its markers: %q", i, data)
			}
		}
		for i, r := range config.Rules {
			leaves, elements := written.rules[i], written.elements[i]
			if want := leaves["Status"] == statusEnabled; r.Enabled != want || (leaves["Status"] != statusEnabled && leaves["Status"] != statusDisabled) {
				t.Fatalf("rule %d: status %q read as %t: %q", i, leaves["Status"], r.Enabled, data)
			}
			if r.Markers != (leaves["DeleteMarkerReplication/Status"] == statusEnabled) {
				t.Fatalf("rule %d: delete markers %q read as %t: %q", i, leaves["DeleteMarkerReplication/Status"], r.Markers, data)
			}
			if destination, ok := strings.CutPrefix(leaves["Destination/Bucket"], destinationPrefix); !ok || destination != config.Bucket {
				t.Fatalf("rule %d: destination %q read as %q: %q", i, leaves["Destination/Bucket"], config.Bucket, data)
			}
			if r.ID != leaves["ID"] {
				t.Fatalf("rule %d: ID %q read as %q: %q", i, leaves["ID"], r.ID, data)
			}
			if want := leaves["Priority"]; (want == "" && r.Priority != 0) || (want != "" && strconv.Itoa(r.Priority) != want) {
				t.Fatalf("rule %d: priority %q read as %d: %q", i, want, r.Priority, data)
			}
			prefix, filtered := leaves["Prefix"], elements["Filter"]
			if filtered {
				prefix = leaves["Filter/Prefix"]
				if elements["Prefix"] {
					t.Fatalf("rule %d: a prefix and a filter read as %q: %q", i, r.Prefix, data)
				}
			}
			if r.Prefix != prefix {
				t.Fatalf("rule %d: prefix %q read as %q: %q", i, prefix, r.Prefix, data)
			}
		}
		// The use case gives a rule without an ID one of its own; so does
		// this, to see the rest round trip.
		for i := range config.Rules {
			if config.Rules[i].ID == "" {
				config.Rules[i].ID = "fuzz-" + strconv.Itoa(i)
			}
		}
		document, err := config.Document()
		if err != nil {
			return
		}
		kept, err := bucket.ParseReplicationConfig(document)
		if err != nil {
			t.Fatalf("the document of a configuration does not parse: %v", err)
		}
		out, err := xml.Marshal(replicationConfigurationOf(kept))
		if err != nil {
			t.Fatal(err)
		}
		var again replicationConfiguration
		if err := decodeShapedXML(bytes.NewReader(out), maxConfigBody, &again, replicationShape(), unsupportedReplication()...); err != nil {
			t.Fatalf("what GET writes does not read: %v\n%s", err, out)
		}
		back, err := replicationRules(again)
		if err != nil {
			t.Fatalf("what GET writes is refused: %v\n%s", err, out)
		}
		if !reflect.DeepEqual(back, kept) {
			t.Fatalf("written and read again\n%+v\nwant\n%+v", back, kept)
		}
	})
}
