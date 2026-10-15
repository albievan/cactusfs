package app_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// The marks of the secret scan: a secret, the path of an endpoint that is a
// credential of its own, and the user of a NATS server. Neither of the
// first two may be found anywhere but sealed.
const (
	concealedMark = "MARKED-target-secret-7f3a9c"
	concealedPath = "/hooks/MARKED-path-token-51d2e8"
)

// targetFixture is the target use cases over the in-memory stores of the
// IAM fixture, whose audit trail the target store writes to.
type targetFixture struct {
	*iamFixture
	targets *app.TargetService
}

func newTargetFixture(t *testing.T) *targetFixture {
	t.Helper()
	f := newIAMFixture(t)
	f.meta.Audit = f.store
	return &targetFixture{
		iamFixture: f,
		targets: app.NewTargetService(app.TargetServiceConfig{
			Store: f.meta, Authz: f.authz, Auditor: f.auditor, KMS: f.kms, Clock: f.clock, Entropy: rand.Reader,
		}),
	}
}

// webhook is a well-formed definition of a webhook with a bearer token.
func webhook(name string) app.TargetInput {
	return app.TargetInput{
		Name: name, Kind: delivery.KindWebhook, Endpoint: "https://hooks.example.com:8443" + concealedPath,
		Buckets: []string{"photos", "logs-*"}, Auth: delivery.Auth{Mode: delivery.AuthBearer}, Secret: concealedMark,
	}
}

// open opens a sealed field of a target with the additional data given.
func (f *targetFixture) open(t *testing.T, sealed iam.SealedSecret, aad []byte) (string, error) {
	t.Helper()
	key, err := f.kms.UnwrapDataKey(context.Background(), sealed.WrappedKey, aad)
	if err != nil {
		return "", err
	}
	plain, err := sealed.Open(key, aad)
	return string(plain), err
}

// selfSigned returns a certificate in PEM, as a target is verified against.
func selfSigned(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "receiver"},
		NotBefore: testNow.Add(-time.Hour), NotAfter: testNow.Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestTargetsAreAuthorized: every use case of targets asks the authorizer
