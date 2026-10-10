package raftmeta

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Raft entry schema. Version 1 is docs/adr/0003-metadata.md; version 2 is
// docs/adr/0008-object-versions.md. Any change that an older binary could not
// apply needs a new version, an ADR, a migration and an upgrade test.
//
// An entry is one version byte followed by a JSON document:
//
//	{"type": "<command>", "<command field>": {...}}
//
// Everything the state machine needs is in the entry: timestamps, identifiers
// and wrapped keys are produced by the proposer, never while applying, so
// that every replica computes the same state.
//
// Version 2 adds "at": the time the metadata leader gave the entry when it
// logged it. The state machine never reads a clock; what must be decided on
// time - whether a retention period has passed - is decided on this.
const (
	// entrySchemaVersion is the oldest entry schema, which every binary
	// applies.
	entrySchemaVersion = 1
	// entrySchemaTimed is the schema whose entries carry the leader's time
	// and the commands of milestone 6.
	entrySchemaTimed = 2
)

// MetadataVersion is the newest entry schema this binary can apply. A node
// announces it when it registers; see Store.entryVersion.
const MetadataVersion = entrySchemaTimed

// Command types.
const (
	cmdCreateBucket = "bucket.create"
	cmdDeleteBucket = "bucket.delete"
	cmdPutObject    = "object.put"
	cmdDeleteObject = "object.delete"
	cmdSetRoot      = "root.set"
	// Schema 2.
	cmdSetVersioning = "bucket.versioning.put"
	cmdDeleteVersion = "object.version.delete"
	cmdSetBucketLock = "bucket.lock.put"
	cmdSetRetention  = "object.retention.put"
	cmdSetLegalHold  = "object.legalhold.put"
	cmdSetTopology   = "topology.set"
	cmdSetNode       = "node.set"
	cmdRemoveNode    = "node.remove"
)

// entry is the decoded form of a Raft log entry. Exactly one of the command
// fields is set, the one that Type names.
type entry struct {
	Type string `json:"type"`
	// At is the time the metadata leader gave the entry (schema 2).
	At           time.Time        `json:"at,omitzero"`
	CreateBucket *bucketRecord    `json:"create_bucket,omitempty"`
	DeleteBucket *deleteBucketCmd `json:"delete_bucket,omitempty"`
	PutObject    *versionRecord   `json:"put_object,omitempty"`
	DeleteObject *deleteObjectCmd `json:"delete_object,omitempty"`
	SetRoot      *rootRecord      `json:"set_root,omitempty"`
	SetTopology  *topologyRecord  `json:"set_topology,omitempty"`
	SetNode      *nodeRecord      `json:"set_node,omitempty"`
	RemoveNode   *removeNodeCmd   `json:"remove_node,omitempty"`
	// IAM is the payload of every identity, session and audit command.
	IAM *iamCmd `json:"iam,omitempty"`
	// Schema 2.
	SetVersioning *versioningCmd    `json:"set_versioning,omitempty"`
	DeleteVersion *deleteVersionCmd `json:"delete_version,omitempty"`
	SetBucketLock *bucketLockCmd    `json:"set_bucket_lock,omitempty"`
	SetRetention  *retentionCmd     `json:"set_retention,omitempty"`
	SetLegalHold  *legalHoldCmd     `json:"set_legal_hold,omitempty"`
	// Multipart uploads (schema 2).
	CreateUpload   *uploadRecord      `json:"create_upload,omitempty"`
	PutPart        *putPartCmd        `json:"put_part,omitempty"`
	CompleteUpload *completeUploadCmd `json:"complete_upload,omitempty"`
	AbortUpload    *uploadRefCmd      `json:"abort_upload,omitempty"`
	// Bucket configuration and tags (schema 2).
	SetBucketConfig *bucketConfigCmd `json:"set_bucket_config,omitempty"`
	SetTags         *tagsCmd         `json:"set_tags,omitempty"`
}

// bucketConfigCmd replaces one configuration document of a bucket; without
// a document it removes it.
type bucketConfigCmd struct {
	Bucket   string `json:"bucket"`
	Kind     string `json:"kind"`
	Document []byte `json:"document,omitempty"`
}

