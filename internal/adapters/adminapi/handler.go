// Package adminapi is the inbound adapter of the management API: REST and
// JSON under /api/v1, as api/openapi/admin.yaml defines it.
//
// The routing, the parameter binding and the request and response types are
// generated from that contract (package api). This package authenticates the
// caller, turns requests into calls of the use cases and their results into
// responses, and nothing else: whether a caller may do something is decided
// by the authorizer inside the use cases, never here.
package adminapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/adminapi/api"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// BasePath is where the API is served.
const BasePath = "/api/v1"

const (
	headerRequestID   = "X-Request-Id"
	headerIdempotency = "Idempotency-Key"
	headerReplayed    = "Idempotent-Replayed"
	// maxJSONBody bounds every request body but an upload.
	maxJSONBody = 1 << 20
	// maxIdempotencyKey is the longest idempotency key accepted.
	maxIdempotencyKey = 128
	// completeTimeout bounds storing the answer of an idempotent request
	// once the request itself is over.
	completeTimeout = 30 * time.Second
)

// The two operations that carry an object, as the generated router names
// them.
const (
	patternUpload   = "PUT " + BasePath + "/buckets/{bucket}/object"
	patternDownload = "GET " + BasePath + "/buckets/{bucket}/object"
)

// patternUnlock is the operation that unlocks a session. Its body holds a
// password, and nothing of it is kept: not a fingerprint either.
const patternUnlock = "POST " + BasePath + "/session/unlock"

// Identities resolves API keys. It is implemented by app.IdentityService.
type Identities interface {
	// SecretFor returns the secret of an access key and the principal it
	// stands for; the error wraps domain.ErrCredentialNotFound for a key
	// that does not exist or does not work any more.
	SecretFor(ctx context.Context, accessKeyID string) ([]byte, iam.Principal, error)
	// Authenticated is told that a request proved it holds the secret.
	Authenticated(p iam.Principal)
	// AuthenticationFailed records a request that did not.
	AuthenticationFailed(ctx context.Context, accessKeyID string, session iam.Session, reason string)
}

// Observation describes one finished request, for metrics and traces.
type Observation struct {
	// Operation is the route that was matched, such as
	// "GET /api/v1/buckets/{bucket}", or "Unknown".
	Operation string
	Status    int
	Duration  time.Duration
}

// Config lists what a Handler depends on.
type Config struct {
	Buckets  *app.BucketService
	Objects  *app.ObjectService
	Search   *app.SearchService
	IAM      *app.IAMService
	Sessions *app.SessionService
	Cluster  *app.ClusterService
	// Settings answers how the nodes are configured. It may be nil on a
	// node that was not given the use case: the operation then answers 501.
	Settings    *app.SettingsService
	Idempotency *app.IdempotencyService
	Identities  Identities
	Clock       ports.Clock
	Logger      *slog.Logger
	// SpecYAML and SpecJSON are the contract, served as it is.
	SpecYAML []byte
	SpecJSON []byte
	// Observe, when not nil, is called after every request.
	Observe func(ctx context.Context, o Observation)
	// Build is which build this node runs. It is told to callers who have
	// been authenticated and to nobody else: only the operations of the
	// contract that need a credential name it.
	Build buildinfo.Info
}

// Handler serves the management API.
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler returns the handler of everything under BasePath.
func NewHandler(cfg Config) *Handler {
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	strict := api.NewStrictHandlerWithOptions(&operations{h: h}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			h.fail(w, r, errors.Join(domain.ErrInvalidArgument, err))
		},
		ResponseErrorHandlerFunc: h.fail,
	})
	api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		BaseURL: BasePath, BaseRouter: h.mux, Middlewares: []api.MiddlewareFunc{h.pipeline},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			h.fail(w, r, errors.Join(domain.ErrInvalidArgument, err))
		},
	})
	h.mux.HandleFunc("GET "+BasePath+"/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		writeBytes(w, "application/json", cfg.SpecJSON)
	})
	h.mux.HandleFunc("GET "+BasePath+"/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		writeBytes(w, "application/yaml", cfg.SpecYAML)
	})
	h.mux.HandleFunc(BasePath+"/", func(w http.ResponseWriter, r *http.Request) {
		h.problem(w, r, problem{status: http.StatusNotFound, code: "NotFound", title: "No such endpoint"})
	})
	return h
}

