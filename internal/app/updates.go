package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// The bounds of checking for releases and of installing one (ADR-0029).
const (
	// CheckThrottle is how often a node asks the update source when it is
	// asked to, at most: once in this time, whoever asks.
	CheckThrottle = time.Minute
	// MinUpdateCheckInterval is the shortest interval of the periodic
	// check a node accepts; zero turns the periodic check off.
	MinUpdateCheckInterval = CheckThrottle
	// MaxReleaseBinarySize is the largest binary of a release a node
	// fetches (the amendment of ADR-0023 by ADR-0029).
	MaxReleaseBinarySize = 256 << 20
	// MaxReleaseBundleSize is the largest Sigstore bundle fetched; the
	// verifier parses none larger.
	MaxReleaseBundleSize = 1 << 20
	// updateCheckTimeout bounds one asking of the source.
	updateCheckTimeout = time.Minute
	// firstCheckDelay is how long after the node started the periodic
	// check first asks, at most: soon enough to be of use, late enough
	// that a node that keeps restarting does not ask at every start.
	firstCheckDelay = 5 * time.Minute
	// upgradeStartTimeout bounds the start of an upgrade once the
	// release has verified; the upgrade itself goes on after it.
	upgradeStartTimeout = 2 * time.Minute
)

// The classes of a check that could not be read (UpdatesView.LastError).
// They are part of the management API's contract: never change one, only
// add.
const (
	CheckErrorUnreachable   = "unreachable"
	CheckErrorRateLimited   = "rate-limited"
	CheckErrorRefused       = "refused"
	CheckErrorInvalidAnswer = "invalid-answer"
	CheckErrorTimeout       = "timeout"
)

// checkErrorClasses are the classes a classifier may give.
var checkErrorClasses = []string{
	CheckErrorUnreachable, CheckErrorRateLimited, CheckErrorRefused, CheckErrorInvalidAnswer, CheckErrorTimeout,
}

// PasswordConfirmer judges a password typed again into a console session:
// SessionService.Confirm.
type PasswordConfirmer interface {
	// Confirm returns the user the session of token belongs to when
	// password is theirs; see SessionService.Confirm for the errors.
	Confirm(ctx context.Context, token, password string, session iam.Session) (string, error)
}

// UpdateServiceConfig lists what an UpdateService depends on.
type UpdateServiceConfig struct {
	// Source is the update source; nil when update-source is not set,
	// and then nothing is ever asked of anyone.
	Source ports.ReleaseSource
	// Verifier verifies the signed lists of a release.
	Verifier ports.ReleaseVerifier
	// Upgrader installs a release: one that says it cannot when
	// upgrade-mode is off.
	Upgrader ports.Upgrader
	Authz    ports.Authorizer
	Auditor  *Auditor
	Clock    ports.Clock
	// Sessions judges the password typed with an upgrade.
	Sessions PasswordConfirmer
	// Current is the running release; Build and Commit are those of the
	// build (Commit may be empty).
	Current release.Version
	Build   string
	Commit  string
	// Channel is which releases are offered.
	Channel release.Channel
	// GOOS and GOARCH name the platform whose binary is installed in
	// release.ModeBinary.
	GOOS   string
	GOARCH string
	// Interval is how often the node asks the source by itself, give or
	// take a tenth; zero never. It is MinUpdateCheckInterval or more.
	Interval time.Duration
	// Classify names the class of an error of the source (the Check*
	// constants); a class it does not know is read as unreachable. When
	// nil, errors are classed by the domain sentinels they wrap.
	Classify func(error) string
	// Entropy spreads the periodic checks of the nodes of a deployment.
	Entropy io.Reader
	// After, when not nil, replaces time.After between two periodic
	// checks.
	After func(time.Duration) <-chan time.Time
	// Lifetime is the context of the service: an upgrade is started under
	// it, not under the request that asked, and its end is audited under
	// it.
	Lifetime context.Context
	// OnProgress, when not nil, is told every progress an upgrade
	// reports, from the upgrader's goroutine.
	OnProgress func(release.Progress)
	// OnCheck, when not nil, is told of every asking of the source, asked
	// for or periodic, once it has ended: the class of its error ("" when
	// the source answered) and whether a release may be installed now, by
	// what the node knows then.
	OnCheck func(class string, installable bool)
	Logger  *slog.Logger
}

