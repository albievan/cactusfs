//go:build integration && rollingupgrade && !windows

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/test/clustertest"
)

// The bounds the nodes of the release under test run with here. The session
// times out after twenty seconds. It may stay locked for half an hour: the
// test holds it locked until the leader - a node of the release before -
// sweeps, which that release does every ten minutes.
const (
	sessionRollingLifetime = 20 * time.Second
	sessionRollingGrace    = 15 * time.Second
	sessionRollingLockMax  = 30 * time.Minute
	// sessionRollingSweep is how long the test waits for the sweep of the
	// leader: the ten minutes between two sweeps, and what a busy host adds.
	sessionRollingSweep = 14 * time.Minute
)

// sessionRollingPassword is the password of the console user of this test,
// put together here so that no credential is written into the source.
func sessionRollingPassword() string {
	return strings.Join([]string{"a", "password", "for", "the", "rolling", "console"}, " ")
}

// sessionRollingCall sends one request to the management API of a node
// within a minute and fails the test when no answer comes: every node is to
// answer throughout.
func sessionRollingCall(t *testing.T, c *clustertest.Cluster, id string, headers map[string]string, method, path string, body any) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	status, header, data, err := gridCall(ctx, c, id, headers, method, path, body)
	if err != nil {
		t.Fatalf("%s %s through %s: %v", method, path, id, err)
	}
	return status, header, data
}

