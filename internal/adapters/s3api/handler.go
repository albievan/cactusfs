// Package s3api is the inbound adapter for the S3-compatible data API. It is
// implemented from AWS's public S3 API reference.
//
// The adapter speaks the protocol and nothing more: it authenticates a
// request (AWS Signature Version 4), translates it into a call on a use
// case, and translates the result or the domain error back into an S3
// response. It decides nothing about who may do what — every use case passes
// the authorizer — and holds no storage logic.
package s3api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// DefaultRegion is the region reported when none is configured.
const DefaultRegion = "us-east-1"

const (
	requestIDSize = 8
	serverName    = "CactusFS"
	xmlHeader     = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
)

// Buckets is the bucket use cases the adapter calls. *app.BucketService
// implements it.
type Buckets interface {
	Create(ctx context.Context, principal iam.Principal, name string) error
	Delete(ctx context.Context, principal iam.Principal, name string) error
	Get(ctx context.Context, principal iam.Principal, name string) (bucket.Bucket, error)
	Location(ctx context.Context, principal iam.Principal, name string) error
	List(ctx context.Context, principal iam.Principal) ([]bucket.Bucket, error)
	CreateWithObjectLock(ctx context.Context, principal iam.Principal, name string) error
	ObjectLock(ctx context.Context, principal iam.Principal, name string) (bucket.Bucket, error)
	SetDefaultRetention(ctx context.Context, principal iam.Principal, name string, d *bucket.DefaultRetention) error
	Versioning(ctx context.Context, principal iam.Principal, name string) (bucket.Versioning, error)
	SetVersioning(ctx context.Context, principal iam.Principal, name string, state bucket.Versioning) error
	Policy(ctx context.Context, principal iam.Principal, name string) ([]byte, error)
	SetPolicy(ctx context.Context, principal iam.Principal, name string, document []byte) error
	CORS(ctx context.Context, principal iam.Principal, name string) (*bucket.CORS, error)
	SetCORS(ctx context.Context, principal iam.Principal, name string, rules *bucket.CORS) error
	CORSOf(ctx context.Context, name string) *bucket.CORS
	Tags(ctx context.Context, principal iam.Principal, name string) (map[string]string, error)
	SetTags(ctx context.Context, principal iam.Principal, name string, tags map[string]string) error
	Lifecycle(ctx context.Context, principal iam.Principal, name string) (*bucket.Lifecycle, error)
	SetLifecycle(ctx context.Context, principal iam.Principal, name string, rules *bucket.Lifecycle) error
}

// Objects is the object use cases the adapter calls. *app.ObjectService
// implements it.
type Objects interface {
	Put(ctx context.Context, principal iam.Principal, in app.PutInput) (object.Version, error)
	Get(ctx context.Context, principal iam.Principal, in app.GetInput) (app.GetOutput, error)
	Stat(ctx context.Context, principal iam.Principal, in app.GetInput) (object.Version, error)
	Copy(ctx context.Context, principal iam.Principal, in app.CopyInput) (app.CopyOutput, error)
	CopyPart(ctx context.Context, principal iam.Principal, in app.CopyPartInput) (object.Part, error)
	ListVersions(ctx context.Context, principal iam.Principal, bucketName string, opts app.VersionListOptions) (app.VersionListPage, error)
	Retention(ctx context.Context, principal iam.Principal, in app.LockInput) (object.Retention, error)
	SetRetention(ctx context.Context, principal iam.Principal, in app.LockInput, retention *object.Retention, bypassGovernance bool) (object.Version, error)
	LegalHold(ctx context.Context, principal iam.Principal, in app.LockInput) (bool, error)
	SetLegalHold(ctx context.Context, principal iam.Principal, in app.LockInput, on bool) (object.Version, error)
	Delete(ctx context.Context, principal iam.Principal, in app.DeleteInput) (app.DeleteOutput, error)
	List(ctx context.Context, principal iam.Principal, bucketName string, opts object.ListOptions) (object.ListPage, error)
	CreateUpload(ctx context.Context, principal iam.Principal, in app.CreateUploadInput) (object.Upload, error)
	UploadPart(ctx context.Context, principal iam.Principal, in app.UploadPartInput) (object.Part, error)
	CompleteUpload(ctx context.Context, principal iam.Principal, in app.CompleteUploadInput) (object.Version, error)
	AbortUpload(ctx context.Context, principal iam.Principal, in app.UploadRef) error
	ListParts(ctx context.Context, principal iam.Principal, in app.ListPartsInput) (app.ListPartsOutput, error)
	ListUploads(ctx context.Context, principal iam.Principal, in app.ListUploadsInput) (app.ListUploadsOutput, error)
	Tags(ctx context.Context, principal iam.Principal, in app.LockInput) (object.Version, error)
	SetTags(ctx context.Context, principal iam.Principal, in app.LockInput, tags map[string]string) (object.Version, error)
}