// tagsCmd replaces the tags of a version.
type tagsCmd struct {
	Bucket     string            `json:"bucket"`
	Key        string            `json:"key"`
	VersionRef string            `json:"version_ref"`
	Tags       map[string]string `json:"tags,omitempty"`
}

// versioningCmd changes the versioning state of a bucket.
type versioningCmd struct {
	Bucket string `json:"bucket"`
	Status string `json:"status"`
}

// retentionRule is the default retention of a bucket.
type retentionRule struct {
	Mode  string `json:"mode"`
	Days  int    `json:"days,omitempty"`
	Years int    `json:"years,omitempty"`
}

// lockRecord is what keeps a version from being removed.
type lockRecord struct {
	Mode        string    `json:"mode,omitempty"`
	RetainUntil time.Time `json:"retain_until,omitzero"`
	LegalHold   bool      `json:"legal_hold,omitempty"`
}

func lockToRecord(l object.Lock) *lockRecord {
	if l.IsZero() {
		return nil
	}
	record := &lockRecord{LegalHold: l.LegalHold}
	if l.Retention != nil {
		record.Mode, record.RetainUntil = string(l.Retention.Mode), l.Retention.Until.UTC()
	}
	return record
}

func (r *lockRecord) toDomain() object.Lock {
	if r == nil {
		return object.Lock{}
	}
	lock := object.Lock{LegalHold: r.LegalHold}
	if r.Mode != "" {
		lock.Retention = &object.Retention{Mode: object.RetentionMode(r.Mode), Until: r.RetainUntil}
	}
	return lock
}

// bucketLockCmd sets or removes the default retention of a bucket.
type bucketLockCmd struct {
	Bucket  string         `json:"bucket"`
	Default *retentionRule `json:"default,omitempty"`
}

// retentionCmd sets or removes the retention of one version. Whether it may
// is decided when it is applied, on the time of the entry.
type retentionCmd struct {
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	VersionRef string `json:"version"`
	// Mode empty removes the retention.
	Mode        string    `json:"mode,omitempty"`
	RetainUntil time.Time `json:"retain_until,omitzero"`
	// Bypass says that the proposer authorized the caller to lift a
	// governance retention. It lifts nothing else.
	Bypass bool `json:"bypass_governance,omitempty"`
}

// legalHoldCmd puts one version under legal hold, or takes it off.
type legalHoldCmd struct {
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	VersionRef string `json:"version"`
	On         bool   `json:"on"`
}

// deleteVersionCmd deletes an object, or one of its versions. What a delete
// without a version does is decided when it is applied.
type deleteVersionCmd struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	// VersionRef names the version to remove for good: "null" or an ID.
	// Empty deletes the object as its bucket's versioning state says.
	VersionRef string `json:"version,omitempty"`
	// MarkerID and At are the ID and time of the delete marker, should one
	// be made.
	MarkerID string    `json:"marker_id"`
	At       time.Time `json:"at"`
	// Bypass says that the proposer authorized the caller to lift a
	// governance retention. It lifts nothing else.
	Bypass bool `json:"bypass_governance,omitempty"`
	// Expect, when set, is the version ID the version this delete is
	// about must have; otherwise the delete is refused.
	Expect string `json:"expect,omitempty"`
}

type removeNodeCmd struct {
	ID string `json:"id"`
}

// nodeRecord is a node of the cluster as it is stored and replicated.
type nodeRecord struct {
	ID              string   `json:"id"`
	RPCAddr         string   `json:"rpc_addr"`
	RaftAddr        string   `json:"raft_addr"`
	Roles           []string `json:"roles"`
	Region          string   `json:"region,omitempty"`
	Zone            string   `json:"zone,omitempty"`
	Rack            string   `json:"rack,omitempty"`
	Host            string   `json:"host,omitempty"`
	MetadataVersion int      `json:"metadata_version"`
}

