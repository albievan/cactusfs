package s3api

import (
	"encoding/xml"
	"fmt"
	"io"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

const (
	s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"
	// maxRequestXML bounds an XML request body.
	maxRequestXML = 64 << 10
	// xmlTimeFormat is the timestamp format of S3 XML responses.
	xmlTimeFormat = "2006-01-02T15:04:05.000Z"
	storageClass  = "STANDARD"
)

type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type listAllMyBucketsResult struct {
	XMLName xml.Name      `xml:"ListAllMyBucketsResult"`
	XMLNS   string        `xml:"xmlns,attr"`
	Owner   owner         `xml:"Owner"`
	Buckets []bucketEntry `xml:"Buckets>Bucket"`
}

type bucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	XMLNS   string   `xml:"xmlns,attr"`
	Region  string   `xml:",chardata"`
}

type listBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	XMLNS                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	KeyCount              int            `xml:"KeyCount"`
	IsTruncated           bool           `xml:"IsTruncated"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	Contents              []objectEntry  `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type objectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *owner `xml:"Owner,omitempty"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

func xmlTime(t time.Time) string {
	return t.UTC().Format(xmlTimeFormat)
}

// createBucketConfiguration is the optional body of CreateBucket.
type createBucketConfiguration struct {
	XMLName            xml.Name `xml:"CreateBucketConfiguration"`
	LocationConstraint string   `xml:"LocationConstraint"`
}

// errMalformedXML reports a request body that is not the XML the operation
// expects.
var errMalformedXML = apiError{400, "MalformedXML", "The XML that you provided was not well formed or did not validate against our published schema."}

// parseCreateBucketConfiguration reads the optional body of CreateBucket. An
// empty body is valid. The body is bounded: an XML document that is too large
// is malformed as far as this API is concerned.
func parseCreateBucketConfiguration(body io.Reader) (createBucketConfiguration, error) {
	var cfg createBucketConfiguration
	data, err := io.ReadAll(io.LimitReader(body, maxRequestXML+1))
	if err != nil {
		return cfg, fmt.Errorf("%w: reading the request body", domain.ErrIncompleteBody)
	}
	if len(data) > maxRequestXML {
		return cfg, errMalformedXML
	}
	if len(data) == 0 {
		return cfg, nil
	}
	if err := xml.Unmarshal(data, &cfg); err != nil {
		return cfg, errMalformedXML
	}
	return cfg, nil
}