// Identities resolves an access key to its secret and principal.
// *app.IdentityService implements it.
type Identities interface {
	SecretFor(ctx context.Context, accessKeyID string) ([]byte, iam.Principal, error)
	// AuthenticationFailed is told of every request whose credentials were
	// not accepted, for the audit trail. reason is a short code and never
	// contains a secret.
	AuthenticationFailed(ctx context.Context, accessKeyID string, session iam.Session, reason string)
	// Authenticated is told that a request's signature verified.
	Authenticated(p iam.Principal)
}

// Observation describes one finished request, for metrics and traces.
type Observation struct {
	// Operation is the S3 operation name, such as "PutObject", or
	// "Unknown" when the request matched none.
	Operation string
	Status    int
	Duration  time.Duration
	BytesIn   int64
	BytesOut  int64
}

// Config configures a Handler.
type Config struct {
	Buckets    Buckets
	Objects    Objects
	Identities Identities
	Clock      ports.Clock
	Logger     *slog.Logger
	// Region is the region name reported to clients; DefaultRegion when
	// empty.
	Region string
	// Domain enables virtual-hosted-style requests: a request for host
	// "<bucket>.<Domain>" addresses that bucket. Empty disables it.
	Domain string
	// Observe, when not nil, is called after every request.
	Observe func(ctx context.Context, o Observation)
}

// Handler serves the S3 API.
type Handler struct {
	cfg Config
}

var _ http.Handler = (*Handler)(nil)

// NewHandler returns a Handler.
func NewHandler(cfg Config) *Handler {
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	cfg.Domain = strings.ToLower(strings.Trim(cfg.Domain, "."))
	return &Handler{cfg: cfg}
}

// request is the state of one request as it moves through the handler.
type request struct {
	*http.Request
	w         *responseWriter
	id        string
	bucket    string
	key       string
	operation string
	principal iam.Principal
	// payloadHash is the x-amz-content-sha256 value the signature covers.
	payloadHash string
	// chunks verifies the chunk signatures of a signed streaming body.
	chunks *chunkSigner
	// version is the version the request names with versionId, if any.
	version *object.VersionRef
}

// responseWriter records what was sent.
type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// WriteHeader implements http.ResponseWriter.
func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write implements http.ResponseWriter.
func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

type countingBody struct {
	io.ReadCloser
	bytes int64
}

// Read implements io.Reader.
func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	return n, err
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := h.cfg.Clock.Now()
	body := &countingBody{ReadCloser: r.Body}
	r.Body = body
	req := &request{Request: r, w: &responseWriter{ResponseWriter: w}, id: newRequestID(), operation: "Unknown"}

	header := w.Header()
	header.Set("Server", serverName)
	header.Set("X-Amz-Request-Id", req.id)
	header.Set("X-Content-Type-Options", "nosniff")

	ctx := app.WithRequestScope(r.Context())
	if err := h.serve(ctx, req); err != nil {
		h.writeError(ctx, req, err)
	}

	duration := h.cfg.Clock.Now().Sub(started)
	h.cfg.Logger.InfoContext(ctx, "s3 request",
		slog.String("request_id", req.id),
		slog.String("operation", req.operation),
		slog.String("method", r.Method),
		slog.String("bucket", req.bucket),
		slog.Int("status", req.w.status),
		slog.String("access_key_id", req.principal.AccessKeyID),
		slog.Int64("bytes_in", body.bytes),
		slog.Int64("bytes_out", req.w.bytes),
		slog.Duration("duration", duration),
	)
	if h.cfg.Observe != nil {
		h.cfg.Observe(ctx, Observation{
			Operation: req.operation, Status: req.w.status, Duration: duration, BytesIn: body.bytes, BytesOut: req.w.bytes,
		})
	}
}