func nodeToRecord(n cluster.Node) *nodeRecord {
	roles := make([]string, len(n.Roles))
	for i, role := range n.Roles {
		roles[i] = string(role)
	}
	return &nodeRecord{
		ID: n.ID, RPCAddr: n.RPCAddr, RaftAddr: n.RaftAddr, Roles: roles,
		Region: n.Labels.Region, Zone: n.Labels.Zone, Rack: n.Labels.Rack, Host: n.Labels.Host,
		MetadataVersion: n.MetadataVersion,
	}
}

func (r nodeRecord) toDomain() cluster.Node {
	roles := make(cluster.Roles, len(r.Roles))
	for i, role := range r.Roles {
		roles[i] = cluster.Role(role)
	}
	return cluster.Node{
		ID: r.ID, RPCAddr: r.RPCAddr, RaftAddr: r.RaftAddr, Roles: roles,
		Labels:          cluster.Labels{Region: r.Region, Zone: r.Zone, Rack: r.Rack, Host: r.Host},
		MetadataVersion: r.MetadataVersion,
	}
}

type deleteBucketCmd struct {
	Name string `json:"name"`
}

type deleteObjectCmd struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// bucketRecord is a bucket as it is stored and replicated.
type bucketRecord struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Versioning is "", "Enabled" or "Suspended" (schema 2).
	Versioning string `json:"versioning,omitempty"`
	ObjectLock bool   `json:"object_lock,omitempty"`
	// DefaultRetention is given to versions put without a retention.
	DefaultRetention *retentionRule `json:"default_retention,omitempty"`
	// Config holds the configuration documents of the bucket by kind:
	// policy, CORS rules, tags (schema 2).
	Config map[string][]byte `json:"config,omitempty"`
}

// versionRecord is an object version as it is stored and replicated.
type versionRecord struct {
	Bucket     string    `json:"bucket"`
	Key        string    `json:"key"`
	VersionID  string    `json:"version_id"`
	DataID     string    `json:"data_id"`
	Size       int64     `json:"size"`
	ETag       string    `json:"etag"`
	ModifiedAt time.Time `json:"modified_at"`
	// Versioned, Seq and DeleteMarker are schema 2. A record without them
	// is the null version of its key, the oldest there can be.
	Versioned    bool   `json:"versioned,omitempty"`
	Seq          uint64 `json:"seq,omitempty"`
	DeleteMarker bool   `json:"delete_marker,omitempty"`
	// Lock is what keeps the version from being removed (schema 2).
	Lock *lockRecord `json:"lock,omitempty"`
	// Parts is the number of parts of a version a multipart upload made;
	// its data is then in "extents" under DataID (schema 2).
	Parts int `json:"parts,omitempty"`
	// Tags are the tags of the version (schema 2).
	Tags map[string]string `json:"tags,omitempty"`
	// Dropped is set only in the answer to an entry that removed this
	// version, never in the state: the parts that went with it, whose
	// data the proposer may now remove.
	Dropped []partRecord `json:"dropped,omitempty"`

	ContentType        string            `json:"content_type,omitempty"`
	ContentEncoding    string            `json:"content_encoding,omitempty"`
	ContentDisposition string            `json:"content_disposition,omitempty"`
	ContentLanguage    string            `json:"content_language,omitempty"`
	CacheControl       string            `json:"cache_control,omitempty"`
	UserMetadata       map[string]string `json:"user_metadata,omitempty"`

	Encryption encryptionRecord `json:"encryption"`
	Layout     layoutRecord     `json:"layout"`
}

type encryptionRecord struct {
	Scheme      string `json:"scheme"`
	KMSKeyID    string `json:"kms_key_id"`
	WrappedKey  []byte `json:"wrapped_key"`
	NoncePrefix []byte `json:"nonce_prefix"`
	SegmentSize int    `json:"segment_size"`
	// KeyMode says how the data key is used; empty for the key of one
	// stream (schema 2).
	KeyMode string `json:"key_mode,omitempty"`
	// Mode, Context and KeyCheck say whose key it takes to read: SSE-KMS
	// and SSE-C (schema 2). Empty for the default.
	Mode     string `json:"mode,omitempty"`
	Context  []byte `json:"context,omitempty"`
	KeyCheck []byte `json:"key_check,omitempty"`
}

