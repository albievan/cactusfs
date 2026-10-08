package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func (f *fixture) versioning(t *testing.T, name string, state bucket.Versioning) {
	t.Helper()
	if err := f.buckets.SetVersioning(context.Background(), porttest.Root(), name, state); err != nil {
		t.Fatalf("SetVersioning(%s): %v", state, err)
	}
}

func (f *fixture) getVersion(key string, ref object.VersionRef) ([]byte, error) {
	out, err := f.objects.Get(context.Background(), porttest.Root(), app.GetInput{Bucket: "b", Key: key, Version: &ref})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	return io.ReadAll(out.Body)
}

// userWith returns a user who holds one built-in role.
func userWith(role string) iam.Principal {
	return iam.Principal{AccessKeyID: "AKIDUSER", User: "u", Policies: []iam.Document{iam.BuiltInPolicies()[role]}}
}

// TestVersionedObjects is the life of an object in a bucket that keeps
// versions: nothing a put or a delete does destroys what was there.
func TestVersionedObjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	root := porttest.Root()

	before := randomBytes(t, 20_000)
	null := f.put(t, "b", "k", before)
	if null.Versioned {
		t.Fatalf("a version written before versioning has an ID of its own: %+v", null.Ref())
	}
	if _, err := f.getVersion("k", object.VersionRef{Null: true}); err != nil {
		t.Fatalf("the null version by name, in a bucket that never kept versions: %v", err)
	}

	f.versioning(t, "b", bucket.VersioningEnabled)
	first, second := randomBytes(t, 30_000), randomBytes(t, 40_000)
	v1 := f.put(t, "b", "k", first)
	v2 := f.put(t, "b", "k", second)
	if !v1.Versioned || !v2.Versioned || v1.VersionID == v2.VersionID || v2.Seq <= v1.Seq {
		t.Fatalf("versions: %+v, %+v", v1.Ref(), v2.Ref())
	}
	shards := f.shardCount()
	if shards != 3*sixDrives {
		t.Fatalf("%d shards for three versions, want %d: an overwrite must not remove what it replaces", shards, 3*sixDrives)
	}
	for name, want := range map[object.VersionRef][]byte{{Null: true}: before, v1.Ref(): first, v2.Ref(): second} {
		if got, err := f.getVersion("k", name); err != nil || !bytes.Equal(got, want) {
			t.Errorf("version %s: %d bytes, %v", name, len(got), err)
		}
	}
	if got, _, err := f.get("b", "k", nil); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("the current version is not the newest: %v", err)
	}

	// Deleting the object: a marker, and everything still there.
	deleted, err := f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "b", Key: "k"})
	if err != nil || deleted.Marker == nil || !deleted.Marker.DeleteMarker || deleted.Removed != nil {
		t.Fatalf("Delete = %+v, %v", deleted, err)
	}
	if f.shardCount() != shards {
		t.Fatalf("a delete in a versioned bucket removed shards: %d left of %d", f.shardCount(), shards)
	}
	if _, _, err := f.get("b", "k", nil); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("Get after a delete = %v, want %v", err, domain.ErrObjectNotFound)
	}
	if got, err := f.getVersion("k", v2.Ref()); err != nil || !bytes.Equal(got, second) {
		t.Errorf("the deleted object's last version by name: %v", err)
	}
	// A marker has no content, and no head.
	marker := deleted.Marker.Ref()
	if _, err := f.getVersion("k", marker); !errors.Is(err, domain.ErrMethodNotAllowed) {
		t.Errorf("Get of a delete marker = %v, want %v", err, domain.ErrMethodNotAllowed)
	}
	if _, err := f.objects.HeadVersion(ctx, root, "b", "k", marker); !errors.Is(err, domain.ErrMethodNotAllowed) {
		t.Errorf("HeadVersion of a delete marker = %v", err)
	}
	unknown := object.VersionRef{ID: object.ID{1, 2, 3}}
	if _, err := f.getVersion("k", unknown); !errors.Is(err, domain.ErrVersionNotFound) {
		t.Errorf("Get of an unknown version = %v, want %v", err, domain.ErrVersionNotFound)
	}

	// Removing the marker brings the object back; removing a version
	// frees its data and nothing else.
	if out, err := f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "b", Key: "k", Version: &marker}); err != nil || out.Removed == nil || !out.Removed.DeleteMarker {
		t.Fatalf("removing the marker = %+v, %v", out, err)
	}
	if got, _, err := f.get("b", "k", nil); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("the object did not come back: %v", err)
	}
	ref := v1.Ref()
	if out, err := f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "b", Key: "k", Version: &ref}); err != nil || out.Removed == nil || out.Removed.VersionID != v1.VersionID {
		t.Fatalf("removing a version = %+v, %v", out, err)
	}
	if f.shardCount() != 2*sixDrives {
		t.Errorf("%d shards after removing one of three versions, want %d", f.shardCount(), 2*sixDrives)
	}
	if _, err := f.getVersion("k", v1.Ref()); !errors.Is(err, domain.ErrVersionNotFound) {
		t.Errorf("a removed version = %v", err)
	}
	for name, want := range map[object.VersionRef][]byte{{Null: true}: before, v2.Ref(): second} {
		if got, err := f.getVersion("k", name); err != nil || !bytes.Equal(got, want) {
			t.Errorf("version %s after another was removed: %v", name, err)
		}
	}

	// Suspended: a put is the null version again and replaces the old one.
	f.versioning(t, "b", bucket.VersioningSuspended)
	replaced := randomBytes(t, 10_000)
	if v := f.put(t, "b", "k", replaced); v.Versioned {
		t.Fatalf("a put into a suspended bucket has an ID: %s", v.Ref())
	}
	if f.shardCount() != 2*sixDrives {
		t.Errorf("%d shards: the replaced null version's shards were not removed, or too many were", f.shardCount())
	}
	if got, err := f.getVersion("k", object.VersionRef{Null: true}); err != nil || !bytes.Equal(got, replaced) {
		t.Errorf("the null version after a suspended put: %v", err)
	}
	if got, err := f.getVersion("k", v2.Ref()); err != nil || !bytes.Equal(got, second) {
		t.Errorf("a version with an ID did not survive suspension: %v", err)
	}
}

