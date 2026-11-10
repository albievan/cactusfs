package app_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// The releases of the tests, the running one being 0.7.0. Of them 0.7.1,
// 0.7.2 and 0.8.1 may be installed.
var updateVersions = []string{"0.6.0", "0.7.0", "0.7.1", "0.7.2", "0.8.0", "0.8.1", "0.8.2-rc.1", "0.9.0", "1.0.0"}

// countingSource is a release source that counts how often its releases
// were asked for.
type countingSource struct {
	*porttest.ReleaseSource
	asked atomic.Int64
}

func (s *countingSource) Releases(ctx context.Context) ([]release.Release, error) {
	s.asked.Add(1)
	return s.ReleaseSource.Releases(ctx)
}

// updateFixture is the update use case with a source listing
// updateVersions, each with a signed SHA256SUMS of its linux/amd64 binary
// and a signed digests.txt, and alice - a cluster-admin - signed in.
type updateFixture struct {
	*iamFixture
	source   *countingSource
	verifier *porttest.ReleaseVerifier
	upgrader *porttest.Upgrader
	updates  *app.UpdateService
	cfg      app.UpdateServiceConfig
	// alice is the principal of her session, token its token.
	alice    iam.Principal
	token    string
	password string
	// progress is what the upgrader reported.
	progressMu sync.Mutex
	progress   []release.Progress
}

func mustVersion(t testing.TB, s string) release.Version {
	t.Helper()
	v, err := release.ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func assetURL(version, name string) string {
	return "https://releases.example/v" + version + "/" + name
}

// releaseFiles returns the assets and files of a release, as the release
// workflow makes them.
func releaseFiles(t testing.TB, version string) ([]release.Asset, map[string][]byte) {
	t.Helper()
	v := mustVersion(t, version)
	name := release.BinaryAsset("linux", "amd64")
	binary := []byte("binary " + version)
	sum := sha256.Sum256(binary)
	sums := fmt.Appendf(nil, "%x  %s\n", sum, name)
	list := sha256.Sum256([]byte("list " + version))
	platform := sha256.Sum256([]byte("platform " + version))
	digests := fmt.Appendf(nil, "registry.example/cactusfs:%s sha256:%x\nregistry.example/cactusfs:%s linux/amd64 sha256:%x\n", v, list, v, platform)
	files := map[string][]byte{
		name:                       binary,
		release.SumsAsset:          sums,
		release.SumsBundleAsset:    porttest.FakeBundle(v, sums),
		release.DigestsAsset:       digests,
		release.DigestsBundleAsset: porttest.FakeBundle(v, digests),
	}
	var assets []release.Asset
	byURL := map[string][]byte{}
	for _, n := range []string{name, release.SumsAsset, release.SumsBundleAsset, release.DigestsAsset, release.DigestsBundleAsset} {
		assets = append(assets, release.Asset{Name: n, URL: assetURL(version, n), Size: int64(len(files[n]))})
		byURL[assetURL(version, n)] = files[n]
	}
	return assets, byURL
}

// newUpdateFixture makes the fixture; tuneFiles may change the release
// files by address - or remove them, and their assets - before the source is
// made, and tune the configuration before the use case is.
func newUpdateFixture(t *testing.T, mode release.Mode, tuneFiles func(map[string][]byte), tune func(*app.UpdateServiceConfig)) *updateFixture {
	t.Helper()
	ctx := t.Context()
	f := &updateFixture{iamFixture: newIAMFixture(t), verifier: &porttest.ReleaseVerifier{}}
	f.password = strings.Join([]string{"the", "admins", "own", "phrase"}, " ")
	if _, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleClusterAdmin}, Password: f.password}); err != nil {
		t.Fatal(err)
	}
	var releases []release.Release
	files := map[string][]byte{}
	for _, version := range updateVersions {
		assets, byURL := releaseFiles(t, version)
		releases = append(releases, release.NewRelease(mustVersion(t, version), testNow.Add(-time.Hour), false, "https://notes.example/"+version, assets))
		for url, content := range byURL {
			files[url] = content
		}
	}
	if tuneFiles != nil {
		tuneFiles(files)
		// A file tuneFiles removed is not among the release's assets.
		for i := range releases {
			releases[i].Assets = slices.DeleteFunc(releases[i].Assets, func(a release.Asset) bool {
				_, kept := files[a.URL]
				return !kept
			})
		}
	}
	f.source = &countingSource{ReleaseSource: porttest.NewReleaseSource(releases, files)}
	f.upgrader = porttest.NewUpgrader(porttest.UpgraderConfig{
		Mode: mode, Nodes: []string{"node-1"}, Now: f.clock.Now,
		Instructions: []string{"helm upgrade cactusfs --set image.digest=sha256:..."},
	})
	f.cfg = app.UpdateServiceConfig{
		Source: f.source, Verifier: f.verifier, Upgrader: f.upgrader, Authz: f.authz, Auditor: f.auditor, Clock: f.clock,
		Sessions: f.sessions, Current: mustVersion(t, "0.7.0"), Build: "86", Commit: "abc1234",
		Channel: release.ChannelStable, GOOS: "linux", GOARCH: "amd64", Interval: 24 * time.Hour,
		Entropy: rand.Reader, Lifetime: t.Context(), Logger: slog.New(slog.NewJSONHandler(f.logs, nil)),
		OnProgress: func(p release.Progress) {
			f.progressMu.Lock()
			defer f.progressMu.Unlock()
			f.progress = append(f.progress, p)
		},
	}
	if tune != nil {
		tune(&f.cfg)
		if u, ok := f.cfg.Upgrader.(*porttest.Upgrader); ok {
			f.upgrader = u
		}
	}
	var err error
	if f.updates, err = app.NewUpdateService(f.cfg); err != nil {
		t.Fatalf("NewUpdateService: %v", err)
	}
	signed, err := f.sessions.SignIn(ctx, "alice", f.password, session("198.51.100.20"))
	if err != nil {
		t.Fatal(err)
	}
	f.token = signed.Token
	if f.alice, err = f.sessions.Authenticate(ctx, signed.Token, signed.CSRFToken, true, session("198.51.100.20")); err != nil {
		t.Fatal(err)
	}
	return f
}

