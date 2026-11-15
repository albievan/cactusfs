package app_test

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// notifyFixture is the notification use cases over the stores of the
// target fixture, with the buckets photos and private, the webhook hook
// (open to photos and logs-*), the NATS server bus (open to photos) and the
// other deployment site-b (open to photos).
type notifyFixture struct {
	*targetFixture
	notify *app.NotificationService
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	ctx := context.Background()
	f := &notifyFixture{targetFixture: newTargetFixture(t)}
	f.notify = app.NewNotificationService(app.NotificationServiceConfig{
		Buckets: f.meta, Targets: f.meta, Authz: f.authz, Auditor: f.auditor,
	})
	for _, name := range []string{"photos", "private"} {
		if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range []app.TargetInput{
		webhook("hook"),
		{
			Name: "bus", Kind: delivery.KindNATS, Endpoint: "tls://nats.example.com", Subject: "cactusfs.events",
			Buckets: []string{"photos"}, Auth: delivery.Auth{Mode: delivery.AuthToken}, Secret: concealedMark,
		},
		{
			Name: "site-b", Kind: delivery.KindS3, Endpoint: "https://s3.site-b.example", Region: "eu-west-1",
			Buckets: []string{"photos"}, Auth: delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEBEXAMPLE0001"}, Secret: concealedMark,
		},
	} {
		if _, err := f.targets.Put(ctx, f.root, in); err != nil {
			t.Fatalf("Put of target %s: %v", in.Name, err)
		}
	}
	return f
}

// rule is a notification rule for put to a target.
func rule(id, target string, on ...delivery.Event) bucket.NotificationRule {
	if len(on) == 0 {
		on = []delivery.Event{delivery.EventPut}
	}
	return bucket.NotificationRule{ID: id, As: bucket.NotifyAsQueue, Target: target, On: on}
}

func rules(r ...bucket.NotificationRule) bucket.NotificationConfig {
	return bucket.NotificationConfig{Rules: r}
}

// stored returns the document the store holds for the bucket.
func (f *notifyFixture) stored(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := f.meta.GetBucketConfig(context.Background(), name, bucket.ConfigNotification)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestNotificationsAreAuthorized: reading the rules needs
// s3:GetBucketNotification, setting and removing them
// s3:PutBucketNotification, both on the bucket; of the built-in roles only
// cluster-admin and bucket-admin hold them. A refusal is audited and changes
// nothing.
func TestNotificationsAreAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newNotifyFixture(t)
	if _, err := f.notify.Set(ctx, f.root, "photos", rules(rule("first", "hook"))); err != nil {
		t.Fatal(err)
	}
	before := f.stored(t, "photos")
	roles := map[string]string{
		iam.RoleClusterAdmin: "admin-user", iam.RoleBucketAdmin: "bucket-user", iam.RoleReadWrite: "writer-user",
		iam.RoleReadOnly: "reader-user", iam.RoleAuditor: "auditor-user",
	}
	principals := map[string]iam.Principal{}
	for role, user := range roles {
		principals[role] = f.as(t, f.user(t, user, role))
	}
	operations := []struct {
		name   string
		action iam.Action
		call   func(p iam.Principal) error
	}{
		{"get", iam.ActionGetBucketNotification, func(p iam.Principal) error {
			_, err := f.notify.Get(ctx, p, "photos")
			return err
		}},
		{"set", iam.ActionPutBucketNotification, func(p iam.Principal) error {
			_, err := f.notify.Set(ctx, p, "photos", rules(rule("first", "hook")))
			return err
		}},
		{"set nothing", iam.ActionPutBucketNotification, func(p iam.Principal) error {
			_, err := f.notify.Set(ctx, p, "photos", bucket.NotificationConfig{})
			return err
		}},
		{"delete", iam.ActionPutBucketNotification, func(p iam.Principal) error {
			return f.notify.Delete(ctx, p, "photos")
		}},
	}
	for _, op := range operations {
		for role, p := range principals {
			allowed := role == iam.RoleClusterAdmin || role == iam.RoleBucketAdmin
			err := op.call(p)
			switch {
			case allowed && err != nil:
				t.Errorf("%s by %s: %v", op.name, role, err)
			case !allowed && !errors.Is(err, domain.ErrAccessDenied):
				t.Errorf("%s by %s = %v, want ErrAccessDenied", op.name, role, err)
			}
			if allowed {
				// Put back what was there for the next caller.
				if _, err := f.notify.Set(ctx, f.root, "photos", rules(rule("first", "hook"))); err != nil {
					t.Fatal(err)
				}
				continue
			}
			found := false
			for _, r := range f.trail(t) {
				found = found || (r.Outcome == audit.Denied && r.Action == string(op.action) && r.Resource == "arn:aws:s3:::photos" && r.Actor == "user/"+roles[role])
			}
			if !found {
				t.Errorf("%s by %s: no denial of %s on the bucket in the trail", op.name, role, op.action)
			}
			if got := f.stored(t, "photos"); !bytes.Equal(got, before) {
				t.Errorf("%s by %s changed the rules to %s", op.name, role, got)
			}
		}
	}
}

// TestNotificationSet: what rules are kept, what is refused, and that a
// refusal leaves the rules the bucket had.
func TestNotificationSet(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		bucket string
		config bucket.NotificationConfig
		// fail makes the store refuse the change.
		fail error
		want error
		// kept are the rules as kept, when they are.
		kept bucket.NotificationConfig
	}{
		"one rule": {
			bucket: "photos", config: rules(rule("first", "hook")),
			kept: rules(rule("first", "hook")),
		},
		"two targets and every kind of change, in order": {
			bucket: "photos",
			config: rules(
				bucket.NotificationRule{
					ID: "logs", As: bucket.NotifyAsTopic, Target: "bus", Prefix: "logs/", Suffix: ".gz",
					On: []delivery.Event{delivery.EventDelete, delivery.EventPut, delivery.EventMarker, delivery.EventComplete},
				},
				rule("first", "hook", delivery.EventMarker),
			),
			// Kept, and returned, with the kinds of change in the order a
			// document has them in.
			kept: rules(
				bucket.NotificationRule{
					ID: "logs", As: bucket.NotifyAsTopic, Target: "bus", Prefix: "logs/", Suffix: ".gz",
					On: []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete},
				},
				rule("first", "hook", delivery.EventMarker),
			),
		},
		"IDs and the queue filled in, past an ID that is taken": {
			bucket: "photos",
			config: rules(
				bucket.NotificationRule{Target: "hook", On: []delivery.Event{delivery.EventPut}},
				rule("rule-2", "hook"),
				bucket.NotificationRule{Target: "bus", As: bucket.NotifyAsTopic, On: []delivery.Event{delivery.EventDelete}},
			),
			kept: rules(
				rule("rule-1", "hook"),
				rule("rule-2", "hook"),
				bucket.NotificationRule{ID: "rule-3", As: bucket.NotifyAsTopic, Target: "bus", On: []delivery.Event{delivery.EventDelete}},
			),
		},
		"a target that does not admit the bucket": {
			bucket: "private", config: rules(rule("first", "hook")), want: domain.ErrInvalidArgument,
		},
		"a target that is not there": {
			bucket: "photos", config: rules(rule("first", "missing")), want: domain.ErrInvalidArgument,
		},
		"another deployment told of changes": {
			bucket: "photos", config: rules(rule("first", "site-b")), want: domain.ErrInvalidArgument,
		},
		"one of two targets refused": {
			bucket: "photos", config: rules(rule("first", "hook"), rule("second", "missing")), want: domain.ErrInvalidArgument,
		},
		"a rule for no kind of change": {
			bucket: "photos", config: rules(bucket.NotificationRule{ID: "first", Target: "hook"}), want: domain.ErrInvalidArgument,
		},
		"a kind of change that is not told": {
			bucket: "photos", config: rules(rule("first", "hook", delivery.EventTags)), want: domain.ErrInvalidArgument,
		},
		"an ID twice": {
			bucket: "photos", config: rules(rule("first", "hook"), rule("first", "bus")), want: domain.ErrInvalidArgument,
		},
		"shown as something else": {
			bucket: "photos", config: rules(bucket.NotificationRule{ID: "first", As: "lambda", Target: "hook", On: []delivery.Event{delivery.EventPut}}),
			want: domain.ErrInvalidArgument,
		},
		"a target name that is none": {
			bucket: "photos", config: rules(rule("first", "Hook!")), want: domain.ErrInvalidArgument,
		},
		"a bucket that is not there": {
			bucket: "missing", config: rules(rule("first", "hook")), want: domain.ErrBucketNotFound,
		},
		"below the level of notifications": {
			bucket: "photos", config: rules(rule("first", "hook")), fail: domain.ErrUnavailable, want: domain.ErrUnavailable,
		},
		"a removal below the level of notifications": {
			bucket: "photos", fail: domain.ErrUnavailable, want: domain.ErrUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newNotifyFixture(t)
			// What the bucket had stays when a change is refused.
			if _, err := f.notify.Set(ctx, f.root, "photos", rules(rule("before", "hook"))); err != nil {
				t.Fatal(err)
			}
			before := f.stored(t, "photos")
			if tt.fail != nil {
				f.meta.FailConfig = map[string]error{bucket.ConfigNotification: tt.fail}
			}
			kept, err := f.notify.Set(ctx, f.root, tt.bucket, tt.config)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("Set = %v, want %v", err, tt.want)
				}
				if got := f.stored(t, "photos"); !bytes.Equal(got, before) {
					t.Errorf("a refused change left %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set: %v", err)
			}
			wantDoc, err := tt.kept.Document()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(kept, tt.kept) {
				t.Errorf("Set returned %+v, want %+v", kept, tt.kept)
			}
			if got := f.stored(t, tt.bucket); !bytes.Equal(got, wantDoc) {
				t.Errorf("stored %s, want %s", got, wantDoc)
			}
			got, err := f.notify.Get(ctx, f.root, tt.bucket)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, kept) {
				t.Errorf("Get = %+v, Set returned %+v", got, kept)
			}
		})
	}
}

