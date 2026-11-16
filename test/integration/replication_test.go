//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/albievan/cactusfs/test/clustertest"
)

const (
	// siteB is the name of the standalone node on the cluster's network:
	// the host of the target's endpoint and the name in its certificate.
	siteB = "siteb"
	// siteSource is the bucket of site A that is replicated, siteReplica
	// the bucket of site B that is its replica, and siteTarget the target
	// of site A that names site B.
	siteSource  = "source"
	siteReplica = "replica"
	siteTarget  = "siteb"
	// siteKeys keys are written siteRounds times each - 500 versions - by
	// siteWorkers writers, through the four nodes in turn; every second key
	// is then deleted, a delete marker each: 50.
	siteKeys    = 100
	siteRounds  = 5
	siteWorkers = 8
	// siteBigSize is the size of the multipart object, in parts of
	// sitePartSize and what is left: 5, 5 and 2 MiB.
	siteBigSize  = 12 << 20
	sitePartSize = 5 << 20
	// siteRetention is how long the retained version is retained: far
	// beyond the 15 minutes within which a retention is left out of a copy
	// (ADR-0026, "Open" 7), and beyond the test.
	siteRetention = 2 * time.Hour
	// sitePause is how long site B is paused while site A takes writes,
	// one every sitePausePace.
	sitePause     = time.Minute
	sitePausePace = 2 * time.Second
	// siteSetup bounds each step of the setting up - a site that answers
	// 503 until it writes at metadata level 5, a usage record not yet
	// exact.
	siteSetup = 2 * time.Minute
	// siteArrival bounds how long what site A acknowledged may take to be
	// at site B: the first burst, and what waited while site B was paused.
	// A paused site takes the connection and never finishes the TLS
	// handshake, which the egress client gives 5 s; after a minute of
	// that a target has failed six times (5 s an attempt, then pauses of
	// 1, 2, 4, 8 and 16 s) and its next pause is Backoff(6) = 32 s - 24 to
	// 32 s with jitter (delivery.Backoff doubles from 1 s). Four minutes is
	// that pause, the next of 64 s and the sending, which takes seconds.
	siteArrival = 4 * time.Minute
	// siteResyncLimit bounds a resync of some 600 versions: it is found
	// within ResyncScanEvery (10 s), and walks one version a round trip.
	siteResyncLimit = 3 * time.Minute
	// siteCustomerKey is a key of a customer for SSE-C - thirty-two times
	// "k" - in base64, with the MD5 digest S3 wants beside it.
	siteCustomerKey    = "a2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2s="
	siteCustomerKeyMD5 = "mT2HRsMGJ5IX5C+0rreZ8Q=="
)

// siteReplicatePolicy is everything the key of site A at site B holds
// (ADR-0026, "Credentials, and what they can do"): the replicate actions
// and those of the later changes, on the replica bucket and its objects.
const siteReplicatePolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
	`"Action":["s3:ReplicateObject","s3:ReplicateDelete","s3:PutObjectTagging","s3:DeleteObjectTagging","s3:PutObjectRetention","s3:PutObjectLegalHold"],` +
	`"Resource":["arn:aws:s3:::` + siteReplica + `","arn:aws:s3:::` + siteReplica + `/*"]}]}`

// errSiteNotYet says that what a site shows is not yet what it is waited
// for, and errSiteUnreachable that it could not be read this time: both
// are asked again until a deadline. Any other error of a judgement is final.
var (
	errSiteNotYet      = errors.New("not yet")
	errSiteUnreachable = errors.New("could not be read")
)

// siteShown is a body of an answer as a message may show it: never one that
// holds a secret key (the answer that creates an API key).
func siteShown(body []byte) string {
	if bytes.Contains(body, []byte("secret_key")) {
		return "(a body that holds a secret key, not shown)"
	}
	return string(body)
}

// siteAdmin is the management API of one site: the hosts that take its
// requests, tried in turn, the client of each, and who asks.
type siteAdmin struct {
	name   string
	hosts  []string
	client func(host string) *http.Client
	auth   map[string]string
}

// call sends one request to one host. A PUT and a POST say they are JSON,
// with a body or without one.
func (a siteAdmin) call(ctx context.Context, host string, headers map[string]string, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+host+":9001/api/v1"+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if method == http.MethodPut || method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range a.auth {
		req.Header.Set(name, value)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := a.client(host).Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s through %s: %w", method, path, host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s through %s: %w", method, path, host, err)
	}
	return resp.StatusCode, data, nil
}

