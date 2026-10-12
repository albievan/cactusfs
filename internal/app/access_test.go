package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
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

// spyAccess is the record of reads with a way to look at, fail and hold up
// what is handed to it.
type spyAccess struct {
	ports.ObjectAccessStore

	mu      sync.Mutex
	batches []ports.ReadBatch
	// before and after, when set, are called around a batch with its
	// number among those received, counted from one. An error of before
	// refuses the batch; an error of after is returned although the batch
	// was recorded - an outcome the sender cannot know.
	before func(n int) error
	after  func(n int) error
}

func (s *spyAccess) AddObjectReads(ctx context.Context, batch ports.ReadBatch) error {
	s.mu.Lock()
	batch.Items = append([]ports.ObjectRead(nil), batch.Items...)
	s.batches = append(s.batches, batch)
	n, before, after := len(s.batches), s.before, s.after
	s.mu.Unlock()
	if before != nil {
		if err := before(n); err != nil {
			return err
		}
	}
	if err := s.ObjectAccessStore.AddObjectReads(ctx, batch); err != nil {
		return err
	}
	if after != nil {
		return after(n)
	}
	return nil
}

func (s *spyAccess) received() []ports.ReadBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ports.ReadBatch(nil), s.batches...)
}

// dropCount adds up what a recorder says it did not count.
type dropCount struct {
	mu    sync.Mutex
	reads int64
}

func (d *dropCount) add(reads int64) {
	d.mu.Lock()
	d.reads += reads
	d.mu.Unlock()
}

func (d *dropCount) total() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

// accessFixture is a fixture whose object service counts reads.
type accessFixture struct {
	*fixture
	store    *spyAccess
	recorder *app.AccessRecorder
	dropped  *dropCount
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	f := &accessFixture{fixture: erasureFixture(t), dropped: &dropCount{}}
	f.meta.Now = f.clock.Now
	f.store = &spyAccess{ObjectAccessStore: f.meta}
	f.recorder = f.newRecorder(t, nil)
	f.objects = f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) {
		cfg.Access, cfg.AccessStore = f.recorder, f.meta
	})
	return f
}

func (f *accessFixture) newRecorder(t *testing.T, change func(cfg *app.AccessRecorderConfig)) *app.AccessRecorder {
	t.Helper()
	cfg := app.AccessRecorderConfig{
		Store: f.store, Node: "node-a", Entropy: porttest.Entropy(), Logger: slog.New(slog.NewJSONHandler(f.logs, nil)),
		OnDrop: f.dropped.add,
	}
	if change != nil {
		change(&cfg)
	}
	recorder, err := app.NewAccessRecorder(cfg)
	if err != nil {
		t.Fatalf("NewAccessRecorder: %v", err)
	}
	return recorder
}

// flush hands the counts over and fails the test when that fails.
func (f *accessFixture) flush(t *testing.T) {
	t.Helper()
	if err := f.recorder.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// recorded returns what the store knows of the reading of an object.
func (f *accessFixture) recorded(t *testing.T, bucketName, key string) object.Access {
	t.Helper()
	access, err := f.meta.ObjectAccess(context.Background(), bucketName, key, object.VersionRef{Null: true})
	if err != nil {
		t.Fatalf("ObjectAccess(%s/%s): %v", bucketName, key, err)
	}
	return access
}

// read reads an object to its end as p and closes it.
func (f *accessFixture) read(p iam.Principal, in app.GetInput) error {
	out, err := f.objects.Get(context.Background(), p, in)
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, out.Body); err != nil {
		_ = out.Body.Close()
		return err
	}
	return out.Body.Close()
}

// download is read, and fails the test when it fails.
func (f *accessFixture) download(t *testing.T, p iam.Principal, in app.GetInput) {
	t.Helper()
	if err := f.read(p, in); err != nil {
		t.Fatalf("download of %s/%s: %v", in.Bucket, in.Key, err)
	}
}

func userNamed(name string, actions ...string) iam.Principal {
	p := allowing(actions...)
	p.AccessKeyID, p.User = "KEY"+name, name
	return p
}

// withoutUploader is the index and the uploads of a deployment in which a
// member does not know entry schema 3 yet: who made a version is not kept,
// and the write is made all the same (ADR-0016).
type withoutUploader struct {
	ports.ObjectIndex
	ports.UploadStore
}

func (w withoutUploader) PutVersion(ctx context.Context, v object.Version) (ports.PutResult, error) {
	v.CreatedBy = ""
	return w.ObjectIndex.PutVersion(ctx, v)
}

func (w withoutUploader) CompleteUpload(ctx context.Context, c ports.Completion) (ports.PutResult, error) {
	c.CreatedBy = ""
	return w.UploadStore.CompleteUpload(ctx, c)
}