func newRequestID() string {
	var id [requestIDSize]byte
	// crypto/rand.Read does not fail on supported platforms.
	_, _ = rand.Read(id[:])
	return strings.ToUpper(hex.EncodeToString(id[:]))
}

// serve authenticates the request, then routes it. No body is read before
// authentication, and nothing about an object is revealed. One thing about a
// bucket is: the cross-origin headers its rules give an origin are sent with
// every answer, a refusal included, as a browser needs them - so a bucket
// with rules for an origin can be told from one without.
func (h *Handler) serve(ctx context.Context, req *request) error {
	h.address(req)
	if req.Method == http.MethodOptions {
		return h.preflight(ctx, req)
	}
	h.crossOrigin(ctx, req)
	if err := h.authenticate(ctx, req); err != nil {
		if code := toAPIError(err); code.status == http.StatusForbidden {
			// Refused credentials, as opposed to a request this adapter
			// cannot serve: that goes to the audit trail.
			h.cfg.Identities.AuthenticationFailed(ctx, claimedAccessKey(req.Request), sessionOf(req), code.code)
		}
		h.cfg.Logger.WarnContext(ctx, "authentication failed",
			slog.String("request_id", req.id),
			slog.String("remote", remoteHost(req.RemoteAddr)),
			slog.String("reason", toAPIError(err).code),
		)
		return err
	}
	return h.route(ctx, req)
}

func remoteHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// address works out which bucket and key the request names, from the Host
// header (virtual-hosted style) or the path (path style).
func (h *Handler) address(req *request) {
	path := strings.TrimPrefix(req.URL.Path, "/")
	if name, ok := h.virtualHostBucket(req.Host); ok {
		req.bucket, req.key = name, path
		return
	}
	req.bucket, req.key, _ = strings.Cut(path, "/")
}

func (h *Handler) virtualHostBucket(host string) (string, bool) {
	if h.cfg.Domain == "" {
		return "", false
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	name, ok := strings.CutSuffix(strings.ToLower(host), "."+h.cfg.Domain)
	return name, ok && name != ""
}

// authenticate verifies the request's signature and sets the principal.
func (h *Handler) authenticate(ctx context.Context, req *request) error {
	query := req.URL.Query()
	header := req.Header.Get("Authorization")
	if query.Has("Signature") || query.Has("AWSAccessKeyId") {
		// Signature Version 2 in the query string.
		return fmt.Errorf("%w: Signature Version 2", errAuthUnsupported)
	}
	var (
		sig    signature
		signed presigned
		err    error
	)
	viaQuery := isPresigned(query)
	switch {
	case viaQuery && header != "":
		// Two signatures: which one counts must not be ours to choose.
		return fmt.Errorf("%w: both a presigned query and an Authorization header", errAuthQuery)
	case viaQuery:
		if signed, err = parsePresigned(query); err != nil {
			return err
		}
		sig = signed.signature
	case header == "" && (req.Method == http.MethodGet || req.Method == http.MethodHead):
		// No credentials, and a request that only reads: the caller is
		// nobody in particular. Whether nobody may read this is the
		// authorizer's to say; without a bucket policy that opens it,
		// the answer is no.
		req.principal = anonymous(req)
		return nil
	case header == "":
		return errAuthMissing
	default:
		if sig, err = parseAuthorization(header); err != nil {
			return err
		}
	}
	secret, principal, err := h.cfg.Identities.SecretFor(ctx, sig.accessKeyID)
	if err != nil {
		return err
	}
	defer clear(secret)
	var checked verified
	if viaQuery {
		checked, err = verifyPresigned(req.Request, signed, secret, h.cfg.Clock.Now())
	} else {
		checked, err = verifySignature(req.Request, sig, secret, h.cfg.Clock.Now())
	}
	if err != nil {
		return err
	}
	if viaQuery {
		// The signature has done its work; what is left of the query
		// string is the request.
		req.URL.RawQuery = withoutParameter(req.URL.RawQuery, presignedParameters()...)
		// A signer may put x-amz-* headers into the query string, where
		// the signature covers them; SDKs do. They mean what the header
		// would mean, and are judged as headers from here on.
		var hoisted []string
		for name, values := range req.URL.Query() {
			if !strings.HasPrefix(strings.ToLower(name), "x-amz-") {
				continue
			}
			if req.Header.Get(name) != "" || len(values) != 1 {
				return fmt.Errorf("%w: %s is given more than once", errAuthQuery, name)
			}
			// A key in a URL is in every log the URL passes through.
			if strings.EqualFold(name, headerSSECKey) || strings.EqualFold(name, copySourceSSEHeaders()[1]) {
				return fmt.Errorf("%w: a customer key is not accepted in a query string", errAuthQuery)
			}
			req.Header.Set(name, values[0])
			hoisted = append(hoisted, name)
		}
		req.URL.RawQuery = withoutParameter(req.URL.RawQuery, hoisted...)
	}
	payloadHash := checked.payloadHash
	req.chunks = checked.chunks
	// Where and how the request arrived is part of who is asking:
	// conditions in policies test it.
	principal.Session = sessionOf(req)
	h.cfg.Identities.Authenticated(principal)
	req.principal, req.payloadHash = principal, payloadHash
	return nil
}

// sessionOf describes the connection a request arrived on.
func sessionOf(req *request) iam.Session {
	session := iam.Session{SecureTransport: req.TLS != nil, RequestID: req.id}
	if addr, err := netip.ParseAddrPort(req.RemoteAddr); err == nil {
		session.SourceIP = addr.Addr().Unmap()
	}
	return session
}

// claimedAccessKey returns the access key ID a request says it is signed
// with, or "" when it says nothing that can be read.
func claimedAccessKey(r *http.Request) string {
	sig, err := parseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return ""
	}
	return sig.accessKeyID
}

