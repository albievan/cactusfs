package s3api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Replica requests (ADR-0026). Another deployment that replicates a bucket
// here sends each version as an ordinary S3 request with headers of its own:
// who it is, and the ID, the sequence, the time and the entity tag the
// version has there. Which requests take them, and how they must be signed,
// is protocol and is decided here; whether the caller may write a copy and
// whether the bucket takes it is the use case's and the state's.
const (
	headerReplicaPrefix    = "x-cactusfs-replica-"
	headerReplicaSource    = headerReplicaPrefix + "source"
	headerReplicaVersionID = headerReplicaPrefix + "version-id"
	headerReplicaSeq       = headerReplicaPrefix + "seq"
	headerReplicaMTime     = headerReplicaPrefix + "mtime"
	headerReplicaETag      = headerReplicaPrefix + "etag"

	// headerReplicationStatus says, on GET and HEAD, where a version
	// stands in replication: PENDING, COMPLETED or FAILED at the source,
	// REPLICA at the destination.
	headerReplicationStatus = "X-Amz-Replication-Status"
)

// replicaKind is the request a replica header comes with, and so which of
// them it takes.
type replicaKind int

const (
	// replicaPut is PutObject: every header.
	replicaPut replicaKind = iota
	// replicaCreate is CreateMultipartUpload: the source only.
	replicaCreate
	// replicaComplete is CompleteMultipartUpload: every header.
	replicaComplete
	// replicaMarker is DeleteObject without a version: every header but
	// the entity tag, which a delete marker does not have.
	replicaMarker
	// replicaAsk is a question of a source - HEAD of the bucket, GET of
	// its versioning, HEAD of a version, the listing of its uploads of a
	// key, the abort of one - or a later change of a version: the source
	// only.
	replicaAsk
)

// replicaHeaders returns the headers a kind of request takes, all of which
// it must carry.
func (k replicaKind) replicaHeaders() []string {
	switch k {
	case replicaCreate, replicaAsk:
		return []string{headerReplicaSource}
	case replicaMarker:
		return []string{headerReplicaSource, headerReplicaVersionID, headerReplicaSeq, headerReplicaMTime}
	default:
		return []string{headerReplicaSource, headerReplicaVersionID, headerReplicaSeq, headerReplicaMTime, headerReplicaETag}
	}
}

// hasReplicaHeaders reports whether a request carries any header of a
// replica request, known or not.
func hasReplicaHeaders(header http.Header) bool {
	for name := range header {
		if strings.HasPrefix(strings.ToLower(name), headerReplicaPrefix) {
			return true
		}
	}
	return false
}

// takesReplicaHeaders reports whether the request is one of those a replica
// may be: a PUT of an object that is no copy, the POST that begins a
// multipart upload or the one that completes it, a DELETE of an object that
// names no version - or one of the questions of a source (HEAD of the
// bucket, GET of its versioning, the listing of its uploads, HEAD of a
// version named by versionId, the abort of an upload) or a later change of
// a version named by versionId (its tags, retention or legal hold), which
// take the source alone. On any other request the headers would be passed
// over, and a write made, or a read answered, as something it is not - they
// are refused instead.
func takesReplicaHeaders(req *request, query url.Values) bool {
	selects := len(query)
	if query.Has(queryClientInfo) {
		selects--
	}
	if req.key == "" {
		switch req.Method {
		case http.MethodHead:
			return selects == 0
		case http.MethodGet:
			return (selects == 1 && query.Has("versioning")) || query.Has("uploads")
		}
		return false
	}
	copies := len(req.Header.Values(headerCopySource)) > 0
	// A later change names its version, and one sub-resource.
	change := selects == 2 && query.Has("versionId")
	switch req.Method {
	case http.MethodPut:
		return !copies && (selects == 0 ||
			(change && (query.Has("tagging") || query.Has("retention") || query.Has("legal-hold"))))
	case http.MethodPost:
		return selects == 1 && (query.Has("uploads") || query.Has("uploadId"))
	case http.MethodDelete:
		return selects == 0 || (selects == 1 && query.Has("uploadId")) || (change && query.Has("tagging"))
	case http.MethodHead:
		return selects == 1 && query.Has("versionId")
	}
	return false
}

// errReplicaHeaders refuses replica headers on a request that takes none.
// Its code is its own (ADR-0026, "The destination, as built"): a source
// tells it from an InvalidArgument of what it sent - which it may class as
// the version's, and fail - and holds the target instead, since the request
// is one this deployment does not take from a source at all.
func errReplicaHeaders() apiError {
	return apiError{http.StatusBadRequest, codeReplicaHeaderNotTaken, "The request does not take x-cactusfs-replica-* headers."}
}