// TestUploaderIsRecorded: every way a version is committed says who did it,
// in the words of the audit trail, and what is answered is what was stored.
func TestUploaderIsRecorded(t *testing.T) {
	t.Parallel()
	data := []byte("who wrote this")
	put := func(f *fixture, p iam.Principal, key string) (object.Version, error) {
		return f.objects.Put(context.Background(), p, app.PutInput{Bucket: "b", Key: key, Body: bytes.NewReader(data), Size: int64(len(data))})
	}
	ways := map[string]func(f *fixture, p iam.Principal) (object.Version, error){
		"put": func(f *fixture, p iam.Principal) (object.Version, error) { return put(f, p, "k") },
		"copy": func(f *fixture, p iam.Principal) (object.Version, error) {
			// The source is somebody else's: the copy is the copier's.
			if _, err := put(f, porttest.Root(), "source"); err != nil {
				return object.Version{}, err
			}
			out, err := f.objects.Copy(context.Background(), p, app.CopyInput{
				Source: app.GetInput{Bucket: "b", Key: "source"}, Destination: app.PutInput{Bucket: "b", Key: "k"}, Digest: tagged,
			})
			return out.Version, err
		},
		"complete": func(f *fixture, p iam.Principal) (object.Version, error) {
			// Begun and filled by root, completed by p: p made the version.
			u := f.createUpload(t, "k")
			part := f.uploadPart(t, u, 1, data)
			return f.objects.CompleteUpload(context.Background(), p, app.CompleteUploadInput{
				Bucket: "b", Key: "k", UploadID: u.ID, Parts: listed(part), ETag: "etag-of-the-parts-1",
			})
		},
	}
	principals := map[string]struct {
		principal iam.Principal
		want      string
	}{
		"root":   {porttest.Root(), "root"},
		"a user": {userNamed("alice", "s3:*"), "user/alice"},
	}
	for way, do := range ways {
		for who, tt := range principals {
			for _, level := range []struct {
				name string
				kept bool
			}{{"schema 3", true}, {"below schema 3", false}} {
				t.Run(way+" by "+who+", "+level.name, func(t *testing.T) {
					t.Parallel()
					f := erasureFixture(t)
					want := tt.want
					if !level.kept {
						want = ""
						f.objects = f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) {
							below := withoutUploader{ObjectIndex: f.meta, UploadStore: f.meta}
							cfg.Index, cfg.Uploads = below, below
						})
					}
					version, err := do(f, tt.principal)
					if err != nil {
						t.Fatalf("the write failed: %v", err)
					}
					if version.CreatedBy != want {
						t.Errorf("the answer says the version was made by %q, want %q", version.CreatedBy, want)
					}
					stored, err := f.meta.GetObject(context.Background(), "b", "k")
					if err != nil {
						t.Fatal(err)
					}
					if stored.CreatedBy != want {
						t.Errorf("the stored version was made by %q, want %q", stored.CreatedBy, want)
					}
					if got, _, err := f.get("b", "k", nil); err != nil || !bytes.Equal(got, data) {
						t.Errorf("the object reads %q, %v", got, err)
					}
				})
			}
		}
	}
}