func encryptionToRecord(e object.Encryption) encryptionRecord {
	return encryptionRecord{
		Scheme: e.Scheme, KMSKeyID: e.KMSKeyID, WrappedKey: e.WrappedKey, NoncePrefix: e.NoncePrefix, SegmentSize: e.SegmentSize,
		KeyMode: e.KeyMode, Mode: e.Mode, Context: e.Context, KeyCheck: e.KeyCheck,
	}
}

func (r encryptionRecord) toDomain() object.Encryption {
	return object.Encryption{
		Scheme: r.Scheme, KMSKeyID: r.KMSKeyID, WrappedKey: r.WrappedKey, NoncePrefix: r.NoncePrefix, SegmentSize: r.SegmentSize,
		KeyMode: r.KeyMode, Mode: r.Mode, Context: r.Context, KeyCheck: r.KeyCheck,
	}
}

type layoutRecord struct {
	DataShards   int           `json:"data_shards"`
	ParityShards int           `json:"parity_shards"`
	StripeSize   int           `json:"stripe_size"`
	StoredSize   int64         `json:"stored_size"`
	Shards       []shardRecord `json:"shards"`
}

type shardRecord struct {
	DriveID string `json:"drive_id"`
	Size    int64  `json:"size"`
}

// rootRecord is the root credential as it is stored and replicated. The
// secret is sealed; the plaintext never enters the log.
type rootRecord struct {
	AccessKeyID string    `json:"access_key_id"`
	KMSKeyID    string    `json:"kms_key_id"`
	WrappedKey  []byte    `json:"wrapped_key"`
	Nonce       []byte    `json:"nonce"`
	Ciphertext  []byte    `json:"ciphertext"`
	CreatedAt   time.Time `json:"created_at"`
}

// topologyRecord is the drive topology as it is stored and replicated. In a
// topology.set command, Version is the version the change is based on; in
// the state it is the version of what is stored.
type topologyRecord struct {
	DeploymentID string         `json:"deployment_id"`
	Version      uint64         `json:"version"`
	Sets         [][]slotRecord `json:"sets"`
}

type slotRecord struct {
	DriveID string   `json:"drive_id"`
	NodeID  string   `json:"node_id,omitempty"`
	Retired []string `json:"retired,omitempty"`
}

func topologyToRecord(t cluster.Topology) *topologyRecord {
	record := &topologyRecord{DeploymentID: t.DeploymentID, Version: t.Version, Sets: make([][]slotRecord, len(t.Sets))}
	for s, set := range t.Sets {
		record.Sets[s] = make([]slotRecord, len(set))
		for i, slot := range set {
			record.Sets[s][i] = slotRecord{DriveID: slot.DriveID, NodeID: slot.NodeID, Retired: slot.Retired}
		}
	}
	return record
}

func (r topologyRecord) toDomain() cluster.Topology {
	t := cluster.Topology{DeploymentID: r.DeploymentID, Version: r.Version, Sets: make([][]cluster.Slot, len(r.Sets))}
	for s, set := range r.Sets {
		t.Sets[s] = make([]cluster.Slot, len(set))
		for i, slot := range set {
			t.Sets[s][i] = cluster.Slot{DriveID: slot.DriveID, NodeID: slot.NodeID, Retired: slot.Retired}
		}
	}
	return t
}

// encodeEntry serialises an entry for the Raft log, in the given schema
// version.
func encodeEntry(e entry, version byte) ([]byte, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("raftmeta: encode entry: %w", err)
	}
	return append([]byte{version}, body...), nil
}

