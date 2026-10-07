package bucket_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

func TestValidateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"shortest", "abc", true},
		{"longest", strings.Repeat("a", 63), true},
		{"digits", "123", true},
		{"hyphens", "my-bucket-01", true},
		{"dots", "logs.example.com", true},
		{"starts with digit", "1bucket", true},
		{"looks like but is not an IP", "1.2.3", true},

		{"empty", "", false},
		{"too short", "ab", false},
		{"too long", strings.Repeat("a", 64), false},
		{"uppercase", "MyBucket", false},
		{"underscore", "my_bucket", false},
		{"space", "my bucket", false},
		{"slash", "my/bucket", false},
		{"traversal", "../etc", false},
		{"non-ASCII", "bücket", false},
		{"NUL", "buck\x00et", false},
		{"leading hyphen", "-bucket", false},
		{"trailing hyphen", "bucket-", false},
		{"leading dot", ".bucket", false},
		{"trailing dot", "bucket.", false},
		{"adjacent dots", "my..bucket", false},
		{"hyphen before dot", "my-.bucket", false},
		{"hyphen after dot", "my.-bucket", false},
		{"IPv4 address", "192.168.5.4", false},
		{"punycode prefix", "xn--bucket", false},
		{"sthree prefix", "sthree-bucket", false},
		{"demo prefix", "amzn-s3-demo-bucket", false},
		{"alias suffix", "bucket-s3alias", false},
		{"object lambda suffix", "bucket--ol-s3", false},
		{"mrap suffix", "bucket.mrap", false},
		{"directory bucket suffix", "bucket--x-s3", false},
		{"table bucket suffix", "bucket--table-s3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := bucket.ValidateName(tt.input)
			if tt.valid && err != nil {
				t.Errorf("ValidateName(%q) = %v, want nil", tt.input, err)
			}
			if !tt.valid && !errors.Is(err, domain.ErrInvalidBucketName) {
				t.Errorf("ValidateName(%q) = %v, want %v", tt.input, err, domain.ErrInvalidBucketName)
			}
		})
	}
}

// FuzzValidateName checks the properties every accepted name must have,
// whatever the input: it is a DNS-compatible host name and can never be read
// as a path.
func FuzzValidateName(f *testing.F) {
	for _, seed := range []string{"abc", "my-bucket", "a.b.c", "../x", "A", "192.168.1.1", "xn--a", strings.Repeat("a", 64)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if err := bucket.ValidateName(name); err != nil {
			if !errors.Is(err, domain.ErrInvalidBucketName) {
				t.Fatalf("ValidateName(%q) returned an untyped error: %v", name, err)
			}
			return
		}
		if len(name) < bucket.MinNameLength || len(name) > bucket.MaxNameLength {
			t.Fatalf("accepted %q with length %d", name, len(name))
		}
		if strings.ContainsAny(name, "/\\\x00 _") || strings.Contains(name, "..") {
			t.Fatalf("accepted %q, which contains a forbidden sequence", name)
		}
		if name != strings.ToLower(name) {
			t.Fatalf("accepted %q, which is not lowercase", name)
		}
		for label := range strings.SplitSeq(name, ".") {
			if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
				t.Fatalf("accepted %q, whose label %q is not a DNS label", name, label)
			}
		}
	})
}
