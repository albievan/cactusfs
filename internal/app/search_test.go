package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/search"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// searchData is what the buckets of a search test hold. One bucket is
// empty, and one answers to "report" by its name alone.
func searchData() map[string][]string {
	return map[string][]string{
		"alpha":         {"docs/Report-2025.pdf", "docs/report-2026.pdf", "img/logo.png", "notes.txt"},
		"beta":          {"Report.txt", "data/a.csv", "data/b.csv"},
		"delta":         {},
		"gamma-reports": {"readme.md", "x/1.bin", "x/2.bin"},
	}
}

// searchFixture is a SearchService over the stores of a fixture, behind the
// authorizer a node has: one that reads bucket policies and audits.
type searchFixture struct {
	base    *fixture
	trail   *porttest.MemIAM
	auditor *app.Auditor
	authz   *app.Authorizer
	// policies is where bucket policies are read, one by one and all at
	// once; asked counts what the search asks the authorizer.
	policies *countingPolicies
	asked    *countingAuthorizer
	search   *app.SearchService
}

// countingPolicies counts the readings of bucket policies - of one bucket,
// as the authorizer reads on its own, and of a page of the walk of all - and
// can move a clock forward with every reading, as reading takes time.
type countingPolicies struct {
	meta  *porttest.MemMetadata
	one   atomic.Int64
	all   atomic.Int64
	clock *porttest.Clock
	takes time.Duration
}

func (c *countingPolicies) GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error) {
	c.one.Add(1)
	c.clock.Advance(c.takes)
	return c.meta.GetBucketConfig(ctx, name, kind)
}

func (c *countingPolicies) BucketPolicies(ctx context.Context, from string, visit func(string, []byte) error) (string, error) {
	c.all.Add(1)
	c.clock.Advance(c.takes)
	return c.meta.BucketPolicies(ctx, from, visit)
}

// countingAuthorizer counts the decisions a search asks for, and can move a
// clock forward with every decision that filters.
type countingAuthorizer struct {
	ports.Authorizer
	filtered atomic.Int64
	clock    *porttest.Clock
	takes    atomic.Int64
}

func (c *countingAuthorizer) Allowed(ctx context.Context, req iam.Request) (bool, error) {
	c.filtered.Add(1)
	c.clock.Advance(time.Duration(c.takes.Load()))
	return c.Authorizer.Allowed(ctx, req)
}

// newSearchFixture stores the data and wires a search over it. index, when
// not nil, stands between the search and the index.
func newSearchFixture(ctx context.Context, t *testing.T, data map[string][]string, index func(ports.ObjectIndex) ports.ObjectIndex) *searchFixture {
	t.Helper()
	f := &searchFixture{base: newFixture(t), trail: porttest.NewMemIAM()}
	for name, keys := range data {
		if err := f.base.meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
			t.Fatalf("CreateBucket(%s): %v", name, err)
		}
		for _, key := range keys {
			f.put(ctx, t, name, key)
		}
	}
	logger := slog.New(slog.NewJSONHandler(f.base.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.auditor = app.NewAuditor(f.trail, f.base.clock, logger)
	f.policies = &countingPolicies{meta: f.base.meta, clock: f.base.clock}
	f.authz = app.NewAuthorizer(logger, f.base.clock, f.auditor).WithBucketPolicies(f.policies)
	f.asked = &countingAuthorizer{Authorizer: f.authz, clock: f.base.clock}
	var scanned ports.ObjectIndex = f.base.meta
	if index != nil {
		scanned = index(scanned)
	}
	var err error
	f.search, err = app.NewSearchService(app.SearchServiceConfig{
		Buckets: f.base.meta, Index: scanned, Policies: f.policies, Authz: f.asked, Clock: f.base.clock, Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewSearchService: %v", err)
	}
	return f
}

func (f *searchFixture) put(ctx context.Context, t *testing.T, bucketName, key string) {
	t.Helper()
	data := []byte(key)
	if _, err := f.base.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: bucketName, Key: key, Body: bytes.NewReader(data), Size: int64(len(data))}); err != nil {
		t.Fatalf("Put(%s/%s): %v", bucketName, key, err)
	}
}

// denials returns the refusals in the audit trail.
func (f *searchFixture) denials(ctx context.Context, t *testing.T) []audit.Record {
	t.Helper()
	f.auditor.Flush(ctx)
	records, err := f.trail.ListAudit(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var denied []audit.Record
	for _, r := range records {
		if r.Outcome == audit.Denied {
			denied = append(denied, r)
		}
	}
	return denied
}

// found names results: "b:name" for a bucket, "o:bucket/key" for an object.
func found(results []app.SearchResult) []string {
	names := []string{}
	for _, r := range results {
		if r.Kind == app.SearchResultBucket {
			names = append(names, "b:"+r.Bucket)
		} else {
			names = append(names, "o:"+r.Bucket+"/"+r.Key)
		}
	}
	return names
}

// oracle is what a search of the data must find in the end, computed the
// plain way: every bucket in name order, its name when names are shown, its
// keys in order when it may be listed.
func oracle(t *testing.T, data map[string][]string, query string, names bool, listable func(string) bool) []string {
	t.Helper()
	q, err := search.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	want := []string{}
	for _, name := range slices.Sorted(maps.Keys(data)) {
		if names && q.Match(name) {
			want = append(want, "b:"+name)
		}
		if !listable(name) {
			continue
		}
		keys := slices.Clone(data[name])
		slices.Sort(keys)
		for _, key := range keys {
			if q.Match(key) {
				want = append(want, "o:"+name+"/"+key)
			}
		}
	}
	return want
}

func everyBucket(string) bool { return true }

// drain asks for page after page until the search is complete, and returns
// everything found and the pages it took.
func drain(ctx context.Context, t *testing.T, s *app.SearchService, principal iam.Principal, in app.SearchInput) ([]string, []app.SearchOutput) {
	t.Helper()
	all, pages := []string{}, []app.SearchOutput{}
	for {
		out, err := s.Search(ctx, principal, in)
		if err != nil {
			t.Fatalf("page %d of %+v: %v", len(pages)+1, in, err)
		}
		pages = append(pages, out)
		all = append(all, found(out.Results)...)
		limit := in.Limit
		if limit == 0 {
			limit = app.DefaultSearchLimit
		}
		switch {
		case len(out.Results) > limit:
			t.Fatalf("page %d has %d results, the limit is %d", len(pages), len(out.Results), limit)
		case out.Complete && out.NextContinuation != "":
			t.Fatalf("page %d is complete and has a continuation", len(pages))
		case out.Complete:
			return all, pages
		case out.NextContinuation == "" || out.NextContinuation == in.Continuation:
			t.Fatalf("page %d is not complete and does not say where to go on: %q", len(pages), out.NextContinuation)
		case len(pages) > 1000:
			t.Fatalf("a search of %+v does not end", in)
		}
		in.Continuation = out.NextContinuation
	}
}

func policyOn(action, resource string) iam.Document {
	return iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{
		Effect: iam.Allow, Actions: []string{action}, Resources: []string{resource},
	}}}
}

// The callers of the search tests. viewer may list alpha by a policy of
// their own and beta by the policy of that bucket; lister may list the
// buckets and none of them; stranger may do nothing.
func viewer() iam.Principal {
	return iam.Principal{AccessKeyID: "V", User: "viewer", Policies: []iam.Document{policyOn("s3:ListBucket", "arn:aws:s3:::alpha")}}
}

func lister() iam.Principal {
	return iam.Principal{AccessKeyID: "L", User: "lister", Policies: []iam.Document{policyOn("s3:ListAllMyBuckets", "*")}}
}

func stranger() iam.Principal { return iam.Principal{AccessKeyID: "S", User: "stranger"} }

// viewerMayList is what viewer may list, once beta's policy is set.
func viewerMayList(name string) bool { return name == "alpha" || name == "beta" }

// openBetaToViewer gives beta a policy that lets viewer list it.
func (f *searchFixture) openBetaToViewer(ctx context.Context, t *testing.T) {
	t.Helper()
	policy := `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/viewer"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::beta"}]}`
	if err := f.base.meta.SetBucketConfig(ctx, "beta", bucket.ConfigPolicy, []byte(policy)); err != nil {
		t.Fatalf("SetBucketConfig: %v", err)
	}
}

