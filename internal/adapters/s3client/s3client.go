// Package s3client is the client of another deployment that replication
// sends to (ADR-0026, "The source is an S3 client of the other site"): a
// small S3 client of our own over the signer of package sigv4 and the egress
// client of ADR-0023. It speaks path-style, to the endpoint of one target of
// kind s3, with the access key of that target.
//
// Every request is signed with SigV4 in the Authorization header, every
// header it carries among the signed ones - the replica headers of
// ADR-0026 among them - and its payload signed too: a document by its
// SHA-256, an object or a part as signed chunks. Nothing is sent
// UNSIGNED-PAYLOAD, and nothing is presigned.
//
// A Client is one target's. It holds the target as the store keeps it, its
// secret sealed through the KMS, and opens the secret for each request only:
// what is open is cleared once the request is signed. A body of chunks holds
// the signing key of the day - never the secret - and clears it when it is
// sent. Requests go through the egress client of ADR-0023, which judges the
// address at every connect, verifies TLS 1.2 or newer against the target's
// CA, never follows a redirect, and names in an error the origin of a
// request and never its path.
//
// Every failure is a *delivery.Error with its class (delivery.ClassOf), as
// the webhook and NATS publishers' are; its text is its class and nothing
// else, and no link of it holds the secret, a bucket, a key or a byte of an
// object. The package logs nothing.
package s3client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/adapters/sealed"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// The replica headers of ADR-0026, as net/http writes their names. Each is
// signed whenever it is sent.
const (
	// HeaderReplicaSource is the deployment ID of the sender. It is sent
	// with every replica request and every probe, which the other
	// deployment judges as s3:ReplicateObject; the answer to the bucket
	// probe carries the deployment the bucket is a replica of in it.
	HeaderReplicaSource = "X-Cactusfs-Replica-Source"
	// HeaderReplicaVersionID is the ID the version has at the source, or
	// "null" for the null version of its key.
	HeaderReplicaVersionID = "X-Cactusfs-Replica-Version-Id"
	// HeaderReplicaSeq is the sequence the version has at the source, in
	// decimal.
	HeaderReplicaSeq = "X-Cactusfs-Replica-Seq"
	// HeaderReplicaMTime is the modification time of the version at the
	// source (MTimeFormat).
	HeaderReplicaMTime = "X-Cactusfs-Replica-Mtime"
	// HeaderReplicaETag is the entity tag of the version, without quotes,
	// which the other deployment compares with the one it computes.
	HeaderReplicaETag = "X-Cactusfs-Replica-Etag"
	// HeaderReplicaObjectLock is answered to the bucket probe, as
	// "Enabled", for a replica bucket that has Object Lock.
	HeaderReplicaObjectLock = "X-Cactusfs-Replica-Object-Lock"
	// objectLockEnabled is the value of HeaderReplicaObjectLock.
	objectLockEnabled = "Enabled"
	// MTimeFormat is RFC 3339 in UTC with nine digits of the second: the
	// time of a version to the nanosecond, of one length always.
	MTimeFormat = "2006-01-02T15:04:05.000000000Z"
)

// The headers of S3 that a version carries.
const (
	headerTagging       = "X-Amz-Tagging"
	headerLockMode      = "X-Amz-Object-Lock-Mode"
	headerLockUntil     = "X-Amz-Object-Lock-Retain-Until-Date"
	headerLockLegalHold = "X-Amz-Object-Lock-Legal-Hold"
	headerMetaPrefix    = "X-Amz-Meta-"
	legalHoldOn         = "ON"
	legalHoldOff        = "OFF"
)

// RetentionMargin is how far past the client's clock a retention must end
// to be sent with a copy. The other deployment refuses a copy whose
// retention does not end after its own clock (ADR-0026, "Open", 7), and the
// two clocks may differ by as much as a signature allows - SigV4 is taken
// within 15 minutes of the server's clock - while the copy takes time to
// send. A retention that ends sooner is left out, so that the version is
// not FAILED for it: the copy then goes unretained for the at most 15
// minutes its retention had left, and its legal hold, which has no date, is
// sent all the same. It is object.RetentionMargin, which the replicator holds
// later changes to as well.
const RetentionMargin = object.RetentionMargin

