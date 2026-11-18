//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/test/clustertest"
)

// builderCall sends one request to the management API of a node with the
// body exactly as given - a policy document keeps its spaces and the order
// of its keys - and returns the status, the headers and the body of the
// answer.
func builderCall(ctx context.Context, c *clustertest.Cluster, id string, headers map[string]string, method, path, body string) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+id+":9001/api/v1"+path, reader)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.Admin(id).Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s through %s: %w", method, path, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s through %s: %w", method, path, id, err)
	}
	return resp.StatusCode, resp.Header, data, nil
}

// builderCited is a statement an explanation cites (CitedStatement).
type builderCited struct {
	Source         string `json:"source"`
	Effect         string `json:"effect"`
	Readable       bool   `json:"readable"`
	Policy         string `json:"policy"`
	StatementIndex *int   `json:"statement_index"`
	Sid            string `json:"sid"`
}

// String puts a cited statement in one line, for comparing and telling.
func (s builderCited) String() string {
	index := "-"
	if s.StatementIndex != nil {
		index = strconv.Itoa(*s.StatementIndex)
	}
	return fmt.Sprintf("%s %s readable=%t policy=%q statement=%s sid=%q", s.Source, s.Effect, s.Readable, s.Policy, index, s.Sid)
}

// builderCitedLines is every cited statement, one line each, in the order
// given.
func builderCitedLines(cited []builderCited) []string {
	lines := make([]string, 0, len(cited))
	for _, s := range cited {
		lines = append(lines, s.String())
	}
	return lines
}

// builderExplanation is the answer of GET /access/explain
// (AccessExplanation).
type builderExplanation struct {
	Subject struct {
		Kind        string `json:"kind"`
		User        string `json:"user"`
		AccessKeyID string `json:"access_key_id"`
	} `json:"subject"`
	Request struct {
		Action   string `json:"action"`
		Resource string `json:"resource"`
	} `json:"request"`
	Authenticates bool           `json:"authenticates"`
	Allowed       *bool          `json:"allowed"`
	Source        string         `json:"source"`
	Statements    []builderCited `json:"statements"`
	// UnknownAddress is what was decided without a source address: none of
	// these questions names a condition on it, so it is empty, and a cited
	// statement can not hide there.
	UnknownAddress []builderCited  `json:"unknown_address"`
	KeyPolicy      json.RawMessage `json:"key_policy"`
	Assumed        struct {
		SecureTransport bool `json:"secure_transport"`
	} `json:"assumed"`
	NotConsidered []string `json:"not_considered"`
}

// builderAuditRecord is a record of the audit trail, as GET /audit gives it.
type builderAuditRecord struct {
	Sequence    int64             `json:"sequence"`
	Actor       string            `json:"actor"`
	AccessKeyID string            `json:"access_key_id"`
	Action      string            `json:"action"`
	Resource    string            `json:"resource"`
	Outcome     string            `json:"outcome"`
	RequestID   string            `json:"request_id"`
	Details     map[string]string `json:"details"`
}