// TestSearchQueries: each kind of query, over several buckets, for somebody
// who may see everything.
func TestSearchQueries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSearchFixture(ctx, t, searchData(), nil)

	tests := map[string]struct {
		query string
		kind  search.Kind
		want  []string
	}{
		"text, whatever its case": {"REPORT", search.Substring, []string{
			"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf", "o:beta/Report.txt", "b:gamma-reports",
		}},
		"text with spaces around it":       {"  logo ", search.Substring, []string{"o:alpha/img/logo.png"}},
		"text that is found nowhere":       {"nothing-like-this", search.Substring, []string{}},
		"a wildcard is for the whole name": {"*.pdf", search.Wildcard, []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf"}},
		"a wildcard, whatever its case":    {"DATA/?.CSV", search.Wildcard, []string{"o:beta/data/a.csv", "o:beta/data/b.csv"}},
		"a wildcard for a bucket":          {"?eta", search.Wildcard, []string{"b:beta"}},
		"a wildcard that is not anchored":  {"report", search.Substring, []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf", "o:beta/Report.txt", "b:gamma-reports"}},
		"a regular expression, as written": {"rx:report", search.Regex, []string{"o:alpha/docs/report-2026.pdf", "b:gamma-reports"}},
		"an anchored regular expression":   {`rx:^x/\d\.bin$`, search.Regex, []string{"o:gamma-reports/x/1.bin", "o:gamma-reports/x/2.bin"}},
		"a bucket and its own objects":     {"rx:^(gamma-reports|readme\\.md)$", search.Regex, []string{"b:gamma-reports", "o:gamma-reports/readme.md"}},
	}
	for name, tt := range tests {
		// Not in parallel: one search at a time for a principal.
		t.Run(name, func(t *testing.T) {
			out, err := f.search.Search(ctx, porttest.Root(), app.SearchInput{Query: tt.query})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if got := found(out.Results); !slices.Equal(got, tt.want) {
				t.Errorf("found %v, want %v", got, tt.want)
			}
			if want := oracle(t, searchData(), tt.query, true, everyBucket); !slices.Equal(found(out.Results), want) {
				t.Errorf("found %v, the oracle says %v", found(out.Results), want)
			}
			if out.MatchKind != tt.kind || !out.Complete || out.NextContinuation != "" || out.Examined != 10 {
				t.Errorf("kind %s, complete %v, continuation %q, examined %d; want %s, complete, none, 10",
					out.MatchKind, out.Complete, out.NextContinuation, out.Examined, tt.kind)
			}
		})
	}

	// An object is found with what a listing says of it; a bucket has a
	// name and nothing else.
	out, err := f.search.Search(ctx, porttest.Root(), app.SearchInput{Query: "rx:^(notes\\.txt|delta)$"})
	if err != nil || len(out.Results) != 2 {
		t.Fatalf("Search = %+v, %v", out, err)
	}
	note, delta := out.Results[0], out.Results[1]
	if note.Kind != app.SearchResultObject || note.Bucket != "alpha" || note.Key != "notes.txt" || note.Size != int64(len("notes.txt")) ||
		note.ModifiedAt.IsZero() {
		t.Errorf("an object was found as %+v", note)
	}
	if delta != (app.SearchResult{Kind: app.SearchResultBucket, Bucket: "delta"}) {
		t.Errorf("a bucket was found as %+v", delta)
	}
	// A search is a read: it is in no audit trail, and what was searched
	// for is in no log.
	if denied := f.denials(ctx, t); len(denied) != 0 {
		t.Errorf("searches left %d audit records", len(denied))
	}
	if logs := f.base.logs.String(); strings.Contains(logs, "notes") || strings.Contains(logs, "REPORT") || !strings.Contains(logs, `"msg":"search"`) {
		t.Errorf("the log of the searches: %s", logs)
	}
}

// TestSearchRefusesWhatIsNoSearch: a query, a limit or a continuation that
// is none is an invalid argument, and says why.
func TestSearchRefusesWhatIsNoSearch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSearchFixture(ctx, t, searchData(), nil)
	token := func(query, bucketName, key string) string {
		made, err := app.EncodeSearchToken(query, bucketName, key, true)
		if err != nil {
			t.Fatal(err)
		}
		return made
	}
	raw := func(document string) string { return base64.RawURLEncoding.EncodeToString([]byte(document)) }
	good := token("report", "alpha", "docs/Report-2025.pdf")
	var fields map[string]any
	if decoded, err := base64.RawURLEncoding.DecodeString(good); err != nil || json.Unmarshal(decoded, &fields) != nil {
		t.Fatalf("a token is not base64url of JSON: %q", good)
	}
	hash, _ := fields["q"].(string)

	tests := map[string]struct {
		in   app.SearchInput
		says string
	}{
		"nothing":                                  {app.SearchInput{Query: "  "}, "nothing to search for"},
		"a regular expression that is none":        {app.SearchInput{Query: "rx:(unclosed"}, "regular expression: "},
		"rx: and nothing":                          {app.SearchInput{Query: "rx:"}, "regular expression"},
		"a query too long":                         {app.SearchInput{Query: strings.Repeat("a", search.MaxLength+1)}, "at most"},
		"a negative limit":                         {app.SearchInput{Query: "a", Limit: -1}, "limit"},
		"a limit too large":                        {app.SearchInput{Query: "a", Limit: app.MaxSearchLimit + 1}, "limit"},
		"a token that is none":                     {app.SearchInput{Query: "report", Continuation: "!!not base64!!"}, "continuation"},
		"a token that is not JSON":                 {app.SearchInput{Query: "report", Continuation: raw("report")}, "continuation"},
		"a token of another version":               {app.SearchInput{Query: "report", Continuation: raw(`{"v":2,"q":"` + hash + `","b":"alpha","k":"","n":true}`)}, "version"},
		"a token with more in it":                  {app.SearchInput{Query: "report", Continuation: raw(`{"v":1,"q":"` + hash + `","b":"alpha","k":"","n":true,"admin":true}`)}, "continuation"},
		"a token and more after it":                {app.SearchInput{Query: "report", Continuation: raw(`{"v":1,"q":"` + hash + `","b":"alpha","k":"","n":true}{}`)}, "continuation"},
		"a token without a bucket":                 {app.SearchInput{Query: "report", Continuation: raw(`{"v":1,"q":"` + hash + `","b":"","k":"","n":true}`)}, "continuation"},
		"a token whose bucket is too short a name": {app.SearchInput{Query: "report", Continuation: raw(`{"v":1,"q":"` + hash + `","b":"a","k":"","n":true}`)}, "continuation"},
		"a token whose bucket is no name at all":   {app.SearchInput{Query: "report", Continuation: raw(`{"v":1,"q":"` + hash + `","b":"alpha\u0000","k":"","n":true}`)}, "continuation"},
		"a token whose bucket is written large":    {app.SearchInput{Query: "report", Continuation: raw(`{"v":1,"q":"` + hash + `","b":"Alpha","k":"","n":true}`)}, "continuation"},
		"a token of another search":                {app.SearchInput{Query: "logo", Continuation: good}, "another search"},
		"a token of another bucket":                {app.SearchInput{Query: "report", Bucket: "beta", Continuation: good}, "another bucket"},
		"a token too long":                         {app.SearchInput{Query: "report", Continuation: strings.Repeat("A", app.MaxSearchContinuation+1)}, "too long"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := f.search.Search(ctx, porttest.Root(), tt.in)
			if !errors.Is(err, domain.ErrInvalidArgument) || !strings.Contains(err.Error(), tt.says) || len(out.Results) != 0 {
				t.Errorf("Search = %+v, %v; want %v that says %q", out, err, domain.ErrInvalidArgument, tt.says)
			}
		})
	}
	// The same token, with the search it belongs to, is good - also with
	// the query typed with spaces around it.
	out, err := f.search.Search(ctx, porttest.Root(), app.SearchInput{Query: " report ", Continuation: good})
	if err != nil || !slices.Equal(found(out.Results), []string{"o:alpha/docs/report-2026.pdf", "o:beta/Report.txt", "b:gamma-reports"}) {
		t.Errorf("Search from a token = %v, %v", found(out.Results), err)
	}
	if _, err := app.NewSearchService(app.SearchServiceConfig{}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("NewSearchService of nothing = %v", err)
	}
}