// for the action the contract names, on the target or on the list; a
// refusal is audited and changes nothing.
func TestTargetsAreAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTargetFixture(t)
	if _, err := f.targets.Put(ctx, f.root, webhook("hook")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	principals := map[string]iam.Principal{
		iam.RoleClusterAdmin: f.as(t, f.user(t, "admin-user", iam.RoleClusterAdmin)),
		iam.RoleAuditor:      f.as(t, f.user(t, "auditor-user", iam.RoleAuditor)),
		iam.RoleBucketAdmin:  f.as(t, f.user(t, "bucket-user", iam.RoleBucketAdmin)),
		iam.RoleReadOnly:     f.as(t, f.user(t, "reader-user", iam.RoleReadOnly)),
	}
	operations := []struct {
		name     string
		action   iam.Action
		resource string
		call     func(p iam.Principal) error
		// allowed are the roles that may.
		allowed []string
	}{
		{"list", iam.ActionListTargets, "arn:cactusfs:admin:::targets", func(p iam.Principal) error {
			_, err := f.targets.List(ctx, p)
			return err
		}, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{"get", iam.ActionGetTarget, "arn:cactusfs:admin:::target/hook", func(p iam.Principal) error {
			_, err := f.targets.Get(ctx, p, "hook")
			return err
		}, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{"put", iam.ActionPutTarget, "arn:cactusfs:admin:::target/hook", func(p iam.Principal) error {
			in := webhook("hook")
			in.Secret = ""
			_, err := f.targets.Put(ctx, p, in)
			return err
		}, []string{iam.RoleClusterAdmin}},
		{"delete", iam.ActionDeleteTarget, "arn:cactusfs:admin:::target/other", func(p iam.Principal) error {
			return f.targets.Delete(ctx, p, "other")
		}, []string{iam.RoleClusterAdmin}},
	}
	for _, op := range operations {
		for role, p := range principals {
			if op.name == "delete" && role == iam.RoleClusterAdmin {
				// What is deleted must be there first.
				if _, err := f.targets.Put(ctx, f.root, webhook("other")); err != nil {
					t.Fatal(err)
				}
			}
			err := op.call(p)
			allowed := false
			for _, r := range op.allowed {
				allowed = allowed || r == role
			}
			switch {
			case allowed && err != nil:
				t.Errorf("%s by %s: %v", op.name, role, err)
			case !allowed && !errors.Is(err, domain.ErrAccessDenied):
				t.Errorf("%s by %s = %v, want ErrAccessDenied", op.name, role, err)
			}
			if allowed {
				continue
			}
			// The refusal is in the trail, for the action and the
			// resource the contract names.
			found := false
			for _, r := range f.trail(t) {
				found = found || (r.Outcome == audit.Denied && r.Action == string(op.action) && r.Resource == op.resource && r.Actor == "user/"+userOf(role))
			}
			if !found {
				t.Errorf("%s by %s: no denial of %s on %s in the trail", op.name, role, op.action, op.resource)
			}
		}
	}
	// What was refused changed nothing: the target is as root left it.
	shown, err := f.targets.Get(ctx, f.root, "hook")
	if err != nil || !shown.SecretSet || shown.Origin != "https://hooks.example.com:8443" {
		t.Errorf("Get after the refusals = %+v, %v", shown, err)
	}
}

// userOf names the user of a role in TestTargetsAreAuthorized.
func userOf(role string) string {
	return map[string]string{
		iam.RoleClusterAdmin: "admin-user", iam.RoleAuditor: "auditor-user", iam.RoleBucketAdmin: "bucket-user", iam.RoleReadOnly: "reader-user",
	}[role]
}

// TestTargetEndpointsThatAreRefused: an endpoint is judged before anything
// is sealed or proposed, by its class - and the refusal never repeats it.
func TestTargetEndpointsThatAreRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTargetFixture(t)
	for name, tt := range map[string]struct {
		kind     delivery.Kind
		endpoint string
	}{
		"in the clear":                 {delivery.KindWebhook, "http://hooks.example.com" + concealedPath},
		"the scheme of another kind":   {delivery.KindWebhook, "tls://hooks.example.com" + concealedPath},
		"nats over https":              {delivery.KindNATS, "https://nats.example.com"},
		"a user and a password":        {delivery.KindWebhook, "https://user:" + concealedMark + "@hooks.example.com" + concealedPath},
		"a query":                      {delivery.KindWebhook, "https://hooks.example.com" + concealedPath + "?token=" + concealedMark},
		"a fragment":                   {delivery.KindWebhook, "https://hooks.example.com" + concealedPath + "#" + concealedMark},
		"a backslash":                  {delivery.KindWebhook, "https://hooks.example.com\\" + concealedMark},
		"a space":                      {delivery.KindWebhook, "https://hooks.example.com/ " + concealedMark},
		"a port that is none":          {delivery.KindWebhook, "https://hooks.example.com:70000" + concealedPath},
		"the metadata service":         {delivery.KindWebhook, "https://169.254.169.254" + concealedPath},
		"the unspecified address":      {delivery.KindWebhook, "https://0.0.0.0" + concealedPath},
		"multicast":                    {delivery.KindWebhook, "https://224.0.0.1" + concealedPath},
		"a name that ends in a number": {delivery.KindWebhook, "https://hooks.123" + concealedPath},
		"a path for nats":              {delivery.KindNATS, "tls://nats.example.com:4222" + concealedPath},
		"a path for another site":      {delivery.KindS3, "https://s3.site-b.example" + concealedPath},
		"too long":                     {delivery.KindWebhook, "https://hooks.example.com/" + strings.Repeat("a", delivery.MaxEndpointLength)},
	} {
		in := webhook("refused")
		in.Kind, in.Endpoint = tt.kind, tt.endpoint
		switch tt.kind {
		case delivery.KindNATS:
			in.Subject, in.Auth = "cactusfs.events", delivery.Auth{Mode: delivery.AuthToken}
		case delivery.KindS3:
			in.Region, in.Auth = "site-b", delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEBEXAMPLE0001"}
		}
		_, err := f.targets.Put(ctx, f.root, in)
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Put = %v, want ErrInvalidArgument", name, err)
			continue
		}
		if strings.Contains(err.Error(), concealedMark) || strings.Contains(err.Error(), "MARKED") || strings.Contains(err.Error(), "hooks.") {
			t.Errorf("%s: the refusal repeats the endpoint: %v", name, err)
		}
	}
	if targets, err := f.targets.List(ctx, f.root); err != nil || len(targets) != 0 {
		t.Errorf("after the refusals the targets are %v, %v", targets, err)
	}
	if strings.Contains(f.logs.String(), "MARKED") || strings.Contains(fmt.Sprint(f.trail(t)), "MARKED") {
		t.Error("a refused endpoint is in the log or the audit trail")
	}
}