// AvailableRelease is a release the source listed, and whether it may be
// installed from the running one.
type AvailableRelease struct {
	release.Release
	// Allowed says whether it may be installed; Refusal says why not.
	Allowed bool
	Refusal release.Reason
}

// UpdatesView is what a node knows of releases. Nothing of it is stored or
// shared with another node, and nothing in it names the update source.
type UpdatesView struct {
	// Current, Build and Commit are the running build.
	Current release.Version
	Build   string
	Commit  string
	// SourceConfigured says whether update-source is set.
	SourceConfigured bool
	Channel          release.Channel
	// CheckedAt is when the source was last asked, answered or not; zero
	// when it was not since the node started.
	CheckedAt time.Time
	// LastError is the class of why the last check could not be read;
	// empty when it was read.
	LastError string
	// Available are the releases newer than Current that the channel
	// offers, newest first, as the source last listed them.
	Available []AvailableRelease
	// NextStep is the newest of Available that may be installed; zero
	// when none may.
	NextStep release.Version
	// Mode is how the deployment installs a release.
	Mode release.Mode
	// UpgradeAvailable says whether an upgrade can be started here;
	// UnavailableReason says why not, empty when it can.
	UpgradeAvailable  bool
	UnavailableReason string
	// Instructions are what an operator does to install NextStep without
	// the console; empty when there is nothing to install.
	Instructions []string
}

// CheckThrottledError is the refusal of a check asked for less than
// CheckThrottle after the last one. It wraps domain.ErrTooManyRequests.
type CheckThrottledError struct {
	// RetryAfter is how long until a check is asked again; at least a
	// second.
	RetryAfter time.Duration
}

// Error implements error.
func (e *CheckThrottledError) Error() string {
	return fmt.Sprintf("%v: the node asked for releases less than %s ago", domain.ErrTooManyRequests, CheckThrottle)
}

// Unwrap returns domain.ErrTooManyRequests.
func (e *CheckThrottledError) Unwrap() error { return domain.ErrTooManyRequests }

// UpdateService checks for releases and installs one (ADR-0029). What a
// check found is kept in this node's memory only.
type UpdateService struct {
	cfg UpdateServiceConfig

	mu sync.Mutex
	// releases are what the source listed when it last answered.
	releases []release.Release
	// checkedAt is when it was last asked; asked when it was last asked
	// or is being asked now, which the throttle counts from.
	checkedAt time.Time
	asked     time.Time
	lastError string
	// starting is set while an upgrade is being verified and started.
	starting bool
}

