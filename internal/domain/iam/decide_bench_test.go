package iam_test

import (
	"testing"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// decideRequests are the requests BenchmarkDecide decides: a user allowed by
// a built-in role, a user refused by default, an anonymous caller let in by
// a public bucket, a user refused by a bucket's Deny, and a user allowed by
// a sub-bucket's policy.
func decideRequests(b testing.TB) map[string]iam.Request {
	b.Helper()
	readWrite := iam.BuiltInPolicies()[iam.RoleReadWrite]
	object := iam.Resource{Bucket: "site", Key: "reports/a.csv"}
	public := mustBucketPolicyTB(b, publicRead, "site")
	denying := mustBucketPolicyTB(b, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:Delete*","Resource":"arn:aws:s3:::site/*"}]}`, "site")
	sub := mustSubBucketPolicy(b, readReports, "site", "reports")
	return map[string]iam.Request{
		"identity-allowed": {Principal: user("bob", readWrite), Action: iam.ActionGetObject, Resource: object, Now: policyNow},
		"identity-refused": {Principal: user("bob", readWrite), Action: iam.ActionPutBucketPolicy, Resource: iam.Resource{Bucket: "site"}, Now: policyNow},
		"anonymous-public": {Action: iam.ActionGetObject, Resource: object, Now: policyNow, BucketPolicy: public},
		"bucket-deny":      {Principal: user("bob", readWrite), Action: iam.ActionDeleteObject, Resource: object, Now: policyNow, BucketPolicy: denying},
		"sub-bucket":       {Principal: user("alice"), Action: iam.ActionGetObject, Resource: object, Now: policyNow, SubBucketPolicy: sub},
	}
}

// mustBucketPolicyTB parses a bucket policy for a test or a benchmark.
func mustBucketPolicyTB(tb testing.TB, document, bucket string) *iam.BucketPolicy {
	tb.Helper()
	policy, err := iam.ParseBucketPolicy([]byte(document), bucket)
	if err != nil {
		tb.Fatalf("ParseBucketPolicy: %v", err)
	}
	return &policy
}

// BenchmarkDecide measures Decide on requests of each source of a decision.
func BenchmarkDecide(b *testing.B) {
	for name, req := range decideRequests(b) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = iam.Decide(req)
			}
		})
	}
}
