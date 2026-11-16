//go:build chaos && !windows

package chaos_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/albievan/cactusfs/test/clustertest"
)

const (
	// replSiteB is the name of the standalone node on the cluster's
	// network: the host of the target's endpoint and the name in its
	// certificate. replSource is the bucket of site A that is replicated,
	// replReplica its replica at site B, and replTarget site A's target
	// that names site B.
	replSiteB   = "siteb"
	replSource  = "source"
	replReplica = "replica"
	replTarget  = "siteb"
	// replVersions puts are acknowledged by site A - each a logical put,
	// tried again through the next node until one acknowledges it - of
	// replKeys keys in turn, by replWriters writers that each wait
	// replPace after every acknowledgement: some three a second.
	replVersions = 300
	replKeys     = 60
	replWriters  = 3
	replPace     = time.Second
	// replResends bounds the copies site A sent - each a PutObject that
	// passed authentication at site B - beyond one a version, over all of
	// them together (PLAN 8.1.9: "none was sent more than 100 times over in
	// total").
	replResends = 100
	// replLead is how many puts a writer goes on past the moment an event
	// is due before it waits for the event to have begun: every event
	// begins while puts are being made, whatever the one before it took.
	// It is more than replWriters, so the puts the event waits for are
	// never held back by that wait.
	replLead = 20
	// replCut is how long site B stays cut off at the least, as long as
	// TestSiteReplication pauses it: a request in flight on a connection
	// the target already held stalls until the egress client's response
	// timeout (30 s) ends it, and only then does the target show a failure;
	// the attempts after it are handshakes that are never completed (5 s
	// each). After a minute the target has failed some four times, and its
	// next pause is Backoff(4): 8 s less up to a quarter for jitter, 6 to
	// 8 s.
	replCut = time.Minute
	// replSetup bounds each step of the setting up, replWriteLimit one
	// logical put through every node in turn, replLeaderLimit the election
	// after a kill, and replWithin the whole test after the image is built.
	replSetup       = 2 * time.Minute
	replWriteLimit  = 2 * time.Minute
	replLeaderLimit = 2 * time.Minute
	replWithin      = 14 * time.Minute
	// replSettle bounds how long, after the last event and the last put,
	// both sites take to agree: what is left of a back-off of the target
	// that held across the cut (Backoff(4), at most 8 s), a new leader
	// after the second kill taking up the targets, and the sending, which
	// takes seconds. Five minutes is many times that.
	replSettle = 5 * time.Minute
	// replAuditLimit bounds how long site B's audit trail may take to hold
	// a record of every copy it holds.
	replAuditLimit = time.Minute
)

// replPolicy is everything the key of site A at site B holds: the replicate
// actions, on the replica bucket and its objects.
const replPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
	`"Action":["s3:ReplicateObject","s3:ReplicateDelete"],` +
	`"Resource":["arn:aws:s3:::` + replReplica + `","arn:aws:s3:::` + replReplica + `/*"]}]}`

// replEvent is a fault the test makes while site A takes puts: it begins
// once at puts are acknowledged, and puts beyond at+replLead wait until it
// has begun.
type replEvent struct {
	name  string
	at    int64
	begun chan struct{}
}

// replAcked is a version site A acknowledged: its key and entity tag.
type replAcked struct {
	key, etag string
}

// replWriter makes the logical puts of the test, counts them and keeps
// what each acknowledgement said.
type replWriter struct {
	clients []*s3.Client
	events  []*replEvent
	next    atomic.Int64
	acked   atomic.Int64
	retried atomic.Int64

	mu       sync.Mutex
	versions map[string]replAcked
}

// acknowledged returns a copy of the versions acknowledged so far, by
// version ID.
func (w *replWriter) acknowledged() map[string]replAcked {
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.versions)
}