// TestVersionPermissions: deleting an object and destroying a version are
// two permissions. The read-write role has the first only: in a bucket that
// keeps versions, a holder of it - or of its stolen key - destroys nothing.
func TestVersionPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	f.versioning(t, "b", bucket.VersioningEnabled)
	content := randomBytes(t, 5000)
	version := f.put(t, "b", "k", content)
	ref := version.Ref()

	tests := []struct {
		name string
		who  iam.Principal
		do   func(p iam.Principal) error
		want error
	}{
		{"read-write deletes the object", userWith(iam.RoleReadWrite), func(p iam.Principal) error {
			_, err := f.objects.Delete(ctx, p, app.DeleteInput{Bucket: "b", Key: "k"})
			return err
		}, nil},
		{"read-write destroys a version", userWith(iam.RoleReadWrite), func(p iam.Principal) error {
			_, err := f.objects.Delete(ctx, p, app.DeleteInput{Bucket: "b", Key: "k", Version: &ref})
			return err
		}, domain.ErrAccessDenied},
		{"read-write suspends versioning", userWith(iam.RoleReadWrite), func(p iam.Principal) error {
			return f.buckets.SetVersioning(ctx, p, "b", bucket.VersioningSuspended)
		}, domain.ErrAccessDenied},
		{"read-write reads a version", userWith(iam.RoleReadWrite), func(p iam.Principal) error {
			_, err := f.objects.HeadVersion(ctx, p, "b", "k", ref)
			return err
		}, nil},
		{"read-only reads a version", userWith(iam.RoleReadOnly), func(p iam.Principal) error {
			_, err := f.objects.HeadVersion(ctx, p, "b", "k", ref)
			return err
		}, nil},
		{"read-only lists versions", userWith(iam.RoleReadOnly), func(p iam.Principal) error {
			_, err := f.objects.ListVersions(ctx, p, "b", app.VersionListOptions{MaxKeys: 10})
			return err
		}, nil},
		{"read-only asks for the versioning state", userWith(iam.RoleReadOnly), func(p iam.Principal) error {
			_, err := f.buckets.Versioning(ctx, p, "b")
			return err
		}, nil},
		{"read-only deletes the object", userWith(iam.RoleReadOnly), func(p iam.Principal) error {
			_, err := f.objects.Delete(ctx, p, app.DeleteInput{Bucket: "b", Key: "k"})
			return err
		}, domain.ErrAccessDenied},
		{"nobody lists versions", iam.Principal{}, func(p iam.Principal) error {
			_, err := f.objects.ListVersions(ctx, p, "b", app.VersionListOptions{MaxKeys: 10})
			return err
		}, domain.ErrAccessDenied},
		{"a holder of GetObject alone reads a named version", iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{
			mustPolicyDoc(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`),
		}}, func(p iam.Principal) error {
			_, err := f.objects.HeadVersion(ctx, p, "b", "k", ref)
			return err
		}, domain.ErrAccessDenied},
		{"bucket-admin destroys a version", userWith(iam.RoleBucketAdmin), func(p iam.Principal) error {
			_, err := f.objects.Delete(ctx, p, app.DeleteInput{Bucket: "b", Key: "k", Version: &ref})
			return err
		}, nil},
	}
	for _, tt := range tests {
		if err := tt.do(tt.who); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
		if tt.want != nil {
			// What was refused changed nothing.
			if got, err := f.getVersion("k", ref); err != nil || !bytes.Equal(got, content) {
				t.Fatalf("after %q was refused, the version is damaged or gone: %v", tt.name, err)
			}
		}
	}
	if err := f.buckets.SetVersioning(ctx, porttest.Root(), "b", "Off"); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("SetVersioning to a state that does not exist = %v", err)
	}
	if err := f.buckets.SetVersioning(ctx, porttest.Root(), "no-such-bucket", bucket.VersioningEnabled); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("SetVersioning of a missing bucket = %v", err)
	}
}

func mustPolicyDoc(t *testing.T, document string) iam.Document {
	t.Helper()
	doc, err := iam.ParsePolicy([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestListVersions: pages, prefixes and delimiters of a version listing.
func TestListVersions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	f.versioning(t, "b", bucket.VersioningEnabled)
	// Three versions each of four keys, and a marker on one.
	for round := range 3 {
		for _, key := range []string{"a/1", "a/2", "b", "c/x/y"} {
			f.put(t, "b", key, []byte(fmt.Sprintf("%s round %d", key, round)))
		}
	}
	if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "b"}); err != nil {
		t.Fatal(err)
	}
	describe := func(page app.VersionListPage) []string {
		var out []string
		for _, v := range page.Versions {
			entry := v.Version.Key
			if v.Version.DeleteMarker {
				entry += "(marker)"
			}
			if v.Latest {
				entry += "*"
			}
			out = append(out, entry)
		}
		return append(out, page.CommonPrefixes...)
	}
	list := func(opts app.VersionListOptions) app.VersionListPage {
		t.Helper()
		page, err := f.objects.ListVersions(ctx, porttest.Root(), "b", opts)
		if err != nil {
			t.Fatalf("ListVersions(%+v): %v", opts, err)
		}
		return page
	}

	all := list(app.VersionListOptions{MaxKeys: 1000})
	want := []string{"a/1*", "a/1", "a/1", "a/2*", "a/2", "a/2", "b(marker)*", "b", "b", "b", "c/x/y*", "c/x/y", "c/x/y"}
	if got := describe(all); !slices.Equal(got, want) || all.Truncated || all.NextKeyMarker != "" {
		t.Fatalf("everything = %v (truncated %v), want %v", got, all.Truncated, want)
	}
	for i := 1; i < len(all.Versions); i++ {
		a, b := all.Versions[i-1].Version, all.Versions[i].Version
		if a.Key == b.Key && a.Seq <= b.Seq {
			t.Errorf("versions of %s are not newest first", a.Key)
		}
	}

	// Pages of every size add up to the whole, with nothing twice.
	for size := 1; size <= 14; size++ {
		var got []string
		opts := app.VersionListOptions{MaxKeys: size}
		for pages := 0; ; pages++ {
			page := list(opts)
			got = append(got, describe(page)...)
			if len(page.Versions) > size {
				t.Fatalf("a page of %d has %d entries", size, len(page.Versions))
			}
			if !page.Truncated {
				break
			}
			if pages > 20 {
				t.Fatalf("pages of %d do not end", size)
			}
			opts.KeyMarker, opts.VersionMarker = page.NextKeyMarker, page.NextVersionMarker
		}
		if !slices.Equal(got, want) {
			t.Fatalf("pages of %d give %v, want %v", size, got, want)
		}
	}

	if got := describe(list(app.VersionListOptions{MaxKeys: 1000, Prefix: "a/"})); !slices.Equal(got, want[:6]) {
		t.Errorf("prefix a/ = %v", got)
	}
	rolled := list(app.VersionListOptions{MaxKeys: 1000, Delimiter: "/"})
	if got := describe(rolled); !slices.Equal(got, []string{"b(marker)*", "b", "b", "b", "a/", "c/"}) {
		t.Errorf("delimiter / = %v", got)
	}
	// Paging through common prefixes.
	var got []string
	opts := app.VersionListOptions{MaxKeys: 2, Delimiter: "/"}
	for range 10 {
		page := list(opts)
		got = append(got, describe(page)...)
		if !page.Truncated {
			break
		}
		opts.KeyMarker, opts.VersionMarker = page.NextKeyMarker, page.NextVersionMarker
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"a/", "b", "b", "b", "b(marker)*", "c/"}) {
		t.Errorf("pages of 2 with a delimiter = %v", got)
	}
	if got := describe(list(app.VersionListOptions{MaxKeys: 1000, Prefix: "c/", Delimiter: "/"})); !slices.Equal(got, []string{"c/x/"}) {
		t.Errorf("prefix c/ with a delimiter = %v", got)
	}

	if page := list(app.VersionListOptions{}); len(page.Versions) != 0 || page.Truncated {
		t.Errorf("max-keys 0 = %+v", page)
	}
	if _, err := f.objects.ListVersions(ctx, porttest.Root(), "b", app.VersionListOptions{MaxKeys: -1}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("negative max-keys = %v", err)
	}
	for _, opts := range []app.VersionListOptions{{}, {MaxKeys: 5}} {
		if _, err := f.objects.ListVersions(ctx, porttest.Root(), "no-such-bucket", opts); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("ListVersions of a missing bucket = %v", err)
		}
	}
}

// TestOldVersionsAreKeptAndCared: the shards of a version that is no longer
// current are not garbage, and bitrot in them is found and healed like in
// any other.
func TestOldVersionsAreKeptAndCared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	for _, d := range f.drives {
		d.Now = func() time.Time { return testNow.Add(-48 * time.Hour) }
	}
	f.versioning(t, "b", bucket.VersioningEnabled)
	old := f.put(t, "b", "k", randomBytes(t, 30_000))
	content := randomBytes(t, 30_000)
	middle := f.put(t, "b", "k", content)
	f.put(t, "b", "k", randomBytes(t, 30_000))
	if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "k"}); err != nil {
		t.Fatal(err)
	}

	// The collector, long after the grace period: nothing of any version.
	collector := app.NewCollector(app.CollectorConfig{
		Drives: asStores(f.drives), Index: f.meta, Clock: f.clock, Grace: 24 * time.Hour, Logger: slog.New(slog.DiscardHandler),
	})
	if stats, err := collector.RunOnce(ctx); err != nil || stats.Removed != 0 {
		t.Fatalf("the collector removed %d shards of versions that exist (%v)", stats.Removed, err)
	}
	if f.shardCount() != 3*sixDrives {
		t.Fatalf("%d shards, want %d", f.shardCount(), 3*sixDrives)
	}

	// A shard of an old version is lost, one of another corrupted.
	drives := f.shardsOf(t, middle)
	if err := drives[2].Delete(ctx, shardID(middle, 2)); err != nil {
		t.Fatal(err)
	}
	f.shardsOf(t, old)[0].Damage(shardID(old, 0))
	scrubber := app.NewScrubber(app.ScrubberConfig{
		Buckets: f.meta, Index: f.meta, Healer: f.objects.Healer(), Logger: slog.New(slog.DiscardHandler),
	})
	stats, err := scrubber.RunOnce(ctx)
	if err != nil || stats.Objects != 3 || stats.Damaged != 2 || stats.ShardsRebuilt != 2 || stats.Unrecoverable != 0 {
		t.Fatalf("scrub = %+v, %v; want three versions checked, two healed", stats, err)
	}
	if f.shardCount() != 3*sixDrives {
		t.Errorf("%d shards after the scrub, want %d", f.shardCount(), 3*sixDrives)
	}
	if got, err := f.getVersion("k", middle.Ref()); err != nil || !bytes.Equal(got, content) {
		t.Errorf("the healed old version: %v", err)
	}
	// Healing by name works for a version that is not current, and a read
	// that finds one degraded queues that version, not the current one.
	if err := drives[1].Delete(ctx, shardID(middle, 1)); err != nil {
		t.Fatal(err)
	}
	if result, err := f.objects.Healer().HealVersion(ctx, "b", "k", middle.Ref()); err != nil || result.Rebuilt != 1 {
		t.Errorf("HealVersion = %+v, %v", result, err)
	}
	// Removed versions are collected as before: their shards go at once.
	for _, v := range []object.Version{old, middle} {
		ref := v.Ref()
		if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "k", Version: &ref}); err != nil {
			t.Fatal(err)
		}
	}
	if f.shardCount() != sixDrives {
		t.Errorf("%d shards after two versions were removed, want %d", f.shardCount(), sixDrives)
	}
}