// NewUpdateService returns an UpdateService. The error wraps
// domain.ErrInvalidArgument when a dependency other than Source, Classify,
// After and OnProgress is missing, or a setting is out of bounds.
func NewUpdateService(cfg UpdateServiceConfig) (*UpdateService, error) {
	if cfg.Verifier == nil || cfg.Upgrader == nil || cfg.Authz == nil || cfg.Auditor == nil || cfg.Clock == nil ||
		cfg.Sessions == nil || cfg.Entropy == nil || cfg.Lifetime == nil || cfg.Logger == nil {
		return nil, fmt.Errorf("app.NewUpdateService: %w: a dependency is missing", domain.ErrInvalidArgument)
	}
	if cfg.Current.IsZero() {
		return nil, fmt.Errorf("app.NewUpdateService: %w: the running version is not known", domain.ErrInvalidArgument)
	}
	if _, err := release.ParseChannel(string(cfg.Channel)); err != nil {
		return nil, fmt.Errorf("app.NewUpdateService: %w", err)
	}
	if cfg.Interval < 0 || (cfg.Interval > 0 && cfg.Interval < MinUpdateCheckInterval) {
		return nil, fmt.Errorf("app.NewUpdateService: %w: the update check interval is 0 or at least %s", domain.ErrInvalidArgument, MinUpdateCheckInterval)
	}
	if cfg.Classify == nil {
		cfg.Classify = classifyCheckError
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	return &UpdateService{cfg: cfg}, nil
}

// classifyCheckError classes an error of the source by the sentinels the
// port promises.
func classifyCheckError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return CheckErrorTimeout
	case errors.Is(err, domain.ErrUnavailable):
		return CheckErrorUnreachable
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrEntityTooLarge):
		return CheckErrorInvalidAnswer
	default:
		return CheckErrorRefused
	}
}

// updatesResource is what every action of releases is asked of - reading,
// checking and installing, the progress included: the one resource
// "updates" (arn:cactusfs:admin:::updates) that iam documents, so that a
// policy that denies it, or grants it alone, means all three.
var updatesResource = iam.Resource{Admin: "updates"}

// Get returns what this node knows of releases. The caller must be allowed
// admin:GetUpdates. It is answered from memory: the source is never asked.
func (s *UpdateService) Get(ctx context.Context, p iam.Principal) (UpdatesView, error) {
	if err := s.cfg.Authz.Authorize(ctx, iam.Request{Principal: p, Action: iam.ActionGetUpdates, Resource: updatesResource}); err != nil {
		return UpdatesView{}, fmt.Errorf("update.Get: %w", err)
	}
	return s.view(ctx), nil
}

// view is what the node knows now.
func (s *UpdateService) view(ctx context.Context) UpdatesView {
	s.mu.Lock()
	releases := slices.Clone(s.releases)
	view := UpdatesView{
		Current: s.cfg.Current, Build: s.cfg.Build, Commit: s.cfg.Commit, SourceConfigured: s.cfg.Source != nil,
		Channel: s.cfg.Channel, CheckedAt: s.checkedAt, LastError: s.lastError, Mode: s.cfg.Upgrader.Mode(),
	}
	s.mu.Unlock()
	for _, r := range release.Available(s.cfg.Current, releases, s.cfg.Channel) {
		refusal := release.Allowed(s.cfg.Current, r.Version, releases, s.cfg.Channel)
		view.Available = append(view.Available, AvailableRelease{Release: r, Allowed: refusal == nil, Refusal: release.ReasonOf(refusal)})
	}
	next, hasNext := release.NextStep(s.cfg.Current, releases, s.cfg.Channel)
	if hasNext {
		view.NextStep = next.Version
	}
	ok, reason, instructions := s.cfg.Upgrader.Availability(ctx)
	view.UpgradeAvailable = ok
	if !ok {
		view.UnavailableReason = reason
	}
	if hasNext {
		view.Instructions = instructions
	}
	return view
}

// Check asks the update source for its releases now and returns what the
// node knows then. The caller must be allowed admin:CheckForUpdates; every
// check is audited, whether or not the source answered - that it did not is
// in the view (LastError), not the error.
//
// The error wraps domain.ErrUpdatesOff when no source is configured, and is
// a *CheckThrottledError (domain.ErrTooManyRequests) when the node asked less
// than CheckThrottle ago; nothing was asked then.
func (s *UpdateService) Check(ctx context.Context, p iam.Principal) (UpdatesView, error) {
	if err := s.cfg.Authz.Authorize(ctx, iam.Request{Principal: p, Action: iam.ActionCheckForUpdates, Resource: updatesResource}); err != nil {
		return UpdatesView{}, fmt.Errorf("update.Check: %w", err)
	}
	if s.cfg.Source == nil {
		return UpdatesView{}, fmt.Errorf("update.Check: %w", domain.ErrUpdatesOff)
	}
	if wait := s.reserveCheck(); wait > 0 {
		return UpdatesView{}, fmt.Errorf("update.Check: %w", &CheckThrottledError{RetryAfter: wait})
	}
	count, class := s.check(ctx)
	details := map[string]string{"releases": fmt.Sprint(count)}
	if class != "" {
		details["error"] = class
	}
	s.cfg.Auditor.Succeeded(ctx, s.cfg.Auditor.Record(p, iam.ActionCheckForUpdates, updatesResource, audit.Succeeded, details))
	return s.view(ctx), nil
}