// versionOf returns the oldest schema version that can express an entry.
func versionOf(e entry) byte {
	switch e.Type {
	case cmdSetVersioning, cmdDeleteVersion, cmdSetBucketLock, cmdSetRetention, cmdSetLegalHold,
		cmdCreateUpload, cmdPutPart, cmdCompleteUpload, cmdAbortUpload, cmdSetBucketConfig, cmdSetTags:
		return entrySchemaTimed
	}
	if !e.At.IsZero() || (e.PutObject != nil && (e.PutObject.Lock != nil || e.PutObject.Encryption.Mode != "" || len(e.PutObject.Tags) != 0)) {
		return entrySchemaTimed
	}
	// A bucket created with what only this schema knows: a binary of the
	// first would create it without, and not notice.
	if e.CreateBucket != nil && (e.CreateBucket.Versioning != "" || e.CreateBucket.ObjectLock) {
		return entrySchemaTimed
	}
	return entrySchemaVersion
}

// decodeEntry parses a Raft log entry. An entry of another schema version, or
// one this binary cannot interpret, wraps domain.ErrUnsupportedFormat: it must
// stop the state machine, never be skipped.
func decodeEntry(data []byte, supported byte) (entry, error) {
	if len(data) == 0 {
		return entry{}, fmt.Errorf("%w: empty metadata entry", domain.ErrUnsupportedFormat)
	}
	if data[0] < entrySchemaVersion || data[0] > supported {
		return entry{}, fmt.Errorf("%w: metadata entry schema version %d", domain.ErrUnsupportedFormat, data[0])
	}
	var e entry
	if err := json.Unmarshal(data[1:], &e); err != nil {
		return entry{}, fmt.Errorf("%w: malformed metadata entry: %w", domain.ErrUnsupportedFormat, err)
	}
	set := 0
	for _, present := range []bool{
		e.CreateBucket != nil, e.DeleteBucket != nil, e.PutObject != nil, e.DeleteObject != nil, e.SetRoot != nil, e.SetTopology != nil,
		e.SetNode != nil, e.RemoveNode != nil, e.IAM != nil, e.SetVersioning != nil, e.DeleteVersion != nil,
		e.SetBucketLock != nil, e.SetRetention != nil, e.SetLegalHold != nil,
		e.CreateUpload != nil, e.PutPart != nil, e.CompleteUpload != nil, e.AbortUpload != nil,
		e.SetBucketConfig != nil, e.SetTags != nil,
	} {
		if present {
			set++
		}
	}
	ok := set == 1
	switch e.Type {
	case cmdCreateBucket:
		ok = ok && e.CreateBucket != nil
	case cmdDeleteBucket:
		ok = ok && e.DeleteBucket != nil
	case cmdPutObject:
		ok = ok && e.PutObject != nil
	case cmdDeleteObject:
		ok = ok && e.DeleteObject != nil
	case cmdSetRoot:
		ok = ok && e.SetRoot != nil
	case cmdSetTopology:
		ok = ok && e.SetTopology != nil
	case cmdSetNode:
		ok = ok && e.SetNode != nil
	case cmdRemoveNode:
		ok = ok && e.RemoveNode != nil
	case cmdSetVersioning:
		ok = ok && e.SetVersioning != nil && data[0] >= entrySchemaTimed
	case cmdDeleteVersion:
		ok = ok && e.DeleteVersion != nil && data[0] >= entrySchemaTimed
	case cmdSetBucketLock:
		ok = ok && e.SetBucketLock != nil && data[0] >= entrySchemaTimed
	case cmdSetRetention:
		ok = ok && e.SetRetention != nil && data[0] >= entrySchemaTimed
	case cmdSetLegalHold:
		ok = ok && e.SetLegalHold != nil && data[0] >= entrySchemaTimed
	case cmdCreateUpload:
		ok = ok && e.CreateUpload != nil && data[0] >= entrySchemaTimed
	case cmdPutPart:
		ok = ok && e.PutPart != nil && data[0] >= entrySchemaTimed
	case cmdCompleteUpload:
		ok = ok && e.CompleteUpload != nil && data[0] >= entrySchemaTimed
	case cmdAbortUpload:
		ok = ok && e.AbortUpload != nil && data[0] >= entrySchemaTimed
	case cmdSetBucketConfig:
		ok = ok && e.SetBucketConfig != nil && data[0] >= entrySchemaTimed
	case cmdSetTags:
		ok = ok && e.SetTags != nil && data[0] >= entrySchemaTimed
	default:
		if !isIAMCommand(e.Type) {
			return entry{}, fmt.Errorf("%w: metadata command %q", domain.ErrUnsupportedFormat, e.Type)
		}
		ok = ok && e.IAM != nil && iamPayloadFits(e.Type, *e.IAM)
	}
	if !ok {
		return entry{}, fmt.Errorf("%w: metadata command %q does not match its payload", domain.ErrUnsupportedFormat, e.Type)
	}
	return e, nil
}

