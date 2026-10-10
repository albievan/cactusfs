package bucket

import (
	"fmt"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// ConfigLifecycle is the lifecycle rules of a bucket, as a configuration
// document.
const ConfigLifecycle = "lifecycle"

// Limits of a lifecycle configuration, from the Amazon S3 API reference.
const (
	maxLifecycleRules  = 1000
	maxLifecycleID     = 255
	maxLifecyclePrefix = 1024
	// MaxLifecycleDays is the greatest number of days a rule may name:
	// about two hundred and seventy years.
	MaxLifecycleDays = 100000
)

// LifecycleRule says what becomes of objects whose keys begin with Prefix
// when they are old enough. A rule only ever removes: there are no storage
// classes to move anything to.
type LifecycleRule struct {
	ID      string `json:"id,omitempty"`
	Enabled bool   `json:"enabled"`
	// Prefix selects the keys the rule is about; empty selects all.
	Prefix string `json:"prefix,omitempty"`
	// ExpirationDays expires the current version of an object that many
	// days after it was made; ExpirationDate expires it on that day. In a
	// bucket that keeps versions, expiring makes a delete marker.
	ExpirationDays int       `json:"expiration_days,omitempty"`
	ExpirationDate time.Time `json:"expiration_date,omitzero"`
	// ExpiredObjectDeleteMarker removes a delete marker that is the only
	// version left of its key.
	ExpiredObjectDeleteMarker bool `json:"expired_object_delete_marker,omitempty"`
	// NoncurrentDays removes a version for good that many days after a
	// newer one took its place.
	NoncurrentDays int `json:"noncurrent_days,omitempty"`
	// AbortIncompleteDays aborts a multipart upload that many days after
	// it began.
	AbortIncompleteDays int `json:"abort_incomplete_days,omitempty"`
}

// Lifecycle is the lifecycle configuration of a bucket.
type Lifecycle struct {
	Rules []LifecycleRule `json:"rules"`
}

func invalidLifecycle(format string, args ...any) error {
	return fmt.Errorf("%w: lifecycle: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// Validate reports whether the configuration is one that can be kept.
func (l Lifecycle) Validate() error {
	if len(l.Rules) == 0 || len(l.Rules) > maxLifecycleRules {
		return invalidLifecycle("needs 1 to %d rules", maxLifecycleRules)
	}
	ids := map[string]bool{}
	for i, r := range l.Rules {
		if len(r.ID) > maxLifecycleID || len(r.Prefix) > maxLifecyclePrefix {
			return invalidLifecycle("rule %d: ID or prefix too long", i)
		}
		if r.ID != "" {
			if ids[r.ID] {
				return invalidLifecycle("rule %d: ID %q is used twice", i, r.ID)
			}
			ids[r.ID] = true
		}
		for _, days := range []int{r.ExpirationDays, r.NoncurrentDays, r.AbortIncompleteDays} {
			if days < 0 || days > MaxLifecycleDays {
				return invalidLifecycle("rule %d: a number of days is between 1 and %d", i, MaxLifecycleDays)
			}
		}
		date := r.ExpirationDate
		if !date.IsZero() && !date.Equal(date.UTC().Truncate(24*time.Hour)) {
			return invalidLifecycle("rule %d: an expiration date is midnight UTC", i)
		}
		ways := 0
		for _, set := range []bool{r.ExpirationDays > 0, !date.IsZero(), r.ExpiredObjectDeleteMarker} {
			if set {
				ways++
			}
		}
		if ways > 1 {
			return invalidLifecycle("rule %d: Days, Date and ExpiredObjectDeleteMarker exclude each other", i)
		}
		if ways == 0 && r.NoncurrentDays == 0 && r.AbortIncompleteDays == 0 {
			return invalidLifecycle("rule %d: does nothing", i)
		}
	}
	return nil
}

// dueAfter reports whether something made at t is due at now under a rule of
// that many days: from the first midnight UTC that is at least that many days
// later, as S3 counts.
//
// A time that is not there - a record that lost it - is never due, and
// neither is anything under a number of days no rule may name: a rule
// removes only what it can show to be old.
func dueAfter(t time.Time, days int, now time.Time) bool {
	if t.IsZero() || days <= 0 || days > MaxLifecycleDays {
		return false
	}
	// By the calendar, not by a product of durations, which overflows.
	at := t.UTC().AddDate(0, 0, days)
	if midnight := at.Truncate(24 * time.Hour); !midnight.Equal(at) {
		at = midnight.AddDate(0, 0, 1)
	}
	return !now.Before(at)
}

// about returns the enabled rules that are about a key.
func (l Lifecycle) about(key string, visit func(r LifecycleRule) bool) bool {
	for _, r := range l.Rules {
		if r.Enabled && strings.HasPrefix(key, r.Prefix) && visit(r) {
			return true
		}
	}
	return false
}

// ExpiresCurrent reports whether the current version of key, made at
// modified, has expired at now.
func (l Lifecycle) ExpiresCurrent(key string, modified, now time.Time) bool {
	return l.about(key, func(r LifecycleRule) bool {
		switch {
		case r.ExpirationDays > 0:
			return dueAfter(modified, r.ExpirationDays, now)
		case !r.ExpirationDate.IsZero():
			return !modified.IsZero() && !now.Before(r.ExpirationDate)
		}
		return false
	})
}

// ExpiresNoncurrent reports whether a version of key that stopped being the
// current one at replaced is to be removed for good at now.
func (l Lifecycle) ExpiresNoncurrent(key string, replaced, now time.Time) bool {
	return l.about(key, func(r LifecycleRule) bool {
		return dueAfter(replaced, r.NoncurrentDays, now)
	})
}

// RemovesLoneMarker reports whether a delete marker that is the only version
// left of key is to be removed.
func (l Lifecycle) RemovesLoneMarker(key string) bool {
	return l.about(key, func(r LifecycleRule) bool { return r.ExpiredObjectDeleteMarker })
}

// AbortsUpload reports whether a multipart upload of key begun at initiated
// is to be aborted at now.
func (l Lifecycle) AbortsUpload(key string, initiated, now time.Time) bool {
	return l.about(key, func(r LifecycleRule) bool {
		return dueAfter(initiated, r.AbortIncompleteDays, now)
	})
}
