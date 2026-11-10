//go:build integration || k8s

// Package releasemirror is a mirror of the releases of CactusFS for the
// end-to-end tests of updates and upgrades (PLAN P.11, ADR-0029) - those of
// test/integration and of test/k8s: a local TLS server that answers GitHub's releases JSON at
// /repos/albievan/cactusfs/releases and serves the files of each release -
// the binaries, SHA256SUMS, digests.txt and their Sigstore bundles - as
// GitHub does, behind a redirect from the address the JSON names.
//
// The lists are signed by a sigstoretest.Sigstore with the identity of the
// production release workflow at the tag of the release (or of another tag,
// when a test asks for that) and GitHub's OIDC issuer, so that a node
// verifies them with its production rule and only its trusted root swapped
// (update-trusted-root). The server's certificate is issued by a CA of the
// mirror's own, which a node is given as update-ca. It listens on the
// loopback address, or - for nodes in containers - where Options say.
//
// It is for tests only and holds nothing secret: its keys are made when it
// starts and are gone when it stops.
package releasemirror

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore"
	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore/sigstoretest"
	"github.com/albievan/cactusfs/internal/domain/release"
)

// ListPath is where the mirror answers the releases JSON, as GitHub's REST
// API does for the repository.
const ListPath = "/repos/albievan/cactusfs/releases"

// downloadPrefix is where the JSON says the files of releases are, as
// GitHub's browser_download_url does: <prefix>v<version>/<name>.
const downloadPrefix = "/albievan/cactusfs/releases/download/"

// Release is a release to publish.
type Release struct {
	// Version is the release, without the "v" of its tag: "0.7.1".
	Version string
	// Prerelease marks a pre-release, as GitHub does.
	Prerelease bool
	// Binaries are the binaries of the release by asset name
	// (release.BinaryAsset): what SHA256SUMS lists, and what is served
	// unless Served says otherwise.
	Binaries map[string][]byte
	// Served replaces what is served for an asset of that name, whatever
	// SHA256SUMS says of it: a tampered binary.
	Served map[string][]byte
	// SignedAs is the version whose tag the identity of the bundles names;
	// empty for Version. Another version is a bundle of another release
	// passed off as this one.
	SignedAs string
	// ListDigest is the digest digests.txt gives the release's image list
	// ("sha256:<hex>"): an image a cluster has. Empty: one made up from the
	// version, which no cluster has.
	ListDigest string
}

// Options say where a mirror listens and at which address it is reached.
type Options struct {
	// Listen is the address the mirror listens on; 127.0.0.1:0 when empty.
	Listen string
	// Host is the IP address the mirror is reached at: its certificate
	// names it, and so do the addresses of the releases JSON. 127.0.0.1
	// when empty.
	Host string
}

// Request is a request the mirror was sent, as far as a test asks about it.
type Request struct {
	// Path is the path of the request, without its query.
	Path string
	// UserAgent is what the request named itself.
	UserAgent string
	// Credentials reports whether the request carried an Authorization,
	// Cookie or Proxy-Authorization header.
	Credentials bool
}

// Mirror is a running mirror. It is safe for concurrent use.
type Mirror struct {
	server *httptest.Server
	// base is https://<host>:<port>, where the mirror is reached.
	base   string
	port   int
	signer *sigstoretest.Sigstore
	caPEM  []byte

	mu        sync.Mutex
	releases  []published
	assets    map[string][]byte
	downloads map[string]string
	requests  []Request
}

// published is a release as the JSON lists it.
type published struct {
	version    string
	prerelease bool
	published  time.Time
	files      []file
}

// file is an asset of a release: its name, where the JSON says it is, and
// its size.
type file struct {
	name string
	url  string
	size int
}

// New starts a mirror on the loopback address whose lists signer signs.
// signer must be new: its timestamp authority signs for a few minutes only,
// and a release is signed when it is published.
func New(signer *sigstoretest.Sigstore) (*Mirror, error) {
	return NewWith(signer, Options{})
}

// NewWith starts a mirror as New does, listening and reached where opts
// say.
func NewWith(signer *sigstoretest.Sigstore, opts Options) (*Mirror, error) {
	if signer == nil {
		return nil, errors.New("releasemirror.New: no signer")
	}
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:0"
	}
	if opts.Host == "" {
		opts.Host = "127.0.0.1"
	}
	host := net.ParseIP(opts.Host)
	if host == nil {
		return nil, fmt.Errorf("releasemirror.New: the host %q is not an IP address", opts.Host)
	}
	caPEM, leaf, err := certificates(host)
	if err != nil {
		return nil, fmt.Errorf("releasemirror.New: %w", err)
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("releasemirror.New: %w", err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, fmt.Errorf("releasemirror.New: %s is not a TCP address", listener.Addr())
	}
	m := &Mirror{signer: signer, caPEM: caPEM, port: addr.Port, assets: map[string][]byte{}, downloads: map[string]string{}}
	m.server = httptest.NewUnstartedServer(http.HandlerFunc(m.serve))
	_ = m.server.Listener.Close()
	m.server.Listener = listener
	m.server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	m.server.StartTLS()
	m.base = "https://" + net.JoinHostPort(host.String(), strconv.Itoa(addr.Port))
	return m, nil
}

// Close stops the mirror.
func (m *Mirror) Close() { m.server.Close() }

// URL is the address of the releases JSON: the setting update-source.
func (m *Mirror) URL() string { return m.base + ListPath }

// Port is the port the mirror listens on.
func (m *Mirror) Port() int { return m.port }

