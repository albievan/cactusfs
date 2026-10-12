package nodetls

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	// DefaultReloadInterval is how often the certificate files are looked
	// at, unless the operator says otherwise.
	DefaultReloadInterval = time.Minute
	// MinReloadInterval is the shortest interval there is: the files are
	// read whole every time.
	MinReloadInterval = 5 * time.Second
	// RefusalReminderInterval is how long a refusal that still stands goes
	// unsaid before it is logged again: said once only, it would leave the
	// log with the files that are rotated out.
	RefusalReminderInterval = time.Hour
)

// ReloadRefusal is why a certificate that was found on disk was not put to
// use. The set is fixed, so that it can label a metric.
type ReloadRefusal string

const (
	// RefusedUnreadable - a file is missing, cannot be read, is too large or
	// is not a regular file.
	RefusedUnreadable ReloadRefusal = "unreadable"
	// RefusedPair - the files do not parse, or the key is not the
	// certificate's. It is what a pair looks like between the writing of
	// one file and of the other.
	RefusedPair ReloadRefusal = "pair"
	// RefusedNotYetValid - the certificate is not valid yet.
	RefusedNotYetValid ReloadRefusal = "not_yet_valid"
	// RefusedExpired - the certificate has expired.
	RefusedExpired ReloadRefusal = "expired"
	// RefusedIdentity - a node certificate that does not name this node.
	RefusedIdentity ReloadRefusal = "identity"
	// RefusedUntrusted - a node certificate that is not valid under the
	// node CA.
	RefusedUntrusted ReloadRefusal = "untrusted"
	// RefusedUsage - a certificate for the listeners clients reach that is
	// not one a server may show.
	RefusedUsage ReloadRefusal = "usage"
)

// ReloadRefusals returns every ReloadRefusal there is.
func ReloadRefusals() []ReloadRefusal {
	return []ReloadRefusal{
		RefusedUnreadable, RefusedPair, RefusedNotYetValid, RefusedExpired, RefusedIdentity, RefusedUntrusted, RefusedUsage,
	}
}

// transient reports whether a refusal is what a replacement in progress
// looks like, and so worth a warning rather than an error.
func (r ReloadRefusal) transient() bool {
	return r == RefusedUnreadable || r == RefusedPair
}

// ReloadConfig is what a Reloader needs.
type ReloadConfig struct {
	// CertFile and KeyFile are the PEM files that are read again.
	CertFile string
	KeyFile  string
	// Interval is the pause between two looks at the files, at least
	// MinReloadInterval; zero means that Run never looks.
	Interval time.Duration
	// Clock tells whether a certificate is valid now, and when a refusal
	// that still stands is due to be said again.
	Clock  ports.Clock
	Logger *slog.Logger
	// Tick, when not nil, replaces the timer between two looks.
	Tick func(time.Duration) <-chan time.Time
}

// Reloader replaces a certificate that is in use by the one in its files,
// when the files change and what they hold passes every check (ADR-0020). A
// candidate that fails one is refused, and the certificate in use stays.
type Reloader struct {
	// kind is "node" or "server", for the log.
	kind     string
	certFile string
	keyFile  string
	interval time.Duration
	clock    ports.Clock
	logger   *slog.Logger
	tick     func(time.Duration) <-chan time.Time
	// read reads one of the two files: within its directory for the node's
	// pair, wherever a link leads for the pair clients are shown.
	read func(path string) ([]byte, error)

	// current is the certificate in use; handshakes read it.
	current *atomic.Pointer[tls.Certificate]
	// check is what a candidate of this kind must pass beyond what every
	// candidate must.
	check func(leaf *x509.Certificate, now time.Time) (ReloadRefusal, error)

	refused  map[ReloadRefusal]*atomic.Uint64
	replaced atomic.Uint64

	// polling is held by the look at the files that is in progress - there
	// is one at a time - for as long as it takes, the reading included. No
	// accessor takes it.
	polling chan struct{}

	// mu guards what follows. It is held to compare and to assign, never
	// while a file is read, a certificate is checked or a line is logged: an
	// accessor does not wait for any of those.
	mu sync.Mutex
	// served is the digest of the files the certificate in use came from;
	// seen is that of what was last found, and pending is why it is not in
	// use - nothing when it is.
	served  [sha256.Size]byte
	seen    [sha256.Size]byte
	pending ReloadRefusal
	// since is when what is pending was first refused, and said when that
	// was last logged.
	since time.Time
	said  time.Time
}

