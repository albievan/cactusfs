package raftmeta

import (
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// The commands of entry schema 5 and what they carry (ADR-0023, "Entry
// schema 5"; ADR-0024 to ADR-0026). Each is of schema 5 by what it is
// (versionOf), is never written below it, and is refused by decodeEntry in
// an entry that claims an older schema.
//
// The encodings are pinned by the golden files of testdata/entry-*.golden.json:
// a place in the outbox is delivery.Seq.String(), 20 hexadecimal characters;
// a version is named as object.VersionRef.String() names it, "null" or 32
// hexadecimal characters; and what is optional is left out when it is empty
// or false.
//
// bucket.quota.put and bucket.usage.walk are applied in bucketusage.go;
// target.put and target.delete in targets.go; outbox.ack, and the rows every
// change of an object appends, in outbox.go; bucket.replica.put,
// replication.resync.put and what makes a version the copy of one of another
// deployment in replica.go.
const (
	cmdSetBucketQuota   = "bucket.quota.put"
	cmdWalkBucketUsage  = "bucket.usage.walk"
	cmdPutTarget        = "target.put"
	cmdDeleteTarget     = "target.delete"
	cmdAckOutbox        = "outbox.ack"
	cmdSetBucketReplica = "bucket.replica.put"
	cmdPutResync        = "replication.resync.put"
)

// bucketQuotaCmd gives a bucket a quota; Bytes 0 removes the one it has.
type bucketQuotaCmd struct {
	Bucket string `json:"bucket"`
	Bytes  int64  `json:"bytes"`
	// Audit is the record of the change, appended in the same transaction.
	Audit *auditRecord `json:"audit,omitempty"`
}

// usageWalkCmd is one entry of the walk that counts a bucket: it visits
// whole keys until it has visited Limit records, then the uploads. Restart
// begins a walk again, or a recount of a bucket whose record is exact.
type usageWalkCmd struct {
	Bucket  string `json:"bucket"`
	Limit   int    `json:"limit"`
	Restart bool   `json:"restart,omitempty"`
}

// sealedRecord is a secret sealed through the KMS, as the secret of an API
// key is (keyRecord).
type sealedRecord struct {
	KMSKeyID   string `json:"kms_key_id"`
	WrappedKey []byte `json:"wrapped_key"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

func sealedToRecord(s iam.SealedSecret) *sealedRecord {
	if s.KMSKeyID == "" && len(s.WrappedKey) == 0 && len(s.Nonce) == 0 && len(s.Ciphertext) == 0 {
		return nil
	}
	return &sealedRecord{KMSKeyID: s.KMSKeyID, WrappedKey: s.WrappedKey, Nonce: s.Nonce, Ciphertext: s.Ciphertext}
}

func (r *sealedRecord) toDomain() iam.SealedSecret {
	if r == nil {
		return iam.SealedSecret{}
	}
	return iam.SealedSecret{KMSKeyID: r.KMSKeyID, WrappedKey: r.WrappedKey, Nonce: r.Nonce, Ciphertext: r.Ciphertext}
}

// targetRecordVersion is the version of a targetRecord.
const targetRecordVersion = 1

// targetAuthRecord is how a target is authenticated to: the mode, and the
// identity the secret belongs to where the mode has one.
type targetAuthRecord struct {
	Mode string `json:"mode"`
	ID   string `json:"id,omitempty"`
}

// targetRecord is a target as an entry carries it and as it is stored
// (ADR-0023, "New in the state"): its fields in this order. The secrets are
// sealed; the plaintext never enters the log. Refs is the store's to count
// and CreatedAt the store's to keep for a target that exists: what an entry
// says of either is not taken.
//
// Endpoint is the origin of the URL the target was defined with - scheme,
// host, port - and EndpointPath its path, sealed through the KMS by the use
// case before it proposed the entry: the path of a webhook may be its
// credential, and neither the log nor the state ever holds one in the clear.
// A target without a path has no EndpointPath.
type targetRecord struct {
	V            int           `json:"v"`
	Name         string        `json:"name"`
	Kind         string        `json:"kind"`
	Endpoint     string        `json:"endpoint"`
	EndpointPath *sealedRecord `json:"endpoint_path,omitempty"`
	Region       string        `json:"region,omitempty"`
	Subject      string        `json:"subject,omitempty"`
	CAPEM        []byte        `json:"ca_pem,omitempty"`
	Buckets      []string      `json:"buckets"`
	// Audit says that the target is sent the audit trail.
	Audit     bool             `json:"audit,omitempty"`
	Auth      targetAuthRecord `json:"auth"`
	Secret    *sealedRecord    `json:"secret,omitempty"`
	SignKey   *sealedRecord    `json:"sign_key,omitempty"`
	Refs      int              `json:"refs,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

func targetToRecord(t delivery.Target) targetRecord {
	buckets := make([]string, len(t.Buckets))
	copy(buckets, t.Buckets)
	return targetRecord{
		V: targetRecordVersion, Name: t.Name, Kind: string(t.Kind), Endpoint: t.Endpoint, EndpointPath: sealedToRecord(t.SealedPath),
		Region: t.Region, Subject: t.Subject,
		CAPEM: t.CAPEM, Buckets: buckets, Audit: t.Audit, Auth: targetAuthRecord{Mode: string(t.Auth.Mode), ID: t.Auth.ID},
		Secret: sealedToRecord(t.Secret), SignKey: sealedToRecord(t.SignKey), Refs: t.Refs,
		CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(),
	}
}

func (r targetRecord) toDomain() delivery.Target {
	return delivery.Target{
		Name: r.Name, Kind: delivery.Kind(r.Kind), Endpoint: r.Endpoint, SealedPath: r.EndpointPath.toDomain(),
		Region: r.Region, Subject: r.Subject,
		CAPEM: r.CAPEM, Buckets: r.Buckets, Audit: r.Audit, Auth: delivery.Auth{Mode: delivery.AuthMode(r.Auth.Mode), ID: r.Auth.ID},
		Secret: r.Secret.toDomain(), SignKey: r.SignKey.toDomain(), Refs: r.Refs, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// targetCmd defines a target, or replaces one.
type targetCmd struct {
	Target targetRecord `json:"target"`
	// Audit is the record of the change, appended in the same transaction.
	Audit *auditRecord `json:"audit,omitempty"`
}

// targetRefCmd names a target: to delete it.
type targetRefCmd struct {
	Name  string       `json:"name"`
	Audit *auditRecord `json:"audit,omitempty"`
}

// ackResult is what became of one version that was to be replicated. It
// names a row of the outbox by Seq, or a version by Bucket, Key and
// VersionRef - never both.
type ackResult struct {
	// Seq is the place of the row, as delivery.Seq.String() writes it.
	Seq string `json:"seq,omitempty"`
	// Bucket, Key and VersionRef name the version; VersionRef as
	// object.VersionRef.String() writes it.
	Bucket     string `json:"b,omitempty"`
	Key        string `json:"k,omitempty"`
	VersionRef string `json:"vid,omitempty"`
	// Status is COMPLETED or FAILED.
	Status string `json:"status"`
}

func resultToRecord(r delivery.Result) ackResult {
	if r.ByRow() {
		return ackResult{Seq: r.Seq.String(), Status: string(r.Status)}
	}
	return ackResult{Bucket: r.Bucket, Key: r.Key, VersionRef: r.Version.String(), Status: string(r.Status)}
}

// toDomain reads a result of an entry. The error wraps
// domain.ErrInvalidArgument for a place or a version that is not written as
// one, and for a result that names a row and a version both: what was read
// is then held to delivery.Result.Validate by whoever applies it.
func (r ackResult) toDomain() (delivery.Result, error) {
	out := delivery.Result{Bucket: r.Bucket, Key: r.Key, Status: object.ReplicationStatus(r.Status)}
	if r.Seq != "" {
		seq, err := delivery.ParseSeq(r.Seq)
		if err != nil {
			return delivery.Result{}, err
		}
		if seq.IsZero() {
			return delivery.Result{}, fmt.Errorf("%w: a result names a row there is", domain.ErrInvalidArgument)
		}
		out.Seq = seq
	}
	if r.VersionRef != "" {
		ref, err := object.ParseVersionRef(r.VersionRef)
		if err != nil {
			return delivery.Result{}, err
		}
		out.Version = ref
	}
	return out, nil
}

// ackOutboxCmd acknowledges what a target was sent: the rows up to Upto,
// the audit records up to AuditUpto, and what became of the versions that
// were to be replicated.
type ackOutboxCmd struct {
	Target string `json:"target"`
	// Upto is a place in the outbox, as delivery.Seq.String() writes it.
	Upto      string      `json:"upto"`
	AuditUpto uint64      `json:"audit_upto,omitempty"`
	Results   []ackResult `json:"results,omitempty"`
}

// bucketReplicaCmd makes a bucket the replica of a deployment; Source empty
// ends that.
type bucketReplicaCmd struct {
	Bucket string `json:"bucket"`
	Source string `json:"source"`
	// Audit is the record of the change, appended in the same transaction.
	Audit *auditRecord `json:"audit,omitempty"`
}

// resyncCmd is the record of the walk that sends another deployment what it
// lacks of a bucket: bucket.Resync, every field always written. It is stored
// as resyncRecord, which says its version before it.
type resyncCmd struct {
	Bucket  string `json:"bucket"`
	Target  string `json:"target"`
	State   string `json:"state"`
	Key     string `json:"key"`
	Version string `json:"version"`
	Scanned uint64 `json:"scanned"`
	Sent    uint64 `json:"sent"`
}

func resyncToRecord(r bucket.Resync) *resyncCmd {
	return &resyncCmd{
		Bucket: r.Bucket, Target: r.Target, State: string(r.State), Key: r.Key, Version: r.Version, Scanned: r.Scanned, Sent: r.Sent,
	}
}

func (r resyncCmd) toDomain() bucket.Resync {
	return bucket.Resync{
		Bucket: r.Bucket, Target: r.Target, State: bucket.ResyncState(r.State), Key: r.Key, Version: r.Version, Scanned: r.Scanned, Sent: r.Sent,
	}
}

// replicaRecord says of a version, a delete marker or the completion of an
// upload that it is the copy of one of another deployment: object.Replica.
// Null is left out when false.
type replicaRecord struct {
	From string `json:"from"`
	Seq  uint64 `json:"seq"`
	Null bool   `json:"null,omitempty"`
}

func replicaToRecord(r *object.Replica) *replicaRecord {
	if r == nil {
		return nil
	}
	return &replicaRecord{From: r.From, Seq: r.Seq, Null: r.Null}
}

func (r *replicaRecord) toDomain() *object.Replica {
	if r == nil {
		return nil
	}
	return &object.Replica{From: r.From, Seq: r.Seq, Null: r.Null}
}