// TestNotificationSetLeavesTheCallersRules: the rules given are not
// changed by what is filled in.
func TestNotificationSetLeavesTheCallersRules(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	on := []delivery.Event{delivery.EventPut}
	given := rules(bucket.NotificationRule{Target: "hook", On: on})
	if _, err := f.notify.Set(context.Background(), f.root, "photos", given); err != nil {
		t.Fatal(err)
	}
	if given.Rules[0].ID != "" || given.Rules[0].As != "" {
		t.Errorf("the caller's rule became %+v", given.Rules[0])
	}
}

// TestNotificationRefusalsSayTheSame: a target that is not there, one of
// another kind and one that does not admit the bucket are refused in the
// same words, so that a bucket's administrator learns nothing of targets
// that are not open to the bucket.
func TestNotificationRefusalsSayTheSame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newNotifyFixture(t)
	if _, err := f.targets.Put(ctx, f.root, app.TargetInput{
		Name: "closed", Kind: delivery.KindWebhook, Endpoint: "https://hooks.example.com" + concealedPath,
		Buckets: []string{"photos"}, Auth: delivery.Auth{Mode: delivery.AuthBearer}, Secret: concealedMark,
	}); err != nil {
		t.Fatal(err)
	}
	said := map[string]bool{}
	for _, target := range []string{"missing", "site-b", "closed"} {
		bucketName := "photos"
		if target == "closed" {
			bucketName = "private"
		}
		_, err := f.notify.Set(ctx, f.root, bucketName, rules(rule("first", target)))
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Fatalf("%s: Set = %v, want ErrInvalidArgument", target, err)
		}
		text := strings.NewReplacer(target, "<target>", bucketName, "<bucket>").Replace(err.Error())
		said[text] = true
		if strings.Contains(err.Error(), concealedMark) || strings.Contains(err.Error(), "example") {
			t.Errorf("%s: the refusal tells of the target: %v", target, err)
		}
	}
	if len(said) != 1 {
		t.Errorf("the refusals differ: %v", said)
	}
}