// reserveCheck takes the turn of a check asked for, and returns how long to
// wait when it is not this one's: zero when it is.
func (s *UpdateService) reserveCheck() time.Duration {
	now := s.cfg.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.asked.IsZero() {
		if wait := s.asked.Add(CheckThrottle).Sub(now); wait > 0 {
			return max(wait.Round(time.Second), time.Second)
		}
	}
	s.asked = now
	return 0
}

// check asks the source and keeps what it said; it returns how many
// releases it listed and the class of its error, "" when it answered. The
// releases of the last answer are kept when it does not.
func (s *UpdateService) check(ctx context.Context) (int, string) {
	askCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	releases, err := s.cfg.Source.Releases(askCtx)
	now := s.cfg.Clock.Now()
	class, count := "", len(releases)
	if err != nil {
		class, count = s.cfg.Classify(err), 0
		if !slices.Contains(checkErrorClasses, class) {
			class = CheckErrorUnreachable
		}
		// The source's error names it by its host at most (the port).
		s.cfg.Logger.WarnContext(ctx, "the update source could not be read", slog.String("class", class), slog.Any("error", err))
	}
	s.mu.Lock()
	s.checkedAt, s.lastError = now, class
	if err == nil {
		s.releases = releases
	}
	_, installable := release.NextStep(s.cfg.Current, s.releases, s.cfg.Channel)
	s.mu.Unlock()
	if s.cfg.OnCheck != nil {
		s.cfg.OnCheck(class, installable)
	}
	return count, class
}

// Run asks the source by itself, every Interval give or take a tenth, the
// first time after at most firstCheckDelay, until ctx ends. Without a source
// or an interval it returns at once.
func (s *UpdateService) Run(ctx context.Context) {
	if s.cfg.Source == nil || s.cfg.Interval == 0 {
		return
	}
	wait := min(s.cfg.Interval, firstCheckDelay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.cfg.After(s.jitter(wait)):
		}
		s.mu.Lock()
		s.asked = s.cfg.Clock.Now()
		s.mu.Unlock()
		if count, class := s.check(ctx); class == "" {
			s.cfg.Logger.InfoContext(ctx, "checked for releases", slog.Int("releases", count))
		}
		wait = s.cfg.Interval
	}
}

// jitter returns d moved by up to a tenth of it either way, so that the
// nodes of a deployment do not all ask at once.
func (s *UpdateService) jitter(d time.Duration) time.Duration {
	span := int64(d / 5)
	if span <= 0 {
		return d
	}
	offset, err := rand.Int(s.cfg.Entropy, big.NewInt(span+1))
	if err != nil {
		return d
	}
	return d - d/10 + time.Duration(offset.Int64())
}

// Progress returns how far the latest upgrade has come. The caller must be
// allowed admin:GetUpdates.
func (s *UpdateService) Progress(ctx context.Context, p iam.Principal) (release.Progress, error) {
	if err := s.cfg.Authz.Authorize(ctx, iam.Request{Principal: p, Action: iam.ActionGetUpdates, Resource: updatesResource}); err != nil {
		return release.Progress{}, fmt.Errorf("update.Progress: %w", err)
	}
	progress, err := s.cfg.Upgrader.Progress(ctx)
	if err != nil {
		return release.Progress{}, fmt.Errorf("update.Progress: %w", err)
	}
	return progress.Normalized(), nil
}

