package s3api

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

const (
	userMetadataPrefix = "X-Amz-Meta-"
	defaultContentType = "binary/octet-stream"
	defaultMaxKeys     = 1000
	encodingURL        = "url"
)

func (h *Handler) listBuckets(ctx context.Context, req *request) error {
	buckets, err := h.cfg.Buckets.List(ctx, req.principal)
	if err != nil {
		return err
	}
	result := listAllMyBucketsResult{
		XMLNS: s3Namespace,
		Owner: owner{ID: req.principal.AccessKeyID, DisplayName: req.principal.AccessKeyID},
	}
	for _, b := range buckets {
		result.Buckets = append(result.Buckets, bucketEntry{Name: b.Name, CreationDate: xmlTime(b.CreatedAt)})
	}
	h.writeXML(ctx, req, http.StatusOK, result)
	return nil
}

func (h *Handler) createBucket(ctx context.Context, req *request) error {
	// The body is optional and names a region; a deployment has one. Like
	// every request body it is checked against what the client declared
	// before the request has any effect.
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	if _, err := parseCreateBucketConfiguration(body.body); err != nil {
		return err
	}
	if _, err := body.verify(); err != nil {
		return err
	}
	create := h.cfg.Buckets.Create
	switch req.Header.Get(headerLockEnabled) {
	case "", "false":
	case "true":
		create = h.cfg.Buckets.CreateWithObjectLock
	default:
		return fmt.Errorf("%w: %s", domain.ErrInvalidArgument, headerLockEnabled)
	}
	if err := create(ctx, req.principal, req.bucket); err != nil {
		return err
	}
	req.w.Header().Set("Location", "/"+req.bucket)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) deleteBucket(ctx context.Context, req *request) error {
	if err := h.cfg.Buckets.Delete(ctx, req.principal, req.bucket); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) headBucket(ctx context.Context, req *request) error {
	if _, err := h.cfg.Buckets.Get(ctx, req.principal, req.bucket); err != nil {
		return err
	}
	req.w.Header().Set("X-Amz-Bucket-Region", h.cfg.Region)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getBucketLocation(ctx context.Context, req *request) error {
	if err := h.cfg.Buckets.Location(ctx, req.principal, req.bucket); err != nil {
		return err
	}
	// The S3 API reports its first region as an empty constraint.
	region := h.cfg.Region
	if region == DefaultRegion {
		region = ""
	}
	h.writeXML(ctx, req, http.StatusOK, locationConstraint{XMLNS: s3Namespace, Region: region})
	return nil
}

func (h *Handler) listObjectsV2(ctx context.Context, req *request) error {
	query := req.URL.Query()
	opts := object.ListOptions{
		Prefix:     query.Get("prefix"),
		Delimiter:  query.Get("delimiter"),
		StartAfter: query.Get("start-after"),
		MaxKeys:    defaultMaxKeys,
	}
	if raw := query.Get("max-keys"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return fmt.Errorf("%w: max-keys", domain.ErrInvalidArgument)
		}
		opts.MaxKeys = n
	}
	token := query.Get("continuation-token")
	if token != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(decoded) == 0 {
			return fmt.Errorf("%w: continuation-token", domain.ErrInvalidArgument)
		}
		opts.Continuation = string(decoded)
	}
	encoding := query.Get("encoding-type")
	if encoding != "" && encoding != encodingURL {
		return fmt.Errorf("%w: encoding-type", domain.ErrInvalidArgument)
	}
	// With encoding-type=url, names are percent-encoded in the response so
	// that characters XML cannot carry survive.
	encode := func(s string) string {
		if encoding == encodingURL {
			return url.QueryEscape(s)
		}
		return s
	}

	page, err := h.cfg.Objects.List(ctx, req.principal, req.bucket, opts)
	if err != nil {
		return err
	}

	result := listBucketResult{
		XMLNS:             s3Namespace,
		Name:              req.bucket,
		Prefix:            encode(opts.Prefix),
		Delimiter:         encode(opts.Delimiter),
		MaxKeys:           opts.MaxKeys,
		KeyCount:          len(page.Objects) + len(page.CommonPrefixes),
		IsTruncated:       page.Truncated,
		EncodingType:      encoding,
		ContinuationToken: token,
		StartAfter:        encode(opts.StartAfter),
	}
	if page.Truncated {
		result.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(page.NextContinuation))
	}
	fetchOwner := query.Get("fetch-owner") == "true"
	for _, v := range page.Objects {
		entry := objectEntry{
			Key: encode(v.Key), LastModified: xmlTime(v.ModifiedAt), ETag: quote(v.ETag), Size: v.Size, StorageClass: storageClass,
		}
		if fetchOwner {
			entry.Owner = &owner{ID: req.principal.AccessKeyID, DisplayName: req.principal.AccessKeyID}
		}
		result.Contents = append(result.Contents, entry)
	}
	for _, prefix := range page.CommonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix{Prefix: encode(prefix)})
	}
	h.writeXML(ctx, req, http.StatusOK, result)
	return nil
}

func quote(etag string) string {
	return `"` + etag + `"`
}