// TestSearchShowsWhatTheCallerMayList: a bucket is searched for who may list
// it - by a policy of their own or by the bucket's - and its name is shown
// to who may list the buckets. The buckets left out leave no record; who may
// see nothing at all is refused, and that is audited once.
func TestSearchShowsWhatTheCallerMayList(t *testing.T) {
	t.Parallel()
	listOnly := policyOn("s3:ListBucket", "*")
	boundToGamma := policyOn("s3:ListBucket", "arn:aws:s3:::gamma-reports")
	everything := iam.Principal{AccessKeyID: "E", User: "everything", Policies: []iam.Document{policyOn("s3:*", "*")}}
	setPolicy := func(ctx context.Context, t *testing.T, f *searchFixture, bucketName, policy string) {
		t.Helper()
		if err := f.base.meta.SetBucketConfig(ctx, bucketName, bucket.ConfigPolicy, []byte(policy)); err != nil {
			t.Fatalf("SetBucketConfig(%s): %v", bucketName, err)
		}
	}
	denyGamma := `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":{"AWS":"arn:aws:iam:::user/everything"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::gamma-reports"}]}`

	tests := map[string]struct {
		// prepare, when given, is what happens to the store before the
		// search that is looked at.
		prepare   func(ctx context.Context, t *testing.T, f *searchFixture)
		principal iam.Principal
		in        app.SearchInput
		want      []string
		wantErr   error
		// denied is the action of the one refusal in the audit trail;
		// empty when there must be none.
		denied string
	}{
		"root sees everything": {
			principal: porttest.Root(), in: app.SearchInput{Query: "report"},
			want: []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf", "o:beta/Report.txt", "b:gamma-reports"},
		},
		"some buckets, by an identity policy and by a bucket policy": {
			principal: viewer(), in: app.SearchInput{Query: "report"},
			want: []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf", "o:beta/Report.txt"},
		},
		"nothing found in what may be listed is no refusal": {
			principal: viewer(), in: app.SearchInput{Query: "readme"}, want: []string{},
		},
		"the names of buckets, and no key, for who may list the buckets only": {
			principal: lister(), in: app.SearchInput{Query: "a"}, want: []string{"b:alpha", "b:beta", "b:delta", "b:gamma-reports"},
		},
		"a key is bound by its own policy": {
			principal: iam.Principal{AccessKeyID: "K", User: "viewer", Policies: []iam.Document{listOnly}, Boundary: &boundToGamma},
			in:        app.SearchInput{Query: "re"}, want: []string{"o:gamma-reports/readme.md"},
		},
		"who may list nothing is refused, once": {
			principal: stranger(), in: app.SearchInput{Query: "report"}, wantErr: domain.ErrAccessDenied, denied: "s3:ListAllMyBuckets",
		},
		"nobody is refused, once": {
			principal: anonymous(), in: app.SearchInput{Query: "report"}, wantErr: domain.ErrAccessDenied, denied: "s3:ListAllMyBuckets",
		},
		"who may list everything finds the keys of every bucket": {
			principal: everything, in: app.SearchInput{Query: "readme"}, want: []string{"o:gamma-reports/readme.md"},
		},
		"a bucket whose policy denies it is left out, whatever the caller's own policy allows": {
			prepare: func(ctx context.Context, t *testing.T, f *searchFixture) {
				t.Helper()
				setPolicy(ctx, t, f, "gamma-reports", denyGamma)
			},
			principal: everything, in: app.SearchInput{Query: "readme"}, want: []string{},
		},
		"a bucket that is denied after a search found it is left out by the next": {
			prepare: func(ctx context.Context, t *testing.T, f *searchFixture) {
				t.Helper()
				out, err := f.search.Search(ctx, everything, app.SearchInput{Query: "readme"})
				if err != nil || !slices.Equal(found(out.Results), []string{"o:gamma-reports/readme.md"}) {
					t.Fatalf("before the policy denies: %v, %v", found(out.Results), err)
				}
				setPolicy(ctx, t, f, "gamma-reports", denyGamma)
			},
			principal: everything, in: app.SearchInput{Query: "readme"}, want: []string{},
		},
		"a bucket whose policy was removed is no longer opened by it": {
			prepare: func(ctx context.Context, t *testing.T, f *searchFixture) {
				t.Helper()
				out, err := f.search.Search(ctx, viewer(), app.SearchInput{Query: "report.txt"})
				if err != nil || !slices.Equal(found(out.Results), []string{"o:beta/Report.txt"}) {
					t.Fatalf("while the policy opens the bucket: %v, %v", found(out.Results), err)
				}
				setPolicy(ctx, t, f, "beta", "")
			},
			principal: viewer(), in: app.SearchInput{Query: "report"},
			want: []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf"},
		},
		"one bucket": {
			principal: porttest.Root(), in: app.SearchInput{Query: "report", Bucket: "beta"}, want: []string{"o:beta/Report.txt"},
		},
		"one bucket, whose name is no result": {
			principal: porttest.Root(), in: app.SearchInput{Query: "a", Bucket: "gamma-reports"}, want: []string{"o:gamma-reports/readme.md"},
		},
		"one bucket, opened by its policy": {
			principal: viewer(), in: app.SearchInput{Query: "csv", Bucket: "beta"}, want: []string{"o:beta/data/a.csv", "o:beta/data/b.csv"},
		},
		"one bucket the caller may not list": {
			principal: viewer(), in: app.SearchInput{Query: "readme", Bucket: "gamma-reports"}, wantErr: domain.ErrAccessDenied, denied: "s3:ListBucket",
		},
		"one bucket that is not there, for who could not list it": {
			principal: viewer(), in: app.SearchInput{Query: "readme", Bucket: "nowhere"}, wantErr: domain.ErrAccessDenied, denied: "s3:ListBucket",
		},
		"one bucket that is not there": {
			principal: porttest.Root(), in: app.SearchInput{Query: "readme", Bucket: "nowhere"}, wantErr: domain.ErrBucketNotFound,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newSearchFixture(ctx, t, searchData(), nil)
			f.openBetaToViewer(ctx, t)
			if tt.prepare != nil {
				tt.prepare(ctx, t, f)
			}
			out, err := f.search.Search(ctx, tt.principal, tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Search: %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && (!slices.Equal(found(out.Results), tt.want) || !out.Complete) {
				t.Errorf("found %v (complete: %v), want %v", found(out.Results), out.Complete, tt.want)
			}
			if tt.wantErr != nil && (len(out.Results) != 0 || out.Examined != 0 || out.NextContinuation != "") {
				t.Errorf("a refusal came with %+v", out)
			}
			denied := f.denials(ctx, t)
			refusals := strings.Count(f.base.logs.String(), "authorization denied")
			switch {
			case tt.denied == "" && (len(denied) != 0 || refusals != 0):
				t.Errorf("the search left %d audit records and %d refusals in the log: %+v", len(denied), refusals, denied)
			case tt.denied != "" && (len(denied) != 1 || denied[0].Action != tt.denied || refusals != 1):
				t.Errorf("the refusal left %d audit records and %d lines in the log, want one of %s: %+v", len(denied), refusals, tt.denied, denied)
			}
		})
	}
}

// TestSearchPagesGainNothingFromAToken: every page decides again what the
// caller may see. A token that points into a bucket the caller may not list,
// or at one that is not there, yields nothing from it - and no record.
func TestSearchPagesGainNothingFromAToken(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSearchFixture(ctx, t, searchData(), nil)
	f.openBetaToViewer(ctx, t)
	token := func(bucketName, key string, named bool) string {
		made, err := app.EncodeSearchToken("re", bucketName, key, named)
		if err != nil {
			t.Fatal(err)
		}
		return made
	}
	everything := oracle(t, searchData(), "re", false, viewerMayList)

	tests := map[string]struct {
		continuation string
		want         []string
	}{
		"into a bucket the caller may not list":       {token("gamma-reports", "", false), []string{}},
		"past a key of a bucket they may not list":    {token("gamma-reports", "readme.md", true), []string{}},
		"at a bucket that is not there, before all":   {token("aaa", "zzz", true), everything},
		"at a bucket that is not there, in between":   {token("alpha0", "", false), []string{"o:beta/Report.txt"}},
		"at a bucket that is not there, after all":    {token("zzz", "", false), []string{}},
		"past the last key of a bucket they may list": {token("alpha", "zzz", true), []string{"o:beta/Report.txt"}},
		"inside a bucket they may list":               {token("alpha", "docs/Report-2025.pdf", true), []string{"o:alpha/docs/report-2026.pdf", "o:beta/Report.txt"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := f.search.Search(ctx, viewer(), app.SearchInput{Query: "re", Continuation: tt.continuation})
			if err != nil || !slices.Equal(found(out.Results), tt.want) || !out.Complete {
				t.Errorf("Search = %v (complete: %v), %v; want %v", found(out.Results), out.Complete, err, tt.want)
			}
		})
	}
	if denied := f.denials(ctx, t); len(denied) != 0 {
		t.Errorf("made-up tokens left %d audit records: %+v", len(denied), denied)
	}

	// Whoever may list nothing is refused whatever token they bring, and
	// who lost a bucket between two pages gets nothing more of it.
	if _, err := f.search.Search(ctx, stranger(), app.SearchInput{Query: "re", Continuation: token("beta", "", true)}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Search by a stranger with a token = %v", err)
	}
	first, err := f.search.Search(ctx, viewer(), app.SearchInput{Query: "csv", Limit: 1})
	if err != nil || first.Complete || !slices.Equal(found(first.Results), []string{"o:beta/data/a.csv"}) {
		t.Fatalf("the first page = %+v, %v", first, err)
	}
	if err := f.base.meta.SetBucketConfig(ctx, "beta", bucket.ConfigPolicy, nil); err != nil {
		t.Fatal(err)
	}
	second, err := f.search.Search(ctx, viewer(), app.SearchInput{Query: "csv", Limit: 1, Continuation: first.NextContinuation})
	if err != nil || len(second.Results) != 0 || !second.Complete {
		t.Errorf("the second page, after the bucket was closed = %+v, %v", second, err)
	}
}

// countingIndex counts the walks of the index and the keys each visited,
// and can move a clock forward with every walk.
type countingIndex struct {
	ports.ObjectIndex
	walks   atomic.Int64
	longest atomic.Int64
	clock   *porttest.Clock
	takes   time.Duration
}

func (c *countingIndex) ScanObjects(ctx context.Context, bucketName, start string, visit func(object.Version) ports.ScanControl) error {
	c.walks.Add(1)
	visited := int64(0)
	err := c.ObjectIndex.ScanObjects(ctx, bucketName, start, func(v object.Version) ports.ScanControl {
		visited++
		return visit(v)
	})
	if visited > c.longest.Load() {
		c.longest.Store(visited)
	}
	if c.clock != nil {
		c.clock.Advance(c.takes)
	}
	return err
}