// codeReplicaHeaderNotTaken is the code of errReplicaHeaders.
const codeReplicaHeaderNotTaken = "ReplicaHeaderNotTaken"

// payloadSigned reports whether the payload of a request is covered by its
// signature as a replica needs it: its SHA-256 in x-amz-content-sha256, or
// signed chunks - never UNSIGNED-PAYLOAD, a CRC, a digest beside an unsigned
// payload, or a presigned URL, whose holder chooses what it sends.
func payloadSigned(req *request) bool {
	if req.principal.Session.Presigned {
		return false
	}
	switch hash := req.payloadHash; hash {
	case payloadStreamingSigned, payloadSignedTrailer:
		return req.chunks != nil
	default:
		raw, err := hex.DecodeString(hash)
		return err == nil && len(raw) == sha256.Size && hash == strings.ToLower(hash)
	}
}

// replicaOf reads what a replica request says of the version it is the copy
// of, or nil for a request that is no replica. It refuses - before anything
// is read of the body - a replica header that the signature does not cover,
// and a payload it does not cover, with 403: such a request could have been
// changed on its way, and a copy makes a version with an ID and a time of
// the sender's choosing. Each refusal is audited as a failed
// authentication. A header that is missing, given twice, not known for the
// request or not readable is 400.
func (h *Handler) replicaOf(ctx context.Context, req *request, kind replicaKind) (*app.ReplicaWrite, error) {
	if !hasReplicaHeaders(req.Header) {
		return nil, nil
	}
	for name := range req.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, headerReplicaPrefix) && !slices.Contains(req.signedHeaders, lower) {
			h.cfg.Identities.AuthenticationFailed(ctx, req.principal.AccessKeyID, sessionOf(req), "ReplicaHeaderNotSigned")
			return nil, fmt.Errorf("%w: %s is not signed", domain.ErrAccessDenied, lower)
		}
	}
	if !payloadSigned(req) {
		h.cfg.Identities.AuthenticationFailed(ctx, req.principal.AccessKeyID, sessionOf(req), "ReplicaPayloadNotSigned")
		return nil, fmt.Errorf("%w: the payload of a replica is not signed", domain.ErrAccessDenied)
	}
	w, err := parseReplicaHeaders(req.Header, kind)
	if err != nil {
		return nil, err
	}
	req.replica = true
	return &w, nil
}

// replicaSourceOf reads the source of a question or a later change of a
// source (replicaAsk): "" for a request that carries no replica header, and
// the refusals of replicaOf for one that does.
func (h *Handler) replicaSourceOf(ctx context.Context, req *request) (string, error) {
	w, err := h.replicaOf(ctx, req, replicaAsk)
	if err != nil || w == nil {
		return "", err
	}
	return w.Source, nil
}

// errReplicaObjectLocked answers a request of a source that Object Lock
// refuses (ADR-0026, "The destination, as built"). Its status is the one of
// any refusal by Object Lock; its code is its own, so that the source can
// tell it from a refused key - AccessDenied, which stalls a target - and
// fail the one change it cannot make.
func errReplicaObjectLocked() apiError {
	return apiError{http.StatusForbidden, codeReplicaObjectLocked, "The object version is protected by Object Lock, and the change is not made."}
}

// codeReplicaObjectLocked is the code of errReplicaObjectLocked.
const codeReplicaObjectLocked = "ObjectLocked"

// errReplicaQuestion refuses, on a question of a source, what the question
// does not take and an ordinary request would act on: the conditions of a
// read, a customer key, a delimiter.
var errReplicaQuestion = fmt.Errorf("%w: a question of a replica source takes no conditions, customer key or delimiter", domain.ErrInvalidArgument)