// TestTargetPut walks through what a definition and its changes do: what is
// sealed and how, what a change without a secret or an endpoint keeps, the
// signing key, and the changes that are refused.
func TestTargetPut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTargetFixture(t)

	made, err := f.targets.Put(ctx, f.root, webhook("hook"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if made.SigningKey == "" || iam.ValidateSecret(made.SigningKey) != nil {
		t.Fatalf("a new webhook was given the signing key %q", made.SigningKey)
	}
	want := delivery.ShownTarget{
		Name: "hook", Kind: delivery.KindWebhook, Origin: "https://hooks.example.com:8443", PathSet: true,
		Buckets: []string{"photos", "logs-*"}, Auth: delivery.Auth{Mode: delivery.AuthBearer}, SecretSet: true, SignKeySet: true,
		CreatedAt: testNow, UpdatedAt: testNow,
	}
	if got := made.Target; fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
		t.Errorf("Put answered\n%#v\nwant\n%#v", got, want)
	}

	// Stored sealed, each field under the additional data that names the
	// target and the field: moved to another, it does not open.
	stored, err := f.meta.GetTarget(ctx, "hook")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Endpoint != "https://hooks.example.com:8443" {
		t.Errorf("the endpoint is kept as %q, want the origin", stored.Endpoint)
	}
	for _, field := range []struct {
		name   string
		sealed iam.SealedSecret
		aad    []byte
		// other is the additional data of the same field of another
		// target.
		other []byte
		want  string
	}{
		{"secret", stored.Secret, delivery.SecretAAD("hook"), delivery.SecretAAD("other"), concealedMark},
		{"signing key", stored.SignKey, delivery.SignKeyAAD("hook"), delivery.SignKeyAAD("other"), made.SigningKey},
		{"path", stored.SealedPath, delivery.PathAAD("hook"), delivery.PathAAD("other"), concealedPath},
	} {
		if field.sealed.KMSKeyID != f.kms.KeyID() || len(field.sealed.WrappedKey) == 0 {
			t.Errorf("%s: not sealed through the KMS: key %q", field.name, field.sealed.KMSKeyID)
		}
		if got, err := f.open(t, field.sealed, field.aad); err != nil || got != field.want {
			t.Errorf("%s does not open to what was given: %v", field.name, err)
		}
		if _, err := f.open(t, field.sealed, field.other); err == nil {
			t.Errorf("%s opens with the additional data of the same field of another target", field.name)
		}
	}
	if _, err := f.open(t, stored.Secret, delivery.SignKeyAAD("hook")); err == nil {
		t.Error("the secret opens as the signing key")
	}

	// A change without a secret, an endpoint or a new key keeps all three;
	// the rest is replaced.
	f.clock.Advance(time.Minute)
	change := webhook("hook")
	change.Endpoint, change.Secret, change.Buckets, change.Audit = "", "", []string{"*"}, true
	changed, err := f.targets.Put(ctx, f.root, change)
	if err != nil {
		t.Fatalf("Put without a secret: %v", err)
	}
	after, err := f.meta.GetTarget(ctx, "hook")
	if err != nil {
		t.Fatal(err)
	}
	if changed.SigningKey != "" || !changed.Target.SecretSet || !changed.Target.SignKeySet || !changed.Target.PathSet ||
		!changed.Target.Audit || changed.Target.Buckets[0] != "*" || !changed.Target.CreatedAt.Equal(testNow) ||
		!changed.Target.UpdatedAt.Equal(testNow.Add(time.Minute)) {
		t.Errorf("the change answered %+v, signing key given: %v", changed.Target, changed.SigningKey != "")
	}
	if fmt.Sprint(after.Secret) != fmt.Sprint(stored.Secret) || fmt.Sprint(after.SignKey) != fmt.Sprint(stored.SignKey) ||
		fmt.Sprint(after.SealedPath) != fmt.Sprint(stored.SealedPath) || after.Endpoint != stored.Endpoint {
		t.Error("a change without a secret, an endpoint or a new key did not keep them as they were")
	}

	// A new signing key is made when asked for, and returned once.
	rotate := change
	rotate.RotateSigningKey = true
	rotated, err := f.targets.Put(ctx, f.root, rotate)
	if err != nil || rotated.SigningKey == "" || rotated.SigningKey == made.SigningKey {
		t.Fatalf("Put with rotate_signing_key = %v; a new key: %v", err, rotated.SigningKey != made.SigningKey)
	}
	after, err = f.meta.GetTarget(ctx, "hook")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.open(t, after.SignKey, delivery.SignKeyAAD("hook")); err != nil || got != rotated.SigningKey {
		t.Errorf("the stored signing key is not the one returned: %v", err)
	}

	// A new endpoint replaces the path; one without a path has none.
	moved := change
	moved.Endpoint = "https://other.example.com/"
	shown, err := f.targets.Put(ctx, f.root, moved)
	if err != nil || shown.Target.PathSet || shown.Target.Origin != "https://other.example.com" {
		t.Errorf("Put of an endpoint without a path = %+v, %v", shown.Target, err)
	}

	// The mode none removes the secret.
	none := change
	none.Auth = delivery.Auth{}
	if shown, err := f.targets.Put(ctx, f.root, none); err != nil || shown.Target.SecretSet || shown.Target.Auth.Mode != delivery.AuthNone {
		t.Errorf("Put with the mode none = %+v, %v", shown.Target, err)
	}

	// A NATS server with a user, its certificates checked.
	nats := app.TargetInput{
		Name: "bus", Kind: delivery.KindNATS, Endpoint: "tls://nats.example.com", Subject: "cactusfs.events",
		CAPEM: selfSigned(t), Buckets: []string{}, Auth: delivery.Auth{Mode: delivery.AuthBasic, ID: "cactusfs"}, Secret: concealedMark,
	}
	if shown, err := f.targets.Put(ctx, f.root, nats); err != nil || shown.SigningKey != "" || shown.Target.SignKeySet || !shown.Target.SecretSet {
		t.Errorf("Put of a NATS server = %+v, signing key %v, %v", shown.Target, shown.SigningKey != "", err)
	}

	for name, tt := range map[string]struct {
		in   func() app.TargetInput
		want error
	}{
		"the kind of a target changed": {func() app.TargetInput {
			in := nats
			in.Name = "hook"
			return in
		}, domain.ErrConflict},
		"a new target without an endpoint": {func() app.TargetInput {
			in := webhook("new")
			in.Endpoint = ""
			return in
		}, domain.ErrInvalidArgument},
		"a change of the mode without a secret": {func() app.TargetInput {
			in := nats
			in.Auth, in.Secret = delivery.Auth{Mode: delivery.AuthToken}, ""
			return in
		}, domain.ErrInvalidArgument},
		"a mode that needs a secret, on a target without one": {func() app.TargetInput {
			in := change
			in.Auth = delivery.Auth{Mode: delivery.AuthBearer}
			return in
		}, domain.ErrInvalidArgument},
		"a secret without a mode": {func() app.TargetInput {
			in := webhook("new")
			in.Auth = delivery.Auth{}
			return in
		}, domain.ErrInvalidArgument},
		"a secret that is too long": {func() app.TargetInput {
			in := webhook("new")
			in.Secret = strings.Repeat("s", app.MaxTargetSecretLength+1)
			return in
		}, domain.ErrInvalidArgument},
		"a new signing key for NATS": {func() app.TargetInput {
			in := nats
			in.RotateSigningKey = true
			return in
		}, domain.ErrInvalidArgument},
		"certificates that are none": {func() app.TargetInput {
			in := webhook("new")
			in.CAPEM = []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
			return in
		}, domain.ErrInvalidArgument},
		"a name no target can have": {func() app.TargetInput {
			return webhook("Not_A_Name")
		}, domain.ErrInvalidArgument},
		"a kind there is not": {func() app.TargetInput {
			in := webhook("new")
			in.Kind = "smtp"
			return in
		}, domain.ErrInvalidArgument},
		"a bucket pattern that is none": {func() app.TargetInput {
			in := webhook("new")
			in.Buckets = []string{"Photos!"}
			return in
		}, domain.ErrInvalidArgument},
	} {
		if _, err := f.targets.Put(ctx, f.root, tt.in()); !errors.Is(err, tt.want) {
			t.Errorf("%s: Put = %v, want %v", name, err, tt.want)
		}
	}

	// Every change is in the trail, with what was done and nothing of
	// what was given.
	var changes []audit.Record
	for _, r := range f.trail(t) {
		if r.Action == string(iam.ActionPutTarget) && r.Outcome == audit.Succeeded {
			changes = append(changes, r)
		}
	}
	if len(changes) != 6 {
		t.Fatalf("%d changes of targets in the trail, want 6: %v", len(changes), changes)
	}
	first := changes[0]
	if first.Resource != "arn:cactusfs:admin:::target/hook" || first.Actor != "root" || first.Details["signing_key"] != "made" ||
		first.Details["secret"] != "set" || first.Details["created"] != "true" || first.Details["origin"] != "https://hooks.example.com:8443" {
		t.Errorf("the record of the definition is %+v", first)
	}
	if kept := changes[1].Details; kept["secret"] != "kept" || kept["endpoint"] != "kept" || kept["signing_key"] != "kept" {
		t.Errorf("the record of the change is %v", kept)
	}
	scanForMarks(t, f, made.SigningKey, rotated.SigningKey)
}

