package s3api

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Versioning of buckets and versions of objects, implemented from the public
// Amazon S3 API reference (PutBucketVersioning, GetBucketVersioning,
// ListObjectVersions, DeleteObjects, and the versionId parameter of the
// object operations).

const (
	headerVersionID    = "X-Amz-Version-Id"
	headerDeleteMarker = "X-Amz-Delete-Marker"
	// maxXMLDepth bounds how deeply the elements of a request body nest.
	maxXMLDepth = 8
	// maxDeleteObjects is the most keys one DeleteObjects request names.
	maxDeleteObjects = 1000
	// maxDeleteXML bounds the body of DeleteObjects: a thousand keys of
	// the greatest length, with room for the markup.
	maxDeleteXML = 2 << 20
)

// decodeStrictXML reads a request body that decides something: which
// versions to destroy, what a bucket keeps. It is bounded, and it is strict -
// a document type declaration, a processing instruction, nesting deeper than
// any of these documents needs, an element that is not one of allowed, or an
// element said twice where the caller did not mark it (with a trailing "*")
// as one that repeats, is malformed, never ignored. An element that was silently skipped could be the
// one that mattered.
// xmlShape says which elements a document may have, and where: for each
// element, by name, the children it may have, and under "" the root. A child
// written with a trailing "*" may repeat.
type xmlShape map[string][]string

// decodeShapedXML is decodeStrictXML for a document whose elements nest: an
// element is accepted only under the parent the shape gives it, and no
// element carries an attribute other than the namespace of the root. The
// flat list of decodeStrictXML would accept a known element in the wrong
// place, and the decoder would then drop it without a word - a rule read
// without its prefix is a rule about every key.
//
// When the document has an element that notImplemented names, wherever it
// is, the error is errNotImplemented.
func decodeShapedXML(body io.Reader, limit int64, into any, shape xmlShape, notImplemented ...string) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return fmt.Errorf("%w: reading the request body", domain.ErrIncompleteBody)
	}
	if int64(len(data)) > limit || len(data) == 0 {
		return errMalformedXML
	}
	may := func(parent, child string) bool {
		for _, name := range shape[parent] {
			if strings.TrimSuffix(name, "*") == child {
				return true
			}
		}
		return false
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	var open []string
	misplaced := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errMalformedXML
		}
		switch t := token.(type) {
		case xml.StartElement:
			if slices.Contains(notImplemented, t.Name.Local) {
				return errNotImplemented()
			}
			parent := ""
			if len(open) > 0 {
				parent = open[len(open)-1]
			}
			for _, attr := range t.Attr {
				namespace := attr.Name.Local == "xmlns" && attr.Name.Space == ""
				if !namespace || len(open) > 0 {
					misplaced = true
				}
			}
			if !may(parent, t.Name.Local) {
				misplaced = true
			}
			open = append(open, t.Name.Local)
		case xml.EndElement:
			open = open[:len(open)-1]
		}
	}
	if misplaced {
		return errMalformedXML
	}
	// The root comes first: decodeStrictXML takes the first name for it.
	flat := slices.Clone(shape[""])
	for _, children := range shape {
		for _, child := range children {
			if base := strings.TrimSuffix(child, "*"); !slices.Contains(flat, base) && !slices.Contains(flat, base+"*") {
				flat = append(flat, child)
			}
		}
	}
	return decodeStrictXML(bytes.NewReader(data), limit, into, flat...)
}

func decodeStrictXML(body io.Reader, limit int64, into any, allowed ...string) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return fmt.Errorf("%w: reading the request body", domain.ErrIncompleteBody)
	}
	if int64(len(data)) > limit || len(data) == 0 {
		return errMalformedXML
	}
	// A name written with a trailing "*" may repeat inside its parent.
	var repeatable []string
	allowed = slices.Clone(allowed)
	for i, name := range allowed {
		if base, ok := strings.CutSuffix(name, "*"); ok {
			allowed[i], repeatable = base, append(repeatable, base)
		}
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	depth, roots := 0, 0
	// The names of the open elements, and what each has had inside it.
	var open []string
	var seen []map[string]int
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errMalformedXML
		}
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			name := t.Name.Local
			if depth > maxXMLDepth || !slices.Contains(allowed, name) || slices.Contains(open, name) {
				// Too deep, not a name of this document, or an
				// element inside one of its own kind.
				return errMalformedXML
			}
			if depth == 1 {
				// One document, with the first allowed name as its root.
				if roots++; roots > 1 || name != allowed[0] {
					return errMalformedXML
				}
			} else {
				// Only what the caller says may repeat, repeats: the
				// entries of a list. A second <Status>, a second <Rule>
				// or a second <Key> in one <Object> is a document that
				// says two things.
				parent := seen[len(seen)-1]
				if parent[name]++; parent[name] > 1 && !slices.Contains(repeatable, name) {
					return errMalformedXML
				}
			}
			open, seen = append(open, name), append(seen, map[string]int{})
		case xml.EndElement:
			depth--
			inside := seen[len(seen)-1]
			if inside["#text"] > 0 && len(inside) > 1 {
				// Text beside elements: "<Quiet>true<Object>...".
				return errMalformedXML
			}
			open, seen = open[:len(open)-1], seen[:len(seen)-1]
		case xml.CharData:
			if len(seen) > 0 && len(bytes.TrimSpace(t)) > 0 {
				seen[len(seen)-1]["#text"] = 1
			}
		case xml.Directive:
			return errMalformedXML
		case xml.ProcInst:
			if t.Target != "xml" {
				return errMalformedXML
			}
		}
	}
	if err := xml.Unmarshal(data, into); err != nil {
		return errMalformedXML
	}
	return nil
}