func (h *Handler) putObject(ctx context.Context, req *request) error {
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	lock, err := lockOf(req.Header)
	if err != nil {
		return err
	}
	sse, err := sseOf(req.Request)
	if err != nil {
		return err
	}
	defer clear(sse.CustomerKey)
	digest := req.Header.Get(headerSSECKeyMD5)
	in := app.PutInput{
		Lock:               lock,
		SSE:                sse,
		Bucket:             req.bucket,
		Key:                req.key,
		Body:               body.body,
		Size:               body.size,
		ContentType:        req.Header.Get("Content-Type"),
		ContentEncoding:    body.contentEncoding,
		ContentDisposition: req.Header.Get("Content-Disposition"),
		ContentLanguage:    req.Header.Get("Content-Language"),
		CacheControl:       req.Header.Get("Cache-Control"),
		UserMetadata:       userMetadata(req.Header),
		Verify:             body.verify,
	}
	version, err := h.cfg.Objects.Put(ctx, req.principal, in)
	if err != nil {
		return err
	}

	header := req.w.Header()
	header.Set("ETag", quote(version.ETag))
	setSSEHeaders(header, version.Encryption, digest)
	setVersionHeader(header, version, false)
	for name, value := range body.checksums {
		header.Set(name, value)
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

// userMetadata extracts the x-amz-meta-* headers, keyed by lowercase name
// without the prefix.
func userMetadata(header http.Header) map[string]string {
	var metadata map[string]string
	for name, values := range header {
		key, ok := strings.CutPrefix(name, userMetadataPrefix)
		if !ok || key == "" {
			continue
		}
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata[strings.ToLower(key)] = strings.Join(values, ",")
	}
	return metadata
}

func (h *Handler) getObject(ctx context.Context, req *request) error {
	customerKey, digest, err := customerKeyOf(req.Request)
	if err != nil {
		return err
	}
	defer clear(customerKey)
	in := app.GetInput{
		Bucket: req.bucket, Key: req.key, Range: parseRange(req.Header.Get("Range")), Version: req.version, CustomerKey: customerKey,
	}
	out, err := h.cfg.Objects.Get(ctx, req.principal, in)
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Body.Close(); err != nil {
			h.cfg.Logger.WarnContext(ctx, "close object", slog.String("request_id", req.id), slog.Any("error", err))
		}
	}()

	header := req.w.Header()
	setObjectHeaders(header, out.Version, digest)
	setVersionHeader(header, out.Version, req.version != nil)
	header.Set("Content-Length", strconv.FormatInt(out.Range.Length, 10))
	status := http.StatusOK
	if in.Range != nil {
		status = http.StatusPartialContent
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d",
			out.Range.Offset, out.Range.Offset+out.Range.Length-1, out.Version.Size))
	}
	req.w.WriteHeader(status)

	if _, err := io.Copy(req.w, out.Body); err != nil {
		// Either the client went away or stored data failed verification
		// part-way through. The use case returned no damaged byte; the
		// response is cut short so that it cannot pass for a whole object.
		return fmt.Errorf("s3api.GetObject: %w", err)
	}
	return nil
}

func (h *Handler) headObject(ctx context.Context, req *request) error {
	customerKey, digest, err := customerKeyOf(req.Request)
	if err != nil {
		return err
	}
	defer clear(customerKey)
	version, err := h.cfg.Objects.Stat(ctx, req.principal, app.GetInput{
		Bucket: req.bucket, Key: req.key, Version: req.version, CustomerKey: customerKey,
	})
	if err != nil {
		return err
	}
	header := req.w.Header()
	setObjectHeaders(header, version, digest)
	setVersionHeader(header, version, req.version != nil)
	header.Set("Content-Length", strconv.FormatInt(version.Size, 10))
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) deleteObject(ctx context.Context, req *request) error {
	bypass, err := bypassOf(req.Header)
	if err != nil {
		return err
	}
	out, err := h.cfg.Objects.Delete(ctx, req.principal, app.DeleteInput{
		Bucket: req.bucket, Key: req.key, Version: req.version, BypassGovernance: bypass,
	})
	if err != nil {
		return err
	}
	header := req.w.Header()
	switch {
	case out.Marker != nil:
		// The object is deleted, and this is the version that says so.
		header.Set(headerDeleteMarker, "true")
		header.Set(headerVersionID, out.Marker.Ref().String())
	case req.version != nil:
		header.Set(headerVersionID, req.version.String())
		if out.Removed != nil && out.Removed.DeleteMarker {
			header.Set(headerDeleteMarker, "true")
		}
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func setObjectHeaders(header http.Header, v object.Version, customerDigest string) {
	contentType := v.ContentType
	if contentType == "" {
		contentType = defaultContentType
	}
	header.Set("Content-Type", contentType)
	header.Set("ETag", quote(v.ETag))
	header.Set("Last-Modified", v.ModifiedAt.UTC().Format(http.TimeFormat))
	header.Set("Accept-Ranges", "bytes")
	setSSEHeaders(header, v.Encryption, customerDigest)
	setLockHeaders(header, v.Lock)
	for name, value := range map[string]string{
		"Content-Encoding":    v.ContentEncoding,
		"Content-Disposition": v.ContentDisposition,
		"Content-Language":    v.ContentLanguage,
		"Cache-Control":       v.CacheControl,
	} {
		if value != "" {
			header.Set(name, value)
		}
	}
	for key, value := range v.UserMetadata {
		header.Set(userMetadataPrefix+key, value)
	}
}

// parseRange parses an HTTP Range header with a single byte range. As HTTP
// allows, a header that is malformed, in another unit or that lists several
// ranges is ignored and the whole object is served.
func parseRange(header string) *object.RangeSpec {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return nil
	}
	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return nil
	}
	switch {
	case first == "":
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return nil
		}
		return &object.RangeSpec{Suffix: suffix}
	case last == "":
		start, err := strconv.ParseInt(first, 10, 64)
		if err != nil || start < 0 {
			return nil
		}
		return &object.RangeSpec{First: start, Last: -1}
	default:
		start, err1 := strconv.ParseInt(first, 10, 64)
		end, err2 := strconv.ParseInt(last, 10, 64)
		if err1 != nil || err2 != nil || start < 0 || end < start {
			return nil
		}
		return &object.RangeSpec{First: start, Last: end}
	}
}