// openTargets says every target is open to every bucket: the store, not
// the use case, has the last word.
type openTargets struct{ ports.TargetStore }

func (openTargets) GetTarget(_ context.Context, name string) (delivery.Target, error) {
	return delivery.Target{Name: name, Kind: delivery.KindWebhook, Buckets: []string{"*"}}, nil
}

// TestNotificationStoreDecides: what the store refuses at commit is refused,
// whatever the use case read of the targets before.
func TestNotificationStoreDecides(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	notify := app.NewNotificationService(app.NotificationServiceConfig{
		Buckets: f.meta, Targets: openTargets{f.meta}, Authz: f.authz, Auditor: f.auditor,
	})
	if _, err := notify.Set(context.Background(), f.root, "private", rules(rule("first", "hook"))); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Set = %v, want ErrInvalidArgument", err)
	}
	if got := f.stored(t, "private"); got != nil {
		t.Errorf("stored %s", got)
	}
}

// failingTargets fails every read of a target.
type failingTargets struct{ ports.TargetStore }

func (failingTargets) GetTarget(context.Context, string) (delivery.Target, error) {
	return delivery.Target{}, domain.ErrUnavailable
}

// TestNotificationTargetsUnreadable: a target that cannot be read is not
// taken for one that is not there.
func TestNotificationTargetsUnreadable(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	notify := app.NewNotificationService(app.NotificationServiceConfig{
		Buckets: f.meta, Targets: failingTargets{f.meta}, Authz: f.authz, Auditor: f.auditor,
	})
	if _, err := notify.Set(context.Background(), f.root, "photos", rules(rule("first", "hook"))); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Set = %v, want ErrUnavailable", err)
	}
}

