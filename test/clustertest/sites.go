//go:build integration || chaos

package clustertest

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// What the tests of two sites share (PLAN 8.1.8, 8.1.9): the management API
// of a site asked through its hosts in turn, the status of a bucket's
// replication, and the listing of a bucket's versions read from the XML in
// the order of the document.

// ErrNotYet says that what a site shows is not yet what it is waited for,
// and ErrUnreachable that it could not be read this time: both are asked
// again until a deadline. Any other error of a judgement is final.
var (
	ErrNotYet      = errors.New("not yet")
	ErrUnreachable = errors.New("could not be read")
)

// defaultChangeWithin bounds the attempts of SiteAdmin.Change when the site
// names no bound of its own.
const defaultChangeWithin = 2 * time.Minute

// Shown is a body of an answer as a message may show it: never one that
// holds a secret key (the answer that creates an API key).
func Shown(body []byte) string {
	if bytes.Contains(body, []byte("secret_key")) {
		return "(a body that holds a secret key, not shown)"
	}
	return string(body)
}

// problemCode returns the code of a problem+json answer, or "".
func problemCode(body []byte) string {
	var problem struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &problem) != nil {
		return ""
	}
	return problem.Code
}

// SiteAdmin is the management API of one site: the hosts that take its
// requests, tried in turn, the client of each, and who asks.
type SiteAdmin struct {
	// Name names the site in a message: "A", "B".
	Name string
	// Hosts are the names the site's admin listeners are reached under, at
	// https://<host>:9001.
	Hosts []string
	// Client returns the HTTP client of a host.
	Client func(host string) *http.Client
	// Auth is added to the headers of every request: the bearer of a key.
	Auth map[string]string
	// Within bounds the attempts of one Change; two minutes when zero.
	Within time.Duration
}

