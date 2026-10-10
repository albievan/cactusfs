package s3api

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Copies and conditional requests: CopyObject, UploadPartCopy, and the
// If-Match family of headers on reads and on the source of a copy.

const (
	headerCopySource        = "x-amz-copy-source"
	headerCopySourceRange   = "x-amz-copy-source-range"
	headerCopySourceVersion = "x-amz-copy-source-version-id"
	headerMetadataDirective = "x-amz-metadata-directive"
	headerTaggingDirective  = "x-amz-tagging-directive"
	copySourceIfPrefix      = "x-amz-copy-source-if-"
)

// copyHeaders are the headers that belong to a copy and to nothing else.
func copyHeaders() []string {
	return append([]string{
		headerCopySource, headerCopySourceRange, headerMetadataDirective, headerTaggingDirective,
		copySourceIfPrefix + "match", copySourceIfPrefix + "none-match", copySourceIfPrefix + "modified-since", copySourceIfPrefix + "unmodified-since",
	}, copySourceSSEHeaders()...)
}

type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	XMLNS        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

type copyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	XMLNS        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// entityTags reads a list of entity tags as a conditional header gives it:
// separated by commas, quoted or not, weak or not, or "*".
//
// strong says that the comparison is the strong one of If-Match, which a
// weak tag never satisfies: such a tag is kept in a form no version has.
func entityTags(value string, strong bool) ([]string, error) {
	var tags []string
	for _, tag := range strings.Split(value, ",") {
		tag, weak := strings.CutPrefix(strings.TrimSpace(tag), "W/")
		if len(tag) >= 2 && strings.HasPrefix(tag, `"`) && strings.HasSuffix(tag, `"`) {
			tag = tag[1 : len(tag)-1]
		}
		if tag == "" || strings.ContainsAny(tag, `" `) {
			return nil, fmt.Errorf("%w: entity tag in a condition", domain.ErrInvalidArgument)
		}
		if weak && strong {
			tag = `W/"` + tag + `"`
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

// conditionsOf reads the four conditional headers whose names begin with
// prefix: "If-" for a read, "x-amz-copy-source-if-" for the source of a copy.
func conditionsOf(header http.Header, prefix string) (object.Conditions, error) {
	var c object.Conditions
	for suffix, into := range map[string]*[]string{"match": &c.IfMatch, "none-match": &c.IfNoneMatch} {
		values := header.Values(prefix + suffix)
		if len(values) == 0 {
			continue
		}
		tags, err := entityTags(strings.Join(values, ","), suffix == "match")
		if err != nil {
			return object.Conditions{}, err
		}
		*into = tags
	}
	for _, date := range []struct {
		suffix string
		into   func(v string) error
	}{
		{"modified-since", func(v string) error { t, err := conditionDate(v); c.IfModifiedSince = t; return err }},
		{"unmodified-since", func(v string) error { t, err := conditionDate(v); c.IfUnmodifiedSince = t; return err }},
	} {
		values := header.Values(prefix + date.suffix)
		switch {
		case len(values) == 0:
		case len(values) > 1:
			return object.Conditions{}, fmt.Errorf("%w: %s%s is given more than once", domain.ErrInvalidArgument, prefix, date.suffix)
		default:
			if err := date.into(values[0]); err != nil {
				return object.Conditions{}, fmt.Errorf("%w: %s%s is not an HTTP date", domain.ErrInvalidArgument, prefix, date.suffix)
			}
		}
	}
	return c, nil
}

// conditionDate reads the date of a condition. The first instant there is
// stands, in a condition, for "no date": it is not accepted as one.
func conditionDate(value string) (time.Time, error) {
	t, err := http.ParseTime(value)
	if err == nil && t.IsZero() {
		err = fmt.Errorf("%w: a date before any object", domain.ErrInvalidArgument)
	}
	return t, err
}

// noChecksums refuses the checksum headers on a copy: no checksum of the
// copy is computed, and a client that asks for one must not think it was.
func noChecksums(header http.Header) error {
	for name := range header {
		if strings.HasPrefix(strings.ToLower(name), checksumHeaderPrefix) || strings.EqualFold(name, "x-amz-sdk-checksum-algorithm") {
			return errNotImplemented()
		}
	}
	return nil
}

// notModified answers a read whose conditions say the caller has the
// version already: no body, and the entity tag it has.
func notModified(req *request, version object.Version) {
	header := req.w.Header()
	header.Set("ETag", quote(version.ETag))
	header.Set("Last-Modified", version.ModifiedAt.UTC().Format(http.TimeFormat))
	if version.CacheControl != "" {
		header.Set("Cache-Control", version.CacheControl)
	}
	setVersionHeader(header, version, req.version != nil)
	req.w.WriteHeader(http.StatusNotModified)
}

// copySourceOf reads what a copy reads: the x-amz-copy-source header, which
// names a bucket, a key and perhaps a version; the customer key of the
// source; and the conditions on it.
func copySourceOf(r *http.Request) (app.GetInput, error) {
	values := r.Header.Values(headerCopySource)
	if len(values) != 1 || values[0] == "" {
		return app.GetInput{}, fmt.Errorf("%w: %s must be given once", domain.ErrInvalidArgument, headerCopySource)
	}
	raw, query, _ := strings.Cut(values[0], "?")
	path, err := url.PathUnescape(strings.TrimPrefix(raw, "/"))
	if err != nil {
		return app.GetInput{}, fmt.Errorf("%w: %s", domain.ErrInvalidArgument, headerCopySource)
	}
	bucketName, key, ok := strings.Cut(path, "/")
	if !ok || bucketName == "" || key == "" {
		return app.GetInput{}, fmt.Errorf("%w: %s names a bucket and a key", domain.ErrInvalidArgument, headerCopySource)
	}
	in := app.GetInput{Bucket: bucketName, Key: key}
	if query != "" {
		parsed, err := url.ParseQuery(query)
		if err != nil || len(parsed) != 1 || len(parsed["versionId"]) != 1 {
			return app.GetInput{}, fmt.Errorf("%w: %s takes versionId and nothing else", domain.ErrInvalidArgument, headerCopySource)
		}
		ref, err := object.ParseVersionRef(parsed.Get("versionId"))
		if err != nil {
			return app.GetInput{}, err
		}
		in.Version = &ref
	}
	if in.Conditions, err = conditionsOf(r.Header, copySourceIfPrefix); err != nil {
		return app.GetInput{}, err
	}
	if in.CustomerKey, _, err = customerKeyFrom(r, copySourceSSEHeaders()); err != nil {
		return app.GetInput{}, err
	}
	return in, nil
}

// directive reads a header that says whether a copy keeps what the source
// has or takes what the request brings.
func directive(header http.Header, name string) (replace bool, err error) {
	switch values := header.Values(name); {
	case len(values) == 0, len(values) == 1 && values[0] == "COPY":
		return false, nil
	case len(values) == 1 && values[0] == "REPLACE":
		return true, nil
	default:
		return false, fmt.Errorf("%w: %s is COPY or REPLACE", domain.ErrInvalidArgument, name)
	}
}

// etagDigest computes the entity tag of bytes as they pass.
func etagDigest(body io.Reader) (io.Reader, func() (string, error)) {
	sum := newETagHash()
	return io.TeeReader(body, sum), func() (string, error) { return hex.EncodeToString(sum.Sum(nil)), nil }
}

// noBody refuses a copy that brings a body: its bytes come from the source.
func noBody(req *request) error {
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	n, err := body.body.Read(make([]byte, 1))
	if n != 0 {
		return fmt.Errorf("%w: a copy has no body", domain.ErrInvalidArgument)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (h *Handler) copyObject(ctx context.Context, req *request) error {
	if err := noChecksums(req.Header); err != nil {
		return err
	}
	if err := noBody(req); err != nil {
		return err
	}
	source, err := copySourceOf(req.Request)
	if err != nil {
		return err
	}
	defer clear(source.CustomerKey)
	if req.Header.Get(headerCopySourceRange) != "" {
		return fmt.Errorf("%w: %s belongs to the copy of a part", domain.ErrInvalidArgument, headerCopySourceRange)
	}
	replaceMetadata, err := directive(req.Header, headerMetadataDirective)
	if err != nil {
		return err
	}
	replaceTags, err := directive(req.Header, headerTaggingDirective)
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
	tags, err := tagsOf(req.Header)
	if err != nil {
		return err
	}
	if tags != nil && !replaceTags {
		return fmt.Errorf("%w: %s comes with %s: REPLACE", domain.ErrInvalidArgument, headerTagging, headerTaggingDirective)
	}
	out, err := h.cfg.Objects.Copy(ctx, req.principal, app.CopyInput{
		Source: source, ReplaceMetadata: replaceMetadata, ReplaceTags: replaceTags, Digest: etagDigest,
		Destination: app.PutInput{
			Bucket: req.bucket, Key: req.key, Lock: lock, SSE: sse, Tags: tags,
			ContentType:        req.Header.Get("Content-Type"),
			ContentEncoding:    req.Header.Get("Content-Encoding"),
			ContentDisposition: req.Header.Get("Content-Disposition"),
			ContentLanguage:    req.Header.Get("Content-Language"),
			CacheControl:       req.Header.Get("Cache-Control"),
			UserMetadata:       userMetadata(req.Header),
		},
	})
	if err != nil {
		return err
	}
	header := req.w.Header()
	setSSEHeaders(header, out.Version.Encryption, req.Header.Get(headerSSECKeyMD5))
	setVersionHeader(header, out.Version, false)
	if out.Source.Versioned || source.Version != nil {
		header.Set(headerCopySourceVersion, out.Source.Ref().String())
	}
	h.writeXML(ctx, req, http.StatusOK, copyObjectResult{
		XMLNS: s3Namespace, ETag: quote(out.Version.ETag), LastModified: xmlTime(out.Version.ModifiedAt),
	})
	return nil
}

func (h *Handler) uploadPartCopy(ctx context.Context, req *request, query url.Values) error {
	if err := noLockHeaders(req.Header); err != nil {
		return err
	}
	if err := noChecksums(req.Header); err != nil {
		return err
	}
	if err := noBody(req); err != nil {
		return err
	}
	ref, err := uploadOf(req, query)
	if err != nil {
		return err
	}
	number, err := strconv.Atoi(query.Get("partNumber"))
	if err != nil {
		return fmt.Errorf("%w: partNumber", domain.ErrInvalidArgument)
	}
	source, err := copySourceOf(req.Request)
	if err != nil {
		return err
	}
	defer clear(source.CustomerKey)
	if raw := req.Header.Get(headerCopySourceRange); raw != "" {
		// The range of a copy is one range with both ends, and must lie
		// inside the source: a range that reaches past the end is an
		// error here, not a shorter part.
		spec := parseRange(raw)
		if spec == nil || spec.Suffix != 0 || spec.Last < spec.First {
			return fmt.Errorf("%w: %s is bytes=first-last", domain.ErrInvalidArgument, headerCopySourceRange)
		}
		source.Range = spec
	}
	for _, name := range []string{headerMetadataDirective, headerTaggingDirective, headerTagging} {
		if len(req.Header.Values(name)) > 0 {
			return fmt.Errorf("%w: %s is not part of this operation", domain.ErrInvalidArgument, name)
		}
	}
	customerKey, _, err := customerKeyOf(req.Request)
	if err != nil {
		return err
	}
	defer clear(customerKey)
	part, err := h.cfg.Objects.CopyPart(ctx, req.principal, app.CopyPartInput{
		Source: source, Digest: etagDigest,
		Part: app.UploadPartInput{Bucket: ref.Bucket, Key: ref.Key, UploadID: ref.UploadID, Number: number, CustomerKey: customerKey},
	})
	if err != nil {
		return err
	}
	h.writeXML(ctx, req, http.StatusOK, copyPartResult{XMLNS: s3Namespace, ETag: quote(part.ETag), LastModified: xmlTime(part.ModifiedAt)})
	return nil
}

func listObjectsV1Parameters() []string {
	return []string{"prefix", "delimiter", "marker", "max-keys", "encoding-type"}
}

type listBucketResultV1 struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	XMLNS          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Marker         string         `xml:"Marker"`
	NextMarker     string         `xml:"NextMarker,omitempty"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	MaxKeys        int            `xml:"MaxKeys"`
	IsTruncated    bool           `xml:"IsTruncated"`
	EncodingType   string         `xml:"EncodingType,omitempty"`
	Contents       []objectEntry  `xml:"Contents"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
}

// listObjectsV1 is the first version of ListObjects: a marker instead of a
// continuation token, and the owner of every object.
func (h *Handler) listObjectsV1(ctx context.Context, req *request, query url.Values) error {
	encoding := query.Get("encoding-type")
	if encoding != "" && encoding != encodingURL {
		return fmt.Errorf("%w: encoding-type", domain.ErrInvalidArgument)
	}
	encode := func(s string) string {
		if encoding == encodingURL {
			return url.QueryEscape(s)
		}
		return s
	}
	maxKeys, err := intParameter(query, "max-keys", defaultMaxKeys, 0)
	if err != nil {
		return err
	}
	opts := object.ListOptions{
		Prefix: query.Get("prefix"), Delimiter: query.Get("delimiter"), Continuation: query.Get("marker"), MaxKeys: maxKeys,
	}
	page, err := h.cfg.Objects.List(ctx, req.principal, req.bucket, opts)
	if err != nil {
		return err
	}
	result := listBucketResultV1{
		XMLNS: s3Namespace, Name: req.bucket, Prefix: encode(opts.Prefix), Marker: encode(opts.Continuation),
		Delimiter: encode(opts.Delimiter), MaxKeys: maxKeys, IsTruncated: page.Truncated, EncodingType: encoding,
	}
	if page.Truncated {
		// S3 gives NextMarker only with a delimiter, and has clients use
		// the last key otherwise. Giving it always harms nobody.
		result.NextMarker = encode(page.NextContinuation)
	}
	who := &owner{ID: req.principal.AccessKeyID, DisplayName: req.principal.AccessKeyID}
	for _, v := range page.Objects {
		result.Contents = append(result.Contents, objectEntry{
			Key: encode(v.Key), LastModified: xmlTime(v.ModifiedAt), ETag: quote(v.ETag), Size: v.Size, StorageClass: storageClass, Owner: who,
		})
	}
	for _, prefix := range page.CommonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix{Prefix: encode(prefix)})
	}
	h.writeXML(ctx, req, http.StatusOK, result)
	return nil
}