// TestNotificationRemoval: no rules, Set with none and Delete remove the
// document; a bucket without one is left as it is; every change is audited
// with what was done, and nothing of the keys the rules name.
func TestNotificationRemoval(t *testing.T) {
	t.Parallel()
	for name, remove := range map[string]func(ctx context.Context, f *notifyFixture) error{
		"set nothing": func(ctx context.Context, f *notifyFixture) error {
			_, err := f.notify.Set(ctx, f.root, "photos", bucket.NotificationConfig{})
			return err
		},
		"delete": func(ctx context.Context, f *notifyFixture) error { return f.notify.Delete(ctx, f.root, "photos") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newNotifyFixture(t)
			set := rules(bucket.NotificationRule{ID: "first", As: bucket.NotifyAsQueue, Target: "hook", On: []delivery.Event{delivery.EventPut}, Prefix: "secret-prefix/"}, rule("second", "bus"))
			if _, err := f.notify.Set(ctx, f.root, "photos", set); err != nil {
				t.Fatal(err)
			}
			if err := remove(ctx, f); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if got := f.stored(t, "photos"); got != nil {
				t.Errorf("stored after removal: %s", got)
			}
			if got, err := f.notify.Get(ctx, f.root, "photos"); err != nil || !got.Empty() {
				t.Errorf("Get after removal = %+v, %v", got, err)
			}
			// Removing what is not there is no error.
			if err := remove(ctx, f); err != nil {
				t.Errorf("remove again: %v", err)
			}
			var changes []map[string]string
			for _, r := range f.trail(t) {
				if r.Action == string(iam.ActionPutBucketNotification) && r.Outcome == audit.Succeeded && r.Resource == "arn:aws:s3:::photos" {
					changes = append(changes, r.Details)
				}
			}
			want := []map[string]string{{"rules": "2", "targets": "bus,hook"}, {"notification": "removed"}, {"notification": "removed"}}
			if len(changes) != len(want) {
				t.Fatalf("audited %v, want %v", changes, want)
			}
			for i := range want {
				for k, v := range want[i] {
					if changes[i][k] != v || len(changes[i]) != len(want[i]) {
						t.Errorf("record %d = %v, want %v", i, changes[i], want[i])
					}
				}
				for _, v := range changes[i] {
					if strings.Contains(v, "secret-prefix") {
						t.Errorf("record %d tells of a prefix: %v", i, changes[i])
					}
				}
			}
		})
	}
}

// TestNotificationWithoutAuditor: a change that cannot be audited is not
// made; reading needs no audit trail.
func TestNotificationWithoutAuditor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newNotifyFixture(t)
	notify := app.NewNotificationService(app.NotificationServiceConfig{Buckets: f.meta, Targets: f.meta, Authz: f.authz})
	if _, err := notify.Set(ctx, f.root, "photos", rules(rule("first", "hook"))); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Set = %v, want ErrUnavailable", err)
	}
	if err := notify.Delete(ctx, f.root, "photos"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Delete = %v, want ErrUnavailable", err)
	}
	if _, err := notify.Get(ctx, f.root, "photos"); err != nil {
		t.Errorf("Get = %v", err)
	}
}

// storedDocument is a bucket store that holds the document given.
type storedDocument struct {
	ports.BucketStore
	document []byte
	err      error
}

func (s storedDocument) GetBucketConfig(context.Context, string, string) ([]byte, error) {
	return s.document, s.err
}

// TestNotificationGetOfWhatCannotBeRead: rules of a later release are
// unavailable, rules that do not parse a fault of the store, a bucket that
// is not there is said to be so.
func TestNotificationGetOfWhatCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newNotifyFixture(t)
	current, err := rules(rule("first", "hook")).Document()
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		store storedDocument
		want  error
	}{
		"of a later release":   {storedDocument{document: bytes.Replace(current, []byte(`"v":1`), []byte(`"v":2`), 1)}, domain.ErrUnavailable},
		"not canonical":        {storedDocument{document: append(append([]byte(nil), current...), ' ')}, domain.ErrIntegrity},
		"not JSON":             {storedDocument{document: []byte("<NotificationConfiguration/>")}, domain.ErrIntegrity},
		"no bucket":            {storedDocument{err: domain.ErrBucketNotFound}, domain.ErrBucketNotFound},
		"the store fails":      {storedDocument{err: domain.ErrUnavailable}, domain.ErrUnavailable},
		"a document of rules":  {storedDocument{document: current}, nil},
		"a bucket without one": {storedDocument{}, nil},
	} {
		tt.store.BucketStore = f.meta
		notify := app.NewNotificationService(app.NotificationServiceConfig{Buckets: tt.store, Targets: f.meta, Authz: f.authz, Auditor: f.auditor})
		if _, err := notify.Get(ctx, f.root, "photos"); !errors.Is(err, tt.want) && (tt.want != nil || err != nil) {
			t.Errorf("%s: Get = %v, want %v", name, err, tt.want)
		}
	}
}

// fakePublisher keeps what it is given and answers with err.
type fakePublisher struct {
	mu     sync.Mutex
	events []ports.Event
	err    error
}

func (p *fakePublisher) Publish(_ context.Context, e ports.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, e)
	return nil
}

func (p *fakePublisher) published() []ports.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ports.Event(nil), p.events...)
}

// notifyVersion is the version of the items of the tests.
func notifyVersion(t testing.TB) object.VersionRef {
	t.Helper()
	id, err := object.ParseID("0f3a5c7e9b1d2f4a6c8e0b2d4f6a8c0e")
	if err != nil {
		t.Fatal(err)
	}
	return object.VersionRef{ID: id}
}