// TestSearchBounds: each bound stops a search with what it has and a token,
// and going on with the token finds, in the end, exactly what an unbounded
// search finds - every match once, in order.
func TestSearchBounds(t *testing.T) {
	t.Parallel()
	const far = 1 << 30
	tests := map[string]struct {
		examined, buckets, chunk int
		walkTakes                time.Duration
		limit                    int
		query                    string
		// first is what the first page must hold.
		first         []string
		firstExamined int
	}{
		"keys examined": {
			examined: 3, buckets: far, chunk: far, query: "report",
			first: []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf"}, firstExamined: 3,
		},
		"keys examined, in chunks": {
			examined: 5, buckets: far, chunk: 2, query: "report",
			first: []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf", "o:beta/Report.txt"}, firstExamined: 5,
		},
		"buckets": {
			examined: far, buckets: 1, chunk: far, query: "report",
			first: []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf"}, firstExamined: 4,
		},
		"time": {
			// Two walks of a second and a half each: after the second
			// the two seconds are over.
			examined: far, buckets: far, chunk: 1, walkTakes: 1500 * time.Millisecond, query: "report",
			first: []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf"}, firstExamined: 2,
		},
		"time, when one walk takes it all": {
			examined: far, buckets: far, chunk: 3, walkTakes: time.Hour, query: "report",
			first: []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf"}, firstExamined: 3,
		},
		"results": {
			examined: far, buckets: far, chunk: far, limit: 1, query: "report",
			first: []string{"o:alpha/docs/Report-2025.pdf"}, firstExamined: 1,
		},
		"results, of bucket names": {
			examined: far, buckets: far, chunk: far, limit: 2, query: "a",
			first: []string{"b:alpha", "b:beta"}, firstExamined: 4,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			var index *countingIndex
			f := newSearchFixture(ctx, t, searchData(), func(next ports.ObjectIndex) ports.ObjectIndex {
				index = &countingIndex{ObjectIndex: next}
				return index
			})
			index.clock, index.takes = f.base.clock, tt.walkTakes
			f.search.WithSearchBounds(tt.examined, tt.buckets, tt.chunk, app.MaxSearchTime)
			in := app.SearchInput{Query: tt.query, Limit: tt.limit}

			first, err := f.search.Search(ctx, porttest.Root(), in)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if first.Complete || first.NextContinuation == "" || !slices.Equal(found(first.Results), tt.first) || first.Examined != tt.firstExamined {
				t.Errorf("the first page: %v, examined %d, complete %v, continuation %q; want %v, %d, not complete and a continuation",
					found(first.Results), first.Examined, first.Complete, first.NextContinuation, tt.first, tt.firstExamined)
			}
			all, pages := drain(ctx, t, f.search, porttest.Root(), in)
			if want := oracle(t, searchData(), tt.query, true, everyBucket); !slices.Equal(all, want) {
				t.Errorf("page by page: %v, want %v", all, want)
			}
			for i, page := range pages {
				if page.Examined > tt.examined {
					t.Errorf("page %d examined %d keys, the bound is %d", i+1, page.Examined, tt.examined)
				}
			}
			// A walk visits a chunk, and one key more to learn that
			// there is more.
			if longest := index.longest.Load(); tt.chunk != far && longest > int64(tt.chunk)+1 {
				t.Errorf("a walk of the index visited %d keys, the chunk is %d", longest, tt.chunk)
			}
		})
	}
}

// TestSearchWithItsOwnBounds: the bounds a node runs with are the ones that
// were decided, and a search within them is one walk a bucket.
func TestSearchWithItsOwnBounds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if app.MaxSearchExamined != 50_000 || app.MaxSearchBuckets != 200 || app.MaxSearchTime != 2*time.Second ||
		app.SearchChunk != 5_000 || app.DefaultSearchLimit != 100 || app.MaxSearchLimit != 500 {
		t.Error("the bounds of a search are not the ones that were decided")
	}
	var index *countingIndex
	f := newSearchFixture(ctx, t, searchData(), func(next ports.ObjectIndex) ports.ObjectIndex {
		index = &countingIndex{ObjectIndex: next}
		return index
	})
	out, err := f.search.Search(ctx, porttest.Root(), app.SearchInput{Query: "*", Limit: app.MaxSearchLimit})
	if err != nil || !out.Complete || len(out.Results) != 14 || out.Examined != 10 || index.walks.Load() != 4 {
		t.Errorf("Search = %d results, examined %d, complete %v, %d walks, %v", len(out.Results), out.Examined, out.Complete, index.walks.Load(), err)
	}
}

// TestSearchPageByPage is the property behind continuation: whatever the
// bounds and whoever asks, paging through a search yields every match
// exactly once and in order - inside a bucket and across buckets.
func TestSearchPageByPage(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// Enough keys for chunks, pages and bounds to end in every place.
	data := searchData()
	for i := range 23 {
		data["alpha"] = append(data["alpha"], fmt.Sprintf("logs/%02d/report.log", i))
		data["gamma-reports"] = append(data["gamma-reports"], fmt.Sprintf("y/%02d.dat", i))
	}
	data["epsilon"] = []string{"report"}
	f := newSearchFixture(ctx, t, data, nil)
	f.openBetaToViewer(ctx, t)

	callers := map[string]struct {
		principal iam.Principal
		names     bool
		listable  func(string) bool
	}{
		"root":   {porttest.Root(), true, everyBucket},
		"viewer": {viewer(), false, viewerMayList},
		"lister": {lister(), true, func(string) bool { return false }},
	}
	for _, query := range []string{"report", "*", "*/0?.*", "rx:^[a-z]+$|\\d\\d", "nothing-like-this"} {
		for who, caller := range callers {
			want := oracle(t, data, query, caller.names, caller.listable)
			for _, limit := range []int{1, 3, 100} {
				for _, chunk := range []int{1, 3, 1000} {
					for _, examined := range []int{1, 5, 1000} {
						for _, buckets := range []int{1, 1000} {
							f.search.WithSearchBounds(examined, buckets, chunk, app.MaxSearchTime)
							all, pages := drain(ctx, t, f.search, caller.principal, app.SearchInput{Query: query, Limit: limit})
							if !slices.Equal(all, want) {
								t.Fatalf("%s searching %q (limit %d, chunk %d, examined %d, buckets %d) in %d pages: %v, want %v",
									who, query, limit, chunk, examined, buckets, len(pages), all, want)
							}
							for _, page := range pages {
								tokenNamesOnly(t, page.NextContinuation, caller.names, caller.listable)
							}
						}
					}
				}
			}
		}
	}
	if denied := f.denials(ctx, t); len(denied) != 0 {
		t.Errorf("paging left %d audit records: %+v", len(denied), denied)
	}
}

// tokenNamesOnly fails when a token, which its holder can read, names a
// bucket the holder may not see.
func tokenNamesOnly(t *testing.T, token string, names bool, listable func(string) bool) {
	t.Helper()
	if token == "" {
		return
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("a token is not base64url: %q", token)
	}
	var fields struct {
		B string `json:"b"`
	}
	if err := json.Unmarshal(decoded, &fields); err != nil {
		t.Fatalf("a token is not JSON: %s", decoded)
	}
	if !names && !listable(fields.B) {
		t.Fatalf("a token names %q, a bucket its holder may not see: %s", fields.B, decoded)
	}
}

// markerIndex is an index whose every bucket begins with a delete marker,
// as the index of a node shows the key of an object that was deleted in a
// bucket that keeps versions.
type markerIndex struct {
	ports.ObjectIndex
}

func (m markerIndex) ScanObjects(ctx context.Context, bucketName, start string, visit func(object.Version) ports.ScanControl) error {
	const gone = "!report-that-is-gone"
	if start <= gone {
		if control := visit(object.Version{Bucket: bucketName, Key: gone, DeleteMarker: true, ModifiedAt: testNow}); control.Stop {
			return nil
		}
	}
	return m.ObjectIndex.ScanObjects(ctx, bucketName, start, visit)
}

// TestSearchDoesNotFindWhatWasDeleted: a delete marker is no result. An
// older version under it is not found either: a search is of what a listing
// shows.
func TestSearchDoesNotFindWhatWasDeleted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSearchFixture(ctx, t, map[string][]string{"kept": {}}, nil)
	if err := f.base.meta.SetBucketVersioning(ctx, "kept", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"report-a.txt", "report-b.txt", "report-c.txt"} {
		f.put(ctx, t, "kept", key)
	}
	deleted, err := f.base.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "kept", Key: "report-b.txt"})
	if err != nil || deleted.Marker == nil {
		t.Fatalf("Delete = %+v, %v", deleted, err)
	}
	out, err := f.search.Search(ctx, porttest.Root(), app.SearchInput{Query: "report"})
	if err != nil || !slices.Equal(found(out.Results), []string{"o:kept/report-a.txt", "o:kept/report-c.txt"}) {
		t.Errorf("Search = %v, %v", found(out.Results), err)
	}

	// The same when the index shows the marker itself: it is looked at,
	// and is no result - on whichever page it falls.
	shown := newSearchFixture(ctx, t, map[string][]string{"kept": {"report-a.txt", "report-c.txt"}}, func(next ports.ObjectIndex) ports.ObjectIndex {
		return markerIndex{ObjectIndex: next}
	})
	out, err = shown.search.Search(ctx, porttest.Root(), app.SearchInput{Query: "report"})
	if err != nil || !slices.Equal(found(out.Results), []string{"o:kept/report-a.txt", "o:kept/report-c.txt"}) || out.Examined != 3 {
		t.Errorf("Search over a marker = %v, examined %d, %v", found(out.Results), out.Examined, err)
	}
	shown.search.WithSearchBounds(1, 1000, 1, app.MaxSearchTime)
	if all, _ := drain(ctx, t, shown.search, porttest.Root(), app.SearchInput{Query: "report"}); !slices.Equal(all, []string{"o:kept/report-a.txt", "o:kept/report-c.txt"}) {
		t.Errorf("Search over a marker, a key a page = %v", all)
	}
}

// gatedIndex is an index whose walks wait until they are let go.
type gatedIndex struct {
	ports.ObjectIndex
	entered chan struct{}
	release chan struct{}
}