// Call sends one request to one host. A PUT and a POST say they are JSON,
// with a body or without one.
func (a SiteAdmin) Call(ctx context.Context, host string, headers map[string]string, method, path string, body any) (int, []byte, error) {
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
	for name, value := range a.Auth {
		req.Header.Set(name, value)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := a.Client(host).Do(req)
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

// Change sends an administrative change through the hosts in turn until
// one answers want, and returns the answer's body. The change has one
// Idempotency-Key, drawn once and sent with every attempt: an attempt that
// was taken and whose answer was lost is answered as it was when it is
// sent again, so a creation - an API key above all - is never made twice.
// A failure of the connection, a 5xx, a 429 (a request whose body may hold
// a secret, recognised by a slow hash) and a 409 that says the state or the
// key is not settled yet (Conflict: a usage record that is not exact;
// RequestInProgress: the key is reserved by an attempt still in progress)
// are tried again within Within; EntityAlreadyExists after an attempt is
// the change made by that attempt; any other answer fails the test. No
// message shows a body that holds a secret key.
func (a SiteAdmin) Change(ctx context.Context, t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	within := a.Within
	if within == 0 {
		within = defaultChangeWithin
	}
	deadline := time.Now().Add(within)
	idempotency := map[string]string{"Idempotency-Key": "site-" + randomHex(t, 16)}
	var last string
	for try := 0; ; try++ {
		host := a.Hosts[try%len(a.Hosts)]
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		status, answer, err := a.Call(attempt, host, idempotency, method, path, body)
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
			last = fmt.Sprintf("%d %s", status, Shown(answer))
		default:
			t.Fatalf("%s %s at site %s through %s = %d %s; want %d", method, path, a.Name, host, status, Shown(answer), want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s at site %s not answered %d within %s: %s", method, path, a.Name, want, within, last)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s %s at site %s: %v", method, path, a.Name, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ReplicationStatus is what GET /buckets/{bucket}/replication/status says.
type ReplicationStatus struct {
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

// Status asks a host for the status of a bucket's replication.
func (a SiteAdmin) Status(ctx context.Context, host, bucket string) (ReplicationStatus, error) {
	attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, body, err := a.Call(attempt, host, nil, http.MethodGet, "/buckets/"+bucket+"/replication/status", nil)
	if err != nil {
		return ReplicationStatus{}, err
	}
	if code != http.StatusOK {
		return ReplicationStatus{}, fmt.Errorf("the replication status through %s = %d %s", host, code, body)
	}
	var shown ReplicationStatus
	if err := json.Unmarshal(body, &shown); err != nil {
		return ReplicationStatus{}, fmt.Errorf("the replication status through %s: %w: %s", host, err, body)
	}
	return shown, nil
}

// Listed is a version or a delete marker as ListObjectVersions lists it.
type Listed struct {
	Key, VersionID, ETag string
	Size                 int64
	Latest, Marker       bool
	Modified             time.Time
}

func (e Listed) String() string {
	kind := "version"
	if e.Marker {
		kind = "marker"
	}
	return fmt.Sprintf("%s %s of %q (latest %t, %d bytes, ETag %s, %s)", kind, e.VersionID, e.Key, e.Latest, e.Size, e.ETag, e.Modified.Format(time.RFC3339Nano))
}

// SignedS3 is where a raw S3 request to a site goes, and the key that signs
// it.
type SignedS3 struct {
	Host                 string
	Client               *http.Client
	AccessKey, SecretKey string
}

// emptySHA256 is the SHA-256 of an empty payload, in hexadecimal.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// listingPages bounds the pages of one listing: far above the ~700 entries
// of the tests' buckets at 1 000 a page.
const listingPages = 50

// ListVersions lists every version and delete marker of a bucket in the
// order of the answer's document - per key from the newest, versions and
// markers interleaved as they are ordered - by reading the XML of
// ListObjectVersions itself: the SDK gives versions and markers in two
// lists, which loses their order among each other. A failure to read is
// ErrUnreachable.
func ListVersions(ctx context.Context, site SignedS3, bucket, prefix string) ([]Listed, error) {
	var entries []Listed
	keyMarker, versionMarker := "", ""
	for range listingPages {
		query := url.Values{"versions": {""}, "prefix": {prefix}}
		if keyMarker != "" {
			query.Set("key-marker", keyMarker)
			query.Set("version-id-marker", versionMarker)
		}
		page, err := listingPage(ctx, site, bucket, query)
		if err != nil {
			return nil, fmt.Errorf("ListObjectVersions %s through %s %w: %w", bucket, site.Host, ErrUnreachable, err)
		}
		entries = append(entries, page.entries...)
		if !page.truncated {
			return entries, nil
		}
		if page.nextKey == "" {
			return nil, fmt.Errorf("ListObjectVersions %s through %s is truncated without a NextKeyMarker", bucket, site.Host)
		}
		keyMarker, versionMarker = page.nextKey, page.nextVersion
	}
	return nil, fmt.Errorf("ListObjectVersions %s through %s did not end within %d pages", bucket, site.Host, listingPages)
}

// listed is one page of ListObjectVersions.
type listed struct {
	entries              []Listed
	truncated            bool
	nextKey, nextVersion string
}

// listedEntry is a Version or a DeleteMarker element of the document.
type listedEntry struct {
	Key          string    `xml:"Key"`
	VersionID    string    `xml:"VersionId"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
	IsLatest     bool      `xml:"IsLatest"`
	LastModified time.Time `xml:"LastModified"`
}

// listingPage sends one signed ListObjectVersions and reads its document
// token by token, in order.
func listingPage(ctx context.Context, site SignedS3, bucket string, query url.Values) (listed, error) {
	attempt, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(attempt, http.MethodGet, "https://"+site.Host+":9000/"+bucket+"?"+query.Encode(), nil)
	if err != nil {
		return listed{}, err
	}
	req.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	credentials := aws.Credentials{AccessKeyID: site.AccessKey, SecretAccessKey: site.SecretKey}
	if err := v4.NewSigner().SignHTTP(attempt, credentials, req, emptySHA256, "s3", "us-east-1", time.Now()); err != nil {
		return listed{}, err
	}
	resp, err := site.Client.Do(req)
	if err != nil {
		return listed{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return listed{}, fmt.Errorf("%d %s", resp.StatusCode, body)
	}
	var page listed
	decoder := xml.NewDecoder(resp.Body)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return page, nil
		}
		if err != nil {
			return listed{}, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "Version", "DeleteMarker":
			var e listedEntry
			if err = decoder.DecodeElement(&e, &start); err == nil {
				page.entries = append(page.entries, Listed{
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
			return listed{}, err
		}
	}
}

// CompareListings says where the listing of site B differs from site A's:
// the same entries, in the same order, equal in everything listed, apart
// from those of onlyA, which B must not hold, and those of onlyB, which A no
// longer holds and B must. A difference that more arrivals can mend wraps
// ErrNotYet; one they cannot is final.
func CompareListings(atA, atB []Listed, onlyA, onlyB map[string]bool) error {
	var a, b []Listed
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
			return fmt.Errorf("%w: entry %d is %s at site A and %s at site B", ErrNotYet, i, a[i], b[i])
		}
	}
	if len(a) != len(b) {
		return fmt.Errorf("%w: site A lists %d entries and site B %d", ErrNotYet, len(a), len(b))
	}
	return nil
}

// Each runs check for every item, workers at a time, and returns what
// failed, at most ten failures told.
func Each(items []string, workers int, check func(string) error) error {
	work := make(chan string)
	var mu sync.Mutex
	var failures []error
	var running sync.WaitGroup
	for range workers {
		running.Go(func() {
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
	running.Wait()
	if len(failures) > 10 {
		failures = append(failures[:10], fmt.Errorf("and %d failures more", len(failures)-10))
	}
	return errors.Join(failures...)
}