// builderAudit reads the whole audit trail through a node, a page after
// another. It is previewAudit's walk with this test's record and its own
// call: builderCall sends a body as written, where gridCall marshals it.
func builderAudit(ctx context.Context, c *clustertest.Cluster, id string, root map[string]string) ([]builderAuditRecord, error) {
	var all []builderAuditRecord
	after := int64(0)
	for {
		status, _, body, err := builderCall(ctx, c, id, root, http.MethodGet, "/audit?limit=1000&after="+strconv.FormatInt(after, 10), "")
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("GET /audit through %s = %d %s", id, status, body)
		}
		var page struct {
			Records []builderAuditRecord `json:"records"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("GET /audit through %s: %w: %s", id, err, body)
		}
		if len(page.Records) == 0 {
			return all, nil
		}
		all = append(all, page.Records...)
		after = page.Records[len(page.Records)-1].Sequence
	}
}

// builderS3Outcome says what a node did with a request of the SDK: true when
// it was carried out, false when it was refused with 403 AccessDenied, and
// an error for any other answer - which is neither of the two an
// explanation can foretell.
func builderS3Outcome(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case statusOf(err) == http.StatusForbidden && apiCode(err) == "AccessDenied":
		return false, nil
	default:
		return false, fmt.Errorf("neither carried out nor refused with 403 AccessDenied: %d %q: %w", statusOf(err), apiCode(err), err)
	}
}

// builderCompact is a document as the management API answers it: without
// the white space it was sent with.
func builderCompact(t *testing.T, document string) string {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(document)); err != nil {
		t.Fatalf("compact %q: %v", document, err)
	}
	return compact.String()
}

// builderExplained is an explanation the test asked for and what its record
// in the audit trail must say.
type builderExplained struct {
	what      string
	requestID string
	outcome   string
	resource  string
	details   map[string]string
}

// TestPolicyBuilderAcrossTheCluster: the policy builder in the product as it
// is deployed (ADR-0032, PLAN 9.11). Four nodes; the changes, the questions
// and the requests are sent to different nodes, so that every answer is
// given on replicated state.
//
//   - A policy changed through node3 is explained on node2, and node1 then
//     decides a real S3 request of the key explained - allowed, refused by
//     a Deny, refused by default: the explanation's verdict and the
//     statements it cites are what node1 did.
//   - Validation through one node refuses what a put through another
//     refuses, with the same faults, and takes what it takes, for the
//     identity policies (a named policy, created and updated, and the
//     inline policy of a key), a bucket's and a sub-bucket's.
//   - A bucket's policy set through S3 on one node is the document the
//     management API reads on another, and back, byte for byte.
//   - Every explanation is in the audit trail, read through a node that
//     answered none of them, before its answer was given.
func TestPolicyBuilderAcrossTheCluster(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	c := clustertest.Start(t, clustertest.Options{})
	root := gridBearer(c.AccessKey, c.SecretKey)

	// mustAdmin makes a change through a node and fails the test unless it
	// is answered with want.
	mustAdmin := func(t *testing.T, id string, want int, method, path, body string) {
		t.Helper()
		status, _, answer, err := builderCall(ctx, c, id, root, method, path, body)
		if err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Fatalf("%s %s through %s = %d %s, want %d", method, path, id, status, answer, want)
		}
	}

	var explained []builderExplained

	// An explanation on one node is what another decides.
	{
		const (
			nodeA, nodeB, nodeC = "node1", "node2", "node3"
			bucketName          = "explained"
			policyName          = "dora-access"
		)
		before := `{"Version":"2012-10-17","Statement":[` +
			`{"Sid":"ReadPublic","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::explained/public/*"}]}`
		after := `{"Version":"2012-10-17","Statement":[` +
			`{"Sid":"ReadPublic","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::explained/public/*"},` +
			`{"Sid":"ReadReports","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::explained/reports/*"},` +
			`{"Sid":"NoSecrets","Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::explained/reports/secret/*"}]}`

		// dora reads through a role whose one policy is changed through
		// node3 alone.
		if _, err := c.S3(nodeC).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)}); err != nil {
			t.Fatalf("CreateBucket through %s: %v", nodeC, err)
		}
		mustAdmin(t, nodeC, http.StatusCreated, http.MethodPost, "/policies", `{"name":"`+policyName+`","document":`+before+`}`)
		mustAdmin(t, nodeC, http.StatusCreated, http.MethodPost, "/roles", `{"name":"dora-reader","policies":["`+policyName+`"]}`)
		doraID, doraSecret, err := gridUser(ctx, c, nodeC, root, "dora", map[string]any{"roles": []string{"dora-reader"}})
		if err != nil {
			t.Fatal(err)
		}
		dora := gridS3(c, nodeA, doraID, doraSecret)
		content := map[string][]byte{}
		for _, key := range []string{"reports/q3.csv", "reports/secret/plan.txt"} {
			content[key] = randomBytes(t, 4096)
			if err := c.Put(ctx, nodeC, bucketName, key, content[key]); err != nil {
				t.Fatalf("Put %s through %s: %v", key, nodeC, err)
			}
		}

		at := func(i int) *int { return &i }
		readReports := builderCited{Source: "identity-policy", Effect: "Allow", Readable: true, Policy: policyName, StatementIndex: at(1), Sid: "ReadReports"}
		noSecrets := builderCited{Source: "identity-policy", Effect: "Deny", Readable: true, Policy: policyName, StatementIndex: at(2), Sid: "NoSecrets"}
		get := func(key string) func(context.Context) error {
			return func(ctx context.Context) error {
				got, err := gridGet(ctx, dora, bucketName, key)
				if err == nil && !bytes.Equal(got, content[key]) {
					return fmt.Errorf("GetObject %s/%s: the content differs", bucketName, key)
				}
				return err
			}
		}
		put := func(key string) func(context.Context) error {
			return func(ctx context.Context) error {
				_, err := dora.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucketName), Key: aws.String(key), Body: bytes.NewReader([]byte("new"))})
				return err
			}
		}
		type row struct {
			name, action, key string
			allowed           bool
			source            string
			cited             []builderCited
			do                func(context.Context) error
		}
		// check asks node2 what node1 will decide for dora's key, has node1
		// decide it, and holds the explanation to what node1 did.
		check := func(t *testing.T, tt row) {
			t.Helper()
			query := url.Values{"access_key_id": {doraID}, "action": {tt.action}, "bucket": {bucketName}, "key": {tt.key}}
			status, header, body, err := builderCall(ctx, c, nodeB, root, http.MethodGet, "/access/explain?"+query.Encode(), "")
			if err != nil {
				t.Fatal(err)
			}
			var answer builderExplanation
			if status != http.StatusOK || json.Unmarshal(body, &answer) != nil {
				t.Fatalf("the explanation through %s = %d %s", nodeB, status, body)
			}
			resource := "arn:aws:s3:::" + bucketName + "/" + tt.key
			explained = append(explained, builderExplained{
				what: tt.name, requestID: header.Get("X-Request-Id"), outcome: "success", resource: "arn:cactusfs:admin:::user/dora",
				details: map[string]string{
					"subject": "key/" + doraID, "access_key_id": doraID, "action": tt.action, "resource": resource,
					"answer": "decision", "allowed": strconv.FormatBool(tt.allowed), "source": tt.source,
				},
			})
			if answer.Subject.Kind != "access_key" || answer.Subject.User != "dora" || answer.Subject.AccessKeyID != doraID ||
				answer.Request.Action != tt.action || answer.Request.Resource != resource || !answer.Authenticates ||
				!answer.Assumed.SecureTransport || len(answer.NotConsidered) != 0 || len(answer.KeyPolicy) != 0 ||
				len(answer.UnknownAddress) != 0 {
				t.Errorf("the explanation through %s = %s", nodeB, body)
			}
			if answer.Allowed == nil || *answer.Allowed != tt.allowed || answer.Source != tt.source {
				t.Errorf("the explanation through %s = %s, want allowed %t by %s", nodeB, body, tt.allowed, tt.source)
			}
			if got, want := builderCitedLines(answer.Statements), builderCitedLines(tt.cited); !slices.Equal(got, want) {
				t.Errorf("the explanation through %s cites %q, want %q", nodeB, got, want)
			}

			// What node1 does with the request explained.
			happened, err := builderS3Outcome(tt.do(ctx))
			if err != nil {
				t.Fatalf("%s through %s: %v", tt.name, nodeA, err)
			}
			if answer.Allowed != nil && happened != *answer.Allowed {
				t.Errorf("%s through %s: carried out %t, the explanation through %s said allowed %t", tt.name, nodeA, happened, nodeB, *answer.Allowed)
			}
			if happened != tt.allowed {
				t.Errorf("%s through %s: carried out %t, want %t", tt.name, nodeA, happened, tt.allowed)
			}
		}

		// Before the change: nothing of the policy matches the report.
		first := row{
			name: "dora's GetObject of a report before the change", action: "s3:GetObject", key: "reports/q3.csv",
			allowed: false, source: "none", cited: nil, do: get("reports/q3.csv"),
		}
		t.Run(first.name, func(t *testing.T) { check(t, first) })

		// The change, through node3. Every read of identities and policies
		// is linearizable (a read index from the leader, then the entries
		// applied up to it), and the authorizer keeps no identity policy,
		// so node2 and node1 decide on the change as soon as it is
		// committed. The wait below holds the two to that, and says what
		// they answered if they do not.
		mustAdmin(t, nodeC, http.StatusOK, http.MethodPut, "/policies/"+policyName, `{"document":`+after+`}`)
		c.Eventually("the change of "+policyName+" to be applied on "+nodeB+" and "+nodeA, time.Minute, func() bool {
			for _, id := range []string{nodeB, nodeA} {
				status, _, body, err := builderCall(ctx, c, id, root, http.MethodGet, "/policies/"+policyName, "")
				var policy struct {
					Document json.RawMessage `json:"document"`
				}
				if err != nil || status != http.StatusOK || json.Unmarshal(body, &policy) != nil || !bytes.Contains(policy.Document, []byte(`"NoSecrets"`)) {
					t.Logf("GET /policies/%s through %s = %d %s (%v)", policyName, id, status, body, err)
					return false
				}
			}
			return true
		})

		tests := []row{
			{
				name: "dora's GetObject of a report", action: "s3:GetObject", key: "reports/q3.csv",
				allowed: true, source: "identity-policy", cited: []builderCited{readReports}, do: get("reports/q3.csv"),
			},
			{
				name: "dora's GetObject of a secret report", action: "s3:GetObject", key: "reports/secret/plan.txt",
				allowed: false, source: "identity-policy", cited: []builderCited{readReports, noSecrets}, do: get("reports/secret/plan.txt"),
			},
			{
				name: "dora's PutObject of a report", action: "s3:PutObject", key: "reports/q4.csv",
				allowed: false, source: "none", cited: nil, do: put("reports/q4.csv"),
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) { check(t, tt) })
		}

		// A key that is not there: 404 after authorization, and recorded.
		query := url.Values{"access_key_id": {"AKIDNOSUCHKEY0000000"}, "action": {"s3:GetObject"}, "bucket": {bucketName}, "key": {"reports/q3.csv"}}
		status, header, body, err := builderCall(ctx, c, nodeB, root, http.MethodGet, "/access/explain?"+query.Encode(), "")
		if err != nil {
			t.Fatal(err)
		}
		if status != http.StatusNotFound || problemCode(body) != "NoSuchEntity" {
			t.Errorf("the explanation of a key that is not there = %d %s, want 404 NoSuchEntity", status, body)
		}
		explained = append(explained, builderExplained{
			what: "a key that is not there", requestID: header.Get("X-Request-Id"), outcome: "failed", resource: "arn:cactusfs:admin:::user",
			details: map[string]string{
				"subject": "key/AKIDNOSUCHKEY0000000", "access_key_id": "AKIDNOSUCHKEY0000000", "action": "s3:GetObject",
				"resource": "arn:aws:s3:::" + bucketName + "/reports/q3.csv", "answer": "no_such_entity",
			},
		})
	}

	// Validation refuses what a put refuses, with the same faults.
	{
		mustAdmin(t, "node4", http.StatusCreated, http.MethodPost, "/buckets", `{"name":"checked"}`)
		mustAdmin(t, "node4", http.StatusCreated, http.MethodPost, "/policies",
			`{"name":"existing","document":{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:ListBucket","Resource":"arn:aws:s3:::checked"}]}}`)
		mustAdmin(t, "node4", http.StatusCreated, http.MethodPost, "/users", `{"name":"erin"}`)

		const (
			identityValid = `{"Version":"2012-10-17","Statement":[{"Sid":"read","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::checked/*"}]}`
			bucketValid   = `{"Version":"2012-10-17","Statement":[{"Sid":"read","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::checked/*"}]}`
			subValid      = `{"Version":"2012-10-17","Statement":[{"Sid":"read","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/erin"},` +
				`"Action":"s3:GetObject","Resource":"arn:aws:s3:::checked/reports/*"}]}`
		)
		type kind struct {
			validate, field string // the validation and the field its document is in
			other           string // the other fields of its body, each followed by a comma
			method, path    string // the put
			putField        string
			putOther        string
			code            string // the code of the put's 400
			created         int    // the status of the put of a valid document
			read            string // what the put would change, read after a refused put
		}
		var (
			createPolicy = kind{
				validate: "/policy-validation", field: "document", other: `"name":"checked",`,
				method: http.MethodPost, path: "/policies", putField: "document", putOther: `"name":"checked",`,
				code: "InvalidArgument", created: http.StatusCreated, read: "/policies/checked",
			}
			updatePolicy = kind{
				validate: "/policy-validation", field: "document", other: `"name":"existing",`,
				method: http.MethodPut, path: "/policies/existing", putField: "document",
				code: "InvalidArgument", created: http.StatusOK, read: "/policies/existing",
			}
			createAPIKey = kind{
				validate: "/policy-validation", field: "document", other: `"key_owner":"erin",`,
				method: http.MethodPost, path: "/users/erin/keys", putField: "inline_policy",
				code: "InvalidArgument", created: http.StatusCreated, read: "/users/erin/keys",
			}
			putBucketPolicy = kind{
				validate: "/buckets/checked/policy/validation", field: "document",
				method: http.MethodPut, path: "/buckets/checked/policy", putField: "document",
				code: "InvalidPolicy", created: http.StatusOK, read: "/buckets/checked/policy",
			}
			putSubBucket = kind{
				validate: "/buckets/checked/subbuckets/reports/policy/validation", field: "policy",
				method: http.MethodPut, path: "/buckets/checked/subbuckets/reports", putField: "policy",
				code: "InvalidPolicy", created: http.StatusOK, read: "/buckets/checked/subbuckets/reports",
			}
		)
		tests := []struct {
			name     string
			kind     kind
			document string
			// pointers are where the faults are, in order; none for a
			// document the put takes.
			pointers []string
		}{
			{
				name: "a named policy with every fault, created", kind: createPolicy,
				document: `{"Version":"1","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"},{"Effect":"Maybe","Action":["s3:*","x"],"Resource":["*","y"],` +
					`"Condition":{"Bool":{"aws:SecureTransport":"yes"},"IpAddress":{"aws:SourceIp":["1.2.3.4","z"]}}}]}`,
				pointers: []string{
					"/Version", "/Statement/1/Effect", "/Statement/1/Action/1", "/Statement/1/Resource/1",
					"/Statement/1/Condition/IpAddress/aws:SourceIp/1", "/Statement/1/Condition/Bool/aws:SecureTransport",
				},
			},
			{name: "a named policy, created", kind: createPolicy, document: identityValid},
			{
				name: "a named policy with an Effect of neither kind, updated", kind: updatePolicy,
				document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::checked/*"},` +
					`{"Effect":"Maybe","Action":"s3:GetObject","Resource":"*"}]}`,
				pointers: []string{"/Statement/1/Effect"},
			},
			{name: "a named policy, updated", kind: updatePolicy, document: identityValid},
			{
				name: "the inline policy of a key without an Action", kind: createAPIKey,
				document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Resource":"*"}]}`,
				pointers: []string{"/Statement/0"},
			},
			{name: "the inline policy of a key", kind: createAPIKey, document: identityValid},
			{
				name: "a bucket's policy about another bucket", kind: putBucketPolicy,
				document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other-bucket/*"}]}`,
				pointers: []string{"/Statement/0/Resource"},
			},
			{
				name: "a bucket's policy with NotPrincipal", kind: putBucketPolicy,
				document: `{"Version":"2012-10-17","Statement":[{"NotPrincipal":"*","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::checked/*"}]}`,
				pointers: []string{"/Statement/0/NotPrincipal"},
			},
			{name: "a bucket's policy", kind: putBucketPolicy, document: bucketValid},
			{
				name: "a sub-bucket's policy outside the sub-bucket", kind: putSubBucket,
				document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/erin"},` +
					`"Action":"s3:GetObject","Resource":"arn:aws:s3:::checked/other/*"}]}`,
				pointers: []string{"/Statement/0/Resource"},
			},
			{
				name: "a sub-bucket's policy that allows anyone", kind: putSubBucket,
				document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::checked/reports/*"}]}`,
				pointers: []string{"/Statement/0/Principal"},
			},
			{name: "a sub-bucket's policy", kind: putSubBucket, document: subValid},
		}
		for i, tt := range tests {
			// Validated through one node, put through the next, read
			// through the one after.
			validateOn := clustertest.Nodes[i%len(clustertest.Nodes)]
			putOn := clustertest.Nodes[(i+1)%len(clustertest.Nodes)]
			readOn := clustertest.Nodes[(i+2)%len(clustertest.Nodes)]
			t.Run(tt.name, func(t *testing.T) {
				status, _, body, err := builderCall(ctx, c, validateOn, root, http.MethodPost, tt.kind.validate,
					`{`+tt.kind.other+`"`+tt.kind.field+`":`+tt.document+`}`)
				if err != nil {
					t.Fatal(err)
				}
				var validation struct {
					Valid      bool              `json:"valid"`
					Faults     json.RawMessage   `json:"faults"`
					Truncated  bool              `json:"truncated"`
					Statements []json.RawMessage `json:"statements"`
				}
				if status != http.StatusOK || json.Unmarshal(body, &validation) != nil {
					t.Fatalf("the validation through %s = %d %s", validateOn, status, body)
				}
				var faults []struct {
					Pointer string `json:"pointer"`
				}
				if err := json.Unmarshal(validation.Faults, &faults); err != nil {
					t.Fatalf("the faults of the validation through %s: %v: %s", validateOn, err, body)
				}
				pointers := make([]string, 0, len(faults))
				for _, f := range faults {
					pointers = append(pointers, f.Pointer)
				}
				if !slices.Equal(pointers, tt.pointers) || validation.Valid != (len(tt.pointers) == 0) || validation.Truncated ||
					(len(validation.Statements) == 0) == validation.Valid {
					t.Fatalf("the validation through %s = %s, want faults at %q", validateOn, body, tt.pointers)
				}

				readStatus, _, before, err := builderCall(ctx, c, readOn, root, http.MethodGet, tt.kind.read, "")
				if err != nil {
					t.Fatal(err)
				}
				status, header, body, err := builderCall(ctx, c, putOn, root, tt.kind.method, tt.kind.path,
					`{`+tt.kind.putOther+`"`+tt.kind.putField+`":`+tt.document+`}`)
				if err != nil {
					t.Fatal(err)
				}
				if validation.Valid {
					// Taken by the validation, taken by the put. The answer
					// of a key's creation holds its secret: it is not told.
					if status != tt.kind.created {
						t.Errorf("the put through %s of a document its validation took = %d", putOn, status)
					}
					return
				}
				var problem struct {
					Code   string          `json:"code"`
					Faults json.RawMessage `json:"faults"`
				}
				// Anything but a 400 is told by its status alone: the answer of
				// a key's creation that should have been refused holds the
				// key's secret.
				if status != http.StatusBadRequest {
					t.Fatalf("the put through %s of a document its validation refused = %d, want 400 %s", putOn, status, tt.kind.code)
				}
				if !strings.HasPrefix(header.Get("Content-Type"), "application/problem+json") ||
					json.Unmarshal(body, &problem) != nil || problem.Code != tt.kind.code {
					t.Fatalf("the put through %s of a document its validation refused = %d %s, want 400 %s", putOn, status, body, tt.kind.code)
				}
				if got, want := builderCompact(t, string(problem.Faults)), builderCompact(t, string(validation.Faults)); got != want {
					t.Errorf("the put through %s is refused with the faults %s; the validation through %s gave %s", putOn, got, validateOn, want)
				}
				// Refused, it changed nothing.
				readAgain, _, after, err := builderCall(ctx, c, readOn, root, http.MethodGet, tt.kind.read, "")
				if err != nil {
					t.Fatal(err)
				}
				if readAgain != readStatus || (readStatus == http.StatusOK && !bytes.Equal(after, before)) ||
					(readStatus != http.StatusOK && problemCode(after) != problemCode(before)) {
					t.Errorf("GET %s through %s after a refused put = %d %s, before it %d %s", tt.kind.read, readOn, readAgain, after, readStatus, before)
				}
			})
		}
	}

	// A bucket's policy is one document through S3 and the management API.
	{
		mustAdmin(t, "node1", http.StatusCreated, http.MethodPost, "/buckets", `{"name":"shared"}`)
		tests := []struct {
			name                 string
			viaS3                bool
			setOn, s3On, adminOn string
			document             string
		}{
			{
				name: "set through S3, keys in another order", viaS3: true, setOn: "node2", s3On: "node3", adminOn: "node4",
				document: `{"Statement":[{"Resource":"arn:aws:s3:::shared/*","Sid":"s3","Principal":{"AWS":"arn:aws:iam:::user/dora"},` +
					`"Action":["s3:GetObject"],"Effect":"Allow"}],"Version":"2012-10-17"}`,
			},
			{
				name: "set through S3, with spaces", viaS3: true, setOn: "node4", s3On: "node1", adminOn: "node2",
				document: "{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": [ { \"Sid\": \"spaced\", \"Effect\": \"Deny\", \"Principal\": \"*\", " +
					"\"Action\": \"s3:GetObject\", \"Resource\": \"arn:aws:s3:::shared/x\" } ]\n}",
			},
			{
				name: "set through the management API", setOn: "node1", s3On: "node3", adminOn: "node2",
				document: `{"Version":"2012-10-17","Statement":[{"Sid":"admin","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::shared/*"}]}`,
			},
			{
				name: "set through the management API, with spaces", setOn: "node3", s3On: "node2", adminOn: "node4",
				document: "{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": [ { \"Effect\": \"Deny\", \"Principal\": \"*\", \"Action\": \"s3:GetObject\", " +
					"\"Resource\": \"arn:aws:s3:::shared/x\" } ]\n}",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if tt.viaS3 {
					if _, err := c.S3(tt.setOn).PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String("shared"), Policy: aws.String(tt.document)}); err != nil {
						t.Fatalf("PutBucketPolicy through %s: %v", tt.setOn, err)
					}
				} else {
					mustAdmin(t, tt.setOn, http.StatusOK, http.MethodPut, "/buckets/shared/policy", `{"document":`+tt.document+`}`)
				}
				// S3 reads the bytes as they were set.
				out, err := c.S3(tt.s3On).GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String("shared")})
				if err != nil {
					t.Fatalf("GetBucketPolicy through %s: %v", tt.s3On, err)
				}
				if got := aws.ToString(out.Policy); got != tt.document {
					t.Errorf("GetBucketPolicy through %s = %q, set through %s as %q", tt.s3On, got, tt.setOn, tt.document)
				}
				// The management API answers JSON: the same document,
				// keys in the order they were set, without the spaces.
				status, _, body, err := builderCall(ctx, c, tt.adminOn, root, http.MethodGet, "/buckets/shared/policy", "")
				if err != nil {
					t.Fatal(err)
				}
				var policy struct {
					Document json.RawMessage `json:"document"`
				}
				if status != http.StatusOK || json.Unmarshal(body, &policy) != nil {
					t.Fatalf("GET the policy through the management API of %s = %d %s", tt.adminOn, status, body)
				}
				if got, want := string(policy.Document), builderCompact(t, tt.document); got != want {
					t.Errorf("the management API of %s reads %s, set through %s as %s", tt.adminOn, got, tt.setOn, want)
				}
			})
		}
	}

	// Every explanation is in the audit trail.
	{
		if len(explained) == 0 {
			t.Fatal("no explanation was asked for")
		}
		// Through a node that answered none of them, at once: each record
		// was committed before its answer was given.
		trail, err := builderAudit(ctx, c, "node4", root)
		if err != nil {
			t.Fatal(err)
		}
		byRequest := map[string][]builderAuditRecord{}
		for _, r := range trail {
			if r.Action == "admin:ExplainAccess" {
				byRequest[r.RequestID] = append(byRequest[r.RequestID], r)
			}
		}
		for _, e := range explained {
			t.Run(e.what, func(t *testing.T) {
				if e.requestID == "" {
					t.Fatal("the answer had no X-Request-Id")
				}
				records := byRequest[e.requestID]
				if len(records) != 1 {
					t.Fatalf("%d records of request %s in the trail through node4, want 1", len(records), e.requestID)
				}
				r := records[0]
				if r.Actor != "root" || r.AccessKeyID != c.AccessKey || r.Resource != e.resource || r.Outcome != e.outcome {
					t.Errorf("the record = %+v, want root's %s on %s", r, e.outcome, e.resource)
				}
				// The details are exactly these: ExplainService writes a fixed
				// set, and anything more could be a part of a policy.
				if !maps.Equal(r.Details, e.details) {
					t.Errorf("the record's details = %q, want %q", r.Details, e.details)
				}
				for name, value := range r.Details {
					if strings.Contains(value, "Statement") || strings.Contains(value, "Effect") {
						t.Errorf("the record's %s holds a policy document: %q", name, value)
					}
				}
			})
		}
	}
}