func (b *gatedIndex) ScanObjects(ctx context.Context, bucketName, start string, visit func(object.Version) ports.ScanControl) error {
	select {
	case b.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.ObjectIndex.ScanObjects(ctx, bucketName, start, visit)
}

// TestSearchesAtOnce: a principal searches once at a time and a node eight
// times; whoever comes meanwhile is told to come again, and can when a
// search has ended. Two principals who search without pause leave room for
// others.
func TestSearchesAtOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if app.SearchesAtOnce != 8 {
		t.Fatalf("a node runs %d searches at a time, 8 were decided", app.SearchesAtOnce)
	}
	index := &gatedIndex{entered: make(chan struct{}), release: make(chan struct{})}
	f := newSearchFixture(ctx, t, map[string][]string{"only": {"report"}}, func(next ports.ObjectIndex) ports.ObjectIndex {
		index.ObjectIndex = next
		return index
	})
	user := func(name string) iam.Principal {
		return iam.Principal{AccessKeyID: "K-" + name, User: name, Policies: []iam.Document{policyOn("s3:*", "*")}}
	}
	type outcome struct {
		who string
		out app.SearchOutput
		err error
	}
	outcomes := make(chan outcome, app.SearchesAtOnce)
	start := func(p iam.Principal) {
		go func() {
			out, err := f.search.Search(ctx, p, app.SearchInput{Query: "report"})
			outcomes <- outcome{who: p.Name(), out: out, err: err}
		}()
		select {
		case <-index.entered:
		case o := <-outcomes:
			t.Fatalf("the search of %s ended before it walked the index: %+v, %v", o.who, o.out, o.err)
		case <-time.After(10 * time.Second):
			t.Fatalf("the search of %s did not reach the index", p.Name())
		}
	}

	start(user("ann"))
	// The same principal, with another key and from elsewhere, is the same.
	again := user("ann")
	again.AccessKeyID = "K-ann-2"
	if _, err := f.search.Search(ctx, again, app.SearchInput{Query: "report"}); !errors.Is(err, domain.ErrTooManyRequests) || !strings.Contains(err.Error(), "yours") {
		t.Errorf("a second search of the same principal = %v, want %v", err, domain.ErrTooManyRequests)
	}
	// Two who are searching do not keep a third out, nor a seventh.
	start(porttest.Root())
	for i := 2; i < app.SearchesAtOnce; i++ {
		start(user(fmt.Sprintf("user-%d", i)))
	}
	if _, err := f.search.Search(ctx, user("cho"), app.SearchInput{Query: "report"}); !errors.Is(err, domain.ErrTooManyRequests) || !strings.Contains(err.Error(), "others") {
		t.Errorf("a ninth search on the node = %v, want %v", err, domain.ErrTooManyRequests)
	}
	// What is no search takes no place: it is refused for what it is.
	if _, err := f.search.Search(ctx, user("cho"), app.SearchInput{Query: "rx:("}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a bad query while the node is busy = %v, want %v", err, domain.ErrInvalidArgument)
	}

	close(index.release)
	for range app.SearchesAtOnce {
		select {
		case o := <-outcomes:
			if o.err != nil || !slices.Equal(found(o.out.Results), []string{"o:only/report"}) {
				t.Errorf("the search of %s = %v, %v", o.who, found(o.out.Results), o.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a search did not end")
		}
	}
	// All have ended: there is room again, for them and for others. The
	// walks are let through from here on.
	go func() {
		for {
			select {
			case <-index.entered:
			case <-ctx.Done():
				return
			}
		}
	}()
	for _, p := range []iam.Principal{user("ann"), user("cho"), porttest.Root()} {
		if _, err := f.search.Search(ctx, p, app.SearchInput{Query: "report"}); err != nil {
			t.Errorf("a search of %s after the others ended: %v", p.Name(), err)
		}
	}
	// A search that ends in an error gives its place back too.
	for range app.SearchesAtOnce + 1 {
		if _, err := f.search.Search(ctx, user("ann"), app.SearchInput{Query: "report", Bucket: "nowhere"}); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("a search of a bucket that is not there = %v", err)
		}
	}
}

// failingIndex cannot be walked.
type failingIndex struct {
	ports.ObjectIndex
	err error
}

func (f failingIndex) ScanObjects(context.Context, string, string, func(object.Version) ports.ScanControl) error {
	return f.err
}

// TestSearchWhenSomethingIsWrong: a search that cannot be made fails; it
// never passes off part of an answer as the whole. A bucket that went
// between the listing of the buckets and the walk is simply not there, and
// one whose policy is damaged is left out, as a listing of it is refused.
func TestSearchWhenSomethingIsWrong(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	logger := slog.New(slog.DiscardHandler)
	clock := porttest.NewClock(testNow)
	everything := iam.Principal{AccessKeyID: "E", User: "everything", Policies: []iam.Document{policyOn("s3:*", "*")}}

	walk := func(err error) *searchFixture {
		return newSearchFixture(ctx, t, searchData(), func(next ports.ObjectIndex) ports.ObjectIndex {
			return failingIndex{ObjectIndex: next, err: err}
		})
	}
	if _, err := walk(domain.ErrUnavailable).search.Search(ctx, porttest.Root(), app.SearchInput{Query: "report"}); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Search over an index that cannot be read = %v", err)
	}
	gone := fmt.Errorf("%w: it was deleted a moment ago", domain.ErrBucketNotFound)
	out, err := walk(gone).search.Search(ctx, porttest.Root(), app.SearchInput{Query: "report"})
	if err != nil || !slices.Equal(found(out.Results), []string{"b:gamma-reports"}) || !out.Complete {
		t.Errorf("Search over buckets that went = %v, %v", found(out.Results), err)
	}

	f := newSearchFixture(ctx, t, searchData(), nil)
	with := func(source interface {
		app.BucketPolicySource
		ports.BucketPolicies
	},
	) *app.SearchService {
		authz := app.NewAuthorizer(logger, clock, nil).WithBucketPolicies(source)
		s, err := app.NewSearchService(app.SearchServiceConfig{
			Buckets: f.base.meta, Index: f.base.meta, Policies: source, Authz: authz, Clock: clock, Logger: logger,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, err := with(failingPolicies{}).Search(ctx, everything, app.SearchInput{Query: "report"}); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Search while policies cannot be read = %v", err)
	}
	// A store of policies that is damaged is no damaged policy of a bucket:
	// nothing can be decided, and the search fails as a whole instead of
	// leaving out every bucket and calling that an answer.
	_, err = with(unreadablePolicies{}).Search(ctx, everything, app.SearchInput{Query: "report"})
	if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrTooManyBuckets) {
		t.Errorf("Search while the store of policies is damaged = %v, want %v and not %v", err, domain.ErrUnavailable, domain.ErrIntegrity)
	}
	// Damaged policies: the names are still the caller's to see, the keys
	// are not; and who may see no names is refused.
	out, err = with(brokenPolicies{}).Search(ctx, everything, app.SearchInput{Query: "report"})
	if err != nil || !slices.Equal(found(out.Results), []string{"b:gamma-reports"}) || !out.Complete {
		t.Errorf("Search over damaged policies = %v, %v", found(out.Results), err)
	}
	if _, err := with(brokenPolicies{}).Search(ctx, viewer(), app.SearchInput{Query: "report"}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Search over damaged policies by who may list no names = %v", err)
	}
	// One damaged policy among good ones leaves out its bucket and no other,
	// on every call: what does not parse is never remembered as no policy.
	damaged := newSearchFixture(ctx, t, searchData(), nil)
	damaged.openBetaToViewer(ctx, t)
	if err := damaged.base.meta.SetBucketConfig(ctx, "alpha", bucket.ConfigPolicy, []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Deny"`)); err != nil {
		t.Fatal(err)
	}
	for call := range 3 {
		out, err = damaged.search.Search(ctx, viewer(), app.SearchInput{Query: "report"})
		if err != nil || !slices.Equal(found(out.Results), []string{"o:beta/Report.txt"}) || !out.Complete {
			t.Errorf("call %d over one damaged policy = %v, %v; want the bucket of that policy left out", call+1, found(out.Results), err)
		}
	}
	if entries, _ := damaged.authz.ParsedPolicies(); entries != 1 || damaged.authz.ParsedPolicy("alpha") != nil {
		t.Errorf("the authorizer remembers %d policies, and of the damaged one %v; want that of beta alone", entries, damaged.authz.ParsedPolicy("alpha"))
	}
	if logs := damaged.base.logs.String(); strings.Count(logs, "the policy of a bucket cannot be read") != 3 {
		t.Errorf("three searches over a damaged policy were logged as: %s", logs)
	}
	// Root is bound by no policy and is asked nothing about one.
	out, err = with(failingPolicies{}).Search(ctx, porttest.Root(), app.SearchInput{Query: "logo"})
	if err != nil || !slices.Equal(found(out.Results), []string{"o:alpha/img/logo.png"}) {
		t.Errorf("Search by root while policies cannot be read = %v, %v", found(out.Results), err)
	}
	// A bucket the caller names is gated by the policy of that bucket alone.
	if _, err := with(failingPolicies{}).Search(ctx, everything, app.SearchInput{Query: "logo", Bucket: "alpha"}); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Search of one bucket while its policy cannot be read = %v", err)
	}
	// A request that is gone is not searched for.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.search.Search(cancelled, viewer(), app.SearchInput{Query: "logo"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Search for a request that is gone = %v", err)
	}
}

// BucketPolicies makes failingPolicies fail to read a page of the policies
// of all buckets too.
func (failingPolicies) BucketPolicies(context.Context, string, func(string, []byte) error) (string, error) {
	return "", domain.ErrUnavailable
}