// Start begins installing version and returns the progress once it has
// begun. The caller must be allowed admin:UpgradeCluster, be the user of a
// console session that is not locked - an API key never installs, whatever
// it may - and have typed the password of that user again (password, for
// the session of sessionToken), which is judged here and nowhere kept.
//
// What is installed: a release the last check listed that release.Allowed
// admits, whose signed list verified for that very version and whose
// artifact is the one the list names - the binary of this platform, its
// SHA-256 computed here as it is fetched (release.ModeBinary), or the image
// (release.ModeKubernetes). The upgrader is given nothing else, and is
// called only once all of that held. Every start and every refusal from the
// password on is audited with the versions from and to; the end of the
// upgrade is audited too, when the upgrader reports it.
//
// The error wraps domain.ErrAccessDenied (not allowed, not a console user,
// a wrong password), domain.ErrSessionLocked, domain.ErrTooManyAttempts and
// domain.ErrCredentialNotFound (as SessionService.Confirm says),
// domain.ErrUpdatesOff, domain.ErrInvalidArgument (not a version),
// domain.ErrUpgradeNotAllowed (a *release.Refusal), domain.ErrUpgradeRunning,
// domain.ErrUpgradeUnavailable, domain.ErrReleaseNotVerified, and the
// errors of the source when a file could not be fetched.
func (s *UpdateService) Start(ctx context.Context, p iam.Principal, sessionToken, password, version string) (release.Progress, error) {
	req := iam.Request{Principal: p, Action: iam.ActionUpgradeCluster, Resource: updatesResource}
	if err := s.cfg.Authz.Authorize(ctx, req); err != nil {
		return release.Progress{}, fmt.Errorf("update.Start: %w", err)
	}
	if p.User == "" || p.AccessKeyID != "" || p.Root || p.NodeID != "" || p.Service != "" {
		// Only a person at the console can type a password again.
		const reason = "an API key cannot install"
		s.cfg.Auditor.Denied(ctx, req, reason)
		return release.Progress{}, fmt.Errorf("update.Start: %w: %s", domain.ErrAccessDenied, reason)
	}
	if s.cfg.Source == nil {
		return release.Progress{}, fmt.Errorf("update.Start: %w", domain.ErrUpdatesOff)
	}
	target, err := release.ParseVersion(version)
	if err != nil {
		return release.Progress{}, fmt.Errorf("update.Start: %w", err)
	}
	user, err := s.cfg.Sessions.Confirm(ctx, sessionToken, password, p.Session)
	if err != nil {
		return release.Progress{}, fmt.Errorf("update.Start: %w", err)
	}
	if user != p.User {
		// The session of the token is not the caller's: no password of
		// somebody else's session stands for this one.
		s.cfg.Auditor.Denied(ctx, req, "the password is of another session")
		return release.Progress{}, fmt.Errorf("update.Start: %w: the session is not the caller's", domain.ErrAccessDenied)
	}
	progress, verified, err := s.start(ctx, p, target)
	if err != nil {
		s.audit(ctx, p, target, verified, audit.Failed, startRefusal(err))
		return release.Progress{}, fmt.Errorf("update.Start: %w", err)
	}
	s.audit(ctx, p, target, verified, audit.Succeeded, "")
	return progress, nil
}

