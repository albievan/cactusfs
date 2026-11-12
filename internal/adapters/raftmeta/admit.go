package raftmeta

import (
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// maxTokenHashBytes bounds the key of a session record: the hex SHA-256 of
// a token (64 bytes), or one of the marks the session use case keeps among
// the sessions, a prefix and such a hash (under 100 bytes).
const maxTokenHashBytes = 128

// maxIdempotencyIDBytes bounds the key of an idempotency record, as
// checkIdempotent does where the record is proposed.
const maxIdempotencyIDBytes = 128

// admit refuses an entry that the state machine could not store, before it
// is logged. A key of no bytes, or of more than bbolt.MaxKeySize, fails the
// write of the state database; the state machine stops at such an entry, on
// every member, and again at every restart, for the entry stays in the log.
// The refusal is definite: nothing was logged.
//
// It is a rule of the proposal, not of the replicated format: what an entry
// in the log means is unchanged, and an entry already logged is applied as
// before. Each key an entry gives the state was audited (H35):
//
//   - bucket.create writes the bucket's name, unchecked where it is applied
//     (the release of schema 4 does not check it either): held here to 1 to
//     bucket.MaxNameLength bytes.
//   - session.put writes the token hash, unchecked where it is applied: held
//     here to 1..maxTokenHashBytes.
//   - idempotency.begin and .complete write the record's ID, which decodeEntry
//     holds to non-empty: held here to at most maxIdempotencyIDBytes.
//   - Every other key is checked where it is applied, and a key that fails is
//     a refusal: object keys (object.ValidateKey: put, version delete,
//     upload), user, group, role and policy names (iam.ValidateName), access
//     key IDs (iam.ValidateAccessKeyID), node IDs (cluster.ValidateNodeID:
//     node.set, a batch of reads), sub-bucket names, tag definition keys,
//     target names (delivery.Target.Validate) and replica sources; or it is
//     written only under a bucket, upload, version, target or key that
//     exists already, and so was checked when that was made; or it is fixed
//     bytes (an ID, a sequence, an index) the state machine makes itself.
func admit(e entry) error {
	switch {
	case e.CreateBucket != nil:
		// Held to its length, as Store.CreateBucket holds it: what a name
		// may consist of is the use case's to judge (bucket.ValidateName),
		// and the stores' contract creates buckets of shorter names. The
		// name is not repeated in the error.
		if name := e.CreateBucket.Name; name == "" || len(name) > bucket.MaxNameLength {
			return fmt.Errorf("%w: a name of %d bytes, it must have 1 to %d", domain.ErrInvalidBucketName, len(name), bucket.MaxNameLength)
		}
	case e.IAM != nil && e.Type == cmdPutSession && e.IAM.Session != nil:
		if hash := e.IAM.Session.TokenHash; hash == "" || len(hash) > maxTokenHashBytes {
			return fmt.Errorf("%w: a session's token hash of %d bytes, it must have 1 to %d", domain.ErrInvalidArgument, len(hash), maxTokenHashBytes)
		}
	case e.IAM != nil && (e.Type == cmdBeginIdem || e.Type == cmdCompleteIdem) && e.IAM.Idem != nil:
		if id := e.IAM.Idem.ID; id == "" || len(id) > maxIdempotencyIDBytes {
			return fmt.Errorf("%w: an idempotency record ID of %d bytes, it must have 1 to %d", domain.ErrInvalidArgument, len(id), maxIdempotencyIDBytes)
		}
	}
	return nil
}