// TestWhatCountsAsARead goes through ADR-0016 row by row: what is a
// download, what is a copy, and what is neither.
func TestWhatCountsAsARead(t *testing.T) {
	t.Parallel()
	alice := userNamed("alice", "s3:*")
	get := app.GetInput{Bucket: "b", Key: "k"}
	type counts struct{ downloads, copies int64 }
	tests := []struct {
		name string
		do   func(t *testing.T, f *accessFixture, v object.Version)
		want counts
	}{
		{"a GET of the whole object", func(t *testing.T, f *accessFixture, _ object.Version) {
			f.download(t, alice, get)
		}, counts{downloads: 1}},
		{"each ranged GET by itself", func(t *testing.T, f *accessFixture, _ object.Version) {
			for _, r := range []object.RangeSpec{{First: 0, Last: 9}, {First: 10, Last: 19}, {Suffix: 5}} {
				in := get
				in.Range = &r
				f.download(t, alice, in)
			}
		}, counts{downloads: 3}},
		{"a transfer that was cut short", func(t *testing.T, f *accessFixture, _ object.Version) {
			out, err := f.objects.Get(context.Background(), alice, get)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(out.Body, make([]byte, 3)); err != nil {
				t.Fatal(err)
			}
			_ = out.Body.Close()
		}, counts{downloads: 1}},
		{"a body that is closed twice", func(t *testing.T, f *accessFixture, _ object.Version) {
			out, err := f.objects.Get(context.Background(), alice, get)
			if err != nil {
				t.Fatal(err)
			}
			_ = out.Body.Close()
			_ = out.Body.Close()
		}, counts{downloads: 1}},
		{"a body that is never closed", func(t *testing.T, f *accessFixture, _ object.Version) {
			if _, err := f.objects.Get(context.Background(), alice, get); err != nil {
				t.Fatal(err)
			}
		}, counts{}},
		{"a GET of a named version", func(t *testing.T, f *accessFixture, _ object.Version) {
			in := get
			in.Version = &object.VersionRef{Null: true}
			f.download(t, alice, in)
		}, counts{downloads: 1}},
		{"HEAD", func(t *testing.T, f *accessFixture, _ object.Version) {
			if _, err := f.objects.Stat(context.Background(), alice, get); err != nil {
				t.Fatal(err)
			}
			if _, err := f.objects.Head(context.Background(), alice, "b", "k"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.objects.HeadVersion(context.Background(), alice, "b", "k", object.VersionRef{Null: true}); err != nil {
				t.Fatal(err)
			}
		}, counts{}},
		{"a GET answered 304", func(t *testing.T, f *accessFixture, v object.Version) {
			in := get
			in.Conditions = object.Conditions{IfNoneMatch: []string{v.ETag}}
			if _, err := f.objects.Get(context.Background(), alice, in); !errors.Is(err, domain.ErrNotModified) {
				t.Fatalf("Get = %v, want not modified", err)
			}
		}, counts{}},
		{"a GET answered 412", func(t *testing.T, f *accessFixture, _ object.Version) {
			in := get
			in.Conditions = object.Conditions{IfMatch: []string{"another"}}
			if _, err := f.objects.Get(context.Background(), alice, in); !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("Get = %v, want precondition failed", err)
			}
		}, counts{}},
		{"a GET of a range there is not", func(t *testing.T, f *accessFixture, _ object.Version) {
			in := get
			in.Range = &object.RangeSpec{First: 1 << 30, Last: -1}
			if _, err := f.objects.Get(context.Background(), alice, in); !errors.Is(err, domain.ErrInvalidRange) {
				t.Fatalf("Get = %v, want invalid range", err)
			}
		}, counts{}},
		{"a GET that is refused", func(t *testing.T, f *accessFixture, _ object.Version) {
			if _, err := f.objects.Get(context.Background(), userNamed("mallory", "s3:ListBucket"), get); !errors.Is(err, domain.ErrAccessDenied) {
				t.Fatalf("Get = %v, want access denied", err)
			}
		}, counts{}},
		{"a GET of data that cannot be opened", func(t *testing.T, f *accessFixture, v object.Version) {
			for i, drive := range f.shardsOf(t, v) {
				_ = drive.Delete(context.Background(), shardID(v, i))
			}
			if _, err := f.objects.Get(context.Background(), alice, get); err == nil {
				t.Fatal("an object without shards was opened")
			}
		}, counts{}},
		{"a degraded GET, healed afterwards", func(t *testing.T, f *accessFixture, v object.Version) {
			_ = f.shardsOf(t, v)[0].Delete(context.Background(), shardID(v, 0))
			f.download(t, alice, get)
			// The healer reads every remaining shard to rebuild the lost one.
			f.runHealer(t, func() bool { return len(f.heals.list()) > 0 })
		}, counts{downloads: 1}},
		{"the source of a copy, once the copy is committed", func(t *testing.T, f *accessFixture, _ object.Version) {
			if _, err := f.objects.Copy(context.Background(), alice, app.CopyInput{Source: get, Destination: app.PutInput{Bucket: "b", Key: "copy"}, Digest: tagged}); err != nil {
				t.Fatal(err)
			}
		}, counts{copies: 1}},
		{"the source of a copy that was not committed", func(t *testing.T, f *accessFixture, _ object.Version) {
			_, err := f.objects.Copy(context.Background(), alice, app.CopyInput{Source: get, Destination: app.PutInput{Bucket: "no-such-bucket", Key: "copy"}, Digest: tagged})
			if !errors.Is(err, domain.ErrBucketNotFound) {
				t.Fatalf("Copy = %v, want no such bucket", err)
			}
		}, counts{}},
		{"the source of a copied part, once the part is committed", func(t *testing.T, f *accessFixture, _ object.Version) {
			u := f.createUpload(t, "assembled")
			_, err := f.objects.CopyPart(context.Background(), alice, app.CopyPartInput{
				Source: get, Part: app.UploadPartInput{Bucket: "b", Key: "assembled", UploadID: u.ID, Number: 1}, Digest: tagged,
			})
			if err != nil {
				t.Fatal(err)
			}
		}, counts{copies: 1}},
		{"the source of a copied part that was not committed", func(t *testing.T, f *accessFixture, _ object.Version) {
			_, err := f.objects.CopyPart(context.Background(), alice, app.CopyPartInput{
				Source: get, Part: app.UploadPartInput{Bucket: "b", Key: "assembled", UploadID: object.ID{7}, Number: 1}, Digest: tagged,
			})
			if !errors.Is(err, domain.ErrUploadNotFound) {
				t.Fatalf("CopyPart = %v, want no such upload", err)
			}
		}, counts{}},
		{"healing", func(t *testing.T, f *accessFixture, v object.Version) {
			f.shardsOf(t, v)[1].Damage(shardID(v, 1))
			if result, err := f.objects.Healer().HealObject(context.Background(), "b", "k"); err != nil || result.Rebuilt != 1 {
				t.Fatalf("HealObject = %+v, %v", result, err)
			}
		}, counts{}},
		{"scrubbing", func(t *testing.T, f *accessFixture, v object.Version) {
			f.shardsOf(t, v)[2].Damage(shardID(v, 2))
			scrubber := app.NewScrubber(app.ScrubberConfig{
				Buckets: f.meta, Index: f.meta, Uploads: f.meta, Healer: f.objects.Healer(), Logger: slog.New(slog.DiscardHandler),
			})
			if stats, err := scrubber.RunOnce(context.Background()); err != nil || stats.Objects != 1 || stats.ShardsRebuilt != 1 {
				t.Fatalf("RunOnce = %+v, %v", stats, err)
			}
		}, counts{}},
		{"collection", func(t *testing.T, f *accessFixture, _ object.Version) {
			collector := app.NewCollector(app.CollectorConfig{
				Drives: asStores(f.drives), Index: f.meta, Clock: f.clock, Grace: time.Hour, Logger: slog.New(slog.DiscardHandler),
				InFlight: func(context.Context, object.ID) (bool, error) { return f.objects.InFlight(object.ID{}), nil },
			})
			if stats, err := collector.RunOnce(context.Background()); err != nil || stats.Shards == 0 {
				t.Fatalf("RunOnce = %+v, %v", stats, err)
			}
		}, counts{}},
		{"the lifecycle worker", func(t *testing.T, f *accessFixture, _ object.Version) {
			// A rule that expires nothing yet: the worker looks at every
			// version of the bucket and removes none.
			f.setRules(t, "b", bucket.LifecycleRule{ID: "later", Enabled: true, ExpirationDays: 30})
			if stats, err := f.lifecycleWorker(nil).RunOnce(context.Background()); err != nil || stats.Expired != 0 {
				t.Fatalf("RunOnce = %+v, %v", stats, err)
			}
		}, counts{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAccessFixture(t)
			v := f.put(t, "b", "k", randomBytes(t, 4000))
			tt.do(t, f, v)
			f.flush(t)
			got := f.recorded(t, "b", "k")
			if got.Downloads != tt.want.downloads || got.Copies != tt.want.copies {
				t.Fatalf("recorded %d downloads and %d copies, want %d and %d", got.Downloads, got.Copies, tt.want.downloads, tt.want.copies)
			}
			if tt.want == (counts{}) {
				if len(f.store.received()) != 0 {
					t.Errorf("something was handed to the store: %+v", f.store.received())
				}
				return
			}
			if len(got.Readers) != 1 || got.Readers[0].Who != "user/alice" || got.Readers[0].Presigned ||
				got.Readers[0].Downloads != tt.want.downloads || got.Readers[0].Copies != tt.want.copies {
				t.Errorf("readers = %+v", got.Readers)
			}
			// Nothing but the object asked about was credited with a read.
			for _, batch := range f.store.received() {
				for _, item := range batch.Items {
					if item.Key != "k" || item.VersionID != v.VersionID || !item.Version.Null {
						t.Errorf("a read of %s version %s (%s) was recorded", item.Key, item.Version, item.VersionID)
					}
				}
			}
			if f.dropped.total() != 0 {
				t.Errorf("%d reads were dropped", f.dropped.total())
			}
		})
	}
}