// start is Start once the caller has proved who they are.
func (s *UpdateService) start(ctx context.Context, p iam.Principal, target release.Version) (release.Progress, release.Verified, error) {
	s.mu.Lock()
	if s.starting {
		s.mu.Unlock()
		return release.Progress{}, release.Verified{}, fmt.Errorf("%w: another is being started", domain.ErrUpgradeRunning)
	}
	releases := slices.Clone(s.releases)
	s.starting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
	}()

	i := slices.IndexFunc(releases, func(r release.Release) bool { return r.Version == target })
	if i < 0 {
		return release.Progress{}, release.Verified{}, &release.Refusal{Reason: release.ReasonUnknownRelease, Current: s.cfg.Current, Target: target}
	}
	if err := release.Allowed(s.cfg.Current, target, releases, s.cfg.Channel); err != nil {
		return release.Progress{}, release.Verified{}, err
	}
	running, err := s.cfg.Upgrader.Progress(ctx)
	if err != nil {
		return release.Progress{}, release.Verified{}, err
	}
	if running.State.Active() {
		return release.Progress{}, release.Verified{}, fmt.Errorf("%w: to %s", domain.ErrUpgradeRunning, running.Target)
	}
	if ok, reason, _ := s.cfg.Upgrader.Availability(ctx); !ok {
		return release.Progress{}, release.Verified{}, fmt.Errorf("%w: %s", domain.ErrUpgradeUnavailable, reason)
	}
	verified, err := s.verify(ctx, releases[i])
	if err != nil {
		return release.Progress{}, release.Verified{}, err
	}

	// The upgrade outlives the request: it is started under a context the
	// caller going away does not end and the service ending does, and the
	// start is bounded by its own time.
	startedBy := actorOf(p)
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upgradeStartTimeout)
	defer cancel()
	go func(stopping <-chan struct{}) {
		select {
		case <-stopping:
			cancel()
		case <-startCtx.Done():
		}
	}(s.cfg.Lifetime.Done())
	if err := s.cfg.Upgrader.Start(startCtx, verified, startedBy, s.reporter(p, verified)); err != nil {
		return release.Progress{}, verified, err
	}
	progress, err := s.cfg.Upgrader.Progress(ctx)
	if err != nil || progress.State == release.StateIdle || progress.State == "" {
		progress = release.Progress{State: release.StateVerifying, Target: target, StartedBy: startedBy, StartedAt: s.cfg.Clock.Now()}
	}
	return progress.Normalized(), verified, nil
}

// verify fetches what the mode installs and holds it to the release's
// signed list, whose bundle must verify for the release's version first.
func (s *UpdateService) verify(ctx context.Context, r release.Release) (release.Verified, error) {
	switch mode := s.cfg.Upgrader.Mode(); mode {
	case release.ModeBinary:
		name := release.BinaryAsset(s.cfg.GOOS, s.cfg.GOARCH)
		if name == "" {
			return release.Verified{}, fmt.Errorf("%w: no release is built for %s/%s", domain.ErrUpgradeUnavailable, s.cfg.GOOS, s.cfg.GOARCH)
		}
		sums, err := s.signedList(ctx, r, release.SumsAsset, release.SumsBundleAsset)
		if err != nil {
			return release.Verified{}, err
		}
		asset, ok := r.Asset(name)
		if !ok {
			return release.Verified{}, fmt.Errorf("%w: the release has no %s", domain.ErrReleaseNotVerified, name)
		}
		// Hashed as it arrives and kept nowhere: what is installed is
		// fetched again by the upgrader, which installs it only if it is
		// these bytes (release.Verified.SHA256).
		hash := sha256.New()
		if err := s.fetch(ctx, asset, MaxReleaseBinarySize, hash); err != nil {
			return release.Verified{}, err
		}
		return release.NewVerifiedBinary(r.Version, sums, name, [32]byte(hash.Sum(nil)))
	case release.ModeKubernetes:
		digests, err := s.signedList(ctx, r, release.DigestsAsset, release.DigestsBundleAsset)
		if err != nil {
			return release.Verified{}, err
		}
		return release.NewVerifiedImage(r.Version, digests)
	default:
		return release.Verified{}, fmt.Errorf("%w: upgrade-mode is %s", domain.ErrUpgradeUnavailable, mode)
	}
}

