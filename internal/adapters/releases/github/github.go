// Package github is the update source of ADR-0029: it reads the releases
// JSON of GitHub's REST API - or of a mirror that answers as it does - and
// downloads the files of a release, as a ports.ReleaseSource.
//
// It asks only the address the operator set (update-source) and files at
// its host - or at github.com when that address is GitHub's API - over https
// only, through the egress client of ADR-0023 as ADR-0029 amends it for
// the update client: every address dialled is judged against egress-allow
// in the dialer, up to egress.MaxRedirects redirects are followed - GitHub
// answers every download with one - each to https only, and no proxy is
// used. It sends no token, no cookie and no Authorization header, so that
// nothing can leak across a redirect; it says who it is with
// "User-Agent: cactusfs/<version>".
//
// What it reads is bounded: the releases JSON to MaxReleasesBody, a file
// to the limit its caller gives and never past MaxDownload, a declared
// Content-Length over the bound refused before a byte of the body is read,
// and what is read counted regardless of what was declared.
//
// Nothing it says is trusted: what is installed is judged from a release's
// signed lists (ports.ReleaseVerifier). Its errors name the source by its
// origin - scheme, host and port - and never by a path or a query, and
// each has a class (ClassOf) that the use case shows as last_error.
package github

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// The bounds of what the source reads (ADR-0029, the amendment to
// ADR-0023).
const (
	// MaxReleasesBody is the most of the releases JSON that is read.
	MaxReleasesBody = 1 << 20
	// MaxDownload is the most of a file of a release that is read,
	// whatever limit the caller of Fetch gives.
	MaxDownload = egress.MaxResponseBodyLimit
	// PerPage is how many releases are asked for, newest first: GitHub's
	// per_page.
	PerPage = 30
	// maxVersionLength bounds the version named in the User-Agent.
	maxVersionLength = 64
)

// Class is what a failure of the source is called: all that the use case
// shows of it (last_error). It never holds the source's words.
type Class string

// The classes of a failure.
const (
	// ClassNone is no failure.
	ClassNone Class = ""
	// ClassUnreachable is a source that could not be connected to, broke
	// the connection, or said the fault is its own (a 5xx or a 408). It
	// wraps domain.ErrUnavailable: asking later may help.
	ClassUnreachable Class = "unreachable"
	// ClassRateLimited is a source that refuses to answer for now because
	// of the rate it is asked at: a 429, or a 403 with
	// X-RateLimit-Remaining: 0 or a Retry-After. It wraps
	// domain.ErrUnavailable.
	ClassRateLimited Class = "rate-limited"
	// ClassRefused is an exchange either side refused: an address
	// egress-allow does not admit, a redirect that is not followed (to
	// http, past the count, malformed), a certificate that does not
	// verify, an asset address that is not https (with
	// domain.ErrInvalidArgument), or a source that refused the request (a
	// 4xx that is no rate limit).
	ClassRefused Class = "refused"
	// ClassInvalidAnswer is an answer that cannot be used: a status other
	// than 200 that is no failure, JSON that does not parse, a body over
	// its bound (with domain.ErrEntityTooLarge when it is a file).
	ClassInvalidAnswer Class = "invalid-answer"
	// ClassTimeout is a source that did not answer in time or stopped
	// sending: the response timeout, or the deadline of the caller. It
	// wraps domain.ErrUnavailable.
	ClassTimeout Class = "timeout"
)

// Error is a failure of the source. Its text names the operation, the
// origin of the source and the class, and nothing of a path, a query or
// what the source said; what went wrong in so many words is the error it
// wraps, for the log of this node.
type Error struct {
	// Op is the operation: "github.Releases" or "github.Fetch".
	Op string
	// Origin is the scheme, host and port asked; empty when the address
	// could not be read.
	Origin string
	// Class is what the failure is called.
	Class Class
	// Status is the HTTP status the source answered with; 0 when it did
	// not answer.
	Status int
	// kind is the domain sentinel the failure is, or nil.
	kind error
	// cause is the failure itself.
	cause error
}

// Error implements error.
func (e *Error) Error() string {
	text := e.Op + ": " + string(e.Class)
	if e.Origin != "" {
		text = e.Op + ": " + e.Origin + ": " + string(e.Class)
	}
	if e.Status != 0 {
		text += ": the source answered " + strconv.Itoa(e.Status)
	}
	return text
}

// Unwrap returns the domain sentinel of the failure, when it has one, and
// the failure itself: domain.ErrUnavailable for ClassUnreachable,
// ClassRateLimited and ClassTimeout; domain.ErrEntityTooLarge for a file
// over its limit; domain.ErrInvalidArgument for an asset address that is
// not https; and the error of the caller's context when it ended.
func (e *Error) Unwrap() []error {
	var errs []error
	if e.kind != nil {
		errs = append(errs, e.kind)
	}
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	return errs
}