// scanForMarks fails when a secret, a path or a signing key is anywhere it
// may not be: the log, the audit trail, a target as it is shown and as it
// is printed.
func scanForMarks(t *testing.T, f *targetFixture, signingKeys ...string) {
	t.Helper()
	ctx := context.Background()
	shown, err := f.targets.List(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.meta.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trail, err := json.Marshal(f.trail(t))
	if err != nil {
		t.Fatal(err)
	}
	answers, err := json.Marshal(shown)
	if err != nil {
		t.Fatal(err)
	}
	places := map[string]string{
		"the log": f.logs.String(), "the audit trail": string(trail), "an answer": string(answers) + fmt.Sprintf("%#v", shown),
		"a printed target": fmt.Sprintf("%v %+v %#v %s", stored, stored, stored, stored),
	}
	for _, target := range stored {
		data, err := json.Marshal(target)
		if err != nil {
			t.Fatal(err)
		}
		places["a printed target"] += string(data)
	}
	for place, text := range places {
		for _, mark := range append([]string{concealedMark, "MARKED", "sealed-"}, signingKeys...) {
			if mark != "" && strings.Contains(text, mark) {
				t.Errorf("%s holds %q", place, mark)
			}
		}
	}
}

// TestTargetsInUse: a target that a bucket's rules name cannot be deleted,
// nor stop admitting that bucket; once the rules are gone it can.
func TestTargetsInUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTargetFixture(t)
	if _, err := f.targets.Put(ctx, f.root, webhook("hook")); err != nil {
		t.Fatal(err)
	}
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	document, err := bucket.NotificationConfig{Rules: []bucket.NotificationRule{
		{ID: "all", As: bucket.NotifyAsQueue, Target: "hook", On: []delivery.Event{delivery.EventPut}},
	}}.Document()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.meta.SetBucketConfig(ctx, "photos", bucket.ConfigNotification, document); err != nil {
		t.Fatal(err)
	}
	shown, err := f.targets.Get(ctx, f.root, "hook")
	if err != nil || shown.Refs != 1 {
		t.Fatalf("Get of a target that rules name = %+v, %v", shown, err)
	}

	narrowed := webhook("hook")
	narrowed.Secret, narrowed.Buckets = "", []string{"logs-*"}
	for name, tt := range map[string]struct {
		call func() error
		want error
	}{
		"deleted while named": {func() error { return f.targets.Delete(ctx, f.root, "hook") }, domain.ErrEntityInUse},
		"narrowed past the bucket that names it": {func() error {
			_, err := f.targets.Put(ctx, f.root, narrowed)
			return err
		}, domain.ErrEntityInUse},
		"deleted when it is not there": {func() error { return f.targets.Delete(ctx, f.root, "missing") }, domain.ErrEntityNotFound},
		"read when it is not there": {func() error {
			_, err := f.targets.Get(ctx, f.root, "missing")
			return err
		}, domain.ErrEntityNotFound},
	} {
		if err := tt.call(); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", name, err, tt.want)
		}
	}

	// Without the rules, both are taken.
	if err := f.meta.SetBucketConfig(ctx, "photos", bucket.ConfigNotification, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.targets.Put(ctx, f.root, narrowed); err != nil {
		t.Errorf("narrowing a target nothing names: %v", err)
	}
	if err := f.targets.Delete(ctx, f.root, "hook"); err != nil {
		t.Errorf("deleting a target nothing names: %v", err)
	}
	deleted := false
	for _, r := range f.trail(t) {
		deleted = deleted || (r.Action == string(iam.ActionDeleteTarget) && r.Outcome == audit.Succeeded && r.Resource == "arn:cactusfs:admin:::target/hook")
	}
	if !deleted {
		t.Error("the deletion is not in the audit trail")
	}
}