// The bounds of a listing of uploads.
const (
	// maxUploadsPage is how many uploads one page of a listing asks for:
	// a page of them stays far below the egress client's 64 KiB.
	maxUploadsPage = 100
	// maxUploadPages bounds how many pages are read for one key. What is
	// beyond them is found by the next attempt, once those found were
	// aborted.
	maxUploadPages = 100
	// maxPartsPage is how many parts one page of a listing of parts asks
	// for: a page of them stays far below the egress client's 64 KiB.
	maxPartsPage = 250
	// maxPartPages bounds how many pages are read of the parts of one
	// upload: enough for object.MaxParts.
	maxPartPages = object.MaxParts/maxPartsPage + 1
)

// Errors of this package; each is the cause of a delivery.Error.
var (
	// ErrNoAnswer is a connection that ended after the request was sent
	// and before an answer arrived.
	ErrNoAnswer = errors.New("s3client: the connection ended without an answer")
	// ErrStatus is an answer that refuses what was sent.
	ErrStatus = errors.New("s3client: the other deployment did not take the request")
	// ErrCredential is the secret of the target, which could not be
	// opened.
	ErrCredential = errors.New("s3client: the credential of the target could not be used")
	// ErrVersion is a version, a part or a reference that is not one to
	// send: of no source, of a size beyond the bound, of a customer's key.
	ErrVersion = errors.New("s3client: not a version to send")
	// ErrUnsendable is a version whose headers cannot be sent: a value with
	// a control character, a name that is no token, more of them than a
	// signature may cover.
	ErrUnsendable = errors.New("s3client: the version cannot be sent in a request")
)

// Options are what a Client needs besides its target, its secret and its
// client.
type Options struct {
	// Source is the ID of this deployment (object.ValidateReplicaSource),
	// sent in HeaderReplicaSource with every probe.
	Source string
	// Version is the version of this build, named in the User-Agent as
	// cactusfs/<version> when it is 1 to 64 letters, digits, '.', '-',
	// '+' and '_'; the User-Agent is "cactusfs" otherwise.
	Version string
	// Clock tells the time a request is signed at, and whether a
	// retention has passed; nil is the system's clock.
	Clock ports.Clock
}

// systemClock is the clock of the system.
type systemClock struct{}

// Now implements ports.Clock.
func (systemClock) Now() time.Time { return time.Now() }

// Client is a ports.SiteClient: the other deployment one target of kind s3
// names. It is safe for concurrent use: the replicator sends several
// versions at once.
//
// Every printed form of a Client - with any verb of fmt, and in a log - is
// the name and the kind of its target.
type Client struct {
	target    delivery.Target
	secrets   sealed.Opener
	client    *egress.Client
	clock     ports.Clock
	source    string
	userAgent string
	// host is the host and port of the endpoint.
	host string
	// chunkSize is the payload of a chunk (DefaultChunkSize), and
	// uploadsPage the uploads a page of a listing asks for.
	chunkSize   int
	uploadsPage int
	// partsPage is how many parts a page of a listing of parts asks for
	// (maxPartsPage).
	partsPage int
}

var _ ports.SiteClient = (*Client)(nil)

// New returns the client of the deployment a target of kind s3 names.
// secrets opens the target's secret - sealed.KMS over the KMS it was sealed
// through: it is opened at every request and never kept open. client is the
// egress client made for the target - with its CA as the roots, and the
// setting egress-allow - and is the only way the client reaches the network.
//
// The target must be one delivery.Target.Validate takes, of kind s3, and
// the egress client one that follows no redirect: a request signed for one
// host is sent to that host and no other, and a body of chunks cannot be
// sent twice. The error wraps domain.ErrInvalidArgument and repeats nothing
// of the target but its name. New connects to nothing.
func New(target delivery.Target, secrets sealed.Opener, client *egress.Client, opts Options) (*Client, error) {
	switch {
	case target.Kind != delivery.KindS3:
		return nil, fmt.Errorf("s3client.New: %w: %s is not another deployment", domain.ErrInvalidArgument, target)
	case secrets == nil || client == nil:
		return nil, fmt.Errorf("s3client.New: %w: no opener of secrets or no client", domain.ErrInvalidArgument)
	case client.Redirects() != 0:
		return nil, fmt.Errorf("s3client.New: %w: an egress client that follows redirects", domain.ErrInvalidArgument)
	}
	if err := object.ValidateReplicaSource(opts.Source); err != nil {
		return nil, fmt.Errorf("s3client.New: the ID of this deployment: %w", err)
	}
	if err := target.Validate(); err != nil {
		return nil, fmt.Errorf("s3client.New: %w", err)
	}
	origin, err := url.Parse(target.Endpoint)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Path != "" {
		// Validate takes only an https origin; this is said again so
		// that what the paths are put after is one.
		return nil, fmt.Errorf("s3client.New: %w: %s has an endpoint that is no origin", domain.ErrInvalidArgument, target)
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	userAgent := "cactusfs"
	if versionText(opts.Version) {
		userAgent += "/" + opts.Version
	}
	return &Client{
		target: target.Clone(), secrets: secrets, client: client, clock: clock, source: opts.Source,
		userAgent: userAgent, host: origin.Host, chunkSize: DefaultChunkSize, uploadsPage: maxUploadsPage, partsPage: maxPartsPage,
	}, nil
}

// String returns the name and the kind of the client's target.
func (c *Client) String() string {
	return "s3 client of " + c.target.String()
}

// Format implements fmt.Formatter: no verb prints more than String does,
// whatever the sealed secret of the target holds.
func (c *Client) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(c.String()))
}