// queryClientInfo is a query parameter AWS SDKs add to name the operation
// they are calling. It selects nothing.
const queryClientInfo = "x-id"

// allowQuery refuses a request that carries a query parameter the routed
// operation does not implement. S3 selects whole features with query
// parameters (?acl, ?uploadId, ?versionId, ...), and new ones keep being
// added, so the rule is an allow-list: a parameter this adapter does not know
// answers 501 and is never ignored. Ignoring one would serve a different
// operation than the client asked for - a DELETE ?uploadId would delete the
// object.
func allowQuery(query url.Values, allowed ...string) error {
	for name := range query {
		if name != queryClientInfo && !slices.Contains(allowed, name) {
			return errNotImplemented()
		}
	}
	return nil
}

// conditionalHeaders are the conditions HTTP lets a request put on what it
// is about. They are read on GET and HEAD of an object; on a write they ask
// for a conditional write, which is not implemented.
func conditionalHeaders() []string {
	return []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"}
}

// x-amz-* request headers that are understood. Every other x-amz-* header
// asks for something that is not implemented.
func knownAmzHeaders() []string {
	return []string{
		"x-amz-date", "x-amz-content-sha256", "x-amz-decoded-content-length", "x-amz-trailer",
		"x-amz-user-agent", "x-amz-sdk-checksum-algorithm",
	}
}

// allowHeaders refuses a request with a header that would change its meaning
// in a way this adapter does not implement: conditional requests, object
// lock, customer-provided or KMS encryption, tagging, ACLs, copies and so on.
// A client that sets "If-None-Match: *" to avoid overwriting, or an object
// lock header to protect a version, must not be told that it worked.
func allowHeaders(header http.Header) error {
	for name, values := range header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		switch {
		case slices.Contains(knownAmzHeaders(), lower), slices.Contains(lockHeaders(), lower):
		case strings.HasPrefix(lower, "x-amz-meta-"), strings.HasPrefix(lower, checksumHeaderPrefix):
		case slices.Contains(copyHeaders(), lower):
			// Read by the copy operations; refused elsewhere, in route.
		case lower == headerTagging:
			// Read by PutObject; refused elsewhere, in route.
		case isSSEHeader(lower):
			// Read, or refused, by the operation: see allowSSEHeaders.
		case lower == "x-amz-storage-class" && len(values) == 1 && values[0] == storageClass:
		default:
			return errNotImplemented()
		}
	}
	return nil
}