// parseReplicaHeaders reads the replica headers of a request of a kind. It
// takes exactly the headers the kind takes, each once:
//
//	x-cactusfs-replica-source      the deployment ID of the sender
//	x-cactusfs-replica-version-id  32 lower-case hexadecimal characters, or "null"
//	x-cactusfs-replica-seq         a decimal from 1 to 2^62, without leading zeros
//	x-cactusfs-replica-mtime       RFC 3339, in UTC ("Z"), with up to nine fractional digits
//	x-cactusfs-replica-etag        32 lower-case hexadecimal characters, then "-" and the
//	                               number of parts for a version of parts; quoted or not
//
// Every error wraps domain.ErrInvalidArgument. What it returns is read, not
// judged: the use case judges it again (app.ReplicaWrite).
func parseReplicaHeaders(header http.Header, kind replicaKind) (app.ReplicaWrite, error) {
	want := kind.replicaHeaders()
	values := map[string]string{}
	for name, given := range header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, headerReplicaPrefix) {
			continue
		}
		// net/http gives every header it read under its canonical name: a
		// name in another case is one written by hand, and could stand
		// beside the canonical one as a second value.
		if http.CanonicalHeaderKey(name) != name {
			return app.ReplicaWrite{}, fmt.Errorf("%w: %s is not in canonical form", domain.ErrInvalidArgument, lower)
		}
		if !slices.Contains(want, lower) {
			return app.ReplicaWrite{}, fmt.Errorf("%w: %s is not part of this operation", domain.ErrInvalidArgument, lower)
		}
		if _, twice := values[lower]; twice || len(given) != 1 {
			return app.ReplicaWrite{}, fmt.Errorf("%w: %s is given more than once", domain.ErrInvalidArgument, lower)
		}
		values[lower] = given[0]
	}
	for _, name := range want {
		if _, ok := values[name]; !ok {
			return app.ReplicaWrite{}, fmt.Errorf("%w: %s is missing", domain.ErrInvalidArgument, name)
		}
	}

	var w app.ReplicaWrite
	w.Source = values[headerReplicaSource]
	if err := object.ValidateReplicaSource(w.Source); err != nil {
		return app.ReplicaWrite{}, fmt.Errorf("%w: %s", err, headerReplicaSource)
	}
	if kind == replicaCreate || kind == replicaAsk {
		return w, nil
	}
	var err error
	if w.Version, err = object.ParseVersionRef(values[headerReplicaVersionID]); err != nil {
		return app.ReplicaWrite{}, fmt.Errorf("%w: %s", domain.ErrInvalidArgument, headerReplicaVersionID)
	}
	if w.Seq, err = parseReplicaSeq(values[headerReplicaSeq]); err != nil {
		return app.ReplicaWrite{}, err
	}
	if w.ModifiedAt, err = parseReplicaTime(values[headerReplicaMTime]); err != nil {
		return app.ReplicaWrite{}, err
	}
	if kind == replicaMarker {
		return w, nil
	}
	if w.ETag, err = parseReplicaETag(values[headerReplicaETag]); err != nil {
		return app.ReplicaWrite{}, err
	}
	return w, nil
}

// parseReplicaSeq reads a sequence: decimal digits only, without a sign or
// a leading zero, from 1 to object.MaxReplicaSeq.
func parseReplicaSeq(s string) (uint64, error) {
	bad := fmt.Errorf("%w: %s is a decimal from 1 to 2^62", domain.ErrInvalidArgument, headerReplicaSeq)
	if s == "" || s[0] == '0' || strings.TrimLeft(s, "0123456789") != "" {
		return 0, bad
	}
	seq, err := strconv.ParseUint(s, 10, 64)
	if err != nil || seq > object.MaxReplicaSeq {
		return 0, bad
	}
	return seq, nil
}

// parseReplicaTime reads a modification time: RFC 3339 in UTC, as
// time.RFC3339Nano writes it or with trailing zeros, and nothing else - no
// other offset, no lower-case letters.
func parseReplicaTime(s string) (time.Time, error) {
	bad := fmt.Errorf("%w: %s is an RFC 3339 time in UTC", domain.ErrInvalidArgument, headerReplicaMTime)
	// time.Parse takes a comma for the point and any number of fractional
	// digits; RFC 3339 as a source writes it has neither.
	if !strings.HasSuffix(s, "Z") || strings.ContainsAny(s, "tz,") {
		return time.Time{}, bad
	}
	if _, fraction, ok := strings.Cut(strings.TrimSuffix(s, "Z"), "."); ok &&
		(fraction == "" || len(fraction) > 9 || strings.TrimLeft(fraction, "0123456789") != "") {
		return time.Time{}, bad
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.IsZero() {
		return time.Time{}, bad
	}
	return t.UTC(), nil
}

// maxReplicaParts is the greatest number of parts an entity tag of a version
// of parts can name.
const maxReplicaParts = object.MaxParts

// parseReplicaETag reads an entity tag as S3 gives it: the MD5 of the
// content in lower-case hexadecimal, or - for a version of parts - the MD5 of
// the MD5s of its parts, a hyphen and their number. It may be quoted.
func parseReplicaETag(s string) (string, error) {
	bad := fmt.Errorf("%w: %s is an entity tag", domain.ErrInvalidArgument, headerReplicaETag)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	digest, parts, multipart := strings.Cut(s, "-")
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != newETagHash().Size() || digest != strings.ToLower(digest) {
		return "", bad
	}
	if multipart {
		n, err := strconv.Atoi(parts)
		if err != nil || n < 1 || n > maxReplicaParts || strconv.Itoa(n) != parts {
			return "", bad
		}
	}
	return s, nil
}