// LoadServer reads the certificate and key of the listeners that clients
// reach - the S3 API and the admin listener - and returns a Reloader that
// serves them. The pair must parse and match, or there is nothing to serve.
// The files are regular files of a bounded size; a link to one is followed
// wherever it leads.
//
// A replacement must, beyond that, be valid at the time and be a certificate
// a server may show. That it chains to a CA is not checked, nor which names
// it carries: the node knows neither the roots its clients trust nor every
// name it is reached under.
//
// The pair read here is held to the same two checks, and is served even when
// it fails one: that is logged as an error, and the node starts. A node that
// does not start cannot be given a better certificate without a restart.
func LoadServer(ctx context.Context, cfg ReloadConfig) (*Reloader, error) {
	r, err := newReloader("server", cfg)
	if err != nil {
		return nil, fmt.Errorf("nodetls.LoadServer: %w", err)
	}
	r.read, r.check = readFollowing, checkServer
	certPEM, keyPEM, err := readPair(r.read, cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("nodetls.LoadServer: %w", err)
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("nodetls.LoadServer: %w: certificate and key: %w", domain.ErrInvalidArgument, err)
	}
	leaf, err := leafOf(&certificate)
	if err != nil {
		return nil, fmt.Errorf("nodetls.LoadServer: %w: certificate: %w", domain.ErrInvalidArgument, err)
	}
	r.current = &atomic.Pointer[tls.Certificate]{}
	r.current.Store(&certificate)
	r.served = pairDigest(certPEM, keyPEM)
	r.seen = r.served
	if reason, err := r.fit(leaf, r.clock.Now()); err != nil {
		r.logger.LogAttrs(ctx, slog.LevelError,
			"the certificate read at start would be refused as a replacement; it is served all the same, and clients may refuse it",
			slog.String("reason", string(reason)), slog.String("error", err.Error()),
			slog.String("subject", leaf.Subject.String()), slog.String("serial", leaf.SerialNumber.String()),
			slog.Time("not_before", leaf.NotBefore), slog.Time("not_after", leaf.NotAfter))
	}
	return r, nil
}

// Reloader returns a Reloader that replaces the node's certificate. A
// replacement must be what the first one had to be: a certificate of the node
// CA - the CA as it was loaded, which is not read again - that names this
// node and no other, good for both ends of a connection, and valid at the
// time.
func (i *Identity) Reloader(cfg ReloadConfig) (*Reloader, error) {
	r, err := newReloader("node", cfg)
	if err != nil {
		return nil, fmt.Errorf("nodetls.Reloader: %w", err)
	}
	r.current = &i.current
	r.served = i.digest
	r.seen = r.served
	r.read, r.check = readConfined, i.checkOwn
	return r, nil
}

func newReloader(kind string, cfg ReloadConfig) (*Reloader, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, fmt.Errorf("%w: no certificate file or no key file", domain.ErrInvalidArgument)
	}
	if cfg.Interval < 0 || (cfg.Interval > 0 && cfg.Interval < MinReloadInterval) {
		return nil, fmt.Errorf("%w: reload interval %s: must be at least %s, or 0 to never reload",
			domain.ErrInvalidArgument, cfg.Interval, MinReloadInterval)
	}
	if cfg.Clock == nil {
		return nil, fmt.Errorf("%w: no clock", domain.ErrInvalidArgument)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Tick == nil {
		cfg.Tick = time.After
	}
	r := &Reloader{
		kind: kind, certFile: cfg.CertFile, keyFile: cfg.KeyFile, interval: cfg.Interval, clock: cfg.Clock,
		logger: cfg.Logger.With(slog.String("certificate", kind), slog.String("cert_file", cfg.CertFile)),
		tick:   cfg.Tick, refused: map[ReloadRefusal]*atomic.Uint64{}, polling: make(chan struct{}, 1),
	}
	for _, reason := range ReloadRefusals() {
		r.refused[reason] = &atomic.Uint64{}
	}
	return r, nil
}