// ClassOf returns the class of an error of this package: ClassNone for
// nil, the class an *Error carries, ClassTimeout for another error that
// is context.DeadlineExceeded, and ClassUnreachable for any other error.
func ClassOf(err error) Class {
	if err == nil {
		return ClassNone
	}
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Class
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassTimeout
	}
	return ClassUnreachable
}

// Config is what a Source is made of.
type Config struct {
	// URL is the setting update-source: the address of the releases JSON,
	// such as https://api.github.com/repos/albievan/cactusfs/releases.
	// https only, with a host, and no user information, query or
	// fragment.
	URL string
	// CA is the setting update-ca: PEM certificates of the roots the
	// source's certificate is verified against; empty for the system's
	// roots. There is no way to skip verification.
	CA []byte
	// Allow is the setting egress-allow (egress.Policy.Allow).
	Allow []netip.Prefix
	// Resolver looks names up; nil is net.DefaultResolver.
	Resolver egress.Resolver
	// Version is the version of this build, named in the User-Agent:
	// letters, digits, '.', '-', '+' and '_', at most 64 of them.
	Version string
	// Timeout is how long the source has to send the headers of an
	// answer, and how long a connection may make no progress
	// (egress.Policy.ResponseTimeout); zero is egress.DefaultResponseTimeout.
	// The whole of a call is bounded by its context.
	Timeout time.Duration
}

// The hosts of GitHub: its REST API answers the releases JSON, and names
// every file of a release at the web host - browser_download_url is
// https://github.com/<owner>/<repo>/releases/download/<tag>/<name> - which
// answers with a redirect to its storage.
const (
	gitHubAPIHost = "api.github.com"
	gitHubWebHost = "github.com"
)

// Source is a ports.ReleaseSource that asks GitHub's REST API, or a mirror
// of it.
type Source struct {
	list      *url.URL
	origin    string
	userAgent string
	client    *egress.Client
	// assetOrigins are the origins - host and port - an asset's address
	// may name: the source's own, and GitHub's web host when the source
	// is GitHub's API.
	assetOrigins map[string]bool
}

// originKey returns the host and port of an https address, the host in
// lower case and the port 443 when none is named.
func originKey(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

var _ ports.ReleaseSource = (*Source)(nil)

// New returns the source of the configuration. A URL that is not https,
// has no host, or has user information, a query or a fragment, a CA that
// holds no certificate, a version that cannot be named in a header, and a
// negative timeout are refused, wrapping domain.ErrInvalidArgument; no
// error repeats the URL.
func New(cfg Config) (*Source, error) {
	list, err := sourceURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("github.New: %w", err)
	}
	if !versionText(cfg.Version) {
		return nil, fmt.Errorf("github.New: %w: the version is not one a User-Agent can name", domain.ErrInvalidArgument)
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("github.New: %w: a negative timeout", domain.ErrInvalidArgument)
	}
	var roots *x509.CertPool
	if len(cfg.CA) > 0 {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(cfg.CA) {
			return nil, fmt.Errorf("github.New: %w: update-ca holds no PEM certificate", domain.ErrInvalidArgument)
		}
	}
	client, _, err := egress.NewClient(egress.Policy{
		Allow:           cfg.Allow,
		RootCAs:         roots,
		ResponseTimeout: cfg.Timeout,
		Resolver:        cfg.Resolver,
		MaxResponseBody: MaxDownload,
		Redirects:       egress.MaxRedirects,
	})
	if err != nil {
		return nil, fmt.Errorf("github.New: %w: %w", domain.ErrInvalidArgument, err)
	}
	origins := map[string]bool{originKey(list): true}
	if originKey(list) == gitHubAPIHost+":443" {
		origins[gitHubWebHost+":443"] = true
	}
	return &Source{
		list:         list,
		origin:       list.Scheme + "://" + list.Host,
		userAgent:    "cactusfs/" + cfg.Version,
		client:       client,
		assetOrigins: origins,
	}, nil
}

// CheckSourceURL reports whether raw may be the setting update-source: an
// https address with a host, and no user information, query or fragment.
// The error wraps domain.ErrInvalidArgument and does not repeat raw, which
// may hold a credential. It is the rule New applies, for the composition
// root to refuse a value before the node starts.
func CheckSourceURL(raw string) error {
	_, err := sourceURL(raw)
	return err
}

