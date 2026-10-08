package bucket_test

import (
	"errors"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestDefaultRetention(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    bucket.DefaultRetention
		want error
	}{
		{"days", bucket.DefaultRetention{Mode: object.Governance, Days: 30}, nil},
		{"years", bucket.DefaultRetention{Mode: object.Compliance, Years: 7}, nil},
		{"the longest", bucket.DefaultRetention{Mode: object.Compliance, Days: bucket.MaxRetentionDays}, nil},
		{"neither", bucket.DefaultRetention{Mode: object.Compliance}, domain.ErrInvalidArgument},
		{"both", bucket.DefaultRetention{Mode: object.Compliance, Days: 1, Years: 1}, domain.ErrInvalidArgument},
		{"no mode", bucket.DefaultRetention{Days: 1}, domain.ErrInvalidArgument},
		{"negative", bucket.DefaultRetention{Mode: object.Governance, Days: -1}, domain.ErrInvalidArgument},
		{"too many days", bucket.DefaultRetention{Mode: object.Governance, Days: bucket.MaxRetentionDays + 1}, domain.ErrInvalidArgument},
		{"too many years", bucket.DefaultRetention{Mode: object.Governance, Years: bucket.MaxRetentionYears + 1}, domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		if err := tt.d.Validate(); !errors.Is(err, tt.want) {
			t.Errorf("%s: Validate = %v, want %v", tt.name, err, tt.want)
		}
	}
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.FixedZone("x", 3600))
	if got := (bucket.DefaultRetention{Mode: object.Governance, Days: 2}).Until(at); !got.Equal(at.Add(48*time.Hour)) || got.Location() != time.UTC {
		t.Errorf("two days from %v = %v", at, got)
	}
	if got := (bucket.DefaultRetention{Mode: object.Governance, Years: 1}).Until(at); !got.Equal(at.Add(365 * 24 * time.Hour)) {
		t.Errorf("a year from %v = %v", at, got)
	}
}

func TestVersioningChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		to   bucket.Versioning
		lock bool
		want error
	}{
		{bucket.VersioningEnabled, false, nil},
		{bucket.VersioningSuspended, false, nil},
		{bucket.VersioningEnabled, true, nil},
		{bucket.VersioningSuspended, true, domain.ErrInvalidBucketState},
		{bucket.Unversioned, false, domain.ErrInvalidArgument},
		{"enabled", false, domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		if err := bucket.ValidateVersioningChange(tt.to, tt.lock); !errors.Is(err, tt.want) {
			t.Errorf("to %q with lock %v: %v, want %v", tt.to, tt.lock, err, tt.want)
		}
	}
}
