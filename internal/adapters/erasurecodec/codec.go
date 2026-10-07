// Package erasurecodec is the outbound adapter for Reed-Solomon erasure
// coding, on top of github.com/klauspost/reedsolomon.
package erasurecodec

import (
	"fmt"
	"sync"

	"github.com/klauspost/reedsolomon"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/erasure"
	"github.com/albievan/cactusfs/internal/ports"
)

// Codec is a ports.ErasureCodec. The zero value is not usable; call New.
type Codec struct {
	mu       sync.Mutex
	encoders map[erasure.Params]reedsolomon.Encoder
}

var _ ports.ErasureCodec = (*Codec)(nil)

// New returns a Codec.
func New() *Codec {
	return &Codec{encoders: map[erasure.Params]reedsolomon.Encoder{}}
}

// encoder returns the Reed-Solomon encoder for params, which must have
// parity. Encoders are built once and are safe for concurrent use.
func (c *Codec) encoder(params erasure.Params) (reedsolomon.Encoder, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if enc, ok := c.encoders[params]; ok {
		return enc, nil
	}
	enc, err := reedsolomon.New(params.Data, params.Parity)
	if err != nil {
		return nil, fmt.Errorf("%w: reed-solomon %d+%d: %w", domain.ErrInvalidArgument, params.Data, params.Parity, err)
	}
	c.encoders[params] = enc
	return enc, nil
}

// Encode implements ports.ErasureCodec.
func (c *Codec) Encode(params erasure.Params, stripe []byte) ([][]byte, error) {
	if err := params.Validate(); err != nil {
		return nil, fmt.Errorf("erasurecodec.Encode: %w", err)
	}
	if len(stripe) == 0 {
		return nil, fmt.Errorf("erasurecodec.Encode: %w: empty stripe", domain.ErrInvalidArgument)
	}
	chunk := (len(stripe) + params.Data - 1) / params.Data

	// The chunks are copies: the caller reuses its stripe buffer.
	chunks := make([][]byte, params.Total())
	backing := make([]byte, chunk*params.Total())
	for i := range chunks {
		chunks[i] = backing[i*chunk : (i+1)*chunk]
	}
	for i := range params.Data {
		start := min(i*chunk, len(stripe))
		copy(chunks[i], stripe[start:min(start+chunk, len(stripe))])
	}
	if params.Parity == 0 {
		return chunks, nil
	}
	enc, err := c.encoder(params)
	if err != nil {
		return nil, fmt.Errorf("erasurecodec.Encode: %w", err)
	}
	if err := enc.Encode(chunks); err != nil {
		return nil, fmt.Errorf("erasurecodec.Encode: %w", err)
	}
	return chunks, nil
}

// Reconstruct implements ports.ErasureCodec.
func (c *Codec) Reconstruct(params erasure.Params, chunks [][]byte, dataOnly bool) error {
	if err := params.Validate(); err != nil {
		return fmt.Errorf("erasurecodec.Reconstruct: %w", err)
	}
	if len(chunks) != params.Total() {
		return fmt.Errorf("erasurecodec.Reconstruct: %w: %d chunks for %d shards", domain.ErrInvalidArgument, len(chunks), params.Total())
	}
	present, missing, length := 0, 0, -1
	for i, chunk := range chunks {
		switch {
		case chunk == nil:
			if !dataOnly || i < params.Data {
				missing++
			}
		case length >= 0 && len(chunk) != length:
			return fmt.Errorf("erasurecodec.Reconstruct: %w: chunks differ in length", domain.ErrIntegrity)
		default:
			present++
			length = len(chunk)
		}
	}
	if missing == 0 {
		return nil
	}
	if present < params.Data || params.Parity == 0 {
		return fmt.Errorf("erasurecodec.Reconstruct: %w: %d of %d chunks are available, %d are needed",
			domain.ErrIntegrity, present, params.Total(), params.Data)
	}

	enc, err := c.encoder(params)
	if err != nil {
		return fmt.Errorf("erasurecodec.Reconstruct: %w", err)
	}
	if dataOnly {
		err = enc.ReconstructData(chunks)
	} else {
		err = enc.Reconstruct(chunks)
	}
	if err != nil {
		return fmt.Errorf("erasurecodec.Reconstruct: %w: %w", domain.ErrIntegrity, err)
	}
	return nil
}