// checkOwn is what a replacement of the node's certificate must pass: the
// checks its peers will make of it, made first.
func (i *Identity) checkOwn(leaf *x509.Certificate, now time.Time) (ReloadRefusal, error) {
	named, err := nodeIDOf(leaf)
	if err != nil {
		return RefusedIdentity, err
	}
	if named != i.nodeID {
		return RefusedIdentity, fmt.Errorf("%w: the certificate names node %q, this node is %q", domain.ErrAccessDenied, named, i.nodeID)
	}
	if err := verifyNode(leaf, i.roots, i.nodeID, now); err != nil {
		return RefusedUntrusted, fmt.Errorf("%w: %w", domain.ErrAccessDenied, err)
	}
	return "", nil
}

// checkServer is what a replacement of the certificate clients are shown
// must pass: that a server may show it. A certificate that says nothing
// about its use may be used for anything.
func checkServer(leaf *x509.Certificate, _ time.Time) (ReloadRefusal, error) {
	unrestricted := len(leaf.ExtKeyUsage) == 0 && len(leaf.UnknownExtKeyUsage) == 0
	if unrestricted || slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) ||
		slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return "", nil
	}
	return RefusedUsage, fmt.Errorf("%w: the certificate is not for server authentication", domain.ErrInvalidArgument)
}

// fit tells why a certificate would not be put to use if it were found on
// disk now, and nothing when it would be: it is valid at the time, and passes
// what a certificate of its kind must.
func (r *Reloader) fit(leaf *x509.Certificate, now time.Time) (ReloadRefusal, error) {
	if now.Before(leaf.NotBefore) {
		return RefusedNotYetValid, errors.New("the certificate is not valid yet")
	}
	if now.After(leaf.NotAfter) {
		return RefusedExpired, errors.New("the certificate has expired")
	}
	return r.check(leaf, now)
}

// pairDigest tells one content of the two files from any other. It is kept
// in memory and compared; it is never logged.
func pairDigest(certPEM, keyPEM []byte) [sha256.Size]byte {
	// Each file is digested by itself first, so that where one ends and
	// the other begins is part of what is compared.
	certDigest, keyDigest := sha256.Sum256(certPEM), sha256.Sum256(keyPEM)
	return sha256.Sum256(append(certDigest[:], keyDigest[:]...))
}

// GetCertificate returns the certificate in use, for tls.Config. It never
// returns nil: there is a certificate from the start, and it is only ever
// exchanged for another.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.current.Load(), nil
}

// NotAfter returns when the certificate in use expires.
func (r *Reloader) NotAfter() time.Time {
	return r.current.Load().Leaf.NotAfter
}

// Refused returns how many distinct candidates were refused for the given
// reason. A candidate that stays on disk is counted once, not at every look.
func (r *Reloader) Refused(reason ReloadRefusal) uint64 {
	if counter, ok := r.refused[reason]; ok {
		return counter.Load()
	}
	return 0
}

// Replaced returns how many times the certificate was replaced.
func (r *Reloader) Replaced() uint64 {
	return r.replaced.Load()
}

// Pending returns why what is on disk is not in use, and nothing when it is
// in use - as far as the last look at the files that ended told. It does not
// wait for one that is in progress.
func (r *Reloader) Pending() ReloadRefusal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending
}

// Run looks at the files every interval until ctx is cancelled. With an
// interval of zero it returns at once and the files are never read again. It
// returns when ctx is cancelled even while a file is being read.
func (r *Reloader) Run(ctx context.Context) {
	if r.interval <= 0 {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.tick(r.interval):
		}
		r.Poll(ctx)
	}
}

// found is what a reading of the two files gave.
type found struct {
	certPEM, keyPEM []byte
	err             error
}