func bucketToRecord(b bucket.Bucket) *bucketRecord {
	record := &bucketRecord{Name: b.Name, CreatedAt: b.CreatedAt.UTC(), Versioning: string(b.Versioning), ObjectLock: b.ObjectLock}
	if d := b.DefaultRetention; d != nil {
		record.DefaultRetention = &retentionRule{Mode: string(d.Mode), Days: d.Days, Years: d.Years}
	}
	return record
}

func (r bucketRecord) toDomain() bucket.Bucket {
	b := bucket.Bucket{Name: r.Name, CreatedAt: r.CreatedAt, Versioning: bucket.Versioning(r.Versioning), ObjectLock: r.ObjectLock}
	if d := r.DefaultRetention; d != nil {
		b.DefaultRetention = &bucket.DefaultRetention{Mode: object.RetentionMode(d.Mode), Days: d.Days, Years: d.Years}
	}
	return b
}

func versionToRecord(v object.Version) *versionRecord {
	shards := make([]shardRecord, len(v.Layout.Shards))
	for i, s := range v.Layout.Shards {
		shards[i] = shardRecord{DriveID: s.DriveID, Size: s.Size}
	}
	return &versionRecord{
		Bucket:             v.Bucket,
		Key:                v.Key,
		VersionID:          v.VersionID.String(),
		DataID:             v.DataID.String(),
		Size:               v.Size,
		ETag:               v.ETag,
		ModifiedAt:         v.ModifiedAt.UTC(),
		Versioned:          v.Versioned,
		Seq:                v.Seq,
		DeleteMarker:       v.DeleteMarker,
		Lock:               lockToRecord(v.Lock),
		Parts:              v.Parts,
		Tags:               v.Tags,
		ContentType:        v.ContentType,
		ContentEncoding:    v.ContentEncoding,
		ContentDisposition: v.ContentDisposition,
		ContentLanguage:    v.ContentLanguage,
		CacheControl:       v.CacheControl,
		UserMetadata:       v.UserMetadata,
		Encryption:         encryptionToRecord(v.Encryption),
		Layout: layoutRecord{
			DataShards:   v.Layout.DataShards,
			ParityShards: v.Layout.ParityShards,
			StripeSize:   v.Layout.StripeSize,
			StoredSize:   v.Layout.StoredSize,
			Shards:       shards,
		},
	}
}

func (r versionRecord) toDomain() (object.Version, error) {
	versionID, err := object.ParseID(r.VersionID)
	if err != nil {
		return object.Version{}, fmt.Errorf("version ID: %w", err)
	}
	dataID, err := object.ParseID(r.DataID)
	if err != nil {
		return object.Version{}, fmt.Errorf("data ID: %w", err)
	}
	shards := make([]object.ShardLocation, len(r.Layout.Shards))
	for i, s := range r.Layout.Shards {
		shards[i] = object.ShardLocation{DriveID: s.DriveID, Size: s.Size}
	}
	return object.Version{
		Bucket:             r.Bucket,
		Key:                r.Key,
		VersionID:          versionID,
		Versioned:          r.Versioned,
		Seq:                r.Seq,
		DeleteMarker:       r.DeleteMarker,
		Lock:               r.Lock.toDomain(),
		Parts:              r.Parts,
		Tags:               r.Tags,
		DataID:             dataID,
		Size:               r.Size,
		ETag:               r.ETag,
		ModifiedAt:         r.ModifiedAt,
		ContentType:        r.ContentType,
		ContentEncoding:    r.ContentEncoding,
		ContentDisposition: r.ContentDisposition,
		ContentLanguage:    r.ContentLanguage,
		CacheControl:       r.CacheControl,
		UserMetadata:       r.UserMetadata,
		Encryption:         r.Encryption.toDomain(),
		Layout: object.Layout{
			DataShards:   r.Layout.DataShards,
			ParityShards: r.Layout.ParityShards,
			StripeSize:   r.Layout.StripeSize,
			StoredSize:   r.Layout.StoredSize,
			Shards:       shards,
		},
	}, nil
}