func (h *Handler) route(ctx context.Context, req *request) error {
	// A query string that does not parse is refused: a lenient parser
	// would drop the malformed pair and with it the feature it selects.
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return apiError{http.StatusBadRequest, "InvalidURI", "Couldn't parse the specified URI."}
	}
	if err := allowHeaders(req.Header); err != nil {
		return err
	}
	if err := allowSSEHeaders(req, query); err != nil {
		return err
	}
	// Conditions are read where an object is read, and nowhere else: a
	// client that says "If-None-Match: *" to keep from overwriting must
	// not be told that it worked.
	plain := true
	for name := range query {
		plain = plain && (name == queryClientInfo || name == "versionId")
	}
	reads := req.key != "" && plain && (req.Method == http.MethodGet || req.Method == http.MethodHead)
	for _, name := range conditionalHeaders() {
		if len(req.Header.Values(name)) > 0 && !reads {
			return errNotImplemented()
		}
	}
	// And what belongs to a copy comes with a copy: a PUT of an object
	// or of a part that names a source.
	// - and nothing else: a PUT of tags or of a retention that names a
	// source is not a copy, and would ignore it.
	part := query.Has("uploadId") && query.Has("partNumber")
	for name := range query {
		part = part && (name == queryClientInfo || name == "uploadId" || name == "partNumber")
	}
	onlyInfo := len(query) == 0 || (len(query) == 1 && query.Has(queryClientInfo))
	copies := req.Method == http.MethodPut && req.key != "" && (onlyInfo || part) && len(req.Header.Values(headerCopySource)) > 0
	for _, name := range copyHeaders() {
		if len(req.Header.Values(name)) > 0 && !copies {
			return fmt.Errorf("%w: %s is not part of this operation", domain.ErrInvalidArgument, name)
		}
	}
	// Tags come with the request that creates an object, and no other.
	selects := len(query)
	if query.Has(queryClientInfo) {
		selects--
	}
	if len(req.Header.Values(headerTagging)) > 0 && (req.Method != http.MethodPut || req.key == "" || selects > 0) {
		return errNotImplemented()
	}

	switch {
	case req.bucket == "":
		if req.Method != http.MethodGet {
			return errMethodNotAllowed()
		}
		if err := allowQuery(query); err != nil {
			return err
		}
		req.operation = "ListBuckets"
		return h.listBuckets(ctx, req)
	case req.key == "":
		return h.routeBucket(ctx, req, query)
	default:
		return h.routeObject(ctx, req, query)
	}
}

func (h *Handler) routeBucket(ctx context.Context, req *request, query url.Values) error {
	if handled, err := h.routeBucketConfig(ctx, req, query); handled {
		return err
	}
	switch req.Method {
	case http.MethodPut:
		if query.Has("versioning") {
			if err := allowQuery(query, "versioning"); err != nil {
				return err
			}
			req.operation = "PutBucketVersioning"
			return h.putBucketVersioning(ctx, req)
		}
		if query.Has("object-lock") {
			if err := allowQuery(query, "object-lock"); err != nil {
				return err
			}
			req.operation = "PutObjectLockConfiguration"
			return h.putObjectLockConfiguration(ctx, req)
		}
		if err := allowQuery(query); err != nil {
			return err
		}
		req.operation = "CreateBucket"
		return h.createBucket(ctx, req)
	case http.MethodPost:
		if query.Has("delete") {
			if err := allowQuery(query, "delete"); err != nil {
				return err
			}
			req.operation = "DeleteObjects"
			return h.deleteObjects(ctx, req)
		}
		if err := allowQuery(query); err != nil {
			return err
		}
		return errMethodNotAllowed()
	case http.MethodDelete:
		if err := allowQuery(query); err != nil {
			return err
		}
		req.operation = "DeleteBucket"
		return h.deleteBucket(ctx, req)
	case http.MethodHead:
		if err := allowQuery(query); err != nil {
			return err
		}
		req.operation = "HeadBucket"
		return h.headBucket(ctx, req)
	case http.MethodGet:
		switch {
		case query.Has("versioning"):
			if err := allowQuery(query, "versioning"); err != nil {
				return err
			}
			req.operation = "GetBucketVersioning"
			return h.getBucketVersioning(ctx, req)
		case query.Has("object-lock"):
			if err := allowQuery(query, "object-lock"); err != nil {
				return err
			}
			req.operation = "GetObjectLockConfiguration"
			return h.getObjectLockConfiguration(ctx, req)
		case query.Has("uploads"):
			if err := allowQuery(query, listUploadsParameters()...); err != nil {
				return err
			}
			req.operation = "ListMultipartUploads"
			return h.listMultipartUploads(ctx, req, query)
		case query.Has("versions"):
			if err := allowQuery(query, listVersionsParameters()...); err != nil {
				return err
			}
			req.operation = "ListObjectVersions"
			return h.listObjectVersions(ctx, req, query)
		case query.Has("location"):
			if err := allowQuery(query, "location"); err != nil {
				return err
			}
			req.operation = "GetBucketLocation"
			return h.getBucketLocation(ctx, req)
		case query.Get("list-type") == "2":
			if err := allowQuery(query, listObjectsV2Parameters()...); err != nil {
				return err
			}
			req.operation = "ListObjectsV2"
			return h.listObjectsV2(ctx, req)
		default:
			// ListObjects version 1 - or a sub-resource this adapter
			// does not know, which the allow-list refuses.
			if err := allowQuery(query, listObjectsV1Parameters()...); err != nil {
				return err
			}
			req.operation = "ListObjects"
			return h.listObjectsV1(ctx, req, query)
		}
	default:
		// POST ?delete and the like: a feature, not a wrong method.
		if err := allowQuery(query); err != nil {
			return err
		}
		return errMethodNotAllowed()
	}
}

