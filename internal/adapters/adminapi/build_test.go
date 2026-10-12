package adminapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// testBuild is the build the fixture's API runs: nothing else in an answer
// can look like any part of it.
var testBuild = buildinfo.Info{Version: "97.98.99-fixture", Build: "424242-of-the-fixture", Commit: "c0ffee15c0de-dirty"}

// serverOf is the `server` object of an answer.
type serverOf struct {
	Server struct {
		Version string  `json:"version"`
		Build   string  `json:"build"`
		Commit  *string `json:"commit"`
	} `json:"server"`
}

func (s serverOf) check(t *testing.T, what string, want buildinfo.Info) {
	t.Helper()
	commit := ""
	if s.Server.Commit != nil {
		commit = *s.Server.Commit
	}
	if s.Server.Version != want.Version || s.Server.Build != want.Build || commit != want.Commit {
		t.Errorf("%s: server = %q build %q commit %q, want %+v", what, s.Server.Version, s.Server.Build, commit, want)
	}
}

// namesTheBuild reports where an answer names the build, or "".
func namesTheBuild(header http.Header, body []byte, build buildinfo.Info) string {
	for _, part := range []string{build.Version, build.Build, strings.TrimSuffix(build.Commit, "-dirty")} {
		if strings.Contains(string(body), part) {
			return "the body has " + part
		}
		for name, values := range header {
			if strings.Contains(strings.Join(values, "\n"), part) {
				return "the header " + name + " has " + part
			}
		}
	}
	// What a server usually says about itself, whatever is in it.
	for _, name := range []string{"Server", "X-Powered-By", "X-Cactusfs-Version", "X-Version"} {
		if header.Get(name) != "" {
			return "there is a header " + name
		}
	}
	return ""
}

// TestTheBuildIsToldToAuthenticatedCallers: the session answers and the
// cluster status say which build answered.
func TestTheBuildIsToldToAuthenticatedCallers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const password = "correct horse battery"
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
		"name": "carol", "roles": []string{iam.RoleReadOnly}, "password": password,
	}})

	var asRoot, signed, asCarol, status serverOf
	f.must(http.StatusOK, call{method: "GET", path: "/session"}).decode(t, &asRoot)
	asRoot.check(t, "GET /session with an API key", testBuild)

	client, _, r := signIn(t, f, "carol", password)
	if r.status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", r.status, r.body)
	}
	r.decode(t, &signed)
	signed.check(t, "POST /session", testBuild)

	f.must(http.StatusOK, call{method: "GET", path: "/session", client: client}).decode(t, &asCarol)
	asCarol.check(t, "GET /session with a session", testBuild)

	f.must(http.StatusOK, call{method: "GET", path: "/cluster"}).decode(t, &status)
	status.check(t, "GET /cluster", testBuild)
}

// TestTheBuildIsNotToldBeforeAuthentication: nothing that is answered to a
// request without a credential that checks out - a refusal, the sign-in that
// failed, the contract, an unknown endpoint, a node that is starting - names
// the release, the build or the commit, in its body or in a header.
func TestTheBuildIsNotToldBeforeAuthentication(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const password = "correct horse battery"
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
		"name": "carol", "roles": []string{iam.RoleReadOnly}, "password": password,
	}})

	// A key nobody was given: an ID and a word after it, as a key is written.
	unknownKey := strings.Join([]string{"AKIANOSUCHKEY", "nothing"}, ":")
	tests := []struct {
		name string
		call call
		want int
	}{
		{name: "who am I, without a credential", call: call{method: "GET", path: "/session"}, want: http.StatusUnauthorized},
		{name: "who am I, with a key that does not exist", call: call{method: "GET", path: "/session", bearer: unknownKey}, want: http.StatusUnauthorized},
		{name: "who am I, with a malformed credential", call: call{method: "GET", path: "/session", bearer: "nonsense"}, want: http.StatusUnauthorized},
		{name: "who am I, with a session that does not exist", call: call{method: "GET", path: "/session", headers: map[string]string{"Cookie": httpsession.CookieName + "=no-such-session"}}, want: http.StatusUnauthorized},
		{name: "the cluster, without a credential", call: call{method: "GET", path: "/cluster"}, want: http.StatusUnauthorized},
		{name: "a sign-in with the wrong password", call: call{method: "POST", path: "/session", body: map[string]any{"username": "carol", "password": "wrong"}}, want: http.StatusUnauthorized},
		{name: "a sign-in of nobody", call: call{method: "POST", path: "/session", body: map[string]any{"username": "nobody", "password": "wrong"}}, want: http.StatusUnauthorized},
		{name: "a sign-in that is not JSON", call: call{method: "POST", path: "/session", raw: []byte("username=carol")}, want: http.StatusUnsupportedMediaType},
		{name: "a sign-in without a body", call: call{method: "POST", path: "/session", body: map[string]any{}}},
		{name: "signing out, without a session", call: call{method: "DELETE", path: "/session"}, want: http.StatusUnauthorized},
		{name: "the contract as YAML", call: call{method: "GET", path: "/openapi.yaml"}, want: http.StatusOK},
		{name: "the contract as JSON", call: call{method: "GET", path: "/openapi.json"}, want: http.StatusOK},
		{name: "an endpoint that does not exist", call: call{method: "GET", path: "/no-such-thing"}, want: http.StatusNotFound},
		{name: "a method the endpoint does not have", call: call{method: "PATCH", path: "/session"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := f.do(tt.call)
			if tt.want != 0 && r.status != tt.want {
				t.Errorf("status = %d, want %d; body: %s", r.status, tt.want, r.body)
			}
			if r.status >= 200 && r.status < 300 && tt.want != http.StatusOK {
				t.Errorf("status = %d: this request was to be refused", r.status)
			}
			if where := namesTheBuild(r.header, r.body, testBuild); where != "" {
				t.Errorf("status %d names the build: %s; body: %s", r.status, where, r.body)
			}
		})
	}

	t.Run("a node that is starting", func(t *testing.T) {
		t.Parallel()
		for _, target := range []string{"/api/v1/session", "/api/v1/cluster", "/", "/docs", "/metrics"} {
			rec := httptest.NewRecorder()
			adminapi.Starting().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://admin.test"+target, nil))
			if where := namesTheBuild(rec.Header(), rec.Body.Bytes(), testBuild); where != "" {
				t.Errorf("GET %s while starting names the build: %s", target, where)
			}
		}
	})
}