// change sends an administrative change through the hosts in turn until
// one answers want, and returns the answer's body. The change has one
// Idempotency-Key, drawn once and sent with every attempt: an attempt that
// was taken and whose answer was lost is answered as it was when it is
// sent again, so a creation - an API key above all - is never made twice.
// A failure of the connection, a 5xx, a 429 (a request whose body may hold
// a secret, recognised by a slow hash) and a 409 that says the state or the
// key is not settled yet (Conflict: a usage record that is not exact;
// RequestInProgress: the key is reserved by an attempt still in progress)
// are tried again within siteSetup; EntityAlreadyExists after an attempt is
// the change made by that attempt; any other answer fails the test. No
// message shows a body that holds a secret key.
func (a siteAdmin) change(ctx context.Context, t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	deadline := time.Now().Add(siteSetup)
	idempotency := map[string]string{"Idempotency-Key": "site-" + randomToken(t)}
	var last string
	for try := 0; ; try++ {
		host := a.hosts[try%len(a.hosts)]
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		status, answer, err := a.call(attempt, host, idempotency, method, path, body)
		cancel()
		code := problemCode(answer)
		switch {
		case err == nil && status == want:
			return answer
		case err == nil && status == http.StatusConflict && code == "EntityAlreadyExists" && try > 0:
			return nil
		case err != nil:
			last = err.Error()
		case status >= 500, status == http.StatusTooManyRequests,
			status == http.StatusConflict && (code == "Conflict" || code == "RequestInProgress"):
			last = fmt.Sprintf("%d %s", status, siteShown(answer))
		default:
			t.Fatalf("%s %s at site %s through %s = %d %s; want %d", method, path, a.name, host, status, siteShown(answer), want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s at site %s not answered %d within %s: %s", method, path, a.name, want, siteSetup, last)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s %s at site %s: %v", method, path, a.name, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// siteStatus is what GET /buckets/{bucket}/replication/status says.
type siteStatus struct {
	Target         string `json:"target"`
	TargetState    string `json:"target_state"`
	LastErrorClass string `json:"last_error_class"`
	Pending        int64  `json:"pending"`
	Failed         int64  `json:"failed"`
	Lost           int64  `json:"lost"`
	ResyncNeeded   bool   `json:"resync_needed"`
	Resync         *struct {
		State   string `json:"state"`
		Scanned int64  `json:"scanned"`
		Sent    int64  `json:"sent"`
	} `json:"resync"`
}

// status asks a host of site A for the status of the source bucket's
// replication.
func (a siteAdmin) status(ctx context.Context, host string) (siteStatus, error) {
	attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, body, err := a.call(attempt, host, nil, http.MethodGet, "/buckets/"+siteSource+"/replication/status", nil)
	if err != nil {
		return siteStatus{}, err
	}
	if code != http.StatusOK {
		return siteStatus{}, fmt.Errorf("the replication status through %s = %d %s", host, code, body)
	}
	var shown siteStatus
	if err := json.Unmarshal(body, &shown); err != nil {
		return siteStatus{}, fmt.Errorf("the replication status through %s: %w: %s", host, err, body)
	}
	return shown, nil
}

// siteEntry is a version or a delete marker as ListObjectVersions lists it.
type siteEntry struct {
	Key, VersionID, ETag string
	Size                 int64
	Latest, Marker       bool
	Modified             time.Time
}

func (e siteEntry) String() string {
	kind := "version"
	if e.Marker {
		kind = "marker"
	}
	return fmt.Sprintf("%s %s of %q (latest %t, %d bytes, ETag %s, %s)", kind, e.VersionID, e.Key, e.Latest, e.Size, e.ETag, e.Modified.Format(time.RFC3339Nano))
}

// siteS3 is where a raw S3 request to a site goes, and the key that signs
// it.
type siteS3 struct {
	host                 string
	client               *http.Client
	accessKey, secretKey string
}

// siteEmptySHA256 is the SHA-256 of an empty payload, in hexadecimal.
const siteEmptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// siteListingPages bounds the pages of one listing: far above the ~700
// entries of the test's buckets at 1 000 a page.
const siteListingPages = 50

// siteListing lists every version and delete marker of a bucket in the
// order of the answer's document - per key from the newest, versions and
// markers interleaved as they are ordered - by reading the XML of
// ListObjectVersions itself: the SDK gives versions and markers in two
// lists, which loses their order among each other. A failure to read is
// errSiteUnreachable.
func siteListing(ctx context.Context, site siteS3, bucket, prefix string) ([]siteEntry, error) {
	var entries []siteEntry
	keyMarker, versionMarker := "", ""
	for range siteListingPages {
		query := url.Values{"versions": {""}, "prefix": {prefix}}
		if keyMarker != "" {
			query.Set("key-marker", keyMarker)
			query.Set("version-id-marker", versionMarker)
		}
		page, err := siteListingPage(ctx, site, bucket, query)
		if err != nil {
			return nil, fmt.Errorf("ListObjectVersions %s through %s %w: %w", bucket, site.host, errSiteUnreachable, err)
		}
		entries = append(entries, page.entries...)
		if !page.truncated {
			return entries, nil
		}
		if page.nextKey == "" {
			return nil, fmt.Errorf("ListObjectVersions %s through %s is truncated without a NextKeyMarker", bucket, site.host)
		}
		keyMarker, versionMarker = page.nextKey, page.nextVersion
	}
	return nil, fmt.Errorf("ListObjectVersions %s through %s did not end within %d pages", bucket, site.host, siteListingPages)
}

// siteListed is one page of ListObjectVersions.
type siteListed struct {
	entries              []siteEntry
	truncated            bool
	nextKey, nextVersion string
}

// siteListedEntry is a Version or a DeleteMarker element of the document.
type siteListedEntry struct {
	Key          string    `xml:"Key"`
	VersionID    string    `xml:"VersionId"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
	IsLatest     bool      `xml:"IsLatest"`
	LastModified time.Time `xml:"LastModified"`
}

// siteListingPage sends one signed ListObjectVersions and reads its
// document token by token, in order.
func siteListingPage(ctx context.Context, site siteS3, bucket string, query url.Values) (siteListed, error) {
	attempt, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(attempt, http.MethodGet, "https://"+site.host+":9000/"+bucket+"?"+query.Encode(), nil)
	if err != nil {
		return siteListed{}, err
	}
	req.Header.Set("X-Amz-Content-Sha256", siteEmptySHA256)
	credentials := aws.Credentials{AccessKeyID: site.accessKey, SecretAccessKey: site.secretKey}
	if err := v4.NewSigner().SignHTTP(attempt, credentials, req, siteEmptySHA256, "s3", "us-east-1", time.Now()); err != nil {
		return siteListed{}, err
	}
	resp, err := site.client.Do(req)
	if err != nil {
		return siteListed{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return siteListed{}, fmt.Errorf("%d %s", resp.StatusCode, body)
	}
	var page siteListed
	decoder := xml.NewDecoder(resp.Body)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return page, nil
		}
		if err != nil {
			return siteListed{}, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "Version", "DeleteMarker":
			var e siteListedEntry
			if err = decoder.DecodeElement(&e, &start); err == nil {
				page.entries = append(page.entries, siteEntry{
					Key: e.Key, VersionID: e.VersionID, ETag: e.ETag, Size: e.Size, Latest: e.IsLatest, Modified: e.LastModified,
					Marker: start.Name.Local == "DeleteMarker",
				})
			}
		case "IsTruncated":
			err = decoder.DecodeElement(&page.truncated, &start)
		case "NextKeyMarker":
			err = decoder.DecodeElement(&page.nextKey, &start)
		case "NextVersionIdMarker":
			err = decoder.DecodeElement(&page.nextVersion, &start)
		}
		if err != nil {
			return siteListed{}, err
		}
	}
}

// siteCompare says where the listing of site B differs from site A's: the
// same entries, in the same order, equal in everything listed, apart from
// those of onlyA, which B must not hold, and those of onlyB, which A no
// longer holds and B must.
func siteCompare(atA, atB []siteEntry, onlyA, onlyB map[string]bool) error {
	var a, b []siteEntry
	for _, e := range atA {
		if !onlyA[e.VersionID] {
			a = append(a, e)
		}
	}
	held := map[string]bool{}
	for _, e := range atB {
		if onlyA[e.VersionID] {
			return fmt.Errorf("site B holds %s, which it must not", e)
		}
		if onlyB[e.VersionID] {
			held[e.VersionID] = true
			continue
		}
		b = append(b, e)
	}
	for id := range onlyB {
		if !held[id] {
			return fmt.Errorf("site B no longer holds the version %s", id)
		}
	}
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return fmt.Errorf("%w: entry %d is %s at site A and %s at site B", errSiteNotYet, i, a[i], b[i])
		}
	}
	if len(a) != len(b) {
		return fmt.Errorf("%w: site A lists %d entries and site B %d", errSiteNotYet, len(a), len(b))
	}
	return nil
}

// siteObject is what the test wrote to a version.
type siteObject struct {
	key  string
	data []byte
	tags map[string]string
}

// siteLedger is every version the test wrote to site A, by version ID, and
// every delete marker it made.
type siteLedger struct {
	mu      sync.Mutex
	objects map[string]siteObject
	markers map[string]string
}

func (l *siteLedger) wrote(id string, o siteObject) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.objects[id] = o
}

func (l *siteLedger) marked(id, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.markers[id] = key
}

// retag records the tags a version was given since it was written.
func (l *siteLedger) retag(id string, tags map[string]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o := l.objects[id]
	o.tags = tags
	l.objects[id] = o
}

func (l *siteLedger) versionIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(maps.Keys(l.objects))
}

func (l *siteLedger) object(id string) siteObject {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.objects[id]
}

// siteReplicationStatus says what a HEAD answered of a version's
// replication: its x-amz-replication-status, empty for none, or the error
// it failed with - which is never a status.
func siteReplicationStatus(head *s3.HeadObjectOutput, err error) string {
	if err != nil {
		return "HEAD failed: " + err.Error()
	}
	return string(head.ReplicationStatus)
}

// siteBytes returns n random bytes.
func siteBytes(n int) ([]byte, error) {
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		return nil, err
	}
	return data, nil
}

// sitePut puts a version through a client and returns its ID.
func sitePut(ctx context.Context, client *s3.Client, bucket, key string, data []byte, tags map[string]string) (string, error) {
	in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)}
	if len(tags) > 0 {
		in.Tagging = aws.String(siteTagging(tags))
	}
	out, err := client.PutObject(ctx, in)
	if err != nil {
		return "", fmt.Errorf("PutObject %q: %w", key, err)
	}
	if aws.ToString(out.VersionId) == "" {
		return "", fmt.Errorf("PutObject %q answered no version ID", key)
	}
	return aws.ToString(out.VersionId), nil
}

func siteTagging(tags map[string]string) string {
	values := url.Values{}
	for k, v := range tags {
		values.Set(k, v)
	}
	return values.Encode()
}

// siteTagSet is a set of tags as the SDK sends it.
func siteTagSet(tags map[string]string) []types.Tag {
	set := make([]types.Tag, 0, len(tags))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		set = append(set, types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	return set
}

// siteTags reads the tags of a version.
func siteTags(ctx context.Context, client *s3.Client, bucket, key, id string) (map[string]string, error) {
	out, err := client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(id)})
	if err != nil {
		return nil, fmt.Errorf("GetObjectTagging %q %s: %w", key, id, err)
	}
	tags := map[string]string{}
	for _, tag := range out.TagSet {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return tags, nil
}

// siteRead reads a version, or the current version when id is empty.
func siteRead(ctx context.Context, client *s3.Client, bucket, key, id string) ([]byte, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if id != "" {
		in.VersionId = aws.String(id)
	}
	out, err := client.GetObject(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("GetObject %q %s: %w", key, id, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("GetObject %q %s: %w", key, id, err)
	}
	return data, nil
}

// siteEach runs check for every item, siteWorkers at a time, and returns
// what failed, at most ten failures told.
func siteEach(items []string, check func(string) error) error {
	work := make(chan string)
	var mu sync.Mutex
	var failures []error
	var workers sync.WaitGroup
	for range siteWorkers {
		workers.Go(func() {
			for item := range work {
				if err := check(item); err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
				}
			}
		})
	}
	for _, item := range items {
		work <- item
	}
	close(work)
	workers.Wait()
	if len(failures) > 10 {
		failures = append(failures[:10], fmt.Errorf("and %d failures more", len(failures)-10))
	}
	return errors.Join(failures...)
}

// TestSiteReplication: replication between two sites (ADR-0026, PLAN
// 8.1.8), in the product as it is deployed. Site A is the four-node
// cluster; site B is one standalone node with four drives on the cluster's
// network, reached by A's nodes as https://siteb:9000. Each site has its own
// CA, its own KMS key file and its own root; site A trusts site B's CA only
// as the target's ca_pem, and holds a key of site B that may do nothing but
// the replicate actions and those of the later changes, on the replica
// bucket - a user of B with a role of that policy, and a key whose inline
// policy is the same.
//
// Site B's bucket keeps versions and has Object Lock, and is made the
// replica of site A's deployment ID before anything is in it. Site A's
// bucket keeps versions and has Object Lock too; one version is put into
// it, then the replication configuration (one rule, every key, delete
// markers copied), then 500 versions of 100 keys through the four nodes,
// a tenth of them tagged, 50 delete markers, one object of 12 MiB in three
// parts, one tagged version retained (governance, two hours), and one
// version under a customer's key.
//
// Asserted by listing the versions of both buckets, read from the XML of
// ListObjectVersions in the order of the document: every version and
// delete marker is at site B with the version ID, in the order - versions
// and markers interleaved per key - of the size, ETag, time and
// latest-ness it has at A, but the version under the customer's key, which
// is FAILED at A and absent at B, and the version put before the
// configuration. The tags of every version and the retention of the
// retained one are equal at both sites, and a sample of the others has
// neither a retention nor a legal hold at B; every byte read from B is what
// was written to A. Then the retained version's tags are changed and its
// retention extended at A, and both arrive at B (the later changes). The
// replica key reads, lists and removes nothing, writes into no other
// bucket and is refused by the management API. A client's put into site
// B's bucket is 409 InvalidBucketState.
//
// Then site B is paused for a minute while site A takes a put every two
// seconds: the target shows a failure of class timeout, the last status
// before B resumes counts every one of them pending, and after it
// everything arrives. Then the version put before the
// configuration, still absent at B, is sent by a resync and only then is
// there, and COMPLETED at A. Then a version is removed for good at A, and
// a later version of its key sent after it: site B keeps the removed one.
// At last site B's bucket is promoted, and a put there is the newest
// version of its key.
//
// No leader is killed (that is PLAN 8.1.9); every request to site A is
// sent through the nodes in turn and tried again where the answer is not
// settled. No --egress-allow: site B is in the engine's private range.
//
// Not parallel, one cluster (PLAN, Milestone 8). It takes some 4 minutes
// after the product image is built (210 s on the test host, linux amd64,
// 2026-11-16): under a minute to start both sites, seconds to write and to
// settle, the minute site B is paused, ten seconds of resync, and the
// reads of some 600 versions at both sites.
func TestSiteReplication(t *testing.T) {
	c := clustertest.Start(t, clustertest.Options{})
	b := clustertest.StartStandalone(t, c.Network(), siteB)
	ctx, cancel := context.WithTimeout(t.Context(), 13*time.Minute)
	defer cancel()
	begun := time.Now()
	nodes := clustertest.Nodes
	atA := siteAdmin{name: "A", hosts: nodes, client: c.Admin, auth: gridBearer(c.AccessKey, c.SecretKey)}
	atB := siteAdmin{name: "B", hosts: []string{b.Name}, client: func(string) *http.Client { return b.Admin() }, auth: gridBearer(b.AccessKey, b.SecretKey)}
	clients := make([]*s3.Client, len(nodes))
	for i, id := range nodes {
		clients[i] = c.S3(id)
	}
	rootB := b.S3()
	// The raw listings: of site A through a node, of site B.
	listingA := func(i int) siteS3 {
		id := nodes[i%len(nodes)]
		return siteS3{host: id, client: c.HTTP(id), accessKey: c.AccessKey, secretKey: c.SecretKey}
	}
	listingB := siteS3{host: b.Name, client: b.HTTP(), accessKey: b.AccessKey, secretKey: b.SecretKey}

	// Site A's deployment ID, which site B's bucket is the replica of.
	var deployment string
	c.Eventually("site A to say its deployment ID", siteSetup, func() bool {
		var shown struct {
			DeploymentID string `json:"deployment_id"`
		}
		status, body, err := atA.call(ctx, nodes[0], nil, http.MethodGet, "/cluster", nil)
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &shown) != nil {
			return false
		}
		deployment = shown.DeploymentID
		return deployment != ""
	})

	// Site B: the replica bucket, with Object Lock - the retained version
	// is held otherwise (object_lock) - and versions.
	createLocked := func(client *s3.Client, site, bucket string) {
		t.Helper()
		c.Eventually("site "+site+" to create the bucket "+bucket, siteSetup, func() bool {
			_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket), ObjectLockEnabledForBucket: aws.Bool(true)})
			switch status := statusOf(err); {
			case err == nil, apiCode(err) == "BucketAlreadyOwnedByYou":
				return true
			case status >= 400 && status < 500:
				t.Fatalf("CreateBucket %s at site %s: %v", bucket, site, err)
			}
			return false
		})
		versioning, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
		if err != nil {
			t.Fatalf("the versioning of %s at site %s: %v", bucket, site, err)
		}
		if versioning.Status != types.BucketVersioningStatusEnabled {
			t.Fatalf("the versioning of %s at site %s = %q; want Enabled", bucket, site, versioning.Status)
		}
	}
	createLocked(rootB, "B", siteReplica)
	var mode struct {
		Replica bool   `json:"replica"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(atB.change(ctx, t, http.MethodPut, "/buckets/"+siteReplica+"/replica", map[string]any{"source": deployment}, http.StatusOK), &mode); err != nil ||
		!mode.Replica || mode.Source != deployment {
		t.Fatalf("site B's bucket made the replica of %s: %+v, %v", deployment, mode, err)
	}

	// Site B: the key site A replicates with, and nothing more.
	policy := json.RawMessage(siteReplicatePolicy)
	atB.change(ctx, t, http.MethodPost, "/policies", map[string]any{"name": "replicate-from-a", "document": policy}, http.StatusCreated)
	atB.change(ctx, t, http.MethodPost, "/roles", map[string]any{"name": "replicator", "policies": []string{"replicate-from-a"}}, http.StatusCreated)
	atB.change(ctx, t, http.MethodPost, "/users", map[string]any{"name": "site-a", "roles": []string{"replicator"}}, http.StatusCreated)
	var replicateKey struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	// The answer holds the secret: it is put into no message.
	if err := json.Unmarshal(atB.change(ctx, t, http.MethodPost, "/users/site-a/keys",
		map[string]any{"description": "site A replicates", "inline_policy": policy}, http.StatusCreated), &replicateKey); err != nil ||
		replicateKey.Key.AccessKeyID == "" || replicateKey.SecretKey == "" {
		t.Fatal("site B made a key of site-a without an access key ID or a secret")
	}

	// Site A: its bucket, one version before any configuration, the
	// target and the configuration.
	createLocked(clients[0], "A", siteSource)
	ledger := &siteLedger{objects: map[string]siteObject{}, markers: map[string]string{}}
	write := func(client *s3.Client, name string, size int, tags map[string]string) string {
		t.Helper()
		data, err := siteBytes(size)
		if err != nil {
			t.Fatal(err)
		}
		id, err := sitePut(ctx, client, siteSource, name, data, tags)
		if err != nil {
			t.Fatalf("site A: %v", err)
		}
		ledger.wrote(id, siteObject{key: name, data: data, tags: tags})
		return id
	}
	before := write(clients[1], "before/one", 3000, nil)
	atA.change(ctx, t, http.MethodPut, "/targets/"+siteTarget, map[string]any{
		"kind": "s3", "endpoint": "https://" + b.Name + ":9000", "region": "us-east-1", "ca_pem": string(b.CAPEM),
		"buckets": []string{siteSource}, "auth": map[string]any{"mode": "sigv4", "id": replicateKey.Key.AccessKeyID, "secret": replicateKey.SecretKey},
	}, http.StatusOK)
	atA.change(ctx, t, http.MethodPut, "/buckets/"+siteSource+"/replication", map[string]any{
		"target": siteTarget, "destination_bucket": siteReplica,
		"rules": []map[string]any{{"id": "everything", "enabled": true, "priority": 1, "delete_markers": true}},
	}, http.StatusOK)
	t.Logf("both sites set up in %s", time.Since(begun).Round(time.Second))

	// The burst: 500 versions of 100 keys, a tenth tagged, then a delete
	// marker on every second key; each worker writes its keys in order,
	// each version through the next node.
	written := time.Now()
	var workers sync.WaitGroup
	failures := make(chan error, siteWorkers)
	for w := range siteWorkers {
		workers.Go(func() {
			for k := w; k < siteKeys; k += siteWorkers {
				name := fmt.Sprintf("v/key-%03d", k)
				for r := range siteRounds {
					var tags map[string]string
					if (k+r)%10 == 0 {
						tags = map[string]string{"key": fmt.Sprint(k), "round": fmt.Sprint(r)}
					}
					data, err := siteBytes(512 * (1 + (k+r)%16))
					if err == nil {
						var id string
						if id, err = sitePut(ctx, clients[(k+r)%len(clients)], siteSource, name, data, tags); err == nil {
							ledger.wrote(id, siteObject{key: name, data: data, tags: tags})
						}
					}
					if err != nil {
						failures <- err
						return
					}
				}
				if k%2 != 0 {
					continue
				}
				out, err := clients[k%len(clients)].DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(siteSource), Key: aws.String(name)})
				if err != nil {
					failures <- fmt.Errorf("DeleteObject %q: %w", name, err)
					return
				}
				if !aws.ToBool(out.DeleteMarker) || aws.ToString(out.VersionId) == "" {
					failures <- fmt.Errorf("DeleteObject %q made no delete marker with a version ID", name)
					return
				}
				ledger.marked(aws.ToString(out.VersionId), name)
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("site A, the burst: %v", err)
	}

	// The object of 12 MiB, in three parts.
	big, err := siteBytes(siteBigSize)
	if err != nil {
		t.Fatal(err)
	}
	upload, err := clients[2].CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(siteSource), Key: aws.String("big/twelve")})
	if err != nil {
		t.Fatalf("site A, CreateMultipartUpload: %v", err)
	}
	var parts []types.CompletedPart
	for n, at := int32(1), 0; at < len(big); n, at = n+1, at+sitePartSize {
		sent, err := clients[2].UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(siteSource), Key: aws.String("big/twelve"), UploadId: upload.UploadId, PartNumber: aws.Int32(n),
			Body: bytes.NewReader(big[at:min(at+sitePartSize, len(big))]),
		})
		if err != nil {
			t.Fatalf("site A, UploadPart %d: %v", n, err)
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(n), ETag: sent.ETag, ChecksumCRC32: sent.ChecksumCRC32})
	}
	completed, err := clients[3].CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(siteSource), Key: aws.String("big/twelve"), UploadId: upload.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		t.Fatalf("site A, CompleteMultipartUpload: %v", err)
	}
	if aws.ToString(completed.VersionId) == "" || !strings.HasSuffix(strings.Trim(aws.ToString(completed.ETag), `"`), "-3") {
		t.Fatalf("site A, CompleteMultipartUpload: version %q, ETag %q; want a version of three parts", aws.ToString(completed.VersionId), aws.ToString(completed.ETag))
	}
	bigID := aws.ToString(completed.VersionId)
	ledger.wrote(bigID, siteObject{key: "big/twelve", data: big})

	// The tagged, retained version.
	until := time.Now().Add(siteRetention).UTC().Truncate(time.Second)
	retainedData, err := siteBytes(4096)
	if err != nil {
		t.Fatal(err)
	}
	retainedTags := map[string]string{"hold": "two-hours", "site": "a"}
	retained, err := clients[3].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(siteSource), Key: aws.String("locked/one"), Body: bytes.NewReader(retainedData), Tagging: aws.String(siteTagging(retainedTags)),
		ObjectLockMode: types.ObjectLockModeGovernance, ObjectLockRetainUntilDate: aws.Time(until),
	})
	if err != nil || aws.ToString(retained.VersionId) == "" {
		t.Fatalf("site A, the retained version: %v", err)
	}
	retainedID := aws.ToString(retained.VersionId)
	ledger.wrote(retainedID, siteObject{key: "locked/one", data: retainedData, tags: retainedTags})

	// The version under a customer's key, which is not ours to send.
	secret, err := clients[0].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(siteSource), Key: aws.String("secret/one"), Body: strings.NewReader("a customer's own"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(siteCustomerKey), SSECustomerKeyMD5: aws.String(siteCustomerKeyMD5),
	})
	if err != nil || aws.ToString(secret.VersionId) == "" {
		t.Fatalf("site A, the version under a customer's key: %v", err)
	}
	secretID := aws.ToString(secret.VersionId)
	t.Logf("site A took 500 versions, 50 delete markers, the multipart, the retained and the SSE-C version in %s", time.Since(written).Round(time.Second))

	listA, err := siteListing(ctx, listingA(0), siteSource, "v/")
	if err != nil {
		t.Fatal(err)
	}
	var versions, markers int
	for _, e := range listA {
		if e.Marker {
			markers++
		} else {
			versions++
		}
	}
	if versions != siteKeys*siteRounds || markers != siteKeys/2 {
		t.Fatalf("site A lists %d versions and %d delete markers under v/; want %d and %d", versions, markers, siteKeys*siteRounds, siteKeys/2)
	}

	// settled waits until site A says nothing of the bucket is pending and
	// failed is the version under the customer's key, and site B lists
	// what A lists but onlyA, and onlyB besides.
	settled := func(what string, limit time.Duration, onlyA, onlyB map[string]bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		started := time.Now()
		for asked := 0; ; asked++ {
			status, err := atA.status(ctx, nodes[asked%len(nodes)])
			switch {
			case err != nil:
				err = fmt.Errorf("the status %w: %w", errSiteUnreachable, err)
			case status.Failed > 1:
				t.Fatalf("%s: %d versions are FAILED; only the one under a customer's key may be: %+v", what, status.Failed, status)
			case status.Pending != 0 || status.Failed != 1:
				err = fmt.Errorf("%w: %d pending and %d failed, the target %s (%s)", errSiteNotYet, status.Pending, status.Failed, status.TargetState, status.LastErrorClass)
			default:
				var listing, atB []siteEntry
				if listing, err = siteListing(ctx, listingA(asked), siteSource, ""); err == nil {
					if atB, err = siteListing(ctx, listingB, siteReplica, ""); err == nil {
						err = siteCompare(listing, atB, onlyA, onlyB)
					}
				}
			}
			switch {
			case err == nil:
				t.Logf("%s: settled in %s", what, time.Since(started).Round(time.Second))
				return
			case !errors.Is(err, errSiteNotYet) && !errors.Is(err, errSiteUnreachable):
				// What can never become right: B holds what it must not,
				// or no longer holds what it must.
				t.Fatalf("%s: %v", what, err)
			case time.Now().After(deadline):
				t.Fatalf("%s within %s: %v", what, limit, err)
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%s: %v", what, ctx.Err())
			case <-time.After(2 * time.Second):
			}
		}
	}
	settled("the burst to arrive at site B", siteArrival, map[string]bool{secretID: true, before: true}, nil)

	// FAILED at A, absent at B: the version under a customer's key.
	head, err := clients[1].HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(siteSource), Key: aws.String("secret/one"), VersionId: aws.String(secretID),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(siteCustomerKey), SSECustomerKeyMD5: aws.String(siteCustomerKeyMD5),
	})
	if got := siteReplicationStatus(head, err); got != string(types.ReplicationStatusFailed) {
		t.Errorf("the version under a customer's key at site A: replication status %q; want FAILED", got)
	}
	if _, err := rootB.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(siteReplica), Key: aws.String("secret/one"), VersionId: aws.String(secretID)}); statusOf(err) != http.StatusNotFound {
		t.Errorf("the version under a customer's key at site B: %v; want 404", err)
	}
	// What each side says of a copy.
	for site, client := range map[string]*s3.Client{"A": clients[2], "B": rootB} {
		bucket, want := siteSource, types.ReplicationStatusCompleted
		if site == "B" {
			bucket, want = siteReplica, types.ReplicationStatusReplica
		}
		head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("big/twelve"), VersionId: aws.String(bigID)})
		if got := siteReplicationStatus(head, err); got != string(want) || aws.ToInt64(head.ContentLength) != siteBigSize {
			t.Errorf("the multipart object at site %s: replication status %q; want %s, of %d bytes", site, got, want, siteBigSize)
		}
	}
	// The later changes: the retained version's tags replaced and its
	// retention extended at A, and both carried to B (ADR-0026, "Later
	// changes, as built").
	retainedTags = map[string]string{"hold": "three-hours", "site": "a", "changed": "later"}
	if _, err := clients[1].PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String(siteSource), Key: aws.String("locked/one"), VersionId: aws.String(retainedID),
		Tagging: &types.Tagging{TagSet: siteTagSet(retainedTags)},
	}); err != nil {
		t.Fatalf("site A, a later change of the tags: %v", err)
	}
	until = until.Add(time.Hour)
	if _, err := clients[2].PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket: aws.String(siteSource), Key: aws.String("locked/one"), VersionId: aws.String(retainedID),
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(until)},
	}); err != nil {
		t.Fatalf("site A, a later change extending the retention: %v", err)
	}
	ledger.retag(retainedID, retainedTags)
	changed := time.Now()
	c.Eventually("the later changes of the retained version to arrive at site B", siteArrival, func() bool {
		tags, err := siteTags(ctx, rootB, siteReplica, "locked/one", retainedID)
		if err != nil || !maps.Equal(tags, retainedTags) {
			return false
		}
		out, err := rootB.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(siteReplica), Key: aws.String("locked/one"), VersionId: aws.String(retainedID)})
		return err == nil && out.Retention != nil && aws.ToTime(out.Retention.RetainUntilDate).Equal(until)
	})
	t.Logf("the later changes arrived at site B in %s", time.Since(changed).Round(time.Second))
	// A change refused there would make the version FAILED: every later
	// wait requires failed to stay 1, and fails at once above it.

	// The retention of the retained version, equal at both sides.
	for site, client := range map[string]*s3.Client{"A": clients[0], "B": rootB} {
		bucket := siteSource
		if site == "B" {
			bucket = siteReplica
		}
		out, err := client.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String("locked/one"), VersionId: aws.String(retainedID)})
		if err != nil || out.Retention == nil || out.Retention.Mode != types.ObjectLockRetentionModeGovernance || !aws.ToTime(out.Retention.RetainUntilDate).Equal(until) {
			t.Errorf("the retention of the retained version at site %s: %+v, %v; want governance until %s", site, out, err, until.Format(time.RFC3339))
		}
	}
	// The tags of every version at both sites, and every byte read from B.
	// checkAt holds a version at the sites named to what was written to
	// site A: its tags at each, and its bytes as site B gives them.
	checkAt := func(id string, sites ...string) error {
		o := ledger.object(id)
		got, err := siteRead(ctx, rootB, siteReplica, o.key, id)
		if err != nil {
			return fmt.Errorf("site B: %w", err)
		}
		if !bytes.Equal(got, o.data) {
			return fmt.Errorf("site B gives %d bytes for the version %s of %q, not the %d written to site A", len(got), id, o.key, len(o.data))
		}
		want := o.tags
		if want == nil {
			want = map[string]string{}
		}
		for _, site := range sites {
			client, bucket := clients[int(id[len(id)-1])%len(clients)], siteSource
			if site == "B" {
				client, bucket = rootB, siteReplica
			}
			tags, err := siteTags(ctx, client, bucket, o.key, id)
			if err != nil {
				return fmt.Errorf("site %s: %w", site, err)
			}
			if !maps.Equal(tags, want) {
				return fmt.Errorf("the version %s of %q has the tags %v at site %s; written %v", id, o.key, tags, site, want)
			}
		}
		return nil
	}
	check := func(id string) error { return checkAt(id, "A", "B") }
	ids := slices.DeleteFunc(ledger.versionIDs(), func(id string) bool { return id == before })
	if err := siteEach(ids, check); err != nil {
		t.Fatalf("the versions at site B: %v", err)
	}
	t.Logf("%d versions read back from site B, byte for byte, their tags as at site A", len(ids))
	// The others carry no lock to B: a sample of them has neither a
	// retention nor a legal hold there.
	sample := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == retainedID })[:20]
	if err := siteEach(sample, func(id string) error {
		o := ledger.object(id)
		_, err := rootB.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(siteReplica), Key: aws.String(o.key), VersionId: aws.String(id)})
		switch {
		case err == nil:
			return fmt.Errorf("the version %s of %q has a retention at site B", id, o.key)
		case apiCode(err) != "NoSuchObjectLockConfiguration":
			return fmt.Errorf("the retention of %s of %q at site B, want NoSuchObjectLockConfiguration: %w", id, o.key, err)
		}
		hold, err := rootB.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{Bucket: aws.String(siteReplica), Key: aws.String(o.key), VersionId: aws.String(id)})
		if err != nil {
			return fmt.Errorf("the legal hold of %s of %q at site B: %w", id, o.key, err)
		}
		if hold.LegalHold == nil || hold.LegalHold.Status != types.ObjectLockLegalHoldStatusOff {
			return fmt.Errorf("the legal hold of %s of %q at site B is %+v; want OFF", id, o.key, hold.LegalHold)
		}
		return nil
	}); err != nil {
		t.Errorf("versions without a lock at site B: %v", err)
	}
	// The key site A holds of site B reads, lists and removes nothing,
	// writes into no other bucket, and is refused by the management API.
	replicator := b.S3As(replicateKey.Key.AccessKeyID, replicateKey.SecretKey)
	denied := func(what string, err error) {
		t.Helper()
		if statusOf(err) != http.StatusForbidden || apiCode(err) != "AccessDenied" {
			t.Errorf("site A's key %s: %v; want 403 AccessDenied", what, err)
		}
	}
	_, err = siteRead(ctx, replicator, siteReplica, "big/twelve", bigID)
	denied("reads a version at site B", err)
	_, err = replicator.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(siteReplica)})
	denied("lists site B's bucket", err)
	_, err = replicator.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(siteReplica), Key: aws.String("big/twelve"), VersionId: aws.String(bigID)})
	denied("removes a version at site B", err)
	if _, err := rootB.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("plain")}); err != nil {
		t.Fatalf("site B, a bucket that is no replica: %v", err)
	}
	_, err = sitePut(ctx, replicator, "plain", "from/a", []byte("not a copy"), nil)
	denied("puts into a bucket of site B that is no replica", err)
	keyAdmin := siteAdmin{name: "B", hosts: []string{b.Name}, client: atB.client, auth: gridBearer(replicateKey.Key.AccessKeyID, replicateKey.SecretKey)}
	if status, body, err := keyAdmin.call(ctx, b.Name, nil, http.MethodGet, "/users", nil); err != nil || status != http.StatusForbidden || problemCode(body) != "AccessDenied" {
		t.Errorf("site A's key as a bearer of site B's management API, GET /users: %d %s, %v; want 403 AccessDenied", status, siteShown(body), err)
	}

	// A client's put into the replica bucket.
	if _, err := sitePut(ctx, rootB, siteReplica, "client/put", []byte("a client's"), nil); statusOf(err) != http.StatusConflict || apiCode(err) != "InvalidBucketState" {
		t.Errorf("a client's put into site B's replica bucket: %v; want 409 InvalidBucketState", err)
	}

	// Site B paused for a minute while site A takes writes: none can
	// arrive, so they are pending; then everything arrives.
	b.Pause()
	paused := time.Now()
	var timedOut bool
	var states, whilePaused []string
	for i := 0; time.Since(paused) < sitePause; i++ {
		whilePaused = append(whilePaused, write(clients[i%len(clients)], fmt.Sprintf("paused/key-%02d", i), 1000+i, nil))
		if status, err := atA.status(ctx, nodes[i%len(nodes)]); err != nil {
			states = append(states, err.Error())
		} else {
			timedOut = timedOut || (status.TargetState != "ok" && status.LastErrorClass == "timeout")
			states = append(states, status.TargetState+"/"+status.LastErrorClass)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(sitePausePace):
		}
	}
	// The last status before B resumes: nothing written meanwhile can have
	// been taken by a frozen site.
	var last siteStatus
	c.Eventually("the status of site A's replication while site B is paused", siteSetup, func() bool {
		var err error
		last, err = atA.status(ctx, nodes[0])
		return err == nil
	})
	b.Unpause()
	if !timedOut {
		t.Errorf("with site B paused for %s, its target never showed a failure of class timeout: %v", sitePause, states)
	}
	if last.Pending < int64(len(whilePaused))-1 {
		t.Errorf("with site B paused, the last status says %d versions pending of the %d written meanwhile", last.Pending, len(whilePaused))
	}
	t.Logf("with site B paused, %d versions written, %d pending at the end; the target was %v", len(whilePaused), last.Pending, slices.Compact(states))
	settled("what was written while site B was paused to arrive", siteArrival, map[string]bool{secretID: true, before: true}, nil)
	if err := siteEach(whilePaused, check); err != nil {
		t.Fatalf("the versions written while site B was paused, at site B: %v", err)
	}

	// The version put before the configuration: still not at B, and
	// there only after a resync.
	if _, err := rootB.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(siteReplica), Key: aws.String("before/one"), VersionId: aws.String(before)}); statusOf(err) != http.StatusNotFound {
		t.Fatalf("the version put before the configuration, before a resync, at site B: %v; want 404", err)
	}
	if got := siteReplicationStatus(clients[2].HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(siteSource), Key: aws.String("before/one"), VersionId: aws.String(before),
	})); got != "" {
		t.Errorf("the version put before the configuration, before a resync, at site A: replication status %q; want none", got)
	}
	atA.change(ctx, t, http.MethodPost, "/buckets/"+siteSource+"/replication/resync", nil, http.StatusAccepted)
	resynced := time.Now()
	c.Eventually("the resync to be done, and the version put before the configuration at site B and COMPLETED at site A", siteResyncLimit, func() bool {
		status, err := atA.status(ctx, nodes[0])
		if err != nil || status.Resync == nil || status.Resync.State != "done" {
			return false
		}
		atB, err := rootB.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(siteReplica), Key: aws.String("before/one"), VersionId: aws.String(before)})
		if err != nil {
			return false
		}
		head, err := clients[1].HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(siteSource), Key: aws.String("before/one"), VersionId: aws.String(before)})
		return err == nil && head.ReplicationStatus == types.ReplicationStatusCompleted && aws.ToString(atB.ETag) == aws.ToString(head.ETag)
	})
	t.Logf("the resync was done in %s", time.Since(resynced).Round(time.Second))
	settled("both sites to list the same after the resync", siteArrival, map[string]bool{secretID: true}, nil)
	if err := check(before); err != nil {
		t.Errorf("the version put before the configuration: %v", err)
	}

	// A version removed for good at A stays at B: a later version of its
	// key, sent after the removal in the order of the outbox, is there
	// when the removal would have been.
	gone := ""
	for _, e := range listA {
		if e.Key == "v/key-001" && !e.Marker {
			gone = e.VersionID // the last listed is the oldest
		}
	}
	if _, err := clients[3].DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(siteSource), Key: aws.String("v/key-001"), VersionId: aws.String(gone)}); err != nil {
		t.Fatalf("site A, removing the version %s for good: %v", gone, err)
	}
	write(clients[0], "v/key-001", 2048, nil)
	settled("a version written after a removal for good to arrive", siteArrival, map[string]bool{secretID: true}, map[string]bool{gone: true})
	if err := checkAt(gone, "B"); err != nil {
		t.Errorf("the version removed for good at site A, at site B: %v", err)
	}

	// Site B is promoted, and a put there is the newest version of its key.
	var promotedMode struct {
		Replica bool   `json:"replica"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(atB.change(ctx, t, http.MethodPut, "/buckets/"+siteReplica+"/replica", map[string]any{"source": ""}, http.StatusOK), &promotedMode); err != nil ||
		promotedMode.Replica || promotedMode.Source != "" {
		t.Fatalf("site B's bucket promoted: %+v, %v", promotedMode, err)
	}
	newest, err := siteBytes(777)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := sitePut(ctx, rootB, siteReplica, "v/key-001", newest, nil)
	if err != nil {
		t.Fatalf("site B, a put after the promotion: %v", err)
	}
	after, err := siteListing(ctx, listingB, siteReplica, "v/key-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) == 0 || after[0].VersionID != promoted || !after[0].Latest {
		t.Fatalf("site B lists %v for v/key-001 after the promotion; want the put %s first and latest", after, promoted)
	}
	if got, err := siteRead(ctx, rootB, siteReplica, "v/key-001", ""); err != nil || !bytes.Equal(got, newest) {
		t.Fatalf("site B gives %d bytes for v/key-001 after the promotion (%v); want the %d put there", len(got), err, len(newest))
	}
	t.Logf("two sites: %d versions written to site A, all at site B, in %s", len(ledger.versionIDs()), time.Since(begun).Round(time.Second))
}
