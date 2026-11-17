package iam

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
)

// The three parsers, by the kind of document they read. A bucket's policy
// is of the bucket "site", a sub-bucket's of its sub-bucket "reports".
type policyKind int

const (
	identityKind policyKind = iota
	bucketKind
	subBucketKind
)

func (k policyKind) String() string {
	return [...]string{"identity", "bucket", "sub-bucket"}[k]
}

// parse runs the parser of the kind; the result is the parsed policy.
func (k policyKind) parse(data []byte) (any, error) {
	switch k {
	case bucketKind:
		return ParseBucketPolicy(data, "site")
	case subBucketKind:
		return ParseSubBucketPolicy(data, "site", "reports")
	default:
		return ParsePolicy(data)
	}
}

// into runs the parser of the kind once, with the fault list given.
func (k policyKind) into(faults *faultList, data []byte) {
	switch k {
	case bucketKind:
		parseBucketPolicyInto(faults, data, "site")
	case subBucketKind:
		parseSubBucketPolicyInto(faults, data, "site", "reports")
	default:
		parsePolicyInto(faults, data)
	}
}

// oracle runs the frozen parser of the kind.
func (k policyKind) oracle(data []byte) (any, error) {
	switch k {
	case bucketKind:
		return oracleParseBucketPolicy(data, "site")
	case subBucketKind:
		return oracleParseSubBucketPolicy(data, "site", "reports")
	default:
		return oracleParsePolicy(data)
	}
}

// statementDoc is a document of one statement whose body is given.
func statementDoc(body string) string {
	return `{"Version":"2012-10-17","Statement":[{` + body + `}]}`
}

// The bodies of a statement each parser takes.
const (
	identityBody  = `"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`
	bucketBody    = `"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`
	subBucketBody = `"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"`
)

// faultRow is a document a parser refuses, and where its faults are.
type faultRow struct {
	name string
	kind policyKind
	doc  string
	// pointers are those of every fault, in order.
	pointers []string
	// offset is the byte offset of the first fault, for one of syntax.
	offset int64
}