func listObjectsV2Parameters() []string {
	return []string{"list-type", "prefix", "delimiter", "max-keys", "start-after", "continuation-token", "encoding-type", "fetch-owner"}
}

// routeUpload routes the operations of a multipart upload, which the
// uploads and uploadId parameters select. handled is false for a request
// that is none of them.
func (h *Handler) routeUpload(ctx context.Context, req *request, query url.Values) (handled bool, err error) {
	switch {
	case query.Has("uploads"):
		if req.Method != http.MethodPost {
			return true, errMethodNotAllowed()
		}
		if err := allowQuery(query, "uploads"); err != nil {
			return true, err
		}
		req.operation = "CreateMultipartUpload"
		return true, h.createMultipartUpload(ctx, req)
	case !query.Has("uploadId"):
		return false, nil
	}
	switch req.Method {
	case http.MethodPut:
		if err := allowQuery(query, "uploadId", "partNumber"); err != nil {
			return true, err
		}
		if len(req.Header.Values(headerCopySource)) > 0 {
			req.operation = "UploadPartCopy"
			return true, h.uploadPartCopy(ctx, req, query)
		}
		req.operation = "UploadPart"
		return true, h.uploadPart(ctx, req, query)
	case http.MethodPost:
		if err := allowQuery(query, "uploadId"); err != nil {
			return true, err
		}
		req.operation = "CompleteMultipartUpload"
		return true, h.completeMultipartUpload(ctx, req, query)
	case http.MethodDelete:
		if err := allowQuery(query, "uploadId"); err != nil {
			return true, err
		}
		req.operation = "AbortMultipartUpload"
		return true, h.abortMultipartUpload(ctx, req, query)
	case http.MethodGet:
		if err := allowQuery(query, listPartsParameters()...); err != nil {
			return true, err
		}
		req.operation = "ListParts"
		return true, h.listParts(ctx, req, query)
	default:
		return true, errMethodNotAllowed()
	}
}