// checked has the fixture's use case ask the source once.
func (f *updateFixture) checked(t *testing.T) app.UpdatesView {
	t.Helper()
	view, err := f.updates.Check(t.Context(), f.alice)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return view
}

// records returns the records of the trail of an action.
func (f *updateFixture) records(t *testing.T, action iam.Action) []audit.Record {
	t.Helper()
	var out []audit.Record
	for _, r := range f.trail(t) {
		if r.Action == string(action) {
			out = append(out, r)
		}
	}
	return out
}

// reported returns the progress the upgrader reported, once there are n of
// them.
func (f *updateFixture) reported(t *testing.T, n int) []release.Progress {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.progressMu.Lock()
		got := slices.Clone(f.progress)
		f.progressMu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNewUpdateService(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*app.UpdateServiceConfig){
		"no verifier":            func(c *app.UpdateServiceConfig) { c.Verifier = nil },
		"no upgrader":            func(c *app.UpdateServiceConfig) { c.Upgrader = nil },
		"no authorizer":          func(c *app.UpdateServiceConfig) { c.Authz = nil },
		"no auditor":             func(c *app.UpdateServiceConfig) { c.Auditor = nil },
		"no clock":               func(c *app.UpdateServiceConfig) { c.Clock = nil },
		"no sessions":            func(c *app.UpdateServiceConfig) { c.Sessions = nil },
		"no entropy":             func(c *app.UpdateServiceConfig) { c.Entropy = nil },
		"no lifetime":            func(c *app.UpdateServiceConfig) { c.Lifetime = nil },
		"no logger":              func(c *app.UpdateServiceConfig) { c.Logger = nil },
		"no running version":     func(c *app.UpdateServiceConfig) { c.Current = release.Version{} },
		"no channel":             func(c *app.UpdateServiceConfig) { c.Channel = "" },
		"another channel":        func(c *app.UpdateServiceConfig) { c.Channel = "nightly" },
		"a negative interval":    func(c *app.UpdateServiceConfig) { c.Interval = -time.Hour },
		"an interval too short":  func(c *app.UpdateServiceConfig) { c.Interval = time.Second },
		"(fine) no source":       func(c *app.UpdateServiceConfig) { c.Source = nil },
		"(fine) never by itself": func(c *app.UpdateServiceConfig) { c.Interval = 0 },
	}
	base := newUpdateFixture(t, release.ModeBinary, nil, nil)
	for name, tune := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := base.cfg
			tune(&cfg)
			_, err := app.NewUpdateService(cfg)
			if strings.HasPrefix(name, "(fine)") {
				if err != nil {
					t.Errorf("NewUpdateService = %v", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("NewUpdateService = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestUpdatesGet(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		mode  release.Mode
		tune  func(*app.UpdateServiceConfig)
		check bool
		want  func(t *testing.T, f *updateFixture, view app.UpdatesView)
	}{
		"no source: nothing is known and nothing is asked": {
			mode: release.ModeBinary, tune: func(c *app.UpdateServiceConfig) { c.Source = nil },
			want: func(t *testing.T, _ *updateFixture, view app.UpdatesView) {
				if view.SourceConfigured || len(view.Available) != 0 || !view.CheckedAt.IsZero() || !view.NextStep.IsZero() || len(view.Instructions) != 0 {
					t.Errorf("view = %+v", view)
				}
			},
		},
		"not checked yet": {
			mode: release.ModeBinary,
			want: func(t *testing.T, f *updateFixture, view app.UpdatesView) {
				if !view.SourceConfigured || len(view.Available) != 0 || !view.CheckedAt.IsZero() || f.source.asked.Load() != 0 {
					t.Errorf("view = %+v, source asked %d times", view, f.source.asked.Load())
				}
				if view.Current.String() != "0.7.0" || view.Build != "86" || view.Commit != "abc1234" || view.Channel != release.ChannelStable {
					t.Errorf("the build in the view = %+v", view)
				}
			},
		},
		"checked: each release with whether it may be installed": {
			mode: release.ModeBinary, check: true,
			want: func(t *testing.T, _ *updateFixture, view app.UpdatesView) {
				got := map[string]string{}
				var order []string
				for _, r := range view.Available {
					got[r.Version.String()] = string(r.Refusal)
					order = append(order, r.Version.String())
					if r.Allowed != (r.Refusal == "") || r.NotesURL == "" {
						t.Errorf("release %s = %+v", r.Version, r)
					}
				}
				want := map[string]string{
					"1.0.0": "major", "0.9.0": "skipped-minor", "0.8.1": "", "0.8.0": "not-newest-patch", "0.7.2": "", "0.7.1": "",
				}
				if fmt.Sprint(got) != fmt.Sprint(want) || !slices.Equal(order, []string{"1.0.0", "0.9.0", "0.8.1", "0.8.0", "0.7.2", "0.7.1"}) {
					t.Errorf("available = %v in the order %v, want %v", got, order, want)
				}
				if view.NextStep.String() != "0.8.1" || !view.CheckedAt.Equal(testNow) || view.LastError != "" {
					t.Errorf("view = %+v", view)
				}
				if !view.UpgradeAvailable || view.UnavailableReason != "" || view.Mode != release.ModeBinary || len(view.Instructions) != 1 {
					t.Errorf("view = %+v", view)
				}
			},
		},
		"the prerelease channel offers the pre-release": {
			mode: release.ModeBinary, check: true, tune: func(c *app.UpdateServiceConfig) { c.Channel = release.ChannelPrerelease },
			want: func(t *testing.T, _ *updateFixture, view app.UpdatesView) {
				if view.NextStep.String() != "0.8.2-rc.1" || view.Available[2].Version.String() != "0.8.2-rc.1" || !view.Available[2].Allowed {
					t.Errorf("view = %+v", view)
				}
			},
		},
		"upgrade-mode off: why, and the instructions": {
			mode: release.ModeOff, check: true,
			want: func(t *testing.T, _ *updateFixture, view app.UpdatesView) {
				if view.UpgradeAvailable || view.Mode != release.ModeOff || len(view.Instructions) != 1 {
					t.Errorf("view = %+v", view)
				}
			},
		},
		"nothing to install: no instructions": {
			mode: release.ModeBinary, check: true, tune: func(c *app.UpdateServiceConfig) { c.Current = mustVersion(t, "1.0.0") },
			want: func(t *testing.T, _ *updateFixture, view app.UpdatesView) {
				if len(view.Available) != 0 || !view.NextStep.IsZero() || len(view.Instructions) != 0 {
					t.Errorf("view = %+v", view)
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpdateFixture(t, tt.mode, nil, tt.tune)
			if tt.check {
				f.checked(t)
			}
			asked := f.source.asked.Load()
			view, err := f.updates.Get(t.Context(), f.alice)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if f.source.asked.Load() != asked {
				t.Error("Get asked the source")
			}
			tt.want(t, f, view)
		})
	}

	t.Run("who may read", func(t *testing.T) {
		t.Parallel()
		f := newUpdateFixture(t, release.ModeBinary, nil, nil)
		auditor := f.as(t, f.user(t, "audrey", iam.RoleAuditor))
		if _, err := f.updates.Get(t.Context(), auditor); err != nil {
			t.Errorf("an auditor reading = %v", err)
		}
		reader := f.as(t, f.user(t, "rita", iam.RoleReadOnly))
		if _, err := f.updates.Get(t.Context(), reader); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("a reader of objects reading = %v", err)
		}
		if _, err := f.updates.Progress(t.Context(), reader); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("a reader of objects reading the progress = %v", err)
		}
		if denied := f.records(t, iam.ActionGetUpdates); len(denied) != 2 || denied[0].Outcome != audit.Denied {
			t.Errorf("the refusals in the trail = %+v", denied)
		}
		progress, err := f.updates.Progress(t.Context(), auditor)
		if err != nil || progress.State != release.StateIdle {
			t.Errorf("Progress = %+v, %v", progress, err)
		}
	})
}

func TestUpdatesCheck(t *testing.T) {
	t.Parallel()
	tests := map[string]func(t *testing.T, f *updateFixture){
		"no source": func(t *testing.T, f *updateFixture) {
			f.cfg.Source = nil
			updates, err := app.NewUpdateService(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := updates.Check(t.Context(), f.alice); !errors.Is(err, domain.ErrUpdatesOff) {
				t.Errorf("Check = %v, want ErrUpdatesOff", err)
			}
			if f.source.asked.Load() != 0 {
				t.Error("the source was asked")
			}
		},
		"an auditor may not make the node call out": func(t *testing.T, f *updateFixture) {
			auditor := f.as(t, f.user(t, "audrey", iam.RoleAuditor))
			if _, err := f.updates.Check(t.Context(), auditor); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("Check = %v, want ErrAccessDenied", err)
			}
			records := f.records(t, iam.ActionCheckForUpdates)
			if f.source.asked.Load() != 0 || len(records) != 1 || records[0].Outcome != audit.Denied || records[0].Actor != "user/audrey" {
				t.Errorf("source asked %d times; records %+v", f.source.asked.Load(), records)
			}
		},
		"a check is audited": func(t *testing.T, f *updateFixture) {
			view := f.checked(t)
			if len(view.Available) != 6 {
				t.Errorf("view = %+v", view)
			}
			records := f.records(t, iam.ActionCheckForUpdates)
			if len(records) != 1 || records[0].Outcome != audit.Succeeded || records[0].Details["releases"] != "9" || records[0].Actor != "user/alice" {
				t.Errorf("records = %+v", records)
			}
		},
		"at most once a minute": func(t *testing.T, f *updateFixture) {
			f.checked(t)
			f.clock.Advance(20 * time.Second)
			_, err := f.updates.Check(t.Context(), f.alice)
			var throttled *app.CheckThrottledError
			if !errors.Is(err, domain.ErrTooManyRequests) || !errors.As(err, &throttled) || throttled.RetryAfter != 40*time.Second {
				t.Fatalf("a check 20 s after another = %v", err)
			}
			if f.source.asked.Load() != 1 {
				t.Errorf("the source was asked %d times", f.source.asked.Load())
			}
			f.clock.Advance(40 * time.Second)
			f.checked(t)
			if f.source.asked.Load() != 2 || len(f.records(t, iam.ActionCheckForUpdates)) != 2 {
				t.Errorf("after a minute the source was asked %d times", f.source.asked.Load())
			}
		},
		"a source that does not answer: its class, and what it said before": func(t *testing.T, f *updateFixture) {
			f.checked(t)
			f.source.SetError(fmt.Errorf("%w: releases.example did not answer", domain.ErrUnavailable))
			f.clock.Advance(time.Minute)
			view := f.checked(t)
			if view.LastError != app.CheckErrorUnreachable || !view.CheckedAt.Equal(testNow.Add(time.Minute)) || len(view.Available) != 6 {
				t.Errorf("view = %+v", view)
			}
			records := f.records(t, iam.ActionCheckForUpdates)
			if records[len(records)-1].Details["error"] != app.CheckErrorUnreachable {
				t.Errorf("record = %+v", records[len(records)-1])
			}
			f.source.SetError(nil)
			f.clock.Advance(time.Minute)
			if view := f.checked(t); view.LastError != "" {
				t.Errorf("view after it answered again = %+v", view)
			}
		},
		"the classes of the default classifier": func(t *testing.T, f *updateFixture) {
			for err, want := range map[error]string{
				context.DeadlineExceeded:    app.CheckErrorTimeout,
				domain.ErrUnavailable:       app.CheckErrorUnreachable,
				domain.ErrInvalidArgument:   app.CheckErrorInvalidAnswer,
				domain.ErrEntityTooLarge:    app.CheckErrorInvalidAnswer,
				errors.New("a 404"):         app.CheckErrorRefused,
				fmt.Errorf("w: %w", io.EOF): app.CheckErrorRefused,
			} {
				f.source.SetError(err)
				f.clock.Advance(time.Minute)
				if view := f.checked(t); view.LastError != want {
					t.Errorf("%v is classed %q, want %q", err, view.LastError, want)
				}
			}
		},
		"every asking is told, with whether a release may be installed": func(t *testing.T, f *updateFixture) {
			type told struct {
				class       string
				installable bool
			}
			var got []told
			f.cfg.OnCheck = func(class string, installable bool) { got = append(got, told{class, installable}) }
			updates, err := app.NewUpdateService(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				f.clock.Advance(time.Minute)
				if _, err := updates.Check(t.Context(), f.alice); err != nil {
					t.Fatal(err)
				}
			}
			// Unanswered before anything was listed: nothing to install.
			f.source.SetError(fmt.Errorf("%w: no", domain.ErrUnavailable))
			check()
			f.source.SetError(nil)
			check()
			// Unanswered again: what was listed before still stands.
			f.source.SetError(context.DeadlineExceeded)
			check()
			// A throttled check asks nothing, and is told nothing.
			if _, err := updates.Check(t.Context(), f.alice); !errors.Is(err, domain.ErrTooManyRequests) {
				t.Fatalf("Check = %v", err)
			}
			want := []told{{app.CheckErrorUnreachable, false}, {"", true}, {app.CheckErrorTimeout, true}}
			if !slices.Equal(got, want) {
				t.Errorf("told %+v, want %+v", got, want)
			}
		},
		"a class a classifier invents is unreachable": func(t *testing.T, f *updateFixture) {
			f.cfg.Classify = func(error) string { return "something the source said" }
			updates, err := app.NewUpdateService(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			f.source.SetError(errors.New("no"))
			view, err := updates.Check(t.Context(), f.alice)
			if err != nil || view.LastError != app.CheckErrorUnreachable {
				t.Errorf("Check = %+v, %v", view, err)
			}
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t, newUpdateFixture(t, release.ModeBinary, nil, nil))
		})
	}
}

func TestUpdatesStart(t *testing.T) {
	t.Parallel()
	type outcome struct {
		err error
		// reason is the refusal of release.Allowed, if any.
		reason release.Reason
		// started says whether the upgrader was to be called.
		started bool
	}
	tests := map[string]struct {
		mode      release.Mode
		tuneFiles func(map[string][]byte)
		tune      func(*app.UpdateServiceConfig)
		// noCheck leaves the cache empty.
		noCheck bool
		// call starts an upgrade, by default alice's to 0.7.1 with her
		// password.
		call func(t *testing.T, f *updateFixture) (release.Progress, error)
		want outcome
		then func(t *testing.T, f *updateFixture, progress release.Progress)
	}{
		"a binary, verified, is installed": {
			mode: release.ModeBinary, want: outcome{started: true},
			then: func(t *testing.T, f *updateFixture, progress release.Progress) {
				if !progress.State.Active() || progress.Target.String() != "0.7.1" || progress.StartedBy != "user/alice" || len(progress.Nodes) != 1 {
					t.Errorf("progress = %+v", progress)
				}
				records := f.records(t, iam.ActionUpgradeCluster)
				sum := sha256.Sum256([]byte("binary 0.7.1"))
				if len(records) != 1 || records[0].Outcome != audit.Succeeded || records[0].Details["from"] != "0.7.0" ||
					records[0].Details["to"] != "0.7.1" || records[0].Details["digest"] != fmt.Sprintf("sha256:%x", sum) || records[0].Details["mode"] != "binary" {
					t.Fatalf("records = %+v", records)
				}
				if f.source.Fetches(assetURL("0.7.1", release.DigestsAsset)) != 0 {
					t.Error("a binary upgrade fetched the digests of the image")
				}
				// The end of the upgrade is reported and audited.
				f.upgrader.Finish()
				reported := f.reported(t, 3)
				if len(reported) != 3 || reported[2].State != release.StateDone {
					t.Fatalf("reported = %+v", reported)
				}
				deadline := time.Now().Add(10 * time.Second)
				for len(f.records(t, iam.ActionUpgradeCluster)) < 2 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				records = f.records(t, iam.ActionUpgradeCluster)
				if len(records) != 2 || records[1].Details["state"] != "done" || records[1].Outcome != audit.Succeeded {
					t.Errorf("records = %+v", records)
				}
				progress, err := f.updates.Progress(t.Context(), f.alice)
				if err != nil || progress.State != release.StateDone {
					t.Errorf("Progress = %+v, %v", progress, err)
				}
			},
		},
		"an image, verified, is installed": {
			mode: release.ModeKubernetes, want: outcome{started: true},
			then: func(t *testing.T, f *updateFixture, progress release.Progress) {
				records := f.records(t, iam.ActionUpgradeCluster)
				list := sha256.Sum256([]byte("list 0.7.1"))
				if progress.Target.String() != "0.7.1" || len(records) != 1 || records[0].Details["digest"] != fmt.Sprintf("sha256:%x", list) {
					t.Errorf("progress = %+v, records = %+v", progress, records)
				}
				if f.source.Fetches(assetURL("0.7.1", release.BinaryAsset("linux", "amd64"))) != 0 {
					t.Error("an image upgrade fetched the binary")
				}
			},
		},
		"the newest patch of the next minor": {
			mode: release.ModeBinary, want: outcome{started: true},
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				return f.updates.Start(t.Context(), f.alice, f.token, f.password, "v0.8.1")
			},
		},
		"an upgrade that fails is audited as failed": {
			mode: release.ModeKubernetes, want: outcome{started: true},
			tune: func(c *app.UpdateServiceConfig) {
				c.Upgrader = porttest.NewUpgrader(porttest.UpgraderConfig{Mode: release.ModeKubernetes, Nodes: []string{"node-1"}, Outcome: release.StateFailed})
			},
			then: func(t *testing.T, f *updateFixture, _ release.Progress) {
				f.upgrader.Finish()
				if reported := f.reported(t, 3); reported[len(reported)-1].State != release.StateFailed {
					t.Fatalf("reported = %+v", reported)
				}
				deadline := time.Now().Add(10 * time.Second)
				for len(f.records(t, iam.ActionUpgradeCluster)) < 2 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				records := f.records(t, iam.ActionUpgradeCluster)
				if len(records) != 2 || records[1].Outcome != audit.Failed || records[1].Details["state"] != "failed" {
					t.Errorf("records = %+v", records)
				}
			},
		},
		"no source": {
			mode: release.ModeBinary, tune: func(c *app.UpdateServiceConfig) { c.Source = nil }, noCheck: true,
			want: outcome{err: domain.ErrUpdatesOff},
		},
		"not allowed to": {
			mode: release.ModeBinary,
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				auditor := f.alice
				auditor.User, auditor.Policies = "audrey", nil
				return f.updates.Start(t.Context(), auditor, f.token, f.password, "0.7.1")
			},
			want: outcome{err: domain.ErrAccessDenied},
			then: func(t *testing.T, f *updateFixture, _ release.Progress) {
				records := f.records(t, iam.ActionUpgradeCluster)
				if len(records) != 1 || records[0].Outcome != audit.Denied || records[0].Actor != "user/audrey" {
					t.Errorf("records = %+v", records)
				}
			},
		},
		"an API key of a cluster-admin": {
			mode: release.ModeBinary,
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				key := f.as(t, f.user(t, "kim", iam.RoleClusterAdmin))
				return f.updates.Start(t.Context(), key, f.token, f.password, "0.7.1")
			},
			want: outcome{err: domain.ErrAccessDenied},
			then: func(t *testing.T, f *updateFixture, _ release.Progress) {
				records := f.records(t, iam.ActionUpgradeCluster)
				if len(records) != 1 || records[0].Outcome != audit.Denied || records[0].Details["reason"] != "an API key cannot install" {
					t.Errorf("records = %+v", records)
				}
			},
		},
		"root": {
			mode: release.ModeBinary,
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				return f.updates.Start(t.Context(), f.root, f.token, f.password, "0.7.1")
			},
			want: outcome{err: domain.ErrAccessDenied},
		},
		"a locked session": {
			mode: release.ModeBinary,
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				locked := f.alice
				locked.LockedSession = true
				return f.updates.Start(t.Context(), locked, f.token, f.password, "0.7.1")
			},
			want: outcome{err: domain.ErrSessionLocked},
		},
		"a wrong password": {
			mode: release.ModeBinary,
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				return f.updates.Start(t.Context(), f.alice, f.token, "not the password", "0.7.1")
			},
			want: outcome{err: domain.ErrAccessDenied},
			then: func(t *testing.T, f *updateFixture, _ release.Progress) {
				failed := f.records(t, app.ActionConfirmPassword)
				if len(failed) != 1 || failed[0].Outcome != audit.Failed || f.verifier.Calls() != 0 {
					t.Errorf("records = %+v, verifier called %d times", failed, f.verifier.Calls())
				}
				// The session is as it was.
				if _, err := f.sessions.Authenticate(t.Context(), f.token, "", false, f.alice.Session); err != nil {
					t.Errorf("the session after a wrong password: %v", err)
				}
			},
		},
		"wrong passwords end the session as at an unlock": {
			mode: release.ModeBinary,
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				for i := 1; i < app.UnlockAttempts; i++ {
					if _, err := f.updates.Start(t.Context(), f.alice, f.token, fmt.Sprintf("guess %d", i), "0.7.1"); !errors.Is(err, domain.ErrAccessDenied) {
						t.Fatalf("guess %d = %v", i, err)
					}
				}
				return f.updates.Start(t.Context(), f.alice, f.token, "the last guess", "0.7.1")
			},
			want: outcome{err: domain.ErrTooManyAttempts},
			then: func(t *testing.T, f *updateFixture, _ release.Progress) {
				if _, err := f.sessions.Authenticate(t.Context(), f.token, "", false, f.alice.Session); !errors.Is(err, domain.ErrAccessDenied) {
					t.Errorf("the session after five wrong passwords: %v", err)
				}
				if _, err := f.updates.Start(t.Context(), f.alice, f.token, f.password, "0.7.1"); !errors.Is(err, domain.ErrCredentialNotFound) {
					t.Errorf("the right password for the ended session = %v", err)
				}
			},
		},
		"the password of another session": {
			mode: release.ModeBinary,
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				other := strings.Join([]string{"bob's", "own", "phrase"}, " ")
				if _, err := f.iam.CreateUser(t.Context(), f.root, app.UserInput{Name: "bob", Roles: []string{iam.RoleReadOnly}, Password: other}); err != nil {
					t.Fatal(err)
				}
				signed, err := f.sessions.SignIn(t.Context(), "bob", other, session("198.51.100.30"))
				if err != nil {
					t.Fatal(err)
				}
				return f.updates.Start(t.Context(), f.alice, signed.Token, other, "0.7.1")
			},
			want: outcome{err: domain.ErrAccessDenied},
		},
		"not a version": {
			mode: release.ModeBinary,
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				return f.updates.Start(t.Context(), f.alice, f.token, f.password, "latest")
			},
			want: outcome{err: domain.ErrInvalidArgument},
		},
		"older":                     {mode: release.ModeBinary, call: startTo("0.6.0"), want: outcome{err: domain.ErrUpgradeNotAllowed, reason: release.ReasonOlder}},
		"equal":                     {mode: release.ModeBinary, call: startTo("0.7.0"), want: outcome{err: domain.ErrUpgradeNotAllowed, reason: release.ReasonEqual}},
		"major":                     {mode: release.ModeBinary, call: startTo("1.0.0"), want: outcome{err: domain.ErrUpgradeNotAllowed, reason: release.ReasonMajor}},
		"a skipped minor":           {mode: release.ModeBinary, call: startTo("0.9.0"), want: outcome{err: domain.ErrUpgradeNotAllowed, reason: release.ReasonSkippedMinor}},
		"not the newest patch":      {mode: release.ModeBinary, call: startTo("0.8.0"), want: outcome{err: domain.ErrUpgradeNotAllowed, reason: release.ReasonNotNewestPatch}},
		"a pre-release on stable":   {mode: release.ModeBinary, call: startTo("0.8.2-rc.1"), want: outcome{err: domain.ErrUpgradeNotAllowed, reason: release.ReasonPrereleaseOnStable}},
		"not in the cache":          {mode: release.ModeBinary, call: startTo("0.7.5"), want: outcome{err: domain.ErrUpgradeNotAllowed, reason: release.ReasonUnknownRelease}},
		"never checked":             {mode: release.ModeBinary, noCheck: true, want: outcome{err: domain.ErrUpgradeNotAllowed, reason: release.ReasonUnknownRelease}},
		"upgrade-mode off":          {mode: release.ModeOff, want: outcome{err: domain.ErrUpgradeUnavailable}},
		"a platform without binary": {mode: release.ModeBinary, tune: func(c *app.UpdateServiceConfig) { c.GOOS = "plan9" }, want: outcome{err: domain.ErrUpgradeUnavailable}},
		"a bundle of another version": {
			mode: release.ModeBinary, want: outcome{err: domain.ErrReleaseNotVerified},
			tuneFiles: func(files map[string][]byte) {
				files[assetURL("0.7.1", release.SumsBundleAsset)] = porttest.FakeBundle(mustVersion(t, "0.7.2"), files[assetURL("0.7.1", release.SumsAsset)])
			},
		},
		"a tampered binary": {
			mode: release.ModeBinary, want: outcome{err: domain.ErrReleaseNotVerified},
			tuneFiles: func(files map[string][]byte) {
				files[assetURL("0.7.1", release.BinaryAsset("linux", "amd64"))] = []byte("binary 0.7.1, changed")
			},
		},
		"a binary larger than any": {
			mode: release.ModeBinary, want: outcome{err: domain.ErrReleaseNotVerified},
			tuneFiles: func(files map[string][]byte) {
				files[assetURL("0.7.1", release.BinaryAsset("linux", "amd64"))] = make([]byte, app.MaxReleaseBinarySize+1)
			},
		},
		"a tampered digests.txt": {
			mode: release.ModeKubernetes, want: outcome{err: domain.ErrReleaseNotVerified},
			tuneFiles: func(files map[string][]byte) {
				url := assetURL("0.7.1", release.DigestsAsset)
				files[url] = append(files[url], []byte("registry.example/cactusfs:0.7.1 linux/arm64 sha256:"+strings.Repeat("0", 64)+"\n")...)
			},
		},
		"a release without its bundle": {
			mode: release.ModeKubernetes, want: outcome{err: domain.ErrReleaseNotVerified},
			tuneFiles: func(files map[string][]byte) {
				delete(files, assetURL("0.7.1", release.DigestsBundleAsset))
			},
		},
		"a release without the binary of the platform": {
			mode: release.ModeBinary, want: outcome{err: domain.ErrReleaseNotVerified},
			tuneFiles: func(files map[string][]byte) {
				delete(files, assetURL("0.7.1", release.BinaryAsset("linux", "amd64")))
			},
		},
		"a source that cannot be reached for a file": {
			mode: release.ModeBinary, want: outcome{err: domain.ErrUnavailable},
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				f.source.SetError(domain.ErrUnavailable)
				return f.updates.Start(t.Context(), f.alice, f.token, f.password, "0.7.1")
			},
		},
		"one upgrade at a time": {
			mode: release.ModeBinary, want: outcome{err: domain.ErrUpgradeRunning, started: true},
			call: func(t *testing.T, f *updateFixture) (release.Progress, error) {
				if _, err := f.updates.Start(t.Context(), f.alice, f.token, f.password, "0.7.1"); err != nil {
					t.Fatalf("the first start: %v", err)
				}
				return f.updates.Start(t.Context(), f.alice, f.token, f.password, "0.7.2")
			},
			then: func(t *testing.T, f *updateFixture, _ release.Progress) {
				// Once the first has ended, another may begin.
				f.upgrader.Finish()
				f.reported(t, 3)
				if _, err := f.updates.Start(t.Context(), f.alice, f.token, f.password, "0.7.2"); err != nil {
					t.Errorf("a start after the first ended = %v", err)
				}
				if f.upgrader.Starts() != 2 {
					t.Errorf("the upgrader was started %d times", f.upgrader.Starts())
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpdateFixture(t, tt.mode, tt.tuneFiles, tt.tune)
			if !tt.noCheck {
				f.checked(t)
			}
			call := tt.call
			if call == nil {
				call = startTo("0.7.1")
			}
			progress, err := call(t, f)
			switch {
			case tt.want.err == nil && err != nil:
				t.Fatalf("Start = %v", err)
			case tt.want.err != nil && !errors.Is(err, tt.want.err):
				t.Fatalf("Start = %v, want %v", err, tt.want.err)
			case release.ReasonOf(err) != tt.want.reason:
				t.Fatalf("Start = %v, want the refusal %q", err, tt.want.reason)
			}
			if started := f.upgrader.Starts() > 0; started != tt.want.started {
				t.Fatalf("the upgrader was started: %v, want %v (%v)", started, tt.want.started, err)
			}
			if tt.want.err != nil && !tt.want.started && f.upgrader.Starts() != 0 {
				t.Fatal("the upgrader was called for a refusal")
			}
			if errors.Is(err, domain.ErrUpgradeNotAllowed) || errors.Is(err, domain.ErrReleaseNotVerified) || errors.Is(err, domain.ErrUpgradeUnavailable) {
				// A refusal from the password on is audited, with the
				// versions.
				records := f.records(t, iam.ActionUpgradeCluster)
				if len(records) != 1 || records[0].Outcome != audit.Failed || records[0].Details["from"] != "0.7.0" || records[0].Details["reason"] == "" {
					t.Errorf("records = %+v", records)
				}
			}
			if tt.then != nil {
				tt.then(t, f, progress)
			}
			// Whatever happened: no password is in the trail or the log.
			for _, r := range f.trail(t) {
				if strings.Contains(fmt.Sprint(r), f.password) {
					t.Fatal("a password is in the audit trail")
				}
			}
			if strings.Contains(f.logs.String(), f.password) {
				t.Fatal("a password is in the log")
			}
			// Let a run under way end with the test.
			f.upgrader.Finish()
		})
	}
}

