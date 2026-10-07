package porttest

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/erasure"
	"github.com/albievan/cactusfs/internal/ports"
)

// Codec is a ports.ErasureCodec for tests: a small systematic erasure code
// over GF(2^8) with a Cauchy matrix. It is written independently of the
// production adapter, slow and simple, and passes the same contract.
type Codec struct {
	exp [510]byte
	log [256]byte
}

var _ ports.ErasureCodec = (*Codec)(nil)

// NewCodec returns a Codec.
func NewCodec() *Codec {
	c := &Codec{}
	x := 1
	for i := range 255 {
		c.exp[i], c.exp[i+255] = byte(x), byte(x)
		c.log[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11d
		}
	}
	return c
}

func (c *Codec) mul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return c.exp[int(c.log[a])+int(c.log[b])]
}

func (c *Codec) inv(a byte) byte {
	return c.exp[255-int(c.log[a])]
}

// row returns the coefficients that produce shard index from the data
// chunks: a unit vector for a data shard, a Cauchy row for a parity shard.
func (c *Codec) row(params erasure.Params, index int) []byte {
	row := make([]byte, params.Data)
	if index < params.Data {
		row[index] = 1
		return row
	}
	for i := range row {
		row[i] = c.inv(element(index) ^ element(i))
	}
	return row
}

// element returns the field element for a shard index. Indexes are below
// 256, which erasure.Params.Validate guarantees.
func element(index int) byte {
	if index < 0 || index > 255 {
		return 0
	}
	return byte(index)
}

// Encode implements ports.ErasureCodec.
func (c *Codec) Encode(params erasure.Params, stripe []byte) ([][]byte, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	if len(stripe) == 0 {
		return nil, fmt.Errorf("%w: empty stripe", domain.ErrInvalidArgument)
	}
	size := (len(stripe) + params.Data - 1) / params.Data
	chunks := make([][]byte, params.Total())
	for i := range chunks {
		chunks[i] = make([]byte, size)
	}
	for i := range params.Data {
		start := min(i*size, len(stripe))
		copy(chunks[i], stripe[start:min(start+size, len(stripe))])
	}
	for p := params.Data; p < params.Total(); p++ {
		c.combine(chunks[p], c.row(params, p), chunks[:params.Data])
	}
	return chunks, nil
}

// combine sets dst to the linear combination of sources with coefficients.
func (c *Codec) combine(dst, coefficients []byte, sources [][]byte) {
	clear(dst)
	for i, coefficient := range coefficients {
		for b, value := range sources[i] {
			dst[b] ^= c.mul(coefficient, value)
		}
	}
}

// Reconstruct implements ports.ErasureCodec.
func (c *Codec) Reconstruct(params erasure.Params, chunks [][]byte, dataOnly bool) error {
	if err := params.Validate(); err != nil {
		return err
	}
	if len(chunks) != params.Total() {
		return fmt.Errorf("%w: %d chunks for %d shards", domain.ErrInvalidArgument, len(chunks), params.Total())
	}
	var present []int
	size := -1
	for i, chunk := range chunks {
		if chunk == nil {
			continue
		}
		if size >= 0 && len(chunk) != size {
			return fmt.Errorf("%w: chunks differ in length", domain.ErrIntegrity)
		}
		size = len(chunk)
		present = append(present, i)
	}
	missing := false
	for i, chunk := range chunks {
		if chunk == nil && (!dataOnly || i < params.Data) {
			missing = true
		}
	}
	if !missing {
		return nil
	}
	if len(present) < params.Data {
		return fmt.Errorf("%w: %d of %d chunks are available, %d are needed", domain.ErrIntegrity, len(present), params.Total(), params.Data)
	}
	present = present[:params.Data]

	// Solve for the data chunks: invert the rows of the shards in hand.
	matrix := make([][]byte, params.Data)
	inverse := make([][]byte, params.Data)
	for r, index := range present {
		matrix[r] = c.row(params, index)
		inverse[r] = make([]byte, params.Data)
		inverse[r][r] = 1
	}
	for col := range params.Data {
		pivot := col
		for pivot < params.Data && matrix[pivot][col] == 0 {
			pivot++
		}
		if pivot == params.Data {
			return fmt.Errorf("%w: chunks cannot be decoded", domain.ErrIntegrity)
		}
		matrix[col], matrix[pivot] = matrix[pivot], matrix[col]
		inverse[col], inverse[pivot] = inverse[pivot], inverse[col]
		scale := c.inv(matrix[col][col])
		for k := range params.Data {
			matrix[col][k] = c.mul(matrix[col][k], scale)
			inverse[col][k] = c.mul(inverse[col][k], scale)
		}
		for r := range params.Data {
			if factor := matrix[r][col]; r != col && factor != 0 {
				for k := range params.Data {
					matrix[r][k] ^= c.mul(factor, matrix[col][k])
					inverse[r][k] ^= c.mul(factor, inverse[col][k])
				}
			}
		}
	}
	sources := make([][]byte, params.Data)
	for r, index := range present {
		sources[r] = chunks[index]
	}
	data := make([][]byte, params.Data)
	for i := range data {
		if chunks[i] != nil {
			data[i] = chunks[i]
			continue
		}
		data[i] = make([]byte, size)
		c.combine(data[i], inverse[i], sources)
	}
	copy(chunks, data)
	if dataOnly {
		return nil
	}
	for p := params.Data; p < params.Total(); p++ {
		if chunks[p] == nil {
			chunks[p] = make([]byte, size)
			c.combine(chunks[p], c.row(params, p), data)
		}
	}
	return nil
}

