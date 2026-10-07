package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func listingKeys() []string {
	return []string{
		"a.txt", "b.txt",
		"photos/2025/jan.jpg", "photos/2025/feb.jpg", "photos/2026/mar.jpg", "photos/cover.jpg",
		"photos0", "videos/clip.mp4", "z.txt",
	}
}

func listFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.createBucket(t, "b")
	for _, key := range listingKeys() {
		f.put(t, "b", key, []byte(key))
	}
	return f
}

func keysOf(page object.ListPage) []string {
	keys := make([]string, len(page.Objects))
	for i, v := range page.Objects {
		keys[i] = v.Key
	}
	return keys
}

func TestList(t *testing.T) {
	t.Parallel()
	f := listFixture(t)

	tests := []struct {
		name             string
		opts             object.ListOptions
		wantKeys         []string
		wantPrefixes     []string
		wantTruncated    bool
		wantContinuation string
	}{
		{
			name:     "everything",
			opts:     object.ListOptions{MaxKeys: 1000},
			wantKeys: []string{"a.txt", "b.txt", "photos/2025/feb.jpg", "photos/2025/jan.jpg", "photos/2026/mar.jpg", "photos/cover.jpg", "photos0", "videos/clip.mp4", "z.txt"},
		},
		{
			name:         "top level with a delimiter",
			opts:         object.ListOptions{Delimiter: "/", MaxKeys: 1000},
			wantKeys:     []string{"a.txt", "b.txt", "photos0", "z.txt"},
			wantPrefixes: []string{"photos/", "videos/"},
		},
		{
			name:         "one level down",
			opts:         object.ListOptions{Prefix: "photos/", Delimiter: "/", MaxKeys: 1000},
			wantKeys:     []string{"photos/cover.jpg"},
			wantPrefixes: []string{"photos/2025/", "photos/2026/"},
		},
		{
			name:     "prefix without delimiter",
			opts:     object.ListOptions{Prefix: "photos/2025/", MaxKeys: 1000},
			wantKeys: []string{"photos/2025/feb.jpg", "photos/2025/jan.jpg"},
		},
		{
			name:     "prefix that is not a directory",
			opts:     object.ListOptions{Prefix: "photos", Delimiter: "/", MaxKeys: 1000},
			wantKeys: []string{"photos0"}, wantPrefixes: []string{"photos/"},
		},
		{name: "prefix that matches nothing", opts: object.ListOptions{Prefix: "nothing/", MaxKeys: 1000}},
		{
			name:     "start after a key",
			opts:     object.ListOptions{StartAfter: "photos0", MaxKeys: 1000},
			wantKeys: []string{"videos/clip.mp4", "z.txt"},
		},
		{
			name:     "start after a name that is not a key",
			opts:     object.ListOptions{StartAfter: "photos/9", MaxKeys: 1000},
			wantKeys: []string{"photos/cover.jpg", "photos0", "videos/clip.mp4", "z.txt"},
		},
		{
			// As in S3, start-after inside a group still reports the group
			// when keys of it follow.
			name:         "start after inside a group",
			opts:         object.ListOptions{StartAfter: "photos/2025/jan.jpg", Delimiter: "/", MaxKeys: 1000},
			wantKeys:     []string{"photos0", "z.txt"},
			wantPrefixes: []string{"photos/", "videos/"},
		},
		{
			name:     "start after before the prefix",
			opts:     object.ListOptions{Prefix: "videos/", StartAfter: "a", MaxKeys: 1000},
			wantKeys: []string{"videos/clip.mp4"},
		},
		{name: "start after the last key", opts: object.ListOptions{StartAfter: "zzz", MaxKeys: 1000}},
		{
			name:     "page limit",
			opts:     object.ListOptions{MaxKeys: 2},
			wantKeys: []string{"a.txt", "b.txt"}, wantTruncated: true, wantContinuation: "b.txt",
		},
		{
			name:     "page ending on a common prefix",
			opts:     object.ListOptions{Delimiter: "/", MaxKeys: 3},
			wantKeys: []string{"a.txt", "b.txt"}, wantPrefixes: []string{"photos/"}, wantTruncated: true, wantContinuation: "photos/",
		},
		{
			name:     "resume after a common prefix skips the whole group",
			opts:     object.ListOptions{Delimiter: "/", Continuation: "photos/", MaxKeys: 1000},
			wantKeys: []string{"photos0", "z.txt"}, wantPrefixes: []string{"videos/"},
		},
		{
			name:     "resume after a key",
			opts:     object.ListOptions{Continuation: "photos/cover.jpg", MaxKeys: 2},
			wantKeys: []string{"photos0", "videos/clip.mp4"}, wantTruncated: true, wantContinuation: "videos/clip.mp4",
		},
		{
			name:     "the token wins over start-after",
			opts:     object.ListOptions{Continuation: "videos/clip.mp4", StartAfter: "a", MaxKeys: 1000},
			wantKeys: []string{"z.txt"},
		},
		{
			name:     "exactly as many keys as the limit is not truncated",
			opts:     object.ListOptions{Prefix: "photos/2025/", MaxKeys: 2},
			wantKeys: []string{"photos/2025/feb.jpg", "photos/2025/jan.jpg"},
		},
		{name: "zero max-keys", opts: object.ListOptions{MaxKeys: 0}},
		{
			name:     "multi-character delimiter",
			opts:     object.ListOptions{Delimiter: "20", MaxKeys: 1000},
			wantKeys: []string{"a.txt", "b.txt", "photos/cover.jpg", "photos0", "videos/clip.mp4", "z.txt"}, wantPrefixes: []string{"photos/20"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			page, err := f.objects.List(context.Background(), porttest.Root(), "b", tt.opts)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if got := keysOf(page); !slices.Equal(got, tt.wantKeys) {
				t.Errorf("keys = %q, want %q", got, tt.wantKeys)
			}
			if !slices.Equal(page.CommonPrefixes, tt.wantPrefixes) {
				t.Errorf("common prefixes = %q, want %q", page.CommonPrefixes, tt.wantPrefixes)
			}
			if page.Truncated != tt.wantTruncated || page.NextContinuation != tt.wantContinuation {
				t.Errorf("truncated = %v, token = %q; want %v, %q", page.Truncated, page.NextContinuation, tt.wantTruncated, tt.wantContinuation)
			}
		})
	}
}

