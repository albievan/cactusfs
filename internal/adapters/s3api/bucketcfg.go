package s3api

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Bucket policy, cross-origin rules and tags: the sub-resources ?policy,
// ?cors and ?tagging of a bucket, and ?tagging of an object.

const headerTagging = "x-amz-tagging"

// maxConfigBody bounds the body of a request that sets one of them.
const maxConfigBody = bucket.MaxConfigBytes

func errNoSuchConfig(code, what string) apiError {
	return apiError{http.StatusNotFound, code, "The " + what + " does not exist."}
}

// configBody reads the body of a request that replaces a configuration. It
// is acted on only when the signature covers it.
func (h *Handler) configBody(req *request) ([]byte, error) {
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return nil, err
	}
	if !body.bound(req.payloadHash) {
		return nil, errUnsignedBody
	}
	raw, err := io.ReadAll(io.LimitReader(body.body, maxConfigBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxConfigBody {
		return nil, fmt.Errorf("%w: configuration larger than %d bytes", domain.ErrEntityTooLarge, maxConfigBody)
	}
	if _, err := body.verify(); err != nil {
		return nil, err
	}
	return raw, nil
}

func (h *Handler) putBucketPolicy(ctx context.Context, req *request) error {
	raw, err := h.configBody(req)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return fmt.Errorf("%w: policy: empty", domain.ErrInvalidArgument)
	}
	if err := h.cfg.Buckets.SetPolicy(ctx, req.principal, req.bucket, raw); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) getBucketPolicy(ctx context.Context, req *request) error {
	raw, err := h.cfg.Buckets.Policy(ctx, req.principal, req.bucket)
	if err != nil {
		return err
	}
	if raw == nil {
		return errNoSuchConfig("NoSuchBucketPolicy", "bucket policy")
	}
	req.w.Header().Set("Content-Type", "application/json")
	req.w.WriteHeader(http.StatusOK)
	_, _ = req.w.Write(raw)
	return nil
}