// TestWhoIsTheReader: the reader is the actor as the audit trail names it;
// nobody in particular is "anonymous"; a presigned URL is its signer's, kept
// apart from the signer's own reads; a node or a service is never a reader.
func TestWhoIsTheReader(t *testing.T) {
	t.Parallel()
	presigned := func(p iam.Principal) iam.Principal {
		p.Session.Presigned = true
		return p
	}
	alice := userNamed("alice", "s3:*")
	tests := []struct {
		name      string
		principal iam.Principal
		who       string
		presigned bool
	}{
		{"root", porttest.Root(), "root", false},
		{"a user", alice, "user/alice", false},
		{"a bare key", iam.Principal{AccessKeyID: "AKID"}, "key/AKID", false},
		{"nobody", anonymous(), "anonymous", false},
		{"nobody, whatever the session says", presigned(anonymous()), "anonymous", false},
		{"the holder of a URL a user signed", presigned(alice), "user/alice", true},
		{"the holder of a URL root signed", presigned(porttest.Root()), "root", true},
		{"a node", iam.Principal{NodeID: "node-b"}, "", false},
		{"a service", iam.Principal{Service: iam.ServiceLifecycle}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAccessFixture(t)
			v := f.put(t, "b", "k", []byte("content"))
			f.recorder.Download(tt.principal, v)
			f.recorder.Copy(tt.principal, v)
			f.flush(t)
			got := f.recorded(t, "b", "k")
			if tt.who == "" {
				if !got.Empty() || len(f.store.received()) != 0 {
					t.Fatalf("a read was recorded: %+v", got)
				}
				return
			}
			want := object.Reader{Who: tt.who, Presigned: tt.presigned, Downloads: 1, Copies: 1, LastAt: testNow}
			if len(got.Readers) != 1 || got.Readers[0] != want {
				t.Errorf("readers = %+v, want %+v", got.Readers, want)
			}
		})
	}

	// A signer's own reads and those of whoever held its URLs are two rows.
	f := newAccessFixture(t)
	v := f.put(t, "b", "k", []byte("content"))
	f.recorder.Download(alice, v)
	f.recorder.Download(presigned(alice), v)
	f.recorder.Download(presigned(alice), v)
	// A delete marker has no content to read, and a version without an ID
	// is none.
	f.recorder.Download(alice, object.Version{Bucket: "b", Key: "k", VersionID: v.VersionID, DeleteMarker: true})
	f.recorder.Download(alice, object.Version{Bucket: "b", Key: "k"})
	f.flush(t)
	got := f.recorded(t, "b", "k")
	if got.Downloads != 3 || len(got.Readers) != 2 || got.Readers[0].Presigned || got.Readers[0].Downloads != 1 ||
		!got.Readers[1].Presigned || got.Readers[1].Downloads != 2 {
		t.Errorf("access = %+v", got)
	}
	// A nil recorder counts nothing and does not mind.
	var none *app.AccessRecorder
	none.Download(alice, v)
	none.Copy(alice, v)
}

