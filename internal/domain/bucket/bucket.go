// Package bucket holds the domain model for buckets.
package bucket

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Limits of a bucket name, from the Amazon S3 general purpose bucket naming
// rules.
const (
	MinNameLength = 3
	MaxNameLength = 63
)

// Bucket is a container of objects.
type Bucket struct {
	// Name is the globally unique, DNS-compatible name of the bucket.
	Name string
	// CreatedAt is when the bucket was created.
	CreatedAt time.Time
	// Versioning says whether the bucket keeps the versions of its objects.
	Versioning Versioning
	// ObjectLock says that the versions of the bucket can be locked. It is
	// decided when the bucket is created and never changes.
	ObjectLock bool
	// DefaultRetention, when not nil, is given to every version put into
	// the bucket without a retention of its own.
	DefaultRetention *DefaultRetention
}

// DefaultRetention is the retention a bucket gives its new versions: a mode,
// and a period in days or in years.
type DefaultRetention struct {
	Mode  object.RetentionMode
	Days  int
	Years int
}

// Limits of a default retention period.
const (
	MaxRetentionDays  = 36500
	MaxRetentionYears = 100
)

// Validate reports whether d is a default retention: a mode, and exactly one
// of days and years, within limits. The error wraps
// domain.ErrInvalidArgument.
func (d DefaultRetention) Validate() error {
	switch {
	case d.Mode != object.Governance && d.Mode != object.Compliance:
		return fmt.Errorf("%w: retention mode %q", domain.ErrInvalidArgument, d.Mode)
	case (d.Days == 0) == (d.Years == 0):
		return fmt.Errorf("%w: a default retention has days or years, not both and not neither", domain.ErrInvalidArgument)
	case d.Days < 0 || d.Days > MaxRetentionDays || d.Years < 0 || d.Years > MaxRetentionYears:
		return fmt.Errorf("%w: default retention period out of range", domain.ErrInvalidArgument)
	}
	return nil
}

// Until returns when a version put at the time at is retained until. A day
// is 24 hours and a year is 365 days: fixed lengths, which every replica
// computes alike and which do not depend on a calendar.
func (d DefaultRetention) Until(at time.Time) time.Time {
	const day = 24 * time.Hour
	return at.Add(time.Duration(d.Days)*day + time.Duration(d.Years)*365*day).UTC()
}

// Versioning is the versioning state of a bucket.
type Versioning string

// The versioning states. A bucket begins unversioned; once it has kept
// versions it can stop making new ones, but it never forgets that it did.
const (
	Unversioned         Versioning = ""
	VersioningEnabled   Versioning = "Enabled"
	VersioningSuspended Versioning = "Suspended"
)

// ValidateVersioningChange checks a change of the versioning state. A bucket
// cannot go back to never having been versioned, and a bucket whose versions
// can be locked cannot stop keeping them. A refusal wraps
// domain.ErrInvalidArgument or domain.ErrInvalidBucketState.
func ValidateVersioningChange(to Versioning, objectLock bool) error {
	switch {
	case to != VersioningEnabled && to != VersioningSuspended:
		return fmt.Errorf("%w: versioning status %q", domain.ErrInvalidArgument, to)
	case to == VersioningSuspended && objectLock:
		return fmt.Errorf("%w: versioning cannot be suspended on a bucket with Object Lock", domain.ErrInvalidBucketState)
	}
	return nil
}

// reservedPrefixes and reservedSuffixes are reserved by the S3 naming rules.
func reservedPrefixes() []string {
	return []string{"xn--", "sthree-", "amzn-s3-demo-"}
}

func reservedSuffixes() []string {
	return []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"}
}

// ValidateName reports whether name is a valid bucket name. The rules are the
// Amazon S3 general purpose bucket naming rules, with one tightening: every
// dot-separated label must start and end with a letter or digit, so that a
// name is always a valid DNS host name for virtual-hosted-style requests.
//
// The returned error wraps domain.ErrInvalidBucketName.
func ValidateName(name string) error {
	if len(name) < MinNameLength || len(name) > MaxNameLength {
		return invalid(name, fmt.Sprintf("length must be %d to %d characters", MinNameLength, MaxNameLength))
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; !isLowerAlnum(c) && c != '.' && c != '-' {
			return invalid(name, "only lowercase letters, digits, dots and hyphens are allowed")
		}
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" {
			return invalid(name, "must not start or end with a dot or contain adjacent dots")
		}
		if !isLowerAlnum(label[0]) || !isLowerAlnum(label[len(label)-1]) {
			return invalid(name, "each dot-separated label must start and end with a letter or digit")
		}
	}
	if net.ParseIP(name) != nil {
		return invalid(name, "must not be formatted as an IP address")
	}
	for _, prefix := range reservedPrefixes() {
		if strings.HasPrefix(name, prefix) {
			return invalid(name, fmt.Sprintf("must not start with %q", prefix))
		}
	}
	for _, suffix := range reservedSuffixes() {
		if strings.HasSuffix(name, suffix) {
			return invalid(name, fmt.Sprintf("must not end with %q", suffix))
		}
	}
	return nil
}

func isLowerAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func invalid(name, reason string) error {
	return fmt.Errorf("%w: %q: %s", domain.ErrInvalidBucketName, name, reason)
}
