package s3api

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Server-side encryption as S3 clients ask for it. Every object is encrypted
// whatever they say; these headers say whose key it takes to read it.
const (
	headerSSE          = "x-amz-server-side-encryption"
	headerSSEKMSKey    = "x-amz-server-side-encryption-aws-kms-key-id"
	headerSSEContext   = "x-amz-server-side-encryption-context"
	headerSSEBucketKey = "x-amz-server-side-encryption-bucket-key-enabled"
	headerSSECAlgo     = "x-amz-server-side-encryption-customer-algorithm"
	headerSSECKey      = "x-amz-server-side-encryption-customer-key"
	headerSSECKeyMD5   = "x-amz-server-side-encryption-customer-key-md5"

	sseAES256 = "AES256"
	// maxSSEContextHeader bounds the encryption context header, which is
	// JSON in base64.
	maxSSEContextHeader = 4096
)

// managedSSEHeaders say which key the server is to manage for a new object.
func managedSSEHeaders() []string {
	return []string{headerSSE, headerSSEKMSKey, headerSSEContext, headerSSEBucketKey}
}

// customerSSEHeaders carry a key the client holds.
func customerSSEHeaders() []string {
	return []string{headerSSECAlgo, headerSSECKey, headerSSECKeyMD5}
}

// allowSSEHeaders refuses the encryption headers on a request that does not
// read them. Which key an object gets is said when it, or its upload, is
// created; a customer key comes with every request that writes or reads the
// object's bytes. Anywhere else a client that sends them expects something
// that would not happen.
func allowSSEHeaders(req *request, query url.Values) error {
	// What the query selects besides the object itself.
	selects := 0
	for name := range query {
		if name != queryClientInfo && name != "versionId" {
			selects++
		}
	}
	onObject := req.bucket != "" && req.key != ""
	creates := onObject && ((req.Method == http.MethodPut && selects == 0 && !query.Has("versionId")) || (req.Method == http.MethodPost && query.Has("uploads")))
	// A part needs the key; completing an upload and listing its parts
	// may bring it, as S3 clients do.
	upload := onObject && query.Has("uploadId") && req.Method != http.MethodDelete
	reads := onObject && (req.Method == http.MethodGet || req.Method == http.MethodHead) && selects == 0
	for _, name := range managedSSEHeaders() {
		if err := oneValue(req.Header, name, creates); err != nil {
			return err
		}
	}
	for _, name := range customerSSEHeaders() {
		if err := oneValue(req.Header, name, creates || upload || reads); err != nil {
			return err
		}
	}
	return nil
}

// oneValue refuses an encryption header that the operation does not read,
// that is given more than once, or that is given empty. Whether a header is
// there is asked of its values, not of the first of them: a header sent
// twice, once empty, is there.
func oneValue(header http.Header, name string, read bool) error {
	values := header.Values(name)
	switch {
	case len(values) == 0:
		return nil
	case !read:
		return fmt.Errorf("%w: %s is not part of this operation", domain.ErrInvalidArgument, name)
	case len(values) != 1 || values[0] == "":
		return fmt.Errorf("%w: %s must be given once, with a value", domain.ErrInvalidArgument, name)
	}
	return nil
}

// customerKeyOf reads the customer key of a request, if it has one, and the
// digest that came with it. A key is only taken from a request that arrived
// over TLS: anywhere else it has already been disclosed.
func customerKeyOf(r *http.Request) (key []byte, digest string, err error) {
	present := 0
	for _, name := range customerSSEHeaders() {
		switch values := r.Header.Values(name); {
		case len(values) == 1 && values[0] != "":
			present++
		case len(values) != 0:
			return nil, "", fmt.Errorf("%w: %s must be given once, with a value", domain.ErrInvalidArgument, name)
		}
	}
	if present == 0 {
		return nil, "", nil
	}
	if present != len(customerSSEHeaders()) {
		return nil, "", fmt.Errorf("%w: a customer key needs each of its three headers", domain.ErrInvalidArgument)
	}
	algorithm, rawKey, rawDigest := r.Header.Get(headerSSECAlgo), r.Header.Get(headerSSECKey), r.Header.Get(headerSSECKeyMD5)
	if r.TLS == nil {
		return nil, "", errCustomerKeyInTheClear()
	}
	if algorithm != sseAES256 {
		return nil, "", errEncryptionAlgorithm()
	}
	key, err = base64.StdEncoding.DecodeString(rawKey)
	if err != nil || len(key) != object.KeySize {
		clear(key)
		return nil, "", fmt.Errorf("%w: a customer key is 256 bits in base64", domain.ErrInvalidArgument)
	}
	want, err := base64.StdEncoding.DecodeString(rawDigest)
	sum := newETagHash()
	sum.Write(key)
	if err != nil || subtle.ConstantTimeCompare(want, sum.Sum(nil)) != 1 {
		clear(key)
		return nil, "", fmt.Errorf("%w: the digest of the customer key does not match the key", domain.ErrInvalidArgument)
	}
	return key, rawDigest, nil
}