// CA is the PEM certificate of the CA that issued the mirror's
// certificate: the content of the file of the setting update-ca.
func (m *Mirror) CA() []byte { return slices.Clone(m.caPEM) }

// Requests returns the requests the mirror was sent, in their order.
func (m *Mirror) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

// Publish signs a release and lists it: its binaries, SHA256SUMS and its
// bundle, a digests.txt of the release's image and its bundle. A release
// published later is listed first, as GitHub lists the newest first.
func (m *Mirror) Publish(r Release) error {
	if r.Version == "" || strings.HasPrefix(r.Version, "v") {
		return fmt.Errorf("releasemirror.Publish: the version %q is not X.Y.Z[-pre]", r.Version)
	}
	signedAs := r.SignedAs
	if signedAs == "" {
		signedAs = r.Version
	}
	identity := sigstore.DefaultIdentityPrefix + signedAs

	names := make([]string, 0, len(r.Binaries))
	for name := range r.Binaries {
		names = append(names, name)
	}
	slices.Sort(names)
	var sums strings.Builder
	for _, name := range names {
		sum := sha256.Sum256(r.Binaries[name])
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	contents := map[string][]byte{}
	for _, name := range names {
		contents[name] = r.Binaries[name]
		if served, ok := r.Served[name]; ok {
			contents[name] = served
		}
	}
	contents[release.SumsAsset] = []byte(sums.String())
	contents[release.DigestsAsset] = digests(r.Version, r.ListDigest)
	for list, bundle := range map[string]string{release.SumsAsset: release.SumsBundleAsset, release.DigestsAsset: release.DigestsBundleAsset} {
		signed, err := m.signer.Sign(identity, sigstore.DefaultIssuer, contents[list])
		if err != nil {
			return fmt.Errorf("releasemirror.Publish: %s of %s: %w", list, r.Version, err)
		}
		contents[bundle] = signed
	}

	p := published{version: r.Version, prerelease: r.Prerelease, published: time.Now().UTC().Truncate(time.Second)}
	m.mu.Lock()
	defer m.mu.Unlock()
	order := append(names, release.SumsAsset, release.SumsBundleAsset, release.DigestsAsset, release.DigestsBundleAsset)
	for _, name := range order {
		id := strconv.Itoa(len(m.assets) + 1)
		path := downloadPrefix + "v" + r.Version + "/" + name
		if _, twice := m.downloads[path]; twice {
			return fmt.Errorf("releasemirror.Publish: %s is published already", r.Version)
		}
		m.assets[id] = contents[name]
		m.downloads[path] = id
		p.files = append(p.files, file{name: name, url: m.base + path, size: len(contents[name])})
	}
	m.releases = append([]published{p}, m.releases...)
	return nil
}

// digests is a digests.txt of a release as scripts/release.sh writes it:
// the image's list - by list when it is given - and the image of each
// platform, by digests made up from the version: the platforms' are never
// pulled, and neither is the list unless a test gives one a cluster has.
func digests(version, list string) []byte {
	digest := func(what string) string {
		sum := sha256.Sum256([]byte(version + " " + what))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	if list == "" {
		list = digest("list")
	}
	ref := "ghcr.io/albievan/cactusfs:" + version
	return []byte(ref + " " + list + "\n" +
		ref + " linux/amd64 " + digest("linux/amd64") + "\n" +
		ref + " linux/arm64 " + digest("linux/arm64") + "\n")
}

// githubRelease and githubAsset are a release and its file in GitHub's
// releases JSON, as far as a node reads it.
type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	PublishedAt string        `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int    `json:"size"`
	ContentType        string `json:"content_type"`
}

// serve answers the releases JSON, an asset's address with a redirect to
// its content, as GitHub does, and its content.
func (m *Mirror) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.requests = append(m.requests, Request{
		Path: r.URL.Path, UserAgent: r.UserAgent(),
		Credentials: r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "",
	})
	m.mu.Unlock()
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case r.URL.Path == ListPath:
		m.serveList(w)
	case strings.HasPrefix(r.URL.Path, downloadPrefix):
		// GitHub answers the address of an asset with a redirect to where
		// its bytes are.
		m.mu.Lock()
		id, ok := m.downloads[r.URL.Path]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, m.base+"/assets/"+id, http.StatusFound)
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		m.mu.Lock()
		data, ok := m.assets[strings.TrimPrefix(r.URL.Path, "/assets/")]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

// serveList answers the releases JSON.
func (m *Mirror) serveList(w http.ResponseWriter) {
	m.mu.Lock()
	out := make([]githubRelease, 0, len(m.releases))
	for _, p := range m.releases {
		g := githubRelease{
			TagName: "v" + p.version, Name: "v" + p.version, Prerelease: p.prerelease,
			PublishedAt: p.published.Format(time.RFC3339),
			HTMLURL:     m.base + "/albievan/cactusfs/releases/tag/v" + p.version,
			Assets:      make([]githubAsset, 0, len(p.files)),
		}
		for _, f := range p.files {
			g.Assets = append(g.Assets, githubAsset{Name: f.name, BrowserDownloadURL: f.url, Size: f.size, ContentType: "application/octet-stream"})
		}
		out = append(out, g)
	}
	m.mu.Unlock()
	data, err := json.Marshal(out)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// certificates makes a CA and a certificate it issued of the loopback
// address and host, and returns the CA's certificate as PEM and the
// server's certificate with its key.
func certificates(host net.IP) ([]byte, tls.Certificate, error) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "releasemirror test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "releasemirror"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback, host},
	}, ca, key.Public(), caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	leaf := tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), leaf, nil
}