// BucketPolicies makes brokenPolicies hold a policy that does not parse for
// every bucket of the search tests, all on one page.
func (brokenPolicies) BucketPolicies(_ context.Context, from string, visit func(string, []byte) error) (string, error) {
	for _, name := range slices.Sorted(maps.Keys(searchData())) {
		if name < from {
			continue
		}
		if err := visit(name, []byte("not a policy")); err != nil {
			return "", err
		}
	}
	return "", nil
}

// unreadablePolicies is a store of policies that is itself damaged: no page
// of it can be read. One bucket at a time it reads as failingPolicies do.
type unreadablePolicies struct{ failingPolicies }

func (unreadablePolicies) BucketPolicies(context.Context, string, func(string, []byte) error) (string, error) {
	return "", fmt.Errorf("%w: stored bucket: unexpected end of JSON input", domain.ErrIntegrity)
}

// manyHidden is the data of the search tests among n buckets that only root
// may list, before, between and after the others.
func manyHidden(n int) map[string][]string {
	data := searchData()
	for i := range n {
		data[fmt.Sprintf("%c-hidden-%03d", "abcdefghijklmnopqrstuvwxyz"[i%26], i)] = []string{"report-that-is-hidden"}
	}
	return data
}

// TestSearchCostsOneReadingOfThePolicies: before it looks at a key, a page
// walks the policies of all buckets once - here four pages of a hundred
// buckets - and asks the authorizer once about every bucket, however many
// buckets are not the caller's, whoever the caller is and whatever
// continuation they bring. A reading takes a quarter of a second here: three
// hundred of them, one for each bucket, would be more than a minute.
func TestSearchCostsOneReadingOfThePolicies(t *testing.T) {
	t.Parallel()
	const hidden = 300
	data := manyHidden(hidden)
	buckets := int64(len(data))
	token := func(t *testing.T, bucketName, key string, named bool) string {
		t.Helper()
		made, err := app.EncodeSearchToken("report", bucketName, key, named)
		if err != nil {
			t.Fatal(err)
		}
		return made
	}
	type cost struct {
		readings, single, decisions int64
		took                        time.Duration
	}
	// page asks for one page and says what it cost.
	page := func(ctx context.Context, t *testing.T, f *searchFixture, principal iam.Principal, continuation string) (app.SearchOutput, cost, error) {
		t.Helper()
		f.policies.all.Store(0)
		f.policies.one.Store(0)
		f.asked.filtered.Store(0)
		began := f.base.clock.Now()
		out, err := f.search.Search(ctx, principal, app.SearchInput{Query: "report", Continuation: continuation})
		return out, cost{
			readings: f.policies.all.Load(), single: f.policies.one.Load(), decisions: f.asked.filtered.Load(),
			took: f.base.clock.Now().Sub(began),
		}, err
	}
	// What a page costs: one walk of all policies, which is four readings
	// and takes the second, none of a single bucket, and a decision for the
	// names of the buckets and one for each bucket.
	const policyPage, reading = 100, 250 * time.Millisecond
	want := cost{readings: (buckets + policyPage - 1) / policyPage, decisions: 1 + buckets, took: time.Second}

	callers := map[string]struct {
		principal iam.Principal
		wantErr   error
		first     []string
	}{
		"who may list two buckets": {viewer(), nil, []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf", "o:beta/Report.txt"}},
		"who may list nothing":     {stranger(), domain.ErrAccessDenied, nil},
		"nobody":                   {anonymous(), domain.ErrAccessDenied, nil},
	}
	for who, caller := range callers {
		t.Run(who, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newSearchFixture(ctx, t, data, nil)
			f.openBetaToViewer(ctx, t)
			f.base.meta.PolicyPage, f.policies.takes = policyPage, reading

			out, got, err := page(ctx, t, f, caller.principal, "")
			if !errors.Is(err, caller.wantErr) || (err == nil && (!slices.Equal(found(out.Results), caller.first) || !out.Complete)) {
				t.Fatalf("Search = %v (complete: %v), %v; want %v, %v", found(out.Results), out.Complete, err, caller.first, caller.wantErr)
			}
			if got != want {
				t.Errorf("the page cost %+v, want %+v", got, want)
			}
			if got.took >= app.MaxSearchTime {
				t.Errorf("the page took %s, a search has %s", got.took, app.MaxSearchTime)
			}
			// Tokens somebody made up, which name a bucket: whatever
			// bucket, there or not, theirs or not, the page costs the same.
			forged := map[string]string{
				"before every bucket":            token(t, "aaa", "", false),
				"a bucket that is not theirs":    token(t, "c-hidden-002", "", false),
				"inside one that is not theirs":  token(t, "c-hidden-002", "report-that-is-hidden", true),
				"just after one that is not":     token(t, "c-hidden-0020", "", false),
				"a bucket that is not there":     token(t, "c-hidden-999", "", false),
				"a bucket that is theirs":        token(t, "alpha", "docs/Report-2025.pdf", true),
				"after every bucket":             token(t, "zzz", "", false),
				"after every bucket, and a key":  token(t, "zzz", strings.Repeat("k", object.MaxKeyLength), true),
				"the last bucket that is hidden": token(t, "z-hidden-285", "", false),
			}
			for name, continuation := range forged {
				_, got, err := page(ctx, t, f, caller.principal, continuation)
				if !errors.Is(err, caller.wantErr) {
					t.Errorf("a token %s: %v, want %v", name, err, caller.wantErr)
				}
				if got != want {
					t.Errorf("a token %s: the page cost %+v, want %+v", name, got, want)
				}
			}
			// Tokens whose bucket is no name a bucket can have are no
			// tokens: refused for what they are, before anything is read.
			// So a name and the same name with a byte after it cannot be
			// told apart by what they cost.
			for _, name := range []string{"a", "zz", "c-hidden-002\x00", "alpha\x00", "Alpha", "a b"} {
				_, got, err := page(ctx, t, f, caller.principal, token(t, name, "", false))
				if !errors.Is(err, domain.ErrInvalidArgument) || got != (cost{}) {
					t.Errorf("a token of the bucket %q: %v at the cost of %+v; want %v and no cost", name, err, got, domain.ErrInvalidArgument)
				}
			}
			// The refusal these pages end in - repeats of it are counted,
			// not written again - is all the audit trail holds: no record
			// of a bucket that was passed over.
			denied := f.denials(ctx, t)
			if (caller.wantErr == nil) != (len(denied) == 0) {
				t.Errorf("the pages left %d audit records: %+v", len(denied), denied)
			}
			for _, record := range denied {
				if record.Action != "s3:ListAllMyBuckets" {
					t.Errorf("a search left the audit record %+v", record)
				}
			}
		})
	}

	// Root is bound by no bucket policy: nothing is read for it.
	t.Run("root", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f := newSearchFixture(ctx, t, data, nil)
		f.base.meta.PolicyPage, f.policies.takes = policyPage, reading
		if _, got, err := page(ctx, t, f, porttest.Root(), ""); err != nil || got != (cost{decisions: 1 + buckets}) {
			t.Errorf("a page for root: %v at the cost of %+v", err, got)
		}
	})
}

// TestSearchTellsNothingOfHiddenBuckets: buckets the caller may not see
// change nothing of what a search says - not its results, not how many keys
// a page examined, not how many pages there are, not where a page stopped.
func TestSearchTellsNothingOfHiddenBuckets(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	run := func(data map[string][]string, examined, buckets, limit int) []app.SearchOutput {
		f := newSearchFixture(ctx, t, data, nil)
		f.openBetaToViewer(ctx, t)
		f.search.WithSearchBounds(examined, buckets, 2, app.MaxSearchTime)
		_, pages := drain(ctx, t, f.search, viewer(), app.SearchInput{Query: "re", Limit: limit})
		return pages
	}
	for _, bounds := range [][3]int{{1000, 1, 100}, {3, 1000, 100}, {1000, 1000, 1}, {2, 1, 2}} {
		plain := run(searchData(), bounds[0], bounds[1], bounds[2])
		among := run(manyHidden(60), bounds[0], bounds[1], bounds[2])
		if len(plain) < 2 {
			t.Fatalf("bounds %v: the search took %d pages, the test needs several", bounds, len(plain))
		}
		if len(among) != len(plain) {
			t.Fatalf("bounds %v: %d pages among hidden buckets, %d without them", bounds, len(among), len(plain))
		}
		for i := range plain {
			a, b := among[i], plain[i]
			if !slices.Equal(found(a.Results), found(b.Results)) || a.Examined != b.Examined || a.Complete != b.Complete ||
				a.NextContinuation != b.NextContinuation {
				t.Errorf("bounds %v, page %d among hidden buckets: %v, examined %d, complete %v, %q; without them: %v, %d, %v, %q", bounds, i+1,
					found(a.Results), a.Examined, a.Complete, a.NextContinuation, found(b.Results), b.Examined, b.Complete, b.NextContinuation)
			}
			tokenNamesOnly(t, a.NextContinuation, false, viewerMayList)
		}
	}
}