func rootToRecord(c iam.RootCredential) *rootRecord {
	return &rootRecord{
		AccessKeyID: c.AccessKeyID,
		KMSKeyID:    c.Secret.KMSKeyID,
		WrappedKey:  c.Secret.WrappedKey,
		Nonce:       c.Secret.Nonce,
		Ciphertext:  c.Secret.Ciphertext,
		CreatedAt:   c.CreatedAt.UTC(),
	}
}

func (r rootRecord) toDomain() iam.RootCredential {
	return iam.RootCredential{
		AccessKeyID: r.AccessKeyID,
		Secret: iam.SealedSecret{
			KMSKeyID: r.KMSKeyID, WrappedKey: r.WrappedKey, Nonce: r.Nonce, Ciphertext: r.Ciphertext,
		},
		CreatedAt: r.CreatedAt,
	}
}

// wireResult is the state machine's answer to an entry as it travels from the
// leader back to the node that proposed it.
type wireResult struct {
	// Codes and Message describe a refusal: the entry was applied and
	// changed nothing. Both are empty when it took effect.
	Codes    []string       `json:"codes,omitempty"`
	Message  string         `json:"message,omitempty"`
	Previous *versionRecord `json:"previous,omitempty"`
	// Stored is the version a put committed, and Marker the delete marker
	// a delete made (schema 2).
	Stored *versionRecord `json:"stored,omitempty"`
	Marker *versionRecord `json:"marker,omitempty"`
	// Parts are parts of an upload that the entry removed.
	Parts []partRecord `json:"parts,omitempty"`
}

func encodeResult(r applyResult) ([]byte, error) {
	w := wireResult{Previous: r.previous, Stored: r.stored, Marker: r.marker, Parts: r.parts}
	if r.err != nil {
		w.Codes, w.Message = domain.Codes(r.err), r.err.Error()
		if len(w.Codes) == 0 {
			// A refusal must stay a refusal on the other side.
			w.Codes = []string{"invalid_argument"}
		}
	}
	return json.Marshal(w)
}

func decodeResult(data []byte) (applyResult, error) {
	var w wireResult
	if err := json.Unmarshal(data, &w); err != nil {
		return applyResult{}, fmt.Errorf("malformed answer from the metadata leader: %w", err)
	}
	r := applyResult{previous: w.Previous, stored: w.Stored, marker: w.Marker, parts: w.Parts}
	if len(w.Codes) > 0 {
		r.err = domain.FromCodes(w.Codes, w.Message)
	}
	return r, nil
}

// iamPayloadFits reports whether an IAM command carries what its type needs.
func iamPayloadFits(kind string, c iamCmd) bool {
	switch kind {
	case cmdPutUser:
		return c.User != nil
	case cmdPutGroup:
		return c.Group != nil
	case cmdPutRole:
		return c.Role != nil
	case cmdPutPolicy:
		return c.Policy != nil
	case cmdCreateKey:
		return c.Key != nil
	case cmdPutSession:
		return c.Session != nil
	case cmdAppendAudit:
		return c.Audit != nil
	case cmdBeginIdem, cmdCompleteIdem:
		return c.Idem != nil && c.Idem.ID != ""
	case cmdDeleteUser, cmdDeleteGroup, cmdDeleteRole, cmdDeletePolicy, cmdDeleteKey, cmdTouchKey, cmdDeleteSession, cmdDeleteIdem:
		return c.Name != ""
	default:
		return true
	}
}
