package object

import (
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
)

// MaxKeyLength is the longest object key, in bytes of its UTF-8 encoding.
const MaxKeyLength = 1024

// MaxSize is the largest object that can be stored in a single PUT.
const MaxSize = 5 << 30

// MaxUserMetadataSize is the limit on the total size of user-defined metadata:
// the sum of the bytes of every key and value.
const MaxUserMetadataSize = 2 << 10

// ValidateKey reports whether key is a valid object key: 1 to 1024 bytes of
// valid UTF-8. Keys are opaque names in the index and never become filesystem
// paths, so no character is forbidden.
//
// The returned error wraps domain.ErrInvalidObjectKey.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: key is empty", domain.ErrInvalidObjectKey)
	case len(key) > MaxKeyLength:
		return fmt.Errorf("%w: key is %d bytes, the maximum is %d", domain.ErrInvalidObjectKey, len(key), MaxKeyLength)
	case !utf8.ValidString(key):
		return fmt.Errorf("%w: key is not valid UTF-8", domain.ErrInvalidObjectKey)
	}
	return nil
}

// ValidateUserMetadata reports whether the user-defined metadata fits the size
// limit. The returned error wraps domain.ErrInvalidArgument.
func ValidateUserMetadata(metadata map[string]string) error {
	total := 0
	for key, value := range metadata {
		total += len(key) + len(value)
	}
	if total > MaxUserMetadataSize {
		return fmt.Errorf("%w: user metadata is %d bytes, the maximum is %d",
			domain.ErrInvalidArgument, total, MaxUserMetadataSize)
	}
	return nil
}

// Version is one immutable version of an object: what a client stored and
// where and how its bytes are kept.
type Version struct {
	// Bucket and Key name the object.
	Bucket string
	Key    string
	// VersionID identifies this version.
	VersionID ID
	// DataID addresses the stored bytes. It is generated per write.
	DataID ID
	// Size is the length of the object as the client sees it, in bytes.
	Size int64
	// ETag is the entity tag returned to clients, without quotes.
	ETag string
	// ModifiedAt is when the version was committed.
	ModifiedAt time.Time

	// Standard HTTP representation metadata supplied by the client.
	ContentType        string
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	// UserMetadata holds the client's own metadata, keyed by lowercase name
	// without the x-amz-meta- prefix.
	UserMetadata map[string]string

	// Encryption describes how the stored bytes are encrypted.
	Encryption Encryption
	// Layout describes how the encrypted bytes are spread over drives.
	Layout Layout

	// Versioned says that the version was written while its bucket kept
	// versions: it has an ID of its own. A version that is not versioned is
	// the null version of its key, of which there is at most one.
	Versioned bool
	// Seq orders the versions of a key: the newest has the greatest. It
	// is given when the version is committed.
	Seq uint64
	// CreatedBy is who committed the version, as the audit trail names
	// actors ("root", "user/<name>", "key/<id>"): whoever put the object,
	// copied it here, or completed its multipart upload. Empty for a
	// version written before this was recorded (entry schema 3, ADR-0016).
	CreatedBy string
	// DeleteMarker says that this version stands for the object having
	// been deleted: it has no content.
	DeleteMarker bool
	// Lock is what keeps the version from being removed, if anything.
	Lock Lock
	// Tags are the tags of the version.
	Tags map[string]string
	// Parts, when not zero, says that the version was made by a multipart
	// upload and consists of that many parts. Its bytes are then those of
	// its parts, in order: DataID names the parts (it was the upload's ID)
	// and addresses no stored data of its own, and Layout is empty.
	Parts int
	// Replication says where the version stands in replication between
	// deployments (ADR-0026); empty for a version no rule is about.
	Replication ReplicationStatus
	// Replica, when not nil, says that the version is committed as the
	// copy of a version of another deployment: with the ID and the time
	// it has there, and at the sequence it has there. It is said by
	// whoever commits the version and is not kept: what is stored of it
	// is Seq, and Replication, which is ReplicationReplica.
	Replica *Replica
}

// ReplicationStatus is where a version stands in replication between
// deployments, as x-amz-replication-status tells it. The empty status is a
// version no rule is about.
type ReplicationStatus string

// The replication statuses. They are stored with the version, not worked out
// from how far delivery has come: after a rule was edited or rows were lost,
// that would call versions replicated that never were.
const (
	// ReplicationPending is a version a rule matched when it was
	// committed, and that the other deployment has not taken yet.
	ReplicationPending ReplicationStatus = "PENDING"
	// ReplicationCompleted is a version the other deployment took, or
	// said it had.
	ReplicationCompleted ReplicationStatus = "COMPLETED"
	// ReplicationFailed is a version that cannot be replicated: one
	// encrypted with a key of the customer's, or one the other deployment
	// refuses for itself.
	ReplicationFailed ReplicationStatus = "FAILED"
	// ReplicationReplica is a version that arrived from another
	// deployment. It is not replicated again.
	ReplicationReplica ReplicationStatus = "REPLICA"
)

// Valid reports whether s is a status a version can have, the empty one
// among them.
func (s ReplicationStatus) Valid() bool {
	switch s {
	case "", ReplicationPending, ReplicationCompleted, ReplicationFailed, ReplicationReplica:
		return true
	}
	return false
}

// MaxDeploymentIDLength is the longest ID of a deployment that a replica can
// name as its source.
const MaxDeploymentIDLength = 64