// TestTargetsWhenTheStoreCannot: what the store and the KMS answer is passed
// on - until every member knows targets a change is unavailable - and a
// failure leaves nothing behind.
func TestTargetsWhenTheStoreCannot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tt := range map[string]struct {
		breakIt func(f *targetFixture)
		want    error
	}{
		"not every member knows schema 5": {func(f *targetFixture) { f.meta.FailWrites = fmt.Errorf("%w: schema 5", domain.ErrUnavailable) }, domain.ErrUnavailable},
		"the KMS cannot make a key":       {func(f *targetFixture) { f.kms.FailGenerate = domain.ErrKeyUnavailable }, domain.ErrKeyUnavailable},
	} {
		f := newTargetFixture(t)
		tt.breakIt(f)
		if _, err := f.targets.Put(ctx, f.root, webhook("hook")); !errors.Is(err, tt.want) {
			t.Errorf("%s: Put = %v, want %v", name, err, tt.want)
		}
		if err := f.targets.Delete(ctx, f.root, "hook"); err == nil {
			t.Errorf("%s: Delete succeeded", name)
		}
		if targets, err := f.targets.List(ctx, f.root); err != nil || len(targets) != 0 {
			t.Errorf("%s: List = %v, %v", name, targets, err)
		}
	}
}