// TestAReadNeverWaitsForItsRecord: a GET succeeds, and is counted, while the
// store refuses what is handed to it and while a flush is stuck in it.
func TestAReadNeverWaitsForItsRecord(t *testing.T) {
	t.Parallel()
	alice := userNamed("alice", "s3:*")
	get := app.GetInput{Bucket: "b", Key: "k"}

	t.Run("the store fails", func(t *testing.T) {
		t.Parallel()
		f := newAccessFixture(t)
		f.put(t, "b", "k", []byte("content"))
		noLeader := fmt.Errorf("%w: the metadata group has no leader", domain.ErrNotLeader)
		f.store.before = func(int) error { return noLeader }
		for range 3 {
			f.download(t, alice, get)
			if err := f.recorder.Flush(context.Background()); !errors.Is(err, domain.ErrNotLeader) {
				t.Fatalf("Flush = %v, want the store's error", err)
			}
		}
		// The batch that is sent again, and what was counted since.
		if f.recorder.Buffered() != 2 || !f.recorded(t, "b", "k").Empty() {
			t.Fatalf("%d aggregates wait, recorded %+v", f.recorder.Buffered(), f.recorded(t, "b", "k"))
		}
		// With a leader again, everything that waited is recorded.
		f.store.before = nil
		f.flush(t)
		if got := f.recorded(t, "b", "k"); got.Downloads != 3 || f.recorder.Buffered() != 0 || f.dropped.total() != 0 {
			t.Errorf("recorded %+v, %d waiting, %d dropped", got, f.recorder.Buffered(), f.dropped.total())
		}
	})

	t.Run("the store blocks", func(t *testing.T) {
		t.Parallel()
		f := newAccessFixture(t)
		f.put(t, "b", "k", []byte("content"))
		entered, release := make(chan struct{}), make(chan struct{})
		f.store.before = func(n int) error {
			if n == 1 {
				close(entered)
				<-release
			}
			return nil
		}
		f.download(t, alice, get)
		flushed := make(chan error, 1)
		go func() { flushed <- f.recorder.Flush(context.Background()) }()
		<-entered

		// The flush is inside the store. Reads go on, and are counted.
		done := make(chan error, 1)
		go func() {
			for range 5 {
				if err := f.read(alice, get); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("a GET failed while a flush was in the store: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a GET waited for the record of reads")
		}
		if f.recorder.Buffered() != 2 {
			t.Errorf("%d aggregates held, want the one on its way and the one counted since", f.recorder.Buffered())
		}
		close(release)
		if err := <-flushed; err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if got := f.recorded(t, "b", "k"); got.Downloads != 6 {
			t.Errorf("recorded %d downloads, want 6", got.Downloads)
		}
	})

	t.Run("no store can be asked yet", func(t *testing.T) {
		t.Parallel()
		f := newAccessFixture(t)
		f.put(t, "b", "k", []byte("content"))
		starting := errors.New("the node is starting")
		ready := starting
		recorder := f.newRecorder(t, func(cfg *app.AccessRecorderConfig) {
			cfg.Ready = func(context.Context) error { return ready }
		})
		f.objects = f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.Access = recorder })
		f.download(t, alice, get)
		if err := recorder.Flush(context.Background()); !errors.Is(err, starting) || len(f.store.received()) != 0 || recorder.Buffered() != 1 {
			t.Fatalf("Flush = %v, %d batches sent, %d waiting", err, len(f.store.received()), recorder.Buffered())
		}
		ready = nil
		if err := recorder.Flush(context.Background()); err != nil || f.recorded(t, "b", "k").Downloads != 1 {
			t.Errorf("Flush once ready = %v, recorded %+v", err, f.recorded(t, "b", "k"))
		}
	})
}

// manyVersions stores n objects and returns their versions.
func (f *accessFixture) manyVersions(t *testing.T, n int) []object.Version {
	t.Helper()
	versions := make([]object.Version, n)
	for i := range versions {
		v := porttest.SampleVersion("b", fmt.Sprintf("key-%05d", i), 1)
		stored, err := f.meta.PutVersion(context.Background(), v)
		if err != nil {
			t.Fatal(err)
		}
		versions[i] = stored.Stored
	}
	return versions
}

// TestTheBufferOfReadsIsBounded: a node holds at most MaxBufferedReads
// aggregates the store has not taken. A read that would need another is
// dropped and counted; one of an aggregate it holds still counts.
func TestTheBufferOfReadsIsBounded(t *testing.T) {
	t.Parallel()
	f := newAccessFixture(t)
	alice, bob := userNamed("alice", "s3:*"), userNamed("bob", "s3:*")
	// Versions that were read and are gone by the time the store is told:
	// what is held is bounded whatever became of them.
	versions := make([]object.Version, app.MaxBufferedReads+1)
	for i := range versions {
		versions[i] = object.Version{Bucket: "b", Key: fmt.Sprintf("key-%05d", i), VersionID: object.ID{1, byte(i), byte(i >> 8)}}
	}
	unavailable := fmt.Errorf("%w: a member does not know such records", domain.ErrUnavailable)
	f.store.before = func(int) error { return unavailable }

	for _, v := range versions[:app.MaxBufferedReads] {
		f.recorder.Download(alice, v)
	}
	if f.recorder.Buffered() != app.MaxBufferedReads || f.dropped.total() != 0 {
		t.Fatalf("%d aggregates held, %d reads dropped", f.recorder.Buffered(), f.dropped.total())
	}
	// A batch that is on its way, and waits to be sent again, counts.
	if err := f.recorder.Flush(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Flush = %v", err)
	}
	if f.recorder.Buffered() != app.MaxBufferedReads {
		t.Fatalf("%d aggregates held after a failed flush", f.recorder.Buffered())
	}
	f.recorder.Download(alice, versions[app.MaxBufferedReads]) // another version
	f.recorder.Download(bob, versions[0])                      // another reader
	f.recorder.Copy(bob, versions[0])
	if f.recorder.Buffered() != app.MaxBufferedReads || f.dropped.total() != 3 {
		t.Fatalf("%d aggregates held, %d reads dropped; want the bound and 3", f.recorder.Buffered(), f.dropped.total())
	}

	// Everything that was held arrives, exactly once, when the store takes it.
	f.store.before = nil
	f.flush(t)
	if f.recorder.Buffered() != 0 {
		t.Fatalf("%d aggregates held after a flush", f.recorder.Buffered())
	}
	// The first batch went twice, under one number; every other once.
	var downloads int64
	handed := map[uint64]bool{}
	for _, batch := range f.store.received() {
		if handed[batch.Seq] {
			continue
		}
		handed[batch.Seq] = true
		if len(batch.Items) > ports.MaxReadBatch {
			t.Errorf("a batch of %d items", len(batch.Items))
		}
		for _, item := range batch.Items {
			downloads += item.Read.Downloads
		}
	}
	if downloads != app.MaxBufferedReads || f.dropped.total() != 3 {
		t.Errorf("%d downloads handed over, %d dropped", downloads, f.dropped.total())
	}
	// And there is room again.
	f.recorder.Download(bob, versions[0])
	if f.recorder.Buffered() != 1 || f.dropped.total() != 3 {
		t.Errorf("%d aggregates held, %d dropped", f.recorder.Buffered(), f.dropped.total())
	}
}

