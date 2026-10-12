package iam_test

import (
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

var policyNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func mustPolicy(t testing.TB, document string) iam.Document {
	t.Helper()
	doc, err := iam.ParsePolicy([]byte(document))
	if err != nil {
		t.Fatalf("ParsePolicy(%s): %v", document, err)
	}
	return doc
}

func user(name string, policies ...iam.Document) iam.Principal {
	return iam.Principal{
		AccessKeyID: "KEY" + strings.ToUpper(name), User: name, Policies: policies,
		Session: iam.Session{SourceIP: netip.MustParseAddr("10.1.2.3"), SecureTransport: true},
	}
}

func decide(p iam.Principal, action iam.Action, resource iam.Resource) error {
	return iam.Decide(iam.Request{Principal: p, Action: action, Resource: resource, Now: policyNow})
}

func TestParsePolicy(t *testing.T) {
	t.Parallel()
	valid := []string{
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}]}`,
		`{"Version":"2012-10-17","Statement":[{"Sid":"a","Effect":"Deny","Action":["s3:*","admin:Get*"],"Resource":["*"]}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"arn:cactusfs:admin:::user/*",
		  "Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","192.168.1.7"]},"Bool":{"aws:SecureTransport":"true"},
		  "DateLessThan":{"aws:CurrentTime":"2030-01-01T00:00:00Z"},"StringLike":{"aws:username":"team-*"}}}]}`,
	}
	for _, document := range valid {
		doc := mustPolicy(t, document)
		// A document reads back as the same document.
		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		again, err := iam.ParsePolicy(encoded)
		if err != nil || !reflect.DeepEqual(again, doc) {
			t.Errorf("round trip of %s: %v\n got %+v\nwant %+v", document, err, again, doc)
		}
	}

	// Strict: whatever this version does not understand is an error. A
	// statement that was silently dropped could be the Deny somebody
	// relied on.
	statement := func(body string) string { return `{"Version":"2012-10-17","Statement":[` + body + `]}` }
	invalid := map[string]string{
		"not JSON":                `{`,
		"empty":                   ``,
		"trailing data":           statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*"}`) + `{}`,
		"wrong version":           `{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`,
		"no version":              `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`,
		"no statements":           `{"Version":"2012-10-17","Statement":[]}`,
		"unknown top-level field": `{"Version":"2012-10-17","Id":"x","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`,
		"unknown statement field": statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Principal":"*"}`),
		"NotAction":               statement(`{"Effect":"Allow","NotAction":"s3:*","Resource":"*"}`),
		"NotResource":             statement(`{"Effect":"Deny","Action":"s3:*","NotResource":"*"}`),
		"effect in lower case":    statement(`{"Effect":"allow","Action":"s3:*","Resource":"*"}`),
		"no effect":               statement(`{"Action":"s3:*","Resource":"*"}`),
		"no action":               statement(`{"Effect":"Allow","Resource":"*"}`),
		"no resource":             statement(`{"Effect":"Allow","Action":"s3:*"}`),
		"empty action list":       statement(`{"Effect":"Allow","Action":[],"Resource":"*"}`),
		"action of a number":      statement(`{"Effect":"Allow","Action":7,"Resource":"*"}`),
		"unknown service":         statement(`{"Effect":"Allow","Action":"ec2:RunInstances","Resource":"*"}`),
		"cluster action":          statement(`{"Effect":"Allow","Action":"cluster:NodeRPC","Resource":"*"}`),
		"action without name":     statement(`{"Effect":"Allow","Action":"s3:","Resource":"*"}`),
		"action with a space":     statement(`{"Effect":"Allow","Action":"s3:Get Object","Resource":"*"}`),
		"resource not an ARN":     statement(`{"Effect":"Allow","Action":"s3:*","Resource":"photos/*"}`),
		"resource of another ARN": statement(`{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:ec2:::x"}`),
		"resource with newline":   statement(`{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::a\nb"}`),
		"unknown operator":        statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"NumericLessThan":{"aws:SourceIp":"1"}}}`),
		"unknown condition key":   statement(`{"Effect":"Deny","Action":"s3:*","Resource":"*","Condition":{"StringEquals":{"aws:Referer":"x"}}}`),
		"operator on wrong key":   statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"Bool":{"aws:SourceIp":"true"}}}`),
		"bad address":             statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.300"}}}`),
		"bad boolean":             statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":"yes"}}}`),
		"bad date":                statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"DateLessThan":{"aws:CurrentTime":"tomorrow"}}}`),
		"condition without value": statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":[]}}}`),
		// encoding/json would keep the last of two fields with one name,
		// and match names without regard to case. A policy may not.
		"effect given twice":              `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Effect":"Allow","Action":"s3:*","Resource":"*"}]}`,
		"resource given twice":            `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"s3:*","Resource":"*","Resource":"arn:aws:s3:::x"}]}`,
		"statement given twice":           `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"s3:*","Resource":"*"}],"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`,
		"operator given twice":            statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":"true"},"Bool":{"aws:SecureTransport":"false"}}}`),
		"condition key given twice":       statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8","aws:SourceIp":"0.0.0.0/0"}}}`),
		"field in lower case":             `{"Version":"2012-10-17","Statement":[{"effect":"Allow","Action":"s3:*","Resource":"*"}]}`,
		"top-level field in another case": `{"version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`,
		"empty condition":                 statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{}}`),
		"operator that tests nothing":     statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"IpAddress":{}}}`),
		"IPv4 range written as IPv6":      statement(`{"Effect":"Deny","Action":"s3:*","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"::ffff:10.0.0.0/104"}}}`),
		"address with a zone":             statement(`{"Effect":"Deny","Action":"s3:*","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"fe80::1%eth0"}}}`),
		"nested too deeply":               statement(`{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":[[[[[[[["true"]]]]]]]]}}}`),
		"too many statements":             `{"Version":"2012-10-17","Statement":[` + strings.TrimSuffix(strings.Repeat(`{"Effect":"Allow","Action":"s3:*","Resource":"*"},`, 65), ",") + `]}`,
		"too large":                       statement(`{"Sid":"` + strings.Repeat("x", 21<<10) + `","Effect":"Allow","Action":"s3:*","Resource":"*"}`),
	}
	for name, document := range invalid {
		if _, err := iam.ParsePolicy([]byte(document)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: ParsePolicy = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	var doc iam.Document
	if err := json.Unmarshal([]byte(`{"Version":"x"}`), &doc); err == nil {
		t.Error("Unmarshal accepted an invalid document")
	}
}

func TestDecideWithPolicies(t *testing.T) {
	t.Parallel()
	readPhotos := mustPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::photos","arn:aws:s3:::photos/*"]}]}`)
	writeAll := mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`)
	denySecret := mustPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Action":"s3:*","Resource":"arn:aws:s3:::photos/secret/*"}]}`)
	onlyGet := mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`)
	adminUsers := mustPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["admin:GetUser","admin:CreateAPIKey"],"Resource":"arn:cactusfs:admin:::user/alice"}]}`)

	object := func(bucket, key string) iam.Resource { return iam.Resource{Bucket: bucket, Key: key} }
	tests := []struct {
		name      string
		principal iam.Principal
		action    iam.Action
		resource  iam.Resource
		allowed   bool
	}{
		{"no policy allows nothing", user("u"), iam.ActionGetObject, object("photos", "a"), false},
		{"allowed by a policy", user("u", readPhotos), iam.ActionGetObject, object("photos", "a/b.jpg"), true},
		{"bucket-level action on the bucket", user("u", readPhotos), iam.ActionListBucket, iam.Resource{Bucket: "photos"}, true},
		{"another action", user("u", readPhotos), iam.ActionPutObject, object("photos", "a"), false},
		{"another bucket", user("u", readPhotos), iam.ActionGetObject, object("photos2", "a"), false},
		{"a bucket whose name only begins the same", user("u", readPhotos), iam.ActionListBucket, iam.Resource{Bucket: "photos-private"}, false},
		{"action names ignore case", user("u", mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"S3:getobject","Resource":"*"}]}`)), iam.ActionGetObject, object("b", "k"), true},
		{"wildcard policy", user("u", writeAll), iam.ActionDeleteBucket, iam.Resource{Bucket: "any"}, true},
		{"account-level action under a wildcard", user("u", writeAll), iam.ActionListAllMyBuckets, iam.Resource{}, true},
		{"s3 wildcard does not grant admin", user("u", writeAll), iam.ActionPutUser, iam.Resource{Admin: "user/bob"}, false},
		{"explicit deny wins over allow", user("u", writeAll, denySecret), iam.ActionGetObject, object("photos", "secret/plan.txt"), false},
		{"deny wins whatever the order", user("u", denySecret, writeAll), iam.ActionGetObject, object("photos", "secret/plan.txt"), false},
		{"deny leaves the rest alone", user("u", writeAll, denySecret), iam.ActionGetObject, object("photos", "public/a"), true},
		{"a deny alone allows nothing", user("u", denySecret), iam.ActionGetObject, object("photos", "public/a"), false},
		{"admin resource by name", user("u", adminUsers), iam.ActionGetUser, iam.Resource{Admin: "user/alice"}, true},
		{"admin resource of another name", user("u", adminUsers), iam.ActionGetUser, iam.Resource{Admin: "user/alice2"}, false},
		{"a key narrowed by its own policy", func() iam.Principal { p := user("u", writeAll); p.Boundary = &onlyGet; return p }(), iam.ActionGetObject, object("b", "k"), true},
		{"a narrowed key cannot do what its policy omits", func() iam.Principal { p := user("u", writeAll); p.Boundary = &onlyGet; return p }(), iam.ActionPutObject, object("b", "k"), false},
		{"a key's policy cannot add to its owner's", func() iam.Principal { p := user("u", readPhotos); p.Boundary = &writeAll; return p }(), iam.ActionPutObject, object("photos", "k"), false},
		{"a key's own deny applies", func() iam.Principal { p := user("u", writeAll); p.Boundary = &denySecret; return p }(), iam.ActionGetObject, object("photos", "secret/x"), false},
		{"root needs no policy", iam.Principal{AccessKeyID: "ROOT", Root: true}, iam.ActionPutUser, iam.Resource{Admin: "user/x"}, true},
		{"a key that is neither root nor a user's", iam.Principal{AccessKeyID: "ORPHAN"}, iam.ActionGetObject, object("b", "k"), false},
		{"root and user at once is nobody", iam.Principal{AccessKeyID: "X", Root: true, User: "u", Policies: []iam.Document{writeAll}}, iam.ActionGetObject, object("b", "k"), false},
		{"a user cannot be granted node RPC", user("u", mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`)), iam.ActionNodeRPC, iam.Resource{}, false},
		{"a '*' in a key is a character, not a wildcard", user("u", mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/report"}]}`)), iam.ActionGetObject, object("b", "*"), false},
		{"a '?' in a key is a character too", user("u", mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/a"}]}`)), iam.ActionGetObject, object("b", "?"), false},
		{"a '*' in a bucket name asked for", user("u", readPhotos), iam.ActionListBucket, iam.Resource{Bucket: "*"}, false},
		{"a key that looks like another bucket's path", user("u", readPhotos), iam.ActionGetObject, object("other", "x/../../photos/a"), false},
		{"a node with policies is still only a node", iam.Principal{NodeID: "n1", Policies: []iam.Document{writeAll}}, iam.ActionGetObject, object("b", "k"), false},
	}
	for _, tt := range tests {
		err := decide(tt.principal, tt.action, tt.resource)
		if tt.allowed && err != nil {
			t.Errorf("%s: refused: %v", tt.name, err)
		}
		if !tt.allowed && !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s: error = %v, want %v", tt.name, err, domain.ErrAccessDenied)
		}
	}
}

func TestConditions(t *testing.T) {
	t.Parallel()
	policy := func(effect, condition string) iam.Document {
		return mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"`+effect+`","Action":"s3:*","Resource":"*","Condition":`+condition+`}]}`)
	}
	allowAll := mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`)
	at := func(ip string, secure bool) iam.Session {
		s := iam.Session{SecureTransport: secure}
		if ip != "" {
			s.SourceIP = netip.MustParseAddr(ip)
		}
		return s
	}
	tests := []struct {
		name     string
		policies []iam.Document
		session  iam.Session
		now      time.Time
		allowed  bool
	}{
		{"address inside the range", []iam.Document{policy("Allow", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`)}, at("10.9.8.7", true), policyNow, true},
		{"address outside the range", []iam.Document{policy("Allow", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`)}, at("11.0.0.1", true), policyNow, false},
		{"one of several ranges", []iam.Document{policy("Allow", `{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","2001:db8::/32"]}}`)}, at("2001:db8::1", true), policyNow, true},
		{"a single address", []iam.Document{policy("Allow", `{"IpAddress":{"aws:SourceIp":"192.168.1.7"}}`)}, at("192.168.1.7", true), policyNow, true},
		{"IPv4 seen as mapped IPv6", []iam.Document{policy("Allow", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`)}, at("::ffff:10.1.1.1", true), policyNow, true},
		{"unknown address gets no allow", []iam.Document{policy("Allow", `{"IpAddress":{"aws:SourceIp":"0.0.0.0/0"}}`)}, at("", true), policyNow, false},
		{"unknown address is not let past a NotIpAddress allow", []iam.Document{policy("Allow", `{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`)}, at("", true), policyNow, false},
		{"deny from outside the office", []iam.Document{allowAll, policy("Deny", `{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`)}, at("8.8.8.8", true), policyNow, false},
		{"no deny from inside the office", []iam.Document{allowAll, policy("Deny", `{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`)}, at("10.2.3.4", true), policyNow, true},
		// A Deny that depends on the address is applied when the address is
		// not known: nobody escapes it by not saying where they come from.
		{"deny from outside, address unknown", []iam.Document{allowAll, policy("Deny", `{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`)}, at("", true), policyNow, false},
		{"deny from a range, address unknown", []iam.Document{allowAll, policy("Deny", `{"IpAddress":{"aws:SourceIp":"192.0.2.0/24"}}`)}, at("", true), policyNow, false},
		{"deny of link-local applies to an address with a zone", []iam.Document{allowAll, policy("Deny", `{"IpAddress":{"aws:SourceIp":"fe80::/10"}}`)}, at("fe80::1%en0", true), policyNow, false},
		{"allow of link-local applies to an address with a zone", []iam.Document{policy("Allow", `{"IpAddress":{"aws:SourceIp":"fe80::/10"}}`)}, at("fe80::1%en0", true), policyNow, true},
		{"deny without TLS", []iam.Document{allowAll, policy("Deny", `{"Bool":{"aws:SecureTransport":"false"}}`)}, at("10.0.0.1", false), policyNow, false},
		{"no deny with TLS", []iam.Document{allowAll, policy("Deny", `{"Bool":{"aws:SecureTransport":"false"}}`)}, at("10.0.0.1", true), policyNow, true},
		{"allow until a date, before it", []iam.Document{policy("Allow", `{"DateLessThan":{"aws:CurrentTime":"2026-12-31T00:00:00Z"}}`)}, at("10.0.0.1", true), policyNow, true},
		{"allow until a date, after it", []iam.Document{policy("Allow", `{"DateLessThan":{"aws:CurrentTime":"2026-12-31T00:00:00Z"}}`)}, at("10.0.0.1", true), policyNow.AddDate(1, 0, 0), false},
		{"allow from a date, before it", []iam.Document{policy("Allow", `{"DateGreaterThan":{"aws:CurrentTime":"2027-01-01T00:00:00Z"}}`)}, at("10.0.0.1", true), policyNow, false},
		{"a time that was never set allows nothing by date", []iam.Document{policy("Allow", `{"DateGreaterThan":{"aws:CurrentTime":"2020-01-01T00:00:00Z"}}`)}, at("10.0.0.1", true), time.Time{}, false},
		{"all conditions must hold", []iam.Document{policy("Allow", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"},"Bool":{"aws:SecureTransport":"true"}}`)}, at("10.0.0.1", false), policyNow, false},
		{"user name matches", []iam.Document{policy("Allow", `{"StringLike":{"aws:username":"ali*"}}`)}, at("10.0.0.1", true), policyNow, true},
		{"user name does not match", []iam.Document{policy("Allow", `{"StringEquals":{"aws:username":"bob"}}`)}, at("10.0.0.1", true), policyNow, false},
		{"user name excluded", []iam.Document{policy("Allow", `{"StringNotEquals":{"aws:username":"alice"}}`)}, at("10.0.0.1", true), policyNow, false},
	}
	for _, tt := range tests {
		p := iam.Principal{AccessKeyID: "K", User: "alice", Policies: tt.policies, Session: tt.session}
		err := iam.Decide(iam.Request{Principal: p, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "b", Key: "k"}, Now: tt.now})
		if tt.allowed != (err == nil) {
			t.Errorf("%s: allowed = %v, want %v (%v)", tt.name, err == nil, tt.allowed, err)
		}
	}
}

func TestBuiltInRoles(t *testing.T) {
	t.Parallel()
	policies := iam.BuiltInPolicies()
	roles := iam.BuiltInRoles()
	if len(policies) != 5 || len(roles) != 5 {
		t.Fatalf("%d built-in policies, %d roles; want 5 of each", len(policies), len(roles))
	}
	object, bucket := iam.Resource{Bucket: "b", Key: "k"}, iam.Resource{Bucket: "b"}
	admin := iam.Resource{Admin: "user/x"}
	// role -> what it must and must not allow
	matrix := map[string]map[iam.Action]bool{
		iam.RoleClusterAdmin: {iam.ActionGetObject: true, iam.ActionPutObject: true, iam.ActionDeleteBucket: true, iam.ActionPutUser: true, iam.ActionGetAuditLog: true, iam.ActionGetSettings: true},
		iam.RoleBucketAdmin:  {iam.ActionGetObject: true, iam.ActionPutObject: true, iam.ActionCreateBucket: true, iam.ActionDeleteBucket: true, iam.ActionPutUser: false, iam.ActionGetAuditLog: false, iam.ActionGetSettings: false},
		iam.RoleReadWrite:    {iam.ActionGetObject: true, iam.ActionPutObject: true, iam.ActionDeleteObject: true, iam.ActionListBucket: true, iam.ActionCreateBucket: false, iam.ActionDeleteBucket: false, iam.ActionPutUser: false, iam.ActionGetSettings: false},
		iam.RoleReadOnly:     {iam.ActionGetObject: true, iam.ActionListBucket: true, iam.ActionPutObject: false, iam.ActionDeleteObject: false, iam.ActionGetUser: false, iam.ActionGetSettings: false},
		iam.RoleAuditor:      {iam.ActionGetAuditLog: true, iam.ActionListUsers: true, iam.ActionGetMetrics: true, iam.ActionGetSettings: true, iam.ActionGetObject: false, iam.ActionListBucket: false, iam.ActionPutUser: false, iam.ActionCreateKey: false, iam.ActionDeleteUser: false},
	}
	for role, actions := range matrix {
		if !iam.IsBuiltIn(role) || !roles[role].BuiltIn || len(roles[role].Policies) != 1 {
			t.Errorf("role %s is not a built-in role with one policy", role)
		}
		if err := policies[role].Validate(); err != nil {
			t.Errorf("built-in policy %s is invalid: %v", role, err)
		}
		for action, want := range actions {
			resource := object
			switch {
			case strings.HasPrefix(string(action), "admin:"):
				resource = admin
			case action == iam.ActionListBucket || action == iam.ActionCreateBucket || action == iam.ActionDeleteBucket:
				resource = bucket
			}
			err := decide(user("u", policies[role]), action, resource)
			if want != (err == nil) {
				t.Errorf("%s: %s allowed = %v, want %v", role, action, err == nil, want)
			}
		}
		// No built-in role grants what only nodes may do.
		if err := decide(user("u", policies[role]), iam.ActionNodeRPC, iam.Resource{}); err == nil {
			t.Errorf("%s grants node RPC", role)
		}
	}
	// The actions of quotas, targets, notifications and replication (PLAN
	// 8.0.3), each on the resource its use case will name. What a role
	// holds of them it holds by a wildcard, and that is the point of the
	// table: all of administration for the cluster-admin; reading it, and
	// nothing that changes anything, for the auditor; the two bucket
	// configurations, which are S3's, for the bucket-admin - and neither the
	// quota of its bucket, nor a target, nor the replica mode.
	target := iam.Resource{Admin: "target/hook"}
	hybrid := []struct {
		action   iam.Action
		resource iam.Resource
		holders  []string
	}{
		{iam.ActionGetBucketQuota, bucket, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{iam.ActionPutBucketQuota, bucket, []string{iam.RoleClusterAdmin}},
		{iam.ActionDeleteBucketQuota, bucket, []string{iam.RoleClusterAdmin}},
		{iam.ActionRecountBucketUsage, bucket, []string{iam.RoleClusterAdmin}},
		{iam.ActionListTargets, iam.Resource{Admin: "targets"}, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{iam.ActionGetTarget, target, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{iam.ActionPutTarget, target, []string{iam.RoleClusterAdmin}},
		{iam.ActionDeleteTarget, target, []string{iam.RoleClusterAdmin}},
		{iam.ActionGetTargetStatus, target, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{iam.ActionGetReplicationStatus, bucket, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{iam.ActionResyncReplication, bucket, []string{iam.RoleClusterAdmin}},
		{iam.ActionGetBucketReplicaMode, bucket, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{iam.ActionPutBucketReplicaMode, bucket, []string{iam.RoleClusterAdmin}},
		{iam.ActionGetBucketNotification, bucket, []string{iam.RoleClusterAdmin, iam.RoleBucketAdmin}},
		{iam.ActionPutBucketNotification, bucket, []string{iam.RoleClusterAdmin, iam.RoleBucketAdmin}},
		{iam.ActionGetReplication, bucket, []string{iam.RoleClusterAdmin, iam.RoleBucketAdmin}},
		{iam.ActionPutReplication, bucket, []string{iam.RoleClusterAdmin, iam.RoleBucketAdmin}},
	}
	for _, tt := range hybrid {
		for role := range policies {
			err := decide(user("u", policies[role]), tt.action, tt.resource)
			if want := slices.Contains(tt.holders, role); want != (err == nil) {
				t.Errorf("%s: %s on %s allowed = %v, want %v", role, tt.action, tt.resource, err == nil, want)
			}
		}
	}
	if iam.IsBuiltIn("operator") {
		t.Error("IsBuiltIn accepts a name that is not built in")
	}
}

func TestEntities(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"alice", "Alice.Smith", "svc-backup_01", "a+b=c,d@e", strings.Repeat("x", 64)} {
		if err := iam.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "a b", "a/b", "a*", "näme", "../x", "a\x00", strings.Repeat("x", 65)} {
		if err := iam.ValidateName(name); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ValidateName(%q) = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	valid := iam.User{Name: "alice", Groups: []string{"ops"}, Roles: []string{"read-only"}}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid user: %v", err)
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = "r" + strings.Repeat("x", i%10) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	invalid := map[string]interface{ Validate() error }{
		"user with a bad name":        iam.User{Name: "a b"},
		"user with a role twice":      iam.User{Name: "a", Roles: []string{"r", "r"}},
		"user with a bad group":       iam.User{Name: "a", Groups: []string{"g/1"}},
		"user with too many roles":    iam.User{Name: "a", Roles: many},
		"group with a bad role":       iam.Group{Name: "g", Roles: []string{""}},
		"role with a bad policy":      iam.Role{Name: "r", Policies: []string{"p p"}},
		"policy without a document":   iam.Policy{Name: "p"},
		"key with a bad ID":           iam.APIKey{AccessKeyID: "a b", Owner: "alice"},
		"key without an owner":        iam.APIKey{AccessKeyID: "AKIDEXAMPLE"},
		"key with a long description": iam.APIKey{AccessKeyID: "AKIDEXAMPLE", Owner: "a", Description: strings.Repeat("d", 257)},
		"key with an invalid policy":  iam.APIKey{AccessKeyID: "AKIDEXAMPLE", Owner: "a", InlinePolicy: &iam.Document{}},
	}
	for name, entity := range invalid {
		if err := entity.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Validate = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	key := iam.APIKey{AccessKeyID: "AKIDEXAMPLE", Owner: "alice", ExpiresAt: policyNow}
	if key.Expired(policyNow.Add(-time.Second)) || !key.Expired(policyNow) || !key.Expired(policyNow.Add(time.Hour)) {
		t.Error("Expired is wrong around the moment of expiry")
	}
	if (iam.APIKey{}).Expired(policyNow) {
		t.Error("a key without an expiry has expired")
	}
	got := iam.RoleNames(iam.User{Roles: []string{"b", "a"}}, []iam.Group{{Roles: []string{"c", "a"}}, {Roles: []string{"b"}}})
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("RoleNames = %v", got)
	}
	if name := user("alice").Name(); name != "user/alice" {
		t.Errorf("Name = %q", name)
	}
	if got := (iam.Resource{Admin: "audit"}).String(); got != "arn:cactusfs:admin:::audit" {
		t.Errorf("admin resource = %q", got)
	}
}

// FuzzParsePolicy: whatever bytes arrive, parsing either fails as an invalid
// argument or yields a document that validates, serialises, and parses back
// to itself. It never panics and never accepts what it would not write.
func FuzzParsePolicy(f *testing.F) {
	f.Add([]byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`))
	f.Add([]byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":["s3:*"],"Resource":["*"],"Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8"]}}}]}`))
	f.Add([]byte(`{"Statement":[{}]}`))
	f.Add([]byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Effect":"Allow","Action":"s3:*","Resource":"*"}]}`))
	f.Add([]byte(`{"Version":"2012-10-17","Statement":[{"effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"IpAddress":{}}}]}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := iam.ParsePolicy(data)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("ParsePolicy error is not an invalid argument: %v", err)
			}
			return
		}
		if err := doc.Validate(); err != nil {
			t.Fatalf("a parsed document does not validate: %v", err)
		}
		// No field of the document is there twice, anywhere.
		if duplicateField(data) {
			t.Fatalf("a document with a repeated field was accepted: %s", data)
		}
		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		again, err := iam.ParsePolicy(encoded)
		if err != nil || !reflect.DeepEqual(again, doc) {
			t.Fatalf("round trip: %v\n%s", err, encoded)
		}
		// Deciding with it never panics either.
		p := iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{doc}, Boundary: &doc}
		_ = iam.Decide(iam.Request{Principal: p, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "b", Key: "k"}, Now: policyNow})
	})
}

// FuzzPolicyDecision checks the three laws of the decision on arbitrary
// patterns: silence refuses; a Deny that matches always refuses, whatever is
// allowed beside it; and a key's own policy can only take away.
func FuzzPolicyDecision(f *testing.F) {
	f.Add("s3:Get*", "arn:aws:s3:::b/*", "s3:*", "*", "b", "k")
	f.Add("s3:PutObject", "arn:aws:s3:::?/k", "admin:*", "arn:aws:s3:::b/k", "b", "k")
	f.Add("*", "*", "s3:GetObject", "arn:aws:s3:::other/*", "b", "a/b/c")
	f.Fuzz(func(t *testing.T, allowAction, allowResource, denyAction, denyResource, bucket, key string) {
		build := func(effect iam.Effect, action, resource string) (iam.Document, bool) {
			doc := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{
				{Effect: effect, Actions: []string{action}, Resources: []string{resource}},
			}}
			return doc, doc.Validate() == nil
		}
		allow, ok1 := build(iam.Allow, allowAction, allowResource)
		deny, ok2 := build(iam.Deny, denyAction, denyResource)
		if !ok1 || !ok2 || bucket == "" {
			return
		}
		resource := iam.Resource{Bucket: bucket, Key: key}
		ask := func(policies []iam.Document, boundary *iam.Document) bool {
			p := iam.Principal{AccessKeyID: "K", User: "u", Policies: policies, Boundary: boundary}
			return iam.Decide(iam.Request{Principal: p, Action: iam.ActionGetObject, Resource: resource, Now: policyNow}) == nil
		}
		if ask(nil, nil) {
			t.Fatal("allowed without any policy")
		}
		if ask([]iam.Document{deny}, nil) {
			t.Fatal("allowed by a Deny alone")
		}
		withAllow := ask([]iam.Document{allow}, nil)
		denyAsAllow, _ := build(iam.Allow, denyAction, denyResource)
		denyMatches := ask([]iam.Document{denyAsAllow}, nil)
		both := ask([]iam.Document{allow, deny}, nil)
		if denyMatches && both {
			t.Fatal("allowed although a matching Deny is present")
		}
		if !denyMatches && both != withAllow {
			t.Fatal("a Deny that does not match changed the decision")
		}
		// A boundary never widens: with any boundary, at most what was
		// allowed without it.
		for _, boundary := range []iam.Document{allow, deny, denyAsAllow} {
			if ask([]iam.Document{allow}, &boundary) && !withAllow {
				t.Fatal("a key's own policy allowed what its owner's did not")
			}
		}
	})
}

// FuzzWildcard compares the policy wildcard matcher with a slow, obviously
// right one, through the one place a pattern is matched against a client's
// string: a resource.
func FuzzWildcard(f *testing.F) {
	f.Add("a*b?c", "aXXbYc")
	f.Add("***", "")
	f.Add("?", "ab")
	f.Add("a*a*a*a*b", "aaaaaaaaaaaaaaaa")
	f.Fuzz(func(t *testing.T, pattern, key string) {
		if strings.ContainsAny(pattern+key, "\x00\n\r") || len(pattern) > 12 || len(key) > 24 || key == "" {
			return
		}
		resource := "arn:aws:s3:::b/" + pattern
		doc := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{
			{Effect: iam.Allow, Actions: []string{"s3:GetObject"}, Resources: []string{resource}},
		}}
		if doc.Validate() != nil {
			return
		}
		want := matchBytes(pattern, key)
		p := iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{doc}}
		got := iam.Decide(iam.Request{Principal: p, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "b", Key: key}, Now: policyNow}) == nil
		if got != want {
			t.Fatalf("pattern %q, key %q: matched = %v, want %v", pattern, key, got, want)
		}
	})
}

// matchBytes is a slow, obviously right wildcard matcher over bytes.
func matchBytes(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if matchBytes(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && matchBytes(pattern[1:], s[1:])
	default:
		return s != "" && s[0] == pattern[0] && matchBytes(pattern[1:], s[1:])
	}
}

// duplicateField reports whether any object in a JSON document has a field
// twice. It is written apart from the parser it checks.
func duplicateField(data []byte) bool {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	type frame struct {
		object bool
		key    bool
		seen   map[string]bool
	}
	var stack []frame
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		top := len(stack) - 1
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				if top >= 0 && stack[top].object {
					stack[top].key = true
				}
				stack = append(stack, frame{object: true, key: true, seen: map[string]bool{}})
			case '[':
				if top >= 0 && stack[top].object {
					stack[top].key = true
				}
				stack = append(stack, frame{})
			default:
				stack = stack[:top]
			}
		case string:
			if top >= 0 && stack[top].object && stack[top].key {
				if stack[top].seen[value] {
					return true
				}
				stack[top].seen[value] = true
				stack[top].key = false
				continue
			}
			if top >= 0 && stack[top].object {
				stack[top].key = true
			}
		default:
			if top >= 0 && stack[top].object {
				stack[top].key = true
			}
		}
	}
}

// TestBypassIsNeverGrantedByAWildcard: lifting a governance retention is
// allowed only by a statement that names it. A role that has "everything"
// does not have it. A Deny covers it however it is written.
func TestBypassIsNeverGrantedByAWildcard(t *testing.T) {
	t.Parallel()
	allow := func(action string) iam.Document {
		return mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"`+action+`","Resource":"*"}]}`)
	}
	deny := func(action string) iam.Document {
		return mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"`+action+`","Resource":"*"}]}`)
	}
	object := iam.Resource{Bucket: "b", Key: "k"}
	tests := []struct {
		name     string
		policies []iam.Document
		want     bool
	}{
		{"everything", []iam.Document{allow("*")}, false},
		{"all of S3", []iam.Document{allow("s3:*")}, false},
		{"a pattern that fits", []iam.Document{allow("s3:Bypass*")}, false},
		{"a pattern with a question mark", []iam.Document{allow("s3:BypassGovernanceRetentio?")}, false},
		{"by name", []iam.Document{allow("s3:BypassGovernanceRetention")}, true},
		{"by name, in another case", []iam.Document{allow("S3:bypassgovernanceretention")}, true},
		{"by name, and denied by a wildcard", []iam.Document{allow("s3:BypassGovernanceRetention"), deny("s3:*")}, false},
		{"by name, and denied by everything", []iam.Document{allow("s3:BypassGovernanceRetention"), deny("*")}, false},
		{"cluster-admin", []iam.Document{iam.BuiltInPolicies()[iam.RoleClusterAdmin]}, false},
		{"bucket-admin", []iam.Document{iam.BuiltInPolicies()[iam.RoleBucketAdmin]}, false},
	}
	for _, tt := range tests {
		err := iam.Decide(iam.Request{
			Principal: iam.Principal{AccessKeyID: "K", User: "u", Policies: tt.policies}, Action: iam.ActionOverrideGovernance, Resource: object,
		})
		if (err == nil) != tt.want {
			t.Errorf("%s: bypass allowed = %v, want %v (%v)", tt.name, err == nil, tt.want, err)
		}
	}
	// The same wildcards do grant what they always did.
	for _, policy := range []iam.Document{allow("*"), allow("s3:*")} {
		err := iam.Decide(iam.Request{Principal: iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{policy}}, Action: iam.ActionDeleteObjectVersion, Resource: object})
		if err != nil {
			t.Errorf("a wildcard no longer grants s3:DeleteObjectVersion: %v", err)
		}
	}
	// A key's inline policy must name it as well.
	named := allow("s3:BypassGovernanceRetention")
	everything := allow("s3:*")
	err := iam.Decide(iam.Request{
		Principal: iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{named}, Boundary: &everything},
		Action:    iam.ActionOverrideGovernance, Resource: object,
	})
	if err == nil {
		t.Error("a key whose inline policy says s3:* may bypass governance")
	}
}