// countingKMS counts the data keys it is asked for.
type countingKMS struct {
	ports.KMS
	generated atomic.Int32
}

// GenerateDataKey implements ports.KMS.
func (k *countingKMS) GenerateDataKey(ctx context.Context, aad []byte) (ports.DataKey, error) {
	k.generated.Add(1)
	return k.KMS.GenerateDataKey(ctx, aad)
}

// TestARefusedTargetCostsTheKMSNothing: a target that cannot be kept is
// refused from its draft, before a data key is asked for.
func TestARefusedTargetCostsTheKMSNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTargetFixture(t)
	kms := &countingKMS{KMS: f.kms}
	targets := app.NewTargetService(app.TargetServiceConfig{
		Store: f.meta, Authz: f.authz, Auditor: f.auditor, KMS: kms, Clock: f.clock, Entropy: rand.Reader,
	})
	for name, change := range map[string]func(in *app.TargetInput){
		"a secret without a mode":           func(in *app.TargetInput) { in.Auth = delivery.Auth{} },
		"a bucket pattern that is none":     func(in *app.TargetInput) { in.Buckets = []string{"Photos!"} },
		"a user where the mode has none":    func(in *app.TargetInput) { in.Auth.ID = "somebody" },
		"a mode a webhook does not know":    func(in *app.TargetInput) { in.Auth.Mode = delivery.AuthToken },
		"a subject for a webhook":           func(in *app.TargetInput) { in.Subject = "events" },
		"a NATS server without its subject": func(in *app.TargetInput) { in.Kind, in.Endpoint = delivery.KindNATS, "tls://nats.example.com" },
		"another site without its region": func(in *app.TargetInput) {
			in.Kind, in.Endpoint, in.Auth = delivery.KindS3, "https://s3.site-b.example", delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEBEXAMPLE0001"}
		},
	} {
		in := webhook("draft")
		change(&in)
		if _, err := targets.Put(ctx, f.root, in); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Put = %v, want ErrInvalidArgument", name, err)
		}
		if n := kms.generated.Load(); n != 0 {
			t.Fatalf("%s: the KMS was asked for %d data keys for a target that was refused", name, n)
		}
	}
	// What is kept is sealed: the path, the secret and the signing key.
	if _, err := targets.Put(ctx, f.root, webhook("draft")); err != nil {
		t.Fatal(err)
	}
	if n := kms.generated.Load(); n != 3 {
		t.Errorf("a webhook with a path, a secret and a signing key asked for %d data keys, want 3", n)
	}
}

