//go:build integration && !windows

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The bounds the node of these tests runs with: a session that times out
// after twenty seconds, ends fifteen seconds after its last upload, and is
// locked for two minutes at the most (ADR-0027). They are short, and not so
// short that a machine busy with other tests misses them: a tick of the
// keeper of leases is five seconds, and nothing here is asserted nearer than
// two seconds to a boundary. Every moment is taken from what the node said
// - when the session reaches its timeout - or from what was measured - when
// the upload ended - never from when the test began.
const (
	sessionLockLifetime = 20 * time.Second
	sessionLockGrace    = 15 * time.Second
	sessionLockMax      = 2 * time.Minute
	// sessionLockSlow is how long the body of the slow upload takes to
	// arrive: past the timeout by half a lifetime.
	sessionLockSlow = 30 * time.Second
)

// sessionLockPassword is the password of the console users of these tests,
// put together here so that no credential is written into the source.
func sessionLockPassword() string {
	return strings.Join([]string{"a", "password", "for", "the", "console"}, " ")
}

// sessionLockNode is the binary, running by itself with the short bounds
// above, and the root credential it was started with.
type sessionLockNode struct {
	// admin is the base of the management API, with its scheme.
	admin string
	root  map[string]string
}

// sessionLockServer starts the node and reads its log while it runs: a node
// whose log nobody reads stops at the line that fills the pipe. The function
// it returns stops the node, and fails the test unless it stopped cleanly.
func sessionLockServer(ctx context.Context, t *testing.T, bin string) (*sessionLockNode, func()) {
	t.Helper()
	dir := t.TempDir()
	accessKey, secretKey := "AKIDSESSIONLOCKROOT0", strings.Repeat("session-lock-", 3)+"0123456789"
	srv := startServer(ctx, t, bin, dir,
		"CACTUSFS_ROOT_ACCESS_KEY="+accessKey, "CACTUSFS_ROOT_SECRET_KEY="+secretKey,
		"CACTUSFS_SESSION_LIFETIME="+sessionLockLifetime.String(),
		"CACTUSFS_SESSION_UPLOAD_GRACE="+sessionLockGrace.String(),
		"CACTUSFS_SESSION_LOCK_MAX="+sessionLockMax.String(),
	)
	node := &sessionLockNode{
		admin: "http://" + waitForListening(t, srv.logs)["admin"] + "/api/v1",
		root:  map[string]string{"Authorization": "Bearer " + accessKey + ":" + secretKey},
	}
	var (
		read       = make(chan struct{})
		sawStopped bool
		notJSON    []string
	)
	go func() {
		defer close(read)
		for srv.logs.Scan() {
			var entry struct {
				Msg string `json:"msg"`
			}
			if err := json.Unmarshal(srv.logs.Bytes(), &entry); err != nil {
				notJSON = append(notJSON, srv.logs.Text())
				continue
			}
			if entry.Msg == "stopped" {
				sawStopped = true
			}
		}
	}()
	return node, func() {
		t.Helper()
		if err := srv.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("send SIGTERM: %v", err)
		}
		// The log is read to its end before the process is waited for:
		// waiting closes the pipe.
		<-read
		if err := srv.cmd.Wait(); err != nil {
			t.Fatalf("server did not exit cleanly on SIGTERM: %v\n%s", err, srv.stderr.String())
		}
		for _, line := range notJSON {
			t.Errorf("log line is not JSON: %q", line)
		}
		if !sawStopped {
			t.Error("server exited without logging a clean stop")
		}
	}
}

// sessionLockConsole is a browser that has signed in: its cookie, its CSRF
// token, and when the node said the session reaches its timeout.
type sessionLockConsole struct {
	node      *sessionLockNode
	cookie    string
	csrf      string
	expiresAt time.Time
}

// reading and changing are the headers of the console's requests.
func (c *sessionLockConsole) reading() map[string]string {
	return map[string]string{"Cookie": c.cookie}
}

func (c *sessionLockConsole) changing() map[string]string {
	return map[string]string{"Cookie": c.cookie, "X-CSRF-Token": c.csrf}
}