type versioningConfiguration struct {
	XMLName   xml.Name `xml:"VersioningConfiguration"`
	XMLNS     string   `xml:"xmlns,attr,omitempty"`
	Status    string   `xml:"Status,omitempty"`
	MFADelete string   `xml:"MfaDelete,omitempty"`
}

func (h *Handler) putBucketVersioning(ctx context.Context, req *request) error {
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	if !body.bound(req.payloadHash) {
		return errUnsignedBody
	}
	var cfg versioningConfiguration
	if err := decodeStrictXML(body.body, maxRequestXML, &cfg, "VersioningConfiguration", "Status", "MfaDelete"); err != nil {
		return err
	}
	if _, err := body.verify(); err != nil {
		return err
	}
	if cfg.MFADelete != "" && cfg.MFADelete != "Disabled" {
		// MFA delete is not implemented; saying yes to it would promise
		// a protection that is not there.
		return errNotImplemented()
	}
	if cfg.Status != string(bucket.VersioningEnabled) && cfg.Status != string(bucket.VersioningSuspended) {
		return errMalformedXML
	}
	if err := h.cfg.Buckets.SetVersioning(ctx, req.principal, req.bucket, bucket.Versioning(cfg.Status)); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getBucketVersioning(ctx context.Context, req *request) error {
	state, err := h.cfg.Buckets.Versioning(ctx, req.principal, req.bucket)
	if err != nil {
		return err
	}
	// A bucket that never kept versions answers with the empty element.
	h.writeXML(ctx, req, http.StatusOK, versioningConfiguration{XMLNS: s3Namespace, Status: string(state)})
	return nil
}

// versionOf reads the versionId parameter of a request.
func versionOf(query url.Values) (*object.VersionRef, error) {
	if !query.Has("versionId") {
		return nil, nil
	}
	ref, err := object.ParseVersionRef(query.Get("versionId"))
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

// setVersionHeader names the version a response is about, when the version
// has a name worth telling: an ID of its own, or the client asked by name.
func setVersionHeader(header http.Header, v object.Version, asked bool) {
	if v.Versioned || asked {
		header.Set(headerVersionID, v.Ref().String())
	}
}

type listVersionsResult struct {
	XMLName             xml.Name `xml:"ListVersionsResult"`
	XMLNS               string   `xml:"xmlns,attr"`
	Name                string   `xml:"Name"`
	Prefix              string   `xml:"Prefix"`
	KeyMarker           string   `xml:"KeyMarker"`
	VersionIDMarker     string   `xml:"VersionIdMarker"`
	NextKeyMarker       string   `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string   `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int      `xml:"MaxKeys"`
	Delimiter           string   `xml:"Delimiter,omitempty"`
	IsTruncated         bool     `xml:"IsTruncated"`
	EncodingType        string   `xml:"EncodingType,omitempty"`
	// Entries are versions and delete markers in the order of the
	// listing; each says with its element name which it is.
	Entries        []versionEntry `xml:",any"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
}

type versionEntry struct {
	XMLName      xml.Name
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
	Size         *int64 `xml:"Size,omitempty"`
	StorageClass string `xml:"StorageClass,omitempty"`
	Owner        *owner `xml:"Owner,omitempty"`
}

func listVersionsParameters() []string {
	return []string{"versions", "prefix", "delimiter", "key-marker", "version-id-marker", "max-keys", "encoding-type"}
}

func (h *Handler) listObjectVersions(ctx context.Context, req *request, query url.Values) error {
	encoding := query.Get("encoding-type")
	if encoding != "" && encoding != encodingURL {
		return fmt.Errorf("%w: encoding-type", domain.ErrInvalidArgument)
	}
	encode := func(s string) string {
		if encoding == encodingURL {
			return encodeName(s)
		}
		return s
	}
	maxKeys := object.MaxListKeys
	if raw := query.Get("max-keys"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return fmt.Errorf("%w: max-keys", domain.ErrInvalidArgument)
		}
		maxKeys = min(n, object.MaxListKeys)
	}
	opts := app.VersionListOptions{
		Prefix: query.Get("prefix"), Delimiter: query.Get("delimiter"), KeyMarker: query.Get("key-marker"), MaxKeys: maxKeys,
	}
	if raw := query.Get("version-id-marker"); raw != "" {
		if opts.KeyMarker == "" {
			return fmt.Errorf("%w: version-id-marker without key-marker", domain.ErrInvalidArgument)
		}
		ref, err := object.ParseVersionRef(raw)
		if err != nil {
			return err
		}
		opts.VersionMarker = &ref
	}
	page, err := h.cfg.Objects.ListVersions(ctx, req.principal, req.bucket, opts)
	if err != nil {
		return err
	}
	result := listVersionsResult{
		XMLNS: s3Namespace, Name: req.bucket, Prefix: encode(opts.Prefix), KeyMarker: encode(opts.KeyMarker),
		VersionIDMarker: query.Get("version-id-marker"), MaxKeys: maxKeys, Delimiter: encode(opts.Delimiter),
		IsTruncated: page.Truncated, EncodingType: encoding, NextKeyMarker: encode(page.NextKeyMarker),
	}
	if page.NextVersionMarker != nil {
		result.NextVersionIDMarker = page.NextVersionMarker.String()
	}
	who := &owner{ID: req.principal.AccessKeyID, DisplayName: req.principal.AccessKeyID}
	for _, listed := range page.Versions {
		v := listed.Version
		entry := versionEntry{
			XMLName: xml.Name{Local: "Version"}, Key: encode(v.Key), VersionID: v.Ref().String(), IsLatest: listed.Latest,
			LastModified: xmlTime(v.ModifiedAt), Owner: who,
		}
		if v.DeleteMarker {
			entry.XMLName.Local = "DeleteMarker"
		} else {
			size := v.Size
			entry.ETag, entry.Size, entry.StorageClass = quote(v.ETag), &size, storageClass
		}
		result.Entries = append(result.Entries, entry)
	}
	for _, prefix := range page.CommonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix{Prefix: encode(prefix)})
	}
	h.writeXML(ctx, req, http.StatusOK, result)
	return nil
}

type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Quiet   bool     `xml:"Quiet"`
	Objects []struct {
		Key       string  `xml:"Key"`
		VersionID *string `xml:"VersionId"`
	} `xml:"Object"`
}

type deleteResult struct {
	XMLName xml.Name       `xml:"DeleteResult"`
	XMLNS   string         `xml:"xmlns,attr"`
	Deleted []deletedEntry `xml:"Deleted"`
	Errors  []deleteError  `xml:"Error"`
}

type deletedEntry struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

type deleteError struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

// deleteObjects deletes up to a thousand objects or versions named in the
// body. Each is authorized and committed on its own, exactly as a single
// delete is; what was refused is reported next to what was done.
func (h *Handler) deleteObjects(ctx context.Context, req *request) error {
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	if !body.bound(req.payloadHash) {
		return errUnsignedBody
	}
	var in deleteRequest
	if err := decodeShapedXML(body.body, maxDeleteXML, &in, deleteShape()); err != nil {
		return err
	}
	// Nothing is deleted on the strength of a body that is not the one
	// the client sent.
	if _, err := body.verify(); err != nil {
		return err
	}
	if len(in.Objects) == 0 || len(in.Objects) > maxDeleteObjects {
		return errMalformedXML
	}
	bypass, err := bypassOf(req.Header)
	if err != nil {
		return err
	}
	result := deleteResult{XMLNS: s3Namespace}
	for _, target := range in.Objects {
		entry := deletedEntry{Key: target.Key}
		input := app.DeleteInput{Bucket: req.bucket, Key: target.Key, BypassGovernance: bypass && target.VersionID != nil}
		var failure error
		if target.VersionID != nil {
			entry.VersionID = *target.VersionID
			ref, err := object.ParseVersionRef(*target.VersionID)
			if err != nil {
				failure = err
			}
			input.Version = &ref
		}
		var out app.DeleteOutput
		if failure == nil {
			out, failure = h.cfg.Objects.Delete(ctx, req.principal, input)
		}
		if failure != nil {
			if errors.Is(failure, domain.ErrBucketNotFound) || ctx.Err() != nil {
				// Nothing in the request can succeed.
				return failure
			}
			api := toAPIError(failure)
			result.Errors = append(result.Errors, deleteError{Key: target.Key, VersionID: entry.VersionID, Code: api.code, Message: api.message})
			continue
		}
		switch {
		case out.Marker != nil:
			entry.DeleteMarker, entry.DeleteMarkerVersionID = true, out.Marker.Ref().String()
		case out.Removed != nil && out.Removed.DeleteMarker:
			entry.DeleteMarker, entry.DeleteMarkerVersionID = true, out.Removed.Ref().String()
		}
		if !in.Quiet {
			result.Deleted = append(result.Deleted, entry)
		}
	}
	h.writeXML(ctx, req, http.StatusOK, result)
	return nil
}

func deleteShape() xmlShape {
	return xmlShape{"": {"Delete"}, "Delete": {"Quiet", "Object*"}, "Object": {"Key", "VersionId", "ETag", "LastModifiedTime", "Size"}}
}