// ErasureCodecContract is the behaviour every ports.ErasureCodec must have:
// encode a stripe, lose any chunks up to the parity count, and get exactly
// the original back; lose more, and get an error, never other bytes.
func ErasureCodecContract(t *testing.T, codec ports.ErasureCodec) {
	t.Helper()

	join := func(params erasure.Params, chunks [][]byte, length int) []byte {
		var out []byte
		for i := range params.Data {
			out = append(out, chunks[i]...)
		}
		return out[:length]
	}
	for _, params := range []erasure.Params{{Data: 1}, {Data: 2, Parity: 2}, {Data: 3, Parity: 2}, {Data: 3, Parity: 3}, {Data: 4, Parity: 4}, {Data: 12, Parity: 4}} {
		for _, length := range []int{1, params.Data, params.Data + 1, 1000, 4096 + 7} {
			stripe := make([]byte, length)
			if _, err := rand.Read(stripe); err != nil {
				t.Fatal(err)
			}
			original := bytes.Clone(stripe)
			chunks, err := codec.Encode(params, stripe)
			if err != nil {
				t.Fatalf("%+v, %d bytes: Encode: %v", params, length, err)
			}
			if len(chunks) != params.Total() {
				t.Fatalf("%+v: %d chunks, want %d", params, len(chunks), params.Total())
			}
			for _, chunk := range chunks {
				if len(chunk) != (length+params.Data-1)/params.Data {
					t.Fatalf("%+v, %d bytes: a chunk is %d bytes", params, length, len(chunk))
				}
			}
			// The caller reuses its stripe buffer.
			clear(stripe)
			if !bytes.Equal(join(params, chunks, length), original) {
				t.Fatalf("%+v, %d bytes: the data chunks are not the stripe", params, length)
			}

			for mask := 0; mask < 1<<params.Total(); mask++ {
				if params.Total() > 8 && mask%257 != 0 {
					continue // sample the large codes
				}
				lost := 0
				for _, dataOnly := range []bool{true, false} {
					work := make([][]byte, len(chunks))
					lost = 0
					for i, chunk := range chunks {
						if mask&(1<<i) != 0 {
							lost++
							continue
						}
						work[i] = bytes.Clone(chunk)
					}
					err := codec.Reconstruct(params, work, dataOnly)
					dataLost := false
					for i := range params.Data {
						dataLost = dataLost || mask&(1<<i) != 0
					}
					if lost > params.Parity && (dataLost || !dataOnly) {
						if !errors.Is(err, domain.ErrIntegrity) {
							t.Fatalf("%+v: lost %d chunks (mask %b): error = %v, want %v", params, lost, mask, err, domain.ErrIntegrity)
						}
						continue
					}
					if err != nil {
						t.Fatalf("%+v: lost %d chunks (mask %b, data only %v): %v", params, lost, mask, dataOnly, err)
					}
					if !bytes.Equal(join(params, work, length), original) {
						t.Fatalf("%+v: lost %d chunks (mask %b): decoded data differs", params, lost, mask)
					}
					if !dataOnly {
						for i := range work {
							if !bytes.Equal(work[i], chunks[i]) {
								t.Fatalf("%+v: chunk %d was not rebuilt exactly (mask %b)", params, i, mask)
							}
						}
					}
				}
			}
		}
	}

	good := erasure.Params{Data: 2, Parity: 2}
	if _, err := codec.Encode(good, nil); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Encode(empty stripe) error = %v, want %v", err, domain.ErrInvalidArgument)
	}
	if _, err := codec.Encode(erasure.Params{Data: 0, Parity: 1}, []byte("x")); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Encode(invalid parameters) error = %v, want %v", err, domain.ErrInvalidArgument)
	}
	if err := codec.Reconstruct(good, make([][]byte, 3), false); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Reconstruct(wrong chunk count) error = %v, want %v", err, domain.ErrInvalidArgument)
	}
	if err := codec.Reconstruct(good, [][]byte{{1, 2}, {3}, nil, nil}, false); !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("Reconstruct(unequal chunks) error = %v, want %v", err, domain.ErrIntegrity)
	}
}
