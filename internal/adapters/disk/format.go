package disk

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// Shard file format, version 1. See docs/adr/0001-on-disk-format.md; any
// change here needs a new version, an ADR, a migration and an upgrade test.
//
//	header (64 bytes)
//	   0  magic            8 bytes  "CFSSHARD"
//	   8  format version   uint16   1
//	  10  header length    uint16   64
//	  12  block size       uint32   data bytes per block
//	  16  data ID          16 bytes
//	  32  shard index      uint32
//	  36  reserved         12 bytes zero
//	  48  header checksum  16 bytes first half of SHA-256(header[0:48])
//	blocks, back to back
//	      checksum         32 bytes SHA-256(data ID || shard index || block number || data)
//	      data             block size bytes; fewer in the last block only
//
// All integers are big endian. An empty shard has no blocks.
const (
	formatVersion  = 1
	headerSize     = 64
	headerChecked  = 48
	headerSumSize  = 16
	checksumSize   = sha256.Size
	defaultBlock   = 128 << 10
	maxBlock       = 16 << 20
	maxShardIndex  = 255
	shardMagic     = "CFSSHARD"
	driveFileMagic = "cactusfs-drive"
)

// header is the decoded header of a shard file.
type header struct {
	blockSize int
	id        object.ShardID
}

func encodeHeader(h header) ([]byte, error) {
	if h.blockSize <= 0 || h.blockSize > maxBlock || h.id.Index < 0 || h.id.Index > maxShardIndex {
		return nil, fmt.Errorf("%w: shard header out of range", domain.ErrInvalidArgument)
	}
	buf := make([]byte, 0, headerSize)
	buf = append(buf, shardMagic...)
	buf = binary.BigEndian.AppendUint16(buf, formatVersion)
	buf = binary.BigEndian.AppendUint16(buf, headerSize)
	buf = binary.BigEndian.AppendUint32(buf, uint32(h.blockSize))
	buf = append(buf, h.id.DataID[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(h.id.Index))
	buf = append(buf, make([]byte, headerChecked-len(buf))...)
	sum := sha256.Sum256(buf)
	return append(buf, sum[:headerSumSize]...), nil
}

// decodeHeader parses and verifies a shard header. A damaged header wraps
// domain.ErrIntegrity; an intact header of another version wraps
// domain.ErrUnsupportedFormat.
func decodeHeader(buf []byte) (header, error) {
	if len(buf) != headerSize || string(buf[:8]) != shardMagic {
		return header{}, fmt.Errorf("%w: not a shard file", domain.ErrIntegrity)
	}
	sum := sha256.Sum256(buf[:headerChecked])
	if string(sum[:headerSumSize]) != string(buf[headerChecked:]) {
		return header{}, fmt.Errorf("%w: shard header checksum mismatch", domain.ErrIntegrity)
	}
	if version := binary.BigEndian.Uint16(buf[8:]); version != formatVersion {
		return header{}, fmt.Errorf("%w: shard format version %d", domain.ErrUnsupportedFormat, version)
	}
	if length := binary.BigEndian.Uint16(buf[10:]); length != headerSize {
		return header{}, fmt.Errorf("%w: shard header length %d", domain.ErrUnsupportedFormat, length)
	}
	blockSize := binary.BigEndian.Uint32(buf[12:])
	index := binary.BigEndian.Uint32(buf[32:])
	if blockSize == 0 || blockSize > maxBlock || index > maxShardIndex {
		return header{}, fmt.Errorf("%w: shard header out of range", domain.ErrIntegrity)
	}
	h := header{blockSize: int(blockSize)}
	copy(h.id.DataID[:], buf[16:32])
	h.id.Index = int(index)
	return h, nil
}

// checksumIndex returns the shard index in the form the block checksum
// covers.
func checksumIndex(id object.ShardID) (uint32, error) {
	if id.Index < 0 || id.Index > maxShardIndex {
		return 0, fmt.Errorf("%w: shard index %d", domain.ErrInvalidArgument, id.Index)
	}
	return uint32(id.Index), nil
}

// blockChecksum binds a block's data to the shard and the position it belongs
// to, so that a block or a whole file moved elsewhere is detected.
func blockChecksum(dataID object.ID, index uint32, block uint64, data []byte) [checksumSize]byte {
	var prefix [object.IDSize + 4 + 8]byte
	copy(prefix[:], dataID[:])
	binary.BigEndian.PutUint32(prefix[object.IDSize:], index)
	binary.BigEndian.PutUint64(prefix[object.IDSize+4:], block)

	h := sha256.New()
	h.Write(prefix[:])
	h.Write(data)
	var sum [checksumSize]byte
	h.Sum(sum[:0])
	return sum
}

// logicalSize returns the number of data bytes in a shard file of the given
// size. A size that no sequence of blocks can produce wraps
// domain.ErrIntegrity.
func logicalSize(fileSize int64, blockSize int) (int64, error) {
	body := fileSize - headerSize
	if body < 0 {
		return 0, fmt.Errorf("%w: shard file is shorter than its header", domain.ErrIntegrity)
	}
	stored := int64(checksumSize + blockSize)
	full, rest := body/stored, body%stored
	if rest == 0 {
		return full * int64(blockSize), nil
	}
	if rest <= checksumSize {
		return 0, fmt.Errorf("%w: shard file ends inside a block checksum", domain.ErrIntegrity)
	}
	return full*int64(blockSize) + rest - checksumSize, nil
}

// shardPath returns the path of a shard inside the drive root. It is built
// from the hexadecimal data ID and the numeric index only.
func shardPath(id object.ShardID) (dir, path string, err error) {
	if id.Index < 0 || id.Index > maxShardIndex {
		return "", "", fmt.Errorf("%w: shard index %d", domain.ErrInvalidArgument, id.Index)
	}
	name := id.DataID.String()
	dir = shardsDir + "/" + name[:2] + "/" + name[2:4]
	return dir, fmt.Sprintf("%s/%s.%d", dir, name, id.Index), nil
}
