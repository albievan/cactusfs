package adminapi_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/upgrade/off"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// newUpdates returns the use case of releases over the fixture's authorizer,
// trail and sessions, the node running 0.7.0 on linux/amd64.
func (f *fixture) newUpdates(source ports.ReleaseSource, upgrader ports.Upgrader) *app.UpdateService {
	f.t.Helper()
	updates, err := app.NewUpdateService(app.UpdateServiceConfig{
		Source: source, Verifier: &porttest.ReleaseVerifier{}, Upgrader: upgrader, Authz: f.authz, Auditor: f.auditor,
		Clock: f.clock, Sessions: f.sessions, Current: mustRelease(f.t, "0.7.0"), Build: "86", Commit: "abc1234",
		Channel: release.ChannelStable, GOOS: "linux", GOARCH: "amd64", Entropy: rand.Reader, Lifetime: context.Background(),
		Logger: slog.New(slog.NewJSONHandler(f.logs, nil)),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return updates
}

func mustRelease(t testing.TB, s string) release.Version {
	t.Helper()
	v, err := release.ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// upgradeSource lists 0.7.1, 0.8.0 and 0.9.0, each with its linux/amd64
// binary and a signed SHA256SUMS; the binary of 0.7.1 is tampered with when
// tampered is set.
func upgradeSource(t *testing.T, tampered bool) *porttest.ReleaseSource {
	t.Helper()
	var releases []release.Release
	files := map[string][]byte{}
	name := release.BinaryAsset("linux", "amd64")
	for _, version := range []string{"0.7.1", "0.8.0", "0.9.0"} {
		v := mustRelease(t, version)
		binary := []byte("binary " + version)
		sum := sha256.Sum256(binary)
		sums := fmt.Appendf(nil, "%x  %s\n", sum, name)
		if tampered && version == "0.7.1" {
			binary = []byte("another binary")
		}
		content := map[string][]byte{name: binary, release.SumsAsset: sums, release.SumsBundleAsset: porttest.FakeBundle(v, sums)}
		var assets []release.Asset
		for file, data := range content {
			url := "https://releases.example/v" + version + "/" + file
			assets = append(assets, release.Asset{Name: file, URL: url, Size: int64(len(data))})
			files[url] = data
		}
		releases = append(releases, release.NewRelease(v, testNow.Add(-time.Hour), false, "https://notes.example/"+version, assets))
	}
	return porttest.NewReleaseSource(releases, files)
}

// newUpdatesFixture is the fixture with the use case of releases made of
// source and upgrader.
func newUpdatesFixture(t *testing.T, source ports.ReleaseSource, upgrader ports.Upgrader) *fixture {
	t.Helper()
	return newFixtureOf(t, func(f *fixture) *app.UpdateService { return f.newUpdates(source, upgrader) }, nil)
}

// updatesAnswer is what GET /updates and POST /updates/check answer, as far
// as these tests read it.
type updatesAnswer struct {
	Current struct {
		Version string `json:"version"`
		Build   string `json:"build"`
	} `json:"current"`
	SourceConfigured bool       `json:"source_configured"`
	Channel          string     `json:"channel"`
	CheckedAt        *time.Time `json:"checked_at"`
	LastError        *string    `json:"last_error"`
	Available        []struct {
		Version string  `json:"version"`
		Allowed bool    `json:"allowed"`
		Refusal *string `json:"refusal"`
	} `json:"available"`
	NextStep          *string  `json:"next_step"`
	Mode              string   `json:"mode"`
	UpgradeAvailable  bool     `json:"upgrade_available"`
	UnavailableReason *string  `json:"unavailable_reason"`
	Instructions      []string `json:"instructions"`
}

// TestUpdatesThroughTheAPI: the answers of the four operations of releases,
// each status the contract names. The password typed with an upgrade is in
// no answer, no line of the log and no record of the trail.
func TestUpdatesThroughTheAPI(t *testing.T) {
	t.Parallel()
	const marked = "MARKED-upgrade-password-a71c"
	media := map[string]string{"Content-Type": "application/json"}
	binaryMode := func(unavailable string) *porttest.Upgrader {
		return porttest.NewUpgrader(porttest.UpgraderConfig{Mode: release.ModeBinary, Unavailable: unavailable, Nodes: []string{"node-a"}})
	}
	// upgrade sends POST /upgrade from a console of a cluster-admin.
	upgrade := func(c *console, version, password string) reply {
		return c.f.do(call{
			method: "POST", path: "/upgrade", client: c.client, headers: c.changing(""),
			body: map[string]any{"version": version, "password": password},
		})
	}
	wantProblem := func(t *testing.T, r reply, status int, code string) {
		t.Helper()
		if r.status != status {
			t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
		}
		if p := r.problem(t); p["code"] != code {
			t.Errorf("problem = %s, want the code %s", r.body, code)
		}
	}

	tests := map[string]struct {
		tampered bool
		noSource bool
		upgrader func() ports.Upgrader
		run      func(t *testing.T, f *fixture)
	}{
		"200: what an auditor reads without a source": {
			noSource: true, upgrader: func() ports.Upgrader { return off.New([]string{"helm upgrade ..."}) },
			run: func(t *testing.T, f *fixture) {
				var got updatesAnswer
				f.must(http.StatusOK, call{method: "GET", path: "/updates", bearer: f.userKey("audrey", iam.RoleAuditor)}).decode(t, &got)
				if got.Current.Version != "0.7.0" || got.Current.Build != "86" || got.SourceConfigured || got.Channel != "stable" ||
					got.Mode != "off" || got.UpgradeAvailable || got.UnavailableReason == nil || *got.UnavailableReason != off.Reason ||
					got.CheckedAt != nil || got.LastError != nil || got.NextStep != nil || len(got.Available) != 0 || got.Instructions == nil {
					t.Errorf("GET /updates = %+v", got)
				}
				var progress struct {
					State string `json:"state"`
					Nodes []any  `json:"nodes"`
				}
				f.must(http.StatusOK, call{method: "GET", path: "/upgrade"}).decode(t, &progress)
				if progress.State != "idle" || progress.Nodes == nil || len(progress.Nodes) != 0 {
					t.Errorf("GET /upgrade = %+v", progress)
				}
			},
		},
		"200 and 429: a check, and one too soon": {
			upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				var got updatesAnswer
				f.must(http.StatusOK, call{method: "POST", path: "/updates/check", headers: media}).decode(t, &got)
				if !got.SourceConfigured || got.CheckedAt == nil || len(got.Available) != 3 || got.NextStep == nil ||
					*got.NextStep != "0.8.0" || got.Mode != "binary" || !got.UpgradeAvailable || got.UnavailableReason != nil {
					t.Errorf("POST /updates/check = %+v", got)
				}
				refusals := map[string]string{}
				for _, r := range got.Available {
					if r.Refusal != nil {
						refusals[r.Version] = *r.Refusal
					}
				}
				if len(refusals) != 1 || refusals["0.9.0"] != "skipped-minor" {
					t.Errorf("refusals = %v", refusals)
				}
				f.clock.Advance(20 * time.Second)
				r := f.do(call{method: "POST", path: "/updates/check", headers: media, bearer: f.root})
				wantProblem(t, r, http.StatusTooManyRequests, "TooManyRequests")
				if r.header.Get("Retry-After") != "40" {
					t.Errorf("Retry-After = %q, want 40", r.header.Get("Retry-After"))
				}
			},
		},
		"403: an auditor may not make the node call out, nor a reader read": {
			upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				wantProblem(t, f.do(call{method: "POST", path: "/updates/check", headers: media, bearer: f.userKey("audrey", iam.RoleAuditor)}),
					http.StatusForbidden, "AccessDenied")
				wantProblem(t, f.do(call{method: "GET", path: "/updates", bearer: f.userKey("rita", iam.RoleReadOnly)}),
					http.StatusForbidden, "AccessDenied")
			},
		},
		"403: an API key never installs, whatever it may": {
			upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				f.must(http.StatusOK, call{method: "POST", path: "/updates/check", headers: media})
				r := f.do(call{method: "POST", path: "/upgrade", bearer: f.root, body: map[string]any{"version": "0.7.1", "password": marked}})
				wantProblem(t, r, http.StatusForbidden, "AccessDenied")
			},
		},
		"403: a wrong password": {
			upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				f.must(http.StatusOK, call{method: "POST", path: "/updates/check", headers: media})
				c := consoleOf(t, f, "carla", iam.RoleClusterAdmin)
				wantProblem(t, upgrade(c, "0.7.1", marked), http.StatusForbidden, "AccessDenied")
			},
		},
		"401: the fifth wrong password ends the session and clears its cookie": {
			upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				f.must(http.StatusOK, call{method: "POST", path: "/updates/check", headers: media})
				c := consoleOf(t, f, "carla", iam.RoleClusterAdmin)
				for i := range 4 {
					if r := upgrade(c, "0.7.1", fmt.Sprintf("%s-%d", marked, i)); r.status != http.StatusForbidden {
						t.Fatalf("guess %d = %d %s", i, r.status, r.body)
					}
				}
				r := upgrade(c, "0.7.1", marked)
				wantProblem(t, r, http.StatusUnauthorized, "Unauthenticated")
				if !strings.Contains(r.header.Get("Set-Cookie"), "Max-Age=0") {
					t.Errorf("Set-Cookie = %q; want the cookie cleared", r.header.Get("Set-Cookie"))
				}
				if r := f.do(call{method: "GET", path: "/session", headers: map[string]string{"Cookie": c.cookie}}); r.status != http.StatusUnauthorized {
					t.Errorf("the session after the fifth wrong password = %d", r.status)
				}
			},
		},
		"400: no version": {
			upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				c := consoleOf(t, f, "carla", iam.RoleClusterAdmin)
				wantProblem(t, upgrade(c, "v-not-a-version", consolePassword()), http.StatusBadRequest, "InvalidArgument")
			},
		},
		"409: checks are off without a source": {
			noSource: true, upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				wantProblem(t, f.do(call{method: "POST", path: "/updates/check", headers: media, bearer: f.root}), http.StatusConflict, "UpdatesOff")
				c := consoleOf(t, f, "carla", iam.RoleClusterAdmin)
				wantProblem(t, upgrade(c, "0.7.1", consolePassword()), http.StatusConflict, "UpdatesOff")
			},
		},
		"409: a skipped minor, and one at a time": {
			upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				f.must(http.StatusOK, call{method: "POST", path: "/updates/check", headers: media})
				c := consoleOf(t, f, "carla", iam.RoleClusterAdmin)
				wantProblem(t, upgrade(c, "0.9.0", consolePassword()), http.StatusConflict, "UpgradeNotAllowed")
				if r := upgrade(c, "0.7.1", consolePassword()); r.status != http.StatusAccepted {
					t.Fatalf("the first upgrade = %d %s", r.status, r.body)
				}
				wantProblem(t, upgrade(c, "0.8.0", consolePassword()), http.StatusConflict, "UpgradeRunning")
			},
		},
		"412: a binary that is not the one its list names": {
			tampered: true, upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				f.must(http.StatusOK, call{method: "POST", path: "/updates/check", headers: media})
				c := consoleOf(t, f, "carla", iam.RoleClusterAdmin)
				r := upgrade(c, "0.7.1", consolePassword())
				wantProblem(t, r, http.StatusPreconditionFailed, "ReleaseNotVerified")
				// The caller is told in the product's words; the chain of
				// what failed is in the log only.
				var body struct {
					Detail string `json:"detail"`
				}
				r.decode(t, &body)
				if body.Detail != "The release could not be verified against its signature; nothing was installed. The node's log says why." {
					t.Errorf("detail = %q", body.Detail)
				}
				if logs := f.logs.String(); !strings.Contains(logs, "the release did not verify") || !strings.Contains(logs, "update.Start") {
					t.Errorf("the chain is not in the log: %s", logs)
				}
			},
		},
		"501: the deployment cannot install from the console": {
			upgrader: func() ports.Upgrader { return binaryMode("the deployment has 4 nodes") },
			run: func(t *testing.T, f *fixture) {
				f.must(http.StatusOK, call{method: "POST", path: "/updates/check", headers: media})
				c := consoleOf(t, f, "carla", iam.RoleClusterAdmin)
				r := upgrade(c, "0.7.1", consolePassword())
				wantProblem(t, r, http.StatusNotImplemented, "UpgradeUnavailable")
				if strings.Contains(f.logs.String(), `"level":"ERROR"`) {
					t.Error("an upgrade the deployment cannot do is logged as an error")
				}
			},
		},
		"202: an upgrade begins, and its progress is read": {
			upgrader: func() ports.Upgrader { return binaryMode("") },
			run: func(t *testing.T, f *fixture) {
				f.must(http.StatusOK, call{method: "POST", path: "/updates/check", headers: media})
				c := consoleOf(t, f, "carla", iam.RoleClusterAdmin)
				r := upgrade(c, "0.7.1", consolePassword())
				if r.status != http.StatusAccepted || r.header.Get("Location") != adminapi.BasePath+"/upgrade" {
					t.Fatalf("POST /upgrade = %d, Location %q: %s", r.status, r.header.Get("Location"), r.body)
				}
				var started struct {
					State     string `json:"state"`
					Target    string `json:"target"`
					StartedBy string `json:"started_by"`
					Nodes     []struct {
						ID    string `json:"id"`
						State string `json:"state"`
					} `json:"nodes"`
				}
				r.decode(t, &started)
				if (started.State != "verifying" && started.State != "running") || started.Target != "0.7.1" || started.StartedBy != "user/carla" {
					t.Errorf("the progress of the start = %+v", started)
				}
				f.must(http.StatusOK, call{method: "GET", path: "/upgrade", client: c.client}).decode(t, &started)
				if started.Target != "0.7.1" || len(started.Nodes) != 1 || started.Nodes[0].ID != "node-a" {
					t.Errorf("GET /upgrade = %+v", started)
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var source ports.ReleaseSource
			if !tt.noSource {
				source = upgradeSource(t, tt.tampered)
			}
			f := newUpdatesFixture(t, source, tt.upgrader())
			tt.run(t, f)
			if strings.Contains(f.logs.String(), marked) || strings.Contains(fmt.Sprint(f.trail()), marked) {
				t.Error("the password of an upgrade is in the log or the audit trail")
			}
		})
	}
}

// TestUpdatesWithoutTheUseCase: a node that was not given the use case
// answers 501 to a caller it has authenticated, and 401 to anybody else.
func TestUpdatesWithoutTheUseCase(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, func(c *adminapi.Config) { c.Updates = nil })
	for _, c := range []call{
		{method: "GET", path: "/updates"},
		{method: "POST", path: "/updates/check", headers: map[string]string{"Content-Type": "application/json"}},
		{method: "POST", path: "/upgrade", body: map[string]any{"version": "0.7.1", "password": "not-a-password-that-is-kept"}},
		{method: "GET", path: "/upgrade"},
	} {
		if r := f.do(c); r.status != http.StatusUnauthorized {
			t.Errorf("%s %s without a credential = %d", c.method, c.path, r.status)
		}
		c.bearer = f.root
		r := f.do(c)
		if r.status != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501: %s", c.method, c.path, r.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "NotImplemented" {
			t.Errorf("%s %s: problem = %s", c.method, c.path, r.body)
		}
	}
}