// run makes puts until replVersions are drawn, the first error, or ctx
// ends; it returns the error.
func (w *replWriter) run(ctx context.Context) error {
	for {
		i := w.next.Add(1) - 1
		if i >= replVersions {
			return nil
		}
		for _, e := range w.events {
			if i < e.at+replLead {
				continue
			}
			select {
			case <-e.begun:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		data := make([]byte, 1024+int(i%8)*512)
		if _, err := rand.Read(data); err != nil {
			return err
		}
		key := fmt.Sprintf("c/key-%02d", i%replKeys)
		id, etag, tries, err := w.put(ctx, int(i), key, data)
		if err != nil {
			return err
		}
		w.mu.Lock()
		w.versions[id] = replAcked{key: key, etag: etag}
		w.mu.Unlock()
		w.acked.Add(1)
		w.retried.Add(int64(tries))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(replPace):
		}
	}
}

// put is one logical put: through the node of its number, then the next
// and the next, until one acknowledges it, within replWriteLimit. A failure
// of the connection, a 5xx, a 429 and a 409 OperationAborted (a conflict of
// the moment, "try again") are tried again - a put that was taken and whose
// answer was lost with its node makes a version more, which the test counts
// from site A's listing - any other answer is final. It returns the version
// ID and entity tag acknowledged, and how many attempts were tried again.
func (w *replWriter) put(ctx context.Context, i int, key string, data []byte) (string, string, int, error) {
	deadline := time.Now().Add(replWriteLimit)
	for try := 0; ; try++ {
		through := clustertest.Nodes[(i+try)%len(clustertest.Nodes)]
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, err := w.clients[(i+try)%len(w.clients)].PutObject(attempt, &s3.PutObjectInput{
			Bucket: aws.String(replSource), Key: aws.String(key), Body: bytes.NewReader(data),
		})
		cancel()
		status := httpStatus(err)
		var api smithy.APIError
		aborted := status == http.StatusConflict && errors.As(err, &api) && api.ErrorCode() == "OperationAborted"
		switch {
		case err == nil && aws.ToString(out.VersionId) != "" && aws.ToString(out.ETag) != "":
			return aws.ToString(out.VersionId), aws.ToString(out.ETag), try, nil
		case err == nil:
			return "", "", try, fmt.Errorf("PutObject %q through %s answered no version ID or no ETag", key, through)
		case ctx.Err() != nil:
			return "", "", try, ctx.Err()
		case status != 0 && status < http.StatusInternalServerError && status != http.StatusTooManyRequests && !aborted:
			return "", "", try, fmt.Errorf("PutObject %q through %s: %w", key, through, err)
		case time.Now().After(deadline):
			return "", "", try, fmt.Errorf("PutObject %q not acknowledged within %s, last through %s: %w", key, replWriteLimit, through, err)
		}
		select {
		case <-ctx.Done():
			return "", "", try, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// replWithinDeadline is limit, or what is left until deadline when that is
// less: a wait of the harness that no context bounds ends with the test.
func replWithinDeadline(limit time.Duration, deadline time.Time) time.Duration {
	return max(min(limit, time.Until(deadline)), 0)
}

// replLeader asks the nodes but not, in turn, who leads site A's metadata
// group, until one names a leader that is not not (any leader when not is
// empty) - and, when confirm is set, that leader names itself when it is
// asked, so that the node about to be killed is the leader by its own
// account; a disagreement asks again. Each question has a deadline of its
// own, as Kill and Restart, which it is asked around, do; the whole is
// bounded by replLeaderLimit and the test's deadline.
func replLeader(c *clustertest.Cluster, not string, confirm bool, deadline time.Time) string {
	what := "a leader of site A"
	if not != "" {
		what += " other than " + not
	}
	if confirm {
		what += ", confirmed by itself"
	}
	ask := func(id string) (string, error) {
		attempt, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return leaderOf(attempt, c, id)
	}
	var leader string
	c.Eventually(what, replWithinDeadline(replLeaderLimit, deadline), func() bool {
		for _, id := range clustertest.Nodes {
			if id == not {
				continue
			}
			now, err := ask(id)
			if err != nil || now == not {
				continue
			}
			if confirm {
				if self, err := ask(now); err != nil || self != now {
					continue
				}
			}
			leader = now
			return true
		}
		return false
	})
	return leader
}

// replRestart starts a killed node again and waits until it is ready, as
// the harness's Restart does, but no longer than the test's deadline.
func replRestart(t *testing.T, c *clustertest.Cluster, id string, deadline time.Time) {
	t.Helper()
	c.StartNode(id)
	for !c.Ready(id) {
		if time.Now().After(deadline) {
			t.Fatalf("node %s did not become ready again before the test's deadline", id)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// replRecord is a record of the audit trail, as GET /audit gives it.
type replRecord struct {
	Sequence uint64            `json:"sequence"`
	Action   string            `json:"action"`
	Outcome  string            `json:"outcome"`
	Details  map[string]string `json:"details"`
}

// replTrail reads the whole audit trail of a site, a page after another.
func replTrail(ctx context.Context, site clustertest.SiteAdmin) ([]replRecord, error) {
	var all []replRecord
	after := uint64(0)
	for {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		status, body, err := site.Call(attempt, site.Hosts[0], nil, http.MethodGet, "/audit?limit=1000&after="+strconv.FormatUint(after, 10), nil)
		cancel()
		switch {
		case err != nil:
			return nil, fmt.Errorf("GET /audit at site %s %w: %w", site.Name, clustertest.ErrUnreachable, err)
		case status != http.StatusOK:
			return nil, fmt.Errorf("GET /audit at site %s %w: %d %s", site.Name, clustertest.ErrUnreachable, status, body)
		}
		var page struct {
			Records   []replRecord `json:"records"`
			NextAfter *uint64      `json:"next_after"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("GET /audit at site %s: %w", site.Name, err)
		}
		all = append(all, page.Records...)
		if page.NextAfter == nil || len(page.Records) == 0 {
			return all, nil
		}
		after = *page.NextAfter
	}
}

// replCopies counts, by version ID, the copies of site A's versions site B
// committed: the records of its audit trail of a replica put
// (s3:ReplicateObject, operation PutObject, succeeded) from site A's
// deployment - one for every copy committed (PLAN 8.1.1, Decisions (7)). A
// copy that arrives a second time is refused (ErrConflict, which its source
// reads as "it is there") and commits nothing, so it has no record
// (ADR-0026, "Order: by the source's sequence").
func replCopies(records []replRecord, deployment string) (map[string]int, int) {
	copies, total := map[string]int{}, 0
	for _, r := range records {
		if r.Action == "s3:ReplicateObject" && r.Outcome == "success" && r.Details["operation"] == "PutObject" && r.Details["source"] == deployment {
			copies[r.Details["version"]]++
			total++
		}
	}
	return copies, total
}

// replSends is how many PutObject requests a site answered, by status
// code and in total: the sum of its cactusfs_s3_requests_total of the
// operation PutObject, read from its metrics. At site B, which the test
// never restarts and where nothing but site A puts, it counts every copy
// site A sent as a PutObject that passed authentication there - those
// committed and those refused as there already.
func replSends(ctx context.Context, site clustertest.SiteAdmin) (map[string]int64, int64, error) {
	attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	host := site.Hosts[0]
	req, err := http.NewRequestWithContext(attempt, http.MethodGet, "https://"+host+":9001/metrics", nil)
	if err != nil {
		return nil, 0, err
	}
	for name, value := range site.Auth {
		req.Header.Set(name, value)
	}
	resp, err := site.Client(host).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("the metrics of site %s: %w", site.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("the metrics of site %s: %w", site.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("the metrics of site %s = %d", site.Name, resp.StatusCode)
	}
	byCode, total := map[string]int64{}, int64(0)
	code := regexp.MustCompile(`code="([0-9]+)"`)
	for line := range strings.Lines(string(body)) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "cactusfs_s3_requests_total{") || !strings.Contains(line, `operation="PutObject"`) {
			continue
		}
		at := strings.LastIndexByte(line, ' ')
		value, err := strconv.ParseFloat(line[at+1:], 64)
		if err != nil {
			return nil, 0, fmt.Errorf("the metrics of site %s: %q: %w", site.Name, line, err)
		}
		status := "?"
		if match := code.FindStringSubmatch(line); match != nil {
			status = match[1]
		}
		byCode[status] += int64(value)
		total += int64(value)
	}
	return byCode, total, nil
}

// TestChaosReplication: replication between two sites (ADR-0026, PLAN
// 8.1.9) while site A loses its leader twice and site B is cut off from it.
// Site A is the four-node cluster, site B one standalone node with four
// drives on the cluster's network, each with its own CA, key file and root,
// as in TestSiteReplication; A's target holds a key of B that may do only
// the replicate actions on the replica bucket. Both buckets keep versions,
// B's is the replica of A's deployment, and one rule replicates every key.
//
// Three writers put 300 versions of 60 keys to A, through the four nodes in
// turn; a put that is not acknowledged is sent again through the next node.
// While they do, four events, each begun while puts are being made:
//   - at 40 puts, A's leader - named by another node and by itself - is
//     killed; once another node leads it is started again;
//   - at 100 puts, B is cut off from every node of A (blackhole routes in
//     B's network namespace, as Isolate cuts a node off) - while the cut
//     holds, at least a minute and until 200 puts, A's status shows its
//     target retrying or stalled with class timeout at least once, and the
//     last status before the cut ends counts the puts of the cut pending;
//   - then B is reconnected;
//   - at 240 puts, A's leader - whichever node it is now - is killed and
//     started again.
//
// Then, once A's status says nothing pending, nothing failed and nothing
// lost, and the two listings of versions agree in key, version ID, ETag,
// size, time, latest-ness and order: every version A acknowledged is in
// its listing with the key and ETag it was acknowledged with (so at B too);
// every version A lists - counted from the listing, since a put sent again
// may have made two - says COMPLETED at A; B's audit trail holds exactly one record of a committed replica put
// for every one of them and none of any other version (a copy that
// arrives again is refused, ErrConflict, and commits nothing); and B
// answered at most replResends (100) PutObject requests more than there
// are versions - the copies sent again, in total, over both kills and the
// cut, counted by B's cactusfs_s3_requests_total, since a refused copy
// leaves no record in the audit trail.
//
// Not parallel, as no container test of milestone 8 is (PLAN, Milestone
// 8): it runs before the parallel tests of the suite begin. It is expected
// to take some seven minutes after the product image is built: a minute
// and a half to start both sites, some three to four minutes of puts with
// the events (the cut alone is a minute, and each kill waits for an
// election and a restart), under a minute to settle, and seconds to read
// back.
func TestChaosReplication(t *testing.T) {
	c := clustertest.Start(t, clustertest.Options{})
	b := clustertest.StartStandalone(t, c.Network(), replSiteB)
	// The test's own limit, after both sites have started, as startCluster
	// gives one.
	ctx, cancel := context.WithTimeout(context.Background(), replWithin)
	defer cancel()
	begun := time.Now()
	nodes := clustertest.Nodes
	bearer := func(accessKeyID, secretKey string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + accessKeyID + ":" + secretKey}
	}
	atA := clustertest.SiteAdmin{Name: "A", Hosts: nodes, Client: c.Admin, Auth: bearer(c.AccessKey, c.SecretKey), Within: replSetup}
	atB := clustertest.SiteAdmin{
		Name: "B", Hosts: []string{b.Name}, Client: func(string) *http.Client { return b.Admin() },
		Auth: bearer(b.AccessKey, b.SecretKey), Within: replSetup,
	}
	clients := make([]*s3.Client, len(nodes))
	for i, id := range nodes {
		clients[i] = c.S3(id)
	}
	listingA := func(i int) clustertest.SignedS3 {
		id := nodes[i%len(nodes)]
		return clustertest.SignedS3{Host: id, Client: c.HTTP(id), AccessKey: c.AccessKey, SecretKey: c.SecretKey}
	}
	listingB := clustertest.SignedS3{Host: b.Name, Client: b.HTTP(), AccessKey: b.AccessKey, SecretKey: b.SecretKey}
	// The waits of the harness that ctx does not bound - Eventually, the
	// start of a node - end by the test's deadline too.
	testDeadline, _ := ctx.Deadline()
	within := func(limit time.Duration) time.Duration { return replWithinDeadline(limit, testDeadline) }

	// Site A's deployment ID, which site B's bucket is the replica of.
	var deployment string
	c.Eventually("site A to say its deployment ID", within(replSetup), func() bool {
		var shown struct {
			DeploymentID string `json:"deployment_id"`
		}
		status, body, err := atA.Call(ctx, nodes[0], nil, http.MethodGet, "/cluster", nil)
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &shown) != nil {
			return false
		}
		deployment = shown.DeploymentID
		return deployment != ""
	})

	// A bucket that keeps versions, at either site: a site may answer 503
	// while it starts.
	createVersioned := func(client *s3.Client, site, bucket string) {
		t.Helper()
		c.Eventually("site "+site+" to create the bucket "+bucket, within(replSetup), func() bool {
			_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
			switch status := httpStatus(err); {
			case err == nil, status == http.StatusConflict:
				return true
			case status >= 400 && status < 500:
				t.Fatalf("CreateBucket %s at site %s: %v", bucket, site, err)
			}
			return false
		})
		c.Eventually("site "+site+" to keep the versions of "+bucket, within(replSetup), func() bool {
			_, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
				Bucket: aws.String(bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
			})
			if status := httpStatus(err); status >= 400 && status < 500 {
				t.Fatalf("PutBucketVersioning %s at site %s: %v", bucket, site, err)
			}
			return err == nil
		})
	}
	createVersioned(b.S3(), "B", replReplica)
	var mode struct {
		Replica bool   `json:"replica"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(atB.Change(ctx, t, http.MethodPut, "/buckets/"+replReplica+"/replica", map[string]any{"source": deployment}, http.StatusOK), &mode); err != nil ||
		!mode.Replica || mode.Source != deployment {
		t.Fatalf("site B's bucket made the replica of %s: %+v, %v", deployment, mode, err)
	}
	policy := json.RawMessage(replPolicy)
	atB.Change(ctx, t, http.MethodPost, "/policies", map[string]any{"name": "replicate-from-a", "document": policy}, http.StatusCreated)
	atB.Change(ctx, t, http.MethodPost, "/roles", map[string]any{"name": "replicator", "policies": []string{"replicate-from-a"}}, http.StatusCreated)
	atB.Change(ctx, t, http.MethodPost, "/users", map[string]any{"name": "site-a", "roles": []string{"replicator"}}, http.StatusCreated)
	var replicateKey struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	// The answer holds the secret: it is put into no message.
	if err := json.Unmarshal(atB.Change(ctx, t, http.MethodPost, "/users/site-a/keys",
		map[string]any{"description": "site A replicates", "inline_policy": policy}, http.StatusCreated), &replicateKey); err != nil ||
		replicateKey.Key.AccessKeyID == "" || replicateKey.SecretKey == "" {
		t.Fatal("site B made a key of site-a without an access key ID or a secret")
	}
	createVersioned(clients[0], "A", replSource)
	atA.Change(ctx, t, http.MethodPut, "/targets/"+replTarget, map[string]any{
		"kind": "s3", "endpoint": "https://" + b.Name + ":9000", "region": "us-east-1", "ca_pem": string(b.CAPEM),
		"buckets": []string{replSource}, "auth": map[string]any{"mode": "sigv4", "id": replicateKey.Key.AccessKeyID, "secret": replicateKey.SecretKey},
	}, http.StatusOK)
	atA.Change(ctx, t, http.MethodPut, "/buckets/"+replSource+"/replication", map[string]any{
		"target": replTarget, "destination_bucket": replReplica,
		"rules": []map[string]any{{"id": "everything", "enabled": true, "priority": 1, "delete_markers": true}},
	}, http.StatusOK)
	t.Logf("both sites set up in %s", time.Since(begun).Round(time.Second))

	// The puts, and the events among them.
	kill1 := &replEvent{name: "the first kill of the leader", at: 40, begun: make(chan struct{})}
	cut := &replEvent{name: "site B cut off", at: 100, begun: make(chan struct{})}
	rejoin := &replEvent{name: "site B reconnected", at: 200, begun: make(chan struct{})}
	kill2 := &replEvent{name: "the second kill of the leader", at: 240, begun: make(chan struct{})}
	writer := &replWriter{clients: clients, events: []*replEvent{kill1, cut, rejoin, kill2}, versions: map[string]replAcked{}}
	writing, stopWriting := context.WithCancel(ctx)
	failures := make(chan error, replWriters)
	var writers sync.WaitGroup
	for range replWriters {
		writers.Go(func() {
			if err := writer.run(writing); err != nil {
				failures <- err
				stopWriting()
			}
		})
	}
	written := make(chan struct{})
	go func() {
		writers.Wait()
		close(written)
	}()
	// However the test ends, the writers end with it, and are waited for.
	defer func() {
		stopWriting()
		<-written
	}()
	putsBegun := time.Now()

	// await polls every interval until the event is due - ready says so,
	// asked at every poll, and its puts are acknowledged - and then marks
	// it begun. A writer that failed fails the test; the puts can never end
	// before an event is due, since they wait for it.
	await := func(e *replEvent, every time.Duration, ready func() bool) {
		t.Helper()
		for !ready() || writer.acked.Load() < e.at {
			select {
			case err := <-failures:
				t.Fatalf("site A, a put before %s: %v", e.name, err)
			case <-written:
				select {
				case err := <-failures:
					t.Fatalf("site A, a put before %s: %v", e.name, err)
				default:
				}
				t.Fatalf("the puts ended before %s, at %d acknowledged", e.name, writer.acked.Load())
			case <-ctx.Done():
				t.Fatalf("waiting for %s at %d acknowledged puts: %v", e.name, writer.acked.Load(), ctx.Err())
			case <-time.After(every):
			}
		}
		close(e.begun)
	}
	always := func() bool { return true }
	// killLeader kills the node that leads site A's metadata group - by its
	// own account as well as another node's - waits until another leads,
	// and starts the killed one again.
	killLeader := func(e *replEvent) {
		t.Helper()
		await(e, 100*time.Millisecond, always)
		leader := replLeader(c, "", true, testDeadline)
		at := writer.acked.Load()
		c.Kill(leader)
		killed := time.Now()
		t.Logf("%s: killed %s, the leader by its own account, at %d acknowledged puts", e.name, leader, at)
		next := replLeader(c, leader, false, testDeadline)
		t.Logf("%s: %s leads after %s", e.name, next, time.Since(killed).Round(100*time.Millisecond))
		replRestart(t, c, leader, testDeadline)
		t.Logf("%s: %s serves again after %s, at %d acknowledged puts", e.name, leader, time.Since(killed).Round(time.Second), writer.acked.Load())
	}

	killLeader(kill1)

	// Site B cut off: every node of A is running again, so each has its
	// address cut.
	await(cut, 100*time.Millisecond, always)
	c.CutOff(b)
	cutAt := time.Now()
	ackedAtCut := writer.acked.Load()
	t.Logf("%s at %d acknowledged puts", cut.name, ackedAtCut)
	// The target must show a failure of class timeout - retrying or
	// stalled - at least once while the cut holds, as 8.1.8 requires of its
	// pause: a request in flight ends at the response timeout, and a
	// handshake that is never answered at the dial's.
	var timedOut bool
	var states []string
	asked := 0
	await(rejoin, 2*time.Second, func() bool {
		asked++
		if status, err := atA.Status(ctx, nodes[asked%len(nodes)], replSource); err != nil {
			states = append(states, "unread")
		} else {
			timedOut = timedOut || ((status.TargetState == "retrying" || status.TargetState == "stalled") && status.LastErrorClass == "timeout")
			states = append(states, status.TargetState+"/"+status.LastErrorClass)
		}
		return time.Since(cutAt) >= replCut
	})
	// The last status before the cut ends: what was put after the cut began
	// cannot have been taken by site B. replWriters puts of slack, for a
	// node whose copy of the state is a commit or two behind the writers.
	ackedInCut := writer.acked.Load() - ackedAtCut
	var last clustertest.ReplicationStatus
	c.Eventually("the status of site A's replication while site B is cut off", within(replSetup), func() bool {
		var err error
		last, err = atA.Status(ctx, nodes[0], replSource)
		return err == nil
	})
	c.Reconnect(b)
	t.Logf("%s after %s, %d puts acknowledged during the cut, %d pending at its end; the target was %v",
		rejoin.name, time.Since(cutAt).Round(time.Second), ackedInCut, last.Pending, slices.Compact(states))
	if !timedOut {
		t.Errorf("with site B cut off for %s, its target never showed retrying or stalled with class timeout: %v", time.Since(cutAt).Round(time.Second), states)
	}
	if last.Pending < ackedInCut-replWriters || last.Pending == 0 {
		t.Errorf("with site B cut off, the last status says %d versions pending; %d puts were acknowledged during the cut", last.Pending, ackedInCut)
	}

	killLeader(kill2)

	// The rest of the puts.
	select {
	case <-written:
	case <-ctx.Done():
		t.Fatalf("the puts did not end: %d acknowledged: %v", writer.acked.Load(), ctx.Err())
	}
	close(failures)
	for err := range failures {
		t.Fatalf("site A, a put: %v", err)
	}
	if got := writer.acked.Load(); got != replVersions {
		t.Fatalf("site A acknowledged %d puts; want %d", got, replVersions)
	}
	t.Logf("site A acknowledged %d puts in %s, %d attempts tried again", replVersions, time.Since(putsBegun).Round(time.Second), writer.retried.Load())

	// Settled: nothing pending, failed or lost at A, and the listings agree.
	var listing []clustertest.Listed
	settling := time.Now()
	deadline := settling.Add(replSettle)
	for asked := 0; ; asked++ {
		status, err := atA.Status(ctx, nodes[asked%len(nodes)], replSource)
		switch {
		case err != nil:
			err = fmt.Errorf("the status %w: %w", clustertest.ErrUnreachable, err)
		case status.Failed > 0 || status.Lost > 0:
			t.Fatalf("site A's replication: %d versions FAILED and %d rows lost; want none: %+v", status.Failed, status.Lost, status)
		case status.Pending != 0:
			err = fmt.Errorf("%w: %d pending, the target %s (%s)", clustertest.ErrNotYet, status.Pending, status.TargetState, status.LastErrorClass)
		default:
			var atB []clustertest.Listed
			if listing, err = clustertest.ListVersions(ctx, listingA(asked), replSource, ""); err == nil {
				if atB, err = clustertest.ListVersions(ctx, listingB, replReplica, ""); err == nil {
					err = clustertest.CompareListings(listing, atB, nil, nil)
				}
			}
		}
		if err == nil {
			break
		}
		switch {
		case !errors.Is(err, clustertest.ErrNotYet) && !errors.Is(err, clustertest.ErrUnreachable):
			t.Fatalf("both sites to list the same versions: %v", err)
		case time.Now().After(deadline):
			t.Fatalf("both sites to list the same versions within %s: %v", replSettle, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("both sites to list the same versions: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	var versions []string
	keys := map[string]string{}
	for _, e := range listing {
		if e.Marker {
			t.Fatalf("site A lists %s; the test made no delete marker", e)
		}
		versions = append(versions, e.VersionID)
		keys[e.VersionID] = e.Key
	}
	if len(versions) < replVersions {
		t.Fatalf("site A lists %d versions; %d puts were acknowledged", len(versions), replVersions)
	}
	// Every version site A acknowledged is in its listing, under its key
	// and with the entity tag it was acknowledged with: a version lost
	// after its acknowledgement is not made up for by one a retried put
	// made. CompareListings has carried each to site B.
	etags := map[string]string{}
	for _, e := range listing {
		etags[e.VersionID] = strings.Trim(e.ETag, `"`)
	}
	acked := writer.acknowledged()
	if len(acked) != replVersions {
		t.Fatalf("site A acknowledged %d distinct version IDs for %d puts", len(acked), replVersions)
	}
	for id, v := range acked {
		switch etag, listed := etags[id]; {
		case !listed:
			t.Errorf("the version %s of %q, acknowledged by site A, is not in its listing", id, v.key)
		case keys[id] != v.key || etag != strings.Trim(v.etag, `"`):
			t.Errorf("the version %s acknowledged as %q with ETag %s is listed at site A as %q with ETag %s", id, v.key, v.etag, keys[id], etag)
		}
	}
	t.Logf("both sites list the same %d versions (%d made by puts sent again), settled in %s",
		len(versions), len(versions)-replVersions, time.Since(settling).Round(time.Second))

	// Every version at A says COMPLETED.
	if err := clustertest.Each(versions, 8, func(id string) error {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		head, err := clients[int(id[len(id)-1])%len(clients)].HeadObject(attempt, &s3.HeadObjectInput{
			Bucket: aws.String(replSource), Key: aws.String(keys[id]), VersionId: aws.String(id),
		})
		switch {
		case err != nil:
			return fmt.Errorf("HEAD the version %s of %q at site A: %w", id, keys[id], err)
		case head.ReplicationStatus != types.ReplicationStatusCompleted:
			return fmt.Errorf("the version %s of %q at site A: replication status %q; want COMPLETED", id, keys[id], head.ReplicationStatus)
		}
		return nil
	}); err != nil {
		t.Fatalf("the versions at site A: %v", err)
	}

	// Site B's audit trail: one committed copy of every version A lists,
	// and of none other - a copy sent again is refused, and commits
	// nothing.
	var copies map[string]int
	var committed int
	c.Eventually("site B's audit trail to hold a copy of every version", within(replAuditLimit), func() bool {
		records, err := replTrail(ctx, atB)
		if err != nil {
			return false
		}
		copies, committed = replCopies(records, deployment)
		for _, id := range versions {
			if copies[id] == 0 {
				return false
			}
		}
		return true
	})
	for id, n := range copies {
		if _, ok := keys[id]; !ok {
			t.Errorf("site B committed %d copies of the version %s, which site A does not list", n, id)
		} else if n > 1 {
			t.Errorf("site B committed %d copies of the version %s of %q; a copy that arrives again must be refused", n, id, keys[id])
		}
	}
	// What was sent: every PutObject site B answered, committed or refused
	// as there already. None was sent more than replResends times over in
	// total: the sends beyond one a version, over all of them.
	byCode, sent, err := replSends(ctx, atB)
	if err != nil {
		t.Fatal(err)
	}
	resends := sent - int64(len(versions))
	t.Logf("site B answered %d copies of %d versions (by status %v), committed %d: %d sent again", sent, len(versions), byCode, committed, resends)
	if sent < int64(committed) {
		t.Errorf("site B answered %d PutObject requests and committed %d copies", sent, committed)
	}
	if resends > replResends {
		t.Errorf("site B answered %d copies of %d versions: %d sent again, more than %d", sent, len(versions), resends, replResends)
	}
	t.Logf("replication through two kills of the leader and a cut: %s in all", time.Since(begun).Round(time.Second))
}