// signedList fetches a signed list of a release and its bundle, and returns
// the list once the bundle verified for the release's version.
func (s *UpdateService) signedList(ctx context.Context, r release.Release, listName, bundleName string) ([]byte, error) {
	list, err := s.fetchNamed(ctx, r, listName, release.MaxListSize)
	if err != nil {
		return nil, err
	}
	bundle, err := s.fetchNamed(ctx, r, bundleName, MaxReleaseBundleSize)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Verifier.Verify(ctx, r.Version, list, bundle); err != nil {
		return nil, err
	}
	return list, nil
}

// fetchNamed fetches a file of a release by its name into memory.
func (s *UpdateService) fetchNamed(ctx context.Context, r release.Release, name string, limit int64) ([]byte, error) {
	asset, ok := r.Asset(name)
	if !ok {
		return nil, fmt.Errorf("%w: the release has no %s", domain.ErrReleaseNotVerified, name)
	}
	var buf bytes.Buffer
	if err := s.fetch(ctx, asset, limit, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// fetch fetches a file of a release. A file larger than it may be is not
// the release's: it does not verify.
func (s *UpdateService) fetch(ctx context.Context, asset release.Asset, limit int64, w io.Writer) error {
	err := s.cfg.Source.Fetch(ctx, asset, limit, w)
	if errors.Is(err, domain.ErrEntityTooLarge) {
		return fmt.Errorf("%w: %s is larger than %d bytes", domain.ErrReleaseNotVerified, asset.Name, limit)
	}
	if err != nil {
		return fmt.Errorf("fetch %s: %w", asset.Name, err)
	}
	return nil
}

// reporter returns what the upgrader reports the progress to: it passes it
// on, and audits the end of the upgrade once.
func (s *UpdateService) reporter(p iam.Principal, verified release.Verified) func(release.Progress) {
	var once sync.Once
	return func(progress release.Progress) {
		if s.cfg.OnProgress != nil {
			s.cfg.OnProgress(progress)
		}
		if !progress.State.Ended() {
			return
		}
		once.Do(func() {
			outcome := audit.Succeeded
			if progress.State == release.StateFailed {
				outcome = audit.Failed
			}
			ctx := s.cfg.Lifetime
			s.cfg.Logger.InfoContext(ctx, "the upgrade has ended",
				slog.String("to", verified.Version().String()), slog.String("state", string(progress.State)))
			record := s.cfg.Auditor.Record(p, iam.ActionUpgradeCluster, updatesResource, outcome, map[string]string{
				"from": s.cfg.Current.String(), "to": verified.Version().String(), "digest": verified.Digest(),
				"state": string(progress.State),
			})
			s.cfg.Auditor.Succeeded(ctx, record)
		})
	}
}

// audit records a start of an upgrade, or its refusal.
func (s *UpdateService) audit(ctx context.Context, p iam.Principal, target release.Version, verified release.Verified, outcome audit.Outcome, reason string) {
	details := map[string]string{"from": s.cfg.Current.String(), "to": target.String(), "mode": string(s.cfg.Upgrader.Mode())}
	if digest := verified.Digest(); digest != "" {
		details["digest"] = digest
	}
	if reason != "" {
		details["reason"] = reason
	}
	s.cfg.Auditor.Succeeded(ctx, s.cfg.Auditor.Record(p, iam.ActionUpgradeCluster, updatesResource, outcome, details))
}

// startRefusal says why an upgrade was not started, in words of this
// product only: never those of the source.
func startRefusal(err error) string {
	if reason := release.ReasonOf(err); reason != "" {
		return "not allowed: " + string(reason)
	}
	for _, sentinel := range []error{
		domain.ErrUpgradeRunning, domain.ErrUpgradeUnavailable, domain.ErrReleaseNotVerified,
		domain.ErrInvalidArgument, context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, sentinel) {
			return sentinel.Error()
		}
	}
	return "the release could not be fetched or started"
}
