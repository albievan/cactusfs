package iam

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// Documents of FuzzExplainAgreesWithDecide and TestDecideAgreesWithTheOracle.
// Identity and key policies come first, then bucket policies; sub-bucket
// policies are of the sub-bucket "reports" of "site". Several put a matching
// Deny before a matching Allow, which only an evaluation that reads on after
// a Deny can get wrong.
var (
	explainIdentities = []string{
		`{"Version":"2012-10-17","Statement":[
			{"Sid":"read","Effect":"Allow","Action":["s3:Get*","s3:List*"],"Resource":"*"},
			{"Sid":"no-delete","Effect":"Deny","Action":"s3:Delete*","Resource":"arn:aws:s3:::site/*"},
			{"Sid":"office","Effect":"Allow","Action":"s3:PutObject","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}},
			{"Sid":"tls","Effect":"Deny","Action":"*","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}`,
		`{"Version":"2012-10-17","Statement":[
			{"Sid":"deny-first","Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"},
			{"Sid":"allow-after","Effect":"Allow","Action":["s3:*","admin:*"],"Resource":"*"}]}`,
		`{"Version":"2012-10-17","Statement":[
			{"Sid":"bypass","Effect":"Allow","Action":"s3:BypassGovernanceRetention","Resource":"*"},
			{"Sid":"preview","Effect":"Allow","Action":"admin:PreviewObject","Resource":"*"},
			{"Sid":"b-users","Effect":"Deny","Action":"*","Resource":"*","Condition":{"StringLike":{"aws:username":"b*"}}}]}`,
		`{"Version":"2012-10-17","Statement":[
			{"Sid":"only-read","Effect":"Allow","Action":"s3:GetObject","Resource":"*"},
			{"Sid":"outside","Effect":"Deny","Action":"*","Resource":"*","Condition":{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`,
		``,
	}
	explainBuckets = []string{
		`{"Version":"2012-10-17","Statement":[
			{"Sid":"public","Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::site","arn:aws:s3:::site/*"]},
			{"Sid":"bob","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:*","Resource":"arn:aws:s3:::site/*"},
			{"Sid":"keep","Effect":"Deny","Principal":"*","Action":"s3:Delete*","Resource":"arn:aws:s3:::site/*"}]}`,
		`{"Version":"2012-10-17","Statement":[
			{"Sid":"deny-first","Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/secret/*"},
			{"Sid":"allow-after","Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::site","arn:aws:s3:::site/*"]}]}`,
		`{"Version":"2012-10-17","Statement":[
			{"Sid":"plain","Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::site","arn:aws:s3:::site/*"],"Condition":{"Bool":{"aws:SecureTransport":"false"}}},
			{"Sid":"bob-office","Effect":"Allow","Principal":{"AWS":["arn:aws:iam:::user/bob","arn:aws:iam:::user/alice"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}},
			{"Sid":"no-bypass","Effect":"Deny","Principal":"*","Action":"s3:BypassGovernanceRetention","Resource":"arn:aws:s3:::site/*"}]}`,
		``,
	}
	explainSubBuckets = []string{
		`{"Version":"2012-10-17","Statement":[
			{"Sid":"alice","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/alice"},"Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::site","arn:aws:s3:::site/reports/*"]},
			{"Sid":"private","Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*","Condition":{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`,
		`{"Version":"2012-10-17","Statement":[
			{"Sid":"deny-first","Effect":"Deny","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"},
			{"Sid":"allow-after","Effect":"Allow","Principal":{"AWS":["arn:aws:iam:::user/bob","arn:aws:iam:::user/alice"]},"Action":["s3:GetObject","s3:PutObject","s3:ListBucket"],"Resource":["arn:aws:s3:::site","arn:aws:s3:::site/reports/*"]}]}`,
		``,
	}
	explainActions = []string{
		"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket", "s3:BypassGovernanceRetention",
		"admin:PreviewObject", "admin:GetUser", "cluster:NodeRPC", "cluster:JoinNode", "s3:GetObjectVersion",
	}
)

