// Package bucket holds the domain model for buckets.
package bucket

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
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