// LogValue implements slog.LogValuer: a client in a log is its target's
// name and kind.
func (c *Client) LogValue() slog.Value {
	return c.target.LogValue()
}

// refused returns the failure of an answer that is not a 2xx to a call of
// kind, of the class of its status and code.
func refused(op string, r response, kind callKind) error {
	return delivery.Failed(classOf(r.status, r.code, kind), fmt.Errorf("s3client.%s: %w: status %d %s", op, ErrStatus, r.status, r.code))
}

// conflicted returns what a 409 OperationAborted of a replica write says.
// The other deployment answers it when the version ID is one the key has
// already - and also for a conflict that committed nothing (domain.ErrConflict
// is one code for both). So it is "maybe there": the version is asked for
// with HeadVersion, and the write is done only when the version is there
// with the entity tag sent (a delete marker: there, with none). Anything
// else is ClassServer, tried again; a failure of the probe is its own.
func (c *Client) conflicted(ctx context.Context, op string, v object.Version) error {
	ref := object.VersionRef{ID: v.VersionID}
	if v.Replica != nil && v.Replica.Null {
		ref = object.VersionRef{Null: true}
	}
	etag, found, err := c.HeadVersion(ctx, v.Bucket, v.Key, ref)
	if err != nil {
		return err
	}
	want := v.ETag
	if v.DeleteMarker {
		want = ""
	}
	if found && etag == want {
		return nil
	}
	return delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w: a conflict, and the version is not there as sent", op, ErrStatus))
}

// isConflict reports whether r is the 409 OperationAborted that a version
// already there is answered with, among other conflicts.
func (r response) isConflict() bool {
	return r.status == http.StatusConflict && r.code == codeOperationAborted
}

// ok reports whether r is a 2xx.
func (r response) ok() bool {
	return r.status >= 200 && r.status <= 299
}

// Check implements ports.SiteClient: HEAD of the bucket, and GET of its
// versioning, each with HeaderReplicaSource. The deployment the bucket is a
// replica of is read from the answer to the HEAD, or, when it has none, from
// the answer to the GET; a value that is no deployment ID is none. A
// deployment of this release answers both only for a bucket that is the
// replica of this one, and refuses them for any other with 409
// InvalidBucketState: ClassNotReplica, as a 409 of the HEAD is, which has no
// body to name its code.
func (c *Client) Check(ctx context.Context, bucketName string) (ports.SiteBucket, error) {
	const op = "Check"
	if err := checkBucket(op, bucketName); err != nil {
		return ports.SiteBucket{}, err
	}
	head, err := c.do(ctx, call{op: op, method: http.MethodHead, bucket: bucketName, header: c.probe()}, 0)
	if err != nil {
		return ports.SiteBucket{}, err
	}
	if !head.ok() {
		switch head.status {
		case http.StatusNotFound:
			// A HEAD has no body: its 404 is the bucket's.
			head.code = codeNoSuchBucket
		case http.StatusConflict:
			// And its 409 is the one a question of a source is given
			// for a bucket that is no replica of it.
			head.code = codeInvalidBucketState
		}
		return ports.SiteBucket{}, refused(op, head, kindCopy)
	}
	get, err := c.do(ctx, call{
		op: op, method: http.MethodGet, bucket: bucketName, query: [][2]string{{"versioning", ""}}, header: c.probe(),
	}, maxDocument)
	if err != nil {
		return ports.SiteBucket{}, err
	}
	if !get.ok() {
		return ports.SiteBucket{}, refused(op, get, kindCopy)
	}
	versioning, err := parseVersioning(get.body)
	if err != nil {
		return ports.SiteBucket{}, delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w", op, err))
	}
	replicaOf := head.header.Get(HeaderReplicaSource)
	if replicaOf == "" {
		replicaOf = get.header.Get(HeaderReplicaSource)
	}
	if object.ValidateReplicaSource(replicaOf) != nil {
		replicaOf = ""
	}
	// Object Lock is said by either answer; a value that is not exactly
	// the one is none.
	lock := head.header.Get(HeaderReplicaObjectLock) == objectLockEnabled || get.header.Get(HeaderReplicaObjectLock) == objectLockEnabled
	return ports.SiteBucket{Versioning: versioning, ReplicaOf: replicaOf, ObjectLock: lock}, nil
}

