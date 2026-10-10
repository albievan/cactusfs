package s3api

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Object Lock, implemented from the public Amazon S3 API reference
// (PutObjectLockConfiguration, PutObjectRetention, PutObjectLegalHold and the
// x-amz-object-lock-* headers). This file moves requests to the use cases
// and back; whether a version may be removed or its retention changed is
// decided by the rules in the domain, when the change is committed.

const (
	headerLockEnabled   = "X-Amz-Bucket-Object-Lock-Enabled"
	headerLockMode      = "X-Amz-Object-Lock-Mode"
	headerLockUntil     = "X-Amz-Object-Lock-Retain-Until-Date"
	headerLockLegalHold = "X-Amz-Object-Lock-Legal-Hold"
	headerLockOverride  = "X-Amz-Bypass-Governance-Retention"
	legalHoldOn         = "ON"
	legalHoldOff        = "OFF"
)

// lockHeaders are the request headers of Object Lock, in lower case.
func lockHeaders() []string {
	return []string{
		"x-amz-bucket-object-lock-enabled", "x-amz-object-lock-mode", "x-amz-object-lock-retain-until-date",
		"x-amz-object-lock-legal-hold", "x-amz-bypass-governance-retention",
	}
}

// Errors of this API that the use cases report with one error and S3 names
// differently by operation.
var (
	errNoLockConfiguration = apiError{http.StatusNotFound, "ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket."}
	errNoObjectLock        = apiError{http.StatusNotFound, "NoSuchObjectLockConfiguration", "The specified object does not have an ObjectLock configuration."}
)

// lockOf reads the lock a put asks for from its headers. A header that is
// there and cannot be understood is an error: a client that asked for a lock
// and did not get one must be told.
func lockOf(header http.Header) (object.Lock, error) {
	var lock object.Lock
	mode, until := header.Get(headerLockMode), header.Get(headerLockUntil)
	if (mode == "") != (until == "") {
		return lock, fmt.Errorf("%w: %s and %s come together", domain.ErrInvalidArgument, headerLockMode, headerLockUntil)
	}
	if mode != "" {
		at, err := time.Parse(time.RFC3339Nano, until)
		if err != nil {
			return lock, fmt.Errorf("%w: %s is not a date", domain.ErrInvalidArgument, headerLockUntil)
		}
		if mode != string(object.Governance) && mode != string(object.Compliance) {
			return lock, fmt.Errorf("%w: %s", domain.ErrInvalidArgument, headerLockMode)
		}
		lock.Retention = &object.Retention{Mode: object.RetentionMode(mode), Until: at.UTC()}
	}
	switch header.Get(headerLockLegalHold) {
	case "", legalHoldOff:
	case legalHoldOn:
		lock.LegalHold = true
	default:
		return lock, fmt.Errorf("%w: %s", domain.ErrInvalidArgument, headerLockLegalHold)
	}
	return lock, nil
}

// bypassOf reads whether the caller asks to lift a governance retention.
func bypassOf(header http.Header) (bool, error) {
	switch header.Get(headerLockOverride) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("%w: %s", domain.ErrInvalidArgument, headerLockOverride)
	}
}

// setLockHeaders tells what keeps a version.
func setLockHeaders(header http.Header, lock object.Lock) {
	if r := lock.Retention; r != nil {
		header.Set(headerLockMode, string(r.Mode))
		header.Set(headerLockUntil, r.Until.UTC().Format(xmlTimeFormat))
	}
	if lock.LegalHold {
		header.Set(headerLockLegalHold, legalHoldOn)
	}
}

