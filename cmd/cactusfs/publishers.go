package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/adapters/events/nats"
	"github.com/albievan/cactusfs/internal/adapters/events/webhook"
	"github.com/albievan/cactusfs/internal/adapters/s3client"
	"github.com/albievan/cactusfs/internal/adapters/sealed"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	// publishersTidy is how often the publishers of targets that are gone
	// are closed, and all of them when the node no longer leads.
	publishersTidy = time.Minute
	// publishersListTimeout bounds the reading of the targets when they
	// are tidied.
	publishersListTimeout = 30 * time.Second
)

// publishers is the app.PublisherSource of the node (ADR-0025, "Delivery"):
// it makes the publisher of a target - a webhook or a NATS server - the
// first time the notifier has something for it, and keeps it while the
// target stays as it was. It is the app.SiteSource of the node as well
// (ADR-0026): the client of another deployment, for a target of kind s3, is
// made, kept, replaced and closed as a publisher is.
//
//   - Each target has an egress client of its own, because the target's
//     certificates are the roots it verifies against; the client is
//     observed by telemetry.ObserveEgress (egress.Client.Observe) and its
//     dial function is behind telemetry.InstrumentEgressDial, and the
//     publisher behind telemetry.InstrumentEventPublisher; the client of
//     another deployment (s3client) is behind telemetry.InstrumentSiteClient.
//   - The sealed fields of a target are opened through the node's KMS
//     (sealed.KMS) by the publisher itself, at each attempt (a webhook),
//     each connection (NATS) or each request (another deployment), and are
//     never kept open here. Making a
//     publisher opens nothing and connects to nothing: a target that
//     cannot be reached holds up neither the start of a node nor a write.
//   - A target that changed - its endpoint, its certificates, the way it
//     proves who it is or any of its sealed fields - gets a new publisher;
//     the one it replaces is closed. Tidy closes the publishers of targets
//     that are gone, and all of them on a node that no longer leads.
//   - A target deleted and made again under its name is told apart by its
//     CreatedAt: the publisher of the newer one is never replaced on behalf
//     of the older one, whose loop the delivery worker may not have ended
//     yet.
//   - Once the node closes (close), no publisher is made.
type publishers struct {
	kms       ports.KMS
	targets   ports.TargetStore
	telemetry *telemetry.Telemetry
	allow     []netip.Prefix
	timeout   time.Duration
	version   string
	source    string
	clock     ports.Clock
	active    func() bool
	logger    *slog.Logger

	mu   sync.Mutex
	made map[string]*madePublisher
	// generation counts the publishers made; each is stamped with the
	// count when it is made, so that tidy closes only those made before
	// it read the targets.
	generation uint64
	// closed is set once the node closes.
	closed bool
}

// madePublisher is a publisher made for a target, and what it was made
// from.
type madePublisher struct {
	// from is the target as it was when the publisher was made.
	from delivery.Target
	// publisher is what a webhook or a NATS server is sent through, and
	// site the client of another deployment: one of the two.
	publisher ports.EventPublisher
	site      ports.SiteClient
	// generation is publishers.generation when it was made.
	generation uint64
	// close lets go of what the publisher holds: the connection to a NATS
	// server, the idle connections of a webhook's client.
	close func()
}

// publishersConfig lists what publishers depends on.
type publishersConfig struct {
	// KMS opens the sealed fields of targets; it is the node's, decorated.
	KMS ports.KMS
	// Targets is read by Tidy; it is the node's store of targets,
	// decorated.
	Targets   ports.TargetStore
	Telemetry *telemetry.Telemetry
	// Allow is the setting egress-allow; Timeout the setting
	// delivery-timeout.
	Allow   []netip.Prefix
	Timeout time.Duration
	// Version is the release, named in the User-Agent of a webhook and of
	// the client of another deployment.
	Version string
	// Source is the ID of this deployment, which the client of another
	// deployment sends with every request (s3client.Options.Source).
	Source string
	Clock  ports.Clock
	// Active reports whether the node leads the metadata group: one that
	// does not delivers nothing and keeps no connection.
	Active func() bool
	Logger *slog.Logger
}