// PutReplica implements ports.SiteClient: PutObject with the five replica
// headers, the representation headers, the user metadata, the tags and the
// lock of v, its body as signed chunks. A 409 OperationAborted is "maybe
// there" and is asked about (conflicted).
//
// A retention that ends within RetentionMargin of the client's clock is left
// out: Object Lock at the other deployment would refuse the copy for it
// (ADR-0026, "Open", 7).
func (c *Client) PutReplica(ctx context.Context, v object.Version, body io.Reader) error {
	const op = "PutReplica"
	header, err := c.replicaHeaders(op, v, true)
	if err != nil {
		return err
	}
	switch {
	case v.DeleteMarker || v.Multipart():
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a marker or a version of parts", op, ErrVersion))
	case v.Size < 0 || v.Size > object.MaxSize:
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a size beyond the bound of one request", op, ErrVersion))
	case body == nil:
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: no body", op, ErrVersion))
	}
	c.versionHeaders(header, v)
	r, err := c.do(ctx, call{
		op: op, method: http.MethodPut, bucket: v.Bucket, key: v.Key, header: header, stream: body, size: v.Size,
	}, 0)
	if err != nil {
		return err
	}
	switch {
	case r.ok():
		return nil
	case r.isConflict():
		return c.conflicted(ctx, op, v)
	}
	return refused(op, r, kindCopy)
}

// CreateUpload implements ports.SiteClient: CreateMultipartUpload with
// HeaderReplicaSource, and the representation headers, the user metadata,
// the tags and the lock of v, which the completed version takes.
func (c *Client) CreateUpload(ctx context.Context, v object.Version) (string, error) {
	const op = "CreateUpload"
	if err := checkObject(op, v.Bucket, v.Key); err != nil {
		return "", err
	}
	if err := checkSendable(op, v); err != nil {
		return "", err
	}
	header := c.probe()
	c.versionHeaders(header, v)
	r, err := c.do(ctx, call{
		op: op, method: http.MethodPost, bucket: v.Bucket, key: v.Key, query: [][2]string{{"uploads", ""}}, header: header,
	}, maxDocument)
	if err != nil {
		return "", err
	}
	if !r.ok() {
		return "", refused(op, r, kindCopy)
	}
	id, err := parseInitiate(r.body)
	if err != nil {
		return "", delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w", op, err))
	}
	return id, nil
}

// PutPart implements ports.SiteClient: UploadPart, its body as signed
// chunks. The upload is a replica upload; the part carries no replica
// header (ADR-0026).
func (c *Client) PutPart(ctx context.Context, bucketName, key, uploadID string, number int, size int64, body io.Reader) (string, error) {
	const op = "PutPart"
	if err := checkObject(op, bucketName, key); err != nil {
		return "", err
	}
	switch {
	case !validUploadID(uploadID):
		return "", delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: no upload ID", op, ErrVersion))
	case object.ValidatePartNumber(number) != nil:
		return "", delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a part number out of range", op, ErrVersion))
	case size < 0 || size > object.MaxSize || body == nil:
		return "", delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a part of a size beyond the bound, or no body", op, ErrVersion))
	}
	r, err := c.do(ctx, call{
		op: op, method: http.MethodPut, bucket: bucketName, key: key,
		query:  [][2]string{{"partNumber", strconv.Itoa(number)}, {"uploadId", uploadID}},
		header: http.Header{}, stream: body, size: size,
	}, 0)
	if err != nil {
		return "", err
	}
	if !r.ok() {
		return "", refused(op, r, kindCopy)
	}
	etag := strings.Trim(r.header.Get("ETag"), `"`)
	if etag == "" || !printableWord(etag) {
		return "", delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w: a part without an entity tag", op, errDocument))
	}
	return etag, nil
}