// sourceURL parses the address of update-source.
func sourceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: the update source is not an address", domain.ErrInvalidArgument)
	case u.Scheme != "https" || u.Opaque != "" || u.Hostname() == "":
		return nil, fmt.Errorf("%w: the update source is not an https address with a host", domain.ErrInvalidArgument)
	case u.User != nil || strings.Contains(u.Host, "@"):
		return nil, fmt.Errorf("%w: the update source has user information", domain.ErrInvalidArgument)
	case u.RawQuery != "" || u.ForceQuery:
		return nil, fmt.Errorf("%w: the update source has a query", domain.ErrInvalidArgument)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, fmt.Errorf("%w: the update source has a fragment", domain.ErrInvalidArgument)
	}
	return u, nil
}

// versionText reports whether v may be named in the User-Agent.
func versionText(v string) bool {
	if v == "" || len(v) > maxVersionLength {
		return false
	}
	for _, c := range v {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '-' || c == '+' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// Releases implements ports.ReleaseSource: GET <update-source>?per_page=30.
// A release that is a draft, or whose tag is no version, is left out, as
// is an entry that is not a release; an asset the domain does not keep
// (release.NewRelease) is dropped.
func (s *Source) Releases(ctx context.Context) ([]release.Release, error) {
	const op = "github.Releases"
	u := *s.list
	u.RawQuery = "per_page=" + strconv.Itoa(PerPage)
	resp, err := s.get(ctx, &u, "application/vnd.github+json")
	if err != nil {
		return nil, s.failed(op, s.origin, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := answered(op, s.origin, resp); err != nil {
		return nil, err
	}
	if declaredLength(resp) > MaxReleasesBody {
		return nil, &Error{Op: op, Origin: s.origin, Class: ClassInvalidAnswer, cause: errors.New("the releases JSON is said to be over 1 MiB")}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxReleasesBody+1))
	if err != nil {
		return nil, s.failed(op, s.origin, err)
	}
	if len(data) > MaxReleasesBody {
		return nil, &Error{Op: op, Origin: s.origin, Class: ClassInvalidAnswer, cause: errors.New("the releases JSON is over 1 MiB")}
	}
	releases, err := parseReleases(data)
	if err != nil {
		return nil, &Error{Op: op, Origin: s.origin, Class: ClassInvalidAnswer, cause: err}
	}
	return releases, nil
}

// Fetch implements ports.ReleaseSource. The asset's address must be https
// at the source's own host and port - or, when the source is GitHub's API
// (api.github.com), at github.com:443, where GitHub names the files of a
// release; any other is refused, class ClassRefused wrapping
// domain.ErrInvalidArgument, and never asked. What that host redirects to
// is followed under the egress rules (https only, each address judged
// against egress-allow, at most egress.MaxRedirects), as GitHub's
// downloads need: their storage is another host. The limit is bounded by
// MaxDownload; a negative one is refused wrapping domain.ErrInvalidArgument.
// A Content-Length over the limit is refused before a byte of the body is
// read; a body that is longer than the limit whatever it declared fails
// once limit bytes are written.
func (s *Source) Fetch(ctx context.Context, asset release.Asset, limit int64, w io.Writer) error {
	const op = "github.Fetch"
	u, err := url.Parse(asset.URL)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Hostname() == "" || u.User != nil {
		// Nothing of the address is told: it may be anything.
		return &Error{Op: op, Class: ClassRefused, kind: domain.ErrInvalidArgument, cause: errors.New("an asset is fetched over https only")}
	}
	origin := u.Scheme + "://" + u.Host
	if !s.assetOrigins[originKey(u)] {
		// Not a host the operator named: the answer of the source does
		// not choose where the node calls.
		return &Error{Op: op, Origin: origin, Class: ClassRefused, kind: domain.ErrInvalidArgument, cause: errors.New("an asset is fetched from the update source's host only")}
	}
	if limit < 0 {
		return &Error{Op: op, Origin: origin, Class: ClassRefused, kind: domain.ErrInvalidArgument, cause: errors.New("a negative limit")}
	}
	limit = min(limit, MaxDownload)
	resp, err := s.get(ctx, u, "application/octet-stream")
	if err != nil {
		return s.failed(op, origin, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := answered(op, origin, resp); err != nil {
		return err
	}
	if declaredLength(resp) > limit {
		return &Error{Op: op, Origin: origin, Class: ClassInvalidAnswer, kind: domain.ErrEntityTooLarge, cause: fmt.Errorf("the file is said to be over %d bytes", limit)}
	}
	body := &reader{r: resp.Body}
	if _, err := io.Copy(w, io.LimitReader(body, limit)); err != nil {
		if body.err == nil {
			// The writer failed, not the source.
			return fmt.Errorf("%s: %w", op, err)
		}
		return s.failed(op, origin, body.err)
	}
	// One byte past the limit tells a body that is longer.
	var probe [1]byte
	n, err := body.Read(probe[:])
	switch {
	case n > 0 || errors.Is(err, egress.ErrResponseTooLarge):
		return &Error{Op: op, Origin: origin, Class: ClassInvalidAnswer, kind: domain.ErrEntityTooLarge, cause: fmt.Errorf("the file is over %d bytes", limit)}
	case err != nil && !errors.Is(err, io.EOF):
		return s.failed(op, origin, err)
	}
	return nil
}

// get sends GET u with the headers of this source and nothing else: no
// token, no cookie, no Authorization.
func (s *Source) get(ctx context.Context, u *url.URL, accept string) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("github: request: %w", err)
	}
	r.Header.Set("Accept", accept)
	r.Header.Set("User-Agent", s.userAgent)
	resp, err := s.client.Do(r)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	return resp, nil
}

// failed classes a failure of the exchange with the source - the egress
// client's, or a read of a body - by its delivery class. The error of the
// caller's context is kept, so that errors.Is finds it; a call the caller
// cancelled is ClassUnreachable and does not wrap domain.ErrUnavailable.
func (s *Source) failed(op, origin string, err error) error {
	class := ClassUnreachable
	switch delivery.ClassOf(err) {
	case delivery.ClassTimeout:
		class = ClassTimeout
	case delivery.ClassEgressRefused, delivery.ClassRedirect, delivery.ClassTLS:
		class = ClassRefused
	case delivery.ClassServer:
		class = ClassInvalidAnswer
	case delivery.ClassInternal:
		if !errors.Is(err, context.Canceled) {
			class = ClassRefused
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		class = ClassTimeout
	}
	failure := &Error{Op: op, Origin: origin, Class: class, cause: err}
	switch {
	case errors.Is(err, context.Canceled):
		// The caller went away: nothing is said of the source.
	case class == ClassUnreachable, class == ClassTimeout, class == ClassRateLimited:
		failure.kind = domain.ErrUnavailable
	}
	return failure
}

// answered returns nil for a 200, and otherwise the failure the status is.
func answered(op, origin string, resp *http.Response) error {
	status := resp.StatusCode
	failure := &Error{Op: op, Origin: origin, Status: status, cause: fmt.Errorf("status %d", status)}
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusTooManyRequests,
		status == http.StatusForbidden && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""):
		failure.Class, failure.kind = ClassRateLimited, domain.ErrUnavailable
	case status >= 500 || status == http.StatusRequestTimeout:
		failure.Class, failure.kind = ClassUnreachable, domain.ErrUnavailable
	case status >= 400:
		failure.Class = ClassRefused
	default:
		failure.Class = ClassInvalidAnswer
	}
	return failure
}

// declaredLength returns the length the source declared for the body of
// resp, or -1. The egress client hides a Content-Length over its own bound
// from resp.ContentLength; the header still says it, and is read for it.
// A Content-Length that came with chunks is dropped by net/http, and the
// body is bounded as it is read.
func declaredLength(resp *http.Response) int64 {
	if resp.ContentLength >= 0 {
		return resp.ContentLength
	}
	values := resp.Header.Values("Content-Length")
	if len(values) == 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// reader records the error of reading a body, so that a failure of the
// source is told from a failure of the writer.
type reader struct {
	r   io.Reader
	err error
}

// Read implements io.Reader.
func (r *reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	// io.EOF is returned as it is, for io.Copy.
	return n, err
}

// githubRelease is a release in GitHub's releases JSON, as far as it is
// read.
type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	PublishedAt *string       `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Assets      []githubAsset `json:"assets"`
}

// githubAsset is a file of a release in GitHub's releases JSON.
type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// parseReleases reads GitHub's releases JSON: an array of releases. An
// entry that does not read as a release, a draft, and a release whose tag
// is no version are left out; what is kept is made by release.NewRelease,
// in the order of the array. JSON that is not an array is an error.
func parseReleases(data []byte) ([]release.Release, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("the releases JSON is not an array: %w", err)
	}
	out := make([]release.Release, 0, len(entries))
	for _, entry := range entries {
		var r githubRelease
		if err := json.Unmarshal(entry, &r); err != nil || r.Draft {
			continue
		}
		version, err := release.ParseVersion(r.TagName)
		if err != nil {
			continue
		}
		var published time.Time
		if r.PublishedAt != nil {
			if t, err := time.Parse(time.RFC3339, *r.PublishedAt); err == nil {
				published = t
			}
		}
		assets := make([]release.Asset, 0, len(r.Assets))
		for _, a := range r.Assets {
			assets = append(assets, release.Asset{Name: a.Name, URL: a.BrowserDownloadURL, Size: a.Size})
		}
		out = append(out, release.NewRelease(version, published, r.Prerelease, r.HTMLURL, assets))
	}
	return out, nil
}