// faultRows is one row for each refusal of the three parsers - each reason
// they give - and rows of several faults at once.
func faultRows() []faultRow {
	tooLarge := statementDoc(`"Sid":"` + strings.Repeat("x", MaxPolicyBytes) + `",` + identityBody)
	none := int64(NoOffset)
	return []faultRow{
		// The form of the document.
		{"identity: too large", identityKind, tooLarge, []string{""}, none},
		{"identity: empty", identityKind, ``, []string{""}, 0},
		// A fault of syntax is at the value that was being read, and at the
		// offset just after the byte that broke it.
		{"identity: not JSON", identityKind, `{"Version" "2012-10-17"}`, []string{"/Version"}, 12},
		{"identity: broken in a statement", identityKind, `{"Version":"2012-10-17","Statement":[{"Effect":}]}`, []string{"/Statement/0/Effect"}, 48},
		{"identity: cut short", identityKind, `{"Version":"2012-10-17","Statement":[`, []string{"/Statement/0"}, 37},
		{"identity: a field twice", identityKind, statementDoc(`"Effect":"Deny","Effect":"Allow","Action":"s3:*","Resource":"*"`), []string{"/Statement/0/Effect"}, none},
		{"identity: an unknown field", identityKind, `{"Version":"2012-10-17","Id":"x","Statement":[{` + identityBody + `}]}`, []string{"/Id"}, none},
		{"identity: an unknown statement field", identityKind, statementDoc(`"Effect":"Allow","NotAction":"s3:*","Resource":"*"`), []string{"/Statement/0/NotAction"}, none},
		{"identity: a field in another case", identityKind, statementDoc(`"effect":"Allow","Action":"s3:*","Resource":"*"`), []string{"/Statement/0/effect"}, none},
		{
			"identity: nested too deeply", identityKind,
			statementDoc(`"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":[[[[[[[["true"]]]]]]]]}}`),
			[]string{"/Statement/0/Condition/Bool/aws:SecureTransport/0/0/0/0"},
			none,
		},
		// Data after the document is at where it begins.
		{"identity: data after the document", identityKind, statementDoc(identityBody) + `{}`, []string{""}, int64(len(statementDoc(identityBody)))},
		{"identity: a value after the document", identityKind, statementDoc(identityBody) + ` x`, []string{""}, int64(len(statementDoc(identityBody)) + 1)},
		{"identity: a value after white space", identityKind, statementDoc(identityBody) + " \n{}", []string{""}, int64(len(statementDoc(identityBody)) + 2)},
		// A closing bracket after the document, which the frozen parsers
		// took (PLAN 9.2a): refused, last.
		{"identity: a closing brace after the document", identityKind, statementDoc(identityBody) + `}`, []string{""}, int64(len(statementDoc(identityBody)))},
		{"identity: a closing bracket and more after white space", identityKind, statementDoc(identityBody) + " \r\n]x", []string{""}, int64(len(statementDoc(identityBody)) + 3)},
		{"identity: a wrong Version, then a closing brace", identityKind, `{"Version":"1","Statement":[{` + identityBody + `}]}}`, []string{"/Version", ""}, none},
		{"identity: a null Condition", identityKind, statementDoc(identityBody + `,"Condition":null`), []string{"/Statement/0/Condition"}, none},
		// What encoding/json refuses.
		{"identity: not an object", identityKind, `[]`, []string{""}, none},
		{"identity: a Version that is a number", identityKind, `{"Version":1,"Statement":[{` + identityBody + `}]}`, []string{"/Version"}, none},
		{"identity: a Statement that is an object", identityKind, `{"Version":"2012-10-17","Statement":{}}`, []string{"/Statement"}, none},
		{"identity: a statement that is a string", identityKind, `{"Version":"2012-10-17","Statement":["x"]}`, []string{"/Statement/0"}, none},
		{"identity: an Effect that is a number", identityKind, statementDoc(`"Effect":1,"Action":"s3:*","Resource":"*"`), []string{"/Statement/0/Effect"}, none},
		{"identity: an Action that is a number", identityKind, statementDoc(`"Effect":"Allow","Action":7,"Resource":"*"`), []string{"/Statement/0/Action"}, none},
		{"identity: a Condition that is a list", identityKind, statementDoc(identityBody + `,"Condition":[]`), []string{"/Statement/0/Condition"}, none},
		{"identity: an operator that is a string", identityKind, statementDoc(identityBody + `,"Condition":{"Bool":"x"}`), []string{"/Statement/0/Condition/Bool"}, none},
		{"identity: a condition value that is a number", identityKind, statementDoc(identityBody + `,"Condition":{"Bool":{"aws:SecureTransport":[true]}}`), []string{"/Statement/0/Condition/Bool/aws:SecureTransport"}, none},
		{
			// encoding/json reports the Action, which stops it, and not the
			// Effect before it, after which it went on.
			"identity: what stops encoding/json is reported first", identityKind,
			statementDoc(`"Effect":1,"Action":{},"Resource":"*"`),
			[]string{"/Statement/0/Action", "/Statement/0/Effect"},
			none,
		},
		// Conditions.
		{"identity: an empty Condition", identityKind, statementDoc(identityBody + `,"Condition":{}`), []string{"/Statement/0/Condition"}, none},
		{"identity: an operator that tests nothing", identityKind, statementDoc(identityBody + `,"Condition":{"IpAddress":{}}`), []string{"/Statement/0/Condition/IpAddress"}, none},
		{"identity: an operator that is null", identityKind, statementDoc(identityBody + `,"Condition":{"IpAddress":null}`), []string{"/Statement/0/Condition/IpAddress"}, none},
		{"identity: an unknown condition key", identityKind, statementDoc(identityBody + `,"Condition":{"StringEquals":{"aws:Referer":"x"}}`), []string{"/Statement/0/Condition/StringEquals/aws:Referer"}, none},
		{"identity: an unknown operator", identityKind, statementDoc(identityBody + `,"Condition":{"NumericLessThan":{"aws:SourceIp":"1"}}`), []string{"/Statement/0/Condition/NumericLessThan"}, none},
		{"identity: an operator on the wrong key", identityKind, statementDoc(identityBody + `,"Condition":{"Bool":{"aws:SourceIp":"true"}}`), []string{"/Statement/0/Condition/Bool/aws:SourceIp"}, none},
		{"identity: a condition without values", identityKind, statementDoc(identityBody + `,"Condition":{"Bool":{"aws:SecureTransport":[]}}`), []string{"/Statement/0/Condition/Bool/aws:SecureTransport"}, none},
		{"identity: a bad address as one string", identityKind, statementDoc(identityBody + `,"Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.300"}}`), []string{"/Statement/0/Condition/IpAddress/aws:SourceIp"}, none},
		{"identity: a bad address in a list", identityKind, statementDoc(identityBody + `,"Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","fe80::1%eth0"]}}`), []string{"/Statement/0/Condition/IpAddress/aws:SourceIp/1"}, none},
		{"identity: a bad boolean", identityKind, statementDoc(identityBody + `,"Condition":{"Bool":{"aws:SecureTransport":"yes"}}`), []string{"/Statement/0/Condition/Bool/aws:SecureTransport"}, none},
		{"identity: a bad date", identityKind, statementDoc(identityBody + `,"Condition":{"DateLessThan":{"aws:CurrentTime":["tomorrow"]}}`), []string{"/Statement/0/Condition/DateLessThan/aws:CurrentTime/0"}, none},
		{"identity: a key with a slash", identityKind, statementDoc(identityBody + `,"Condition":{"StringEquals":{"a/b~c":"x"}}`), []string{"/Statement/0/Condition/StringEquals/a~1b~0c"}, none},
		// The document.
		{"identity: the wrong Version", identityKind, `{"Version":"2008-10-17","Statement":[{` + identityBody + `}]}`, []string{"/Version"}, none},
		{"identity: no Version", identityKind, `{"Statement":[{` + identityBody + `}]}`, []string{""}, none},
		{"identity: no statements", identityKind, `{"Version":"2012-10-17","Statement":[]}`, []string{"/Statement"}, none},
		{"identity: no Statement", identityKind, `{"Version":"2012-10-17"}`, []string{""}, none},
		{
			"identity: too many statements", identityKind,
			`{"Version":"2012-10-17","Statement":[` + strings.TrimSuffix(strings.Repeat(`{"Effect":"Allow","Action":"s3:*","Resource":"*"},`, 65), ",") + `]}`,
			[]string{"/Statement"},
			none,
		},
		{"identity: an Effect in lower case", identityKind, statementDoc(`"Effect":"allow","Action":"s3:*","Resource":"*"`), []string{"/Statement/0/Effect"}, none},
		{"identity: no Effect", identityKind, statementDoc(`"Action":"s3:*","Resource":"*"`), []string{"/Statement/0"}, none},
		{"identity: no Action", identityKind, statementDoc(`"Effect":"Allow","Resource":"*"`), []string{"/Statement/0"}, none},
		{"identity: an empty Resource", identityKind, statementDoc(`"Effect":"Allow","Action":"s3:*","Resource":[]`), []string{"/Statement/0/Resource"}, none},
		{"identity: an action of another service", identityKind, statementDoc(`"Effect":"Allow","Action":"ec2:RunInstances","Resource":"*"`), []string{"/Statement/0/Action"}, none},
		{"identity: an action with a space, in a list", identityKind, statementDoc(`"Effect":"Allow","Action":["s3:GetObject","s3:Get Object"],"Resource":"*"`), []string{"/Statement/0/Action/1"}, none},
		{"identity: a resource that is no ARN", identityKind, statementDoc(`"Effect":"Allow","Action":"s3:*","Resource":["photos/*"]`), []string{"/Statement/0/Resource/0"}, none},
		{"identity: a resource with a newline", identityKind, statementDoc(`"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::a\nb"`), []string{"/Statement/0/Resource"}, none},
		{
			"identity: every fault, the first first", identityKind,
			`{"Version":"1","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"},{"Effect":"Maybe","Action":["s3:*","x"],"Resource":["*","y"],` +
				`"Condition":{"Bool":{"aws:SecureTransport":"yes"},"IpAddress":{"aws:SourceIp":["1.2.3.4","z"]}}}]}`,
			[]string{
				"/Version", "/Statement/1/Effect", "/Statement/1/Action/1", "/Statement/1/Resource/1",
				"/Statement/1/Condition/IpAddress/aws:SourceIp/1", "/Statement/1/Condition/Bool/aws:SecureTransport",
			},
			none,
		},
		{
			"identity: faults of conditions and then of the rest", identityKind,
			statementDoc(`"Effect":"Allow","Action":"x","Resource":"*","Condition":{"Foo":{"k":"v"},"Bool":{}}`),
			[]string{"/Statement/0/Condition/Bool", "/Statement/0/Condition/Foo", "/Statement/0/Action"},
			none,
		},
		{"identity: fields twice in two statements", identityKind, `{"Version":"2012-10-17","Statement":[{"Sid":"a","Sid":"b"},{"x":1,"x":2}]}`, []string{"/Statement/0/Sid", "/Statement/1/x", "/Statement/1/x"}, none},

		// A bucket's policy.
		{"bucket: too large", bucketKind, tooLarge, []string{""}, none},
		{"bucket: not JSON", bucketKind, `Principal: *`, []string{""}, 1},
		{"bucket: data after the document", bucketKind, statementDoc(bucketBody) + `{}`, []string{""}, int64(len(statementDoc(bucketBody)))},
		{"bucket: a closing brace after the document", bucketKind, statementDoc(bucketBody) + "\t}", []string{""}, int64(len(statementDoc(bucketBody)) + 1)},
		{"bucket: another bucket, then a closing bracket", bucketKind, statementDoc(`"Principal":"*","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"`) + `]`, []string{"/Statement/0/Resource", ""}, none},
		{"bucket: NotPrincipal", bucketKind, statementDoc(`"NotPrincipal":"*","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/NotPrincipal"}, none},
		{"bucket: a principal of another kind", bucketKind, statementDoc(`"Principal":{"Service":"s3.amazonaws.com"},"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Principal/Service"}, none},
		{"bucket: a Sid that is a number", bucketKind, statementDoc(`"Sid":1,` + bucketBody), []string{"/Statement/0/Sid"}, none},
		{"bucket: a Statement that is a string", bucketKind, `{"Version":"2012-10-17","Statement":"x"}`, []string{"/Statement"}, none},
		{"bucket: a null Condition", bucketKind, statementDoc(bucketBody + `,"Condition":null`), []string{"/Statement/0/Condition"}, none},
		{"bucket: no principal", bucketKind, statementDoc(identityBody), []string{"/Statement/0"}, none},
		{"bucket: a principal that is a name", bucketKind, statementDoc(`"Principal":"alice","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Principal"}, none},
		{"bucket: a principal that is a list", bucketKind, statementDoc(`"Principal":["*"],"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Principal"}, none},
		{"bucket: an AWS that is a number", bucketKind, statementDoc(`"Principal":{"AWS":1},"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Principal/AWS"}, none},
		{"bucket: a principal naming nobody", bucketKind, statementDoc(`"Principal":{"AWS":[]},"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Principal/AWS"}, none},
		{"bucket: a principal with no AWS", bucketKind, statementDoc(`"Principal":{},"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Principal"}, none},
		{"bucket: a user that is a pattern", bucketKind, statementDoc(`"Principal":{"AWS":"arn:aws:iam:::user/*"},"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Principal/AWS"}, none},
		{
			"bucket: two accounts among users", bucketKind,
			statementDoc(`"Principal":{"AWS":["arn:aws:iam:::user/alice","arn:aws:iam::1:root","*","x"]},"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`),
			[]string{"/Statement/0/Principal/AWS/1", "/Statement/0/Principal/AWS/3"},
			none,
		},
		{
			// Three faults at one place: none hides another.
			"bucket: a statement of a Resource only", bucketKind,
			`{"Version":"2012-10-17","Statement":[{"Resource":"arn:aws:s3:::site/*"}]}`,
			[]string{"/Statement/0", "/Statement/0", "/Statement/0"},
			none,
		},
		{"bucket: what an identity policy refuses", bucketKind, statementDoc(`"Principal":"*","Effect":"Perhaps","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Effect"}, none},
		{"bucket: no Version", bucketKind, `{"Statement":[{` + bucketBody + `}]}`, []string{""}, none},
		{"bucket: no Statement", bucketKind, `{"Version":"2012-10-17"}`, []string{""}, none},
		{"bucket: an Effect that is a number", bucketKind, statementDoc(`"Principal":"*","Effect":1,"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Effect"}, none},
		{"bucket: an administration action", bucketKind, statementDoc(`"Principal":"*","Effect":"Allow","Action":"admin:ListUsers","Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Action"}, none},
		{"bucket: an action no wildcard grants", bucketKind, statementDoc(`"Principal":"*","Effect":"Allow","Action":["s3:GetObject","s3:ReplicateObject"],"Resource":"arn:aws:s3:::site/*"`), []string{"/Statement/0/Action/1"}, none},
		{"bucket: another bucket", bucketKind, statementDoc(`"Principal":"*","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"`), []string{"/Statement/0/Resource"}, none},
		{
			"bucket: faults of principals and of the statement", bucketKind,
			`{"Version":"2012-10-17","Statement":[{"Principal":"bob","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"},` +
				`{"Principal":"*","Effect":"Never","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*","Condition":null}]}`,
			[]string{"/Statement/1/Condition", "/Statement/0/Principal", "/Statement/1/Effect"},
			none,
		},

		// A sub-bucket's policy.
		{"sub-bucket: too large", subBucketKind, tooLarge, []string{""}, none},
		{"sub-bucket: data after the document", subBucketKind, statementDoc(subBucketBody) + ` x`, []string{""}, int64(len(statementDoc(subBucketBody)) + 1)},
		{"sub-bucket: a closing bracket after the document", subBucketKind, statementDoc(subBucketBody) + `]`, []string{""}, int64(len(statementDoc(subBucketBody)))},
		{"sub-bucket: anyone, then a closing brace", subBucketKind, statementDoc(`"Principal":"*","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"`) + `}`, []string{"/Statement/0/Principal", ""}, none},
		{"sub-bucket: what a bucket policy refuses, then a closing brace", subBucketKind, statementDoc(`"Principal":"*","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"`) + `}`, []string{"/Statement/0/Resource", ""}, none},
		{"sub-bucket: what a bucket policy refuses", subBucketKind, statementDoc(`"Principal":"*","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"`), []string{"/Statement/0/Resource"}, none},
		{"sub-bucket: anyone", subBucketKind, statementDoc(`"Principal":"*","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"`), []string{"/Statement/0/Principal"}, none},
		{"sub-bucket: anyone, as AWS", subBucketKind, statementDoc(`"Principal":{"AWS":"*"},"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"`), []string{"/Statement/0/Principal/AWS"}, none},
		{"sub-bucket: anyone among users", subBucketKind, statementDoc(`"Principal":{"AWS":["arn:aws:iam:::user/alice","*"]},"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"`), []string{"/Statement/0/Principal/AWS/1"}, none},
		{"sub-bucket: a wildcard", subBucketKind, statementDoc(`"Principal":{"AWS":"arn:aws:iam:::user/alice"},"Effect":"Allow","Action":"s3:Get*","Resource":"arn:aws:s3:::site/reports/*"`), []string{"/Statement/0/Action"}, none},
		{"sub-bucket: denying a listing", subBucketKind, statementDoc(`"Principal":{"AWS":"arn:aws:iam:::user/alice"},"Effect":"Deny","Action":["s3:GetObject","s3:ListBucket"],"Resource":"arn:aws:s3:::site"`), []string{"/Statement/0/Action/1"}, none},
		{"sub-bucket: another sub-bucket", subBucketKind, statementDoc(`"Principal":{"AWS":"arn:aws:iam:::user/alice"},"Effect":"Allow","Action":"s3:GetObject","Resource":["arn:aws:s3:::site/reports/*","arn:aws:s3:::site/drafts/*"]`), []string{"/Statement/0/Resource/1"}, none},
		{
			"sub-bucket: every fault of two statements", subBucketKind,
			`{"Version":"2012-10-17","Statement":[{` + subBucketBody + `},{"Principal":"*","Effect":"Allow","Action":["s3:PutBucketPolicy","s3:GetObject"],"Resource":"arn:aws:s3:::site/*"}]}`,
			[]string{"/Statement/1/Principal", "/Statement/1/Action/0", "/Statement/1/Resource"},
			none,
		},
	}
}

// TestFaultsAreLocated: every refusal of the three parsers says where it is,
// reads as it read before the faults were located, and wraps the same
// sentinels.
func TestFaultsAreLocated(t *testing.T) {
	t.Parallel()
	for _, row := range faultRows() {
		_, err := row.kind.parse([]byte(row.doc))
		var refused *PolicyError
		if !errors.As(err, &refused) {
			t.Errorf("%s: %v, want a *PolicyError", row.name, err)
			continue
		}
		faults := refused.Faults()
		var pointers []string
		for _, f := range faults {
			pointers = append(pointers, f.Pointer)
		}
		if !slices.Equal(pointers, row.pointers) {
			t.Errorf("%s: faults at %q, want %q\n%v", row.name, pointers, row.pointers, faults)
		}
		if faults[0].Offset != row.offset {
			t.Errorf("%s: offset %d, want %d", row.name, faults[0].Offset, row.offset)
		}
		for _, f := range faults {
			// Data after the document has its offset wherever it is:
			// where that data begins.
			if f.Reason == dataAfterReason {
				if at, stray := strayAfter([]byte(row.doc)); !stray || f.Offset != at {
					t.Errorf("%s: data after the document at %d, read apart at %d (%v)", row.name, f.Offset, at, stray)
				}
			}
		}
		for _, f := range faults[1:] {
			if f.Offset != NoOffset && f.Reason != dataAfterReason {
				t.Errorf("%s: a fault after the first has the offset %d", row.name, f.Offset)
			}
		}
		_, before := row.kind.oracle([]byte(row.doc))
		if before == nil {
			// The one refusal the frozen parsers did not make.
			if !refusedForStrayClose(row.kind, []byte(row.doc), err) {
				t.Errorf("%s: reads %q, was accepted", row.name, err)
			}
		} else {
			if err.Error() != before.Error() {
				t.Errorf("%s: reads %q, read %v", row.name, err, before)
			}
			holdSentinels(t, row.name, err, before)
		}
		for _, f := range faults {
			if want := expectedStatement(f.Pointer); f.Statement != want {
				t.Errorf("%s: the fault at %q is of statement %d, want %d", row.name, f.Pointer, f.Statement, want)
			}
			if !strings.HasPrefix(f.Reason, "invalid argument: policy: ") {
				t.Errorf("%s: the reason %q is not a policy's", row.name, f.Reason)
			}
		}
		if refused.Truncated() {
			t.Errorf("%s: truncated", row.name)
		}
	}

	// Faults at one place are each said, once: of a bucket statement of a
	// Resource only, that it has no Principal, no Effect and no Action.
	_, err := ParseBucketPolicy([]byte(`{"Version":"2012-10-17","Statement":[{"Resource":"arn:aws:s3:::site/*"}]}`), "site")
	var refused *PolicyError
	if !errors.As(err, &refused) {
		t.Fatalf("a statement of a Resource only: %v", err)
	}
	says := []string{"Principal is required", "Effect must be Allow or Deny", `action "": want`}
	if faults := refused.Faults(); len(faults) != len(says) {
		t.Errorf("a statement of a Resource only: %v", faults)
	} else {
		for i, fault := range faults {
			if !strings.Contains(fault.Reason, says[i]) {
				t.Errorf("a statement of a Resource only: fault %d reads %q, want %q", i, fault.Reason, says[i])
			}
		}
	}

	// A name is no fault of the document: it is refused as before, with no
	// faults.
	for _, names := range [][2]string{{"Site", "reports"}, {"site", "Reports"}, {"site", ""}} {
		_, err := ParseSubBucketPolicy([]byte(statementDoc(subBucketBody)), names[0], names[1])
		var refused *PolicyError
		if err == nil || errors.As(err, &refused) || errors.Is(err, domain.ErrInvalidPolicy) {
			t.Errorf("names %q: %v, want an error of the name with no faults", names, err)
		}
	}

	// Validate says what only a document made by hand can have.
	for _, tt := range []struct {
		doc     Document
		pointer string
	}{
		{Document{Version: PolicyVersion, Statements: []Statement{{Effect: Allow, Actions: []string{"*"}, Resources: []string{"*"}, Conditions: []Condition{
			{Operator: OpBool, Key: KeySecureTransport, Values: []string{"true"}}, {Operator: OpBool, Key: KeySecureTransport, Values: []string{"false"}},
		}}}}, "/Statement/0/Condition/Bool/aws:SecureTransport"},
		{Document{Version: PolicyVersion, Statements: []Statement{{Effect: Allow, Actions: []string{"*"}, Resources: []string{"*"}, Conditions: []Condition{
			{Operator: OpStringEquals, Key: "aws:Referer", Values: []string{"x"}},
		}}}}, "/Statement/0/Condition/StringEquals/aws:Referer"},
		{Document{Version: PolicyVersion, Statements: []Statement{{Effect: Allow, Actions: []string{"*", "x"}, Resources: []string{"*"}}}}, "/Statement/0/Action/1"},
	} {
		var refused *PolicyError
		if err := tt.doc.Validate(); !errors.As(err, &refused) || refused.Faults()[0].Pointer != tt.pointer || !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Validate(%+v) = %v, want a fault at %q", tt.doc, err, tt.pointer)
		}
		if err := oracleValidate(tt.doc); err == nil || err.Error() != tt.doc.Validate().Error() {
			t.Errorf("Validate(%+v) reads %q, read %v", tt.doc, tt.doc.Validate(), err)
		}
	}
}

// TestFaultsAreBounded: a document with more faults than are collected is
// refused with the first MaxPolicyFaults, and says it had more.
func TestFaultsAreBounded(t *testing.T) {
	t.Parallel()
	bad := strings.TrimSuffix(strings.Repeat(`{"Effect":"x","Action":["a","b"],"Resource":"c"},`, MaxPolicyStatements), ",")
	for _, kind := range []policyKind{identityKind, bucketKind, subBucketKind} {
		doc := `{"Version":"2012-10-17","Statement":[` + bad + `]}`
		if kind != identityKind {
			doc = strings.ReplaceAll(doc, `{"Effect"`, `{"Principal":"*","Effect"`)
		}
		_, err := kind.parse([]byte(doc))
		var refused *PolicyError
		if !errors.As(err, &refused) {
			t.Fatalf("%s: %v", kind, err)
		}
		if len(refused.Faults()) != MaxPolicyFaults || !refused.Truncated() {
			t.Errorf("%s: %d faults, truncated %v; want %d and true", kind, len(refused.Faults()), refused.Truncated(), MaxPolicyFaults)
		}
		if _, before := kind.oracle([]byte(doc)); before == nil || before.Error() != err.Error() {
			t.Errorf("%s: reads %q, read %v", kind, err, before)
		}
	}
	// One made without a fault reads as an invalid argument, and does not
	// fail.
	if zero := (&PolicyError{}); zero.Error() != domain.ErrInvalidArgument.Error() || len(zero.Faults()) != 0 {
		t.Errorf("a PolicyError without faults reads %q", zero.Error())
	}
	// One fewer than the bound is not truncated.
	few := `{"Version":"2012-10-17","Statement":[` + strings.TrimSuffix(strings.Repeat(`{"Effect":"x","Action":"s3:*","Resource":"*"},`, 50), ",") + `]}`
	var refused *PolicyError
	if _, err := ParsePolicy([]byte(few)); !errors.As(err, &refused) || len(refused.Faults()) != 50 || refused.Truncated() {
		t.Errorf("50 faults: %v", err)
	}
}

// dataAfterReason is the reason of data after the document.
var dataAfterReason = invalidPolicy("data after the document").Error()

// strayAfter reports whether data begins with one JSON value followed by
// something other than JSON white space, and the byte offset at which that
// begins: read apart from the parsers, with encoding/json.
func strayAfter(data []byte) (int64, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var first json.RawMessage
	if decoder.Decode(&first) != nil {
		return 0, false
	}
	rest := bytes.TrimLeft(data[decoder.InputOffset():], " \t\r\n")
	if len(rest) == 0 {
		return 0, false
	}
	return int64(len(data) - len(rest)), true
}

// refusedForStrayClose reports whether err is the one refusal the parsers
// make of a document the frozen parsers accepted (PLAN 9.2a): the document
// is followed by a closing bracket - the one text after a document those
// took - and its only fault is data after the document, at that bracket,
// wrapping the sentinels of the parser.
func refusedForStrayClose(kind policyKind, data []byte, err error) bool {
	at, stray := strayAfter(data)
	var refused *PolicyError
	if !stray || (data[at] != '}' && data[at] != ']') || !errors.As(err, &refused) {
		return false
	}
	want := PolicyFault{Pointer: "", Statement: NoStatement, Reason: dataAfterReason, Offset: at}
	faults := refused.Faults()
	sentinels := errors.Is(err, domain.ErrInvalidArgument) && errors.Is(err, domain.ErrInvalidPolicy) == (kind == subBucketKind) &&
		!errors.Is(err, domain.ErrInvalidBucketName)
	return len(faults) == 1 && faults[0] == want && err.Error() == dataAfterReason && sentinels && !refused.Truncated()
}

// holdToOracle fails unless a parser's result and error are the frozen
// parser's: the same documents accepted, to the same result; the same
// refused, with the same reason (sameReason) and the same sentinels. It
// reports whether the document was refused.
func holdToOracle(t *testing.T, name string, data []byte, got any, err error, was any, before error) bool {
	t.Helper()
	if (err == nil) != (before == nil) {
		t.Fatalf("%s: accepted %v, was accepted %v: %v / %v\n%q", name, err == nil, before == nil, err, before, data)
	}
	if err == nil {
		if !reflect.DeepEqual(got, was) {
			t.Fatalf("%s: parsed as %+v, was %+v", name, got, was)
		}
		return false
	}
	if !sameReason(data, err.Error(), before.Error()) {
		t.Fatalf("%s: reads %q, read %q\n%q", name, err, before, data)
	}
	holdSentinels(t, name, err, before)
	return true
}

// expectedStatement is the statement a pointer is in, read apart from the
// parser: the index after "/Statement/".
func expectedStatement(pointer string) int {
	parts := strings.Split(pointer, "/")
	if len(parts) < 3 || parts[1] != "Statement" {
		return NoStatement
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n < 0 || strconv.Itoa(n) != parts[2] {
		return NoStatement
	}
	return n
}

// holdSentinels fails unless err and before wrap the same sentinels.
func holdSentinels(t *testing.T, name string, err, before error) {
	t.Helper()
	for _, sentinel := range []error{domain.ErrInvalidArgument, domain.ErrInvalidPolicy, domain.ErrInvalidBucketName} {
		if errors.Is(err, sentinel) != errors.Is(before, sentinel) {
			t.Errorf("%s: wraps %v: %v, wrapped it: %v", name, sentinel, errors.Is(err, sentinel), errors.Is(before, sentinel))
		}
	}
}

// sameReason reports whether a reason is the one the frozen parser gave of
// data, or one of those it could have given: of two operators that test
// nothing, or two keys or operators not supported, it named the one a Go map
// gave it first. The parsers now name the first as written. Both reasons
// must be of one kind, about one statement, and name what is really there:
// an operator of that statement's Condition, or a key of one of its
// operators.
func sameReason(data []byte, now, before string) bool {
	if now == before {
		return true
	}
	a, aOK := readUnordered(now)
	b, bOK := readUnordered(before)
	if !aOK || !bOK || a.statement != b.statement || a.kind != b.kind {
		return false
	}
	return namedInCondition(data, a) && namedInCondition(data, b)
}

// The kinds of reason whose operand a Go map chose.
const (
	// Each is the index of its group in readUnordered's expression.
	reasonTestsNothing = iota + 2
	reasonKeyNotSupported
	reasonOperatorNotSupported
)

// unorderedReason is a reason whose operand a Go map chose: of which
// statement, of which kind, and the operator or key it names.
type unorderedReason struct {
	statement string
	kind      int
	name      string
}

// readUnordered reads a reason whose operand a Go map chose. Which kind it
// is, is told by which alternative matched - not by whether its name is
// empty, as the name of an operator may be.
func readUnordered(reason string) (unorderedReason, bool) {
	unordered := regexp.MustCompile(`(?s)^invalid argument: policy: statement (\d+): condition (?:(.*) tests nothing|key (".*") is not supported|operator (".*") is not supported)$`)
	at := unordered.FindStringSubmatchIndex(reason)
	if at == nil {
		return unorderedReason{}, false
	}
	read := unorderedReason{statement: reason[at[2]:at[3]]}
	for kind := reasonTestsNothing; kind <= reasonOperatorNotSupported; kind++ {
		if at[2*kind] < 0 {
			continue
		}
		read.kind, read.name = kind, reason[at[2*kind]:at[2*kind+1]]
		if kind == reasonTestsNothing {
			return read, true
		}
		name, err := strconv.Unquote(read.name)
		read.name = name
		return read, err == nil
	}
	return unorderedReason{}, false
}

// namedInCondition reports whether the operator or key of a reason is a
// field, at its depth, of the Condition of its statement in data: read apart
// from the parser.
func namedInCondition(data []byte, reason unorderedReason) bool {
	var document map[string]any
	if json.NewDecoder(strings.NewReader(string(data))).Decode(&document) != nil {
		return false
	}
	statements, _ := document["Statement"].([]any)
	i, err := strconv.Atoi(reason.statement)
	if err != nil || i < 0 || i >= len(statements) {
		return false
	}
	statement, _ := statements[i].(map[string]any)
	condition, _ := statement["Condition"].(map[string]any)
	switch reason.kind {
	case reasonTestsNothing, reasonOperatorNotSupported:
		_, ok := condition[reason.name]
		return ok
	default:
		key := reason.name
		for _, tests := range condition {
			if tests, ok := tests.(map[string]any); ok {
				if _, ok := tests[key]; ok {
					return true
				}
			}
		}
		return false
	}
}

// resolves reports whether a JSON pointer names a value of the document - the
// first, of a text that has data after it - read apart from the parser. Of
// a text that is no JSON there is nothing to name, and every pointer is
// taken: it says where the parser was when it met the fault.
func resolves(data []byte, pointer string) bool {
	var value any
	if pointer == "" || json.NewDecoder(strings.NewReader(string(data))).Decode(&value) != nil {
		return true
	}
	for _, token := range strings.Split(pointer[1:], "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			next, ok := v[token]
			if !ok {
				return false
			}
			value = next
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(v) || strconv.Itoa(i) != token {
				return false
			}
			value = v[i]
		default:
			return false
		}
	}
	return true
}

// FuzzPolicyFaults: for each of the three parsers, whatever bytes arrive, a
// document is accepted exactly when it has no fault, and the error of one
// that is refused reads as its first fault. Each fault is of a value that
// is there in the document (but one of syntax, which has an offset), of the
// statement its pointer is in. And the parsers accept and refuse what the
// frozen parsers did: the same documents, to the same result; the same
// refusals, with the same text and the same sentinels - but for one class
// (PLAN 9.2a): a document followed by a closing bracket, which they took and
// the parsers refuse as data after the document, its only fault. What the
// parsers accept is JSON. A stored bucket policy is read as the frozen
// parser read it, that class included, and its StoredPolicyDocument is JSON.
func FuzzPolicyFaults(f *testing.F) {
	for _, row := range faultRows() {
		f.Add([]byte(row.doc))
	}
	f.Add([]byte(statementDoc(identityBody)))
	f.Add([]byte(statementDoc(bucketBody)))
	f.Add([]byte(statementDoc(subBucketBody)))
	f.Add([]byte(statementDoc(identityBody + `,"Condition":{"A":{},"B":{}}`)))
	f.Add([]byte(statementDoc(identityBody + `,"Condition":{"Bool":{"k1":"x","k2":"y"},"X":{"aws:SourceIp":"1"},"Y":{"aws:SourceIp":"1"}}`)))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, kind := range []policyKind{identityKind, bucketKind, subBucketKind} {
			// The reading that does not locate refuses what the reading
			// that does refuses, for the same first reason - but for the
			// order of a map.
			quick, located := &faultList{}, &faultList{locate: true}
			kind.into(quick, data)
			kind.into(located, data)
			if quick.failed() != located.failed() {
				t.Fatalf("%s: refused %v without locating, %v locating\n%q", kind, quick.failed(), located.failed(), data)
			}
			if quick.failed() && !sameReason(data, quick.faults[0].Reason, located.faults[0].Reason) {
				t.Fatalf("%s: %q without locating, %q locating\n%q", kind, quick.faults[0].Reason, located.faults[0].Reason, data)
			}
			got, err := kind.parse(data)
			was, before := kind.oracle(data)
			// A bucket's policy already stored is read as the frozen
			// parser read it: a closing bracket after it is taken. What
			// is read is StoredPolicyDocument, which is JSON, and the
			// parser of a put takes it to the same policy. A record that
			// is JSON is its own document.
			if kind == bucketKind {
				stored, storedErr := ParseStoredBucketPolicy(data, "site")
				holdToOracle(t, "bucket, stored", data, stored, storedErr, was, before)
				document := StoredPolicyDocument(data)
				if json.Valid(data) && !bytes.Equal(document, data) {
					t.Fatalf("StoredPolicyDocument changed a record that is JSON\n%q\n%q", data, document)
				}
				if storedErr == nil {
					again, err := ParseBucketPolicy(document, "site")
					if err != nil || !json.Valid(document) || !reflect.DeepEqual(again, stored) {
						t.Fatalf("StoredPolicyDocument of a stored policy: %v\n%q\n%q", err, data, document)
					}
				}
			}
			// What is accepted is one JSON value and nothing else.
			if err == nil && !json.Valid(data) {
				t.Fatalf("%s: accepted what is not JSON\n%q", kind, data)
			}
			if err != nil && before == nil {
				// The one refusal the frozen parsers did not make.
				if !refusedForStrayClose(kind, data, err) {
					t.Fatalf("%s: refused with %q, was accepted\n%q", kind, err, data)
				}
				continue
			}
			if !holdToOracle(t, kind.String(), data, got, err, was, before) {
				continue
			}
			var refused *PolicyError
			if !errors.As(err, &refused) {
				t.Fatalf("%s: %v is no *PolicyError", kind, err)
			}
			faults := refused.Faults()
			if len(faults) == 0 || len(faults) > MaxPolicyFaults || (refused.Truncated() && len(faults) != MaxPolicyFaults) {
				t.Fatalf("%s: %d faults, truncated %v", kind, len(faults), refused.Truncated())
			}
			if err.Error() != faults[0].Reason {
				t.Fatalf("%s: reads %q, its first fault %q", kind, err, faults[0].Reason)
			}
			for i, fault := range faults {
				if fault.Offset == NoOffset && !resolves(data, fault.Pointer) {
					t.Fatalf("%s: fault %d is at %q, which is not in the document\n%q", kind, i, fault.Pointer, data)
				}
				if fault.Offset != NoOffset && (fault.Offset < 0 || fault.Offset > int64(len(data))) {
					t.Fatalf("%s: fault %d is at offset %d of %d bytes", kind, i, fault.Offset, len(data))
				}
				if want := expectedStatement(fault.Pointer); fault.Statement != want && fault.Offset == NoOffset {
					t.Fatalf("%s: the fault at %q is of statement %d, want %d", kind, fault.Pointer, fault.Statement, want)
				}
			}
		}
	})
}

// TestSameReason: two reasons are taken for one only when both name, of one
// statement, an operator or a key that is there.
func TestSameReason(t *testing.T) {
	t.Parallel()
	data := []byte(statementDoc(identityBody + `,"Condition":{"":{},"A":{},"B":{},"Bool":{"k1":"x","k2":"y"}}`))
	reason := func(s string) string { return "invalid argument: policy: statement 0: condition " + s }
	for _, tt := range []struct {
		now, before string
		want        bool
	}{
		{reason("A tests nothing"), reason("B tests nothing"), true},
		{reason(" tests nothing"), reason("A tests nothing"), true},
		{reason(`operator "" is not supported`), reason(`operator "B" is not supported`), true},
		{reason(" tests nothing"), reason(`operator "" is not supported`), false},
		{reason(`key "k1" is not supported`), reason(`key "k2" is not supported`), true},
		{reason(`operator "A" is not supported`), reason(`operator "B" is not supported`), true},
		{reason("A tests nothing"), reason("C tests nothing"), false},
		{reason(`key "k1" is not supported`), reason(`key "A" is not supported`), false},
		{reason(`operator "A" is not supported`), reason(`operator "k1" is not supported`), false},
		{reason("A tests nothing"), reason(`operator "B" is not supported`), false},
		{reason("A tests nothing"), "invalid argument: policy: statement 1: condition B tests nothing", false},
	} {
		if got := sameReason(data, tt.now, tt.before); got != tt.want {
			t.Errorf("sameReason(%q, %q) = %v, want %v", tt.now, tt.before, got, tt.want)
		}
	}
}

// acceptedDocument is a document the parsers accept, for the benchmarks.
type acceptedDocument struct {
	name string
	kind policyKind
	doc  []byte
}

// acceptedDocuments are a small document, and identity and bucket policies
// of 60 statements with lists and conditions.
func acceptedDocuments() []acceptedDocument {
	statements := func(principal string) string {
		var parts []string
		for i := range 60 {
			parts = append(parts, `{"Sid":"s`+strconv.Itoa(i)+`","Effect":"Allow",`+principal+
				`"Action":["s3:GetObject","s3:PutObject"],"Resource":["arn:aws:s3:::site/p`+strconv.Itoa(i)+`/*"],`+
				`"Condition":{"Bool":{"aws:SecureTransport":"true"},"IpAddress":{"aws:SourceIp":["10.0.0.0/8"]}}}`)
		}
		return `{"Version":"2012-10-17","Statement":[` + strings.Join(parts, ",") + `]}`
	}
	return []acceptedDocument{
		{"small", identityKind, []byte(statementDoc(identityBody))},
		{"identity-60", identityKind, []byte(statements(""))},
		{"bucket-60", bucketKind, []byte(statements(`"Principal":{"AWS":["arn:aws:iam:::user/alice","*"]},`))},
	}
}

// BenchmarkParseAccepted: a document that is accepted is read once, without
// a tree or a pointer, and costs what it did before its faults were located
// ("before" is the frozen parser).
func BenchmarkParseAccepted(b *testing.B) {
	for _, d := range acceptedDocuments() {
		if _, err := d.kind.parse(d.doc); err != nil {
			b.Fatalf("%s: %v", d.name, err)
		}
		b.Run(d.name+"/now", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := d.kind.parse(d.doc); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(d.name+"/before", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := d.kind.oracle(d.doc); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// manyKeys is a document of at most MaxPolicyBytes whose one Condition
// operator has as many keys as fit - some 2 900, of two characters each,
// whose values are numbers. Every field is checked for being given twice
// before the first value is refused.
func manyKeys() []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var keys []string
	doc := ""
	for i := 0; i < len(alphabet)*len(alphabet); i++ {
		keys = append(keys, `"`+alphabet[i/len(alphabet):i/len(alphabet)+1]+alphabet[i%len(alphabet):i%len(alphabet)+1]+`":0`)
		next := statementDoc(identityBody + `,"Condition":{"Bool":{` + strings.Join(keys, ",") + `}}`)
		if len(next) > MaxPolicyBytes {
			break
		}
		doc = next
	}
	return []byte(doc)
}

// TestManyKeysAreRefused: the document of BenchmarkRefuseManyKeys is what
// it says, and is refused as before.
func TestManyKeysAreRefused(t *testing.T) {
	t.Parallel()
	doc := manyKeys()
	_, err := ParsePolicy(doc)
	_, before := oracleParsePolicy(doc)
	var refused *PolicyError
	if !errors.As(err, &refused) || !refused.Truncated() || before == nil || !sameReason(doc, err.Error(), before.Error()) {
		t.Errorf("%d bytes: %v, before %v", len(doc), err, before)
	}
	if keys := strings.Count(string(doc), `":0`); keys < 2800 {
		t.Errorf("only %d keys", keys)
	}
}

// BenchmarkRefuseManyKeys: refusing an object of thousands of fields takes
// time in proportion to them, not to their square.
func BenchmarkRefuseManyKeys(b *testing.B) {
	doc := manyKeys()
	for _, run := range []struct {
		name  string
		parse func([]byte) (Document, error)
	}{{"now", ParsePolicy}, {"before", oracleParsePolicy}} {
		b.Run(run.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := run.parse(doc); err == nil {
					b.Fatal("accepted")
				}
			}
		})
	}
}
