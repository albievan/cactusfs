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
	"github.com/albievan/cactusfs/internal/ports"
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

// TestListVersionsEdges: delimiters that are not ASCII, and a listing whose
// marker disappears between two pages.
func TestListVersionsEdges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	f.versioning(t, "b", bucket.VersioningEnabled)
	for _, key := range []string{"xéy", "xêz", "xëw", "a\x00b", "a\x00c"} {
		f.put(t, "b", key, []byte(key))
	}
	list := func(opts app.VersionListOptions) app.VersionListPage {
		t.Helper()
		page, err := f.objects.ListVersions(ctx, porttest.Root(), "b", opts)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	keys := func(page app.VersionListPage) []string {
		var out []string
		for _, v := range page.Versions {
			out = append(out, v.Version.Key)
		}
		return append(out, page.CommonPrefixes...)
	}
	// Whatever the delimiter, a version listing names what the object
	// listing names.
	for _, delimiter := range []string{"é", "ê", "\x00", "x", "y"} {
		objects, err := f.objects.List(ctx, porttest.Root(), "b", object.ListOptions{MaxKeys: 1000, Delimiter: delimiter})
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, v := range objects.Objects {
			want = append(want, v.Key)
		}
		want = append(want, objects.CommonPrefixes...)
		if got := keys(list(app.VersionListOptions{MaxKeys: 1000, Delimiter: delimiter})); !slices.Equal(got, want) {
			t.Errorf("delimiter %q: versions %q, objects %q", delimiter, got, want)
		}
		// And page by page.
		var paged []string
		opts := app.VersionListOptions{MaxKeys: 1, Delimiter: delimiter}
		for range 20 {
			page := list(opts)
			paged = append(paged, keys(page)...)
			if !page.Truncated {
				break
			}
			opts.KeyMarker, opts.VersionMarker = page.NextKeyMarker, page.NextVersionMarker
		}
		slices.Sort(paged)
		sorted := slices.Clone(want)
		slices.Sort(sorted)
		if !slices.Equal(paged, sorted) {
			t.Errorf("delimiter %q, pages of one: %q, want %q", delimiter, paged, sorted)
		}
	}

	// The version a page ended with is removed before the next page is
	// asked for. Where it was cannot be told any more: the listing goes on
	// with the next key and repeats nothing.
	for range 4 {
		f.put(t, "b", "k", []byte("again"))
	}
	f.put(t, "b", "z", []byte("z"))
	first := list(app.VersionListOptions{MaxKeys: 2, Prefix: "k"})
	if len(first.Versions) != 2 || first.NextVersionMarker == nil {
		t.Fatalf("first page = %+v", first)
	}
	if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "k", Version: first.NextVersionMarker}); err != nil {
		t.Fatal(err)
	}
	rest := list(app.VersionListOptions{MaxKeys: 100, KeyMarker: first.NextKeyMarker, VersionMarker: first.NextVersionMarker})
	for _, v := range rest.Versions {
		if v.Version.Key <= "k" {
			t.Errorf("after a marker that is gone the listing went back to %q", v.Version.Key)
		}
	}
	if len(rest.Versions) == 0 {
		t.Error("after a marker that is gone the listing ended instead of going on")
	}
}

