package telemetry

import (
	"context"
	"io"

	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// InstrumentSiteClient returns next wrapped so that every call to another
// deployment is traced and counted (ADR-0026), under the port "site".
// Neither a bucket, a key nor the target is a label or an attribute: a
// bucket's name and a key are a tenant's, and a target names a site. What
// each request does on the wire is seen by the egress client the site
// client is given (ObserveEgress). The bytes of every version and part sent
// are counted as the client reads them (cactusfs_replication_bytes_total).
func (t *Telemetry) InstrumentSiteClient(next ports.SiteClient) ports.SiteClient {
	return &siteClient{t: t, next: next}
}

type siteClient struct {
	t    *Telemetry
	next ports.SiteClient
}

const portSite = "site"

func (*siteClient) instrumented() {}

// Check implements ports.SiteClient.
func (s *siteClient) Check(ctx context.Context, bucketName string) (ports.SiteBucket, error) {
	ctx, done := s.t.call(ctx, portSite, "Check")
	answer, err := s.next.Check(ctx, bucketName)
	done(err)
	return answer, err
}

// PutReplica implements ports.SiteClient.
func (s *siteClient) PutReplica(ctx context.Context, v object.Version, body io.Reader) error {
	ctx, done := s.t.call(ctx, portSite, "PutReplica")
	err := s.next.PutReplica(ctx, v, s.t.countedBody(body))
	done(err)
	return err
}

// CreateUpload implements ports.SiteClient.
func (s *siteClient) CreateUpload(ctx context.Context, v object.Version) (string, error) {
	ctx, done := s.t.call(ctx, portSite, "CreateUpload")
	id, err := s.next.CreateUpload(ctx, v)
	done(err)
	return id, err
}

// PutPart implements ports.SiteClient.
func (s *siteClient) PutPart(ctx context.Context, bucketName, key, uploadID string, number int, size int64, body io.Reader) (string, error) {
	ctx, done := s.t.call(ctx, portSite, "PutPart")
	etag, err := s.next.PutPart(ctx, bucketName, key, uploadID, number, size, s.t.countedBody(body))
	done(err)
	return etag, err
}

// CompleteReplica implements ports.SiteClient.
func (s *siteClient) CompleteReplica(ctx context.Context, v object.Version, uploadID string, parts []ports.SitePart) error {
	ctx, done := s.t.call(ctx, portSite, "CompleteReplica")
	err := s.next.CompleteReplica(ctx, v, uploadID, parts)
	done(err)
	return err
}

// AbortUpload implements ports.SiteClient.
func (s *siteClient) AbortUpload(ctx context.Context, bucketName, key, uploadID string) error {
	ctx, done := s.t.call(ctx, portSite, "AbortUpload")
	err := s.next.AbortUpload(ctx, bucketName, key, uploadID)
	done(err)
	return err
}

// ListParts implements ports.SiteClient.
func (s *siteClient) ListParts(ctx context.Context, bucketName, key, uploadID string) ([]ports.SitePart, bool, error) {
	ctx, done := s.t.call(ctx, portSite, "ListParts")
	parts, found, err := s.next.ListParts(ctx, bucketName, key, uploadID)
	done(err)
	return parts, found, err
}

// ListReplicaUploads implements ports.SiteClient.
func (s *siteClient) ListReplicaUploads(ctx context.Context, bucketName, key string) ([]string, error) {
	ctx, done := s.t.call(ctx, portSite, "ListReplicaUploads")
	ids, err := s.next.ListReplicaUploads(ctx, bucketName, key)
	done(err)
	return ids, err
}

// PutMarker implements ports.SiteClient.
func (s *siteClient) PutMarker(ctx context.Context, v object.Version) error {
	ctx, done := s.t.call(ctx, portSite, "PutMarker")
	err := s.next.PutMarker(ctx, v)
	done(err)
	return err
}

// HeadVersion implements ports.SiteClient.
func (s *siteClient) HeadVersion(ctx context.Context, bucketName, key string, ref object.VersionRef) (string, bool, error) {
	ctx, done := s.t.call(ctx, portSite, "HeadVersion")
	etag, found, err := s.next.HeadVersion(ctx, bucketName, key, ref)
	done(err)
	return etag, found, err
}

// PutTags implements ports.SiteClient.
func (s *siteClient) PutTags(ctx context.Context, bucketName, key string, ref object.VersionRef, tags map[string]string) error {
	ctx, done := s.t.call(ctx, portSite, "PutTags")
	err := s.next.PutTags(ctx, bucketName, key, ref, tags)
	done(err)
	return err
}

// PutRetention implements ports.SiteClient.
func (s *siteClient) PutRetention(ctx context.Context, bucketName, key string, ref object.VersionRef, r *object.Retention) error {
	ctx, done := s.t.call(ctx, portSite, "PutRetention")
	err := s.next.PutRetention(ctx, bucketName, key, ref, r)
	done(err)
	return err
}

// PutLegalHold implements ports.SiteClient.
func (s *siteClient) PutLegalHold(ctx context.Context, bucketName, key string, ref object.VersionRef, on bool) error {
	ctx, done := s.t.call(ctx, portSite, "PutLegalHold")
	err := s.next.PutLegalHold(ctx, bucketName, key, ref, on)
	done(err)
	return err
}