// The bits of FuzzExplainAgreesWithDecide's request: each sets one thing, so
// that every combination of kinds of principal - also those no
// authentication makes - is asked.
const (
	bitAccessKey    = iota // the principal has an access key
	bitRoot                // it is the root identity
	bitNode                // it is node n1
	bitService             // it is a service
	bitReplication         // the service is the replicator, not the lifecycle worker
	bitBob                 // it is the user bob
	bitAlice               // it is the user alice (unless bob)
	bitLocked              // its session is locked
	bitRevoked             // the node is revoked
	bitInside              // the address is known, 10.1.2.3
	bitOutside             // the address is known, 192.0.2.1 (unless inside)
	bitTLS                 // the request arrived over TLS
	bitSecondPolicy        // the user has a second policy: the key policy's document
	bitNamed               // the policies are named, one for each
	bitMisnamed            // the policies are named, one name for them all
	bitBoundary            // the key has its own policy
	bitListing             // the request is a listing with the prefix, of no key
	bitNodeResource        // the resource is node n1
	bitCreator             // the user committed the version
	bitBucketPolicy        // the bucket has its policy
	bitSubBucket           // the sub-bucket "reports" has its policy
)

// OldDecide is the Decide of bd35f29 (decideoracle_test.go), for the tests of
// package iam_test.
func OldDecide(req Request) error { return oldDecide(req) }

// explainRequest builds the request of one input: documents that do not
// parse are left out.
func explainRequest(bits uint64, identity, boundary, bucketPolicy, subBucketPolicy []byte, action, key, prefix string) Request {
	on := func(bit int) bool { return bits>>bit&1 == 1 }
	var p Principal
	if on(bitAccessKey) {
		p.AccessKeyID = "KEY"
	}
	p.Root = on(bitRoot)
	if on(bitNode) {
		p.NodeID = "n1"
	}
	if on(bitService) {
		p.Service = ServiceLifecycle
		if on(bitReplication) {
			p.Service = ServiceReplication
		}
	}
	switch {
	case on(bitBob):
		p.User = "bob"
	case on(bitAlice):
		p.User = "alice"
	}
	p.LockedSession = on(bitLocked)
	p.NodeRevoked = on(bitRevoked)
	switch {
	case on(bitInside):
		p.Session.SourceIP = netip.MustParseAddr("10.1.2.3")
	case on(bitOutside):
		p.Session.SourceIP = netip.MustParseAddr("192.0.2.1")
	}
	p.Session.SecureTransport = on(bitTLS)
	if doc, err := ParsePolicy(identity); err == nil {
		p.Policies = []Document{doc}
	}
	keyPolicy, keyErr := ParsePolicy(boundary)
	if keyErr == nil && on(bitSecondPolicy) {
		p.Policies = append(p.Policies, keyPolicy)
	}
	switch {
	case on(bitNamed):
		p.PolicyNames = []string{"first", "second"}[:len(p.Policies)]
	case on(bitMisnamed):
		p.PolicyNames = []string{"only"}
	}
	if keyErr == nil && on(bitBoundary) {
		p.Boundary = &keyPolicy
	}
	req := Request{
		Principal: p, Action: Action(action), Resource: Resource{Bucket: "site", Key: key},
		Now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC),
	}
	if on(bitListing) {
		req.ListPrefix, req.Resource.Key = prefix, ""
	}
	if on(bitNodeResource) {
		req.Resource = Resource{Node: "n1"}
	}
	if on(bitCreator) {
		req.ObjectCreator = "user/" + p.User
	}
	if policy, err := ParseBucketPolicy(bucketPolicy, "site"); err == nil && on(bitBucketPolicy) {
		req.BucketPolicy = &policy
	}
	if policy, err := ParseSubBucketPolicy(subBucketPolicy, "site", "reports"); err == nil && on(bitSubBucket) {
		req.SubBucketPolicy = &policy
	}
	return req
}

// bitsOf returns the bits with each of the given ones set.
func bitsOf(bits ...int) uint64 {
	var b uint64
	for _, bit := range bits {
		b |= 1 << bit
	}
	return b
}