// TestSearchKeepsItsTimeWithASlowAuthorizer: deciding which buckets are the
// caller's counts towards the time of a call. When the decisions leave time,
// the call does one walk and says where it stopped; when they take it all,
// the call fails as a service that is busy does - it does not go on for as
// long as there are buckets, and does not say how far it got.
func TestSearchKeepsItsTimeWithASlowAuthorizer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Four buckets at 400 ms a decision: the time is over when the walk
	// begins, so every page does one bucket and paging still ends.
	f := newSearchFixture(ctx, t, searchData(), nil)
	f.asked.takes.Store(int64(400 * time.Millisecond))
	began := f.base.clock.Now()
	first, err := f.search.Search(ctx, porttest.Root(), app.SearchInput{Query: "report"})
	if err != nil || first.Complete || !slices.Equal(found(first.Results), []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf"}) {
		t.Fatalf("the first page = %v (complete: %v), %v", found(first.Results), first.Complete, err)
	}
	if took := f.base.clock.Now().Sub(began); took != 2*time.Second {
		t.Errorf("the first page took %s of five decisions at 400ms", took)
	}
	all, pages := drain(ctx, t, f.search, porttest.Root(), app.SearchInput{Query: "report"})
	if want := oracle(t, searchData(), "report", true, everyBucket); !slices.Equal(all, want) || len(pages) != 4 {
		t.Errorf("page by page with a slow authorizer: %v in %d pages, want %v in 4", all, len(pages), want)
	}

	// Three hundred buckets at 300 ms a decision would be a minute and a
	// half. The call gives up when its two seconds are over.
	for who, principal := range map[string]iam.Principal{"viewer": viewer(), "stranger": stranger(), "lister": lister(), "root": porttest.Root()} {
		many := newSearchFixture(ctx, t, manyHidden(300), nil)
		// The first bucket there is has a policy, which opens it to nobody.
		closed := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::a-hidden-000"}]}`
		if err := many.base.meta.SetBucketConfig(ctx, "a-hidden-000", bucket.ConfigPolicy, []byte(closed)); err != nil {
			t.Fatal(err)
		}
		many.asked.takes.Store(int64(300 * time.Millisecond))
		began := many.base.clock.Now()
		out, err := many.search.Search(ctx, principal, app.SearchInput{Query: "report"})
		if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrAccessDenied) || len(out.Results) != 0 || out.NextContinuation != "" {
			t.Errorf("%s: Search = %+v, %v; want %v and nothing else", who, out, err, domain.ErrUnavailable)
		}
		// It says which kind of unavailable: the one that may be a limit.
		if !errors.Is(err, domain.ErrTooManyBuckets) {
			t.Errorf("%s: Search = %v, want %v with it", who, err, domain.ErrTooManyBuckets)
		}
		// The decision about the names, then decisions until the time of
		// a call is over: seven of 300 ms.
		if took, asked := many.base.clock.Now().Sub(began), many.asked.filtered.Load(); took > app.MaxSearchTime+2*300*time.Millisecond || asked != 8 {
			t.Errorf("%s: the call took %s and %d decisions; a search has %s", who, took, asked, app.MaxSearchTime)
		}
		if denied := many.denials(ctx, t); len(denied) != 0 {
			t.Errorf("%s: a search that ran out of time left %d audit records", who, len(denied))
		}
		// What the call parsed before it gave up is remembered, so that
		// the same call made again has less to do: after a restart, calls
		// that fail so get further each time. Root reads no policy.
		if entries, _ := many.authz.ParsedPolicies(); (entries == 1) != (who != "root") {
			t.Errorf("%s: after a search that ran out of time the authorizer remembers %d policies", who, entries)
		}
		// The same call, when the authorizer is quick again, is answered.
		many.asked.takes.Store(0)
		if _, err := many.search.Search(ctx, principal, app.SearchInput{Query: "report"}); err != nil && (who != "stranger" || !errors.Is(err, domain.ErrAccessDenied)) {
			t.Errorf("%s: the search again: %v", who, err)
		}
	}
}

// TestSearchPagesWhileTheStoreChanges is the property of paging through a
// store that is written to between the pages: objects are put and deleted,
// buckets made, before and after the place the search has reached. Every key
// that was there before the first page and is there after the last is found
// exactly once; nothing is found twice; everything found comes in order.
func TestSearchPagesWhileTheStoreChanges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, limit := range []int{1, 2, 5} {
		for _, chunk := range []int{1, 3, 1000} {
			for _, buckets := range []int{1, 1000} {
				data := searchData()
				for i := range 12 {
					data["alpha"] = append(data["alpha"], fmt.Sprintf("logs/%02d/report.log", i))
					data["gamma-reports"] = append(data["gamma-reports"], fmt.Sprintf("y/%02d-report.dat", i))
				}
				f := newSearchFixture(ctx, t, data, nil)
				f.search.WithSearchBounds(1000, buckets, chunk, app.MaxSearchTime)
				// survivors are the keys nothing below deletes.
				survivors := map[[2]string]bool{}
				for name, keys := range data {
					for _, key := range keys {
						survivors[[2]string{name, key}] = true
					}
				}
				remove := func(bucketName, key string) {
					if !survivors[[2]string{bucketName, key}] {
						return
					}
					if _, err := f.base.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: bucketName, Key: key}); err != nil {
						t.Fatalf("Delete(%s/%s): %v", bucketName, key, err)
					}
					delete(survivors, [2]string{bucketName, key})
				}
				create := func(name string) {
					if err := f.base.meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
						t.Fatalf("CreateBucket(%s): %v", name, err)
					}
					f.put(ctx, t, name, "report-in-a-new-bucket")
				}
				// change is what happens after the page of that number.
				change := func(page int) {
					switch page % 6 {
					case 0:
						f.put(ctx, t, "alpha", fmt.Sprintf("a-report-put-early-%03d", page))
						f.put(ctx, t, "gamma-reports", fmt.Sprintf("zz/report-put-late-%03d", page))
					case 1:
						remove("alpha", fmt.Sprintf("logs/%02d/report.log", page%12))
						remove("gamma-reports", fmt.Sprintf("y/%02d-report.dat", (page+5)%12))
					case 2:
						create(fmt.Sprintf("aaa-new-%03d", page))
						create(fmt.Sprintf("zzz-new-%03d", page))
					case 3:
						f.put(ctx, t, "beta", fmt.Sprintf("data/report-%03d.csv", page))
						remove("beta", "Report.txt")
					case 4:
						remove("alpha", "docs/Report-2025.pdf")
						create(fmt.Sprintf("between-%03d", page))
					default:
						// A key that is there is written again.
						f.put(ctx, t, "gamma-reports", "readme.md")
						f.put(ctx, t, "alpha", "docs/report-2026.pdf")
					}
				}

				in := app.SearchInput{Query: "rx:(?i)report|readme", Limit: limit}
				query, err := search.Parse(in.Query)
				if err != nil {
					t.Fatal(err)
				}
				seen, last, pages := map[[2]string]int{}, [2]string{}, 0
				for {
					out, err := f.search.Search(ctx, porttest.Root(), in)
					if err != nil {
						t.Fatalf("limit %d, chunk %d, buckets %d: page %d: %v", limit, chunk, buckets, pages+1, err)
					}
					pages++
					for _, r := range out.Results {
						if r.Kind != app.SearchResultObject {
							continue
						}
						at := [2]string{r.Bucket, r.Key}
						if at[0] < last[0] || (at[0] == last[0] && at[1] <= last[1]) {
							t.Fatalf("limit %d, chunk %d, buckets %d: page %d found %v after %v", limit, chunk, buckets, pages, at, last)
						}
						last = at
						seen[at]++
					}
					if out.Complete {
						break
					}
					if pages > 2000 {
						t.Fatalf("limit %d, chunk %d, buckets %d: the search does not end", limit, chunk, buckets)
					}
					in.Continuation = out.NextContinuation
					change(pages)
				}
				if pages < 3 {
					t.Fatalf("limit %d, chunk %d, buckets %d: the search took %d pages, the test needs writes between pages", limit, chunk, buckets, pages)
				}
				matching := 0
				for at := range survivors {
					if !query.Match(at[1]) {
						continue
					}
					matching++
					if seen[at] != 1 {
						t.Errorf("limit %d, chunk %d, buckets %d: %v, which was there all along, was found %d times in %d pages", limit, chunk, buckets, at, seen[at], pages)
					}
				}
				if matching < 10 || matching == len(seen) && pages > 6 {
					// Some of what was put between pages lies ahead of
					// the search and is found: the test would say little
					// if nothing were.
					t.Errorf("limit %d, chunk %d, buckets %d: %d keys survived and %d were found in %d pages", limit, chunk, buckets, matching, len(seen), pages)
				}
			}
		}
	}
}

// orderedReads writes down, in order, the listings of the buckets and the
// readings of their policies that a search makes.
type orderedReads struct {
	ports.BucketStore
	policies ports.BucketPolicies

	mu    sync.Mutex
	reads []string
}

func (o *orderedReads) note(read string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reads = append(o.reads, read)
}

func (o *orderedReads) ListBuckets(ctx context.Context) ([]bucket.Bucket, error) {
	list, err := o.BucketStore.ListBuckets(ctx)
	// Written down when the listing is over: a reading of policies that
	// began before then would stand before it.
	o.note("buckets")
	return list, err
}

func (o *orderedReads) GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error) {
	o.note("policy of " + name)
	return o.BucketStore.GetBucketConfig(ctx, name, kind)
}

func (o *orderedReads) BucketPolicies(ctx context.Context, from string, visit func(string, []byte) error) (string, error) {
	o.note("policies")
	return o.policies.BucketPolicies(ctx, from, visit)
}