// sseOf reads what a request that creates an object, or begins an upload,
// says about its encryption.
func sseOf(r *http.Request) (object.SSE, error) {
	key, _, err := customerKeyOf(r)
	if err != nil {
		return object.SSE{}, err
	}
	managed := false
	for _, name := range managedSSEHeaders() {
		values := r.Header.Values(name)
		if len(values) > 1 || (len(values) == 1 && values[0] == "") {
			clear(key)
			return object.SSE{}, fmt.Errorf("%w: %s must be given once, with a value", domain.ErrInvalidArgument, name)
		}
		managed = managed || len(values) == 1
	}
	if key != nil {
		if managed {
			clear(key)
			return object.SSE{}, fmt.Errorf("%w: a customer key and server-managed encryption exclude each other", domain.ErrInvalidArgument)
		}
		return object.SSE{Mode: object.SSEC, CustomerKey: key}, nil
	}
	if raw := r.Header.Get(headerSSEBucketKey); raw != "" && !strings.EqualFold(raw, "false") {
		// Bucket keys are an economy of AWS KMS that is not implemented.
		return object.SSE{}, errNotImplemented()
	}
	sse := object.SSE{KMSKeyID: r.Header.Get(headerSSEKMSKey)}
	switch mode := r.Header.Get(headerSSE); mode {
	case "", sseAES256:
		if sse.KMSKeyID != "" || r.Header.Get(headerSSEContext) != "" {
			return object.SSE{}, fmt.Errorf("%w: a KMS key or an encryption context needs %s: aws:kms", domain.ErrInvalidArgument, headerSSE)
		}
		return object.SSE{}, nil
	case object.SSEKMS:
		sse.Mode = object.SSEKMS
	case "aws:kms:dsse":
		return object.SSE{}, errNotImplemented()
	default:
		return object.SSE{}, fmt.Errorf("%w: server-side encryption %q", domain.ErrInvalidArgument, mode)
	}
	if raw := r.Header.Get(headerSSEContext); raw != "" {
		if len(raw) > maxSSEContextHeader {
			return object.SSE{}, fmt.Errorf("%w: the encryption context is too large", domain.ErrInvalidArgument)
		}
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return object.SSE{}, fmt.Errorf("%w: the encryption context is JSON in base64", domain.ErrInvalidArgument)
		}
		if sse.Context, err = parseContext(decoded); err != nil {
			return object.SSE{}, err
		}
	}
	return sse, nil
}

// parseContext reads an encryption context: one JSON object of strings, no
// key twice, and nothing after it.
func parseContext(raw []byte) (map[string]string, error) {
	invalid := fmt.Errorf("%w: the encryption context is one JSON object of strings", domain.ErrInvalidArgument)
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return nil, invalid
	}
	context := map[string]string{}
	for decoder.More() {
		name, err := decoder.Token()
		key, isString := name.(string)
		if err != nil || !isString {
			return nil, invalid
		}
		value, err := decoder.Token()
		text, isString := value.(string)
		if _, twice := context[key]; err != nil || !isString || twice {
			return nil, invalid
		}
		context[key] = text
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, invalid
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid
	}
	return context, nil
}

// setSSEHeaders says in a response how what it describes is encrypted. The
// customer key is never echoed. digest is the digest of it that the request
// itself carried, and is echoed when there was one: none is stored.
func setSSEHeaders(header http.Header, enc object.Encryption, digest string) {
	switch enc.Mode {
	case object.SSEC:
		header.Set(headerSSECAlgo, sseAES256)
		if digest != "" {
			header.Set(headerSSECKeyMD5, digest)
		}
	case object.SSEKMS:
		header.Set(headerSSE, object.SSEKMS)
		header.Set(headerSSEKMSKey, enc.KMSKeyID)
	default:
		header.Set(headerSSE, sseAES256)
	}
}

// isSSEHeader reports whether a lower-case header name is one of the
// encryption headers.
func isSSEHeader(lower string) bool {
	return slices.Contains(managedSSEHeaders(), lower) || slices.Contains(customerSSEHeaders(), lower)
}