func writeBytes(w http.ResponseWriter, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// requestInfo is what the adapter knows about the request being served.
type requestInfo struct {
	id      string
	session iam.Session
	// principal is who makes the request, once authenticated.
	principal iam.Principal
	// token is the session token the request came with, when it was
	// authenticated by a session.
	token string
	// sessionEnded is set by the operation that ended the session of the
	// request: the answer then makes the browser forget its cookie.
	sessionEnded bool
	// contentLength is the declared length of the request body, or -1.
	contentLength int64
	// started is set once the response has begun.
	started bool
}

type infoKey struct{}

func infoOf(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(infoKey{}).(*requestInfo)
	if info == nil {
		return &requestInfo{}
	}
	return info
}

// statusWriter remembers whether, and how, a response began.
type statusWriter struct {
	http.ResponseWriter
	info   *requestInfo
	status int
}

// WriteHeader implements http.ResponseWriter.
func (w *statusWriter) WriteHeader(status int) {
	if !w.info.started {
		w.info.started, w.status = true, status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write implements http.ResponseWriter.
func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.info.started {
		w.info.started, w.status = true, http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func newRequestID() string {
	var id [12]byte
	// crypto/rand.Read does not fail on supported platforms.
	_, _ = rand.Read(id[:])
	return strings.ToUpper(hex.EncodeToString(id[:]))
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	begun := h.cfg.Clock.Now()
	info := &requestInfo{id: newRequestID(), contentLength: r.ContentLength}
	info.session = iam.Session{SecureTransport: r.TLS != nil, RequestID: info.id}
	if addr, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		info.session.SourceIP = addr.Addr().Unmap()
	}
	header := w.Header()
	header.Set(headerRequestID, info.id)
	header.Set("X-Content-Type-Options", "nosniff")
	// Answers are about the caller and the moment: nothing may keep them.
	header.Set("Cache-Control", "no-store")

	sw := &statusWriter{ResponseWriter: w, info: info}
	ctx := context.WithValue(r.Context(), infoKey{}, info)
	r = r.WithContext(ctx)
	h.mux.ServeHTTP(sw, r)

	if h.cfg.Observe != nil {
		operation := r.Pattern
		if operation == "" || strings.HasSuffix(operation, BasePath+"/") {
			operation = "Unknown"
		}
		h.cfg.Observe(ctx, Observation{Operation: operation, Status: sw.status, Duration: h.cfg.Clock.Now().Sub(begun)})
	}
}

// pipeline runs between the generated routing and every operation of the
// contract: it authenticates, bounds the body and makes the request safe to
// repeat. The generated code has, by then, marked in the context whether the
// contract asks for a credential.
func (h *Handler) pipeline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		info := infoOf(ctx)
		public := ctx.Value(api.ApiKeyScopes) == nil && ctx.Value(api.SessionScopes) == nil
		if !public {
			if err := h.authenticate(r, info); err != nil {
				h.fail(w, r, err)
				return
			}
		}
		// Which operation this is, is what the router matched - never what
		// the path looks like: a user can be named "object".
		upload := r.Pattern == patternUpload
		download := r.Pattern == patternDownload && r.Method == http.MethodGet
		if r.Pattern == patternDownload && r.Method != http.MethodGet {
			// HEAD would fetch and decrypt an object to throw it away;
			// what is known about an object has its own operation.
			w.Header().Set("Allow", http.MethodGet)
			h.problem(w, r, problem{status: http.StatusMethodNotAllowed, code: "MethodNotAllowed", title: "Method not allowed"})
			return
		}
		if upload || download {
			// The listener bounds a whole request and response, for
			// messages that are small. An object is not, and its caller
			// is known by now: the transfer takes as long as it takes.
			transfer := http.NewResponseController(w)
			_ = transfer.SetReadDeadline(time.Time{})
			_ = transfer.SetWriteDeadline(time.Time{})
		}
		if !upload {
			if r.Method == http.MethodPost || r.Method == http.MethodPut {
				if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
					h.problem(w, r, problem{
						status: http.StatusUnsupportedMediaType, code: "UnsupportedMediaType", title: "Unsupported media type",
						detail: "the body of this request must be application/json",
					})
					return
				}
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
		}
		if upload && info.token != "" {
			// The session use case is told that an upload of the session
			// is in flight on this node, and when it no longer is: what a
			// session that reaches its timeout meanwhile then is, it
			// decides (ADR-0027). Nothing is decided here.
			defer h.cfg.Sessions.UploadBegun(ctx, info.token)()
		}
		key := r.Header.Get(headerIdempotency)
		if key == "" || public || !httpsession.Mutating(r) || r.Pattern == patternUnlock {
			// Unlocking takes the header and does nothing with it, as
			// signing in does: its body holds a password, of which not
			// even a hash is kept, and its answer holds a session, which
			// is never given twice.
			next.ServeHTTP(w, r)
			return
		}
		h.idempotent(w, r, info, key, upload, next)
	})
}