var (
	_ app.PublisherSource = (*publishers)(nil)
	_ app.SiteSource      = (*publishers)(nil)
)

func newPublishers(cfg publishersConfig) *publishers {
	p := &publishers{
		kms: cfg.KMS, targets: cfg.Targets, telemetry: cfg.Telemetry, allow: slices.Clone(cfg.Allow), timeout: cfg.Timeout,
		version: cfg.Version, source: cfg.Source, clock: cfg.Clock, active: cfg.Active, logger: cfg.Logger, made: map[string]*madePublisher{},
	}
	if p.logger == nil {
		p.logger = slog.New(slog.DiscardHandler)
	}
	return p
}

// Errors of the source of publishers; each is a delivery.Error of class
// internal, so the item is tried again.
var (
	// errPublishersClosed is a publisher asked for after the node closed.
	errPublishersClosed = errors.New("publishers: the node is closing")
	// errTargetReplaced is a publisher asked for on behalf of a target
	// that was deleted and made again under its name since.
	errTargetReplaced = errors.New("publishers: the target was made again since")
	// errOtherKind is a publisher asked for of a target of kind s3, or a
	// client of another deployment of a target of any other kind.
	errOtherKind = errors.New("publishers: the target is not of that kind")
)

// Publisher implements app.PublisherSource. It makes nothing that connects
// or opens a secret: the publisher does both when it publishes.
func (p *publishers) Publisher(_ context.Context, target delivery.Target) (ports.EventPublisher, error) {
	if !target.Kind.Serves(delivery.ConsumerNotification) {
		return nil, fmt.Errorf("publishers.Publisher: %w: %w: %s is not sent to through a publisher", errOtherKind, domain.ErrInvalidArgument, target)
	}
	m, err := p.madeFor(target)
	switch {
	case err != nil:
		return nil, fmt.Errorf("publishers.Publisher: %w", err)
	case m.publisher == nil:
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("publishers.Publisher: %w", errOtherKind))
	}
	return m.publisher, nil
}

// Site implements app.SiteSource: the client of the other deployment a
// target of kind s3 names. As a publisher, it makes nothing that connects or
// opens a secret: the client does both for each request.
func (p *publishers) Site(_ context.Context, target delivery.Target) (ports.SiteClient, error) {
	if target.Kind != delivery.KindS3 {
		return nil, fmt.Errorf("publishers.Site: %w: %w: %s is not another deployment", errOtherKind, domain.ErrInvalidArgument, target)
	}
	m, err := p.madeFor(target)
	switch {
	case err != nil:
		return nil, fmt.Errorf("publishers.Site: %w", err)
	case m.site == nil:
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("publishers.Site: %w", errOtherKind))
	}
	return m.site, nil
}

// madeFor returns what was made for a target, made now when nothing was or
// the target changed since.
func (p *publishers) madeFor(target delivery.Target) (*madePublisher, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, delivery.Failed(delivery.ClassInternal, errPublishersClosed)
	}
	if m := p.made[target.Name]; m != nil {
		if target.CreatedAt.Before(m.from.CreatedAt) {
			// The loop of a target that was deleted, still running: the
			// publisher of the target now under the name stays.
			return nil, delivery.Failed(delivery.ClassInternal, errTargetReplaced)
		}
		if m.from.SameConnection(target) {
			return m, nil
		}
		delete(p.made, target.Name)
		m.close()
	}
	m, err := p.build(target)
	if err != nil {
		return nil, err
	}
	p.generation++
	m.generation = p.generation
	p.made[target.Name] = m
	return m, nil
}

