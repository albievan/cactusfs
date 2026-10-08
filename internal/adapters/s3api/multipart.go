package s3api

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Multipart uploads: CreateMultipartUpload, UploadPart,
// CompleteMultipartUpload, AbortMultipartUpload, ListParts and
// ListMultipartUploads, as the Amazon S3 API reference describes them.
// UploadPartCopy and reading by part number are not implemented and answer
// 501.

// maxCompleteXML bounds the body of CompleteMultipartUpload: ten thousand
// parts, each with an entity tag and every checksum there is.
const maxCompleteXML = 8 << 20

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartUpload struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber        int     `xml:"PartNumber"`
		ETag              string  `xml:"ETag"`
		ChecksumCRC32     *string `xml:"ChecksumCRC32"`
		ChecksumCRC32C    *string `xml:"ChecksumCRC32C"`
		ChecksumCRC64NVME *string `xml:"ChecksumCRC64NVME"`
		ChecksumSHA256    *string `xml:"ChecksumSHA256"`
	} `xml:"Part"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type listPartsResult struct {
	XMLName              xml.Name    `xml:"ListPartsResult"`
	XMLNS                string      `xml:"xmlns,attr"`
	Bucket               string      `xml:"Bucket"`
	Key                  string      `xml:"Key"`
	UploadID             string      `xml:"UploadId"`
	Initiator            owner       `xml:"Initiator"`
	Owner                owner       `xml:"Owner"`
	StorageClass         string      `xml:"StorageClass"`
	PartNumberMarker     int         `xml:"PartNumberMarker"`
	NextPartNumberMarker int         `xml:"NextPartNumberMarker"`
	MaxParts             int         `xml:"MaxParts"`
	IsTruncated          bool        `xml:"IsTruncated"`
	EncodingType         string      `xml:"EncodingType,omitempty"`
	Parts                []partEntry `xml:"Part"`
}

type partEntry struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type listMultipartUploadsResult struct {
	XMLName            xml.Name       `xml:"ListMultipartUploadsResult"`
	XMLNS              string         `xml:"xmlns,attr"`
	Bucket             string         `xml:"Bucket"`
	KeyMarker          string         `xml:"KeyMarker"`
	UploadIDMarker     string         `xml:"UploadIdMarker"`
	NextKeyMarker      string         `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string         `xml:"NextUploadIdMarker,omitempty"`
	Prefix             string         `xml:"Prefix"`
	Delimiter          string         `xml:"Delimiter,omitempty"`
	MaxUploads         int            `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
	EncodingType       string         `xml:"EncodingType,omitempty"`
	Uploads            []uploadEntry  `xml:"Upload"`
	CommonPrefixes     []commonPrefix `xml:"CommonPrefixes"`
}

type uploadEntry struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	Initiator    owner  `xml:"Initiator"`
	Owner        owner  `xml:"Owner"`
	StorageClass string `xml:"StorageClass"`
	Initiated    string `xml:"Initiated"`
}

// partChecksums maps the checksum elements of a listed part to the headers
// that carry the same checksums when a part is uploaded.
func partChecksums() map[string]string {
	return map[string]string{
		"ChecksumCRC32":     checksumHeaderPrefix + "crc32",
		"ChecksumCRC32C":    checksumHeaderPrefix + "crc32c",
		"ChecksumCRC64NVME": checksumHeaderPrefix + "crc64nvme",
		"ChecksumSHA256":    checksumHeaderPrefix + "sha256",
	}
}

// uploadOf reads the uploadId of a request. An ID that is not one names no
// upload.
func uploadOf(req *request, query url.Values) (app.UploadRef, error) {
	id, err := object.ParseID(query.Get("uploadId"))
	if err != nil || id.IsZero() {
		return app.UploadRef{}, fmt.Errorf("%w: %q", domain.ErrUploadNotFound, query.Get("uploadId"))
	}
	return app.UploadRef{Bucket: req.bucket, Key: req.key, UploadID: id}, nil
}

// noLockHeaders refuses the Object Lock headers on an operation that does
// not read them: what keeps the object is said when the upload begins, and a
// client that says it later must not be told that it worked.
func noLockHeaders(header http.Header) error {
	for _, name := range lockHeaders() {
		if header.Get(name) != "" {
			return fmt.Errorf("%w: %s is not part of this operation", domain.ErrInvalidArgument, name)
		}
	}
	return nil
}

// intParameter reads a query parameter that is a number of at least low.
func intParameter(query url.Values, name string, fallback, low int) (int, error) {
	raw := query.Get(name)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < low {
		return 0, fmt.Errorf("%w: %s", domain.ErrInvalidArgument, name)
	}
	return n, nil
}

// headerChecksumType asks for a checksum of the whole object to be made when
// the upload is completed, which is not implemented.
const headerChecksumType = "x-amz-checksum-type"

func (h *Handler) createMultipartUpload(ctx context.Context, req *request) error {
	if req.Header.Get(headerChecksumType) != "" {
		return errNotImplemented()
	}
	// The request has no body to speak of; one that has is not ours.
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	if n, _ := body.body.Read(make([]byte, 1)); n != 0 {
		return fmt.Errorf("%w: a request to begin a multipart upload has no body", domain.ErrInvalidArgument)
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
	upload, err := h.cfg.Objects.CreateUpload(ctx, req.principal, app.CreateUploadInput{
		Bucket: req.bucket, Key: req.key, Lock: lock, SSE: sse,
		ContentType:        req.Header.Get("Content-Type"),
		ContentEncoding:    req.Header.Get("Content-Encoding"),
		ContentDisposition: req.Header.Get("Content-Disposition"),
		ContentLanguage:    req.Header.Get("Content-Language"),
		CacheControl:       req.Header.Get("Cache-Control"),
		UserMetadata:       userMetadata(req.Header),
	})
	if err != nil {
		return err
	}
	setSSEHeaders(req.w.Header(), upload.Encryption, req.Header.Get(headerSSECKeyMD5))
	h.writeXML(ctx, req, http.StatusOK, initiateMultipartUploadResult{
		XMLNS: s3Namespace, Bucket: req.bucket, Key: req.key, UploadID: upload.ID.String(),
	})
	return nil
}

func (h *Handler) uploadPart(ctx context.Context, req *request, query url.Values) error {
	if err := noLockHeaders(req.Header); err != nil {
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
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	customerKey, digest, err := customerKeyOf(req.Request)
	if err != nil {
		return err
	}
	defer clear(customerKey)
	part, err := h.cfg.Objects.UploadPart(ctx, req.principal, app.UploadPartInput{
		Bucket: ref.Bucket, Key: ref.Key, UploadID: ref.UploadID, Number: number, CustomerKey: customerKey,
		Body: body.body, Size: body.size, Verify: body.verify,
		Checksums: func() map[string]string { return body.checksums },
	})
	if err != nil {
		return err
	}
	header := req.w.Header()
	header.Set("ETag", quote(part.ETag))
	if customerKey != nil {
		header.Set(headerSSECAlgo, sseAES256)
		header.Set(headerSSECKeyMD5, digest)
	} else {
		header.Set(headerSSE, sseAES256)
	}
	for name, value := range body.checksums {
		header.Set(name, value)
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

// multipartETag returns the entity tag of an object made of parts: the MD5
// of the MD5s of its parts, and their number. It is formed from the entity
// tags the client listed; completing checks that they are the stored ones.
func multipartETag(parts []object.CompletedPart) (string, error) {
	sum := newETagHash()
	for _, part := range parts {
		raw, err := hex.DecodeString(part.ETag)
		if err != nil || len(raw) != sum.Size() {
			return "", fmt.Errorf("%w: part %d: entity tag", domain.ErrPartNotFound, part.Number)
		}
		sum.Write(raw)
	}
	return hex.EncodeToString(sum.Sum(nil)) + "-" + strconv.Itoa(len(parts)), nil
}

func (h *Handler) completeMultipartUpload(ctx context.Context, req *request, query url.Values) error {
	if req.Header.Get(headerChecksumType) != "" {
		return errNotImplemented()
	}
	if err := noLockHeaders(req.Header); err != nil {
		return err
	}
	ref, err := uploadOf(req, query)
	if err != nil {
		return err
	}
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	// The body says which parts make the object. It is acted on only when
	// the signature covers it.
	if !body.bound(req.payloadHash) {
		return errUnsignedBody
	}
	var in completeMultipartUpload
	err = decodeStrictXML(body.body, maxCompleteXML, &in,
		"CompleteMultipartUpload", "Part*", "PartNumber", "ETag", "ChecksumCRC32", "ChecksumCRC32C", "ChecksumCRC64NVME", "ChecksumSHA256")
	if err != nil {
		return err
	}
	if _, err := body.verify(); err != nil {
		return err
	}
	if len(in.Parts) == 0 || len(in.Parts) > object.MaxParts {
		return errMalformedXML
	}
	parts := make([]object.CompletedPart, len(in.Parts))
	for i, listed := range in.Parts {
		parts[i] = object.CompletedPart{Number: listed.PartNumber, ETag: strings.Trim(strings.TrimSpace(listed.ETag), `"`)}
		for element, value := range map[string]*string{
			"ChecksumCRC32": listed.ChecksumCRC32, "ChecksumCRC32C": listed.ChecksumCRC32C,
			"ChecksumCRC64NVME": listed.ChecksumCRC64NVME, "ChecksumSHA256": listed.ChecksumSHA256,
		} {
			if value == nil {
				continue
			}
			if parts[i].Checksums == nil {
				parts[i].Checksums = map[string]string{}
			}
			parts[i].Checksums[partChecksums()[element]] = strings.TrimSpace(*value)
		}
	}
	etag, err := multipartETag(parts)
	if err != nil {
		return err
	}
	customerKey, digest, err := customerKeyOf(req.Request)
	if err != nil {
		return err
	}
	defer clear(customerKey)
	version, err := h.cfg.Objects.CompleteUpload(ctx, req.principal, app.CompleteUploadInput{
		Bucket: ref.Bucket, Key: ref.Key, UploadID: ref.UploadID, Parts: parts, ETag: etag, CustomerKey: customerKey,
	})
	if err != nil {
		return err
	}
	header := req.w.Header()
	setSSEHeaders(header, version.Encryption, digest)
	setVersionHeader(header, version, false)
	h.writeXML(ctx, req, http.StatusOK, completeMultipartUploadResult{
		XMLNS: s3Namespace, Bucket: req.bucket, Key: req.key, ETag: quote(version.ETag),
		Location: "/" + req.bucket + "/" + (&url.URL{Path: req.key}).EscapedPath(),
	})
	return nil
}