// ValidateReplicaSource reports whether source can be the ID of the
// deployment a bucket takes its versions from: 1 to 64 letters, digits and
// hyphens. The error wraps domain.ErrInvalidArgument.
//
// It is part of the replicated format: the state machine of every member
// refuses a source by it, and it looks at bytes only.
func ValidateReplicaSource(source string) error {
	valid := source != "" && len(source) <= MaxDeploymentIDLength
	for i := 0; valid && i < len(source); i++ {
		c := source[i]
		valid = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-'
	}
	if !valid {
		return fmt.Errorf("%w: a deployment is named with 1 to %d letters, digits and hyphens", domain.ErrInvalidArgument, MaxDeploymentIDLength)
	}
	return nil
}

// MaxReplicaSeq is the greatest sequence a replica can have at its source:
// 2^62. A copy is stored at the sequence of its source, and a version made
// here afterwards is given one above the greatest the bucket holds; a replica
// at the end of the numbers would leave nothing above it, and no version
// could ever outrank it. No deployment comes near the bound by itself: a
// sequence is the index of an entry of its log.
const MaxReplicaSeq = 1 << 62

// Replica says of a version, a delete marker or a multipart upload that it
// is the copy of one of another deployment (ADR-0026).
type Replica struct {
	// From is the ID of the deployment the version comes from. A replica
	// bucket takes versions from the one deployment it is a replica of.
	From string
	// Seq is the sequence the version has there, from 1 to MaxReplicaSeq.
	// The copy is stored at it, so that the versions of a key are in the
	// order of their source whatever order they arrive in.
	Seq uint64
	// Null says that the version is the null version of its key at the
	// source - one written before the bucket there kept versions - and is
	// stored as the null version here.
	Null bool
}

// Validate reports whether r names a source and a sequence from 1 to
// MaxReplicaSeq. The error wraps domain.ErrInvalidArgument.
//
// It is part of the replicated format: the state machine of every member
// refuses a replica by it.
func (r Replica) Validate() error {
	if err := ValidateReplicaSource(r.From); err != nil {
		return err
	}
	if r.Seq == 0 || r.Seq > MaxReplicaSeq {
		return fmt.Errorf("%w: a replica has the sequence of its source, which is from 1 to 2^62", domain.ErrInvalidArgument)
	}
	return nil
}

// Multipart reports whether the version consists of parts.
func (v Version) Multipart() bool {
	return v.Parts > 0
}

// NullVersion is how the null version is named to clients.
const NullVersion = "null"

// VersionRef names one version of a key: the null version, or the version
// with an ID.
type VersionRef struct {
	Null bool
	ID   ID
}

// ParseVersionRef parses a version as clients name it: "null", or the 32
// lowercase hexadecimal characters of an ID. Anything else wraps
// domain.ErrInvalidArgument.
func ParseVersionRef(s string) (VersionRef, error) {
	if s == NullVersion {
		return VersionRef{Null: true}, nil
	}
	id, err := ParseID(s)
	if err != nil || id.IsZero() {
		return VersionRef{}, fmt.Errorf("%w: version ID %q", domain.ErrInvalidArgument, s)
	}
	return VersionRef{ID: id}, nil
}

// String returns the reference as clients name it.
func (r VersionRef) String() string {
	if r.Null {
		return NullVersion
	}
	return r.ID.String()
}

// Ref returns the reference that names this version.
func (v Version) Ref() VersionRef {
	if !v.Versioned {
		return VersionRef{Null: true}
	}
	return VersionRef{ID: v.VersionID}
}

// Is reports whether ref names this version.
func (v Version) Is(ref VersionRef) bool {
	return v.Ref() == ref
}

// Encryption describes the envelope encryption of one object version.
type Encryption struct {
	// Scheme names the streaming AEAD format, see StreamSchemeV1.
	Scheme string
	// KMSKeyID identifies the KMS key that wraps the data key.
	KMSKeyID string
	// WrappedKey is the object's data key, wrapped by the KMS key.
	WrappedKey []byte
	// NoncePrefix is the random part of every segment nonce.
	NoncePrefix []byte
	// SegmentSize is the plaintext size of every segment but the last.
	SegmentSize int
	// Mode is how the data key is managed: SSES3, SSEKMS or SSEC.
	Mode string
	// Context is the canonical SSE-KMS encryption context, bound to the
	// wrapped key.
	Context []byte
	// KeyCheck belongs to SSE-C: the value a later read checks the
	// client's key against. The key itself is never stored, nor any
	// digest of it.
	KeyCheck []byte
	// KeyMode says how the data key is used: "" encrypts the version's
	// stream with it; KeyModePartHKDF derives a key per part from it.
	KeyMode string
}

// Layout describes how the encrypted bytes of one object version are stored.
// The stream is cut into stripes of StripeSize bytes; every stripe is split
// into DataShards equal parts and extended with ParityShards parity parts.
// Shard i of every stripe is appended to the shard at Shards[i].
type Layout struct {
	// DataShards and ParityShards are the erasure coding parameters. With
	// one data shard and no parity the single shard is the stream itself.
	DataShards   int
	ParityShards int
	// StripeSize is the number of stream bytes per stripe.
	StripeSize int
	// StoredSize is the length of the encrypted stream in bytes.
	StoredSize int64
	// Shards locates each shard, by shard index.
	Shards []ShardLocation
}

// ShardLocation says where one shard of an object version lives.
type ShardLocation struct {
	// DriveID is the generated identifier of the drive.
	DriveID string
	// Size is the number of bytes written to the shard. A shard of any other
	// length is damaged, which lets healing tell which shard to rebuild.
	Size int64
}

// ShardID addresses one shard of stored data.
type ShardID struct {
	// DataID is the object version's data identifier.
	DataID ID
	// Index is the position of the shard in the layout.
	Index int
}

// String returns a log-friendly form of the shard identifier.
func (s ShardID) String() string {
	return fmt.Sprintf("%s.%d", s.DataID, s.Index)
}