func (h *Handler) routeObject(ctx context.Context, req *request, query url.Values) error {
	if handled, err := h.routeUpload(ctx, req, query); handled {
		return err
	}
	// Reading and deleting take versionId. partNumber, uploadId, tagging,
	// the response-* overrides and the rest all select features that do
	// not exist yet.
	allowed := []string{}
	if req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodDelete {
		allowed = append(allowed, "versionId")
	}
	// The lock of a version is read and changed with GET and PUT; its
	// tags also removed with DELETE.
	sub := ""
	if req.Method == http.MethodGet || req.Method == http.MethodPut {
		for _, name := range []string{"retention", "legal-hold"} {
			if query.Has(name) {
				sub, allowed = name, []string{name, "versionId"}
			}
		}
	}
	if query.Has("tagging") && (req.Method == http.MethodGet || req.Method == http.MethodPut || req.Method == http.MethodDelete) {
		sub, allowed = "tagging", []string{"tagging", "versionId"}
	}
	if err := allowQuery(query, allowed...); err != nil {
		return err
	}
	var err error
	if req.version, err = versionOf(query); err != nil {
		return err
	}
	switch {
	case sub == "retention" && req.Method == http.MethodGet:
		req.operation = "GetObjectRetention"
		return h.getObjectRetention(ctx, req)
	case sub == "retention":
		req.operation = "PutObjectRetention"
		return h.putObjectRetention(ctx, req)
	case sub == "legal-hold" && req.Method == http.MethodGet:
		req.operation = "GetObjectLegalHold"
		return h.getObjectLegalHold(ctx, req)
	case sub == "legal-hold":
		req.operation = "PutObjectLegalHold"
		return h.putObjectLegalHold(ctx, req)
	case sub == "tagging" && req.Method == http.MethodGet:
		req.operation = "GetObjectTagging"
		return h.getObjectTagging(ctx, req)
	case sub == "tagging" && req.Method == http.MethodPut:
		req.operation = "PutObjectTagging"
		return h.putObjectTagging(ctx, req)
	case sub == "tagging":
		req.operation = "DeleteObjectTagging"
		return h.deleteObjectTagging(ctx, req)
	}
	switch req.Method {
	case http.MethodPut:
		if len(req.Header.Values(headerCopySource)) > 0 {
			req.operation = "CopyObject"
			return h.copyObject(ctx, req)
		}
		req.operation = "PutObject"
		return h.putObject(ctx, req)
	case http.MethodGet:
		req.operation = "GetObject"
		return h.getObject(ctx, req)
	case http.MethodHead:
		req.operation = "HeadObject"
		return h.headObject(ctx, req)
	case http.MethodDelete:
		req.operation = "DeleteObject"
		return h.deleteObject(ctx, req)
	default:
		return errMethodNotAllowed()
	}
}

// writeError sends an S3 error response. Causes that the client must not
// learn about are logged here and reported as InternalError.
func (h *Handler) writeError(ctx context.Context, req *request, err error) {
	api := toAPIError(err)
	if api.status >= http.StatusInternalServerError && api.status != http.StatusNotImplemented {
		h.cfg.Logger.ErrorContext(ctx, "s3 request failed",
			slog.String("request_id", req.id), slog.String("operation", req.operation), slog.Any("error", err))
	}
	if req.w.status != 0 {
		// The response has begun; all that is left is to cut it short so
		// that the client sees a failed transfer, not a complete object.
		abortResponse(req)
		return
	}
	if api.status >= http.StatusInternalServerError && !req.principal.Anonymous() {
		// A caller we know is told that the store cannot serve it right
		// now, and must be able to hear it. A client that is still
		// sending its upload when the connection closes sees a reset and
		// never the answer, so a body of modest size is taken off the
		// wire first. Nothing is read for a caller who did not
		// authenticate.
		_, _ = io.CopyN(io.Discard, req.Body, maxDrainedBody)
	}
	if req.Method == http.MethodHead {
		req.w.WriteHeader(api.status)
		return
	}
	h.writeXML(ctx, req, api.status, errorResponse{
		Code: api.code, Message: api.message, Resource: req.URL.Path, RequestID: req.id,
	})
}

// maxDrainedBody is how much of an unread request body is discarded before
// an answer that says "try again".
const maxDrainedBody = 1 << 20

// abortResponse ends a response that has already started. With the write
// deadline in the past nothing more can be sent, and net/http closes the
// connection instead of completing the response: the client receives fewer
// bytes than the Content-Length announced and reports a failed transfer.
func abortResponse(req *request) {
	_ = http.NewResponseController(req.w.ResponseWriter).SetWriteDeadline(time.Unix(1, 0))
}

func (h *Handler) writeXML(ctx context.Context, req *request, status int, body any) {
	data, err := xml.Marshal(body)
	if err != nil {
		h.cfg.Logger.ErrorContext(ctx, "encode response", slog.String("request_id", req.id), slog.Any("error", err))
		req.w.WriteHeader(http.StatusInternalServerError)
		return
	}
	req.w.Header().Set("Content-Type", "application/xml")
	req.w.WriteHeader(status)
	if _, err := io.WriteString(req.w, xmlHeader); err == nil {
		_, err = req.w.Write(data)
		if err != nil && !errors.Is(err, context.Canceled) {
			h.cfg.Logger.DebugContext(ctx, "response not delivered", slog.String("request_id", req.id), slog.Any("error", err))
		}
	}
}