func (h *Handler) deleteBucketPolicy(ctx context.Context, req *request) error {
	if err := h.cfg.Buckets.SetPolicy(ctx, req.principal, req.bucket, nil); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

type corsConfiguration struct {
	XMLName xml.Name      `xml:"CORSConfiguration"`
	XMLNS   string        `xml:"xmlns,attr,omitempty"`
	Rules   []corsRuleXML `xml:"CORSRule"`
}

type corsRuleXML struct {
	ID             string   `xml:"ID,omitempty"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedHeaders []string `xml:"AllowedHeader"`
	ExposeHeaders  []string `xml:"ExposeHeader"`
	MaxAgeSeconds  *int     `xml:"MaxAgeSeconds,omitempty"`
}

func (h *Handler) putBucketCORS(ctx context.Context, req *request) error {
	raw, err := h.configBody(req)
	if err != nil {
		return err
	}
	var in corsConfiguration
	err = decodeStrictXML(strings.NewReader(string(raw)), maxConfigBody, &in,
		"CORSConfiguration", "CORSRule*", "ID", "AllowedOrigin*", "AllowedMethod*", "AllowedHeader*", "ExposeHeader*", "MaxAgeSeconds")
	if err != nil {
		return err
	}
	rules := bucket.CORS{}
	for _, r := range in.Rules {
		rule := bucket.CORSRule{
			ID: r.ID, AllowedOrigins: r.AllowedOrigins, AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders,
		}
		if r.MaxAgeSeconds != nil {
			rule.MaxAgeSeconds = *r.MaxAgeSeconds
		}
		rules.Rules = append(rules.Rules, rule)
	}
	if err := h.cfg.Buckets.SetCORS(ctx, req.principal, req.bucket, &rules); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getBucketCORS(ctx context.Context, req *request) error {
	rules, err := h.cfg.Buckets.CORS(ctx, req.principal, req.bucket)
	if err != nil {
		return err
	}
	if rules == nil {
		return errNoSuchConfig("NoSuchCORSConfiguration", "CORS configuration")
	}
	out := corsConfiguration{XMLNS: s3Namespace}
	for _, r := range rules.Rules {
		rule := corsRuleXML{
			ID: r.ID, AllowedOrigins: r.AllowedOrigins, AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders,
		}
		if r.MaxAgeSeconds > 0 {
			age := r.MaxAgeSeconds
			rule.MaxAgeSeconds = &age
		}
		out.Rules = append(out.Rules, rule)
	}
	h.writeXML(ctx, req, http.StatusOK, out)
	return nil
}

func (h *Handler) deleteBucketCORS(ctx context.Context, req *request) error {
	if err := h.cfg.Buckets.SetCORS(ctx, req.principal, req.bucket, nil); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

type tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Tags    []tagXML `xml:"TagSet>Tag"`
}

type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// readTags reads a Tagging document. A key given twice is refused: which
// value it has must not be ours to choose.
func (h *Handler) readTags(req *request) (map[string]string, error) {
	raw, err := h.configBody(req)
	if err != nil {
		return nil, err
	}
	var in tagging
	if err := decodeStrictXML(strings.NewReader(string(raw)), maxConfigBody, &in, "Tagging", "TagSet", "Tag*", "Key", "Value"); err != nil {
		return nil, err
	}
	tags := map[string]string{}
	for _, tag := range in.Tags {
		if _, twice := tags[tag.Key]; twice {
			return nil, fmt.Errorf("%w: tag %q is given twice", domain.ErrInvalidArgument, tag.Key)
		}
		tags[tag.Key] = tag.Value
	}
	return tags, nil
}

func tagsXML(tags map[string]string) tagging {
	out := tagging{XMLNS: s3Namespace}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		out.Tags = append(out.Tags, tagXML{Key: key, Value: tags[key]})
	}
	return out
}

func (h *Handler) putBucketTagging(ctx context.Context, req *request) error {
	tags, err := h.readTags(req)
	if err != nil {
		return err
	}
	if err := h.cfg.Buckets.SetTags(ctx, req.principal, req.bucket, tags); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) getBucketTagging(ctx context.Context, req *request) error {
	tags, err := h.cfg.Buckets.Tags(ctx, req.principal, req.bucket)
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		return errNoSuchConfig("NoSuchTagSet", "TagSet")
	}
	h.writeXML(ctx, req, http.StatusOK, tagsXML(tags))
	return nil
}

func (h *Handler) deleteBucketTagging(ctx context.Context, req *request) error {
	if err := h.cfg.Buckets.SetTags(ctx, req.principal, req.bucket, nil); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) putObjectTagging(ctx context.Context, req *request) error {
	tags, err := h.readTags(req)
	if err != nil {
		return err
	}
	version, err := h.cfg.Objects.SetTags(ctx, req.principal, h.lockInput(req), tags)
	if err != nil {
		return err
	}
	setVersionHeader(req.w.Header(), version, req.version != nil)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getObjectTagging(ctx context.Context, req *request) error {
	version, err := h.cfg.Objects.Tags(ctx, req.principal, h.lockInput(req))
	if err != nil {
		return err
	}
	setVersionHeader(req.w.Header(), version, req.version != nil)
	h.writeXML(ctx, req, http.StatusOK, tagsXML(version.Tags))
	return nil
}

func (h *Handler) deleteObjectTagging(ctx context.Context, req *request) error {
	version, err := h.cfg.Objects.SetTags(ctx, req.principal, h.lockInput(req), nil)
	if err != nil {
		return err
	}
	setVersionHeader(req.w.Header(), version, req.version != nil)
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

// tagsOf reads the x-amz-tagging header of a request that creates an
// object: tags as a URL query string.
func tagsOf(header http.Header) (map[string]string, error) {
	values := header.Values(headerTagging)
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("%w: %s is given more than once", domain.ErrInvalidArgument, headerTagging)
	}
	parsed, err := url.ParseQuery(values[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not a query string", domain.ErrInvalidArgument, headerTagging)
	}
	tags := map[string]string{}
	for key, list := range parsed {
		if len(list) != 1 {
			return nil, fmt.Errorf("%w: tag %q is given twice", domain.ErrInvalidArgument, key)
		}
		tags[key] = list[0]
	}
	if err := object.ValidateTags(tags, object.MaxObjectTags); err != nil {
		return nil, err
	}
	return tags, nil
}

// routeBucketConfig routes the configuration sub-resources of a bucket.
// handled is false for a request that names none of them.
func (h *Handler) routeBucketConfig(ctx context.Context, req *request, query url.Values) (handled bool, err error) {
	type operations struct {
		name             string
		get, put, remove func(context.Context, *request) error
	}
	var ops operations
	switch {
	case query.Has("policy"):
		ops = operations{"policy", h.getBucketPolicy, h.putBucketPolicy, h.deleteBucketPolicy}
	case query.Has("cors"):
		ops = operations{"cors", h.getBucketCORS, h.putBucketCORS, h.deleteBucketCORS}
	case query.Has("tagging"):
		ops = operations{"tagging", h.getBucketTagging, h.putBucketTagging, h.deleteBucketTagging}
	default:
		return false, nil
	}
	if err := allowQuery(query, ops.name); err != nil {
		return true, err
	}
	title := map[string]string{"policy": "BucketPolicy", "cors": "BucketCors", "tagging": "BucketTagging"}[ops.name]
	switch req.Method {
	case http.MethodGet:
		req.operation = "Get" + title
		return true, ops.get(ctx, req)
	case http.MethodPut:
		req.operation = "Put" + title
		return true, ops.put(ctx, req)
	case http.MethodDelete:
		req.operation = "Delete" + title
		return true, ops.remove(ctx, req)
	default:
		return true, errMethodNotAllowed()
	}
}

// Cross-origin requests.

const (
	headerOrigin         = "Origin"
	headerRequestMethod  = "Access-Control-Request-Method"
	headerRequestHeaders = "Access-Control-Request-Headers"
)

// errCORSForbidden is the one answer to a preflight that is not allowed,
// whatever the reason: no such bucket, no rules, no rule that matches.
func errCORSForbidden() apiError {
	return apiError{http.StatusForbidden, "AccessForbidden", "CORSResponse: This CORS request is not allowed."}
}

// preflight answers a browser's question whether a cross-origin request may
// be made. It is asked without credentials, and answered from the rules of
// the bucket alone.
func (h *Handler) preflight(ctx context.Context, req *request) error {
	req.operation = "Preflight"
	origin, method := req.Header.Get(headerOrigin), req.Header.Get(headerRequestMethod)
	if req.bucket == "" || origin == "" || method == "" {
		return errCORSForbidden()
	}
	var asked []string
	for _, name := range strings.Split(req.Header.Get(headerRequestHeaders), ",") {
		if name = strings.TrimSpace(name); name != "" {
			// What is echoed in the answer is a header name, or nothing.
			if !bucket.HeaderName(name) || strings.Contains(name, "*") {
				return errCORSForbidden()
			}
			asked = append(asked, name)
		}
	}
	// The answer depends on what was asked, for any cache in between.
	req.w.Header().Add("Vary", headerOrigin+", "+headerRequestMethod+", "+headerRequestHeaders)
	rules := h.cfg.Buckets.CORSOf(ctx, req.bucket)
	if rules == nil {
		return errCORSForbidden()
	}
	rule, ok := rules.Match(origin, method, asked)
	if !ok {
		return errCORSForbidden()
	}
	header := req.w.Header()
	setCORSHeaders(header, rule, origin)
	header.Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
	if len(asked) > 0 {
		header.Set("Access-Control-Allow-Headers", strings.Join(asked, ", "))
	}
	if rule.MaxAgeSeconds > 0 {
		header.Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAgeSeconds))
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func setCORSHeaders(header http.Header, rule bucket.CORSRule, origin string) {
	if rule.AllowsAnyOrigin() {
		header.Set("Access-Control-Allow-Origin", "*")
	} else {
		header.Set("Access-Control-Allow-Origin", origin)
	}
	if len(rule.ExposeHeaders) > 0 {
		header.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
}

// crossOrigin adds, to the answer to a request a browser makes for a script
// of another origin, the headers that let the script see it - when a rule of
// the bucket allows that origin and method.
func (h *Handler) crossOrigin(ctx context.Context, req *request) {
	origin := req.Header.Get(headerOrigin)
	if origin == "" || req.bucket == "" {
		return
	}
	rules := h.cfg.Buckets.CORSOf(ctx, req.bucket)
	if rules == nil {
		return
	}
	// With rules, the answer depends on the origin whether or not this
	// one is allowed: a cache must not give one origin another's answer.
	req.w.Header().Add("Vary", headerOrigin)
	if rule, ok := rules.Match(origin, req.Method, nil); ok {
		setCORSHeaders(req.w.Header(), rule, origin)
	}
}

// anonymous is the principal of a request that carries no credentials. What
// it may do is decided where everything is decided: by the authorizer, which
// lets it read what the policy of a bucket opens to everyone and nothing else.
func anonymous(req *request) iam.Principal {
	return iam.Principal{Session: sessionOf(req)}
}