// startTo starts alice's upgrade to a version, with her password.
func startTo(version string) func(t *testing.T, f *updateFixture) (release.Progress, error) {
	return func(t *testing.T, f *updateFixture) (release.Progress, error) {
		return f.updates.Start(t.Context(), f.alice, f.token, f.password, version)
	}
}

// TestUpdatesStartOnlyOneIsVerifiedAtATime: two starts at the same moment
// install at most one release.
func TestUpdatesStartOnlyOneIsVerifiedAtATime(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t, release.ModeBinary, nil, nil)
	f.checked(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = f.updates.Start(t.Context(), f.alice, f.token, f.password, "0.7.1")
		})
	}
	wg.Wait()
	started := 0
	for _, err := range errs {
		switch {
		case err == nil:
			started++
		case !errors.Is(err, domain.ErrUpgradeRunning):
			t.Errorf("a start at the same time = %v", err)
		}
	}
	if started != 1 || f.upgrader.Starts() != 1 {
		t.Errorf("%d starts succeeded and the upgrader was started %d times", started, f.upgrader.Starts())
	}
	f.upgrader.Finish()
}

// TestUpdatesActionsAreAskedOfTheUpdatesResource: reading, checking,
// installing and the progress are all asked of the one resource iam
// documents, arn:cactusfs:admin:::updates - a Deny of it refuses each of
// them whatever else is allowed, and a grant of it alone allows each.
func TestUpdatesActionsAreAskedOfTheUpdatesResource(t *testing.T) {
	t.Parallel()
	policy := func(t *testing.T, document string) iam.Document {
		t.Helper()
		parsed, err := iam.ParsePolicy([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	const (
		allowAll     = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"admin:*","Resource":"*"}]}`
		denyUpdates  = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"admin:*","Resource":"arn:cactusfs:admin:::updates"}]}`
		readUpdates  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"admin:GetUpdates","Resource":"arn:cactusfs:admin:::updates"}]}`
		ownsUpdates  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["admin:GetUpdates","admin:CheckForUpdates","admin:UpgradeCluster"],"Resource":"arn:cactusfs:admin:::updates"}]}`
		upgradeOther = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["admin:GetUpdates","admin:UpgradeCluster"],"Resource":"arn:cactusfs:admin:::upgrade"}]}`
	)
	call := map[string]func(t *testing.T, f *updateFixture, p iam.Principal) error{
		"Get": func(t *testing.T, f *updateFixture, p iam.Principal) error {
			_, err := f.updates.Get(t.Context(), p)
			return err
		},
		"Check": func(t *testing.T, f *updateFixture, p iam.Principal) error {
			_, err := f.updates.Check(t.Context(), p)
			return err
		},
		"Progress": func(t *testing.T, f *updateFixture, p iam.Principal) error {
			_, err := f.updates.Progress(t.Context(), p)
			return err
		},
		"Start": func(t *testing.T, f *updateFixture, p iam.Principal) error {
			f.checked(t)
			_, err := f.updates.Start(t.Context(), p, f.token, f.password, "0.7.1")
			if err == nil {
				f.upgrader.Finish()
			}
			return err
		},
	}
	tests := []struct {
		name      string
		documents []string
		method    string
		want      error
	}{
		{"a Deny of updates refuses Get under admin:* on *", []string{allowAll, denyUpdates}, "Get", domain.ErrAccessDenied},
		{"a Deny of updates refuses Check under admin:* on *", []string{allowAll, denyUpdates}, "Check", domain.ErrAccessDenied},
		{"a Deny of updates refuses Progress under admin:* on *", []string{allowAll, denyUpdates}, "Progress", domain.ErrAccessDenied},
		{"a Deny of updates refuses Start under admin:* on *", []string{allowAll, denyUpdates}, "Start", domain.ErrAccessDenied},
		{"GetUpdates on updates alone allows Progress", []string{readUpdates}, "Progress", nil},
		{"GetUpdates on updates alone allows Get", []string{readUpdates}, "Get", nil},
		{"GetUpdates on updates alone does not allow Start", []string{readUpdates}, "Start", domain.ErrAccessDenied},
		{"the three actions on updates allow Start", []string{ownsUpdates}, "Start", nil},
		{"the three actions on updates allow Check", []string{ownsUpdates}, "Check", nil},
		{"a resource upgrade is not the one asked: Progress", []string{upgradeOther}, "Progress", domain.ErrAccessDenied},
		{"a resource upgrade is not the one asked: Start", []string{upgradeOther}, "Start", domain.ErrAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newUpdateFixture(t, release.ModeBinary, nil, nil)
			p := f.alice
			p.Policies = nil
			for _, d := range tt.documents {
				p.Policies = append(p.Policies, policy(t, d))
			}
			err := call[tt.method](t, f, p)
			if tt.want == nil && err != nil {
				t.Fatalf("%s = %v, want allowed", tt.method, err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("%s = %v, want %v", tt.method, err, tt.want)
			}
			if tt.method == "Start" && tt.want != nil && f.upgrader.Starts() != 0 {
				t.Error("the upgrader was started")
			}
		})
	}
}

// failingProgress is an upgrader whose progress cannot be read.
type failingProgress struct {
	ports.Upgrader
}

func (failingProgress) Progress(context.Context) (release.Progress, error) {
	return release.Progress{}, fmt.Errorf("%w: the cluster cannot be read", domain.ErrUnavailable)
}

func TestUpdatesProgressThatCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t, release.ModeBinary, nil, func(c *app.UpdateServiceConfig) {
		c.Upgrader = failingProgress{Upgrader: c.Upgrader}
	})
	if _, err := f.updates.Progress(t.Context(), f.alice); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Progress = %v", err)
	}
	f.checked(t)
	if _, err := f.updates.Start(t.Context(), f.alice, f.token, f.password, "0.7.1"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Start = %v", err)
	}
	if f.upgrader.Starts() != 0 {
		t.Error("the upgrader was started without knowing whether one runs")
	}
}

// fakeTimer is the After of a test: it hands out a channel per wait and
// notes how long each was.
type fakeTimer struct {
	mu    sync.Mutex
	waits []time.Duration
	ch    chan time.Time
}

func (ft *fakeTimer) after(d time.Duration) <-chan time.Time {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.waits = append(ft.waits, d)
	return ft.ch
}

func (ft *fakeTimer) asked() []time.Duration {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return slices.Clone(ft.waits)
}

func TestUpdatesRun(t *testing.T) {
	t.Parallel()
	t.Run("asks at every tick, spread by a tenth", func(t *testing.T) {
		t.Parallel()
		timer := &fakeTimer{ch: make(chan time.Time)}
		f := newUpdateFixture(t, release.ModeBinary, nil, func(c *app.UpdateServiceConfig) { c.After = timer.after })
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.updates.Run(ctx)
		}()
		for range 3 {
			timer.ch <- testNow
			f.clock.Advance(24 * time.Hour)
		}
		// The third tick was taken; wait for the fourth wait to be asked
		// for, which comes after the third check.
		deadline := time.Now().Add(10 * time.Second)
		for len(timer.asked()) < 4 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		cancel()
		<-done
		if f.source.asked.Load() != 3 {
			t.Errorf("the source was asked %d times in three ticks", f.source.asked.Load())
		}
		waits := timer.asked()
		if len(waits) != 4 {
			t.Fatalf("waits = %v", waits)
		}
		within := func(d, of time.Duration) bool { return d >= of-of/10 && d <= of+of/10 }
		if !within(waits[0], 5*time.Minute) {
			t.Errorf("the first wait = %v, want 5m give or take a tenth", waits[0])
		}
		for _, w := range waits[1:] {
			if !within(w, 24*time.Hour) {
				t.Errorf("a wait = %v, want 24h give or take a tenth", w)
			}
		}
		// What it found is known without asking again.
		view, err := f.updates.Get(t.Context(), f.alice)
		if err != nil || view.NextStep.String() != "0.8.1" {
			t.Errorf("Get = %+v, %v", view, err)
		}
		// A periodic check is not audited as somebody's.
		if records := f.records(t, iam.ActionCheckForUpdates); len(records) != 0 {
			t.Errorf("records = %+v", records)
		}
	})
	t.Run("a check asked for right after a periodic one waits", func(t *testing.T) {
		t.Parallel()
		timer := &fakeTimer{ch: make(chan time.Time)}
		f := newUpdateFixture(t, release.ModeBinary, nil, func(c *app.UpdateServiceConfig) { c.After = timer.after })
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.updates.Run(ctx)
		}()
		timer.ch <- testNow
		deadline := time.Now().Add(10 * time.Second)
		for len(timer.asked()) < 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		cancel()
		<-done
		if _, err := f.updates.Check(t.Context(), f.alice); !errors.Is(err, domain.ErrTooManyRequests) {
			t.Errorf("Check right after a periodic check = %v", err)
		}
	})
	for name, tune := range map[string]func(*app.UpdateServiceConfig){
		"no source":   func(c *app.UpdateServiceConfig) { c.Source = nil },
		"no interval": func(c *app.UpdateServiceConfig) { c.Interval = 0 },
	} {
		t.Run(name+": returns at once", func(t *testing.T) {
			t.Parallel()
			f := newUpdateFixture(t, release.ModeBinary, nil, tune)
			f.updates.Run(t.Context())
			if f.source.asked.Load() != 0 {
				t.Error("the source was asked")
			}
		})
	}
	t.Run("ends with its context", func(t *testing.T) {
		t.Parallel()
		f := newUpdateFixture(t, release.ModeBinary, nil, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		f.updates.Run(ctx)
	})
	t.Run("without randomness the interval is kept", func(t *testing.T) {
		t.Parallel()
		timer := &fakeTimer{ch: make(chan time.Time)}
		f := newUpdateFixture(t, release.ModeBinary, nil, func(c *app.UpdateServiceConfig) {
			c.After, c.Entropy = timer.after, failingReader{}
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.updates.Run(ctx)
		}()
		deadline := time.Now().Add(10 * time.Second)
		for len(timer.asked()) < 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		cancel()
		<-done
		if waits := timer.asked(); len(waits) != 1 || waits[0] != 5*time.Minute {
			t.Errorf("waits = %v", waits)
		}
	})
}

// failingReader is a source of randomness that has none.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no randomness") }
