package noderpc

import (
	"errors"
	"fmt"

	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// NodeInfo is what a node says about itself.
type NodeInfo struct {
	Node cluster.Node
	// Formed reports whether the node knows of a metadata group.
	Formed bool
	// DeploymentID is the deployment the node knows it belongs to, or "".
	DeploymentID string
	Drives       []cluster.DriveState
}

// maxReadLength bounds one ReadShard.
const maxReadLength = 2 << 20

// maxShardIndex is the highest shard index that can be named on the wire;
// erasure sets have at most 16 drives.
const maxShardIndex = 255

// maxChunk bounds one chunk of a WriteShard stream, well below the limit on
// a message.
const maxChunk = 1 << 20

// toWire turns an error into its form on the wire; nil stays nil.
func toWire(err error) *nodev1.Error {
	if err == nil {
		return nil
	}
	return &nodev1.Error{Codes: domain.Codes(err), Message: err.Error()}
}

// fromWire rebuilds the error another node reported. It reads as the node's
// message, prefixed with which node said it.
func fromWire(nodeID string, e *nodev1.Error) error {
	if e == nil {
		return nil
	}
	return domain.FromCodes(e.GetCodes(), fmt.Sprintf("node %s: %s", nodeID, e.GetMessage()))
}

func shardToWire(id object.ShardID) *nodev1.ShardId {
	// An index outside the range no drive accepts is sent as one: the
	// other node refuses it.
	index := uint32(maxShardIndex + 1)
	if id.Index >= 0 && id.Index <= maxShardIndex {
		index = uint32(id.Index)
	}
	return &nodev1.ShardId{DataId: id.DataID[:], Index: index}
}

func shardFromWire(id *nodev1.ShardId) (object.ShardID, error) {
	dataID, err := dataIDFromWire(id.GetDataId())
	if err != nil {
		return object.ShardID{}, err
	}
	if id.GetIndex() > maxShardIndex {
		return object.ShardID{}, fmt.Errorf("%w: shard index %d", domain.ErrInvalidArgument, id.GetIndex())
	}
	return object.ShardID{DataID: dataID, Index: int(id.GetIndex())}, nil
}

func dataIDFromWire(raw []byte) (object.ID, error) {
	var id object.ID
	if len(raw) != len(id) {
		return id, fmt.Errorf("%w: data ID of %d bytes", domain.ErrInvalidID, len(raw))
	}
	copy(id[:], raw)
	return id, nil
}

var errNoHeader = errors.New("the stream did not begin with a header")