// actor marks the context of one of two changes that race.
type actor struct{}

// racingStore makes the worst interleaving of two changes as likely as it
// can be: the change marked "rotate" waits, before it is proposed, until the
// change marked "keep" has read the target, and "keep" is proposed only
// after "rotate" was - each waiting a while at most, for a change that is
// held back elsewhere does not come.
type racingStore struct {
	ports.TargetStore
	keepRead chan struct{}
	once     sync.Once
	inPut    chan struct{}
	rotated  chan struct{}
}

// GetTarget implements ports.TargetStore.
func (s *racingStore) GetTarget(ctx context.Context, name string) (delivery.Target, error) {
	target, err := s.TargetStore.GetTarget(ctx, name)
	if ctx.Value(actor{}) == "keep" {
		// Once the target has been read, not before.
		s.once.Do(func() { close(s.keepRead) })
	}
	return target, err
}

// PutTarget implements ports.TargetStore.
func (s *racingStore) PutTarget(ctx context.Context, t delivery.Target, record audit.Record) error {
	if ctx.Value(actor{}) == "rotate" {
		close(s.inPut)
		select {
		case <-s.keepRead:
		case <-time.After(300 * time.Millisecond):
		}
		defer close(s.rotated)
	}
	if ctx.Value(actor{}) == "keep" {
		select {
		case <-s.rotated:
		case <-time.After(300 * time.Millisecond):
		}
	}
	return s.TargetStore.PutTarget(ctx, t, record)
}

// TestConcurrentChangesCannotUndoARotation: a change that keeps the signing
// key, made while a rotation of it is between its reading and its proposal,
// waits for the rotation, and keeps the new key.
func TestConcurrentChangesCannotUndoARotation(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	store := &racingStore{TargetStore: f.meta, keepRead: make(chan struct{}), inPut: make(chan struct{}), rotated: make(chan struct{})}
	targets := app.NewTargetService(app.TargetServiceConfig{
		Store: store, Authz: f.authz, Auditor: f.auditor, KMS: f.kms, Clock: f.clock, Entropy: rand.Reader,
	})
	if _, err := targets.Put(context.Background(), f.root, webhook("hook")); err != nil {
		t.Fatal(err)
	}
	rotate, keep := webhook("hook"), webhook("hook")
	rotate.Secret, rotate.Endpoint, rotate.RotateSigningKey = "", "", true
	keep.Secret, keep.Endpoint, keep.Buckets = "", "", []string{"*"}

	var (
		wg      sync.WaitGroup
		rotated app.TargetPutResult
		errs    [2]error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		rotated, errs[0] = targets.Put(context.WithValue(context.Background(), actor{}, "rotate"), f.root, rotate)
	}()
	<-store.inPut
	go func() {
		defer wg.Done()
		_, errs[1] = targets.Put(context.WithValue(context.Background(), actor{}, "keep"), f.root, keep)
	}()
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || rotated.SigningKey == "" {
		t.Fatalf("the two changes: %v, %v", errs[0], errs[1])
	}
	stored, err := f.meta.GetTarget(context.Background(), "hook")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.open(t, stored.SignKey, delivery.SignKeyAAD("hook")); err != nil || got != rotated.SigningKey {
		t.Errorf("after a rotation and a change that keeps the key, the stored key is not the rotated one (%v)", err)
	}
	if len(stored.Buckets) != 1 || stored.Buckets[0] != "*" {
		t.Errorf("the change that came second is not the one stored: buckets %v", stored.Buckets)
	}
}