var errUnauthenticated = errors.New("not authenticated")

// errNotImplemented is the error of an operation of the contract that this
// node was not given the use case for.
var errNotImplemented = errors.New("not available on this node")

// authenticate establishes who makes the request: the owner of an API key
// sent as a bearer credential, or the user of a console session.
func (h *Handler) authenticate(r *http.Request, info *requestInfo) error {
	ctx := r.Context()
	refuse := func(accessKeyID, reason string) error {
		h.cfg.Identities.AuthenticationFailed(ctx, accessKeyID, info.session, reason)
		return errUnauthenticated
	}
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		scheme, credential, _ := strings.Cut(authorization, " ")
		id, secret, ok := strings.Cut(strings.TrimSpace(credential), ":")
		if !strings.EqualFold(scheme, "Bearer") || !ok || id == "" || secret == "" {
			return refuse("", "malformed Authorization header")
		}
		want, principal, err := h.cfg.Identities.SecretFor(ctx, id)
		if errors.Is(err, domain.ErrCredentialNotFound) {
			return refuse(id, "unknown or expired access key")
		}
		if err != nil {
			return err
		}
		defer clear(want)
		if subtle.ConstantTimeCompare(want, []byte(secret)) != 1 {
			return refuse(id, "wrong secret key")
		}
		principal.Session = info.session
		h.cfg.Identities.Authenticated(principal)
		info.principal = principal
		return nil
	}
	if token := httpsession.Token(r); token != "" {
		// The session use case records its own refusals.
		principal, err := h.cfg.Sessions.Authenticate(ctx, token, httpsession.CSRFToken(r), httpsession.Mutating(r), info.session)
		if errors.Is(err, domain.ErrAccessDenied) {
			return errors.Join(errUnauthenticated, err)
		}
		if err != nil {
			return err
		}
		info.principal, info.token = principal, token
		return nil
	}
	return refuse("", "no credential")
}

// scopeOf names whose idempotency keys a request's key belongs with.
func scopeOf(info *requestInfo) string {
	switch p := info.principal; {
	case info.token != "":
		return "session:" + p.User
	case p.Root:
		return "root:" + p.AccessKeyID
	default:
		return "key:" + p.AccessKeyID
	}
}

// answerRecorder passes a response on and keeps a copy of it.
type answerRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	// tooLarge is set once the body no longer fits what is kept.
	tooLarge bool
}

// WriteHeader implements http.ResponseWriter.
func (a *answerRecorder) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
	a.ResponseWriter.WriteHeader(status)
}