// notifyItem is a row of the outbox for the webhook hook.
func notifyItem(t testing.TB, event delivery.Event) delivery.Item {
	t.Helper()
	item := delivery.Item{
		Seq: delivery.Seq{Index: 1234, Row: 2}, Event: event, Bucket: "photos", Key: "2026/a b+c.jpg",
		Version: notifyVersion(t), Size: 1024, ETag: "9b2cf535f27731c974343645a3985328",
		At: time.Date(2026, 10, 12, 9, 30, 0, 123456789, time.UTC), By: "user/alice",
		To: []delivery.Consumer{{Kind: delivery.ConsumerReplication, Target: "site-b"}, {Kind: delivery.ConsumerNotification, Target: "hook"}},
	}
	if event == delivery.EventMarker {
		item.Size, item.ETag = 0, ""
	}
	return item
}

// hookTarget is the webhook the items are for.
func hookTarget() delivery.Target {
	return delivery.Target{Name: "hook", Kind: delivery.KindWebhook, Buckets: []string{"photos", "logs-*"}}
}

// recordOf reads the one record of a body, as a receiver would.
func recordOf(t testing.TB, body []byte) map[string]any {
	t.Helper()
	var records struct {
		Records []map[string]any `json:"Records"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&records); err != nil {
		t.Fatalf("the body is not JSON: %v: %s", err, body)
	}
	if len(records.Records) != 1 {
		t.Fatalf("%d records, want one: %s", len(records.Records), body)
	}
	return records.Records[0]
}

// s3Of returns the bucket and the object of a record.
func s3Of(t testing.TB, record map[string]any) (bucketOf, objectOf map[string]any) {
	t.Helper()
	s3, ok := record["s3"].(map[string]any)
	if !ok {
		t.Fatalf("s3 = %v", record["s3"])
	}
	if bucketOf, ok = s3["bucket"].(map[string]any); !ok {
		t.Fatalf("s3.bucket = %v", s3["bucket"])
	}
	if objectOf, ok = s3["object"].(map[string]any); !ok {
		t.Fatalf("s3.object = %v", s3["object"])
	}
	return bucketOf, objectOf
}

// TestNotificationRecord: what the record of each kind of change says.
func TestNotificationRecord(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		item func() delivery.Item
		// eventName is the name told; absent the fields left out of
		// the object; identity whether userIdentity is there.
		eventName string
		absent    []string
		identity  bool
	}{
		"put": {
			item: func() delivery.Item { return notifyItem(t, delivery.EventPut) }, eventName: "ObjectCreated:Put", identity: true,
		},
		"complete": {
			item: func() delivery.Item { return notifyItem(t, delivery.EventComplete) }, eventName: "ObjectCreated:CompleteMultipartUpload", identity: true,
		},
		"marker": {
			item: func() delivery.Item { return notifyItem(t, delivery.EventMarker) }, eventName: "ObjectRemoved:DeleteMarkerCreated",
			absent: []string{"size", "eTag"}, identity: true,
		},
		"delete": {
			item: func() delivery.Item { return notifyItem(t, delivery.EventDelete) }, eventName: "ObjectRemoved:Delete", identity: true,
		},
		"a marker removed for good": {
			item: func() delivery.Item {
				item := notifyItem(t, delivery.EventDelete)
				item.Size, item.ETag = 0, ""
				return item
			},
			eventName: "ObjectRemoved:Delete", absent: []string{"size", "eTag"}, identity: true,
		},
		"no actor recorded": {
			item: func() delivery.Item {
				item := notifyItem(t, delivery.EventPut)
				item.By = ""
				return item
			},
			eventName: "ObjectCreated:Put",
		},
		"a bucket that keeps no versions": {
			item: func() delivery.Item {
				item := notifyItem(t, delivery.EventPut)
				item.Version = object.VersionRef{Null: true}
				return item
			},
			eventName: "ObjectCreated:Put", absent: []string{"versionId"}, identity: true,
		},
		"an empty object": {
			item: func() delivery.Item {
				item := notifyItem(t, delivery.EventPut)
				item.Size, item.ETag = 0, "d41d8cd98f00b204e9800998ecf8427e"
				return item
			},
			eventName: "ObjectCreated:Put", identity: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			item := tt.item()
			body, err := app.NotificationRecord(item)
			if err != nil {
				t.Fatal(err)
			}
			record := recordOf(t, body)
			if record["eventName"] != tt.eventName || record["eventVersion"] != "2.1" || record["eventSource"] != "cactusfs:s3" ||
				record["eventTime"] != "2026-10-12T09:30:00.123456789Z" {
				t.Errorf("record = %v", record)
			}
			for _, field := range []string{"requestParameters", "responseElements", "awsRegion"} {
				if _, ok := record[field]; ok {
					t.Errorf("the record has %s", field)
				}
			}
			identity, ok := record["userIdentity"].(map[string]any)
			if ok != tt.identity || (ok && identity["principalId"] != "user/alice") {
				t.Errorf("userIdentity = %v, want present %v", record["userIdentity"], tt.identity)
			}
			b, obj := s3Of(t, record)
			if b["name"] != "photos" || len(b) != 1 {
				t.Errorf("bucket = %v", b)
			}
			if obj["key"] != "2026/a+b%2Bc.jpg" || obj["sequencer"] != "00000000000004d20002" {
				t.Errorf("object = %v", obj)
			}
			want := map[string]any{
				"size": json.Number(strconv.FormatInt(item.Size, 10)), "eTag": item.ETag, "versionId": "0f3a5c7e9b1d2f4a6c8e0b2d4f6a8c0e",
			}
			for _, field := range tt.absent {
				delete(want, field)
			}
			for field, value := range want {
				if obj[field] != value {
					t.Errorf("object.%s = %v, want %v", field, obj[field], value)
				}
			}
			if len(obj) != len(want)+2 {
				t.Errorf("object has %d fields, want %d: %v", len(obj), len(want)+2, obj)
			}
		})
	}
}

// TestNotificationEventNames: every kind of change a rule can ask for has
// its name, and no other kind has one.
func TestNotificationEventNames(t *testing.T) {
	t.Parallel()
	for event, want := range map[delivery.Event]string{
		delivery.EventPut: "ObjectCreated:Put", delivery.EventComplete: "ObjectCreated:CompleteMultipartUpload",
		delivery.EventMarker: "ObjectRemoved:DeleteMarkerCreated", delivery.EventDelete: "ObjectRemoved:Delete",
		delivery.EventTags: "", delivery.EventRetention: "", delivery.EventLegalHold: "", "other": "",
	} {
		got, ok := app.NotificationEventName(event)
		if got != want || ok != (want != "") || ok != event.Notified() {
			t.Errorf("NotificationEventName(%s) = %q, %v; want %q", event, got, ok, want)
		}
		if !ok {
			item := notifyItem(t, delivery.EventPut)
			item.Event = event
			if _, err := app.NotificationRecord(item); !errors.Is(err, domain.ErrInvalidArgument) || strings.Contains(err.Error(), item.Key) {
				t.Errorf("NotificationRecord of %s = %v", event, err)
			}
		}
	}
}

// TestNotificationKey: keys are encoded as S3 encodes them in a
// notification, and come back whole.
func TestNotificationKey(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]string{
		"photo.jpg":           "photo.jpg",
		"red flower.jpg":      "red+flower.jpg",
		"a+b":                 "a%2Bb",
		"dir/sub/file":        "dir/sub/file",
		"q?x=1&y=2#z":         "q%3Fx%3D1%26y%3D2%23z",
		"café":                "caf%C3%A9",
		"../../etc/hosts":     "../../etc/hosts",
		"100%":                "100%25",
		"<script>\"'":         "%3Cscript%3E%22%27",
		"tab\tnew\nline":      "tab%09new%0Aline",
		"~user/-_.":           "~user/-_.",
		"invalid\xffutf8\x00": "invalid%FFutf8%00",
	} {
		got := app.NotificationKey(key)
		if got != want {
			t.Errorf("NotificationKey(%q) = %q, want %q", key, got, want)
		}
		if back, err := url.QueryUnescape(got); err != nil || back != key {
			t.Errorf("QueryUnescape(%q) = %q, %v; want %q", got, back, err, key)
		}
	}
}

// notifyGolden holds the records of TestNotificationRecordGolden, checked
// field by field against ADR-0025 and changed only with it.
//
//go:embed testdata/notification-records.golden.jsonl
var notifyGolden embed.FS

// TestNotificationRecordGolden: the bytes of one record of each class are
// pinned; a receiver written against them reads every later one.
func TestNotificationRecordGolden(t *testing.T) {
	t.Parallel()
	var lines [][]byte
	for _, event := range []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete} {
		item := notifyItem(t, event)
		switch event {
		case delivery.EventComplete:
			// Of an upload by nobody recorded, in a bucket that keeps
			// no versions.
			item.By, item.Version, item.ETag = "", object.VersionRef{Null: true}, "9b2cf535f27731c974343645a3985328-3"
		case delivery.EventDelete:
			item.Seq = delivery.Seq{Index: 1235}
			item.By = "service/lifecycle"
		}
		body, err := app.NotificationRecord(item)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, body)
	}
	got := bytes.Join(lines, []byte("\n"))
	want, err := notifyGolden.ReadFile("testdata/notification-records.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if want = bytes.TrimSpace(want); !bytes.Equal(got, want) {
		t.Errorf("records:\n got %s\nwant %s", got, want)
	}
}

// TestNotifierDelivers: the notification of an item is published through
// the target's publisher, as the message the contract names, with the ID
// of the item and its record as the body.
func TestNotifierDelivers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	publisher := &fakePublisher{}
	var asked []string
	var mu sync.Mutex
	notifier := app.NewNotifier(app.NotifierConfig{Publishers: app.PublisherSourceFunc(func(_ context.Context, target delivery.Target) (ports.EventPublisher, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, target.Name)
		return publisher, nil
	})})
	for _, event := range []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete} {
		item := notifyItem(t, event)
		status, err := notifier.Deliver(ctx, hookTarget(), item)
		if err != nil || status != "" {
			t.Fatalf("Deliver of %s = %q, %v", event, status, err)
		}
	}
	events := publisher.published()
	if len(events) != 4 || len(asked) != 4 || asked[0] != "hook" {
		t.Fatalf("published %d, asked %v", len(events), asked)
	}
	for i, event := range []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete} {
		item := notifyItem(t, event)
		e := events[i]
		want, err := app.NotificationRecord(item)
		if err != nil {
			t.Fatal(err)
		}
		body, ok := e.Data.(json.RawMessage)
		if e.ID != item.ID() || e.ID != "00000000000004d20002" || e.Type != ports.MessageNotificationObject || e.Sequence != 1234 ||
			!e.Time.Equal(item.At) || !ok || !bytes.Equal(body, want) {
			t.Errorf("event %d = %+v", i, e)
		}
		// What a publisher that marshals the event's data sends is the
		// record, byte for byte.
		if marshalled, err := json.Marshal(e.Data); err != nil || !bytes.Equal(marshalled, want) {
			t.Errorf("event %d marshals to %s, %v", i, marshalled, err)
		}
	}
}

// TestNotifierFailures: a failure of the publisher, or of the source of
// publishers, keeps its class - one without a class is internal - and its
// text is the class alone; an item that is not to be sent is passed, as
// rejected, without a publisher being asked for.
func TestNotifierFailures(t *testing.T) {
	t.Parallel()
	const markedURL = "https://hooks.example.com/MARKED-endpoint-path"
	refused := delivery.Failed(delivery.ClassAuth, errors.New("401 from "+markedURL))
	for name, tt := range map[string]struct {
		target    delivery.Target
		item      func() delivery.Item
		source    app.PublisherSource
		publishFn error
		want      delivery.Class
		asked     bool
	}{
		"the target refuses the credential": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventPut) },
			publishFn: refused, want: delivery.ClassAuth, asked: true,
		},
		"the target rejects the message": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventPut) },
			publishFn: delivery.Failed(delivery.ClassRejected, errors.New("400 from "+markedURL)), want: delivery.ClassRejected, asked: true,
		},
		"an error without a class": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventPut) },
			publishFn: errors.New("dial " + markedURL), want: delivery.ClassInternal, asked: true,
		},
		"the deadline of the attempt ended": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventPut) },
			publishFn: fmt.Errorf("post %s: %w", markedURL, context.DeadlineExceeded), want: delivery.ClassTimeout, asked: true,
		},
		"a classed failure that also ran out of time keeps its class": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventPut) },
			publishFn: delivery.Failed(delivery.ClassServer, fmt.Errorf("503 from %s: %w", markedURL, context.DeadlineExceeded)),
			want:      delivery.ClassServer, asked: true,
		},
		"the deadline ended while the publisher was made": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventPut) },
			source: app.PublisherSourceFunc(func(context.Context, delivery.Target) (ports.EventPublisher, error) {
				return nil, fmt.Errorf("unwrap: %w", context.DeadlineExceeded)
			}),
			want: delivery.ClassTimeout,
		},
		"the credential cannot be opened": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventPut) },
			source: app.PublisherSourceFunc(func(context.Context, delivery.Target) (ports.EventPublisher, error) {
				return nil, errors.New("unwrap failed for " + markedURL)
			}),
			want: delivery.ClassInternal,
		},
		"no publisher made": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventPut) },
			source: app.PublisherSourceFunc(func(context.Context, delivery.Target) (ports.EventPublisher, error) { return nil, nil }),
			want:   delivery.ClassInternal,
		},
		"nothing to publish through": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventPut) },
			source: nil, want: delivery.ClassInternal,
		},
		"a target that no longer admits the bucket": {
			target: delivery.Target{Name: "hook", Kind: delivery.KindWebhook, Buckets: []string{"logs-*"}},
			item:   func() delivery.Item { return notifyItem(t, delivery.EventPut) }, want: delivery.ClassRejected,
		},
		"another deployment": {
			target: delivery.Target{Name: "hook", Kind: delivery.KindS3, Buckets: []string{"photos"}},
			item:   func() delivery.Item { return notifyItem(t, delivery.EventPut) }, want: delivery.ClassRejected,
		},
		// An item handed to the wrong Deliverer is held, as the worker
		// holds one of a kind nothing delivers.
		"an item for the target as a replica": {
			target: delivery.Target{Name: "site-b", Kind: delivery.KindWebhook, Buckets: []string{"photos"}},
			item:   func() delivery.Item { return notifyItem(t, delivery.EventPut) }, want: delivery.ClassInternal,
		},
		"an item not for the target": {
			target: delivery.Target{Name: "other", Kind: delivery.KindWebhook, Buckets: []string{"photos"}},
			item:   func() delivery.Item { return notifyItem(t, delivery.EventPut) }, want: delivery.ClassInternal,
		},
		"a change that is not told": {
			target: hookTarget(), item: func() delivery.Item { return notifyItem(t, delivery.EventTags) }, want: delivery.ClassRejected,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			publisher := &fakePublisher{err: tt.publishFn}
			asked := false
			source := tt.source
			if source == nil && name != "nothing to publish through" {
				source = app.PublisherSourceFunc(func(context.Context, delivery.Target) (ports.EventPublisher, error) {
					asked = true
					return publisher, nil
				})
			}
			logs := &syncBuffer{}
			notifier := app.NewNotifier(app.NotifierConfig{Publishers: source, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
			status, err := notifier.Deliver(ctx, tt.target, tt.item())
			if err == nil || status != "" {
				t.Fatalf("Deliver = %q, %v", status, err)
			}
			if got := delivery.ClassOf(err); got != tt.want {
				t.Errorf("class %q, want %q", got, tt.want)
			}
			if err.Error() != "delivery failed: "+string(tt.want) {
				t.Errorf("the failure says %q", err.Error())
			}
			if asked != tt.asked {
				t.Errorf("a publisher asked for: %v, want %v", asked, tt.asked)
			}
			if len(publisher.published()) != 0 {
				t.Errorf("published %v", publisher.published())
			}
			for _, secret := range []string{markedURL, "2026/a b+c.jpg", "a+b%2Bc", "photos"} {
				if strings.Contains(logs.String(), secret) || strings.Contains(err.Error(), secret) {
					t.Errorf("%q told: %s; %v", secret, logs.String(), err)
				}
			}
		})
	}
}

// FuzzNotificationRecord: whatever the key, the actor, the size, the
// version, the entity tag, the place and the kind of change, the record is
// valid JSON of one record, holds no address of a client, names the version
// exactly when it is not the null version, has an entity tag without a
// quote, and its key gives the key back, byte for byte.
func FuzzNotificationRecord(f *testing.F) {
	version := []byte{0x0f, 0x3a, 0x5c, 0x7e, 0x9b, 0x1d, 0x2f, 0x4a, 0x6c, 0x8e, 0x0b, 0x2d, 0x4f, 0x6a, 0x8c, 0x0e}
	f.Add("photo.jpg", "user/alice", int64(1024), uint8(0), uint64(1234), uint16(2), version, "9b2cf535f27731c974343645a3985328")
	f.Add("red flower+.jpg", "", int64(0), uint8(1), uint64(0), uint16(0), []byte{}, `"9b2c-3"`)
	f.Add("../../etc/hosts", "key/AKID", int64(-1), uint8(2), uint64(1<<63), uint16(65535), make([]byte, 16), "")
	f.Add("\x00\xff\"<>& /%2F", "\xff\"", int64(1<<62), uint8(3), uint64(7), uint16(1), version[:15], `""a"b""`)
	f.Add("a", "root", int64(5), uint8(4), uint64(1), uint16(0), version, "\"")
	events := []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete, delivery.EventTags}
	f.Fuzz(func(t *testing.T, key, by string, size int64, kind uint8, index uint64, row uint16, versionID []byte, etag string) {
		// Sixteen bytes are a version ID, anything else the null version.
		ref := object.VersionRef{Null: true}
		if len(versionID) == object.IDSize {
			ref = object.VersionRef{ID: object.ID(versionID)}
		}
		item := delivery.Item{
			Seq: delivery.Seq{Index: index, Row: row}, Event: events[int(kind)%len(events)], Bucket: "photos", Key: key,
			Version: ref, Size: size, ETag: etag, At: testNow.Add(time.Duration(row) * time.Millisecond), By: by,
		}
		body, err := app.NotificationRecord(item)
		if !item.Event.Notified() {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("NotificationRecord of %s = %v", item.Event, err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(body) {
			t.Fatalf("not JSON: %s", body)
		}
		record := recordOf(t, body)
		if _, ok := record["requestParameters"]; ok {
			t.Fatal("the record has requestParameters")
		}
		_, obj := s3Of(t, record)
		encoded, ok := obj["key"].(string)
		if !ok {
			t.Fatalf("key = %v", obj["key"])
		}
		if back, err := url.QueryUnescape(encoded); err != nil || back != key {
			t.Fatalf("key %q came back as %q, %v", key, back, err)
		}
		if obj["sequencer"] != item.Seq.String() {
			t.Fatalf("sequencer = %v, want %s", obj["sequencer"], item.Seq)
		}
		if _, ok := record["userIdentity"]; ok != (by != "") {
			t.Fatalf("userIdentity = %v for actor %q", record["userIdentity"], by)
		}
		named := !ref.Null && !ref.ID.IsZero()
		if got, ok := obj["versionId"]; ok != named || (named && got != ref.ID.String()) {
			t.Fatalf("versionId = %v for version %+v", got, ref)
		}
		if got, ok := obj["eTag"]; ok {
			text, isText := got.(string)
			if !isText || text == "" || strings.Contains(text, `"`) {
				t.Fatalf("eTag = %#v for %q", got, etag)
			}
		}
	})
}