// FuzzExplainAgreesWithDecide holds Decide, Evaluate and Explain to the
// decision of bd35f29 (decideoracle_test.go) - the same source, the same
// error text, the same sentinels - and Explain to what it says: it allows
// exactly what Decide allows; every statement it cites is where it says, has
// the effect it says, and matches the request; a refusal by a Deny cites a
// Deny of that source and an Allow an Allow; and a condition is said to be
// decided on an unknown address only when the address was not known.
func FuzzExplainAgreesWithDecide(f *testing.F) {
	user := []int{bitAccessKey, bitInside, bitTLS, bitBucketPolicy, bitSubBucket}
	seed := func(identity, bucket, sub, action, key, prefix string, bits ...int) {
		f.Add(bitsOf(bits...), []byte(identity), []byte(explainIdentities[3]), []byte(bucket), []byte(sub), action, key, prefix)
	}
	id, bucket, sub := explainIdentities, explainBuckets, explainSubBuckets
	seed(id[0], bucket[0], sub[0], "s3:GetObject", "reports/a", "", append(user, bitAlice, bitNamed, bitBoundary)...)
	seed(id[0], bucket[0], sub[0], "s3:GetObject", "reports/a", "")
	seed(id[0], bucket[0], sub[0], "s3:DeleteObject", "a", "", append(user, bitBob, bitNamed, bitBoundary)...)
	seed(id[0], bucket[0], sub[0], "s3:PutObject", "a", "", bitAccessKey, bitAlice, bitTLS, bitBoundary)
	seed(id[0], bucket[0], sub[0], "s3:ListBucket", "", "reports/", append(user, bitAlice, bitNamed, bitListing)...)
	seed(id[0], bucket[0], sub[0], "s3:PutObject", "a", "", append(user, bitAlice, bitLocked)...)
	seed(id[0], bucket[0], sub[0], "s3:GetObject", "a", "", append(user, bitAlice, bitLocked)...)
	seed(id[2], bucket[0], sub[0], "admin:PreviewObject", "a", "", bitAccessKey, bitBob, bitCreator)
	seed(id[0], "", "", "s3:GetObject", "a", "", bitAccessKey, bitRoot)
	seed(id[0], "", "", "cluster:NodeRPC", "", "", bitNode)
	seed(id[0], "", "", "cluster:JoinNode", "", "", bitNode, bitRevoked, bitNodeResource)
	seed(id[0], "", "", "s3:DeleteObject", "a", "", bitService)
	seed(id[0], bucket[2], "", "s3:BypassGovernanceRetention", "a", "", append(user, bitBob)...)
	// A matching Deny before a matching Allow: in one identity document,
	// across two, in a bucket's policy, in a sub-bucket's, and a bucket's
	// Deny of an anonymous caller.
	seed(id[1], "", "", "s3:GetObject", "reports/a", "", bitAccessKey, bitAlice, bitNamed)
	f.Add(bitsOf(bitAccessKey, bitAlice, bitSecondPolicy, bitNamed, bitInside, bitTLS),
		[]byte(`{"Version":"2012-10-17","Statement":[{"Sid":"deny","Effect":"Deny","Action":"s3:GetObject","Resource":"*"}]}`),
		[]byte(id[0]), []byte(""), []byte(""), "s3:GetObject", "a", "")
	seed("", bucket[1], "", "s3:GetObject", "secret/a", "", bitAccessKey, bitBob, bitBucketPolicy)
	seed("", "", sub[1], "s3:GetObject", "reports/a", "", bitAccessKey, bitBob, bitSubBucket)
	seed("", bucket[1], "", "s3:GetObject", "secret/a", "", bitBucketPolicy)
	f.Fuzz(func(t *testing.T, bits uint64, identity, boundary, bucketPolicy, subBucketPolicy []byte, action, key, prefix string) {
		explainCheck(t, explainRequest(bits, identity, boundary, bucketPolicy, subBucketPolicy, action, key, prefix))
	})
}

// splitMix is SplitMix64: numbers that are the same on every run of a seed,
// for a sample of tests (gosec G404 refuses math/rand). It is no source of
// secrets.
type splitMix struct{ state uint64 }

