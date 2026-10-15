package adminapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"mime"
	"net/http"
	"path"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/adminapi/api"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// operations implements the generated server interface: one method per
// operation of the contract. A method translates and calls a use case; an
// error it returns becomes a problem+json answer in Handler.fail.
type operations struct {
	h *Handler
}

var _ api.StrictServerInterface = (*operations)(nil)

func who(ctx context.Context) iam.Principal { return infoOf(ctx).principal }

// --- session ---

// server is the build of this node, as the contract says it. Only answers
// to authenticated callers carry it.
func (o *operations) server() api.ServerVersion {
	build := o.h.cfg.Build
	return api.ServerVersion{Version: build.Version, Build: build.Build, Commit: stringOrNil(build.Commit)}
}

// SignIn implements api.StrictServerInterface.
func (o *operations) SignIn(ctx context.Context, request api.SignInRequestObject) (api.SignInResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	signed, err := o.h.cfg.Sessions.SignIn(ctx, request.Body.Username, request.Body.Password, infoOf(ctx).session)
	if errors.Is(err, domain.ErrAccessDenied) {
		// Not who they say they are: 401, and no word about why.
		return nil, errors.Join(errUnauthenticated, err)
	}
	if err != nil {
		return nil, err
	}
	// The browser keeps the cookie for as long as the session can be
	// locked, which is longer than to its timeout: it is what unlocks.
	cookie := httpsession.Cookie(signed.Token, signed.CookieExpires).String()
	return api.SignIn200JSONResponse{
		Body:    api.Session{User: signed.User, CsrfToken: signed.CSRFToken, ExpiresAt: signed.ExpiresAt, Server: o.server()},
		Headers: api.SignIn200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

// GetSession implements api.StrictServerInterface.
func (o *operations) GetSession(ctx context.Context, _ api.GetSessionRequestObject) (api.GetSessionResponseObject, error) {
	info := infoOf(ctx)
	identity := api.Identity{Kind: api.IdentityKindUser, Via: api.IdentityViaApiKey, Server: o.server()}
	if info.principal.Root {
		identity.Kind = api.IdentityKindRoot
	} else {
		identity.User = &info.principal.User
	}
	if info.token != "" {
		identity.Via = api.IdentityViaSession
		// What the session is - locked or not - is the use case's to say.
		status, err := o.h.cfg.Sessions.Status(ctx, info.token)
		if errors.Is(err, domain.ErrAccessDenied) {
			// It ended between the authentication of this request and now.
			return nil, errors.Join(errUnauthenticated, err)
		}
		if err != nil {
			return nil, err
		}
		identity.Locked, identity.ExpiresAt, identity.LockEndsAt = &status.Locked, &status.ExpiresAt, timeOrNil(status.LockEndsAt)
	} else {
		identity.AccessKeyId = &info.principal.AccessKeyID
	}
	return api.GetSession200JSONResponse(identity), nil
}

// SignOut implements api.StrictServerInterface.
func (o *operations) SignOut(ctx context.Context, _ api.SignOutRequestObject) (api.SignOutResponseObject, error) {
	info := infoOf(ctx)
	if info.token != "" {
		if err := o.h.cfg.Sessions.SignOut(ctx, info.token, info.session); err != nil {
			return nil, err
		}
	}
	cookie := httpsession.Expired().String()
	return api.SignOut204Response{Headers: api.SignOut204ResponseHeaders{SetCookie: &cookie}}, nil
}

// UnlockSession implements api.StrictServerInterface. Whether the password
// unlocks the session, and what follows from one that does not, is the use
// case's; this carries the tokens.
func (o *operations) UnlockSession(ctx context.Context, request api.UnlockSessionRequestObject) (api.UnlockSessionResponseObject, error) {
	info := infoOf(ctx)
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	if info.token == "" {
		return nil, fmt.Errorf("%w: the request was made with an API key, which has no session to unlock", domain.ErrInvalidArgument)
	}
	unlocked, err := o.h.cfg.Sessions.Unlock(ctx, info.token, request.Body.Password, info.session)
	if errors.Is(err, domain.ErrCredentialNotFound) && errors.Is(err, domain.ErrTooManyAttempts) {
		// This request ended the session, for too many wrong passwords:
		// 401, and the browser forgets its cookie. A session that had
		// ended before is a 401 too, and the cookie is left alone: it may
		// be this session, unlocked a moment ago by the same request sent
		// twice, and the cookie the browser has by now is the new one.
		info.sessionEnded = true
	}
	if err != nil {
		return nil, err
	}
	cookie := httpsession.Cookie(unlocked.Token, unlocked.CookieExpires).String()
	return api.UnlockSession200JSONResponse{
		Body:    api.Session{User: unlocked.User, CsrfToken: unlocked.CSRFToken, ExpiresAt: unlocked.ExpiresAt, Server: o.server()},
		Headers: api.UnlockSession200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

// --- buckets ---

// ListBuckets implements api.StrictServerInterface.
func (o *operations) ListBuckets(ctx context.Context, request api.ListBucketsRequestObject) (api.ListBucketsResponseObject, error) {
	if d := request.Params.Details; d == nil || !*d {
		buckets, err := o.h.cfg.Buckets.List(ctx, who(ctx))
		if err != nil {
			return nil, err
		}
		out := make(api.ListBuckets200JSONResponse, len(buckets))
		for i, b := range buckets {
			out[i] = api.Bucket{Name: b.Name, CreatedAt: b.CreatedAt}
		}
		return out, nil
	}
	// What of each bucket the caller may be shown is the use case's to
	// say: this maps what it answers.
	detailed, err := o.h.cfg.Buckets.ListDetailed(ctx, who(ctx))
	if err != nil {
		return nil, err
	}
	out := make(api.ListBuckets200JSONResponse, len(detailed))
	for i, d := range detailed {
		out[i] = api.Bucket{Name: d.Bucket.Name, CreatedAt: d.Bucket.CreatedAt, Details: bucketDetailsOf(d)}
	}
	return out, nil
}

// bucketDetailsOf maps what a list says of a bucket beyond its name to the
// contract.
func bucketDetailsOf(d app.BucketDetail) *api.BucketDetails {
	out := api.BucketDetails{AccessIncluded: d.AccessIncluded, PolicyIncluded: d.PolicyIncluded}
	if d.AccessIncluded {
		if d.Bucket.CreatedBy != "" {
			out.CreatedBy = &d.Bucket.CreatedBy
		}
		activity := api.BucketActivity{
			Uploads:   make([]api.BucketActivityEntry, len(d.Activity.Uploads)),
			Downloads: make([]api.BucketActivityEntry, len(d.Activity.Downloads)),
		}
		for i, u := range d.Activity.Uploads {
			activity.Uploads[i] = activityEntryOf(u.At, u.By, u.Key, false)
		}
		for i, r := range d.Activity.Downloads {
			activity.Downloads[i] = activityEntryOf(r.At, r.By, r.Key, r.Presigned)
		}
		out.Activity = &activity
	}
	if d.PolicyIncluded && d.Policy != nil {
		out.Policy = &api.BucketPolicySummary{Statements: d.Policy.Statements, OpenToAnyone: d.Policy.OpenToAnyone}
	}
	return &out
}

// activityEntryOf maps one upload or download to the contract. A key the
// use case withheld is empty, and is left out.
func activityEntryOf(at time.Time, by, key string, presigned bool) api.BucketActivityEntry {
	entry := api.BucketActivityEntry{At: at, By: by}
	if key != "" {
		entry.Key = &key
	}
	if presigned {
		entry.Presigned = &presigned
	}
	return entry
}

// CreateBucket implements api.StrictServerInterface.
func (o *operations) CreateBucket(ctx context.Context, request api.CreateBucketRequestObject) (api.CreateBucketResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	if err := o.h.cfg.Buckets.Create(ctx, who(ctx), request.Body.Name); err != nil {
		return nil, err
	}
	b, err := o.h.cfg.Buckets.Get(ctx, who(ctx), request.Body.Name)
	if err != nil {
		return nil, err
	}
	return api.CreateBucket201JSONResponse{Name: b.Name, CreatedAt: b.CreatedAt}, nil
}

// GetBucket implements api.StrictServerInterface.
func (o *operations) GetBucket(ctx context.Context, request api.GetBucketRequestObject) (api.GetBucketResponseObject, error) {
	b, err := o.h.cfg.Buckets.Get(ctx, who(ctx), request.Bucket)
	if err != nil {
		return nil, err
	}
	return api.GetBucket200JSONResponse{Name: b.Name, CreatedAt: b.CreatedAt}, nil
}

// DeleteBucket implements api.StrictServerInterface.
func (o *operations) DeleteBucket(ctx context.Context, request api.DeleteBucketRequestObject) (api.DeleteBucketResponseObject, error) {
	if err := o.h.cfg.Buckets.Delete(ctx, who(ctx), request.Bucket); err != nil {
		return nil, err
	}
	return api.DeleteBucket204Response{}, nil
}

// --- objects ---

const defaultListLimit = 200

// GetBucketUsage implements api.StrictServerInterface.
func (o *operations) GetBucketUsage(ctx context.Context, request api.GetBucketUsageRequestObject) (api.GetBucketUsageResponseObject, error) {
	usage, err := o.h.cfg.Objects.Usage(ctx, who(ctx), request.Bucket)
	if err != nil {
		return nil, err
	}
	return api.GetBucketUsage200JSONResponse{ObjectCount: usage.Objects, TotalSize: usage.Bytes, Complete: usage.Complete}, nil
}

// ListObjects implements api.StrictServerInterface.
func (o *operations) ListObjects(ctx context.Context, request api.ListObjectsRequestObject) (api.ListObjectsResponseObject, error) {
	opts := object.ListOptions{MaxKeys: defaultListLimit}
	if p := request.Params.Prefix; p != nil {
		opts.Prefix = *p
	}
	if d := request.Params.Delimiter; d != nil {
		opts.Delimiter = *d
	}
	if c := request.Params.ContinuationToken; c != nil {
		opts.Continuation = *c
	}
	if l := request.Params.Limit; l != nil {
		if *l < 1 || *l > object.MaxListKeys {
			return nil, fmt.Errorf("%w: limit must be 1 to %d", domain.ErrInvalidArgument, object.MaxListKeys)
		}
		opts.MaxKeys = *l
	}
	// Whether the caller may be shown what was asked for with "access" is
	// the use case's to say: this maps what it answers.
	listing, err := o.h.cfg.Objects.ListWithAccess(ctx, who(ctx), request.Bucket, opts, request.Params.Access != nil && *request.Params.Access)
	if err != nil {
		return nil, err
	}
	page := listing.Page
	out := api.ObjectList{
		Objects: make([]api.ObjectSummary, len(page.Objects)), CommonPrefixes: page.CommonPrefixes, Truncated: page.Truncated,
		AccessIncluded: listing.Included,
	}
	if out.CommonPrefixes == nil {
		out.CommonPrefixes = []string{}
	}
	for i, v := range page.Objects {
		out.Objects[i] = api.ObjectSummary{Key: v.Key, Size: v.Size, Etag: v.ETag, LastModified: v.ModifiedAt}
		if i < len(listing.Access) && listing.Access[i] != nil {
			out.Objects[i].Access = accessSummaryOf(*listing.Access[i])
		}
	}
	if page.Truncated {
		out.NextContinuationToken = &page.NextContinuation
	}
	return api.ListObjects200JSONResponse(out), nil
}

func metadataOf(v object.Version) api.ObjectMetadata {
	return api.ObjectMetadata{
		Bucket: v.Bucket, Key: v.Key, Size: v.Size, Etag: v.ETag, LastModified: v.ModifiedAt,
		ContentType: v.ContentType, Encryption: v.Encryption.Scheme,
	}
}

// GetObjectMetadata implements api.StrictServerInterface.
func (o *operations) GetObjectMetadata(ctx context.Context, request api.GetObjectMetadataRequestObject) (api.GetObjectMetadataResponseObject, error) {
	// As a read would see it: the metadata of an object under a customer
	// key is for the holder of that key, which this API never is.
	v, err := o.h.cfg.Objects.Stat(ctx, who(ctx), app.GetInput{Bucket: request.Bucket, Key: request.Params.Key})
	if err != nil {
		return nil, err
	}
	return api.GetObjectMetadata200JSONResponse(metadataOf(v)), nil
}

// accessSummaryOf maps what a listing says of the access to an object to the
// contract.
func accessSummaryOf(a app.AccessSummary) *api.ObjectAccessSummary {
	out := api.ObjectAccessSummary{UploadedAt: a.UploadedAt, DownloadCount: a.Downloads, CopyCount: a.Copies}
	if a.UploadedBy != "" {
		out.UploadedBy = &a.UploadedBy
	}
	if !a.LastDownloadedAt.IsZero() {
		out.LastDownloadedAt = &a.LastDownloadedAt
	}
	return &out
}

// accessOf maps what is known of the reading of a version to the contract.
func accessOf(a app.ObjectAccess) api.ObjectAccess {
	out := api.ObjectAccess{
		UploadedAt: a.Version.ModifiedAt, DownloadCount: a.Access.Downloads, CopyCount: a.Access.Copies,
		Readers:      make([]api.ObjectReader, len(a.Access.Readers)),
		OtherReaders: a.Access.OtherReaders, OtherDownloads: a.Access.OtherDownloads, OtherCopies: a.Access.OtherCopies,
	}
	if a.Version.CreatedBy != "" {
		out.UploadedBy = &a.Version.CreatedBy
	}
	if !a.Access.LastAt.IsZero() {
		out.LastDownloadedAt = &a.Access.LastAt
	}
	for i, r := range a.Access.Readers {
		out.Readers[i] = api.ObjectReader{Who: r.Who, Presigned: r.Presigned, Downloads: r.Downloads, Copies: r.Copies, LastAt: r.LastAt}
	}
	return out
}

// GetObjectAccess implements api.StrictServerInterface.
func (o *operations) GetObjectAccess(ctx context.Context, request api.GetObjectAccessRequestObject) (api.GetObjectAccessResponseObject, error) {
	// Which version is named is read by the use case, after it has asked
	// whether the caller may know anything of the object at all.
	version := ""
	if id := request.Params.VersionId; id != nil {
		version = *id
	}
	access, err := o.h.cfg.Objects.Access(ctx, who(ctx), request.Bucket, request.Params.Key, version)
	if err != nil {
		return nil, err
	}
	return api.GetObjectAccess200JSONResponse(accessOf(access)), nil
}

// GetObject implements api.StrictServerInterface. The content is always
// sent as an attachment of an opaque type: whatever was uploaded, a browser
// saves it and never renders it on the console's origin.
func (o *operations) GetObject(ctx context.Context, request api.GetObjectRequestObject) (api.GetObjectResponseObject, error) {
	out, err := o.h.cfg.Objects.Get(ctx, who(ctx), app.GetInput{Bucket: request.Bucket, Key: request.Params.Key})
	if err != nil {
		return nil, err
	}
	v := out.Version
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(v.Key)})
	if disposition == "" {
		disposition = "attachment"
	}
	modified := v.ModifiedAt.UTC().Format(http.TimeFormat)
	return api.GetObject200ApplicationoctetStreamResponse{
		Body: out.Body, ContentLength: v.Size,
		Headers: api.GetObject200ResponseHeaders{
			ContentDisposition: &disposition, ContentLength: &v.Size, ETag: &v.ETag, LastModified: &modified,
		},
	}, nil
}

// contentTag is the entity tag of an object stored through this API: a
// SHA-256 of the content, cut to the length S3 clients expect and marked,
// like the tag of a multipart upload, as not being an MD5.
func contentTag(sum hash.Hash) string {
	return `"` + hex.EncodeToString(sum.Sum(nil))[:32] + `-1"`
}

// PutObject implements api.StrictServerInterface.
func (o *operations) PutObject(ctx context.Context, request api.PutObjectRequestObject) (api.PutObjectResponseObject, error) {
	size := infoOf(ctx).contentLength
	if size < 0 {
		return nil, fmt.Errorf("%w: Content-Length is required", domain.ErrInvalidArgument)
	}
	contentType := "application/octet-stream"
	if t := request.Params.ContentType; t != nil && *t != "" {
		contentType = *t
	}
	sum := sha256.New()
	v, err := o.h.cfg.Objects.Put(ctx, who(ctx), app.PutInput{
		Bucket: request.Bucket, Key: request.Params.Key, Body: io.TeeReader(request.Body, sum), Size: size,
		ContentType: contentType,
		Verify:      func() (string, error) { return contentTag(sum), nil },
	})
	if err != nil {
		return nil, err
	}
	return api.PutObject200JSONResponse(metadataOf(v)), nil
}

// DeleteObject implements api.StrictServerInterface.
func (o *operations) DeleteObject(ctx context.Context, request api.DeleteObjectRequestObject) (api.DeleteObjectResponseObject, error) {
	if _, err := o.h.cfg.Objects.Delete(ctx, who(ctx), app.DeleteInput{Bucket: request.Bucket, Key: request.Params.Key}); err != nil {
		return nil, err
	}
	return api.DeleteObject204Response{}, nil
}

// --- search ---

// Search implements api.StrictServerInterface.
func (o *operations) Search(ctx context.Context, request api.SearchRequestObject) (api.SearchResponseObject, error) {
	in := app.SearchInput{
		Query: request.Params.Q, Bucket: optional(request.Params.Bucket), Continuation: optional(request.Params.Continuation),
	}
	if l := request.Params.Limit; l != nil {
		// Zero is the use case's word for "not said"; here it was said.
		if *l < 1 || *l > app.MaxSearchLimit {
			return nil, fmt.Errorf("%w: limit must be 1 to %d", domain.ErrInvalidArgument, app.MaxSearchLimit)
		}
		in.Limit = *l
	}
	found, err := o.h.cfg.Search.Search(ctx, who(ctx), in)
	if err != nil {
		return nil, err
	}
	out := api.SearchResults{
		Results: make([]api.SearchResult, len(found.Results)), Match: api.SearchResultsMatch(found.MatchKind),
		Examined: found.Examined, Complete: found.Complete, NextContinuation: stringOrNil(found.NextContinuation),
	}
	for i, r := range found.Results {
		if r.Kind == app.SearchResultBucket {
			out.Results[i] = api.SearchResult{Kind: api.SearchResultKindBucket, Bucket: r.Bucket}
			continue
		}
		out.Results[i] = api.SearchResult{
			Kind: api.SearchResultKindObject, Bucket: r.Bucket, Key: &r.Key, Size: &r.Size,
			LastModified: timeOrNil(r.ModifiedAt), Etag: stringOrNil(r.ETag),
		}
	}
	return api.Search200JSONResponse(out), nil
}

// --- users ---

func names(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func optional[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func userOf(u iam.User) api.User {
	return api.User{
		Name: u.Name, Groups: names(u.Groups), Roles: names(u.Roles), Disabled: u.Disabled,
		HasPassword: u.PasswordHash != "", CreatedAt: u.CreatedAt,
	}
}

// ListUsers implements api.StrictServerInterface.
func (o *operations) ListUsers(ctx context.Context, _ api.ListUsersRequestObject) (api.ListUsersResponseObject, error) {
	users, err := o.h.cfg.IAM.ListUsers(ctx, who(ctx))
	if err != nil {
		return nil, err
	}
	out := make(api.ListUsers200JSONResponse, len(users))
	for i, u := range users {
		out[i] = userOf(u)
	}
	return out, nil
}

// CreateUser implements api.StrictServerInterface.
func (o *operations) CreateUser(ctx context.Context, request api.CreateUserRequestObject) (api.CreateUserResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	b := request.Body
	u, err := o.h.cfg.IAM.CreateUser(ctx, who(ctx), app.UserInput{
		Name: b.Name, Groups: optional(b.Groups), Roles: optional(b.Roles), Disabled: optional(b.Disabled), Password: optional(b.Password),
	})
	if err != nil {
		return nil, err
	}
	return api.CreateUser201JSONResponse(userOf(u)), nil
}

// GetUser implements api.StrictServerInterface.
func (o *operations) GetUser(ctx context.Context, request api.GetUserRequestObject) (api.GetUserResponseObject, error) {
	u, err := o.h.cfg.IAM.GetUser(ctx, who(ctx), request.Name)
	if err != nil {
		return nil, err
	}
	return api.GetUser200JSONResponse(userOf(u)), nil
}

// UpdateUser implements api.StrictServerInterface.
func (o *operations) UpdateUser(ctx context.Context, request api.UpdateUserRequestObject) (api.UpdateUserResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	b := request.Body
	u, err := o.h.cfg.IAM.UpdateUser(ctx, who(ctx), app.UserInput{
		Name: request.Name, Groups: b.Groups, Roles: b.Roles, Disabled: b.Disabled, Password: optional(b.Password),
	})
	if err != nil {
		return nil, err
	}
	return api.UpdateUser200JSONResponse(userOf(u)), nil
}

// DeleteUser implements api.StrictServerInterface.
func (o *operations) DeleteUser(ctx context.Context, request api.DeleteUserRequestObject) (api.DeleteUserResponseObject, error) {
	if err := o.h.cfg.IAM.DeleteUser(ctx, who(ctx), request.Name); err != nil {
		return nil, err
	}
	return api.DeleteUser204Response{}, nil
}

// --- API keys ---

// documentOf turns a policy into the JSON object the contract carries.
func documentOf(d iam.Document) (api.PolicyDocument, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var out api.PolicyDocument
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func keyOf(k iam.APIKey) (api.APIKey, error) {
	out := api.APIKey{
		AccessKeyId: k.AccessKeyID, Owner: k.Owner, Description: k.Description, CreatedAt: k.CreatedAt,
		ExpiresAt: timeOrNil(k.ExpiresAt), LastUsedAt: timeOrNil(k.LastUsedAt),
	}
	if k.InlinePolicy != nil {
		doc, err := documentOf(*k.InlinePolicy)
		if err != nil {
			return api.APIKey{}, err
		}
		out.InlinePolicy = &doc
	}
	return out, nil
}

// ListAPIKeys implements api.StrictServerInterface.
func (o *operations) ListAPIKeys(ctx context.Context, request api.ListAPIKeysRequestObject) (api.ListAPIKeysResponseObject, error) {
	keys, err := o.h.cfg.IAM.ListAPIKeys(ctx, who(ctx), request.Name)
	if err != nil {
		return nil, err
	}
	out := make(api.ListAPIKeys200JSONResponse, len(keys))
	for i, k := range keys {
		if out[i], err = keyOf(k); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CreateAPIKey implements api.StrictServerInterface.
func (o *operations) CreateAPIKey(ctx context.Context, request api.CreateAPIKeyRequestObject) (api.CreateAPIKeyResponseObject, error) {
	in := app.APIKeyInput{Owner: request.Name}
	if b := request.Body; b != nil {
		in.Description, in.ExpiresAt = optional(b.Description), optional(b.ExpiresAt)
		if b.InlinePolicy != nil {
			raw, err := json.Marshal(*b.InlinePolicy)
			if err != nil {
				return nil, fmt.Errorf("%w: inline policy: %w", domain.ErrInvalidArgument, err)
			}
			in.InlinePolicy = raw
		}
	}
	creds, key, err := o.h.cfg.IAM.CreateAPIKey(ctx, who(ctx), in)
	if err != nil {
		return nil, err
	}
	out, err := keyOf(key)
	if err != nil {
		return nil, err
	}
	return api.CreateAPIKey201JSONResponse{Key: out, SecretKey: creds.SecretKey}, nil
}

// DeleteAPIKey implements api.StrictServerInterface.
func (o *operations) DeleteAPIKey(ctx context.Context, request api.DeleteAPIKeyRequestObject) (api.DeleteAPIKeyResponseObject, error) {
	if err := o.h.cfg.IAM.DeleteAPIKey(ctx, who(ctx), request.AccessKeyId); err != nil {
		return nil, err
	}
	return api.DeleteAPIKey204Response{}, nil
}

// --- groups ---

func groupOf(g iam.Group) api.Group {
	return api.Group{Name: g.Name, Roles: names(g.Roles), CreatedAt: g.CreatedAt}
}

// ListGroups implements api.StrictServerInterface.
func (o *operations) ListGroups(ctx context.Context, _ api.ListGroupsRequestObject) (api.ListGroupsResponseObject, error) {
	groups, err := o.h.cfg.IAM.ListGroups(ctx, who(ctx))
	if err != nil {
		return nil, err
	}
	out := make(api.ListGroups200JSONResponse, len(groups))
	for i, g := range groups {
		out[i] = groupOf(g)
	}
	return out, nil
}

func (o *operations) putGroup(ctx context.Context, name string, roles []string, create bool) (api.Group, error) {
	if err := o.h.cfg.IAM.PutGroup(ctx, who(ctx), iam.Group{Name: name, Roles: roles}, create); err != nil {
		return api.Group{}, err
	}
	g, err := o.h.cfg.IAM.GetGroup(ctx, who(ctx), name)
	if err != nil {
		return api.Group{}, err
	}
	return groupOf(g), nil
}

// CreateGroup implements api.StrictServerInterface.
func (o *operations) CreateGroup(ctx context.Context, request api.CreateGroupRequestObject) (api.CreateGroupResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	g, err := o.putGroup(ctx, request.Body.Name, request.Body.Roles, true)
	if err != nil {
		return nil, err
	}
	return api.CreateGroup201JSONResponse(g), nil
}

// GetGroup implements api.StrictServerInterface.
func (o *operations) GetGroup(ctx context.Context, request api.GetGroupRequestObject) (api.GetGroupResponseObject, error) {
	g, err := o.h.cfg.IAM.GetGroup(ctx, who(ctx), request.Name)
	if err != nil {
		return nil, err
	}
	return api.GetGroup200JSONResponse(groupOf(g)), nil
}

// UpdateGroup implements api.StrictServerInterface.
func (o *operations) UpdateGroup(ctx context.Context, request api.UpdateGroupRequestObject) (api.UpdateGroupResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	g, err := o.putGroup(ctx, request.Name, request.Body.Roles, false)
	if err != nil {
		return nil, err
	}
	return api.UpdateGroup200JSONResponse(g), nil
}

// DeleteGroup implements api.StrictServerInterface.
func (o *operations) DeleteGroup(ctx context.Context, request api.DeleteGroupRequestObject) (api.DeleteGroupResponseObject, error) {
	if err := o.h.cfg.IAM.DeleteGroup(ctx, who(ctx), request.Name); err != nil {
		return nil, err
	}
	return api.DeleteGroup204Response{}, nil
}

// --- roles ---

func roleOf(r iam.Role) api.Role {
	return api.Role{Name: r.Name, Policies: names(r.Policies), BuiltIn: r.BuiltIn, CreatedAt: r.CreatedAt}
}

// ListRoles implements api.StrictServerInterface.
func (o *operations) ListRoles(ctx context.Context, _ api.ListRolesRequestObject) (api.ListRolesResponseObject, error) {
	roles, err := o.h.cfg.IAM.ListRoles(ctx, who(ctx))
	if err != nil {
		return nil, err
	}
	out := make(api.ListRoles200JSONResponse, len(roles))
	for i, r := range roles {
		out[i] = roleOf(r)
	}
	return out, nil
}

func (o *operations) putRole(ctx context.Context, name string, policies []string, create bool) (api.Role, error) {
	if err := o.h.cfg.IAM.PutRole(ctx, who(ctx), iam.Role{Name: name, Policies: policies}, create); err != nil {
		return api.Role{}, err
	}
	r, err := o.h.cfg.IAM.GetRole(ctx, who(ctx), name)
	if err != nil {
		return api.Role{}, err
	}
	return roleOf(r), nil
}

// CreateRole implements api.StrictServerInterface.
func (o *operations) CreateRole(ctx context.Context, request api.CreateRoleRequestObject) (api.CreateRoleResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	r, err := o.putRole(ctx, request.Body.Name, request.Body.Policies, true)
	if err != nil {
		return nil, err
	}
	return api.CreateRole201JSONResponse(r), nil
}

// GetRole implements api.StrictServerInterface.
func (o *operations) GetRole(ctx context.Context, request api.GetRoleRequestObject) (api.GetRoleResponseObject, error) {
	r, err := o.h.cfg.IAM.GetRole(ctx, who(ctx), request.Name)
	if err != nil {
		return nil, err
	}
	return api.GetRole200JSONResponse(roleOf(r)), nil
}

// UpdateRole implements api.StrictServerInterface.
func (o *operations) UpdateRole(ctx context.Context, request api.UpdateRoleRequestObject) (api.UpdateRoleResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	r, err := o.putRole(ctx, request.Name, request.Body.Policies, false)
	if err != nil {
		return nil, err
	}
	return api.UpdateRole200JSONResponse(r), nil
}

// DeleteRole implements api.StrictServerInterface.
func (o *operations) DeleteRole(ctx context.Context, request api.DeleteRoleRequestObject) (api.DeleteRoleResponseObject, error) {
	if err := o.h.cfg.IAM.DeleteRole(ctx, who(ctx), request.Name); err != nil {
		return nil, err
	}
	return api.DeleteRole204Response{}, nil
}

// --- policies ---

func policyOf(p iam.Policy) (api.Policy, error) {
	doc, err := documentOf(p.Document)
	if err != nil {
		return api.Policy{}, err
	}
	return api.Policy{Name: p.Name, Document: doc, BuiltIn: p.BuiltIn, CreatedAt: p.CreatedAt}, nil
}

// ListPolicies implements api.StrictServerInterface.
func (o *operations) ListPolicies(ctx context.Context, _ api.ListPoliciesRequestObject) (api.ListPoliciesResponseObject, error) {
	policies, err := o.h.cfg.IAM.ListPolicies(ctx, who(ctx))
	if err != nil {
		return nil, err
	}
	out := make(api.ListPolicies200JSONResponse, len(policies))
	for i, p := range policies {
		if out[i], err = policyOf(p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (o *operations) putPolicy(ctx context.Context, name string, document api.PolicyDocument, create bool) (api.Policy, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return api.Policy{}, fmt.Errorf("%w: policy document: %w", domain.ErrInvalidArgument, err)
	}
	if err := o.h.cfg.IAM.PutPolicy(ctx, who(ctx), name, raw, create); err != nil {
		return api.Policy{}, err
	}
	p, err := o.h.cfg.IAM.GetPolicy(ctx, who(ctx), name)
	if err != nil {
		return api.Policy{}, err
	}
	return policyOf(p)
}

// CreatePolicy implements api.StrictServerInterface.
func (o *operations) CreatePolicy(ctx context.Context, request api.CreatePolicyRequestObject) (api.CreatePolicyResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	p, err := o.putPolicy(ctx, request.Body.Name, request.Body.Document, true)
	if err != nil {
		return nil, err
	}
	return api.CreatePolicy201JSONResponse(p), nil
}

// GetPolicy implements api.StrictServerInterface.
func (o *operations) GetPolicy(ctx context.Context, request api.GetPolicyRequestObject) (api.GetPolicyResponseObject, error) {
	p, err := o.h.cfg.IAM.GetPolicy(ctx, who(ctx), request.Name)
	if err != nil {
		return nil, err
	}
	out, err := policyOf(p)
	if err != nil {
		return nil, err
	}
	return api.GetPolicy200JSONResponse(out), nil
}

// UpdatePolicy implements api.StrictServerInterface.
func (o *operations) UpdatePolicy(ctx context.Context, request api.UpdatePolicyRequestObject) (api.UpdatePolicyResponseObject, error) {
	if request.Body == nil {
		return nil, fmt.Errorf("%w: no body", domain.ErrInvalidArgument)
	}
	p, err := o.putPolicy(ctx, request.Name, request.Body.Document, false)
	if err != nil {
		return nil, err
	}
	return api.UpdatePolicy200JSONResponse(p), nil
}

// DeletePolicy implements api.StrictServerInterface.
func (o *operations) DeletePolicy(ctx context.Context, request api.DeletePolicyRequestObject) (api.DeletePolicyResponseObject, error) {
	if err := o.h.cfg.IAM.DeletePolicy(ctx, who(ctx), request.Name); err != nil {
		return nil, err
	}
	return api.DeletePolicy204Response{}, nil
}

// --- cluster ---

func stringOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetCluster implements api.StrictServerInterface.
func (o *operations) GetCluster(ctx context.Context, _ api.GetClusterRequestObject) (api.GetClusterResponseObject, error) {
	status, err := o.h.cfg.Cluster.Status(ctx, who(ctx))
	if err != nil {
		return nil, err
	}
	out := api.ClusterStatus{
		DeploymentId: status.DeploymentID, AnsweredBy: status.AnsweredBy, Server: o.server(), Leader: status.Leader,
		Nodes: make([]api.Node, len(status.Nodes)), ErasureSets: make([]api.ErasureSet, len(status.Sets)),
		NodeFailuresTolerated: status.NodeFailuresTolerated, DrivesOnline: status.DrivesOnline, DrivesTotal: status.DrivesTotal,
	}
	for i, n := range status.Nodes {
		roles := make([]string, len(n.Roles))
		for j, role := range n.Roles {
			roles[j] = string(role)
		}
		version := n.MetadataVersion
		out.Nodes[i] = api.Node{
			Id: n.ID, Roles: roles, Leader: n.ID == status.Leader, MetadataVersion: &version,
			Labels: api.NodeLabels{
				Region: stringOrNil(n.Labels.Region), Zone: stringOrNil(n.Labels.Zone),
				Rack: stringOrNil(n.Labels.Rack), Host: stringOrNil(n.Labels.Host),
			},
		}
	}
	for i, set := range status.Sets {
		drives := make([]api.Drive, len(set))
		for j, d := range set {
			state := api.Offline
			if d.Online {
				state = api.Online
			}
			drives[j] = api.Drive{Id: d.ID, NodeId: d.NodeID, State: state}
		}
		out.ErasureSets[i] = api.ErasureSet{Index: i, Drives: drives}
	}
	return api.GetCluster200JSONResponse(out), nil
}

// GetMaintenance implements api.StrictServerInterface.
func (o *operations) GetMaintenance(ctx context.Context, _ api.GetMaintenanceRequestObject) (api.GetMaintenanceResponseObject, error) {
	m, err := o.h.cfg.Cluster.Maintenance(ctx, who(ctx))
	if err != nil {
		return nil, err
	}
	return api.GetMaintenance200JSONResponse{
		NodeId: m.NodeID, StartedAt: m.StartedAt,
		Scrub: api.ScrubStatus{
			Passes: m.ScrubPasses, LastPassAt: timeOrNil(m.LastScrubAt), Objects: int64(m.LastScrub.Objects),
			Damaged: int64(m.LastScrub.Damaged), ShardsRebuilt: int64(m.LastScrub.ShardsRebuilt),
			Unrecoverable: int64(m.LastScrub.Unrecoverable), Bytes: m.LastScrub.Bytes,
		},
		Heal: api.HealStatus{Objects: m.HealedObjects, ShardsRebuilt: m.HealedShards, Unrecoverable: m.UnrecoverableObject},
		Collection: api.CollectionStatus{
			Passes: m.GCPasses, LastPassAt: timeOrNil(m.LastGCAt), Shards: int64(m.LastGC.Shards), Removed: int64(m.LastGC.Removed),
		},
	}, nil
}

// --- settings ---

// bytesOf carries a number of bytes into the contract's signed integer.
func bytesOf(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}

func filesystemOf(fs cluster.Filesystem) *api.SettingsFilesystem {
	if fs == (cluster.Filesystem{}) {
		return nil
	}
	return &api.SettingsFilesystem{Type: fs.Type, Device: fs.Device, MountPoint: fs.MountPoint}
}

// stringsOf returns list as an array of the contract, which is never null.
func stringsOf(list []string) []string {
	return append([]string{}, list...)
}

// nodeSettingsOf translates the answer of one node. A node that did not
// answer has its reason and nothing else; of what went wrong the use case
// has told this adapter nothing.
func nodeSettingsOf(answer cluster.NodeAnswer) api.NodeSettings {
	if !answer.Answered {
		reason := api.Failed
		switch answer.Reason {
		case cluster.ReasonUnreachable:
			reason = api.Unreachable
		case cluster.ReasonTimeout:
			reason = api.Timeout
		case cluster.ReasonUnsupported:
			reason = api.Unsupported
		case cluster.ReasonFailed, cluster.ReasonNone:
		}
		return api.NodeSettings{NodeId: answer.NodeID, Reason: &reason}
	}
	n := answer.Settings
	roles, labels := stringsOf(n.Roles), map[string]string{}
	for name, value := range n.Labels {
		labels[name] = value
	}
	settings := make([]api.Setting, len(n.Settings))
	for i, setting := range n.Settings {
		settings[i] = api.Setting{
			Name: setting.Name, Env: stringOrNil(setting.Env), Flag: stringOrNil(setting.Flag), Group: stringOrNil(setting.Group),
			Kind: string(setting.Kind), Scope: string(setting.Scope), Source: string(setting.Source),
			Set: setting.Set, Value: stringOrNil(setting.Value),
		}
	}
	drives := make([]api.DriveSettings, len(n.Drives))
	for i, drive := range n.Drives {
		drives[i] = api.DriveSettings{
			Id: drive.ID, Directory: drive.Directory, Label: stringOrNil(drive.Label), Filesystem: filesystemOf(drive.Filesystem),
			TotalBytes: bytesOf(drive.TotalBytes), FreeBytes: bytesOf(drive.FreeBytes), Online: drive.Online,
		}
		if set := drive.ErasureSet; set >= 0 {
			drives[i].ErasureSet = &set
		}
	}
	certificates := make([]api.SettingsCertificate, len(n.Certificates))
	for i, certificate := range n.Certificates {
		certificates[i] = api.SettingsCertificate{
			Use: certificate.Use, Subject: certificate.Subject, Issuer: certificate.Issuer,
			DnsNames: stringsOf(certificate.DNSNames), IpAddresses: stringsOf(certificate.IPAddresses),
			NotBefore: certificate.NotBefore, NotAfter: certificate.NotAfter,
		}
	}
	out := api.NodeSettings{
		NodeId: answer.NodeID, Answered: true, Roles: &roles, Labels: &labels,
		StartedAt: timeOrNil(n.StartedAt), KmsKeyId: stringOrNil(n.KMSKeyID),
		Settings: &settings, Drives: &drives, Certificates: &certificates,
	}
	if n.Build != (cluster.Build{}) {
		out.Server = &api.ServerVersion{Version: n.Build.Version, Build: n.Build.Build, Commit: stringOrNil(n.Build.Commit)}
	}
	if n.Metadata != (cluster.Volume{}) {
		out.Metadata = &api.SettingsVolume{
			Directory: n.Metadata.Directory, Filesystem: filesystemOf(n.Metadata.Filesystem),
			TotalBytes: bytesOf(n.Metadata.TotalBytes), FreeBytes: bytesOf(n.Metadata.FreeBytes),
		}
	}
	return out
}

// GetSettings implements api.StrictServerInterface. It translates what the
// use case gathered and decides nothing: who may read it, what is redacted
// and what a node that did not answer is said to be are the use case's.
func (o *operations) GetSettings(ctx context.Context, _ api.GetSettingsRequestObject) (api.GetSettingsResponseObject, error) {
	if o.h.cfg.Settings == nil {
		return nil, fmt.Errorf("settings: %w", errNotImplemented)
	}
	view, err := o.h.cfg.Settings.Get(ctx, who(ctx))
	if err != nil {
		return nil, err
	}
	out := api.Settings{
		AnsweredBy: view.AnsweredBy,
		Cluster: api.SettingsCluster{
			DeploymentId: view.Cluster.DeploymentID, MetadataLevel: view.Cluster.MetadataLevel, Leader: stringOrNil(view.Cluster.Leader),
			Nodes: view.Cluster.Nodes, Voters: view.Cluster.Voters,
			ErasureSets: make([]api.SettingsErasureSet, len(view.Cluster.ErasureSets)),
		},
		Nodes: make([]api.NodeSettings, len(view.Nodes)), Mismatches: make([]api.SettingsMismatch, len(view.Mismatches)),
	}
	for i, set := range view.Cluster.ErasureSets {
		out.Cluster.ErasureSets[i] = api.SettingsErasureSet{
			Index: set.Index, Drives: set.Drives, Data: set.Data, Parity: set.Parity,
			ReadQuorum: set.ReadQuorum, WriteQuorum: set.WriteQuorum,
		}
	}
	for i, answer := range view.Nodes {
		out.Nodes[i] = nodeSettingsOf(answer)
	}
	for i, mismatch := range view.Mismatches {
		values := make([]api.SettingsMismatchValue, len(mismatch.Values))
		for j, value := range mismatch.Values {
			values[j] = api.SettingsMismatchValue{NodeId: value.NodeID, Value: value.Value}
		}
		out.Mismatches[i] = api.SettingsMismatch{Setting: mismatch.Setting, Values: values}
	}
	return api.GetSettings200JSONResponse(out), nil
}

// --- audit ---

const defaultAuditLimit = 100

// sequenceOf carries a sequence number of the trail into the contract's
// signed integer.
func sequenceOf(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}

// ListAudit implements api.StrictServerInterface.
func (o *operations) ListAudit(ctx context.Context, request api.ListAuditRequestObject) (api.ListAuditResponseObject, error) {
	after, limit := optional(request.Params.After), defaultAuditLimit
	if l := request.Params.Limit; l != nil {
		limit = *l
	}
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: after must not be negative and limit must be 1 to 1000", domain.ErrInvalidArgument)
	}
	// One more than asked for tells whether there is a next page.
	records, err := o.h.cfg.IAM.AuditLog(ctx, who(ctx), uint64(after), limit+1)
	if err != nil {
		return nil, err
	}
	page := api.AuditPage{}
	if len(records) > limit {
		records = records[:limit]
		next := sequenceOf(records[limit-1].Sequence)
		page.NextAfter = &next
	}
	page.Records = make([]api.AuditRecord, len(records))
	for i, r := range records {
		record := api.AuditRecord{
			Sequence: sequenceOf(r.Sequence), Time: r.Time, Actor: r.Actor, Action: r.Action, Resource: r.Resource,
			Outcome: string(r.Outcome), Hash: r.Hash, PreviousHash: r.PreviousHash,
			AccessKeyId: stringOrNil(r.AccessKeyID), SourceIp: stringOrNil(r.SourceIP), RequestId: stringOrNil(r.RequestID),
		}
		if len(r.Details) > 0 {
			details := r.Details
			record.Details = &details
		}
		page.Records[i] = record
	}
	return api.ListAudit200JSONResponse(page), nil
}

// --- quotas, targets, notifications, replication (PLAN 8.0.3) ---
//
// The operations below are in the contract and have no use case yet: the
// contract comes first, and each is written with the step of PLAN.md that
// names it. Until then a caller that has authenticated is told 501, as for
// any operation this node was not given the use case for - which says
// nothing of any bucket or target, so it is said before anybody is asked
// what the caller may do.

// notYet is the answer of an operation that is not written yet.
func notYet(operation string) error {
	return fmt.Errorf("%s: %w", operation, errNotImplemented)
}

// GetBucketQuota implements api.StrictServerInterface (PLAN 8.3.3).
func (o *operations) GetBucketQuota(context.Context, api.GetBucketQuotaRequestObject) (api.GetBucketQuotaResponseObject, error) {
	return nil, notYet("quota")
}

// PutBucketQuota implements api.StrictServerInterface (PLAN 8.3.3).
func (o *operations) PutBucketQuota(context.Context, api.PutBucketQuotaRequestObject) (api.PutBucketQuotaResponseObject, error) {
	return nil, notYet("quota")
}

// DeleteBucketQuota implements api.StrictServerInterface (PLAN 8.3.3).
func (o *operations) DeleteBucketQuota(context.Context, api.DeleteBucketQuotaRequestObject) (api.DeleteBucketQuotaResponseObject, error) {
	return nil, notYet("quota")
}

// RecountBucketUsage implements api.StrictServerInterface (PLAN 8.3.3).
func (o *operations) RecountBucketUsage(context.Context, api.RecountBucketUsageRequestObject) (api.RecountBucketUsageResponseObject, error) {
	return nil, notYet("usage recount")
}

// ListTargets implements api.StrictServerInterface (PLAN 8.0.13).
func (o *operations) ListTargets(context.Context, api.ListTargetsRequestObject) (api.ListTargetsResponseObject, error) {
	return nil, notYet("targets")
}

// GetTarget implements api.StrictServerInterface (PLAN 8.0.13).
func (o *operations) GetTarget(context.Context, api.GetTargetRequestObject) (api.GetTargetResponseObject, error) {
	return nil, notYet("targets")
}

// PutTarget implements api.StrictServerInterface (PLAN 8.0.13).
func (o *operations) PutTarget(context.Context, api.PutTargetRequestObject) (api.PutTargetResponseObject, error) {
	return nil, notYet("targets")
}

// DeleteTarget implements api.StrictServerInterface (PLAN 8.0.13).
func (o *operations) DeleteTarget(context.Context, api.DeleteTargetRequestObject) (api.DeleteTargetResponseObject, error) {
	return nil, notYet("targets")
}

// GetTargetStatus implements api.StrictServerInterface (PLAN 8.0.14).
func (o *operations) GetTargetStatus(context.Context, api.GetTargetStatusRequestObject) (api.GetTargetStatusResponseObject, error) {
	return nil, notYet("target status")
}

// GetBucketNotifications implements api.StrictServerInterface (PLAN 8.2.2).
func (o *operations) GetBucketNotifications(context.Context, api.GetBucketNotificationsRequestObject) (api.GetBucketNotificationsResponseObject, error) {
	return nil, notYet("notifications")
}

// PutBucketNotifications implements api.StrictServerInterface (PLAN 8.2.2).
func (o *operations) PutBucketNotifications(context.Context, api.PutBucketNotificationsRequestObject) (api.PutBucketNotificationsResponseObject, error) {
	return nil, notYet("notifications")
}

// DeleteBucketNotifications implements api.StrictServerInterface (PLAN 8.2.2).
func (o *operations) DeleteBucketNotifications(context.Context, api.DeleteBucketNotificationsRequestObject) (api.DeleteBucketNotificationsResponseObject, error) {
	return nil, notYet("notifications")
}

// GetBucketReplication implements api.StrictServerInterface (PLAN 8.1.3).
func (o *operations) GetBucketReplication(context.Context, api.GetBucketReplicationRequestObject) (api.GetBucketReplicationResponseObject, error) {
	return nil, notYet("replication")
}

// PutBucketReplication implements api.StrictServerInterface (PLAN 8.1.3).
func (o *operations) PutBucketReplication(context.Context, api.PutBucketReplicationRequestObject) (api.PutBucketReplicationResponseObject, error) {
	return nil, notYet("replication")
}

// DeleteBucketReplication implements api.StrictServerInterface (PLAN 8.1.3).
func (o *operations) DeleteBucketReplication(context.Context, api.DeleteBucketReplicationRequestObject) (api.DeleteBucketReplicationResponseObject, error) {
	return nil, notYet("replication")
}

// GetBucketReplicationStatus implements api.StrictServerInterface (PLAN 8.1.7).
func (o *operations) GetBucketReplicationStatus(context.Context, api.GetBucketReplicationStatusRequestObject) (api.GetBucketReplicationStatusResponseObject, error) {
	return nil, notYet("replication status")
}

// ResyncBucketReplication implements api.StrictServerInterface (PLAN 8.1.6).
func (o *operations) ResyncBucketReplication(context.Context, api.ResyncBucketReplicationRequestObject) (api.ResyncBucketReplicationResponseObject, error) {
	return nil, notYet("replication resync")
}

// GetBucketReplicaMode implements api.StrictServerInterface (PLAN 8.1.1).
func (o *operations) GetBucketReplicaMode(context.Context, api.GetBucketReplicaModeRequestObject) (api.GetBucketReplicaModeResponseObject, error) {
	return nil, notYet("replica mode")
}

// PutBucketReplicaMode implements api.StrictServerInterface (PLAN 8.1.1).
func (o *operations) PutBucketReplicaMode(context.Context, api.PutBucketReplicaModeRequestObject) (api.PutBucketReplicaModeResponseObject, error) {
	return nil, notYet("replica mode")
}
