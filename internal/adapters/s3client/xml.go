package s3client

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// The bounds of what is read of an answer. The egress client bounds every
// body to 64 KiB already; these are tighter where an answer is small.
const (
	// maxErrorBody bounds what is read of the body of a refusal.
	maxErrorBody = 16 << 10
	// maxDocument bounds a document of an answer that is not a listing.
	maxDocument = 16 << 10
	// maxListing bounds one page of a listing of uploads: the egress
	// client's own bound, which a page of maxUploadsPage stays below.
	maxListing = 64 << 10
	// maxCode bounds an error code.
	maxCode = 64
	// maxUploadID bounds the ID of an upload the other deployment gives.
	maxUploadID = 1024
)

// errDocument is an answer whose body is not the document it should be.
var errDocument = errors.New("s3client: the answer is not the document expected")

// readBounded reads r to its end, or to limit bytes and one more - which is
// too much.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("s3client: reading the answer: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: longer than %d bytes", errDocument, limit)
	}
	return data, nil
}

// decodeXML decodes data into v, whose XMLName names the root element:
// a document of another root is errDocument. encoding/xml expands no
// entities but its five and reads no external DTD, so nothing outside the
// body is ever read.
func decodeXML(data []byte, v any) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	if decoder.Decode(v) != nil {
		// The decoder's error can quote the document, and the document a
		// key: it is not kept.
		return fmt.Errorf("%w: it does not decode", errDocument)
	}
	return nil
}

// errorDocument is an S3 error response.
type errorDocument struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
}

// parseErrorCode returns the code of an S3 error document, or "" when data
// is none or its code is not a word: letters and digits, at most maxCode of
// them. Nothing else of the document - its message, its resource, which may
// name a key - is kept.
func parseErrorCode(data []byte) string {
	var doc errorDocument
	if decodeXML(data, &doc) != nil || !codeWord(doc.Code) {
		return ""
	}
	return doc.Code
}

// codeWord reports whether s can be an S3 error code.
func codeWord(s string) bool {
	if s == "" || len(s) > maxCode {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// versioningDocument is the answer of GetBucketVersioning.
type versioningDocument struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Status  string   `xml:"Status"`
}

// parseVersioning returns the versioning state an answer of
// GetBucketVersioning gives. A state that is none of the three is
// errDocument.
func parseVersioning(data []byte) (bucket.Versioning, error) {
	var doc versioningDocument
	if err := decodeXML(data, &doc); err != nil {
		return "", err
	}
	switch state := bucket.Versioning(doc.Status); state {
	case bucket.Unversioned, bucket.VersioningEnabled, bucket.VersioningSuspended:
		return state, nil
	}
	return "", fmt.Errorf("%w: a versioning state that is none", errDocument)
}

// initiateDocument is the answer of CreateMultipartUpload.
type initiateDocument struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	UploadID string   `xml:"UploadId"`
}

// parseInitiate returns the ID of the upload an answer of
// CreateMultipartUpload gives. An ID that is not one a query can carry
// (uploadID) is errDocument.
func parseInitiate(data []byte) (string, error) {
	var doc initiateDocument
	if err := decodeXML(data, &doc); err != nil {
		return "", err
	}
	if !validUploadID(doc.UploadID) {
		return "", fmt.Errorf("%w: no upload ID", errDocument)
	}
	return doc.UploadID, nil
}

// validUploadID reports whether s can be the ID of an upload: 1 to maxUploadID
// printable ASCII characters without a space.
func validUploadID(s string) bool {
	return s != "" && len(s) <= maxUploadID && printableWord(s)
}

