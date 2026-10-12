package app_test

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// benchBuckets is a deployment of many buckets, each with a policy, as a
// search sees it: the list of the buckets, and their policies page by page.
// It lends its documents without copying them, so that what a benchmark
// reports is the work of the search and of the authorizer.
type benchBuckets struct {
	ports.BucketStore
	list      []bucket.Bucket
	names     []string
	documents [][]byte
}

// The page of benchBuckets, which is that of the metadata store.
const (
	benchPageBuckets = 256
	benchPageBytes   = 1 << 20
)

func (d *benchBuckets) ListBuckets(context.Context) ([]bucket.Bucket, error) {
	return d.list, nil
}

func (d *benchBuckets) BucketPolicies(ctx context.Context, from string, visit func(string, []byte) error) (string, error) {
	read, size := 0, 0
	for i := sort.SearchStrings(d.names, from); i < len(d.names); i++ {
		if read >= benchPageBuckets || size >= benchPageBytes {
			return d.names[i], nil
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		read++
		size += len(d.documents[i])
		if err := visit(d.names[i], d.documents[i]); err != nil {
			return "", err
		}
	}
	return "", nil
}

// benchPolicy is a bucket policy of about the given size: statements, no
// more than a policy may have, that open parts of the bucket to users - none
// of them to who searches.
func benchPolicy(b *testing.B, bucketName string, size int) []byte {
	b.Helper()
	statements := min(60, max(1, size/270))
	each := size / statements
	var doc strings.Builder
	doc.WriteString(`{"Version":"2012-10-17","Statement":[`)
	for i := range statements {
		if i > 0 {
			doc.WriteByte(',')
		}
		began := doc.Len()
		fmt.Fprintf(&doc, `{"Sid":"team%04d","Effect":"Allow","Principal":{"AWS":["arn:aws:iam:::user/team%04d-a","arn:aws:iam:::user/team%04d-b"]},`+
			`"Action":["s3:GetObject","s3:PutObject","s3:ListBucket"],"Resource":["arn:aws:s3:::%s"`, i, i, i, bucketName)
		for j := 0; doc.Len()-began < each-60; j++ {
			fmt.Fprintf(&doc, `,"arn:aws:s3:::%s/teams/%04d/project-%03d/*"`, bucketName, i, j)
		}
		doc.WriteString(`]}`)
	}
	doc.WriteString(`]}`)
	document := []byte(doc.String())
	if _, err := iam.ParseBucketPolicy(document, bucketName); err != nil {
		b.Fatalf("the policy of the benchmark is none: %v", err)
	}
	return document
}

// BenchmarkSearchDeciding measures one page of a search by somebody who is
// not root, in a deployment where every bucket has a policy: the listing of
// the buckets, the walk of their policies and a decision about every bucket.
// Cold, the authorizer remembers nothing, as after a restart, and parses
// every policy; warm, it has made the same search before, and hashes them.
// The page must stay far inside app.MaxSearchTime.
//
// "heap/doc" is how many bytes of heap the authorizer's memory of the
// policies takes for a byte of their documents.
func BenchmarkSearchDeciding(b *testing.B) {
	for _, size := range []struct {
		buckets, policy int
	}{{5_000, 17 << 10}, {20_000, 1200}} {
		data := &benchBuckets{BucketStore: porttest.NewMemMetadata()}
		for i := range size.buckets {
			name := fmt.Sprintf("bucket-%05d", i)
			document := benchPolicy(b, name, size.policy)
			data.list = append(data.list, bucket.Bucket{Name: name, CreatedAt: testNow})
			data.names = append(data.names, name)
			data.documents = append(data.documents, document)
		}
		logger := slog.New(slog.DiscardHandler)
		// The clock stands still: no page is cut short by its time.
		clock := porttest.NewClock(testNow)
		who := iam.Principal{AccessKeyID: "V", User: "viewer", Policies: []iam.Document{policyOn("s3:ListBucket", "arn:aws:s3:::bucket-00000")}}
		service := func(b *testing.B) (*app.SearchService, *app.Authorizer) {
			b.Helper()
			authz := app.NewAuthorizer(logger, clock, nil).WithBucketPolicies(data)
			s, err := app.NewSearchService(app.SearchServiceConfig{
				Buckets: data, Index: porttest.NewMemMetadata(), Policies: data, Authz: authz, Clock: clock, Logger: logger,
			})
			if err != nil {
				b.Fatal(err)
			}
			return s, authz
		}
		page := func(b *testing.B, s *app.SearchService) {
			b.Helper()
			out, err := s.Search(b.Context(), who, app.SearchInput{Query: "report"})
			if err != nil || !out.Complete {
				b.Fatalf("Search = %+v, %v", out, err)
			}
		}
		name := fmt.Sprintf("%dx%dB", size.buckets, len(data.documents[0]))
		b.Run(name+"/cold", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				s, _ := service(b)
				page(b, s)
			}
		})
		b.Run(name+"/warm", func(b *testing.B) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			s, authz := service(b)
			page(b, s)
			runtime.GC()
			runtime.ReadMemStats(&after)
			entries, remembered := authz.ParsedPolicies()
			if entries == 0 || remembered == 0 {
				b.Fatal("nothing was remembered")
			}
			b.ReportAllocs()
			for b.Loop() {
				page(b, s)
			}
			b.ReportMetric(float64(entries), "remembered")
			b.ReportMetric((float64(after.HeapAlloc)-float64(before.HeapAlloc))/float64(remembered), "heap/doc")
			runtime.KeepAlive(authz)
		})
	}
}