// heldStore holds the first proposal of a change of the target named
// "hook" until it is let go; every other is proposed at once.
type heldStore struct {
	ports.TargetStore
	held    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

// PutTarget implements ports.TargetStore.
func (s *heldStore) PutTarget(ctx context.Context, t delivery.Target, record audit.Record) error {
	if t.Name == "hook" && s.held.CompareAndSwap(false, true) {
		s.entered <- struct{}{}
		<-s.release
	}
	return s.TargetStore.PutTarget(ctx, t, record)
}

// TestAChangeThatStopsWaitingForItsName: a change that waits for the name
// another change holds stops when its context ends, with the error of the
// context and nothing done; the name is free once the other is done, and
// no lock is left behind.
func TestAChangeThatStopsWaitingForItsName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTargetFixture(t)
	store := &heldStore{TargetStore: f.meta, entered: make(chan struct{}), release: make(chan struct{})}
	targets := app.NewTargetService(app.TargetServiceConfig{
		Store: store, Authz: f.authz, Auditor: f.auditor, KMS: f.kms, Clock: f.clock, Entropy: rand.Reader,
	})

	holding := make(chan error, 1)
	go func() {
		_, err := targets.Put(ctx, f.root, webhook("hook"))
		holding <- err
	}()
	<-store.entered
	if n := app.HeldTargetNames(targets); n != 1 {
		t.Errorf("%d names locked while one change is proposed, want 1", n)
	}

	// A change and a deletion of the name wait, and stop with their
	// contexts.
	waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := targets.Put(waiting, f.root, webhook("hook")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a change that waited past its deadline = %v, want context.DeadlineExceeded", err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if err := targets.Delete(cancelled, f.root, "hook"); !errors.Is(err, context.Canceled) {
		t.Errorf("a deletion whose context ended = %v, want context.Canceled", err)
	}
	// Another name is not held by it.
	if _, err := targets.Put(ctx, f.root, webhook("other")); err != nil {
		t.Errorf("a change of another name while hook is held: %v", err)
	}

	store.release <- struct{}{}
	if err := <-holding; err != nil {
		t.Fatalf("the change that held the name: %v", err)
	}
	// The name is free again, and usable.
	again := webhook("hook")
	again.Secret, again.Endpoint, again.Buckets = "", "", []string{"*"}
	if _, err := targets.Put(ctx, f.root, again); err != nil {
		t.Errorf("a change of the name once it is free: %v", err)
	}
	if err := targets.Delete(ctx, f.root, "hook"); err != nil {
		t.Errorf("a deletion of the name once it is free: %v", err)
	}
	if n := app.HeldTargetNames(targets); n != 0 {
		t.Errorf("%d names are still locked once every change is done", n)
	}
	// What gave up changed nothing: the trail has the four changes made.
	changes := 0
	for _, r := range f.trail(t) {
		if (r.Action == string(iam.ActionPutTarget) || r.Action == string(iam.ActionDeleteTarget)) && r.Outcome == audit.Succeeded {
			changes++
		}
	}
	if changes != 4 {
		t.Errorf("%d changes of targets in the trail, want 4", changes)
	}
}