// TestReadWriteUnderSuspendedVersioning: what the read-write role cannot
// destroy, it cannot destroy while versioning is enabled. Suspended, a put
// or a delete replaces the null version, as S3 defines it - and versions
// with IDs stay. The role cannot suspend versioning itself.
func TestReadWriteUnderSuspendedVersioning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	writer := userWith(iam.RoleReadWrite)
	f.versioning(t, "b", bucket.VersioningEnabled)
	kept := f.put(t, "b", "k", []byte("has an ID"))
	if err := f.buckets.SetVersioning(ctx, writer, "b", bucket.VersioningSuspended); !errors.Is(err, domain.ErrAccessDenied) {
		t.Fatalf("read-write suspending versioning = %v", err)
	}
	f.versioning(t, "b", bucket.VersioningSuspended)
	put := func(content string) {
		t.Helper()
		if _, err := f.objects.Put(ctx, writer, app.PutInput{Bucket: "b", Key: "k", Body: bytes.NewReader([]byte(content)), Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
	}
	put("null one")
	put("null two")
	if _, err := f.objects.Delete(ctx, writer, app.DeleteInput{Bucket: "b", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	null := object.VersionRef{Null: true}
	if _, err := f.objects.Delete(ctx, writer, app.DeleteInput{Bucket: "b", Key: "k", Version: &null}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("read-write removing the null version by name = %v", err)
	}
	if got, err := f.getVersion("k", kept.Ref()); err != nil || string(got) != "has an ID" {
		t.Errorf("the version with an ID after puts and a delete under suspension: %q, %v", got, err)
	}
	if f.shardCount() != sixDrives {
		t.Errorf("%d shards, want only those of the version with an ID (%d)", f.shardCount(), sixDrives)
	}
}

// TestObjectLockUseCases: who may lock, who may lift, and that nobody lifts
// compliance.
func TestObjectLockUseCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	f.meta.Now = f.clock.Now
	root := porttest.Root()
	if err := f.buckets.CreateWithObjectLock(ctx, root, "vault"); err != nil {
		t.Fatal(err)
	}
	if b, err := f.buckets.ObjectLock(ctx, root, "vault"); err != nil || !b.ObjectLock || b.Versioning != bucket.VersioningEnabled {
		t.Fatalf("ObjectLock of a locked bucket = %+v, %v", b, err)
	}
	if _, err := f.buckets.ObjectLock(ctx, root, "b"); !errors.Is(err, domain.ErrLockNotConfigured) {
		t.Errorf("ObjectLock of a plain bucket = %v", err)
	}
	month := testNow.Add(30 * 24 * time.Hour)
	put := func(p iam.Principal, key string, lock object.Lock) (object.Version, error) {
		return f.objects.Put(ctx, p, app.PutInput{Bucket: "vault", Key: key, Body: bytes.NewReader([]byte(key)), Size: int64(len(key)), Lock: lock})
	}
	compliance := object.Lock{Retention: &object.Retention{Mode: object.Compliance, Until: month}}
	governance := object.Lock{Retention: &object.Retention{Mode: object.Governance, Until: month}}
	policy := func(actions string) iam.Principal {
		return iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{
			mustPolicyDoc(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[`+actions+`],"Resource":"*"}]}`),
		}}
	}
	writer := userWith(iam.RoleReadWrite)
	admin := userWith(iam.RoleClusterAdmin)
	bypasser := policy(`"s3:*","s3:BypassGovernanceRetention"`)

	ledger, err := put(root, "ledger", compliance)
	if err != nil || ledger.Lock.Retention == nil {
		t.Fatalf("put with compliance retention = %+v, %v", ledger.Lock, err)
	}
	draft, err := put(root, "draft", governance)
	if err != nil {
		t.Fatal(err)
	}
	shards := f.shardCount()

	// Locking is a permission on top of writing.
	if _, err := put(writer, "x", governance); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("read-write putting a locked version = %v, want %v", err, domain.ErrAccessDenied)
	}
	if _, err := put(writer, "x", object.Lock{LegalHold: true}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("read-write putting a version under legal hold = %v", err)
	}
	if _, err := put(writer, "plain", object.Lock{}); err != nil {
		t.Errorf("read-write putting a plain version into a locked bucket: %v", err)
	}
	if f.shardCount() != shards+sixDrives {
		t.Errorf("refused puts left shards behind: %d, want %d", f.shardCount(), shards+sixDrives)
	}

	ref := func(v object.Version) *object.VersionRef { r := v.Ref(); return &r }
	remove := func(p iam.Principal, v object.Version, bypass bool) error {
		_, err := f.objects.Delete(ctx, p, app.DeleteInput{Bucket: "vault", Key: v.Key, Version: ref(v), BypassGovernance: bypass})
		return err
	}
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"root removes a compliance-locked version", remove(root, ledger, false), domain.ErrObjectLocked},
		{"root removes it with bypass", remove(root, ledger, true), domain.ErrObjectLocked},
		{"cluster-admin removes it with bypass", remove(admin, ledger, true), domain.ErrAccessDenied},
		{"a holder of the bypass removes it", remove(bypasser, ledger, true), domain.ErrObjectLocked},
		{"cluster-admin removes a governance-locked version", remove(admin, draft, false), domain.ErrObjectLocked},
		{"cluster-admin asks to bypass", remove(admin, draft, true), domain.ErrAccessDenied},
		{"a holder of the bypass who does not ask", remove(bypasser, draft, false), domain.ErrObjectLocked},
	}
	for _, tt := range tests {
		if !errors.Is(tt.err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, tt.err, tt.want)
		}
	}
	if f.shardCount() != shards+sixDrives {
		t.Fatalf("a refused delete removed shards: %d", f.shardCount())
	}
	for _, v := range []object.Version{ledger, draft} {
		if got, err := f.objects.Get(ctx, root, app.GetInput{Bucket: "vault", Key: v.Key, Version: ref(v)}); err != nil {
			t.Fatalf("locked version %s after the attempts: %v", v.Key, err)
		} else {
			_ = got.Body.Close()
		}
	}

	// Retention: extended by anybody who may set it, shortened by nobody
	// under compliance, and under governance only with the bypass.
	in := func(v object.Version) app.LockInput {
		return app.LockInput{Bucket: "vault", Key: v.Key, Version: ref(v)}
	}
	shorter := &object.Retention{Mode: object.Governance, Until: testNow.Add(time.Hour)}
	longer := &object.Retention{Mode: object.Compliance, Until: month.Add(24 * time.Hour)}
	retentions := []struct {
		name string
		do   func() error
		want error
	}{
		{"compliance extended", func() error { _, err := f.objects.SetRetention(ctx, root, in(ledger), longer, false); return err }, nil},
		{"compliance shortened by root", func() error {
			_, err := f.objects.SetRetention(ctx, root, in(ledger), &object.Retention{Mode: object.Compliance, Until: month}, true)
			return err
		}, domain.ErrObjectLocked},
		{"compliance removed by root", func() error { _, err := f.objects.SetRetention(ctx, root, in(ledger), nil, true); return err }, domain.ErrObjectLocked},
		{"governance shortened without bypass", func() error { _, err := f.objects.SetRetention(ctx, bypasser, in(draft), shorter, false); return err }, domain.ErrObjectLocked},
		{"governance shortened by cluster-admin asking to bypass", func() error { _, err := f.objects.SetRetention(ctx, admin, in(draft), shorter, true); return err }, domain.ErrAccessDenied},
		{"governance shortened with the bypass", func() error { _, err := f.objects.SetRetention(ctx, bypasser, in(draft), shorter, true); return err }, nil},
		{"retention set by read-write", func() error { _, err := f.objects.SetRetention(ctx, writer, in(draft), longer, false); return err }, domain.ErrAccessDenied},
		{"retention read by read-write", func() error { _, err := f.objects.Retention(ctx, writer, in(draft)); return err }, domain.ErrAccessDenied},
		{"legal hold set by read-write", func() error { _, err := f.objects.SetLegalHold(ctx, writer, in(draft), true); return err }, domain.ErrAccessDenied},
		{"retention of a version that has none", func() error {
			_, err := f.objects.Retention(ctx, root, app.LockInput{Bucket: "vault", Key: "plain"})
			return err
		}, domain.ErrLockNotConfigured},
		{"retention in a bucket without Object Lock", func() error {
			_, err := f.objects.SetRetention(ctx, root, app.LockInput{Bucket: "b", Key: "k"}, longer, false)
			return err
		}, domain.ErrLockNotConfigured},
		{"a lock on a put into a bucket without", func() error {
			_, err := f.objects.Put(ctx, root, app.PutInput{Bucket: "b", Key: "k", Body: bytes.NewReader(nil), Lock: compliance})
			return err
		}, domain.ErrLockNotConfigured},
	}
	for _, tt := range retentions {
		if err := tt.do(); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}
	if got, err := f.objects.Retention(ctx, root, in(ledger)); err != nil || got.Mode != object.Compliance || !got.Until.Equal(longer.Until) {
		t.Errorf("the compliance retention at the end = %+v, %v", got, err)
	}

	// Legal hold keeps a version whose retention has ended.
	if _, err := f.objects.SetLegalHold(ctx, root, in(draft), true); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Hour) // the shortened governance retention has ended
	if err := remove(root, draft, true); !errors.Is(err, domain.ErrObjectLocked) {
		t.Errorf("removing a version under legal hold = %v", err)
	}
	if on, err := f.objects.LegalHold(ctx, root, in(draft)); err != nil || !on {
		t.Errorf("LegalHold = %v, %v", on, err)
	}
	if _, err := f.objects.SetLegalHold(ctx, root, in(draft), false); err != nil {
		t.Fatal(err)
	}
	if err := remove(root, draft, false); err != nil {
		t.Errorf("removing a version whose retention has ended and whose hold is off: %v", err)
	}
	// Time passes the compliance date: then, and only then, it can go.
	f.clock.Advance(40 * 24 * time.Hour)
	if err := remove(writer, ledger, false); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("read-write removing a version = %v", err)
	}
	if err := remove(root, ledger, false); err != nil {
		t.Errorf("removing a version whose compliance retention has ended: %v", err)
	}

	// The default retention of the bucket, and who may set it.
	rule := &bucket.DefaultRetention{Mode: object.Compliance, Days: 7}
	if err := f.buckets.SetDefaultRetention(ctx, writer, "vault", rule); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("read-write setting a default retention = %v", err)
	}
	if err := f.buckets.SetDefaultRetention(ctx, root, "vault", &bucket.DefaultRetention{Mode: object.Compliance}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a default retention without a period = %v", err)
	}
	if err := f.buckets.SetDefaultRetention(ctx, root, "b", rule); !errors.Is(err, domain.ErrLockNotConfigured) {
		t.Errorf("a default retention on a plain bucket = %v", err)
	}
	if err := f.buckets.SetDefaultRetention(ctx, root, "vault", rule); err != nil {
		t.Fatal(err)
	}
	// Even a caller who may not lock gets the bucket's lock on what they put.
	defaulted, err := put(writer, "defaulted", object.Lock{})
	if err != nil || defaulted.Lock.Retention == nil || defaulted.Lock.Retention.Mode != object.Compliance ||
		!defaulted.Lock.Retention.Until.Equal(f.clock.Now().Add(7*24*time.Hour)) {
		t.Fatalf("a put into a bucket with a default retention = %+v, %v", defaulted.Lock, err)
	}
	if err := remove(root, defaulted, true); !errors.Is(err, domain.ErrObjectLocked) {
		t.Errorf("removing a version locked by the bucket's default = %v", err)
	}
	if err := f.buckets.SetVersioning(ctx, root, "vault", bucket.VersioningSuspended); !errors.Is(err, domain.ErrInvalidBucketState) {
		t.Errorf("suspending versioning on a locked bucket = %v", err)
	}
	if err := f.buckets.Delete(ctx, root, "vault"); !errors.Is(err, domain.ErrBucketNotEmpty) {
		t.Errorf("deleting a bucket that holds a locked version = %v", err)
	}
}

// TestStoredDataIsRemovedOnlyWhenNothingRefersToIt: before the shards of a
// removed version are deleted, the index is asked whether anything still
// refers to their data. When it says yes, or cannot say, they stay.
func TestStoredDataIsRemovedOnlyWhenNothingRefersToIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, index := range map[string]func(f *fixture) *referencedIndex{
		"still referred to":    func(f *fixture) *referencedIndex { return &referencedIndex{ObjectIndex: f.meta, referenced: true} },
		"the index cannot say": func(f *fixture) *referencedIndex { return &referencedIndex{ObjectIndex: f.meta, err: errBoom} },
	} {
		f := erasureFixture(t)
		objects := f.objectServiceOver(t, index(f))
		put := func(content string) {
			t.Helper()
			if _, err := objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: bytes.NewReader([]byte(content)), Size: int64(len(content))}); err != nil {
				t.Fatal(err)
			}
		}
		put("first")
		put("second") // removes the first version for good
		if _, err := objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "k"}); err != nil {
			t.Fatal(err)
		}
		if f.shardCount() != 2*sixDrives {
			t.Errorf("%s: %d shards left, want those of both versions (%d)", name, f.shardCount(), 2*sixDrives)
		}
	}
}

// referencedIndex answers the question about stored data as a test says.
type referencedIndex struct {
	ports.ObjectIndex
	referenced bool
	err        error
}

func (i *referencedIndex) IsDataReferenced(context.Context, object.ID) (bool, error) {
	return i.referenced, i.err
}