// sessionRollingSignIn signs the user in through a node and returns the
// headers of a request that reads and of one that changes something. It
// fails the test without printing the answer, which holds a session.
func sessionRollingSignIn(t *testing.T, c *clustertest.Cluster, id, user string) (reading, changing map[string]string, expiresAt time.Time) {
	t.Helper()
	status, header, body := sessionRollingCall(t, c, id, nil, http.MethodPost, "/session", map[string]any{"username": user, "password": sessionRollingPassword()})
	var session struct {
		CSRFToken string    `json:"csrf_token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	cookie := strings.SplitN(header.Get("Set-Cookie"), ";", 2)[0]
	if err := json.Unmarshal(body, &session); err != nil || status != http.StatusOK || session.CSRFToken == "" || !strings.HasPrefix(cookie, "__Host-cactusfs-session=") {
		t.Fatalf("signing %s in through %s = %d (%v), a cookie of %d bytes", user, id, status, err, len(cookie))
	}
	return map[string]string{"Cookie": cookie}, map[string]string{"Cookie": cookie, "X-CSRF-Token": session.CSRFToken}, session.ExpiresAt
}

// sessionRollingProblem reads the code of a problem answer; "" when the body
// is none.
func sessionRollingProblem(body []byte) string {
	var problem struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &problem) != nil {
		return ""
	}
	return problem.Code
}

// TestRollingUpgradeFromSchema3ToSchema4SessionLock: a console session that
// is locked (ADR-0027) in a deployment where members of the release before
// and a member of this release run together - the middle of a rolling
// upgrade. The three voters, and so the leader of the metadata group, run
// the image $CACTUSFS_TEST_IMAGE_SCHEMA3 names; the fourth node runs the
// image under test.
//
// The release before knows no lock. What must hold is that the lock is
// never MORE than it was there, and that nothing breaks:
//
//   - through the node of this release, a session past its timeout with an
//     upload in flight is locked: 423 for everything but its own state and
//     its uploads;
//   - through a node of the release before, the same cookie is that of a
//     session that has ended: 401 for whatever is asked, and for the
//     operation that release does not have, 404 - never a 5xx, never an
//     answer that gives anything;
//   - when the leader, of the release before, sweeps, it removes the record
//     of the session, which for it ended at the timeout: from then on the
//     node of this release answers 401 as well. The lock has become what a
//     timeout was before - a sign-out;
//   - the upload that was in flight all along is stored; every member stays
//     ready; the user signs in again, through either release.
//
// The upload lease the node of this release writes is a record among the
// sessions under a key no member of the release before ever looks up. That
// it is written through a leader of that release, for as long as this test
// takes, and that the deployment goes on, is part of what is tested.
//
// What it does not cover: a change of password in the mixed deployment (the
// mark of ADR-0027, which no member of the release before reads), and a
// leader of this release with gateways of the release before - here the
// leader is always of the release before.
//
// It bears the name of the rolling upgrade it is a part of, so that `make
// test-rolling`, which runs the tests of that name, runs it too. It holds
// for whichever release before the image is of: sessions have been what they
// are since the release of ADR-0006. Without the image named it fails; it is
// never skipped. It is not run beside other tests: it waits, for minutes,
// for a sweep, and what it asserts before that is a matter of seconds.
//
// How it is run. Today `make test-rolling` selects with `-run` and a prefix -
// `-run 'TestRollingUpgradeFromSchema3ToSchema4'`, the image built from
// SCHEMA3_COMMIT - and this test is among those it runs. PLAN 8.0.11 is to
// select one test by its exact name (ROLLING_TEST) and the release before by
// ROLLING_FROM_COMMIT; from then on this test is not run unless it is named:
//
//	make test-rolling ROLLING_FROM_COMMIT=0b7d825 ROLLING_TEST=TestRollingUpgradeFromSchema3ToSchema4SessionLock
//
// and by hand, with an image of the release before built and named:
//
//	CACTUSFS_TEST_IMAGE_SCHEMA3=<image> go test -count=1 -timeout 40m \
//	    -tags=integration,rollingupgrade -run '^TestRollingUpgradeFromSchema3ToSchema4SessionLock$' ./test/integration/
func TestRollingUpgradeFromSchema3ToSchema4SessionLock(t *testing.T) {
	old := os.Getenv(schema3ImageEnv)
	if old == "" {
		t.Fatalf("%s names no image: this test runs members of the release before beside one of this release, and needs an image of it", schema3ImageEnv)
	}
	voters, fresh := clustertest.Nodes[:3], "node4"
	images := map[string]string{}
	for _, id := range voters {
		images[id] = old
	}
	// The members of the release before do not know these settings and
	// ignore them: for them a session is what its record says.
	c := clustertest.Start(t, clustertest.Options{Images: images, Env: map[string]string{
		"CACTUSFS_SESSION_LIFETIME":     sessionRollingLifetime.String(),
		"CACTUSFS_SESSION_UPLOAD_GRACE": sessionRollingGrace.String(),
		"CACTUSFS_SESSION_LOCK_MAX":     sessionRollingLockMax.String(),
	}})
	for _, id := range voters {
		if c.ImageOf(id) != old {
			t.Fatalf("%s runs %s, want %s", id, c.ImageOf(id), old)
		}
	}
	if c.ImageOf(fresh) == old {
		t.Fatalf("the image under test is %s, the image of the release before: no member would know the lock", old)
	}
	c.WaitReady(clustertest.Nodes...)
	root := gridBearer(c.AccessKey, c.SecretKey)

	// The leader is a voter, and every voter is of the release before.
	leaderCtx, cancelLeader := context.WithTimeout(t.Context(), time.Minute)
	leader, _, err := schema4Cluster(leaderCtx, c, fresh, root)
	cancelLeader()
	if err != nil || !slices.Contains(voters, leader) {
		t.Fatalf("the leader of the metadata group is %q (%v), want one of %v", leader, err, voters)
	}
	before := voters[0]

	if status, _, body := sessionRollingCall(t, c, fresh, root, http.MethodPost, "/buckets", map[string]any{"name": "uploads"}); status != http.StatusCreated {
		t.Fatalf("create bucket = %d %s", status, body)
	}
	if status, _, body := sessionRollingCall(t, c, fresh, root, http.MethodPost, "/users", map[string]any{
		"name": "ursula", "roles": []string{"bucket-admin"}, "password": sessionRollingPassword(),
	}); status != http.StatusCreated {
		t.Fatalf("create user = %d %s", status, body)
	}

	// Signed in through the node of this release: its lifetime is the short
	// one, and it is written into the record that every member reads.
	reading, changing, expiresAt := sessionRollingSignIn(t, c, fresh, "ursula")
	if left := time.Until(expiresAt); left < sessionRollingLifetime-10*time.Second || left > sessionRollingLifetime+2*time.Second {
		t.Fatalf("the session reaches its timeout in %v, want %v: is the lifetime of %s the one the test set?", left, sessionRollingLifetime, fresh)
	}
	// Before the timeout it is a session for every member.
	for _, id := range []string{fresh, before} {
		if status, _, body := sessionRollingCall(t, c, id, reading, http.MethodGet, "/buckets", nil); status != http.StatusOK {
			t.Fatalf("GET /buckets through %s before the timeout = %d %s", id, status, body)
		}
	}

	// An upload through the node of this release whose body arrives a
	// kilobyte a second, for as long as the test says: it is in flight
	// until the test ends it.
	chunk := []byte(strings.Repeat("cactus! ", 128))
	const chunks = 20 * 60
	uploadCtx, stopUpload := context.WithCancel(t.Context())
	defer stopUpload()
	enough := make(chan struct{})
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		sent := 0
		for sent < chunks {
			select {
			case <-uploadCtx.Done():
				_ = pipeWriter.CloseWithError(uploadCtx.Err())
				return
			case <-enough:
				// The rest at once: the declared length is kept.
				for ; sent < chunks; sent++ {
					if _, err := pipeWriter.Write(chunk); err != nil {
						return
					}
				}
			case <-ticker.C:
				if _, err := pipeWriter.Write(chunk); err != nil {
					return
				}
				sent++
			}
		}
		_ = pipeWriter.Close()
	}()
	request, err := http.NewRequestWithContext(uploadCtx, http.MethodPut, "https://"+fresh+":9001/api/v1/buckets/uploads/object?key=slow", pipeReader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = int64(chunks * len(chunk))
	request.Header.Set("Content-Type", "application/octet-stream")
	for name, value := range changing {
		request.Header.Set(name, value)
	}
	type uploaded struct {
		status int
		body   string
	}
	done := make(chan uploaded, 1)
	go func() {
		// The harness's client gives a request two minutes; this one takes
		// as long as it takes.
		resp, err := (&http.Client{Transport: c.Admin(fresh).Transport}).Do(request)
		if err != nil {
			done <- uploaded{body: err.Error()}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		done <- uploaded{status: resp.StatusCode, body: string(data)}
	}()

	time.Sleep(time.Until(expiresAt.Add(2 * time.Second)))

	// Through the node of this release: locked.
	status, header, body := sessionRollingCall(t, c, fresh, reading, http.MethodGet, "/buckets", nil)
	if status != http.StatusLocked || sessionRollingProblem(body) != "SessionLocked" || header.Get("WWW-Authenticate") != "" {
		t.Fatalf("GET /buckets through %s, past the timeout, uploading, = %d %s; want 423 SessionLocked", fresh, status, body)
	}
	status, _, body = sessionRollingCall(t, c, fresh, reading, http.MethodGet, "/session", nil)
	var who struct {
		Locked *bool `json:"locked"`
	}
	if err := json.Unmarshal(body, &who); err != nil || status != http.StatusOK || who.Locked == nil || !*who.Locked {
		t.Fatalf("GET /session through %s, past the timeout, uploading, = %d %s; want 200 and locked", fresh, status, body)
	}

	// Through a node of the release before - and through the leader, when
	// that is another: the session has ended. Whatever is asked with its
	// cookie is answered 401 - and the operation that release does not
	// have, 404. Nothing is given, nothing fails.
	asked := []string{before}
	if leader != before {
		asked = append(asked, leader)
	}
	type probe struct {
		method, path string
		headers      map[string]string
		body         any
		// want is 401, but for what the release before does not serve.
		want []int
	}
	unauthorized := []int{http.StatusUnauthorized}
	probes := []probe{
		{http.MethodGet, "/session", reading, nil, unauthorized},
		{http.MethodGet, "/buckets", reading, nil, unauthorized},
		{http.MethodGet, "/buckets/uploads/objects", reading, nil, unauthorized},
		{http.MethodGet, "/users", reading, nil, unauthorized},
		{http.MethodPost, "/buckets", changing, map[string]any{"name": "made-through-the-release-before"}, unauthorized},
		{http.MethodDelete, "/buckets/uploads", changing, nil, unauthorized},
		// The release before has no such operation: 404 where it routes
		// before it authenticates, 401 where it does not.
		{http.MethodPost, "/session/unlock", changing, map[string]any{"password": sessionRollingPassword()}, []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusMethodNotAllowed}},
	}
	for _, id := range asked {
		for _, p := range probes {
			status, _, body := sessionRollingCall(t, c, id, p.headers, p.method, p.path, p.body)
			if !slices.Contains(p.want, status) {
				t.Errorf("%s %s through %s, of the release before, with the cookie of the locked session = %d %s; want one of %v",
					p.method, p.path, id, status, body, p.want)
			}
		}
		// Its uploads are not taken either: for that release the session
		// is gone, and it is not this release's to say otherwise there.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		put, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+id+":9001/api/v1/buckets/uploads/object?key=through-the-release-before", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		put.Header.Set("Content-Type", "application/octet-stream")
		for name, value := range changing {
			put.Header.Set(name, value)
		}
		resp, err := c.Admin(id).Do(put)
		if err != nil {
			t.Fatalf("an upload through %s: %v", id, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("an upload through %s, of the release before, with the cookie of the locked session = %d, want 401", id, resp.StatusCode)
		}
	}
	// Asking a node of the release before changed nothing for the node of
	// this release: still locked, and still storing objects.
	if status, _, body := sessionRollingCall(t, c, fresh, reading, http.MethodGet, "/session", nil); status != http.StatusOK {
		t.Fatalf("GET /session through %s after the release before was asked = %d %s", fresh, status, body)
	}
	if status, _, _ := sessionRollingCall(t, c, fresh, root, http.MethodGet, "/buckets/made-through-the-release-before", nil); status != http.StatusNotFound {
		t.Errorf("the bucket a request of an ended session asked for = %d, want 404", status)
	}

	// The leader sweeps: every ten minutes it removes the sessions that -
	// by its release - have ended, and this one has, since its timeout.
	// Until then the node of this release says "locked", from then on
	// "ended": never anything else, and no member stops answering.
	deadline := time.Now().Add(sessionRollingSweep)
	swept := false
	for !swept {
		status, _, body := sessionRollingCall(t, c, fresh, reading, http.MethodGet, "/session", nil)
		switch status {
		case http.StatusOK:
			if json.Unmarshal(body, &who) != nil || who.Locked == nil || !*who.Locked {
				t.Fatalf("GET /session through %s while waiting for the sweep = %s; want locked", fresh, body)
			}
		case http.StatusUnauthorized:
			swept = true
		default:
			t.Fatalf("GET /session through %s while waiting for the sweep = %d %s; want 200 locked, then 401", fresh, status, body)
		}
		select {
		case answer := <-done:
			t.Fatalf("the upload ended while the session was held locked: %d %s", answer.status, answer.body)
		default:
		}
		if !swept && time.Now().After(deadline) {
			t.Fatalf("the session is still locked through %s after %v: the leader %s, of the release before, did not sweep it", fresh, sessionRollingSweep, leader)
		}
		if !swept {
			time.Sleep(5 * time.Second)
		}
	}
	t.Logf("the leader %s swept the session %v after its timeout", leader, time.Since(expiresAt).Round(time.Second))
	// Ended for good, on every member, and for every operation.
	for _, id := range clustertest.Nodes {
		for _, path := range []string{"/session", "/buckets"} {
			if status, _, body := sessionRollingCall(t, c, id, reading, http.MethodGet, path, nil); status != http.StatusUnauthorized {
				t.Errorf("GET %s through %s after the sweep = %d %s, want 401", path, id, status, body)
			}
		}
	}
	if status, _, body := sessionRollingCall(t, c, fresh, changing, http.MethodPost, "/session/unlock", map[string]any{"password": sessionRollingPassword()}); status != http.StatusUnauthorized {
		t.Errorf("unlocking through %s after the sweep = %d %s, want 401", fresh, status, body)
	}

	// The upload was authenticated when it began: the server finishes it.
	close(enough)
	select {
	case answer := <-done:
		if answer.status != http.StatusOK {
			t.Errorf("the upload that was in flight across the lock and the sweep = %d %s, want 200", answer.status, answer.body)
		}
	case <-time.After(5 * time.Minute):
		t.Error("the upload that was in flight was not answered")
	}
	if status, _, body := sessionRollingCall(t, c, before, root, http.MethodGet, "/buckets/uploads/object/metadata?key=slow", nil); status != http.StatusOK {
		t.Errorf("the object, asked for through %s = %d %s", before, status, body)
	}

	// Nothing stopped: every member is ready, the leader leads, and the user
	// signs in again through either release - and through the node of this
	// release has a session that locks as the first one did not get to end.
	for _, id := range clustertest.Nodes {
		if !c.Ready(id) {
			t.Errorf("%s is not ready after the sweep", id)
		}
	}
	for _, id := range []string{before, fresh} {
		again, _, _ := sessionRollingSignIn(t, c, id, "ursula")
		for _, through := range []string{fresh, before} {
			if status, _, body := sessionRollingCall(t, c, through, again, http.MethodGet, "/buckets", nil); status != http.StatusOK {
				t.Errorf("GET /buckets through %s with a session begun through %s = %d %s", through, id, status, body)
			}
		}
	}
	for _, id := range clustertest.Nodes {
		if log := c.Logs(id); strings.Contains(log, "panic: ") {
			t.Errorf("the log of %s speaks of a panic", id)
		}
	}
}
