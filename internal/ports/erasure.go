package ports

import "github.com/albievan/cactusfs/internal/domain/erasure"

// ErasureCodec computes and uses the parity of a stripe.
type ErasureCodec interface {
	// Encode splits a stripe into params.Data chunks of equal length, the
	// last padded with zeros, and appends params.Parity parity chunks of
	// that length. The stripe must not be empty.
	Encode(params erasure.Params, stripe []byte) ([][]byte, error)
	// Reconstruct fills in the missing chunks of a stripe. chunks has one
	// entry per shard; a nil entry is missing. The chunks that are present
	// must have equal length and at least params.Data of them are needed;
	// otherwise the error wraps domain.ErrIntegrity. With dataOnly set only
	// the data chunks are rebuilt, which is all a read needs.
	Reconstruct(params erasure.Params, chunks [][]byte, dataOnly bool) error
}
