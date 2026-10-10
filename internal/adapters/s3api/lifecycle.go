package s3api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// Lifecycle rules: the ?lifecycle sub-resource of a bucket. Rules expire and
// remove; there are no storage classes, so transitions are not implemented,
// and neither are filters on tags or on size.

type lifecycleConfiguration struct {
	XMLName xml.Name           `xml:"LifecycleConfiguration"`
	XMLNS   string             `xml:"xmlns,attr,omitempty"`
	Rules   []lifecycleRuleXML `xml:"Rule"`
}

type lifecycleRuleXML struct {
	ID     string `xml:"ID,omitempty"`
	Status string `xml:"Status"`
	// Prefix is where a rule of the first version of the API keeps its
	// prefix; Filter is where a later one does. Both are read; Filter is
	// written.
	Prefix *string `xml:"Prefix,omitempty"`
	Filter *struct {
		Prefix string `xml:"Prefix"`
	} `xml:"Filter"`
	Expiration *struct {
		Days                      *int    `xml:"Days,omitempty"`
		Date                      *string `xml:"Date,omitempty"`
		ExpiredObjectDeleteMarker *bool   `xml:"ExpiredObjectDeleteMarker,omitempty"`
	} `xml:"Expiration,omitempty"`
	Noncurrent *struct {
		NoncurrentDays int `xml:"NoncurrentDays"`
	} `xml:"NoncurrentVersionExpiration,omitempty"`
	Abort *struct {
		DaysAfterInitiation int `xml:"DaysAfterInitiation"`
	} `xml:"AbortIncompleteMultipartUpload,omitempty"`
}

// unsupportedLifecycle are elements of a lifecycle configuration that ask for
// what is not implemented. A rule that carries one is refused as such: read
// without it, the rule would remove more, or other things, than was meant.
func unsupportedLifecycle() []string {
	return []string{"Transition", "NoncurrentVersionTransition", "Tag", "And", "ObjectSizeGreaterThan", "ObjectSizeLessThan", "NewerNoncurrentVersions"}
}

// lifecycleShape is where each element of a lifecycle configuration belongs.
func lifecycleShape() xmlShape {
	return xmlShape{
		"":                               {"LifecycleConfiguration"},
		"LifecycleConfiguration":         {"Rule*"},
		"Rule":                           {"ID", "Status", "Prefix", "Filter", "Expiration", "NoncurrentVersionExpiration", "AbortIncompleteMultipartUpload"},
		"Filter":                         {"Prefix"},
		"Expiration":                     {"Days", "Date", "ExpiredObjectDeleteMarker"},
		"NoncurrentVersionExpiration":    {"NoncurrentDays"},
		"AbortIncompleteMultipartUpload": {"DaysAfterInitiation"},
	}
}