// TestListPagingIsCompleteAndWithoutDuplicates: paging through a listing with
// any page size yields exactly what one large page yields.
func TestListPagingIsCompleteAndWithoutDuplicates(t *testing.T) {
	t.Parallel()
	f := listFixture(t)
	ctx := context.Background()

	for _, base := range []object.ListOptions{
		{},
		{Delimiter: "/"},
		{Prefix: "photos/", Delimiter: "/"},
		{Prefix: "photos/"},
		{Prefix: "p", Delimiter: "/"},
		{Delimiter: "o"},
	} {
		whole := base
		whole.MaxKeys = 1000
		want, err := f.objects.List(ctx, porttest.Root(), "b", whole)
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		for pageSize := 1; pageSize <= 4; pageSize++ {
			var keys, prefixes []string
			opts := base
			opts.MaxKeys = pageSize
			for pages := 0; ; pages++ {
				if pages > 50 {
					t.Fatalf("%+v page size %d: listing does not end", base, pageSize)
				}
				page, err := f.objects.List(ctx, porttest.Root(), "b", opts)
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				if n := len(page.Objects) + len(page.CommonPrefixes); n > pageSize {
					t.Fatalf("%+v: page holds %d entries, limit %d", base, n, pageSize)
				}
				keys = append(keys, keysOf(page)...)
				prefixes = append(prefixes, page.CommonPrefixes...)
				if !page.Truncated {
					break
				}
				opts.Continuation = page.NextContinuation
			}
			if !slices.Equal(keys, keysOf(want)) || !slices.Equal(prefixes, want.CommonPrefixes) {
				t.Errorf("%+v page size %d:\n got %q %q\nwant %q %q", base, pageSize, keys, prefixes, keysOf(want), want.CommonPrefixes)
			}
		}
	}
}

func TestListErrors(t *testing.T) {
	t.Parallel()
	f := listFixture(t)
	ctx := context.Background()

	if _, err := f.objects.List(ctx, porttest.Root(), "missing", object.ListOptions{MaxKeys: 10}); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("List(missing bucket) error = %v", err)
	}
	if _, err := f.objects.List(ctx, porttest.Root(), "missing", object.ListOptions{MaxKeys: 0}); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("List(missing bucket, max-keys 0) error = %v", err)
	}
	if _, err := f.objects.List(ctx, porttest.Root(), "b", object.ListOptions{MaxKeys: -1}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("List(negative max-keys) error = %v", err)
	}
	// A token past every possible key ends the listing.
	page, err := f.objects.List(ctx, porttest.Root(), "b", object.ListOptions{Prefix: "\xff", Delimiter: "\xff", Continuation: "\xff\xff", MaxKeys: 10})
	if err != nil || len(page.Objects) != 0 || page.Truncated {
		t.Errorf("List(token with no successor) = %+v, %v", page, err)
	}
}

func TestListCapsThePageAtOneThousand(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	// The index is populated directly: this test is about the page limit.
	for i := range object.MaxListKeys + 5 {
		v := porttest.SampleVersion("b", "key-"+string(rune('a'+i/676%26))+string(rune('a'+i/26%26))+string(rune('a'+i%26)), 1)
		if _, err := f.meta.PutVersion(context.Background(), v); err != nil {
			t.Fatalf("PutVersion: %v", err)
		}
	}
	page, err := f.objects.List(context.Background(), porttest.Root(), "b", object.ListOptions{MaxKeys: 5000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != object.MaxListKeys || !page.Truncated {
		t.Errorf("page holds %d objects, truncated = %v; want %d, true", len(page.Objects), page.Truncated, object.MaxListKeys)
	}
}