// TestBatchesOfReads: what was counted is handed over in batches of at most
// ports.MaxReadBatch, numbered upwards from one under the node's name and the
// number it chose when it started.
func TestBatchesOfReads(t *testing.T) {
	t.Parallel()
	f := newAccessFixture(t)
	alice := userNamed("alice", "s3:*")
	const n = 2*ports.MaxReadBatch + 88
	versions := f.manyVersions(t, n)
	for _, v := range versions {
		f.recorder.Download(alice, v)
		f.recorder.Download(alice, v)
	}
	f.flush(t)

	batches := f.store.received()
	if len(batches) != 3 || len(batches[0].Items) != ports.MaxReadBatch || len(batches[1].Items) != ports.MaxReadBatch || len(batches[2].Items) != 88 {
		t.Fatalf("%d batches", len(batches))
	}
	seen := map[string]bool{}
	var seq uint64
	for i, batch := range batches {
		seq++
		if batch.Node != "node-a" || batch.Boot != batches[0].Boot || batch.Seq != seq {
			t.Errorf("batch %d is {%s %d %d}", i, batch.Node, batch.Boot, batch.Seq)
		}
		for _, item := range batch.Items {
			if seen[item.Key] || item.Read.Downloads != 2 || item.Read.Who != "user/alice" {
				t.Errorf("item %+v", item)
			}
			seen[item.Key] = true
		}
	}
	if len(seen) != n {
		t.Errorf("%d versions in the batches, want %d", len(seen), n)
	}
	// The number of a start is read from the entropy source: another start
	// has another, and begins again at one.
	again := f.newRecorder(t, func(cfg *app.AccessRecorderConfig) {
		entropy := porttest.Entropy()
		_, _ = io.ReadFull(entropy, make([]byte, 8))
		cfg.Entropy = entropy
	})
	again.Download(alice, versions[0])
	if err := again.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := f.store.received()[3]
	if last.Boot == batches[0].Boot || last.Seq != 1 {
		t.Errorf("the first batch of another start is {%d %d}; the one before had %d", last.Boot, last.Seq, batches[0].Boot)
	}
	if got := f.recorded(t, "b", versions[0].Key); got.Downloads != 3 {
		t.Errorf("recorded %d downloads, want 3", got.Downloads)
	}
	// An empty flush sends nothing.
	f.flush(t)
	if len(f.store.received()) != 4 {
		t.Errorf("%d batches after a flush of nothing", len(f.store.received()))
	}
}

// TestAFailedFlushIsRepeatedUnderItsNumber: a batch whose outcome the node
// does not know is sent again as it was, under the same number, and the
// store records it once.
func TestAFailedFlushIsRepeatedUnderItsNumber(t *testing.T) {
	t.Parallel()
	f := newAccessFixture(t)
	alice := userNamed("alice", "s3:*")
	v := f.put(t, "b", "k", []byte("content"))
	f.recorder.Download(alice, v)
	f.recorder.Download(alice, v)

	// Recorded, but the answer is lost; then refused outright; then taken.
	unknown := fmt.Errorf("%w: the leader changed", domain.ErrOutcomeUnknown)
	f.store.after = func(n int) error {
		if n == 1 {
			return unknown
		}
		return nil
	}
	f.store.before = func(n int) error {
		if n == 2 {
			return domain.ErrNotLeader
		}
		return nil
	}
	if err := f.recorder.Flush(context.Background()); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("first Flush = %v", err)
	}
	// Counted meanwhile: not part of the batch that is sent again.
	f.recorder.Download(alice, v)
	f.recorder.Copy(alice, v)
	if err := f.recorder.Flush(context.Background()); !errors.Is(err, domain.ErrNotLeader) {
		t.Fatalf("second Flush = %v", err)
	}
	f.flush(t)

	batches := f.store.received()
	if len(batches) != 4 {
		t.Fatalf("%d batches, want the first three times and the next once", len(batches))
	}
	for i := range 3 {
		b := batches[i]
		if b.Seq != 1 || b.Boot != batches[0].Boot || len(b.Items) != 1 || b.Items[0].Read != (object.Read{Who: "user/alice", Downloads: 2}) {
			t.Errorf("attempt %d = %+v", i+1, b)
		}
	}
	if b := batches[3]; b.Seq != 2 || len(b.Items) != 1 || b.Items[0].Read != (object.Read{Who: "user/alice", Downloads: 1, Copies: 1}) {
		t.Errorf("the next batch = %+v", b)
	}
	if got := f.recorded(t, "b", "k"); got.Downloads != 3 || got.Copies != 1 {
		t.Errorf("recorded %d downloads and %d copies, want 3 and 1", got.Downloads, got.Copies)
	}
	if f.dropped.total() != 0 {
		t.Errorf("%d reads dropped", f.dropped.total())
	}
}

// TestABatchRefusedForGoodIsDropped: a batch the store refuses as what it is
// would be refused every time; it is dropped and counted, and what was
// counted after it is not held up.
func TestABatchRefusedForGoodIsDropped(t *testing.T) {
	t.Parallel()
	f := newAccessFixture(t)
	alice := userNamed("alice", "s3:*")
	v := f.put(t, "b", "k", []byte("content"))
	f.store.before = func(n int) error {
		if n == 1 {
			return fmt.Errorf("%w: a batch of nobody", domain.ErrInvalidArgument)
		}
		return nil
	}
	f.recorder.Download(alice, v)
	f.recorder.Download(alice, v)
	f.flush(t)
	if f.dropped.total() != 2 || f.recorder.Buffered() != 0 {
		t.Fatalf("%d reads dropped, %d aggregates held", f.dropped.total(), f.recorder.Buffered())
	}
	f.recorder.Download(alice, v)
	f.flush(t)
	if got := f.recorded(t, "b", "k"); got.Downloads != 1 || f.store.received()[1].Seq != 2 {
		t.Errorf("recorded %+v; the next batch had number %d", got, f.store.received()[1].Seq)
	}
}