// CompleteReplica implements ports.SiteClient: CompleteMultipartUpload with
// the five replica headers. A 409 OperationAborted is "maybe there" and is
// asked about (conflicted). A 200 is a completion only when its body is a
// CompleteMultipartUploadResult: S3 answers 200 before it completes and tells
// a failure after it in the body, which is then that failure; a body that is
// neither is ClassServer.
func (c *Client) CompleteReplica(ctx context.Context, v object.Version, uploadID string, parts []ports.SitePart) error {
	const op = "CompleteReplica"
	header, err := c.replicaHeaders(op, v, true)
	if err != nil {
		return err
	}
	if !validUploadID(uploadID) || len(parts) == 0 || len(parts) > object.MaxParts {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: no upload ID, or no parts or too many", op, ErrVersion))
	}
	doc := completeDocument{Parts: make([]completePart, 0, len(parts))}
	for _, part := range parts {
		etag := strings.Trim(part.ETag, `"`)
		if object.ValidatePartNumber(part.Number) != nil || etag == "" || !printableWord(etag) {
			return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a part without a number or an entity tag", op, ErrVersion))
		}
		doc.Parts = append(doc.Parts, completePart{PartNumber: part.Number, ETag: `"` + etag + `"`})
	}
	body, err := marshalXML(doc)
	if err != nil {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w", op, err))
	}
	header.Set("Content-Type", "application/xml")
	r, err := c.do(ctx, call{
		op: op, method: http.MethodPost, bucket: v.Bucket, key: v.Key, query: [][2]string{{"uploadId", uploadID}},
		header: header, body: body,
	}, maxDocument)
	if err != nil {
		return err
	}
	if r.ok() {
		completed, code := completeAnswer(r.body)
		switch {
		case completed:
			return nil
		case code == "":
			return delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w: a completion that is no result", op, errDocument))
		}
		r = response{status: http.StatusInternalServerError, code: code}
		if code == codeOperationAborted {
			return c.conflicted(ctx, op, v)
		}
		return refused(op, r, kindCopy)
	}
	if r.isConflict() {
		return c.conflicted(ctx, op, v)
	}
	return refused(op, r, kindCopy)
}

// AbortUpload implements ports.SiteClient: AbortMultipartUpload with
// HeaderReplicaSource. An upload that is not there (404 NoSuchUpload) is
// gone already, and success.
func (c *Client) AbortUpload(ctx context.Context, bucketName, key, uploadID string) error {
	const op = "AbortUpload"
	if err := checkObject(op, bucketName, key); err != nil {
		return err
	}
	if !validUploadID(uploadID) {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: no upload ID", op, ErrVersion))
	}
	r, err := c.do(ctx, call{
		op: op, method: http.MethodDelete, bucket: bucketName, key: key, query: [][2]string{{"uploadId", uploadID}},
		header: c.probe(),
	}, 0)
	if err != nil {
		return err
	}
	if r.ok() || (r.status == http.StatusNotFound && r.code == codeNoSuchUpload) {
		return nil
	}
	return refused(op, r, kindCopy)
}

// ListParts implements ports.SiteClient: ListParts of the upload, page by
// page (partsPage parts a page, so that a page stays below the egress
// client's bound), at most maxPartPages pages. It carries no replica header:
// the other deployment takes none on a listing of parts, and judges the
// listing of the parts of a replica upload as s3:ReplicateObject (ADR-0026,
// "The destination, as built"). A 404 NoSuchUpload is an upload that is not
// there: not found, and no error.
func (c *Client) ListParts(ctx context.Context, bucketName, key, uploadID string) ([]ports.SitePart, bool, error) {
	const op = "ListParts"
	if err := checkObject(op, bucketName, key); err != nil {
		return nil, false, err
	}
	if !validUploadID(uploadID) {
		return nil, false, delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: no upload ID", op, ErrVersion))
	}
	var parts []ports.SitePart
	marker := 0
	for range maxPartPages {
		query := [][2]string{{"uploadId", uploadID}, {"max-parts", strconv.Itoa(c.partsPage)}}
		if marker > 0 {
			query = append(query, [2]string{"part-number-marker", strconv.Itoa(marker)})
		}
		r, err := c.do(ctx, call{op: op, method: http.MethodGet, bucket: bucketName, key: key, query: query, header: http.Header{}}, maxListing)
		if err != nil {
			return nil, false, err
		}
		switch {
		case r.status == http.StatusNotFound && r.code == codeNoSuchUpload:
			return nil, false, nil
		case !r.ok():
			return nil, false, refused(op, r, kindCopy)
		}
		page, err := parseParts(r.body, uploadID)
		if err != nil {
			return nil, false, delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w", op, err))
		}
		for _, part := range page.parts {
			if len(parts) > 0 && part.Number <= parts[len(parts)-1].Number || part.Number <= marker {
				return nil, false, delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w: parts out of order", op, errDocument))
			}
			parts = append(parts, part)
		}
		if !page.truncated {
			return parts, true, nil
		}
		if page.next <= marker {
			// A listing that does not move on is not followed round.
			return nil, false, delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w: a listing that does not go on", op, errDocument))
		}
		marker = page.next
	}
	return nil, false, delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w: more parts than an upload has", op, errDocument))
}