// Uint64 returns the next number.
func (s *splitMix) Uint64() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ z>>30) * 0xBF58476D1CE4E5B9
	z = (z ^ z>>27) * 0x94D049BB133111EB
	return z ^ z>>31
}

// IntN returns a number in [0, n), for n of at most 65536.
func (s *splitMix) IntN(n int) int {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], s.Uint64())
	return int(binary.BigEndian.Uint16(b[:2])) % n
}

// TestDecideAgreesWithTheOracle asks a seeded sample of requests - every
// combination of the bits, of the documents above, and of actions and keys -
// of Decide, Evaluate and Explain, and holds them to the decision of
// bd35f29, as FuzzExplainAgreesWithDecide does.
func TestDecideAgreesWithTheOracle(t *testing.T) {
	t.Parallel()
	r := &splitMix{state: 93}
	keys := []string{"reports/a", "a", "secret/a", "reports/"}
	pick := func(list []string) []byte { return []byte(list[r.IntN(len(list))]) }
	for range 5000 {
		req := explainRequest(r.Uint64(), pick(explainIdentities), pick(explainIdentities), pick(explainBuckets), pick(explainSubBuckets),
			explainActions[r.IntN(len(explainActions))], keys[r.IntN(len(keys))], keys[r.IntN(len(keys))])
		explainCheck(t, req)
		if t.Failed() {
			return
		}
	}
}

// explainCheck holds one request's Decide, Evaluate and Explain to the
// oracle and Explain to what it says.
func explainCheck(t *testing.T, req Request) {
	t.Helper()
	oldSource, oldErr := oldEvaluate(req)
	source, err := Evaluate(req)
	decided := Decide(req)
	e := Explain(req)
	if source != oldSource || e.Source != oldSource {
		t.Fatalf("source: Evaluate %q, Explain %q, bd35f29 %q (%v)", source, e.Source, oldSource, oldErr)
	}
	for name, got := range map[string]error{"Evaluate": err, "Decide": decided, "Explain": e.Err(), "the old Decide": oldDecide(req)} {
		if !explainSameError(got, oldErr) {
			t.Fatalf("%s: %v, bd35f29: %v", name, got, oldErr)
		}
		for _, sentinel := range []error{domain.ErrAccessDenied, domain.ErrSessionLocked} {
			if errors.Is(got, sentinel) != errors.Is(oldErr, sentinel) {
				t.Fatalf("%s: %v, bd35f29: %v, as to %v", name, got, oldErr, sentinel)
			}
		}
	}
	if e.Allowed != (oldErr == nil) {
		t.Fatalf("Explain allowed %v, bd35f29 %v", e.Allowed, oldErr)
	}
	if (e.Rule != "") != (e.Source == SourceRule) {
		t.Fatalf("rule %q with source %q", e.Rule, e.Source)
	}
	if e.Source == SourceRule && len(e.Statements) != 0 {
		t.Fatalf("a rule answered, and %d statements are cited", len(e.Statements))
	}
	for _, c := range e.Statements {
		if c.Source == SourceKeyBound {
			t.Fatalf("the key's statement %+v among the others", c)
		}
		explainCheckCited(t, req, c, true)
	}
	if e.KeyPolicy != nil {
		if req.Principal.Boundary == nil {
			t.Fatal("a key's verdict without a key policy")
		}
		for _, c := range e.KeyPolicy.Statements {
			if c.Source != SourceKeyBound {
				t.Fatalf("the statement %+v among the key's", c)
			}
			explainCheckCited(t, req, c, true)
		}
		allowedByKey := slices.ContainsFunc(e.KeyPolicy.Statements, func(c CitedStatement) bool { return c.Effect == Allow })
		deniedByKey := slices.ContainsFunc(e.KeyPolicy.Statements, func(c CitedStatement) bool { return c.Effect == Deny })
		if e.KeyPolicy.Allowed != (allowedByKey && !deniedByKey) {
			t.Fatalf("the key's policy allowed %v, cites %+v", e.KeyPolicy.Allowed, e.KeyPolicy.Statements)
		}
	}
	if e.Source == SourceKeyBound && (e.KeyPolicy == nil || e.KeyPolicy.Allowed) {
		t.Fatalf("refused by the key's policy, which said %+v", e.KeyPolicy)
	}
	for _, c := range e.UnknownAddress {
		if req.Principal.Session.SourceIP.IsValid() {
			t.Fatalf("%+v decided on an unknown address, with %v known", c, req.Principal.Session.SourceIP)
		}
		s := explainCheckCited(t, req, c, false)
		if !slices.ContainsFunc(s.Conditions, func(c Condition) bool { return c.Operator == OpIPAddress || c.Operator == OpNotIPAddress }) {
			t.Fatalf("%+v has no condition on the address", c)
		}
	}
	switch e.Source {
	case SourceIdentity, SourceBucket, SourceSubBucket:
		effect := Allow
		if !e.Allowed {
			effect = Deny
		}
		if !slices.ContainsFunc(e.Statements, func(c CitedStatement) bool { return c.Source == e.Source && c.Effect == effect }) {
			t.Fatalf("%s by %s (%v), citing %+v", effect, e.Source, e.Err(), e.Statements)
		}
	}
}