// TestRecorderRuns: Run hands the counts over every interval and once more
// when it is told to stop, so that a node that stops in good order loses
// nothing.
func TestRecorderRuns(t *testing.T) {
	t.Parallel()
	f := newAccessFixture(t)
	alice := userNamed("alice", "s3:*")
	v := f.put(t, "b", "k", []byte("content"))

	// The clock of the interval is the test's.
	tick := make(chan time.Time)
	asked := make(chan time.Duration, 16)
	taken := make(chan int, 16)
	f.store.after = func(n int) error {
		taken <- n
		return nil
	}
	recorder := f.newRecorder(t, func(cfg *app.AccessRecorderConfig) {
		cfg.Tick = func(d time.Duration) <-chan time.Time {
			asked <- d
			return tick
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		recorder.Run(ctx, 45*time.Second)
	}()
	if d := <-asked; d != 45*time.Second {
		t.Errorf("Run waits %s between two flushes, want the interval it was given", d)
	}

	// Nothing is sent before the interval has passed.
	recorder.Download(alice, v)
	select {
	case n := <-taken:
		t.Fatalf("batch %d was sent before the interval had passed", n)
	case <-time.After(20 * time.Millisecond):
	}
	tick <- testNow
	<-taken
	<-asked
	if got := f.recorded(t, "b", "k"); got.Downloads != 1 {
		t.Fatalf("after an interval %d downloads are recorded, want 1", got.Downloads)
	}
	// An interval in which nothing was read sends nothing.
	tick <- testNow
	<-asked
	if len(f.store.received()) != 1 {
		t.Fatalf("%d batches after an idle interval", len(f.store.received()))
	}

	// Counted, and then the node stops before the interval has passed.
	recorder.Download(alice, v)
	recorder.Copy(alice, v)
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
	if got := f.recorded(t, "b", "k"); got.Downloads != 2 || got.Copies != 1 || recorder.Buffered() != 0 {
		t.Errorf("after the stop %d downloads and %d copies are recorded, %d aggregates were left", got.Downloads, got.Copies, recorder.Buffered())
	}

	// A stop while the store cannot take anything ends all the same.
	f.store.before = func(int) error { return domain.ErrNotLeader }
	quiet := f.newRecorder(t, func(cfg *app.AccessRecorderConfig) {
		cfg.Tick = func(time.Duration) <-chan time.Time { return nil }
	})
	quiet.Download(alice, v)
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	quiet.Run(ctx, 0)
	if quiet.Buffered() != 1 {
		t.Errorf("%d aggregates held after a stop without a store", quiet.Buffered())
	}
}

// TestNewAccessRecorder: a recorder needs a store, a node to speak for and a
// number for this start.
func TestNewAccessRecorder(t *testing.T) {
	t.Parallel()
	meta := porttest.NewMemMetadata()
	logger := slog.New(slog.DiscardHandler)
	for name, tt := range map[string]struct {
		cfg  app.AccessRecorderConfig
		want error
	}{
		"no store":   {app.AccessRecorderConfig{Node: "node-a", Entropy: porttest.Entropy(), Logger: logger}, domain.ErrInvalidArgument},
		"no node":    {app.AccessRecorderConfig{Store: meta, Entropy: porttest.Entropy(), Logger: logger}, domain.ErrInvalidArgument},
		"no entropy": {app.AccessRecorderConfig{Store: meta, Node: "node-a", Entropy: bytes.NewReader([]byte{1, 2}), Logger: logger}, io.ErrUnexpectedEOF},
		"everything": {app.AccessRecorderConfig{Store: meta, Node: "node-a", Entropy: porttest.Entropy(), Logger: logger}, nil},
	} {
		recorder, err := app.NewAccessRecorder(tt.cfg)
		if !errors.Is(err, tt.want) || (err == nil) != (recorder != nil) {
			t.Errorf("%s: NewAccessRecorder = %v, %v; want %v", name, recorder, err, tt.want)
		}
	}
}

// TestObjectAccess: who uploaded a version and who read it is told to those
// who hold the permission of its own, asked for before anything else.
func TestObjectAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAccessFixture(t)
	f.createBucket(t, "kept")
	if err := f.meta.SetBucketVersioning(ctx, "kept", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	alice := userNamed("alice", "s3:*")
	if _, err := f.objects.Put(ctx, alice, app.PutInput{Bucket: "b", Key: "k", Body: bytes.NewReader([]byte("v1")), Size: 2}); err != nil {
		t.Fatal(err)
	}
	first, err := f.objects.Put(ctx, alice, app.PutInput{Bucket: "kept", Key: "k", Body: bytes.NewReader([]byte("v1")), Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	if _, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "kept", Key: "k", Body: bytes.NewReader([]byte("v2")), Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.objects.Put(ctx, alice, app.PutInput{Bucket: "kept", Key: "gone", Body: bytes.NewReader([]byte("v1")), Size: 2}); err != nil {
		t.Fatal(err)
	}
	marked, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "kept", Key: "gone"})
	if err != nil || marked.Marker == nil {
		t.Fatalf("Delete = %+v, %v", marked, err)
	}
	f.download(t, alice, app.GetInput{Bucket: "b", Key: "k"})
	f.download(t, porttest.Root(), app.GetInput{Bucket: "b", Key: "k"})
	oldest := first.Ref()
	f.download(t, alice, app.GetInput{Bucket: "kept", Key: "k", Version: &oldest})
	f.flush(t)

	role := func(name string) iam.Principal {
		return iam.Principal{AccessKeyID: "K" + name, User: name, Policies: []iam.Document{iam.BuiltInPolicies()[name]}}
	}
	tests := []struct {
		name      string
		principal iam.Principal
		bucket    string
		key       string
		version   string
		want      error
		// uploader and downloads are what a successful answer says.
		uploader  string
		downloads int64
	}{
		{name: "root", principal: porttest.Root(), bucket: "b", key: "k", uploader: "user/alice", downloads: 2},
		{name: "a cluster admin", principal: role(iam.RoleClusterAdmin), bucket: "b", key: "k", uploader: "user/alice", downloads: 2},
		{name: "an auditor, who cannot read the object", principal: role(iam.RoleAuditor), bucket: "b", key: "k", uploader: "user/alice", downloads: 2},
		{name: "the null version by name", principal: porttest.Root(), bucket: "b", key: "k", version: "null", uploader: "user/alice", downloads: 2},
		{name: "the current version of several", principal: porttest.Root(), bucket: "kept", key: "k", uploader: "root"},
		{name: "an older version", principal: porttest.Root(), bucket: "kept", key: "k", version: oldest.String(), uploader: "user/alice", downloads: 1},
		{name: "a bucket admin, who can read the object", principal: role(iam.RoleBucketAdmin), bucket: "b", key: "k", want: domain.ErrAccessDenied},
		{name: "a read-write user", principal: role(iam.RoleReadWrite), bucket: "b", key: "k", want: domain.ErrAccessDenied},
		{name: "a read-only user", principal: role(iam.RoleReadOnly), bucket: "b", key: "k", want: domain.ErrAccessDenied},
		{name: "who holds s3:GetObject and every other s3 action", principal: alice, bucket: "b", key: "k", want: domain.ErrAccessDenied},
		{name: "nobody", principal: anonymous(), bucket: "b", key: "k", want: domain.ErrAccessDenied},
		{name: "who may not see it, of an object there is not", principal: alice, bucket: "b", key: "nope", want: domain.ErrAccessDenied},
		{name: "who may not see it, of a bucket there is not", principal: alice, bucket: "nope", key: "k", want: domain.ErrAccessDenied},
		{name: "who may not see it, with a version that is none", principal: alice, bucket: "b", key: "k", version: "latest", want: domain.ErrAccessDenied},
		{name: "an object there is not", principal: porttest.Root(), bucket: "b", key: "nope", want: domain.ErrObjectNotFound},
		{name: "a bucket there is not", principal: porttest.Root(), bucket: "nope", key: "k", want: domain.ErrBucketNotFound},
		{name: "a version there is not", principal: porttest.Root(), bucket: "kept", key: "k", version: object.ID{9}.String(), want: domain.ErrVersionNotFound},
		{name: "a version that is none", principal: porttest.Root(), bucket: "b", key: "k", version: "latest", want: domain.ErrInvalidArgument},
		{name: "a key that is none", principal: porttest.Root(), bucket: "b", key: "", want: domain.ErrInvalidObjectKey},
		{name: "an object that was deleted", principal: porttest.Root(), bucket: "kept", key: "gone", want: domain.ErrObjectNotFound},
		{name: "a delete marker", principal: porttest.Root(), bucket: "kept", key: "gone", version: marked.Marker.Ref().String(), want: domain.ErrMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(f.authz.Requests())
			got, err := f.objects.Access(ctx, tt.principal, tt.bucket, tt.key, tt.version)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Access = %v, want %v", err, tt.want)
			}
			// One question to the authorizer, about this action on this object.
			asked := f.authz.Requests()[before:]
			if len(asked) != 1 || asked[0].Action != iam.ActionGetObjectAccess || asked[0].Resource != (iam.Resource{Bucket: tt.bucket, Key: tt.key}) {
				t.Errorf("the authorizer was asked %+v", asked)
			}
			if err != nil {
				if !got.Access.Empty() || got.Version.Key != "" {
					t.Errorf("a refusal came with %+v", got)
				}
				return
			}
			if got.Version.CreatedBy != tt.uploader || got.Access.Downloads != tt.downloads || got.Version.Bucket != tt.bucket || got.Version.Key != tt.key {
				t.Errorf("Access = uploaded by %q, %d downloads; want %q and %d", got.Version.CreatedBy, got.Access.Downloads, tt.uploader, tt.downloads)
			}
		})
	}

	// Without a record of reads on the node there is nothing to say - to
	// those who may ask.
	plain := f.objectServiceWith(t, func(*app.ObjectServiceConfig) {})
	if _, err := plain.Access(ctx, porttest.Root(), "b", "k", ""); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Access without a store = %v", err)
	}
	if _, err := plain.Access(ctx, alice, "b", "k", ""); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Access without a store, for who may not ask = %v", err)
	}
	// The store's failure is the answer's.
	broken := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.AccessStore = failingAccess{} })
	if _, err := broken.Access(ctx, porttest.Root(), "b", "k", ""); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Access over a store that fails = %v", err)
	}
}

// failingAccess is a record of reads that cannot be reached.
type failingAccess struct{}

func (failingAccess) AddObjectReads(context.Context, ports.ReadBatch) error {
	return domain.ErrUnavailable
}

func (failingAccess) ObjectAccess(context.Context, string, string, object.VersionRef) (object.Access, error) {
	return object.Access{}, domain.ErrUnavailable
}
