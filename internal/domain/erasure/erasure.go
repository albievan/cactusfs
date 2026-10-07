// Package erasure holds the domain rules of erasure coding: how many data and
// parity shards an object gets, what a quorum is, how the encrypted stream of
// an object is cut into stripes and shards, and which drives hold them.
package erasure

import (
	"encoding/binary"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

const (
	// MinSetDrives is the smallest erasure set that has parity.
	MinSetDrives = 4
	// MaxSetDrives is the largest erasure set.
	MaxSetDrives = 16
	// DefaultStripeSize is the number of stream bytes per stripe.
	DefaultStripeSize = 1 << 20
	// MaxStripeSize bounds the stripe size a stored object may declare.
	MaxStripeSize = 64 << 20
	// maxShards is the limit of the Reed-Solomon code over GF(2^8).
	maxShards = 256
)

// Params are the erasure coding parameters of one object: it is stored as
// Data shards of content plus Parity shards of redundancy, and survives the
// loss of any Parity of them.
type Params struct {
	Data   int
	Parity int
}

// Total returns the number of shards.
func (p Params) Total() int {
	return p.Data + p.Parity
}

// Validate reports whether the parameters describe a usable code. The error
// wraps domain.ErrInvalidArgument.
func (p Params) Validate() error {
	if p.Data < 1 || p.Parity < 0 || p.Total() > maxShards {
		return fmt.Errorf("%w: erasure parameters %d+%d", domain.ErrInvalidArgument, p.Data, p.Parity)
	}
	return nil
}

// ReadQuorum is the number of shards needed to read an object: any Data of
// them.
func (p Params) ReadQuorum() int {
	return p.Data
}

// WriteQuorum is the number of shards that must be durable before a write may
// be committed: Data, plus one when Data equals Parity. Without the extra
// shard, two halves of a set could each accept a different write of the same
// object.
func (p Params) WriteQuorum() int {
	if p.Parity > 0 && p.Data == p.Parity {
		return p.Data + 1
	}
	return p.Data
}

// ParamsFor returns the default parameters for an erasure set of the given
// number of drives: parity 2 for 4 or 5 drives, 3 for 6 or 7, 4 for 8 to 16.
// With fewer than four drives there is no parity: an object is one shard on
// one drive, still encrypted and checksummed.
func ParamsFor(setDrives int) (Params, error) {
	switch {
	case setDrives < 1 || setDrives > MaxSetDrives:
		return Params{}, fmt.Errorf("%w: an erasure set has 1 to %d drives, not %d", domain.ErrInvalidArgument, MaxSetDrives, setDrives)
	case setDrives < MinSetDrives:
		return Params{Data: 1, Parity: 0}, nil
	case setDrives <= 5:
		return Params{Data: setDrives - 2, Parity: 2}, nil
	case setDrives <= 7:
		return Params{Data: setDrives - 3, Parity: 3}, nil
	default:
		return Params{Data: setDrives - 4, Parity: 4}, nil
	}
}

// SetLayout divides a number of drives into erasure sets of equal size: one
// set for up to 16 drives, otherwise the fewest sets of at most 16. A count
// that cannot be divided evenly is refused, because sets of different sizes
// would give objects different durability.
func SetLayout(drives int) (sets, size int, err error) {
	if drives < 1 {
		return 0, 0, fmt.Errorf("%w: at least one drive is required", domain.ErrInvalidArgument)
	}
	sets = (drives + MaxSetDrives - 1) / MaxSetDrives
	if drives%sets != 0 {
		return 0, 0, fmt.Errorf("%w: %d drives cannot be divided into %d equal erasure sets of at most %d drives; "+
			"use %d or %d drives", domain.ErrInvalidArgument, drives, sets, MaxSetDrives, drives-drives%sets, drives+sets-drives%sets)
	}
	return sets, drives / sets, nil
}

// Placement chooses where the shards of a new object go: the erasure set, and
// for each shard the position of its drive in that set. The choice is derived
// from the object's random data ID, so objects spread evenly and no two
// shards of one object share a drive. total must not exceed size.
func Placement(dataID object.ID, sets, size, total int) (set int, positions []int, err error) {
	if sets < 1 || size < 1 || total < 1 || total > size {
		return 0, nil, fmt.Errorf("%w: cannot place %d shards on %d sets of %d drives", domain.ErrInvalidArgument, total, sets, size)
	}
	// Sixteen random bits each are plenty to choose among a handful of sets
	// and at most sixteen drives.
	set = int(binary.BigEndian.Uint16(dataID[:2])) % sets
	first := int(binary.BigEndian.Uint16(dataID[8:10])) % size
	positions = make([]int, total)
	for i := range positions {
		positions[i] = (first + i) % size
	}
	return set, positions, nil
}

// Geometry describes how a stream is cut into stripes and shards. Every
// stripe holds StripeSize bytes of the stream, the last one possibly fewer.
// A stripe is split into Data chunks of equal length, the last padded with
// zeros, and extended with Parity chunks of that length. Chunk i of every
// stripe is appended to shard i.
type Geometry struct {
	Params
	StripeSize int
}

// Validate reports whether the geometry is usable. The error wraps
// domain.ErrInvalidArgument.
func (g Geometry) Validate() error {
	if err := g.Params.Validate(); err != nil {
		return err
	}
	if g.StripeSize < 1 || g.StripeSize > MaxStripeSize {
		return fmt.Errorf("%w: stripe size %d", domain.ErrInvalidArgument, g.StripeSize)
	}
	return nil
}

// Chunk returns the length of each shard's chunk of a stripe that holds
// stripeLen stream bytes.
func (g Geometry) Chunk(stripeLen int) int {
	return (stripeLen + g.Data - 1) / g.Data
}

// Stripes returns the number of stripes of a stream of the given length.
func (g Geometry) Stripes(streamSize int64) int64 {
	return (streamSize + int64(g.StripeSize) - 1) / int64(g.StripeSize)
}

// StripeLen returns the number of stream bytes in a stripe.
func (g Geometry) StripeLen(streamSize, stripe int64) int {
	return int(min(int64(g.StripeSize), streamSize-stripe*int64(g.StripeSize)))
}

// ShardOffset returns where the chunk of a stripe begins in each shard.
func (g Geometry) ShardOffset(stripe int64) int64 {
	return stripe * int64(g.Chunk(g.StripeSize))
}

// ShardSize returns the length of each shard of a stream of the given length.
func (g Geometry) ShardSize(streamSize int64) int64 {
	stripes := g.Stripes(streamSize)
	if stripes == 0 {
		return 0
	}
	last := g.StripeLen(streamSize, stripes-1)
	return (stripes-1)*int64(g.Chunk(g.StripeSize)) + int64(g.Chunk(last))
}