// explainCheckCited finds the statement c cites in the request, checks that
// it is the statement c says - Sid, effect, a principal it is about - and,
// when matched, that it matches the request; it returns the statement.
func explainCheckCited(t *testing.T, req Request, c CitedStatement, matched bool) Statement {
	t.Helper()
	var documents []Document
	var principals [][]string
	switch c.Source {
	case SourceIdentity:
		documents = req.Principal.Policies
	case SourceKeyBound:
		if req.Principal.Boundary != nil {
			documents = []Document{*req.Principal.Boundary}
		}
	case SourceBucket:
		if req.BucketPolicy != nil {
			documents, principals = []Document{req.BucketPolicy.Document}, req.BucketPolicy.Principals
		}
	case SourceSubBucket:
		if req.SubBucketPolicy != nil && req.SubBucketPolicy.covers(req) {
			documents, principals = []Document{req.SubBucketPolicy.policy.Document}, req.SubBucketPolicy.policy.Principals
		}
	}
	if c.Document < 0 || c.Document >= len(documents) || c.Statement < 0 || c.Statement >= len(documents[c.Document].Statements) {
		t.Fatalf("%+v cites no statement of the request", c)
	}
	if c.Source != SourceIdentity && c.Document != 0 {
		t.Fatalf("%+v: a source of one document", c)
	}
	s := documents[c.Document].Statements[c.Statement]
	if s.Sid != c.Sid || s.Effect != c.Effect {
		t.Fatalf("%+v cites a statement with Sid %q and effect %s", c, s.Sid, s.Effect)
	}
	wantName := ""
	if c.Source == SourceIdentity && len(req.Principal.PolicyNames) == len(req.Principal.Policies) {
		wantName = req.Principal.PolicyNames[c.Document]
	}
	if c.Policy != wantName {
		t.Fatalf("%+v names policy %q, want %q", c, c.Policy, wantName)
	}
	if principals != nil {
		if !slices.Contains(principals[c.Statement], PrincipalAnyone) && (req.Principal.User == "" || !slices.Contains(principals[c.Statement], req.Principal.User)) {
			t.Fatalf("%+v is about %v, not %q", c, principals[c.Statement], req.Principal.User)
		}
		if s.Effect == Allow && req.Action.byNameOnly() {
			t.Fatalf("%+v: an Allow of %s cited from a %s", c, req.Action, c.Source)
		}
	}
	if !matched {
		return s
	}
	ctx := Context{SourceIP: req.Principal.Session.SourceIP, SecureTransport: req.Principal.Session.SecureTransport, Now: req.Now, Username: req.Principal.User}
	if !s.oldMatches(req.Action, req.Resource.String(), ctx) {
		t.Fatalf("%+v does not match %s on %s", c, req.Action, req.Resource)
	}
	return s
}

// explainSameError reports whether two errors are both nil, or say the same.
func explainSameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Error() == b.Error()
}