type objectLockConfiguration struct {
	XMLName           xml.Name `xml:"ObjectLockConfiguration"`
	XMLNS             string   `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string   `xml:"ObjectLockEnabled,omitempty"`
	Rule              *struct {
		DefaultRetention *struct {
			Mode  string `xml:"Mode"`
			Days  *int   `xml:"Days"`
			Years *int   `xml:"Years"`
		} `xml:"DefaultRetention"`
	} `xml:"Rule"`
}

func (h *Handler) getObjectLockConfiguration(ctx context.Context, req *request) error {
	b, err := h.cfg.Buckets.ObjectLock(ctx, req.principal, req.bucket)
	if errors.Is(err, domain.ErrLockNotConfigured) {
		return errNoLockConfiguration
	}
	if err != nil {
		return err
	}
	out := objectLockConfiguration{XMLNS: s3Namespace, ObjectLockEnabled: "Enabled"}
	if d := b.DefaultRetention; d != nil {
		rule := &struct {
			Mode  string `xml:"Mode"`
			Days  *int   `xml:"Days"`
			Years *int   `xml:"Years"`
		}{Mode: string(d.Mode)}
		if d.Days > 0 {
			rule.Days = &d.Days
		} else {
			rule.Years = &d.Years
		}
		out.Rule = &struct {
			DefaultRetention *struct {
				Mode  string `xml:"Mode"`
				Days  *int   `xml:"Days"`
				Years *int   `xml:"Years"`
			} `xml:"DefaultRetention"`
		}{DefaultRetention: rule}
	}
	h.writeXML(ctx, req, http.StatusOK, out)
	return nil
}

func (h *Handler) putObjectLockConfiguration(ctx context.Context, req *request) error {
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	if !body.bound(req.payloadHash) {
		return errUnsignedBody
	}
	var cfg objectLockConfiguration
	shape := xmlShape{
		"": {"ObjectLockConfiguration"}, "ObjectLockConfiguration": {"ObjectLockEnabled", "Rule"},
		"Rule": {"DefaultRetention"}, "DefaultRetention": {"Mode", "Days", "Years"},
	}
	if err := decodeShapedXML(body.body, maxRequestXML, &cfg, shape); err != nil {
		return err
	}
	if _, err := body.verify(); err != nil {
		return err
	}
	if cfg.ObjectLockEnabled != "" && cfg.ObjectLockEnabled != "Enabled" {
		return errMalformedXML
	}
	var rule *bucket.DefaultRetention
	if cfg.Rule != nil {
		d := cfg.Rule.DefaultRetention
		if d == nil {
			return errMalformedXML
		}
		rule = &bucket.DefaultRetention{Mode: object.RetentionMode(d.Mode)}
		if d.Days != nil {
			rule.Days = *d.Days
		}
		if d.Years != nil {
			rule.Years = *d.Years
		}
		if (d.Days != nil && *d.Days <= 0) || (d.Years != nil && *d.Years <= 0) {
			return fmt.Errorf("%w: a retention period is a positive number", domain.ErrInvalidArgument)
		}
	}
	if err := h.cfg.Buckets.SetDefaultRetention(ctx, req.principal, req.bucket, rule); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

type retentionDocument struct {
	XMLName         xml.Name `xml:"Retention"`
	XMLNS           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

func (h *Handler) lockInput(req *request) app.LockInput {
	return app.LockInput{Bucket: req.bucket, Key: req.key, Version: req.version}
}

func (h *Handler) getObjectRetention(ctx context.Context, req *request) error {
	retention, err := h.cfg.Objects.Retention(ctx, req.principal, h.lockInput(req))
	if errors.Is(err, domain.ErrLockNotConfigured) {
		return errNoObjectLock
	}
	if err != nil {
		return err
	}
	h.writeXML(ctx, req, http.StatusOK, retentionDocument{
		XMLNS: s3Namespace, Mode: string(retention.Mode), RetainUntilDate: retention.Until.UTC().Format(xmlTimeFormat),
	})
	return nil
}

func (h *Handler) putObjectRetention(ctx context.Context, req *request) error {
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	if !body.bound(req.payloadHash) {
		return errUnsignedBody
	}
	var doc retentionDocument
	if err := decodeStrictXML(body.body, maxRequestXML, &doc, "Retention", "Mode", "RetainUntilDate"); err != nil {
		return err
	}
	if _, err := body.verify(); err != nil {
		return err
	}
	bypass, err := bypassOf(req.Header)
	if err != nil {
		return err
	}
	// An empty document removes the retention; anything else names both a
	// mode and a date.
	var retention *object.Retention
	if doc.Mode != "" || doc.RetainUntilDate != "" {
		until, err := time.Parse(time.RFC3339Nano, doc.RetainUntilDate)
		if err != nil || (doc.Mode != string(object.Governance) && doc.Mode != string(object.Compliance)) {
			return errMalformedXML
		}
		retention = &object.Retention{Mode: object.RetentionMode(doc.Mode), Until: until.UTC()}
	}
	stored, err := h.cfg.Objects.SetRetention(ctx, req.principal, h.lockInput(req), retention, bypass)
	if err != nil {
		return err
	}
	setVersionHeader(req.w.Header(), stored, req.version != nil)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

type legalHoldDocument struct {
	XMLName xml.Name `xml:"LegalHold"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status"`
}

func (h *Handler) getObjectLegalHold(ctx context.Context, req *request) error {
	on, err := h.cfg.Objects.LegalHold(ctx, req.principal, h.lockInput(req))
	if err != nil {
		return err
	}
	status := legalHoldOff
	if on {
		status = legalHoldOn
	}
	h.writeXML(ctx, req, http.StatusOK, legalHoldDocument{XMLNS: s3Namespace, Status: status})
	return nil
}

func (h *Handler) putObjectLegalHold(ctx context.Context, req *request) error {
	body, err := newPayload(req.Request, req.payloadHash, req.chunks)
	if err != nil {
		return err
	}
	if !body.bound(req.payloadHash) {
		return errUnsignedBody
	}
	var doc legalHoldDocument
	if err := decodeStrictXML(body.body, maxRequestXML, &doc, "LegalHold", "Status"); err != nil {
		return err
	}
	if _, err := body.verify(); err != nil {
		return err
	}
	if doc.Status != legalHoldOn && doc.Status != legalHoldOff {
		return errMalformedXML
	}
	stored, err := h.cfg.Objects.SetLegalHold(ctx, req.principal, h.lockInput(req), doc.Status == legalHoldOn)
	if err != nil {
		return err
	}
	setVersionHeader(req.w.Header(), stored, req.version != nil)
	req.w.WriteHeader(http.StatusOK)
	return nil
}
