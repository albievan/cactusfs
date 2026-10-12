//go:build integration && fixturegen && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// The names of the fixture. The test that opens it does not know them: it
// reads them from the manifest.
const (
	schema4FixtureViaS3    = "schema-four-s3"
	schema4FixtureViaAdmin = "schema-four-admin"
	schema4FixtureVersions = "schema-four-versions"
	schema4FixtureReborn   = "schema-four-reborn"
	schema4FixtureUnused   = "schema-four-unused"
	schema4FixtureLocked   = "schema-four-locked"
	schema4FixtureMany     = "schema-four-many"

	// schema4FixtureManyKeys is how many keys the bucket of many keys holds,
	// of one byte each: more than a walk of fifty at a time ends in a few
	// steps. schema4FixtureManyAtOnce is how many of them are on their way
	// at a time while the fixture is written.
	schema4FixtureManyKeys   = 300
	schema4FixtureManyAtOnce = 8

	schema4FixtureWriter   = "fixture-writer"
	schema4FixtureReader   = "fixture-reader"
	schema4FixtureOperator = "fixture-operator"
	schema4FixtureGroupOf  = "fixture-readers"
	schema4FixtureRoleOf   = "fixture-report-readers"
	schema4FixturePolicyOf = "fixture-reports-read"

	// schema4FixturePublic lets anybody read under public/ of the bucket
	// that was created through the management API.
	schema4FixturePublic = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::schema-four-admin/public/*"}]}`
	// schema4FixtureGrant is the policy of the fixture's own role: whoever
	// has it lists and reads the bucket that was created through S3.
	schema4FixtureGrant = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::schema-four-s3","arn:aws:s3:::schema-four-s3/*"]}]}`
	// schema4FixtureInline narrows the key of the user that only reads to
	// what is under reports/ of that bucket.
	schema4FixtureInline = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::schema-four-s3/reports/*"}]}`
	// schema4FixtureKept says of the bucket with Object Lock what whoever
	// meets it must know.
	schema4FixtureKept = "This bucket can never be emptied: one version is under a governance retention until the end of 2099 and one under a legal hold. A test that opens the fixture works on a copy and removes the copy as a directory."
)

// TestWriteSchema4Fixture writes testdata/schema-4 with the binary of the
// tree it is run in. It was run once, at commit 84334a7:
//
//	go test -tags='integration fixturegen' -run TestWriteSchema4Fixture -count=1 ./test/integration/
//
// and must not be run again on a later tree: the fixture is what THAT binary
// wrote. It is kept so that it can be seen how the fixture came to be, and
// refuses to write over one that exists.
//
// The node is a standalone one of four drives. It runs in a temporary
// directory and hands over what it counted every five seconds, the least the
// binary allows; when it has stopped, its directories are copied to the
// fixture, with a manifest of what was written (schema4FixtureContents). A
// node on its own writes at level 4 from its first start (ADR-0019): every
// put, copy and batch of reads below is an entry of schema 4, and every
// bucket is created with its creator. It holds:
//
//	schema-four-s3        created through S3 with the API key of the user
//	                      fixture-writer. Keeps no versions. Six uploads, by
//	                      the user and by root, one of them a copy: the
//	                      record of its latest use names the last three. Six
//	                      downloads that each have a row of their own -
//	                      signed by root, by the user and by the narrowed
//	                      key of fixture-reader, with a URL root presigned,
//	                      and through the management API - each handed over
//	                      before the next was made: the record names the
//	                      last three, in that order. One object never read;
//	                      an upload in progress of "film", three parts of a
//	                      few thousand bytes: the binary asks nothing of a
//	                      part's size before the upload is completed
//	schema-four-admin     created through the management API by
//	                      fixture-operator, signed in to the console. A
//	                      bucket policy: anybody reads under public/. One
//	                      object there, downloaded without a credential, and
//	                      one beside it that the policy does not reach. A
//	                      lifecycle that reaches nothing there is, CORS and
//	                      tags. Fewer rows than are kept
//	schema-four-versions  created through S3 by root. Keeps versions. "doc"
//	                      in two versions, by root and by the user, the
//	                      older one downloaded by its ID; "gone", one
//	                      version under a delete marker; "album", a
//	                      multipart upload the user completed: a part of
//	                      5 MiB, the least a part that is not the last may
//	                      be, and one of a thousand bytes
//	schema-four-reborn    created through S3 by root, uploaded to, downloaded
//	                      from, emptied and deleted; then created again, by
//	                      the user. Its record began with nothing, and holds
//	                      the one upload made since
//	schema-four-unused    created through the management API by root. Never
//	                      used: the state holds no record of it
//	schema-four-locked    created through S3 by root, with Object Lock.
//	                      "ledger", one version under a governance retention
//	                      until the end of 2099; "draft", one under a legal
//	                      hold. It can never be emptied, and is tiny
//	schema-four-many      created through S3 by root. Keeps no versions.
//	                      Three hundred keys, k/000 to k/299, of one byte
//	                      each, none downloaded: the last three by the
//	                      user, one after the other, so that the record of
//	                      its latest use is decided; the others by root,
//	                      eight at a time
//
// The manifest also says of every bucket what it holds in figures (its
// usage): counted here from what was written, and not read back - but the
// objects and their bytes are compared with what the node itself counts
// before it is stopped.
//
// and, besides root and what every deployment has from its first start: the
// policy fixture-reports-read, the role fixture-report-readers that has it,
// the group fixture-readers that gives the role; the users fixture-writer
// (bucket-admin), fixture-operator (bucket-admin, with a password) and
// fixture-reader (in the group, no role of its own); the API key of
// fixture-writer and that of fixture-reader, which an inline policy narrows
// to reports/.
//
// What is NOT in it, because no API of the binary of 84334a7 writes it:
// sub-buckets, tag rules and tag definitions (as in the schema-3 fixture).
func TestWriteSchema4Fixture(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", schema4FixtureDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture); err == nil {
		t.Fatalf("%s exists: the fixture is written once, by the binary it is a fixture of", fixture)
	}
	commit, err := exec.CommandContext(t.Context(), "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("which commit this tree is: %v", err)
	}
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	node, stop := schema4FixtureServer(ctx, t, bin, dir)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// manage sends a request that changes something to the management API,
	// as whoever the headers name, and returns the answer.
	manage := func(who map[string]string, path string, body any) []byte {
		t.Helper()
		status, _, answer := adminCall(ctx, t, http.MethodPost, node.admin+path, who, body)
		if status != http.StatusCreated {
			// Not the answer: the one to the creation of a key holds
			// its secret.
			t.Fatalf("POST %s = %d", path, status)
		}
		return answer
	}
	// key makes an API key of a user, with an inline policy or without.
	key := func(user, inline string) schema3Writer {
		t.Helper()
		body := map[string]any{}
		if inline != "" {
			body["inline_policy"] = json.RawMessage(inline)
		}
		var created struct {
			Key struct {
				AccessKeyID string `json:"access_key_id"`
			} `json:"key"`
			SecretKey string `json:"secret_key"`
		}
		if err := json.Unmarshal(manage(schema3RootBearer(), "/users/"+user+"/keys", body), &created); err != nil || created.Key.AccessKeyID == "" || created.SecretKey == "" {
			t.Fatalf("create a key of %s: no key in the answer (%v)", user, err)
		}
		return schema3Writer{User: user, Actor: "user/" + user, AccessKeyID: created.Key.AccessKeyID, SecretKey: created.SecretKey}
	}

	// Who there is. The policy, the role that has it, the group that gives
	// the role; then the users, and the two keys.
	contents := &schema4FixtureContents{Commit: strings.TrimSpace(string(commit))}
	manage(schema3RootBearer(), "/policies", map[string]any{"name": schema4FixturePolicyOf, "document": json.RawMessage(schema4FixtureGrant)})
	manage(schema3RootBearer(), "/roles", map[string]any{"name": schema4FixtureRoleOf, "policies": []string{schema4FixturePolicyOf}})
	manage(schema3RootBearer(), "/groups", map[string]any{"name": schema4FixtureGroupOf, "roles": []string{schema4FixtureRoleOf}})
	contents.IAM = schema4FixtureIAM{
		Users: []schema4FixtureUser{
			{Name: schema4FixtureWriter, Groups: []string{}, Roles: []string{"bucket-admin"}},
			{Name: schema4FixtureOperator, Groups: []string{}, Roles: []string{"bucket-admin"}, Console: true},
			{Name: schema4FixtureReader, Groups: []string{schema4FixtureGroupOf}, Roles: []string{}},
		},
		Groups:   []schema4FixtureGroup{{Name: schema4FixtureGroupOf, Roles: []string{schema4FixtureRoleOf}}},
		Roles:    []schema4FixtureRole{{Name: schema4FixtureRoleOf, Policies: []string{schema4FixturePolicyOf}}},
		Policies: []string{schema4FixturePolicyOf},
	}
	for _, u := range contents.IAM.Users {
		user := map[string]any{"name": u.Name, "groups": u.Groups, "roles": u.Roles}
		if u.Console {
			user["password"] = schema4FixturePassword()
		}
		manage(schema3RootBearer(), "/users", user)
	}
	contents.Writer = key(schema4FixtureWriter, "")
	contents.Scoped = schema4FixtureScoped{
		schema3Writer: key(schema4FixtureReader, schema4FixtureInline), InlinePolicy: schema4FixtureInline,
		Allowed: schema4FixtureTarget{Bucket: schema4FixtureViaS3, Key: "reports/2026/q1.txt"},
		Refused: schema4FixtureTarget{Bucket: schema4FixtureViaS3, Key: "data/blob.bin"},
	}
	user := node.client(contents.Writer.AccessKeyID, contents.Writer.SecretKey)
	scoped := node.client(contents.Scoped.AccessKeyID, contents.Scoped.SecretKey)
	clients := map[string]*s3.Client{schema3Root: node.root, contents.Writer.Actor: user, contents.Scoped.Actor: scoped}

	// made writes a bucket down that was just created.
	made := func(name, by, through string) *schema4FixtureBucket {
		b := &schema4FixtureBucket{
			Name: name, CreatedBy: by, CreatedThrough: through, Objects: []*schema4FixtureObject{}, Markers: []schema3Marker{}, Uploads: []schema3Upload{},
			Activity: schema4FixtureActivity{Uploads: []schema4Upload{}, Downloads: []schema4Download{}},
		}
		contents.Buckets = append(contents.Buckets, b)
		return b
	}
	// record writes a new version down, over whatever its key had: it is
	// an upload, by who made it.
	record := func(b *schema4FixtureBucket, who, key, version string, content []byte) *schema4FixtureObject {
		for _, o := range b.Objects {
			if o.Key == key {
				o.Latest = false
			}
		}
		o := &schema4FixtureObject{schema3Object: schema3Object{
			Key: key, VersionID: version, Latest: true, Size: int64(len(content)), SHA256: sumOf(content), UploadedBy: who, Readers: []schema3Reader{},
		}}
		b.Objects = append(b.Objects, o)
		b.Activity.uploaded(who, key)
		return o
	}
	put := func(b *schema4FixtureBucket, who, key string, content []byte) *schema4FixtureObject {
		t.Helper()
		out, err := clients[who].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(b.Name), Key: aws.String(key), Body: bytes.NewReader(content)})
		must("PutObject "+key, err)
		return record(b, who, key, aws.ToString(out.VersionId), content)
	}
	// download reads a version, signed, as often as it says. The node has
	// handed the reads over when it returns: so the next download is a
	// later one in the record of the bucket, and not one of the same batch,
	// whose order nothing here decides.
	download := func(b *schema4FixtureBucket, o *schema4FixtureObject, who string, times int) {
		t.Helper()
		for range times {
			got, err := schema3Get(ctx, clients[who], b.Name, &o.schema3Object)
			must("GetObject "+o.Key, err)
			if sumOf(got) != o.SHA256 {
				t.Fatalf("GetObject %s: not what was put", o.Key)
			}
			o.count(who, false, 1, 0)
		}
		b.Activity.downloaded(who, o.Key, false)
		schema4FixtureSettled(ctx, t, node, contents)
	}
	// fetch reads a version over plain HTTP, as often as it says: with a
	// presigned URL, without a credential, or through the management API.
	// It, too, returns when the reads are handed over.
	fetch := func(b *schema4FixtureBucket, o *schema4FixtureObject, address string, headers map[string]string, who string, presigned bool, times int) {
		t.Helper()
		for range times {
			status, _, got := adminCall(ctx, t, http.MethodGet, address, headers, nil)
			if status != http.StatusOK || sumOf(got) != o.SHA256 {
				t.Fatalf("download of %s as %s = %d, %d bytes", o.Key, who, status, len(got))
			}
			o.count(who, presigned, 1, 0)
		}
		b.Activity.downloaded(who, o.Key, presigned)
		schema4FixtureSettled(ctx, t, node, contents)
	}

	// (a) The bucket created through S3, by the key of the user. It keeps
	// no versions.
	_, err = user.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(schema4FixtureViaS3)})
	must("CreateBucket with the key of the user", err)
	plain := made(schema4FixtureViaS3, contents.Writer.Actor, "s3")
	q1 := put(plain, contents.Writer.Actor, "reports/2026/q1.txt", []byte("first quarter"))
	q2 := put(plain, contents.Writer.Actor, "reports/2026/q2.txt", []byte("second quarter"))
	summary := put(plain, contents.Writer.Actor, "reports/summary.txt", schema3Content("summary", 3_000))
	blob := put(plain, contents.Writer.Actor, "data/blob.bin", schema3Content("blob", 4_096))
	put(plain, schema3Root, "notes/unread.txt", []byte("nobody reads this"))
	// The copy: root reads the blob, as a copy - which is not a download -
	// and is who uploaded what it became.
	_, err = node.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(plain.Name), Key: aws.String("copies/blob.bin"), CopySource: aws.String(plain.Name + "/" + blob.Key)})
	must("CopyObject", err)
	blob.count(schema3Root, false, 0, 1)
	record(plain, schema3Root, "copies/blob.bin", "", schema3Content("blob", 4_096))
	// Six downloads that each have a row of their own, one after the other.
	download(plain, q1, schema3Root, 3)
	download(plain, q1, contents.Writer.Actor, 2)
	download(plain, summary, contents.Writer.Actor, 1)
	presigned, err := s3.NewPresignClient(node.root).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(plain.Name), Key: aws.String(q2.Key)})
	must("presign", err)
	headers := map[string]string{}
	for name, values := range presigned.SignedHeader {
		if http.CanonicalHeaderKey(name) != "Host" {
			headers[name] = values[0]
		}
	}
	fetch(plain, q2, presigned.URL, headers, schema3Root, true, 1)
	fetch(plain, summary, node.admin+"/buckets/"+plain.Name+"/object?key="+url.QueryEscape(summary.Key), schema3RootBearer(), schema3Root, false, 1)
	// The narrowed key reads what its inline policy names - which its owner
	// may read because of its group alone - and nothing else.
	download(plain, q1, contents.Scoped.Actor, 1)
	if _, err := scoped.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(contents.Scoped.Refused.Bucket), Key: aws.String(contents.Scoped.Refused.Key)}); apiCode(err) != "AccessDenied" {
		t.Fatalf("GetObject of %s with the narrowed key: %v; want AccessDenied", contents.Scoped.Refused.Key, err)
	}
	// The upload in progress, by the user: three parts, all small. The
	// binary takes a part of any size - it is at the completion that every
	// part but the last must be of object.MinPartSize - and this upload is
	// never completed here. It is not an upload yet.
	upload, err := user.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(plain.Name), Key: aws.String("film")})
	must("CreateMultipartUpload", err)
	film := schema3Upload{Key: "film", UploadID: aws.ToString(upload.UploadId)}
	number := int32(0)
	for _, reel := range [][]byte{schema3Content("reel one", 5_000), schema3Content("reel two", 3_000), schema3Content("reel three", 1_000)} {
		number++
		_, err = user.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(plain.Name), Key: aws.String(film.Key), UploadId: upload.UploadId, PartNumber: aws.Int32(number), Body: bytes.NewReader(reel)})
		must(fmt.Sprintf("UploadPart %d of %s", number, film.Key), err)
		film.Parts = append(film.Parts, schema3Part{Number: number, Size: int64(len(reel)), SHA256: sumOf(reel)})
	}
	plain.Uploads = append(plain.Uploads, film)

	// (b) The bucket created through the management API, by who signed in
	// to the console: the cookie, and the CSRF token for what changes
	// something. It has a policy, a lifecycle, CORS and tags.
	manage(schema4FixtureSignIn(ctx, t, node, schema4FixtureOperator, schema4FixturePassword()), "/buckets", map[string]any{"name": schema4FixtureViaAdmin})
	public := made(schema4FixtureViaAdmin, "user/"+schema4FixtureOperator, "admin")
	public.Policy = schema4FixturePublic
	_, err = node.root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(public.Name), Policy: aws.String(schema4FixturePublic)})
	must("PutBucketPolicy", err)
	// The lifecycle reaches nothing the fixture holds, and would not for a
	// hundred years: whoever opens the fixture finds every object.
	public.Lifecycle = []schema4FixtureLifecycleRule{{ID: "old-drafts", Prefix: "drafts/", ExpirationDays: 36_500}}
	_, err = node.root.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(public.Name),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
			ID: aws.String(public.Lifecycle[0].ID), Status: types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String(public.Lifecycle[0].Prefix)},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(public.Lifecycle[0].ExpirationDays)},
		}}},
	})
	must("PutBucketLifecycleConfiguration", err)
	public.CORS = []schema4FixtureCORSRule{{AllowedOrigins: []string{"https://app.example.test"}, AllowedMethods: []string{"GET", "HEAD"}, MaxAgeSeconds: 600}}
	_, err = node.root.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(public.Name), CORSConfiguration: &types.CORSConfiguration{
		CORSRules: []types.CORSRule{{AllowedOrigins: public.CORS[0].AllowedOrigins, AllowedMethods: public.CORS[0].AllowedMethods, MaxAgeSeconds: aws.Int32(public.CORS[0].MaxAgeSeconds)}},
	}})
	must("PutBucketCors", err)
	public.Tags = map[string]string{"Team": "Storage", "env": "Prod"}
	_, err = node.root.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(public.Name), Tagging: &types.Tagging{
		TagSet: []types.Tag{{Key: aws.String("Team"), Value: aws.String("Storage")}, {Key: aws.String("env"), Value: aws.String("Prod")}},
	}})
	must("PutBucketTagging", err)
	readme := put(public, contents.Writer.Actor, "public/readme.txt", []byte("for everybody"))
	put(public, schema3Root, "private/ledger.txt", []byte("for nobody else"))
	fetch(public, readme, node.s3+"/"+public.Name+"/"+readme.Key, nil, "anonymous", false, 2)

	// (c) The bucket that keeps versions, created through S3 by root.
	_, err = node.root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(schema4FixtureVersions)})
	must("CreateBucket "+schema4FixtureVersions, err)
	_, err = node.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(schema4FixtureVersions), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
	must("PutBucketVersioning", err)
	versions := made(schema4FixtureVersions, schema3Root, "s3")
	versions.Versions = true
	older := put(versions, schema3Root, "doc", []byte("first"))
	put(versions, contents.Writer.Actor, "doc", []byte("second"))
	gone := put(versions, contents.Writer.Actor, "gone", []byte("soon"))
	// The multipart upload that was completed, by the user: two parts, the
	// first of the least a part that is not the last may be. It is an
	// upload of the bucket, and a version like the others.
	const albumKey = "album"
	started, err := user.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(versions.Name), Key: aws.String(albumKey)})
	must("CreateMultipartUpload "+albumKey, err)
	var (
		whole      []byte
		albumParts []schema3Part
		listed     []types.CompletedPart
	)
	number = 0
	for _, side := range [][]byte{schema3Content("album one", object.MinPartSize), schema3Content("album two", 1_000)} {
		number++
		stored, err := user.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(versions.Name), Key: aws.String(albumKey), UploadId: started.UploadId, PartNumber: aws.Int32(number), Body: bytes.NewReader(side)})
		must(fmt.Sprintf("UploadPart %d of %s", number, albumKey), err)
		whole = append(whole, side...)
		albumParts = append(albumParts, schema3Part{Number: number, Size: int64(len(side)), SHA256: sumOf(side)})
		listed = append(listed, types.CompletedPart{PartNumber: aws.Int32(number), ETag: stored.ETag})
	}
	finished, err := user.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(versions.Name), Key: aws.String(albumKey), UploadId: started.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: listed},
	})
	must("CompleteMultipartUpload "+albumKey, err)
	album := record(versions, contents.Writer.Actor, albumKey, aws.ToString(finished.VersionId), whole)
	album.ETag, album.Parts = strings.Trim(aws.ToString(finished.ETag), `"`), albumParts
	if want := fmt.Sprintf("-%d", len(albumParts)); album.VersionID == "" || !strings.HasSuffix(album.ETag, want) {
		t.Fatalf("the completed multipart upload gave the version %q and the entity tag %q; want one that ends in %s", album.VersionID, album.ETag, want)
	}
	download(versions, older, schema3Root, 1)
	// A delete marker is not an upload.
	marker, err := node.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(versions.Name), Key: aws.String(gone.Key)})
	must("DeleteObject", err)
	gone.Latest = false
	versions.Markers = append(versions.Markers, schema3Marker{Key: gone.Key, VersionID: aws.ToString(marker.VersionId)})
	if older.VersionID == "" || aws.ToString(marker.VersionId) == "" {
		t.Fatalf("the versioned bucket gave the version %q and the marker %q", older.VersionID, aws.ToString(marker.VersionId))
	}

	// (d) The bucket that was there before under the same name. The first
	// one, root's, was uploaded to and downloaded from - the download handed
	// over, or it would be credited to the bucket that follows (ADR-0019) -
	// then emptied and deleted. The second, the user's, begins with nothing.
	_, err = node.root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(schema4FixtureReborn)})
	must("CreateBucket "+schema4FixtureReborn, err)
	const firstLife = "first/life.txt"
	_, err = node.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(schema4FixtureReborn), Key: aws.String(firstLife), Body: strings.NewReader("of the bucket that was")})
	must("PutObject "+firstLife, err)
	_, err = schema3Get(ctx, node.root, schema4FixtureReborn, &schema3Object{Key: firstLife})
	must("GetObject "+firstLife, err)
	for deadline := time.Now().Add(time.Minute); ; {
		uploads, downloads, shown := schema4FixtureGrid(ctx, t, node, schema3RootBearer())[schema4FixtureReborn].activity()
		if shown && len(uploads) == 1 && len(downloads) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the use of the first %s was not recorded: shown %t, %d uploads, %d downloads", schema4FixtureReborn, shown, len(uploads), len(downloads))
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	_, err = node.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(schema4FixtureReborn), Key: aws.String(firstLife)})
	must("DeleteObject "+firstLife, err)
	_, err = node.root.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(schema4FixtureReborn)})
	must("DeleteBucket "+schema4FixtureReborn, err)
	_, err = user.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(schema4FixtureReborn)})
	must("CreateBucket "+schema4FixtureReborn+" again", err)
	reborn := made(schema4FixtureReborn, contents.Writer.Actor, "s3")
	if differs := schema4FixtureShown(ctx, t, node, contents, false); len(differs) != 0 {
		t.Fatalf("a bucket created under the name of one that was deleted does not begin with nothing:\n%s", strings.Join(differs, "\n"))
	}
	put(reborn, contents.Writer.Actor, "second/life.txt", []byte("of the bucket that is"))

	// (e) The bucket nobody uses, created through the management API by
	// root.
	manage(schema3RootBearer(), "/buckets", map[string]any{"name": schema4FixtureUnused})
	made(schema4FixtureUnused, schema3Root, "admin")

	// (f) The bucket with Object Lock, which keeps versions because of it.
	// What is locked in it stays: the objects are a few bytes.
	_, err = node.root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(schema4FixtureLocked), ObjectLockEnabledForBucket: aws.Bool(true)})
	must("CreateBucket with Object Lock", err)
	locked := made(schema4FixtureLocked, schema3Root, "s3")
	locked.Versions, locked.ObjectLock, locked.Note = true, true, schema4FixtureKept
	ledger := put(locked, schema3Root, "ledger", []byte("kept"))
	ledger.Retention = &schema4FixtureRetention{Mode: string(types.ObjectLockRetentionModeGovernance), Until: time.Date(2099, time.December, 31, 0, 0, 0, 0, time.UTC)}
	_, err = node.root.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket: aws.String(locked.Name), Key: aws.String(ledger.Key), VersionId: aws.String(ledger.VersionID),
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(ledger.Retention.Until)},
	})
	must("PutObjectRetention", err)
	draft := put(locked, contents.Writer.Actor, "draft", []byte("held"))
	draft.LegalHold = true
	_, err = node.root.PutObjectLegalHold(ctx, &s3.PutObjectLegalHoldInput{
		Bucket: aws.String(locked.Name), Key: aws.String(draft.Key), VersionId: aws.String(draft.VersionID),
		LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOn},
	})
	must("PutObjectLegalHold", err)
	if ledger.VersionID == "" || draft.VersionID == "" {
		t.Fatalf("the bucket with Object Lock gave the versions %q and %q", ledger.VersionID, draft.VersionID)
	}

	// (g) The bucket of many keys, created through S3 by root. It keeps no
	// versions, and every key is one byte. All but the last three are put
	// by root, several at a time, and written down in the order of their
	// keys; the last three by the user, one after the other: they are the
	// record of the bucket's latest use, and their order is decided.
	_, err = node.root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(schema4FixtureMany)})
	must("CreateBucket "+schema4FixtureMany, err)
	many := made(schema4FixtureMany, schema3Root, "s3")
	manyKey := func(i int) string { return fmt.Sprintf("k/%03d", i) }
	const letters = "abcdefghijklmnopqrstuvwxyz"
	manyContent := func(i int) []byte { return []byte{letters[i%len(letters)]} }
	failed := make([]error, schema4FixtureManyKeys-bucket.MaxRecentUploads)
	var wait sync.WaitGroup
	atOnce := make(chan struct{}, schema4FixtureManyAtOnce)
	for i := range failed {
		wait.Add(1)
		atOnce <- struct{}{}
		go func() {
			defer wait.Done()
			defer func() { <-atOnce }()
			_, failed[i] = node.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(many.Name), Key: aws.String(manyKey(i)), Body: bytes.NewReader(manyContent(i))})
		}()
	}
	wait.Wait()
	for i, err := range failed {
		must("PutObject "+manyKey(i), err)
		record(many, schema3Root, manyKey(i), "", manyContent(i))
	}
	for i := len(failed); i < schema4FixtureManyKeys; i++ {
		put(many, contents.Writer.Actor, manyKey(i), manyContent(i))
	}

	// What every bucket holds, in figures: counted from what was written
	// down above. The node counts the objects and their bytes itself, and
	// must count the same.
	for _, b := range contents.Buckets {
		b.Usage = b.usage()
		var counted struct {
			ObjectCount int64 `json:"object_count"`
			TotalSize   int64 `json:"total_size"`
			Complete    bool  `json:"complete"`
		}
		status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/buckets/"+b.Name+"/usage", schema3RootBearer(), nil)
		if err := json.Unmarshal(body, &counted); err != nil || status != http.StatusOK || !counted.Complete || counted.ObjectCount != b.Usage.Objects || counted.TotalSize != b.Usage.CurrentBytes {
			t.Fatalf("the usage of %s = %d %s (%v); what was written is %d objects of %d bytes", b.Name, status, body, err, b.Usage.Objects, b.Usage.CurrentBytes)
		}
	}
	if many.Usage.Objects != schema4FixtureManyKeys || many.Usage.CurrentBytes != schema4FixtureManyKeys {
		t.Fatalf("%s holds %d objects of %d bytes; want %d of one byte each", many.Name, many.Usage.Objects, many.Usage.CurrentBytes, schema4FixtureManyKeys)
	}

	// Nothing is a fixture of reads that only a node's memory held: the
	// node is not stopped before the management API shows every read, who
	// created every bucket and the latest use of each as the manifest will
	// say them - and that it writes at level 4.
	schema4FixtureSettled(ctx, t, node, contents)
	if differs := schema4FixtureShown(ctx, t, node, contents, false); len(differs) != 0 {
		t.Fatalf("the buckets are not shown as they were used:\n%s", strings.Join(differs, "\n"))
	}
	shown := schema4FixtureGrid(ctx, t, node, schema3RootBearer())
	var settings struct {
		Cluster struct {
			MetadataLevel int `json:"metadata_level"`
		} `json:"cluster"`
	}
	status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/settings", schema3RootBearer(), nil)
	if err := json.Unmarshal(body, &settings); err != nil || status != http.StatusOK || settings.Cluster.MetadataLevel != 4 {
		t.Fatalf("the settings = %d (%v), metadata level %d: this tree does not write a fixture of schema 4", status, err, settings.Cluster.MetadataLevel)
	}
	contents.MetadataLevel = settings.Cluster.MetadataLevel
	stop()
	if out := node.srv.stderr.String(); strings.Contains(out, fixtureSecretKey) || strings.Contains(out, contents.Writer.SecretKey) ||
		strings.Contains(out, contents.Scoped.SecretKey) || strings.Contains(out, schema4FixturePassword()) {
		t.Fatal("a secret was written to standard error")
	}

	// What the state says of itself goes into the manifest: its schema, its
	// buckets, who created each bucket - and the records of the buckets'
	// latest use, byte for byte, which must say what the node showed while
	// it ran, to the nanosecond.
	meta := filepath.Join(dir, "meta")
	state := schema4StateOf(t, meta)
	if state.Schema != 4 {
		t.Fatalf("the state is of schema %d: this tree does not write a fixture of schema 4", state.Schema)
	}
	contents.StateSchema = state.Schema
	stored, records := schema4FixtureStored(t, meta)
	contents.StateBuckets = stored
	for _, b := range contents.Buckets {
		if by := state.Creators[b.Name]; by != b.CreatedBy {
			t.Fatalf("the state says %s was created by %q; want %q", b.Name, by, b.CreatedBy)
		}
		parsed := state.Activity[b.Name]
		stood := &schema4FixtureActivity{Uploads: parsed.Uploads, Downloads: parsed.Downloads}
		uploads, downloads := stood.entries()
		shownUploads, shownDownloads, _ := shown[b.Name].activity()
		if got, want := schema4FixtureRows(uploads, true)+"| "+schema4FixtureRows(downloads, true), schema4FixtureRows(shownUploads, true)+"| "+schema4FixtureRows(shownDownloads, true); got != want {
			t.Fatalf("the stored record of %s says %s; the node showed %s", b.Name, got, want)
		}
		raw, ok := records[b.Name]
		if ok != (len(b.Activity.Uploads)+len(b.Activity.Downloads) > 0) {
			t.Fatalf("the state has a record of %s: %t; it has %d uploads and %d downloads", b.Name, ok, len(b.Activity.Uploads), len(b.Activity.Downloads))
		}
		// The times as they were written: the rest was compared above,
		// with what the node showed and that with what was done.
		for i := range b.Activity.Uploads {
			b.Activity.Uploads[i].At = parsed.Uploads[i].At
		}
		for i := range b.Activity.Downloads {
			b.Activity.Downloads[i].At = parsed.Downloads[i].At
		}
		if ok {
			b.Activity.Record, b.Activity.RecordSHA256 = raw, sumOf([]byte(raw))
		}
	}
	if len(records) != len(contents.Buckets)-1 || len(state.Creators) != len(contents.Buckets) {
		t.Fatalf("the state holds %d buckets and %d records of their latest use; want %d, and a record of all but the one that was never used", len(state.Creators), len(records), len(contents.Buckets))
	}

	manifest, err := json.MarshalIndent(contents, "", "  ")
	must("the manifest", err)
	must("the fixture's directory", os.MkdirAll(fixture, 0o700))
	copyTree(t, dir, fixture)
	// The repository keeps no empty directory, so the fixture has none:
	// what is opened from a clone is then exactly what is here. The binary
	// makes the ones it wants (a drive's tmp, the snapshots) when it
	// starts.
	var dirs []string
	must("the fixture's directories", filepath.WalkDir(fixture, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			dirs = append(dirs, path)
		}
		return err
	}))
	for _, path := range slices.Backward(dirs) {
		entries, err := os.ReadDir(path)
		must("the fixture's directories", err)
		if len(entries) == 0 {
			must("an empty directory of the fixture", os.Remove(path))
		}
	}
	must("the manifest", os.WriteFile(filepath.Join(fixture, schema4FixtureManifest), append(manifest, '\n'), 0o600))
}