// TestSearchListsTheBucketsBeforeItReadsTheirPolicies: the policies are read
// from this node's state without asking the leader, and that is as good as a
// consistent read only after one: so the one consistent listing of the
// buckets comes first, for whoever asks, and every page of policies after
// it. Root causes no reading of a policy at all.
func TestSearchListsTheBucketsBeforeItReadsTheirPolicies(t *testing.T) {
	t.Parallel()
	for who, principal := range map[string]iam.Principal{"viewer": viewer(), "stranger": stranger(), "lister": lister(), "root": porttest.Root()} {
		t.Run(who, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newSearchFixture(ctx, t, searchData(), nil)
			f.openBetaToViewer(ctx, t)
			reads := &orderedReads{BucketStore: f.base.meta, policies: f.base.meta}
			logger := slog.New(slog.DiscardHandler)
			authz := app.NewAuthorizer(logger, f.base.clock, nil).WithBucketPolicies(reads)
			s, err := app.NewSearchService(app.SearchServiceConfig{
				Buckets: reads, Index: f.base.meta, Policies: reads, Authz: authz, Clock: f.base.clock, Logger: logger,
			})
			if err != nil {
				t.Fatal(err)
			}
			for call := range 2 {
				reads.mu.Lock()
				reads.reads = nil
				reads.mu.Unlock()
				if _, err := s.Search(ctx, principal, app.SearchInput{Query: "report"}); err != nil && (who != "stranger" || !errors.Is(err, domain.ErrAccessDenied)) {
					t.Fatalf("call %d: %v", call+1, err)
				}
				// Four buckets, three to a page of the fake: two pages.
				want := []string{"buckets", "policies", "policies"}
				if who == "root" {
					want = []string{"buckets"}
				}
				if !slices.Equal(reads.reads, want) {
					t.Errorf("call %d read %v, want %v", call+1, reads.reads, want)
				}
			}
		})
	}
}

// TestSearchRemembersParsedPolicies: from one search to the next the
// authorizer remembers what the policies of the buckets say, by the hash of
// the document it read - so that a page hashes every policy and parses only
// those that changed - and nothing else: a changed policy is parsed again, a
// policy that is gone is forgotten, and what finds no room is parsed every
// time and decides all the same.
func TestSearchRemembersParsedPolicies(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSearchFixture(ctx, t, searchData(), nil)
	f.openBetaToViewer(ctx, t)
	set := func(bucketName, policy string) {
		t.Helper()
		if err := f.base.meta.SetBucketConfig(ctx, bucketName, bucket.ConfigPolicy, []byte(policy)); err != nil {
			t.Fatalf("SetBucketConfig(%s): %v", bucketName, err)
		}
	}
	open := func(bucketName string) string {
		return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/viewer"},` +
			`"Action":"s3:ListBucket","Resource":"arn:aws:s3:::` + bucketName + `"}]}`
	}
	shut := func(bucketName string) string { return strings.Replace(open(bucketName), "Allow", "Deny", 1) }
	// lists says which buckets a search by viewer looked into.
	lists := func(want ...string) {
		t.Helper()
		all, _ := drain(ctx, t, f.search, viewer(), app.SearchInput{Query: "rx:."})
		got := []string{}
		for _, name := range all {
			if b, _, _ := strings.Cut(strings.TrimPrefix(name, "o:"), "/"); !slices.Contains(got, b) {
				got = append(got, b)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("viewer searched %v, want %v", got, want)
		}
	}
	remembered := func(entries int, documents ...string) {
		t.Helper()
		size := 0
		for _, d := range documents {
			size += len(d)
		}
		if n, bytes := f.authz.ParsedPolicies(); n != entries || bytes != size {
			t.Errorf("the authorizer remembers %d policies of %d bytes, want %d of %d", n, bytes, entries, size)
		}
	}

	// Root reads no policy and so leaves nothing to remember.
	if _, err := f.search.Search(ctx, porttest.Root(), app.SearchInput{Query: "report"}); err != nil {
		t.Fatal(err)
	}
	remembered(0)

	set("beta", open("beta"))
	set("gamma-reports", open("gamma-reports"))
	lists("alpha", "beta", "gamma-reports")
	remembered(2, open("beta"), open("gamma-reports"))
	beta, gamma := f.authz.ParsedPolicy("beta"), f.authz.ParsedPolicy("gamma-reports")
	if beta == nil || gamma == nil {
		t.Fatalf("remembered of beta %v, of gamma-reports %v", beta, gamma)
	}

	// The next search parses nothing: what is remembered is what was.
	lists("alpha", "beta", "gamma-reports")
	if f.authz.ParsedPolicy("beta") != beta || f.authz.ParsedPolicy("gamma-reports") != gamma {
		t.Error("policies that did not change were parsed again")
	}

	// A policy that changed is parsed again, and it decides: no search is
	// answered from what the policy said before.
	set("gamma-reports", shut("gamma-reports"))
	lists("alpha", "beta")
	remembered(2, open("beta"), shut("gamma-reports"))
	if f.authz.ParsedPolicy("beta") != beta || f.authz.ParsedPolicy("gamma-reports") == gamma {
		t.Error("after one policy changed: that one was not parsed again, or the other was")
	}
	// The same document as before, set again, says what it said before.
	set("gamma-reports", open("gamma-reports"))
	lists("alpha", "beta", "gamma-reports")

	// A request that is no search reads the policy of its bucket from the
	// store - one consistent read - and is decided by what it read there,
	// whatever is remembered: it is the gate.
	f.policies.one.Store(0)
	f.policies.all.Store(0)
	request := iam.Request{Principal: viewer(), Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: "gamma-reports"}}
	if err := f.authz.Authorize(app.WithRequestScope(ctx), request); err != nil {
		t.Errorf("Authorize by the policy of the bucket: %v", err)
	}
	set("gamma-reports", shut("gamma-reports"))
	if err := f.authz.Authorize(app.WithRequestScope(ctx), request); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Authorize after the policy was changed, and no search since: %v", err)
	}
	if one, all := f.policies.one.Load(), f.policies.all.Load(); one != 2 || all != 0 {
		t.Errorf("two requests that are no search read %d policies and %d pages of all, want 2 and none", one, all)
	}
	if f.authz.ParsedPolicy("gamma-reports") == nil {
		t.Error("a request that is no search changed what is remembered")
	}

	// A policy that is removed, and one whose bucket is deleted, are
	// forgotten by the next search that walks all policies.
	set("gamma-reports", "")
	lists("alpha", "beta")
	remembered(1, open("beta"))
	set("delta", open("delta"))
	lists("alpha", "beta")
	remembered(2, open("beta"), open("delta"))
	if err := f.base.meta.DeleteBucket(ctx, "delta"); err != nil {
		t.Fatal(err)
	}
	lists("alpha", "beta")
	remembered(1, open("beta"))

	// A search of one bucket reads that bucket's policy from the store.
	f.policies.one.Store(0)
	f.policies.all.Store(0)
	if _, err := f.search.Search(ctx, viewer(), app.SearchInput{Query: "report", Bucket: "beta"}); err != nil {
		t.Errorf("a search of one bucket: %v", err)
	}
	if one, all := f.policies.one.Load(), f.policies.all.Load(); one != 1 || all != 0 {
		t.Errorf("a search of one bucket read %d policies and %d pages of all, want 1 and none", one, all)
	}

	// Where there is room for one policy, one is remembered; the others are
	// parsed by every search, and decide as they would.
	f.authz.WithParsedPolicyBounds(1, 1<<20)
	if err := f.base.meta.CreateBucket(ctx, bucket.Bucket{Name: "delta", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	f.put(ctx, t, "delta", "d.txt")
	set("delta", open("delta"))
	set("gamma-reports", open("gamma-reports"))
	for range 2 {
		lists("alpha", "beta", "delta", "gamma-reports")
		remembered(1, open("beta"))
	}
	// And where there is room for no document of their size, none: the
	// one that was remembered goes when its policy changes.
	f.authz.WithParsedPolicyBounds(10, len(shut("beta"))-1)
	set("beta", shut("beta"))
	lists("alpha", "delta", "gamma-reports")
	remembered(0)
	set("beta", open("beta"))
	lists("alpha", "beta", "delta", "gamma-reports")
	remembered(0)
}

// TestSearchesRememberPoliciesTogether: searches of several principals at
// once, while policies change, share what the authorizer remembers, and each
// is decided by a policy that was the bucket's when it read it.
func TestSearchesRememberPoliciesTogether(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSearchFixture(ctx, t, manyHidden(40), nil)
	f.openBetaToViewer(ctx, t)
	open := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::%s"}]}`
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			who := iam.Principal{AccessKeyID: fmt.Sprintf("K%d", i), User: fmt.Sprintf("user-%d", i), Policies: viewer().Policies}
			for range 20 {
				out, err := f.search.Search(ctx, who, app.SearchInput{Query: "rx:^docs/"})
				if err != nil || !slices.Equal(found(out.Results), []string{"o:alpha/docs/Report-2025.pdf", "o:alpha/docs/report-2026.pdf"}) {
					t.Errorf("%s: %v, %v", who.User, found(out.Results), err)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for i := range 60 {
			name := fmt.Sprintf("%c-hidden-%03d", "abcdefghijklmnopqrstuvwxyz"[i%40%26], i%40)
			document := fmt.Appendf(nil, open, name)
			if i%3 == 2 {
				document = nil
			}
			if err := f.base.meta.SetBucketConfig(ctx, name, bucket.ConfigPolicy, document); err != nil {
				t.Errorf("SetBucketConfig(%s): %v", name, err)
				return
			}
		}
	})
	wg.Wait()
	if entries, _ := f.authz.ParsedPolicies(); entries == 0 || entries > 41 {
		t.Errorf("the authorizer remembers %d policies of at most 41", entries)
	}
}