// ListReplicaUploads implements ports.SiteClient: ListMultipartUploads of
// the key as a prefix, with HeaderReplicaSource and encoding-type=url, page
// by page, keeping the uploads of the key itself. It reads at most
// maxUploadPages pages; what is beyond them is found by the next attempt,
// once those found were aborted.
func (c *Client) ListReplicaUploads(ctx context.Context, bucketName, key string) ([]string, error) {
	const op = "ListReplicaUploads"
	if err := checkObject(op, bucketName, key); err != nil {
		return nil, err
	}
	var ids []string
	keyMarker, uploadMarker := "", ""
	for range maxUploadPages {
		query := [][2]string{
			{"uploads", ""}, {"encoding-type", "url"}, {"max-uploads", strconv.Itoa(c.uploadsPage)}, {"prefix", key},
		}
		if keyMarker != "" {
			query = append(query, [2]string{"key-marker", keyMarker})
			if uploadMarker != "" {
				query = append(query, [2]string{"upload-id-marker", uploadMarker})
			}
		}
		r, err := c.do(ctx, call{op: op, method: http.MethodGet, bucket: bucketName, query: query, header: c.probe()}, maxListing)
		if err != nil {
			return nil, err
		}
		if !r.ok() {
			return nil, refused(op, r, kindCopy)
		}
		page, err := parseListing(r.body, key)
		if err != nil {
			return nil, delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w", op, err))
		}
		ids = append(ids, page.uploads...)
		if !page.truncated {
			break
		}
		if page.nextKey == keyMarker && page.nextUpload == uploadMarker {
			// A listing that does not move on is not followed round.
			return nil, delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w: a listing that does not go on", op, errDocument))
		}
		if page.nextKey > key {
			// The listing is in the order of keys, and of uploads within
			// one: the uploads of the key itself are all listed.
			break
		}
		keyMarker, uploadMarker = page.nextKey, page.nextUpload
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// PutMarker implements ports.SiteClient: DeleteObject without a versionId,
// with HeaderReplicaSource, -version-id, -seq and -mtime. A 409
// OperationAborted is "maybe there" and is asked about (conflicted).
func (c *Client) PutMarker(ctx context.Context, v object.Version) error {
	const op = "PutMarker"
	if !v.DeleteMarker {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: not a delete marker", op, ErrVersion))
	}
	if v.VersionID.IsZero() {
		// The copy of a marker has an ID that is not all zeros, the null
		// one too (ADR-0026).
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a marker without an ID", op, ErrVersion))
	}
	header, err := c.replicaHeaders(op, v, false)
	if err != nil {
		return err
	}
	r, err := c.do(ctx, call{op: op, method: http.MethodDelete, bucket: v.Bucket, key: v.Key, header: header}, 0)
	if err != nil {
		return err
	}
	switch {
	case r.ok():
		return nil
	case r.isConflict():
		return c.conflicted(ctx, op, v)
	}
	return refused(op, r, kindCopy)
}

// HeadVersion implements ports.SiteClient: HEAD of the version with
// HeaderReplicaSource. A 404 is a version that is not there - a HEAD has no
// body to tell a missing bucket from a missing version, and Check, asked
// first, tells of the bucket. A 405 is a delete marker of that ID, which is
// there and has no entity tag. A 2xx without an entity tag is ClassServer. A
// 409 is a bucket that is no replica of this deployment: ClassNotReplica.
func (c *Client) HeadVersion(ctx context.Context, bucketName, key string, ref object.VersionRef) (string, bool, error) {
	const op = "HeadVersion"
	if err := checkObject(op, bucketName, key); err != nil {
		return "", false, err
	}
	if err := checkRef(op, ref); err != nil {
		return "", false, err
	}
	r, err := c.do(ctx, call{
		op: op, method: http.MethodHead, bucket: bucketName, key: key, query: [][2]string{{"versionId", ref.String()}},
		header: c.probe(),
	}, 0)
	if err != nil {
		return "", false, err
	}
	switch {
	case r.ok():
		etag := strings.Trim(r.header.Get("ETag"), `"`)
		if etag == "" || !printableWord(etag) {
			// A version that is there has an entity tag: an answer
			// without one says nothing either way.
			return "", false, delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w: an entity tag that is none", op, errDocument))
		}
		return etag, true, nil
	case r.status == http.StatusNotFound:
		return "", false, nil
	case r.status == http.StatusMethodNotAllowed:
		return "", true, nil
	case r.status == http.StatusConflict:
		// A HEAD has no body: its 409 is the one a question of a source
		// is given for a bucket that is no replica of it.
		r.code = codeInvalidBucketState
	}
	return "", false, refused(op, r, kindCopy)
}

// PutTags implements ports.SiteClient: PutObjectTagging of the version, or
// DeleteObjectTagging for nil tags.
func (c *Client) PutTags(ctx context.Context, bucketName, key string, ref object.VersionRef, tags map[string]string) error {
	const op = "PutTags"
	query := [][2]string{{"tagging", ""}, {"versionId", ref.String()}}
	if tags == nil {
		return c.change(ctx, op, http.MethodDelete, bucketName, key, ref, query, nil, kindChange)
	}
	doc := tagDocument{Tags: make([]tagXML, 0, len(tags))}
	for _, tag := range object.TagList(tags) {
		doc.Tags = append(doc.Tags, tagXML{Key: tag.Key, Value: tag.Value})
	}
	return c.change(ctx, op, http.MethodPut, bucketName, key, ref, query, doc, kindChange)
}

// PutRetention implements ports.SiteClient: PutObjectRetention of the
// version; nil sends the empty document, which removes the retention where
// the other deployment allows it. No bypass of a governance retention is
// ever asked for.
func (c *Client) PutRetention(ctx context.Context, bucketName, key string, ref object.VersionRef, r *object.Retention) error {
	const op = "PutRetention"
	doc := retentionDocument{}
	if r != nil {
		doc.Mode, doc.RetainUntilDate = string(r.Mode), r.Until.UTC().Format(time.RFC3339Nano)
	}
	return c.change(ctx, op, http.MethodPut, bucketName, key, ref, [][2]string{{"retention", ""}, {"versionId", ref.String()}}, doc, kindRetention)
}

// PutLegalHold implements ports.SiteClient: PutObjectLegalHold of the
// version.
func (c *Client) PutLegalHold(ctx context.Context, bucketName, key string, ref object.VersionRef, on bool) error {
	const op = "PutLegalHold"
	doc := legalHoldDocument{Status: legalHoldOff}
	if on {
		doc.Status = legalHoldOn
	}
	return c.change(ctx, op, http.MethodPut, bucketName, key, ref, [][2]string{{"legal-hold", ""}, {"versionId", ref.String()}}, doc, kindChange)
}

// change sends a later change of a version, with HeaderReplicaSource: the
// document doc, or no body when doc is nil. kind says how a refusal of it is
// classed; a refusal by Object Lock (ObjectLocked) fails the change alone.
func (c *Client) change(ctx context.Context, op, method, bucketName, key string, ref object.VersionRef, query [][2]string, doc any, kind callKind) error {
	if err := checkObject(op, bucketName, key); err != nil {
		return err
	}
	if err := checkRef(op, ref); err != nil {
		return err
	}
	// With the source: the other deployment then takes the change only in
	// this source's replica bucket, and tells a refusal by Object Lock
	// from a refused key (ADR-0026).
	rq := call{op: op, method: method, bucket: bucketName, key: key, query: query, header: c.probe()}
	if doc != nil {
		body, err := marshalXML(doc)
		if err != nil {
			return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w", op, err))
		}
		rq.body = body
		rq.header.Set("Content-Type", "application/xml")
	}
	r, err := c.do(ctx, rq, 0)
	if err != nil {
		return err
	}
	if !r.ok() {
		return refused(op, r, kind)
	}
	return nil
}

// probe returns the headers of a probe: HeaderReplicaSource, this
// deployment's ID.
func (c *Client) probe() http.Header {
	header := http.Header{}
	header.Set(HeaderReplicaSource, c.source)
	return header
}

// replicaHeaders returns the replica headers of v: its source, its version
// ID, its sequence and its time, and its entity tag when etag is set.
func (c *Client) replicaHeaders(op string, v object.Version, etag bool) (http.Header, error) {
	if err := checkObject(op, v.Bucket, v.Key); err != nil {
		return nil, err
	}
	if err := checkSendable(op, v); err != nil {
		return nil, err
	}
	switch {
	case v.Replica == nil:
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: no source and sequence", op, ErrVersion))
	case v.Replica.Validate() != nil:
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a source or a sequence that is none", op, ErrVersion))
	case v.Replica.From != c.source:
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: of another source than this deployment", op, ErrVersion))
	case !v.Replica.Null && v.VersionID.IsZero():
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a version without an ID", op, ErrVersion))
	case v.ModifiedAt.IsZero():
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a version without a time", op, ErrVersion))
	case etag && (v.ETag == "" || !printableWord(v.ETag)):
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: a version without an entity tag", op, ErrVersion))
	}
	versionID := v.VersionID.String()
	if v.Replica.Null {
		versionID = object.NullVersion
	}
	header := http.Header{}
	header.Set(HeaderReplicaSource, v.Replica.From)
	header.Set(HeaderReplicaVersionID, versionID)
	header.Set(HeaderReplicaSeq, strconv.FormatUint(v.Replica.Seq, 10))
	header.Set(HeaderReplicaMTime, v.ModifiedAt.UTC().Format(MTimeFormat))
	if etag {
		header.Set(HeaderReplicaETag, v.ETag)
	}
	return header, nil
}

// versionHeaders adds what a version carries besides its bytes: its
// representation headers, its user metadata, its tags and its lock. A
// retention that ends within RetentionMargin of the client's clock is left
// out; its legal hold is not.
func (c *Client) versionHeaders(header http.Header, v object.Version) {
	for name, value := range map[string]string{
		"Content-Type": v.ContentType, "Content-Encoding": v.ContentEncoding,
		"Content-Disposition": v.ContentDisposition, "Content-Language": v.ContentLanguage,
		"Cache-Control": v.CacheControl,
	} {
		if value != "" {
			header.Set(name, value)
		}
	}
	for name, value := range v.UserMetadata {
		header.Set(headerMetaPrefix+name, value)
	}
	if len(v.Tags) > 0 {
		pairs := make([]string, 0, len(v.Tags))
		for _, tag := range object.TagList(v.Tags) {
			pairs = append(pairs, url.QueryEscape(tag.Key)+"="+url.QueryEscape(tag.Value))
		}
		header.Set(headerTagging, strings.Join(pairs, "&"))
	}
	if r := v.Lock.Retention; r != nil && r.Until.After(c.clock.Now().Add(RetentionMargin)) {
		header.Set(headerLockMode, string(r.Mode))
		header.Set(headerLockUntil, r.Until.UTC().Format(time.RFC3339Nano))
	}
	if v.Lock.LegalHold {
		header.Set(headerLockLegalHold, legalHoldOn)
	}
}

// checkSendable refuses a version of a customer's key, which is not this
// deployment's to read or to send (ADR-0026): ClassSSEC.
func checkSendable(op string, v object.Version) error {
	if v.Encryption.Mode == object.SSEC {
		return delivery.Failed(delivery.ClassSSEC, fmt.Errorf("s3client.%s: %w: encrypted with a key of the customer's", op, ErrVersion))
	}
	return nil
}

// checkBucket refuses a bucket that is no name of one: nothing could be
// sent to it.
func checkBucket(op, bucketName string) error {
	if bucket.ValidateName(bucketName) != nil {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: no bucket", op, ErrVersion))
	}
	return nil
}

// checkObject refuses a bucket or a key that is no name of one.
func checkObject(op, bucketName, key string) error {
	if err := checkBucket(op, bucketName); err != nil {
		return err
	}
	if object.ValidateKey(key) != nil {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: no key", op, ErrVersion))
	}
	return nil
}

// checkRef refuses a reference to a version that names none: not null, and
// without an ID.
func checkRef(op string, ref object.VersionRef) error {
	if !ref.Null && ref.ID.IsZero() {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w: no version", op, ErrVersion))
	}
	return nil
}

// versionText reports whether v can be named in a User-Agent: 1 to 64
// letters, digits, '.', '-', '+' and '_'.
func versionText(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for i := range len(v) {
		c := v[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') &&
			c != '.' && c != '-' && c != '+' && c != '_' {
			return false
		}
	}
	return true
}