func (h *Handler) abortMultipartUpload(ctx context.Context, req *request, query url.Values) error {
	ref, err := uploadOf(req, query)
	if err != nil {
		return err
	}
	if err := h.cfg.Objects.AbortUpload(ctx, req.principal, ref); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func listPartsParameters() []string {
	return []string{"uploadId", "max-parts", "part-number-marker", "encoding-type"}
}

func (h *Handler) listParts(ctx context.Context, req *request, query url.Values) error {
	ref, err := uploadOf(req, query)
	if err != nil {
		return err
	}
	encoding := query.Get("encoding-type")
	if encoding != "" && encoding != encodingURL {
		return fmt.Errorf("%w: encoding-type", domain.ErrInvalidArgument)
	}
	maxParts, err := intParameter(query, "max-parts", object.MaxListKeys, 0)
	if err != nil {
		return err
	}
	maxParts = min(maxParts, object.MaxListKeys)
	marker, err := intParameter(query, "part-number-marker", 0, 0)
	if err != nil {
		return err
	}
	customerKey, _, err := customerKeyOf(req.Request)
	if err != nil {
		return err
	}
	defer clear(customerKey)
	out, err := h.cfg.Objects.ListParts(ctx, req.principal, app.ListPartsInput{UploadRef: ref, After: marker, Max: maxParts, CustomerKey: customerKey})
	if err != nil {
		return err
	}
	key := req.key
	if encoding == encodingURL {
		key = url.QueryEscape(key)
	}
	who := owner{ID: req.principal.AccessKeyID, DisplayName: req.principal.AccessKeyID}
	result := listPartsResult{
		XMLNS: s3Namespace, Bucket: req.bucket, Key: key, UploadID: ref.UploadID.String(), EncodingType: encoding,
		Initiator: owner{ID: out.Upload.Initiator, DisplayName: out.Upload.Initiator}, Owner: who, StorageClass: storageClass,
		PartNumberMarker: marker, MaxParts: maxParts, IsTruncated: out.Truncated,
	}
	if out.Truncated {
		result.NextPartNumberMarker = out.NextAfter
	}
	for _, part := range out.Parts {
		result.Parts = append(result.Parts, partEntry{
			PartNumber: part.Number, LastModified: xmlTime(part.ModifiedAt), ETag: quote(part.ETag), Size: part.Size,
		})
	}
	h.writeXML(ctx, req, http.StatusOK, result)
	return nil
}

func listUploadsParameters() []string {
	return []string{"uploads", "prefix", "delimiter", "key-marker", "upload-id-marker", "max-uploads", "encoding-type"}
}

func (h *Handler) listMultipartUploads(ctx context.Context, req *request, query url.Values) error {
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
	maxUploads, err := intParameter(query, "max-uploads", object.MaxListKeys, 0)
	if err != nil {
		return err
	}
	maxUploads = min(maxUploads, object.MaxListKeys)
	in := app.ListUploadsInput{
		Bucket: req.bucket, Prefix: query.Get("prefix"), Delimiter: query.Get("delimiter"),
		KeyMarker: query.Get("key-marker"), Max: maxUploads,
	}
	if raw := query.Get("upload-id-marker"); raw != "" && in.KeyMarker != "" {
		// Without a key marker the upload ID marker means nothing, as
		// the S3 reference has it.
		if in.UploadIDMarker, err = object.ParseID(raw); err != nil {
			return fmt.Errorf("%w: upload-id-marker", domain.ErrInvalidArgument)
		}
	}
	out, err := h.cfg.Objects.ListUploads(ctx, req.principal, in)
	if err != nil {
		return err
	}
	result := listMultipartUploadsResult{
		XMLNS: s3Namespace, Bucket: req.bucket, Prefix: encode(in.Prefix), Delimiter: encode(in.Delimiter),
		KeyMarker: encode(in.KeyMarker), UploadIDMarker: query.Get("upload-id-marker"), MaxUploads: maxUploads,
		IsTruncated: out.Truncated, EncodingType: encoding,
	}
	if out.Truncated {
		result.NextKeyMarker = encode(out.NextKeyMarker)
		if !out.NextUploadIDMarker.IsZero() {
			result.NextUploadIDMarker = out.NextUploadIDMarker.String()
		}
	}
	who := owner{ID: req.principal.AccessKeyID, DisplayName: req.principal.AccessKeyID}
	for _, upload := range out.Uploads {
		result.Uploads = append(result.Uploads, uploadEntry{
			Key: encode(upload.Key), UploadID: upload.ID.String(), Owner: who, StorageClass: storageClass,
			Initiator: owner{ID: upload.Initiator, DisplayName: upload.Initiator}, Initiated: xmlTime(upload.InitiatedAt),
		})
	}
	for _, prefix := range out.CommonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix{Prefix: encode(prefix)})
	}
	h.writeXML(ctx, req, http.StatusOK, result)
	return nil
}