// sessionLockAnswer reads the session an answer of sign-in or of unlock
// holds. It fails the test without printing the answer: that holds a session.
func sessionLockAnswer(t *testing.T, node *sessionLockNode, what string, status int, header http.Header, body []byte) *sessionLockConsole {
	t.Helper()
	var session struct {
		CSRFToken string    `json:"csrf_token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	set := header.Get("Set-Cookie")
	cookie := strings.SplitN(set, ";", 2)[0]
	if err := json.Unmarshal(body, &session); err != nil || status != http.StatusOK || session.CSRFToken == "" ||
		!strings.HasPrefix(cookie, "__Host-cactusfs-session=") || session.ExpiresAt.IsZero() {
		t.Fatalf("%s = %d (%v), a cookie of %d bytes", what, status, err, len(cookie))
	}
	// The cookie is the one it always was, and is kept for as long as the
	// session can be locked: its timeout and the cap.
	parsed, err := http.ParseSetCookie(set)
	if err != nil {
		t.Fatalf("%s: the cookie does not parse: %v", what, err)
	}
	if !parsed.HttpOnly || !parsed.Secure || parsed.SameSite != http.SameSiteStrictMode || parsed.Path != "/" || parsed.Domain != "" {
		t.Errorf("%s: the cookie is HttpOnly %v, Secure %v, SameSite %v, Path %q, Domain %q",
			what, parsed.HttpOnly, parsed.Secure, parsed.SameSite, parsed.Path, parsed.Domain)
	}
	if kept := parsed.Expires.Sub(session.ExpiresAt); kept < sessionLockMax-2*time.Second || kept > sessionLockMax+2*time.Second {
		t.Errorf("%s: the cookie is kept %v past the timeout, want %v", what, kept, sessionLockMax)
	}
	return &sessionLockConsole{node: node, cookie: cookie, csrf: session.CSRFToken, expiresAt: session.ExpiresAt}
}

// sessionLockSignIn makes a user with a role and signs the user in.
func sessionLockSignIn(ctx context.Context, t *testing.T, node *sessionLockNode, user, role string) *sessionLockConsole {
	t.Helper()
	if status, _, body := adminCall(ctx, t, http.MethodPost, node.admin+"/users", node.root, map[string]any{
		"name": user, "roles": []string{role}, "password": sessionLockPassword(),
	}); status != http.StatusCreated {
		t.Fatalf("create user %s = %d %s", user, status, body)
	}
	status, header, body := adminCall(ctx, t, http.MethodPost, node.admin+"/session", nil, map[string]any{"username": user, "password": sessionLockPassword()})
	c := sessionLockAnswer(t, node, "signing "+user+" in", status, header, body)
	if left := time.Until(c.expiresAt); left < sessionLockLifetime-10*time.Second || left > sessionLockLifetime+2*time.Second {
		t.Fatalf("the session of %s reaches its timeout in %v, want %v", user, left, sessionLockLifetime)
	}
	return c
}

// sessionLockState asks GET /session. It returns the status, and whether the
// answer says that the session is locked.
func (c *sessionLockConsole) state(ctx context.Context, t *testing.T) (status int, locked bool, lockEndsAt time.Time) {
	t.Helper()
	status, _, body := adminCall(ctx, t, http.MethodGet, c.node.admin+"/session", c.reading(), nil)
	if status != http.StatusOK {
		return status, false, time.Time{}
	}
	var who struct {
		Via        string     `json:"via"`
		Locked     *bool      `json:"locked"`
		ExpiresAt  *time.Time `json:"expires_at"`
		LockEndsAt *time.Time `json:"lock_ends_at"`
	}
	if err := json.Unmarshal(body, &who); err != nil || who.Via != "session" || who.Locked == nil || who.ExpiresAt == nil || who.LockEndsAt == nil {
		t.Fatalf("GET /session = %s (%v)", body, err)
	}
	if !who.ExpiresAt.Equal(c.expiresAt) {
		t.Errorf("GET /session says the timeout is %v, sign-in said %v", who.ExpiresAt, c.expiresAt)
	}
	return status, *who.Locked, *who.LockEndsAt
}

// sessionLockUpload is an upload whose body arrives slowly.
type sessionLockUpload struct {
	// done is told the status of the answer, 0 when there was none, and
	// when it arrived.
	done chan sessionLockUploaded
}

type sessionLockUploaded struct {
	status int
	at     time.Time
	body   string
}

// upload begins an upload of this console whose body arrives over a time: a
// kilobyte every quarter of a second. It is authenticated once, when it
// begins.
func (c *sessionLockConsole) upload(ctx context.Context, t *testing.T, bucket, key string, over time.Duration) *sessionLockUpload {
	t.Helper()
	const every = 250 * time.Millisecond
	chunk := []byte(strings.Repeat("cactus! ", 128))
	chunks := int(over / every)
	reader, writer := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.node.admin+"/buckets/"+bucket+"/object?key="+key, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(chunks * len(chunk))
	req.Header.Set("Content-Type", "application/octet-stream")
	for name, value := range c.changing() {
		req.Header.Set(name, value)
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for range chunks {
			select {
			case <-ctx.Done():
				_ = writer.CloseWithError(ctx.Err())
				return
			case <-ticker.C:
			}
			if _, err := writer.Write(chunk); err != nil {
				return
			}
		}
		_ = writer.Close()
	}()
	up := &sessionLockUpload{done: make(chan sessionLockUploaded, 1)}
	go func() {
		// No timeout of the client: the upload takes as long as it takes.
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			up.done <- sessionLockUploaded{at: time.Now(), body: err.Error()}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		up.done <- sessionLockUploaded{status: resp.StatusCode, at: time.Now(), body: string(data)}
	}()
	return up
}

// stored waits for the answer of the upload and fails the test unless the
// object was stored.
func (u *sessionLockUpload) stored(t *testing.T, what string) time.Time {
	t.Helper()
	select {
	case answer := <-u.done:
		if answer.status != http.StatusOK {
			t.Fatalf("%s = %d %s, want 200", what, answer.status, answer.body)
		}
		return answer.at
	case <-time.After(60 * time.Second):
		t.Fatalf("%s was not answered", what)
		return time.Time{}
	}
}

// sessionLockProblem fails the test unless an answer is the problem that is
// expected. Only a 401 may say WWW-Authenticate: a console takes that, and
// the status, for "the session is gone".
func sessionLockProblem(t *testing.T, what string, status int, header http.Header, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &problem); err != nil || status != wantStatus || problem.Code != wantCode {
		t.Fatalf("%s = %d %s, want %d %s", what, status, body, wantStatus, wantCode)
	}
	if wantStatus != http.StatusUnauthorized && header.Get("WWW-Authenticate") != "" {
		t.Errorf("%s is answered WWW-Authenticate %q", what, header.Get("WWW-Authenticate"))
	}
}

// sessionLockAudit waits until the audit trail has a record of a user's that
// matches, and fails the test when none comes: refusals are written in the
// background.
func sessionLockAudit(ctx context.Context, t *testing.T, node *sessionLockNode, what string, matches func(actor, action, outcome string, details map[string]string) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/audit?limit=1000", node.root, nil)
		var page struct {
			Records []struct {
				Actor   string            `json:"actor"`
				Action  string            `json:"action"`
				Outcome string            `json:"outcome"`
				Details map[string]string `json:"details"`
			} `json:"records"`
		}
		if err := json.Unmarshal(body, &page); err != nil || status != http.StatusOK {
			t.Fatalf("GET /audit = %d %s (%v)", status, body, err)
		}
		for _, r := range page.Records {
			if matches(r.Actor, r.Action, r.Outcome, r.Details) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit trail has no record of %s among its %d records", what, len(page.Records))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestSessionLock: the binary, with a session that times out after twenty
// seconds, and an upload that is slower than that (ADR-0027).
//
//   - A session that reaches its timeout while it uploads is locked: it
//     stores objects, and is refused everything else with 423 until the
//     password of its user unlocks it - which gives it a new cookie, the old
//     one being refused from then on. The upload that was in flight all
//     along is stored.
//   - Nobody unlocks: the session is locked at every moment the upload is
//     in flight, and ends a grace after the upload did.
//   - Nothing is uploaded: the session ends at its timeout, as it always did.
//
// It is not run beside other tests: what it asserts is a matter of seconds.
func TestSessionLock(t *testing.T) {
	bin := buildBinary(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	node, stop := sessionLockServer(ctx, t, bin)
	if status, _, body := adminCall(ctx, t, http.MethodPost, node.admin+"/buckets", node.root, map[string]any{"name": "uploads"}); status != http.StatusCreated {
		t.Fatalf("create bucket = %d %s", status, body)
	}
	password := sessionLockPassword()

	t.Run("locked while it uploads, and unlocked with the password", func(t *testing.T) {
		c := sessionLockSignIn(ctx, t, node, "ursula", "bucket-admin")
		if status, locked, _ := c.state(ctx, t); status != http.StatusOK || locked {
			t.Fatalf("GET /session after sign-in = %d, locked %v", status, locked)
		}
		if status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/buckets", c.reading(), nil); status != http.StatusOK {
			t.Fatalf("GET /buckets before the timeout = %d %s", status, body)
		}
		// The body arrives over thirty seconds; the session times out
		// after twenty.
		slow := c.upload(ctx, t, "uploads", "slow", sessionLockSlow)
		time.Sleep(time.Until(c.expiresAt.Add(2 * time.Second)))

		status, header, body := adminCall(ctx, t, http.MethodGet, node.admin+"/buckets", c.reading(), nil)
		sessionLockProblem(t, "GET /buckets past the timeout, uploading,", status, header, body, http.StatusLocked, "SessionLocked")
		status, locked, lockEndsAt := c.state(ctx, t)
		if status != http.StatusOK || !locked || !lockEndsAt.Equal(c.expiresAt.Add(sessionLockMax)) {
			t.Fatalf("GET /session past the timeout, uploading, = %d, locked %v until %v", status, locked, lockEndsAt)
		}
		// A change is refused like a reading, and changes nothing.
		status, header, body = adminCall(ctx, t, http.MethodPost, node.admin+"/buckets", c.changing(), map[string]any{"name": "made-while-locked"})
		sessionLockProblem(t, "POST /buckets by the locked session", status, header, body, http.StatusLocked, "SessionLocked")
		// A second upload, begun now, is stored.
		if status, _, body := adminCall(ctx, t, http.MethodPut, node.admin+"/buckets/uploads/object?key=second",
			map[string]string{"Cookie": c.cookie, "X-CSRF-Token": c.csrf, "Content-Type": "application/octet-stream"}, []byte("begun while locked")); status != http.StatusOK {
			t.Fatalf("an upload begun by the locked session = %d %s", status, body)
		}

		// A wrong password: 403, and still locked.
		status, header, body = adminCall(ctx, t, http.MethodPost, node.admin+"/session/unlock", c.changing(), map[string]any{"password": "not the password"})
		sessionLockProblem(t, "unlocking with a wrong password", status, header, body, http.StatusForbidden, "AccessDenied")
		if status, locked, _ := c.state(ctx, t); status != http.StatusOK || !locked {
			t.Fatalf("GET /session after a wrong password = %d, locked %v", status, locked)
		}
		// The right one: 200, a new cookie and a new CSRF token.
		status, header, body = adminCall(ctx, t, http.MethodPost, node.admin+"/session/unlock", c.changing(), map[string]any{"password": password})
		unlocked := sessionLockAnswer(t, node, "unlocking", status, header, body)
		if unlocked.cookie == c.cookie || unlocked.csrf == c.csrf {
			t.Fatal("the unlocked session has the cookie or the CSRF token it had")
		}
		if left := time.Until(unlocked.expiresAt); left < sessionLockLifetime-10*time.Second || left > sessionLockLifetime+2*time.Second {
			t.Errorf("the unlocked session reaches its timeout in %v, want %v", left, sessionLockLifetime)
		}
		// The old cookie is refused, the new one has its authority back.
		status, header, body = adminCall(ctx, t, http.MethodGet, node.admin+"/session", c.reading(), nil)
		sessionLockProblem(t, "GET /session with the old cookie", status, header, body, http.StatusUnauthorized, "Unauthenticated")
		status, header, body = adminCall(ctx, t, http.MethodPost, node.admin+"/session/unlock", c.changing(), map[string]any{"password": password})
		sessionLockProblem(t, "unlocking with the old cookie", status, header, body, http.StatusUnauthorized, "Unauthenticated")
		// That may be the unlock itself, sent twice: its answer leaves the
		// cookie alone, which by now is the new one.
		if set := header.Get("Set-Cookie"); set != "" {
			t.Errorf("unlocking with the old cookie is answered Set-Cookie of %d bytes", len(set))
		}
		if status, locked, _ := unlocked.state(ctx, t); status != http.StatusOK || locked {
			t.Fatalf("GET /session after the unlock = %d, locked %v", status, locked)
		}
		status, _, body = adminCall(ctx, t, http.MethodGet, node.admin+"/buckets", unlocked.reading(), nil)
		if status != http.StatusOK || !strings.Contains(string(body), `"uploads"`) {
			t.Fatalf("GET /buckets after the unlock = %d %s", status, body)
		}
		if strings.Contains(string(body), "made-while-locked") {
			t.Error("the bucket a locked session asked for was made")
		}

		// The upload that began before the timeout, under the cookie that
		// is refused by now, is stored: it was authenticated when it began.
		slow.stored(t, "the upload that was in flight when the session locked")
		for _, key := range []string{"slow", "second"} {
			if status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/buckets/uploads/object/metadata?key="+key, node.root, nil); status != http.StatusOK {
				t.Errorf("the object %s = %d %s", key, status, body)
			}
		}

		// The audit trail holds the denial, the failure and the unlock.
		sessionLockAudit(ctx, t, node, "the refusal of the locked session", func(actor, _, outcome string, details map[string]string) bool {
			return actor == "user/ursula" && outcome == "denied" && details["reason"] == "the session is locked"
		})
		sessionLockAudit(ctx, t, node, "the wrong password at the unlock", func(_, action, outcome string, _ map[string]string) bool {
			return action == "auth:UnlockSession" && outcome == "failed"
		})
		sessionLockAudit(ctx, t, node, "the unlock", func(actor, action, outcome string, _ map[string]string) bool {
			return actor == "user/ursula" && action == "auth:UnlockSession" && outcome == "success"
		})
	})

	t.Run("ended a grace after its uploads when nobody unlocks", func(t *testing.T) {
		c := sessionLockSignIn(ctx, t, node, "walter", "bucket-admin")
		slow := c.upload(ctx, t, "uploads", "unattended", sessionLockSlow)
		time.Sleep(time.Until(c.expiresAt.Add(2 * time.Second)))
		if status, locked, _ := c.state(ctx, t); status != http.StatusOK || !locked {
			t.Fatalf("GET /session past the timeout, uploading, = %d, locked %v", status, locked)
		}
		// While the upload is in flight the session is locked at every
		// moment: asked once a second until the upload has ended, it never
		// answers anything else. A lease that lapsed under the upload would
		// show here as a 401.
		var ended time.Time
		for ended.IsZero() {
			select {
			case answer := <-slow.done:
				if answer.status != http.StatusOK {
					t.Fatalf("the upload of a session nobody unlocks = %d %s, want 200", answer.status, answer.body)
				}
				ended = answer.at
			case <-time.After(time.Second):
				asked := time.Now()
				status, locked, _ := c.state(ctx, t)
				select {
				case answer := <-slow.done:
					// It ended while the session was asked: that answer
					// says nothing of a session with an upload in flight.
					slow.done <- answer
				default:
					if status != http.StatusOK || !locked {
						t.Fatalf("GET /session with the upload in flight, %v past the timeout, = %d, locked %v", asked.Sub(c.expiresAt), status, locked)
					}
				}
			}
		}
		// It ends one grace after the upload did - the measured end, as the
		// test saw the answer, which is a moment after the node's - and is
		// locked until then. Asked every quarter of a second, with two
		// seconds either way for the two clocks and the asking.
		const margin = 2 * time.Second
		for {
			asked := time.Now()
			status, header, body := adminCall(ctx, t, http.MethodGet, node.admin+"/session", c.reading(), nil)
			after := asked.Sub(ended)
			if status == http.StatusUnauthorized {
				sessionLockProblem(t, "GET /session of the ended session", status, header, body, http.StatusUnauthorized, "Unauthenticated")
				if after < sessionLockGrace-margin {
					t.Errorf("the session ended %v after its last upload; the grace is %v", after, sessionLockGrace)
				}
				break
			}
			if status != http.StatusOK {
				t.Fatalf("GET /session after the upload = %d %s", status, body)
			}
			if after > sessionLockGrace+margin {
				t.Fatalf("the session is still there %v after its last upload ended; the grace is %v", after, sessionLockGrace)
			}
			time.Sleep(250 * time.Millisecond)
		}
		// Ended is ended: the password does not bring it back.
		status, header, body := adminCall(ctx, t, http.MethodPost, node.admin+"/session/unlock", c.changing(), map[string]any{"password": password})
		sessionLockProblem(t, "unlocking the ended session", status, header, body, http.StatusUnauthorized, "Unauthenticated")
	})

	t.Run("ended at the timeout when nothing is uploaded", func(t *testing.T) {
		c := sessionLockSignIn(ctx, t, node, "xavier", "bucket-admin")
		time.Sleep(time.Until(c.expiresAt.Add(-2 * time.Second)))
		if status, locked, _ := c.state(ctx, t); status != http.StatusOK || locked {
			t.Fatalf("GET /session two seconds before the timeout = %d, locked %v", status, locked)
		}
		time.Sleep(time.Until(c.expiresAt.Add(time.Second)))
		status, header, body := adminCall(ctx, t, http.MethodGet, node.admin+"/session", c.reading(), nil)
		sessionLockProblem(t, "GET /session at the timeout, nothing uploading,", status, header, body, http.StatusUnauthorized, "Unauthenticated")
		status, header, body = adminCall(ctx, t, http.MethodGet, node.admin+"/buckets", c.reading(), nil)
		sessionLockProblem(t, "GET /buckets at the timeout, nothing uploading,", status, header, body, http.StatusUnauthorized, "Unauthenticated")
	})

	t.Run("a change of password ends the active and the locked sessions", func(t *testing.T) {
		locked := sessionLockSignIn(ctx, t, node, "yvonne", "bucket-admin")
		slow := locked.upload(ctx, t, "uploads", "while-the-password-changes", sessionLockSlow)
		time.Sleep(time.Until(locked.expiresAt.Add(2 * time.Second)))
		if status, isLocked, _ := locked.state(ctx, t); status != http.StatusOK || !isLocked {
			t.Fatalf("GET /session past the timeout, uploading, = %d, locked %v", status, isLocked)
		}
		// A second session of the same user, active.
		status, header, body := adminCall(ctx, t, http.MethodPost, node.admin+"/session", nil, map[string]any{"username": "yvonne", "password": password})
		active := sessionLockAnswer(t, node, "signing yvonne in again", status, header, body)
		if status, isLocked, _ := active.state(ctx, t); status != http.StatusOK || isLocked {
			t.Fatalf("GET /session of the second session = %d, locked %v", status, isLocked)
		}

		changed := strings.Join([]string{"the", "password", "root", "gave", "her"}, " ")
		if status, _, body := adminCall(ctx, t, http.MethodPut, node.admin+"/users/yvonne", node.root, map[string]any{
			"roles": []string{"bucket-admin"}, "groups": []string{}, "disabled": false, "password": changed,
		}); status != http.StatusOK {
			t.Fatalf("PUT /users/yvonne with a new password = %d %s", status, body)
		}
		changedAt := time.Now()
		for name, c := range map[string]*sessionLockConsole{"locked": locked, "active": active} {
			status, header, body := adminCall(ctx, t, http.MethodGet, node.admin+"/session", c.reading(), nil)
			sessionLockProblem(t, "GET /session of the "+name+" session after the password changed", status, header, body, http.StatusUnauthorized, "Unauthenticated")
			status, header, body = adminCall(ctx, t, http.MethodPost, node.admin+"/session/unlock", c.changing(), map[string]any{"password": changed})
			sessionLockProblem(t, "unlocking the "+name+" session with the new password", status, header, body, http.StatusUnauthorized, "Unauthenticated")
		}
		// The upload in flight is finished by the server, as one is whose
		// session ends in any other way.
		slow.stored(t, "the upload in flight when the password changed")

		// A session begun within app.PasswordChangeMargin (5 s) of the
		// change is ended as one from before it: the new password signs in
		// once that has passed.
		time.Sleep(time.Until(changedAt.Add(5*time.Second + 2*time.Second)))
		status, header, body = adminCall(ctx, t, http.MethodPost, node.admin+"/session", nil, map[string]any{"username": "yvonne", "password": changed})
		fresh := sessionLockAnswer(t, node, "signing in with the new password", status, header, body)
		if status, isLocked, _ := fresh.state(ctx, t); status != http.StatusOK || isLocked {
			t.Fatalf("GET /session of the session of the new password = %d, locked %v", status, isLocked)
		}
		status, header, body = adminCall(ctx, t, http.MethodPost, node.admin+"/session", nil, map[string]any{"username": "yvonne", "password": password})
		sessionLockProblem(t, "signing in with the old password", status, header, body, http.StatusUnauthorized, "Unauthenticated")
	})

	stop()
}