// Poll looks at the files once, and reports whether the certificate in use
// was replaced by what they hold. One look happens at a time: a second waits
// for the first to end.
//
// When ctx is cancelled Poll returns false at once, whatever it is waiting
// for. A reading that does not end - a mount that hangs - is left behind
// with the goroutine that does it; no look begins until it has ended, and
// the certificate in use stays.
func (r *Reloader) Poll(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case r.polling <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	// Read by a goroutine of its own: a read of a file cannot be cancelled.
	read := make(chan found, 1)
	go func() {
		certPEM, keyPEM, err := readPair(r.read, r.certFile, r.keyFile)
		read <- found{certPEM, keyPEM, err}
	}()
	select {
	case pair := <-read:
		defer func() { <-r.polling }()
		return r.consider(ctx, pair)
	case <-ctx.Done():
		// The look is over when the reading is, whenever that will be.
		go func() {
			<-read
			<-r.polling
		}()
		return false
	}
}

// consider judges what was read, and puts it to use when it passes. It is
// called by the one look in progress.
func (r *Reloader) consider(ctx context.Context, pair found) bool {
	if pair.err != nil {
		// No content to tell from another: the error stands in for it.
		r.refuse(ctx, sha256.Sum256([]byte(pair.err.Error())), RefusedUnreadable, nil, pair.err)
		return false
	}
	digest := pairDigest(pair.certPEM, pair.keyPEM)
	r.mu.Lock()
	inUse := digest == r.served
	if inUse {
		r.seen, r.pending = digest, ""
	}
	r.mu.Unlock()
	if inUse {
		return false
	}
	candidate, err := tls.X509KeyPair(pair.certPEM, pair.keyPEM)
	if err != nil {
		r.refuse(ctx, digest, RefusedPair, nil, err)
		return false
	}
	leaf, err := leafOf(&candidate)
	if err != nil {
		r.refuse(ctx, digest, RefusedPair, nil, err)
		return false
	}
	if reason, err := r.fit(leaf, r.clock.Now()); err != nil {
		r.refuse(ctx, digest, reason, leaf, err)
		return false
	}

	r.mu.Lock()
	previous := r.current.Swap(&candidate).Leaf
	r.served, r.seen, r.pending = digest, digest, ""
	r.mu.Unlock()
	r.replaced.Add(1)
	r.logger.InfoContext(ctx, "certificate replaced; new handshakes present the new one",
		slog.String("subject", leaf.Subject.String()), slog.String("serial", leaf.SerialNumber.String()),
		slog.Time("not_after", leaf.NotAfter),
		slog.String("replaced_serial", previous.SerialNumber.String()), slog.Time("replaced_not_after", previous.NotAfter))
	if leaf.NotAfter.Before(previous.NotAfter) {
		r.logger.WarnContext(ctx, "the new certificate expires sooner than the one it replaced",
			slog.Time("not_after", leaf.NotAfter), slog.Time("replaced_not_after", previous.NotAfter))
	}
	return true
}

// refuse records that what is on disk is not put to use. It is counted once
// for one content of the files, not at every look, and logged then - and
// again, at the same level, every RefusalReminderInterval for as long as it
// stands. The key is never part of what is logged, nor the digest.
func (r *Reloader) refuse(ctx context.Context, digest [sha256.Size]byte, reason ReloadRefusal, leaf *x509.Certificate, err error) {
	now := r.clock.Now()
	r.mu.Lock()
	again := digest == r.seen && reason == r.pending
	if again && now.Sub(r.said) < RefusalReminderInterval {
		r.mu.Unlock()
		return
	}
	if !again {
		r.seen, r.pending, r.since = digest, reason, now
		r.refused[reason].Add(1)
	}
	r.said = now
	since := r.since
	r.mu.Unlock()

	inUse := r.current.Load().Leaf
	attrs := []slog.Attr{
		slog.String("reason", string(reason)), slog.String("error", err.Error()),
		slog.String("in_use_serial", inUse.SerialNumber.String()), slog.Time("in_use_not_after", inUse.NotAfter),
	}
	if leaf != nil {
		attrs = append(attrs, slog.String("subject", leaf.Subject.String()), slog.String("serial", leaf.SerialNumber.String()),
			slog.Time("not_after", leaf.NotAfter))
	}
	if again {
		attrs = append(attrs, slog.Time("refused_since", since))
	}
	level, message := slog.LevelError, "the certificate on disk is refused; the one in use stays"
	if reason.transient() {
		level, message = slog.LevelWarn, "the certificate on disk cannot be used yet; the one in use stays, and the files are read again"
	}
	r.logger.LogAttrs(ctx, level, message, attrs...)
}