func (h *Handler) putBucketLifecycle(ctx context.Context, req *request) error {
	raw, err := h.configBody(req)
	if err != nil {
		return err
	}
	var in lifecycleConfiguration
	if err := decodeShapedXML(bytes.NewReader(raw), maxConfigBody, &in, lifecycleShape(), unsupportedLifecycle()...); err != nil {
		return err
	}
	rules := bucket.Lifecycle{}
	for i, r := range in.Rules {
		rule := bucket.LifecycleRule{ID: r.ID}
		switch r.Status {
		case "Enabled":
			rule.Enabled = true
		case "Disabled":
		default:
			return fmt.Errorf("%w: lifecycle: rule %d: Status is Enabled or Disabled", domain.ErrInvalidArgument, i)
		}
		switch {
		case r.Prefix != nil && r.Filter != nil:
			return fmt.Errorf("%w: lifecycle: rule %d: Prefix and Filter exclude each other", domain.ErrInvalidArgument, i)
		case r.Prefix != nil:
			rule.Prefix = *r.Prefix
		case r.Filter != nil:
			rule.Prefix = r.Filter.Prefix
		}
		if e := r.Expiration; e != nil {
			if e.Days != nil {
				if *e.Days <= 0 || *e.Days > bucket.MaxLifecycleDays {
					return fmt.Errorf("%w: lifecycle: rule %d: Days is a positive number", domain.ErrInvalidArgument, i)
				}
				rule.ExpirationDays = *e.Days
			}
			if e.Date != nil {
				date, err := time.Parse(time.RFC3339, *e.Date)
				if err != nil {
					if date, err = time.Parse(time.DateOnly, *e.Date); err != nil {
						return fmt.Errorf("%w: lifecycle: rule %d: Date", domain.ErrInvalidArgument, i)
					}
				}
				rule.ExpirationDate = date.UTC()
			}
			rule.ExpiredObjectDeleteMarker = e.ExpiredObjectDeleteMarker != nil && *e.ExpiredObjectDeleteMarker
		}
		if r.Noncurrent != nil {
			if r.Noncurrent.NoncurrentDays <= 0 || r.Noncurrent.NoncurrentDays > bucket.MaxLifecycleDays {
				return fmt.Errorf("%w: lifecycle: rule %d: NoncurrentDays is a positive number", domain.ErrInvalidArgument, i)
			}
			rule.NoncurrentDays = r.Noncurrent.NoncurrentDays
		}
		if r.Abort != nil {
			if r.Abort.DaysAfterInitiation <= 0 || r.Abort.DaysAfterInitiation > bucket.MaxLifecycleDays {
				return fmt.Errorf("%w: lifecycle: rule %d: DaysAfterInitiation is a positive number", domain.ErrInvalidArgument, i)
			}
			rule.AbortIncompleteDays = r.Abort.DaysAfterInitiation
		}
		rules.Rules = append(rules.Rules, rule)
	}
	if err := h.cfg.Buckets.SetLifecycle(ctx, req.principal, req.bucket, &rules); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getBucketLifecycle(ctx context.Context, req *request) error {
	rules, err := h.cfg.Buckets.Lifecycle(ctx, req.principal, req.bucket)
	if err != nil {
		return err
	}
	if rules == nil {
		return errNoSuchConfig("NoSuchLifecycleConfiguration", "lifecycle configuration")
	}
	out := lifecycleConfiguration{XMLNS: s3Namespace}
	for _, r := range rules.Rules {
		rule := lifecycleRuleXML{ID: r.ID, Status: "Disabled"}
		if r.Enabled {
			rule.Status = "Enabled"
		}
		rule.Filter = &struct {
			Prefix string `xml:"Prefix"`
		}{Prefix: r.Prefix}
		if r.ExpirationDays > 0 || !r.ExpirationDate.IsZero() || r.ExpiredObjectDeleteMarker {
			rule.Expiration = &struct {
				Days                      *int    `xml:"Days,omitempty"`
				Date                      *string `xml:"Date,omitempty"`
				ExpiredObjectDeleteMarker *bool   `xml:"ExpiredObjectDeleteMarker,omitempty"`
			}{}
			if r.ExpirationDays > 0 {
				days := r.ExpirationDays
				rule.Expiration.Days = &days
			}
			if !r.ExpirationDate.IsZero() {
				date := r.ExpirationDate.UTC().Format(time.RFC3339)
				rule.Expiration.Date = &date
			}
			if r.ExpiredObjectDeleteMarker {
				yes := true
				rule.Expiration.ExpiredObjectDeleteMarker = &yes
			}
		}
		if r.NoncurrentDays > 0 {
			rule.Noncurrent = &struct {
				NoncurrentDays int `xml:"NoncurrentDays"`
			}{r.NoncurrentDays}
		}
		if r.AbortIncompleteDays > 0 {
			rule.Abort = &struct {
				DaysAfterInitiation int `xml:"DaysAfterInitiation"`
			}{r.AbortIncompleteDays}
		}
		out.Rules = append(out.Rules, rule)
	}
	h.writeXML(ctx, req, http.StatusOK, out)
	return nil
}

func (h *Handler) deleteBucketLifecycle(ctx context.Context, req *request) error {
	if err := h.cfg.Buckets.SetLifecycle(ctx, req.principal, req.bucket, nil); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}
