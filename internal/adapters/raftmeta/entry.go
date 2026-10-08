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

// Raft entry schema, version 1. See docs/adr/0003-metadata.md; any change
// that an older binary could not apply needs a new version, an ADR, a
// migration and an upgrade test.
//
// An entry is one version byte followed by a JSON document:
//
//	{"type": "<command>", "<command field>": {...}}
//
// Everything the state machine needs is in the entry: timestamps, identifiers
// and wrapped keys are produced by the proposer, never while applying, so
// that every replica computes the same state.
const entrySchemaVersion = 1

// MetadataVersion is the newest entry schema this binary can apply. A node
// announces it when it registers; see Store.entryVersion.
const MetadataVersion = entrySchemaVersion

// Command types.
const (
	cmdCreateBucket = "bucket.create"
	cmdDeleteBucket = "bucket.delete"
	cmdPutObject    = "object.put"
	cmdDeleteObject = "object.delete"
	cmdSetRoot      = "root.set"
	cmdSetTopology  = "topology.set"
	cmdSetNode      = "node.set"
	cmdRemoveNode   = "node.remove"
)

// entry is the decoded form of a Raft log entry. Exactly one of the command
// fields is set, the one that Type names.
type entry struct {
	Type         string           `json:"type"`
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
// Every command that exists today is part of version 1.
func versionOf(entry) byte {
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
		e.SetNode != nil, e.RemoveNode != nil, e.IAM != nil,
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
	return &bucketRecord{Name: b.Name, CreatedAt: b.CreatedAt.UTC()}
}

func (r bucketRecord) toDomain() bucket.Bucket {
	return bucket.Bucket{Name: r.Name, CreatedAt: r.CreatedAt}
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
		ContentType:        v.ContentType,
		ContentEncoding:    v.ContentEncoding,
		ContentDisposition: v.ContentDisposition,
		ContentLanguage:    v.ContentLanguage,
		CacheControl:       v.CacheControl,
		UserMetadata:       v.UserMetadata,
		Encryption: encryptionRecord{
			Scheme:      v.Encryption.Scheme,
			KMSKeyID:    v.Encryption.KMSKeyID,
			WrappedKey:  v.Encryption.WrappedKey,
			NoncePrefix: v.Encryption.NoncePrefix,
			SegmentSize: v.Encryption.SegmentSize,
		},
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
		Encryption: object.Encryption{
			Scheme:      r.Encryption.Scheme,
			KMSKeyID:    r.Encryption.KMSKeyID,
			WrappedKey:  r.Encryption.WrappedKey,
			NoncePrefix: r.Encryption.NoncePrefix,
			SegmentSize: r.Encryption.SegmentSize,
		},
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
}

func encodeResult(r applyResult) ([]byte, error) {
	w := wireResult{Previous: r.previous}
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
	r := applyResult{previous: w.Previous}
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
	case cmdDeleteUser, cmdDeleteGroup, cmdDeleteRole, cmdDeletePolicy, cmdDeleteKey, cmdTouchKey, cmdDeleteSession:
		return c.Name != ""
	default:
		return true
	}
}
