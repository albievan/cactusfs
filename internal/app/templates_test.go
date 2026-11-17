package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestUseCasesAskWhatTheCatalogueSays (carried from PLAN 9.1 to 9.5): every
// question a use case puts to the authorizer - to gate an operation or to
// filter what is shown - is of an action of the catalogue, on a resource
// that fits one of the templates the catalogue gives that action. The use
// cases are driven through a recording authorizer, by root (so that each
// goes on past its gate and asks what it asks after it) and by a user
// without a policy (so that each is refused at its first question), and
// every question recorded is checked. Every action of the catalogue must be
// asked by the walk, but those only an adapter asks (askedByAdapters), which
// the adapter's own tests hold to the catalogue.
func TestUseCasesAskWhatTheCatalogueSays(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	identities := porttest.NewMemIAM()
	auditor := app.NewAuditor(identities, f.clock, logger)
	f.buckets = app.NewBucketService(f.meta, f.authz, f.clock).WithAuditor(auditor)
	services := struct {
		iam           *app.IAMService
		subBuckets    *app.SubBucketService
		targets       *app.TargetService
		notifications *app.NotificationService
		replication   *app.ReplicationService
		quotas        *app.QuotaService
		tags          *app.TagDefinitionService
		tools         *app.PolicyToolsService
		explain       *app.ExplainService
		cluster       *app.ClusterService
		counter       *app.UsageCounter
		search        *app.SearchService
		idempotency   *app.IdempotencyService
	}{
		iam: app.NewIAMService(app.IAMServiceConfig{
			Store: identities, Trail: identities, Authz: f.authz, Auditor: auditor, KMS: f.kms, Hasher: &porttest.Hasher{},
			Clock: f.clock, Entropy: rand.Reader, Sessions: identities,
		}),
		subBuckets: app.NewSubBucketService(f.meta, f.authz, auditor, f.clock, logger),
		targets: app.NewTargetService(app.TargetServiceConfig{
			Store: f.meta, Authz: f.authz, Auditor: auditor, KMS: f.kms, Clock: f.clock, Entropy: rand.Reader,
			Delivery: porttest.ScriptedDeliveryStatusReader(porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering}),
		}),
		notifications: app.NewNotificationService(app.NotificationServiceConfig{Buckets: f.meta, Targets: f.meta, Authz: f.authz, Auditor: auditor}),
		replication: app.NewReplicationService(app.ReplicationServiceConfig{
			Buckets: f.meta, Store: f.meta, Targets: f.meta, Authz: f.authz, Auditor: auditor, Usage: f.meta, Outbox: f.meta,
			Delivery: porttest.ScriptedDeliveryStatusReader(porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering}),
		}),
		quotas: app.NewQuotaService(app.QuotaServiceConfig{
			Buckets: f.meta, Usage: f.meta, Authz: f.authz, Auditor: auditor,
			Level: func(context.Context) (int, error) { return 5, nil },
		}),
		tags:  app.NewTagDefinitionService(app.TagDefinitionServiceConfig{Store: f.meta, Authz: f.authz, Auditor: auditor, Clock: f.clock}),
		tools: app.NewPolicyToolsService(f.authz, auditor),
		explain: app.NewExplainService(app.ExplainServiceConfig{
			Store: identities, Trail: identities, Authz: f.authz, Explainer: &porttest.Explainer{}, Auditor: auditor, Clock: f.clock, Logger: logger,
		}),
		cluster: app.NewClusterService(app.ClusterServiceConfig{
			View: staticView{}, Authz: f.authz, NodeID: "n1", Leader: func() string { return "n1" }, Maintenance: app.NewMaintenance("n1", f.clock),
			Probe: func(context.Context, cluster.Slot) bool { return true },
		}),
		counter: app.NewUsageCounter(app.UsageCounterConfig{
			Buckets: f.meta, Usage: f.meta, Authz: f.authz, Auditor: auditor, Logger: logger, Active: func() bool { return true },
		}),
		idempotency: app.NewIdempotencyService(identities, f.kms, f.clock, rand.Reader).WithAuthorizer(f.authz),
	}
	var err error
	if services.search, err = app.NewSearchService(app.SearchServiceConfig{
		Buckets: f.meta, Index: f.meta, Policies: f.meta, Authz: f.authz, Clock: f.clock, Logger: logger,
	}); err != nil {
		t.Fatal(err)
	}
	// The releases, and how every node is configured: over fixtures of
	// their own, asking the same recording authorizer.
	updates := newUpdateFixture(t, release.ModeBinary, nil, func(cfg *app.UpdateServiceConfig) { cfg.Authz = f.authz }).updates
	others := map[string]porttest.NodeBehaviour{}
	for _, node := range []string{"n2", "n3", "n4"} {
		others[node] = porttest.NodeBehaviour{Settings: settingsOf(node, "us-east-1", "d"+node[1:]+"-1")}
	}
	settings := newSettingsFixture(t, fourNodes(t), others, func(cfg *app.SettingsServiceConfig) { cfg.Authz = f.authz }).service
	root := porttest.Root()
	if err := f.buckets.Create(ctx, root, "photos"); err != nil {
		t.Fatal(err)
	}
	version := f.put(t, "photos", "team/a.png", []byte("a picture"))
	if _, err := services.iam.CreateUser(ctx, root, app.UserInput{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	ref := &object.VersionRef{ID: version.VersionID}
	// A bucket under Object Lock, with a version under a governance
	// retention, and a bucket that is the replica of another deployment.
	if err := f.buckets.CreateWithObjectLock(ctx, root, "vault"); err != nil {
		t.Fatal(err)
	}
	locked := f.put(t, "vault", "deed.pdf", []byte("a deed"))
	lockedRef := &object.VersionRef{ID: locked.VersionID}
	if err := f.buckets.Create(ctx, root, "mirror"); err != nil {
		t.Fatal(err)
	}
	if err := f.buckets.SetVersioning(ctx, root, "mirror", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := services.replication.SetReplicaMode(ctx, root, "mirror", "site-b"); err != nil {
		t.Fatal(err)
	}
	policy := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`)
	bucketPolicy := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}]}`)
	subPolicy := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/team/*"}]}`)

	// Every use case, as the S3 and the management APIs call them. What
	// they answer does not matter here: only what they ask.
	script := func(p iam.Principal) {
		ignore := func(...any) {}
		// IAM.
		ignore(services.iam.PutPolicy(ctx, p, "readers", policy, true))
		ignore(services.iam.GetPolicy(ctx, p, "readers"))
		ignore(services.iam.ListPolicies(ctx, p))
		ignore(services.iam.PutRole(ctx, p, iam.Role{Name: "reader", Policies: []string{"readers"}}, true))
		ignore(services.iam.GetRole(ctx, p, "reader"))
		ignore(services.iam.ListRoles(ctx, p))
		ignore(services.iam.PutGroup(ctx, p, iam.Group{Name: "team", Roles: []string{"reader"}}, true))
		ignore(services.iam.GetGroup(ctx, p, "team"))
		ignore(services.iam.ListGroups(ctx, p))
		ignore(services.iam.CreateUser(ctx, p, app.UserInput{Name: "bob", Groups: []string{"team"}}))
		ignore(services.iam.UpdateUser(ctx, p, app.UserInput{Name: "bob", Groups: []string{"team"}}))
		ignore(services.iam.GetUser(ctx, p, "bob"))
		ignore(services.iam.ListUsers(ctx, p))
		creds, _, err := services.iam.CreateAPIKey(ctx, p, app.APIKeyInput{Owner: "bob", InlinePolicy: policy})
		ignore(services.iam.ListAPIKeys(ctx, p, "bob"))
		ignore(services.iam.AuditLog(ctx, p, 0, 10))
		if err == nil {
			ignore(services.iam.DeleteAPIKey(ctx, p, creds.AccessKeyID))
		}
		ignore(services.iam.DeleteAPIKey(ctx, p, "AKIANOSUCHKEY0000000"))
		ignore(services.iam.DeleteUser(ctx, p, "bob"))
		ignore(services.iam.DeleteGroup(ctx, p, "team"))
		ignore(services.iam.DeleteRole(ctx, p, "reader"))
		ignore(services.iam.DeletePolicy(ctx, p, "readers"))
		// The policy tools and the explanation.
		ignore(services.tools.Catalogue(ctx, p))
		ignore(services.tools.ValidateIdentity(ctx, p, app.IdentityPolicyValidation{Document: policy}))
		ignore(services.tools.ValidateIdentity(ctx, p, app.IdentityPolicyValidation{Document: policy, Name: "readers"}))
		ignore(services.tools.ValidateIdentity(ctx, p, app.IdentityPolicyValidation{Document: policy, KeyOwner: "alice"}))
		ignore(services.tools.ValidateBucket(ctx, p, "photos", bucketPolicy))
		ignore(services.tools.ValidateSubBucket(ctx, p, "photos", "team", subPolicy))
		ignore(services.explain.Explain(ctx, p, app.ExplainRequest{User: "alice", Action: iam.ActionGetObject, Bucket: "photos", Key: "team/a.png"}))
		ignore(services.explain.Explain(ctx, p, app.ExplainRequest{Anonymous: true, Action: iam.ActionListBucket, Bucket: "photos", ListPrefix: "team/"}))
		ignore(services.explain.Explain(ctx, p, app.ExplainRequest{AccessKeyID: "AKIANOSUCHKEY0000000", Action: iam.ActionListAllMyBuckets}))
		ignore(services.explain.Explain(ctx, p, app.ExplainRequest{User: "alice", Action: iam.ActionGetUser, AdminResource: "user/alice"}))
		// Buckets.
		ignore(f.buckets.List(ctx, p))
		ignore(f.buckets.ListDetailed(ctx, p))
		ignore(f.buckets.Get(ctx, p, "photos"))
		ignore(f.buckets.Location(ctx, p, "photos"))
		ignore(f.buckets.Versioning(ctx, p, "photos"))
		ignore(f.buckets.SetVersioning(ctx, p, "photos", bucket.VersioningEnabled))
		ignore(f.buckets.SetPolicy(ctx, p, "photos", bucketPolicy))
		ignore(f.buckets.Policy(ctx, p, "photos"))
		ignore(f.buckets.SetPolicy(ctx, p, "photos", nil))
		ignore(f.buckets.CORS(ctx, p, "photos"))
		ignore(f.buckets.SetCORS(ctx, p, "photos", nil))
		ignore(f.buckets.Tags(ctx, p, "photos"))
		ignore(f.buckets.SetTags(ctx, p, "photos", []object.Tag{{Key: "k", Value: "v"}}))
		ignore(f.buckets.TagRules(ctx, p, "photos"))
		ignore(f.buckets.DeleteTagRules(ctx, p, "photos"))
		ignore(f.buckets.Lifecycle(ctx, p, "photos"))
		ignore(f.buckets.SetLifecycle(ctx, p, "photos", nil))
		ignore(f.buckets.ObjectLock(ctx, p, "photos"))
		ignore(f.buckets.Create(ctx, p, "spare"))
		ignore(f.buckets.Delete(ctx, p, "spare"))
		// Sub-buckets.
		ignore(services.subBuckets.Put(ctx, p, "photos", "team", subPolicy))
		ignore(services.subBuckets.List(ctx, p, "photos"))
		ignore(services.subBuckets.Get(ctx, p, "photos", "team"))
		ignore(services.subBuckets.Put(ctx, p, "photos", "empty", nil))
		ignore(services.subBuckets.Delete(ctx, p, "photos", "empty"))
		// Objects, versions, locks, tags, uploads, previews.
		ignore(f.objects.Put(ctx, p, app.PutInput{Bucket: "photos", Key: "b.txt", Body: bytes.NewReader([]byte("b")), Size: 1}))
		ignore(f.objects.Head(ctx, p, "photos", "team/a.png"))
		ignore(f.objects.HeadVersion(ctx, p, "photos", "team/a.png", *ref))
		ignore(f.objects.Get(ctx, p, app.GetInput{Bucket: "photos", Key: "team/a.png"}))
		ignore(f.objects.Get(ctx, p, app.GetInput{Bucket: "photos", Key: "team/a.png", Version: ref}))
		ignore(f.objects.List(ctx, p, "photos", object.ListOptions{Prefix: "team/", MaxKeys: 10}))
		ignore(f.objects.ListVersions(ctx, p, "photos", app.VersionListOptions{MaxKeys: 10}))
		ignore(f.objects.ListWithAccess(ctx, p, "photos", object.ListOptions{MaxKeys: 10}, true))
		ignore(f.objects.Access(ctx, p, "photos", "team/a.png", ""))
		ignore(f.objects.Usage(ctx, p, "photos"))
		ignore(f.objects.Tags(ctx, p, app.LockInput{Bucket: "photos", Key: "team/a.png"}))
		ignore(f.objects.SetTags(ctx, p, app.LockInput{Bucket: "photos", Key: "team/a.png"}, []object.Tag{{Key: "k", Value: "v"}}))
		ignore(f.objects.Retention(ctx, p, app.LockInput{Bucket: "photos", Key: "team/a.png"}))
		ignore(f.objects.LegalHold(ctx, p, app.LockInput{Bucket: "photos", Key: "team/a.png"}))
		ignore(f.objects.Copy(ctx, p, app.CopyInput{Source: app.GetInput{Bucket: "photos", Key: "team/a.png"}, Destination: app.PutInput{Bucket: "photos", Key: "c.png"}}))
		ignore(f.objects.Preview(ctx, p, app.PreviewInput{Bucket: "photos", Key: "team/a.png"}))
		ignore(f.objects.ReplicationVersion(ctx, p, "photos", "team/a.png", *ref))
		upload, err := f.objects.CreateUpload(ctx, p, app.CreateUploadInput{Bucket: "photos", Key: "big.bin"})
		ignore(f.objects.ListUploads(ctx, p, app.ListUploadsInput{Bucket: "photos", Max: 10}))
		if err == nil {
			uploadRef := app.UploadRef{Bucket: "photos", Key: "big.bin", UploadID: upload.ID}
			ignore(f.objects.ListParts(ctx, p, app.ListPartsInput{UploadRef: uploadRef, Max: 10}))
			ignore(f.objects.AbortUpload(ctx, p, uploadRef))
		}
		ignore(f.objects.Delete(ctx, p, app.DeleteInput{Bucket: "photos", Key: "b.txt"}))
		ignore(f.objects.SetTags(ctx, p, app.LockInput{Bucket: "photos", Key: "team/a.png"}, nil))
		ignore(services.counter.Recount(ctx, p, "photos"))
		// Object Lock: the bucket's default, a retention lifted by bypass,
		// a legal hold, a version removed past its governance.
		ignore(f.buckets.SetDefaultRetention(ctx, p, "vault", &bucket.DefaultRetention{Mode: object.Governance, Days: 1}))
		vaulted := app.LockInput{Bucket: "vault", Key: "deed.pdf", Version: lockedRef}
		ignore(f.objects.SetRetention(ctx, p, vaulted, &object.Retention{Mode: object.Governance, Until: testNow.Add(48 * time.Hour)}, false))
		ignore(f.objects.SetRetention(ctx, p, vaulted, &object.Retention{Mode: object.Governance, Until: testNow.Add(time.Hour)}, true))
		ignore(f.objects.SetLegalHold(ctx, p, vaulted, true))
		ignore(f.objects.SetLegalHold(ctx, p, vaulted, false))
		ignore(f.objects.Delete(ctx, p, app.DeleteInput{Bucket: "vault", Key: "deed.pdf", Version: lockedRef, BypassGovernance: true}))
		// Copies of another deployment's: a version, a delete marker.
		ignore(f.objects.Put(ctx, p, app.PutInput{
			Bucket: "mirror", Key: "r.txt", Body: bytes.NewReader([]byte("r")), Size: 1,
			Replica: &app.ReplicaWrite{Source: "site-b", Version: object.VersionRef{ID: version.VersionID}, ModifiedAt: testNow},
		}))
		ignore(f.objects.Delete(ctx, p, app.DeleteInput{
			Bucket: "mirror", Key: "r.txt", Replica: &app.ReplicaWrite{Source: "site-b", Version: object.VersionRef{ID: locked.VersionID}, ModifiedAt: testNow},
		}))
		ignore(f.objects.ReplicaBucket(ctx, p, "mirror", "site-b"))
		// Search, and an operation's first question asked before its key
		// is reserved (the requests of adminapi's askedFirstOf).
		ignore(services.search.Search(ctx, p, app.SearchInput{Query: "a", Limit: 10}))
		ignore(services.search.Search(ctx, p, app.SearchInput{Query: "a", Bucket: "photos", Limit: 10}))
		ignore(services.idempotency.Begin(ctx, app.IdempotentRequest{
			Scope: "POST /api/v1/users", Key: "k-" + p.Name(), Operation: "createUser", Principal: p,
			Action: iam.ActionPutUser, Resource: iam.Resource{Admin: "user/carl"},
		}))
		// Releases, the cluster and its settings.
		ignore(updates.Get(ctx, p))
		ignore(updates.Progress(ctx, p))
		ignore(updates.Check(ctx, p))
		ignore(updates.Start(ctx, p, "", "", "0.8.0"))
		ignore(services.cluster.Status(ctx, p))
		ignore(services.cluster.Maintenance(ctx, p))
		ignore(settings.Get(ctx, p))
		// Targets, notifications, replication.
		ignore(services.targets.Put(ctx, p, app.TargetInput{
			Name: "hook", Kind: delivery.KindWebhook, Endpoint: "https://hooks.example.com/in", Buckets: []string{"photos"},
		}))
		ignore(services.targets.List(ctx, p))
		ignore(services.targets.Get(ctx, p, "hook"))
		statusCtx, cancel := context.WithTimeout(ctx, time.Second)
		ignore(services.targets.Status(statusCtx, p, "hook"))
		cancel()
		ignore(services.notifications.Get(ctx, p, "photos"))
		ignore(services.notifications.Set(ctx, p, "photos", bucket.NotificationConfig{}))
		ignore(services.notifications.Delete(ctx, p, "photos"))
		ignore(services.replication.Configuration(ctx, p, "photos"))
		ignore(services.replication.SetConfiguration(ctx, p, "photos", bucket.ReplicationConfig{}))
		ignore(services.replication.DeleteConfiguration(ctx, p, "photos"))
		ignore(services.replication.Status(ctx, p, "photos"))
		ignore(services.replication.Resync(ctx, p, "photos"))
		ignore(services.replication.ReplicaMode(ctx, p, "photos"))
		ignore(services.replication.SetReplicaMode(ctx, p, "spare", ""))
		ignore(services.targets.Delete(ctx, p, "hook"))
		// Quotas and the catalogue of tags.
		ignore(services.quotas.Set(ctx, p, "photos", 1<<30))
		ignore(services.quotas.Get(ctx, p, "photos"))
		ignore(services.quotas.Delete(ctx, p, "photos"))
		ignore(services.tags.Create(ctx, p, app.TagDefinitionInput{Key: "team", Values: []string{"a"}}))
		ignore(services.tags.List(ctx, p))
		ignore(services.tags.Get(ctx, p, "team"))
		ignore(services.tags.Delete(ctx, p, "team"))
		// A tag key may hold a '/' ({tag_key}).
		ignore(services.tags.Create(ctx, p, app.TagDefinitionInput{Key: "team/x", Values: []string{"a"}}))
		ignore(services.tags.Get(ctx, p, "team/x"))
		ignore(services.tags.Delete(ctx, p, "team/x"))
	}
	script(root)
	nobody := iam.Principal{AccessKeyID: "AKIANOBODY0000000000", User: "nobody", Session: session("10.0.0.3")}
	script(nobody)
	// A locked session asks to repeat a request (auth:RepeatRequest) before
	// a key is reserved.
	lockedSession := iam.Principal{User: "nobody", Session: session("10.0.0.3"), LockedSession: true}
	ignore := func(...any) {}
	ignore(services.idempotency.Begin(ctx, app.IdempotentRequest{
		Scope: "POST /api/v1/users", Key: "k-locked", Operation: "createUser", Principal: lockedSession,
		Action: iam.ActionPutUser, Resource: iam.Resource{Admin: "user/carl"},
	}))

	asked := map[iam.Action]bool{}
	for _, request := range append(f.authz.Requests(), f.authz.Filtered()...) {
		asked[request.Action] = true
		if request.Action == app.ActionRepeatRequest {
			// Not an action a policy grants, and so not in the catalogue:
			// the authorizer refuses it to a locked session by a rule of
			// its own (ADR-0027), and no other principal is asked it.
			if !request.Principal.LockedSession || request.Resource != (iam.Resource{Admin: "request"}) {
				t.Errorf("%s asked of %s on %s", request.Action, request.Principal.Name(), request.Resource)
			}
			continue
		}
		entry, ok := iam.LookupAction(request.Action)
		if !ok {
			t.Errorf("%s on %s: the action is not in the catalogue", request.Action, request.Resource)
			continue
		}
		if !entry.AskedOn(request.Resource) {
			t.Errorf("%s is asked on %s, which fits none of its templates %v", request.Action, request.Resource, entry.Resources)
		}
	}
	// And the walk asked every action of the catalogue, but those only an
	// adapter asks.
	if !asked[app.ActionRepeatRequest] {
		t.Errorf("no use case asked %s: the walk did not reach it", app.ActionRepeatRequest)
	}
	for _, action := range iam.Catalogue() {
		if _, adapter := askedByAdapters()[action.Name]; adapter == asked[action.Name] {
			if adapter {
				t.Errorf("%s is listed as asked only by an adapter, and a use case asked it: take it off the list", action.Name)
			} else {
				t.Errorf("no use case asked %s: the walk did not reach it", action.Name)
			}
		}
	}
}

// askedByAdapters are the actions of the catalogue that no use case asks -
// an inbound adapter asks them itself - each with where, and the test of
// that adapter which holds the question to the catalogue.
func askedByAdapters() map[iam.Action]string {
	return map[iam.Action]string{
		iam.ActionGetMetrics: "the scrape of /metrics, asked by the telemetry adapter (internal/adapters/telemetry/http.go); " +
			"held to the catalogue by TestMetricsRequireAuthenticationAndAuthorization's check of its templates",
	}
}