// build makes the publisher of a target.
func (p *publishers) build(target delivery.Target) (*madePublisher, error) {
	policy := egress.Policy{Allow: p.allow, ResponseTimeout: p.timeout}
	if len(target.CAPEM) > 0 {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(target.CAPEM) {
			return nil, fmt.Errorf("%w: the certificates of %s cannot be read", domain.ErrInvalidArgument, target)
		}
		policy.RootCAs = roots
	}
	client, dial, err := egress.NewClient(policy)
	if err != nil {
		return nil, err
	}
	client = client.Observe(p.telemetry.ObserveEgress)
	dial = p.telemetry.InstrumentEgressDial(dial)
	opener := sealed.KMS{KMS: p.kms}
	switch target.Kind {
	case delivery.KindWebhook:
		hook, err := webhook.New(target, opener, client, webhook.Options{Version: p.version, Clock: p.clock})
		if err != nil {
			return nil, err
		}
		return &madePublisher{from: target.Clone(), publisher: p.telemetry.InstrumentEventPublisher(hook), close: client.CloseIdleConnections}, nil
	case delivery.KindNATS:
		server, err := nats.New(target, opener, dial)
		if err != nil {
			return nil, err
		}
		return &madePublisher{from: target.Clone(), publisher: p.telemetry.InstrumentEventPublisher(server), close: func() { _ = server.Close() }}, nil
	case delivery.KindS3:
		site, err := s3client.New(target, opener, client, s3client.Options{Source: p.source, Version: p.version, Clock: p.clock})
		if err != nil {
			return nil, err
		}
		return &madePublisher{from: target.Clone(), site: p.telemetry.InstrumentSiteClient(site), close: client.CloseIdleConnections}, nil
	default:
		return nil, fmt.Errorf("%w: %s is not sent to through a publisher", domain.ErrInvalidArgument, target)
	}
}

// Run tidies the publishers every publishersTidy until ctx ends, and then
// closes them all. The node closes the source for good when it closes
// (close).
func (p *publishers) Run(ctx context.Context) {
	ticker := time.NewTicker(publishersTidy)
	defer ticker.Stop()
	defer p.closeAll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tidy(ctx)
		}
	}
}

// tidy closes the publishers of targets that are gone, and every publisher
// when the node does not lead. When the targets cannot be read it keeps
// them all: a target that is still there would only be made again.
func (p *publishers) tidy(ctx context.Context) {
	if p.active != nil && !p.active() {
		p.closeAll()
		return
	}
	// What is made while the targets are read is not judged by them: a
	// target made in between is not in the list.
	p.mu.Lock()
	before := p.generation
	p.mu.Unlock()
	listCtx, cancel := context.WithTimeout(ctx, publishersListTimeout)
	defer cancel()
	targets, err := p.targets.ListTargets(listCtx)
	if err != nil {
		p.logger.DebugContext(ctx, "the publishers of targets are not tidied: the targets cannot be read", slog.String("error", err.Error()))
		return
	}
	kept := make(map[string]bool, len(targets))
	for _, target := range targets {
		kept[target.Name] = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, m := range p.made {
		if !kept[name] && m.generation <= before {
			delete(p.made, name)
			m.close()
		}
	}
}

// close closes every publisher for good: none is made afterwards. The node
// calls it when it closes, once its background tasks have returned.
func (p *publishers) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.closeAll()
}

// closeAll closes every publisher.
func (p *publishers) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, m := range p.made {
		delete(p.made, name)
		m.close()
	}
}

// userAgentVersion returns the version the User-Agent of a webhook names:
// version, when a User-Agent can name it (webhook.ValidVersion), and the
// release embedded in the binary otherwise - a version given to the build
// that cannot be named would make every webhook target refuse to be made.
// The fallback is logged once, as a warning.
func userAgentVersion(ctx context.Context, logger *slog.Logger, version string) string {
	if webhook.ValidVersion(version) {
		return version
	}
	release := buildinfo.Release()
	logger.WarnContext(ctx, "the version of this build cannot be named in the User-Agent of a webhook; the release is named instead",
		slog.String("release", release))
	return release
}