// Write implements http.ResponseWriter.
func (a *answerRecorder) Write(p []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	if a.body.Len()+len(p) > app.MaxIdempotentAnswer {
		a.tooLarge = true
	} else {
		a.body.Write(p)
	}
	return a.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection.
func (a *answerRecorder) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// idempotent serves a request that carries an idempotency key.
func (h *Handler) idempotent(w http.ResponseWriter, r *http.Request, info *requestInfo, key string, upload bool, next http.Handler) {
	ctx := r.Context()
	if len(key) > maxIdempotencyKey || strings.ContainsFunc(key, func(c rune) bool { return c < 0x21 || c > 0x7e }) {
		h.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("Idempotency-Key must be 1 to 128 printable ASCII characters")))
		return
	}
	// What makes two requests the same request. An upload is not read
	// twice to find out: its length and type stand for its content.
	var content string
	if upload {
		content = "length:" + strconv.FormatInt(r.ContentLength, 10) + ";type:" + r.Header.Get("Content-Type")
	} else {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// Too large maps to 413 by itself; anything else is a
			// request that did not arrive whole.
			h.fail(w, r, errors.Join(domain.ErrIncompleteBody, err))
			return
		}
		sum := sha256.Sum256(body)
		content = hex.EncodeToString(sum[:])
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	answer, reservation, err := h.cfg.Idempotency.Begin(ctx, app.IdempotentRequest{
		Scope: scopeOf(info), Key: key, Fingerprint: strings.Join([]string{r.Method, r.URL.Path, r.URL.RawQuery, content}, "\n"),
		// Who asks, and whether for an upload: what follows from that is
		// the use case's to say.
		Principal: info.principal, Upload: upload,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if answer != nil {
		if answer.ContentType != "" {
			w.Header().Set("Content-Type", answer.ContentType)
		}
		w.Header().Set(headerReplayed, "true")
		w.WriteHeader(answer.Status)
		_, _ = w.Write(answer.Body)
		return
	}

	recorder := &answerRecorder{ResponseWriter: w}
	next.ServeHTTP(recorder, r)

	// The request is over; its answer is stored whether or not the client
	// is still there.
	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	if recorder.tooLarge || recorder.status == 0 || recorder.status == http.StatusLocked {
		// Not an answer to keep. That of a locked session least of all: it
		// is of the moment, and the repeat that comes once the session is
		// unlocked is to be carried out, not told "locked" for a day.
		err = h.cfg.Idempotency.Abandon(done, reservation)
	} else {
		err = h.cfg.Idempotency.Complete(done, reservation, app.IdempotentAnswer{
			Status: recorder.status, ContentType: recorder.Header().Get("Content-Type"), Body: recorder.body.Bytes(),
		})
	}
	if err != nil {
		h.cfg.Logger.ErrorContext(ctx, "the answer of an idempotent request could not be stored",
			slog.String("request_id", info.id), slog.Any("error", err))
	}
}

// codeTooManyRequests is the code of the problem that says "again in a
// moment", as against the one that makes a caller wait after failed
// sign-ins.
const codeTooManyRequests = "TooManyRequests"

// detailTooManyBuckets is what a caller is told when a search could not
// decide in its time which buckets are theirs. It is said in so many words
// because the answer is a 503 with Retry-After like any other, and unlike
// any other it may be no passing state.
const detailTooManyBuckets = "Deciding which buckets you may search took longer than a search has. " +
	"A node that was started not long ago remembers more of the bucket policies with every search and gets further each time: ask again. " +
	"If the answer stays the same, this deployment has more buckets with policies than a search can decide in its time - " +
	"a limit of the deployment's size, not an outage. A search of one bucket, and a search by the root identity, are not subject to it."

// problem is a problem+json answer in the making.
type problem struct {
	status int
	code   string
	title  string
	detail string
}

// problemOf maps an error of the use cases to an answer. This is the only
// place where domain errors become HTTP.
func problemOf(err error) problem {
	is := func(target error) bool { return errors.Is(err, target) }
	var tooLarge *http.MaxBytesError
	switch {
	case is(domain.ErrEntityTooLarge), errors.As(err, &tooLarge):
		return problem{status: http.StatusRequestEntityTooLarge, code: "EntityTooLarge", title: "Request body too large"}
	case is(errUnauthenticated), is(domain.ErrCredentialNotFound):
		return problem{status: http.StatusUnauthorized, code: "Unauthenticated", title: "Authentication required"}
	case is(domain.ErrTooManyAttempts):
		return problem{status: http.StatusTooManyRequests, code: "TooManyAttempts", title: "Too many attempts"}
	case is(domain.ErrTooManyRequests):
		return problem{status: http.StatusTooManyRequests, code: codeTooManyRequests, title: "Too many requests at once"}
	case is(domain.ErrSessionLocked):
		// Not a 401, which says that the session is gone, and not a denial:
		// the session is there and waits for its user's password (ADR-0027).
		// It is asked before the denial, so that an error that names both is
		// told as the lock.
		return problem{status: http.StatusLocked, code: "SessionLocked", title: "The session is locked"}
	case is(domain.ErrAccessDenied):
		return problem{status: http.StatusForbidden, code: "AccessDenied", title: "Access denied"}
	case is(domain.ErrQuotaExceeded):
		// A limit and no denial: told apart from one by its code, never by
		// its status (ADR-0024).
		return problem{status: http.StatusForbidden, code: "QuotaExceeded", title: "The bucket quota has been reached"}
	case is(domain.ErrBucketNotFound):
		return problem{status: http.StatusNotFound, code: "NoSuchBucket", title: "No such bucket"}
	case is(domain.ErrObjectNotFound):
		return problem{status: http.StatusNotFound, code: "NoSuchObject", title: "No such object"}
	case is(domain.ErrVersionNotFound):
		return problem{status: http.StatusNotFound, code: "NoSuchVersion", title: "No such version"}
	case is(domain.ErrEntityNotFound):
		return problem{status: http.StatusNotFound, code: "NoSuchEntity", title: "No such entity"}
	case is(domain.ErrMethodNotAllowed):
		return problem{status: http.StatusMethodNotAllowed, code: "MethodNotAllowed", title: "Not possible for this resource"}
	case is(domain.ErrBucketAlreadyExists):
		return problem{status: http.StatusConflict, code: "BucketAlreadyExists", title: "Bucket already exists"}
	case is(domain.ErrBucketNotEmpty):
		return problem{status: http.StatusConflict, code: "BucketNotEmpty", title: "Bucket is not empty"}
	case is(domain.ErrEntityExists):
		return problem{status: http.StatusConflict, code: "EntityAlreadyExists", title: "Entity already exists"}
	case is(domain.ErrEntityInUse):
		return problem{status: http.StatusConflict, code: "EntityInUse", title: "Entity is in use"}
	case is(domain.ErrRequestInProgress):
		return problem{status: http.StatusConflict, code: "RequestInProgress", title: "Request in progress"}
	case is(domain.ErrConflict):
		return problem{status: http.StatusConflict, code: "Conflict", title: "State has changed"}
	case is(domain.ErrIdempotencyKeyReused):
		return problem{status: http.StatusUnprocessableEntity, code: "IdempotencyKeyReused", title: "Idempotency key reused"}
	case is(domain.ErrInvalidBucketName):
		return problem{status: http.StatusBadRequest, code: "InvalidBucketName", title: "Invalid bucket name"}
	case is(domain.ErrInvalidObjectKey):
		return problem{status: http.StatusBadRequest, code: "InvalidObjectKey", title: "Invalid object key"}
	case is(domain.ErrInvalidArgument), is(domain.ErrIncompleteBody), is(domain.ErrBadDigest), is(domain.ErrInvalidRange),
		is(domain.ErrCustomerKeyRequired):
		return problem{status: http.StatusBadRequest, code: "InvalidArgument", title: "Invalid request"}
	case is(errNotImplemented):
		return problem{status: http.StatusNotImplemented, code: "NotImplemented", title: "Not available on this node"}
	case is(domain.ErrUnavailable), is(domain.ErrNotLeader), is(domain.ErrOutcomeUnknown), is(domain.ErrInsufficientDrives),
		is(domain.ErrKeyUnavailable), is(domain.ErrShuttingDown), is(domain.ErrDriveOffline), is(domain.ErrNoTopology),
		is(context.DeadlineExceeded), is(context.Canceled):
		return problem{status: http.StatusServiceUnavailable, code: "ServiceUnavailable", title: "Temporarily unavailable"}
	default:
		return problem{status: http.StatusInternalServerError, code: "InternalError", title: "Internal error"}
	}
}

// fail answers a request with the problem an error stands for.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	info := infoOf(ctx)
	p := problemOf(err)
	if info.sessionEnded && !info.started {
		httpsession.Clear(w)
	}
	switch {
	case info.started:
		// The response has begun - a download failed part-way. Nothing
		// more can be said in it: with the write deadline in the past
		// net/http closes the connection, and the client sees a transfer
		// that fell short.
		h.cfg.Logger.ErrorContext(ctx, "response aborted", slog.String("request_id", info.id), slog.Any("error", err))
		_ = http.NewResponseController(w).SetWriteDeadline(time.Unix(1, 0))
		return
	case p.status >= http.StatusInternalServerError:
		// What went wrong inside is for the log, not for the caller.
		switch {
		case errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled):
			// Nothing went wrong inside: the caller went away - the
			// console asked for something else - and what it had asked for
			// was given up. That is said, and it is no error of this node.
			// A fault that only happens to meet a caller who left is
			// still a fault, and is logged as one below.
			h.cfg.Logger.InfoContext(ctx, "request cancelled by the client", slog.String("request_id", info.id), slog.Any("error", err))
		case errors.Is(err, domain.ErrTooManyBuckets):
			// A limit of the size of the deployment, not an outage.
			h.cfg.Logger.WarnContext(ctx, "request refused", slog.String("request_id", info.id), slog.Any("error", err))
		default:
			h.cfg.Logger.ErrorContext(ctx, "request failed", slog.String("request_id", info.id), slog.Any("error", err))
		}
		if p.status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "1")
		}
		if errors.Is(err, domain.ErrTooManyBuckets) {
			// The one failure of this kind a caller is told about, in
			// words of this adapter: it tells nothing of what is inside,
			// and without it the caller would wait for what does not pass.
			p.detail = detailTooManyBuckets
		}
	case p.status == http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Bearer realm="cactusfs"`)
	case p.code == codeTooManyRequests:
		// The node is busy with this caller's own work, or with as much
		// as it does at a time: a moment later there is room again.
		w.Header().Set("Retry-After", "1")
		p.detail = err.Error()
	case p.status == http.StatusTooManyRequests:
		w.Header().Set("Retry-After", "300")
	case p.status == http.StatusForbidden, p.status == http.StatusLocked:
		// A refusal says nothing about what it protects.
	default:
		p.detail = err.Error()
	}
	h.problem(w, r, p)
}

func (h *Handler) problem(w http.ResponseWriter, r *http.Request, p problem) {
	info := infoOf(r.Context())
	body := api.Problem{Type: "about:blank", Title: p.title, Status: p.status, Code: p.code}
	if p.detail != "" {
		body.Detail = &p.detail
	}
	if info.id != "" {
		body.RequestId = &info.id
	}
	instance := r.URL.Path
	body.Instance = &instance
	response := api.GetSessiondefaultApplicationProblemPlusJSONResponse{Body: body, StatusCode: p.status}
	if err := response.VisitGetSessionResponse(w); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "write problem", slog.String("request_id", info.id), slog.Any("error", err))
	}
}