// printableWord reports whether s is printable ASCII without a space.
func printableWord(s string) bool {
	for i := range len(s) {
		if c := s[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// listingDocument is the answer of ListMultipartUploads, asked with
// encoding-type=url.
type listingDocument struct {
	XMLName            xml.Name `xml:"ListMultipartUploadsResult"`
	EncodingType       string   `xml:"EncodingType"`
	IsTruncated        bool     `xml:"IsTruncated"`
	NextKeyMarker      string   `xml:"NextKeyMarker"`
	NextUploadIDMarker string   `xml:"NextUploadIdMarker"`
	Uploads            []struct {
		Key      string `xml:"Key"`
		UploadID string `xml:"UploadId"`
	} `xml:"Upload"`
}

// listing is one page of a listing of uploads, its keys decoded.
type listing struct {
	// uploads are the IDs of the uploads of the key asked for.
	uploads []string
	// truncated says that there is a page after this one, which begins
	// after nextKey and nextUpload.
	truncated  bool
	nextKey    string
	nextUpload string
}

// parseListing reads a page of ListMultipartUploads and keeps the uploads
// of key. Keys and the key marker are decoded when the answer says they are
// encoded as asked; an upload whose key does not decode, or whose ID is
// none, is errDocument.
func parseListing(data []byte, key string) (listing, error) {
	var doc listingDocument
	if err := decodeXML(data, &doc); err != nil {
		return listing{}, err
	}
	decode := func(s string) (string, error) { return s, nil }
	switch doc.EncodingType {
	case "url":
		decode = url.QueryUnescape
	case "":
	default:
		return listing{}, fmt.Errorf("%w: an encoding that was not asked for", errDocument)
	}
	out := listing{truncated: doc.IsTruncated}
	for _, upload := range doc.Uploads {
		name, err := decode(upload.Key)
		if err != nil || !validUploadID(upload.UploadID) {
			return listing{}, fmt.Errorf("%w: an upload without a key or an ID", errDocument)
		}
		if name == key {
			out.uploads = append(out.uploads, upload.UploadID)
		}
	}
	if out.truncated {
		next, err := decode(doc.NextKeyMarker)
		if err != nil || next == "" || (doc.NextUploadIDMarker != "" && !validUploadID(doc.NextUploadIDMarker)) {
			return listing{}, fmt.Errorf("%w: a truncated listing without where it goes on", errDocument)
		}
		out.nextKey, out.nextUpload = next, doc.NextUploadIDMarker
	}
	return out, nil
}

// partsDocument is the answer of ListParts.
type partsDocument struct {
	XMLName              xml.Name `xml:"ListPartsResult"`
	UploadID             string   `xml:"UploadId"`
	IsTruncated          bool     `xml:"IsTruncated"`
	NextPartNumberMarker int      `xml:"NextPartNumberMarker"`
	Parts                []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
		Size       int64  `xml:"Size"`
	} `xml:"Part"`
}

// partsPage is one page of a listing of parts.
type partsPage struct {
	parts     []ports.SitePart
	truncated bool
	// next is the number the next page begins after.
	next int
}

// parseParts reads a page of ListParts of the upload uploadID. An answer of
// another upload, a part whose number is out of range, whose entity tag is
// none or whose size is below nothing, is errDocument.
func parseParts(data []byte, uploadID string) (partsPage, error) {
	var doc partsDocument
	if err := decodeXML(data, &doc); err != nil {
		return partsPage{}, err
	}
	if doc.UploadID != uploadID {
		return partsPage{}, fmt.Errorf("%w: the parts of another upload", errDocument)
	}
	out := partsPage{truncated: doc.IsTruncated, next: doc.NextPartNumberMarker}
	for _, part := range doc.Parts {
		etag := strings.Trim(part.ETag, `"`)
		if object.ValidatePartNumber(part.PartNumber) != nil || etag == "" || !printableWord(etag) || part.Size < 0 || part.Size > object.MaxSize {
			return partsPage{}, fmt.Errorf("%w: a part without a number, an entity tag or a size", errDocument)
		}
		out.parts = append(out.parts, ports.SitePart{Number: part.PartNumber, ETag: etag, Size: part.Size})
	}
	return out, nil
}

// completeResult is the answer of a CompleteMultipartUpload that completed.
type completeResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
}

// completeAnswer reads the body of a 200 to CompleteMultipartUpload, which
// S3 sends before the upload is complete and ends with an error document
// instead of the result when it fails: completed is set for a
// CompleteMultipartUploadResult, code is the code of an error document, and
// a body that is neither is neither.
func completeAnswer(data []byte) (completed bool, code string) {
	var result completeResult
	if decodeXML(data, &result) == nil {
		return true, ""
	}
	return false, parseErrorCode(data)
}

// completeDocument is the body of CompleteMultipartUpload.
type completeDocument struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []completePart `xml:"Part"`
}

// completePart is one part of completeDocument.
type completePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// tagDocument is the body of PutObjectTagging.
type tagDocument struct {
	XMLName xml.Name `xml:"Tagging"`
	Tags    []tagXML `xml:"TagSet>Tag"`
}

// tagXML is one tag of tagDocument.
type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// retentionDocument is the body of PutObjectRetention; an empty one
// removes the retention.
type retentionDocument struct {
	XMLName         xml.Name `xml:"Retention"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

// legalHoldDocument is the body of PutObjectLegalHold.
type legalHoldDocument struct {
	XMLName xml.Name `xml:"LegalHold"`
	Status  string   `xml:"Status"`
}

// marshalXML returns the document v with the XML declaration before it.
func marshalXML(v any) ([]byte, error) {
	data, err := xml.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("s3client: a document that does not marshal: %w", err)
	}
	return append([]byte(xml.Header), data...), nil
}
