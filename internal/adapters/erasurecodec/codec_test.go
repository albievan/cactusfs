package erasurecodec_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/erasurecodec"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/erasure"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func randomStripe(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// join puts the data chunks back together and cuts the padding off.
func join(params erasure.Params, chunks [][]byte, length int) []byte {
	var out []byte
	for i := range params.Data {
		out = append(out, chunks[i]...)
	}
	return out[:length]
}

func clone(chunks [][]byte) [][]byte {
	out := make([][]byte, len(chunks))
	for i, c := range chunks {
		out[i] = bytes.Clone(c)
	}
	return out
}

func TestContract(t *testing.T) {
	t.Parallel()
	porttest.ErasureCodecContract(t, erasurecodec.New())
}

// TestCodecsAgreeOnTheData: the test codec and the production codec are
// written independently; both must return the same data from the same losses.
func TestCodecsAgreeOnTheData(t *testing.T) {
	t.Parallel()
	real, fake := erasurecodec.New(), porttest.NewCodec()
	params := erasure.Params{Data: 4, Parity: 2}
	stripe := randomStripe(t, 5000)

	for _, codec := range []ports.ErasureCodec{real, fake} {
		chunks, err := codec.Encode(params, bytes.Clone(stripe))
		if err != nil {
			t.Fatal(err)
		}
		chunks[1], chunks[3] = nil, nil
		if err := codec.Reconstruct(params, chunks, true); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(join(params, chunks, len(stripe)), stripe) {
			t.Errorf("%T does not return the original data", codec)
		}
	}
}

// TestLoseUpToParityShards is the property erasure coding exists for: encode,
// lose any shards up to the parity count, decode, and get the original back.
// For small codes every combination of losses is tried.
func TestLoseUpToParityShards(t *testing.T) {
	t.Parallel()
	codec := erasurecodec.New()

	for _, params := range []erasure.Params{{Data: 1}, {Data: 2, Parity: 2}, {Data: 3, Parity: 2}, {Data: 3, Parity: 3}, {Data: 4, Parity: 4}, {Data: 12, Parity: 4}} {
		for _, length := range []int{1, params.Data - 1, params.Data, params.Data + 1, 1000, 4096 + 7} {
			if length < 1 {
				continue
			}
			stripe := randomStripe(t, length)
			chunks, err := codec.Encode(params, stripe)
			if err != nil {
				t.Fatalf("%+v, %d bytes: Encode: %v", params, length, err)
			}
			if len(chunks) != params.Total() {
				t.Fatalf("%+v: %d chunks, want %d", params, len(chunks), params.Total())
			}
			for _, chunk := range chunks {
				if len(chunk) != len(chunks[0]) {
					t.Fatalf("%+v: chunks differ in length", params)
				}
			}
			if !bytes.Equal(join(params, chunks, length), stripe) {
				t.Fatalf("%+v, %d bytes: the data chunks are not the stripe", params, length)
			}

			for mask := 0; mask < 1<<params.Total(); mask++ {
				lost := 0
				for i := range params.Total() {
					if mask&(1<<i) != 0 {
						lost++
					}
				}
				if params.Total() > 8 && mask%257 != 0 {
					continue // sample the large codes
				}
				damaged := clone(chunks)
				for i := range damaged {
					if mask&(1<<i) != 0 {
						damaged[i] = nil
					}
				}
				for _, dataOnly := range []bool{true, false} {
					work := clone(damaged)
					err := codec.Reconstruct(params, work, dataOnly)
					if lost > params.Parity {
						if !errors.Is(err, domain.ErrIntegrity) {
							t.Fatalf("%+v: lost %d shards (mask %b): error = %v, want %v", params, lost, mask, err, domain.ErrIntegrity)
						}
						continue
					}
					if err != nil {
						t.Fatalf("%+v: lost %d shards (mask %b): %v", params, lost, mask, err)
					}
					if !bytes.Equal(join(params, work, length), stripe) {
						t.Fatalf("%+v: lost %d shards (mask %b): decoded data differs", params, lost, mask)
					}
					if !dataOnly {
						for i := range work {
							if !bytes.Equal(work[i], chunks[i]) {
								t.Fatalf("%+v: shard %d was not rebuilt exactly (mask %b)", params, i, mask)
							}
						}
					}
				}
			}
		}
	}
}

func TestEncodeDoesNotAliasTheStripe(t *testing.T) {
	t.Parallel()
	codec := erasurecodec.New()
	stripe := randomStripe(t, 1000)
	original := bytes.Clone(stripe)

	chunks, err := codec.Encode(erasure.Params{Data: 4, Parity: 2}, stripe)
	if err != nil {
		t.Fatal(err)
	}
	clear(stripe) // the caller reuses its buffer
	if !bytes.Equal(join(erasure.Params{Data: 4, Parity: 2}, chunks, 1000), original) {
		t.Error("the chunks changed when the stripe buffer was reused")
	}
}

func TestInvalidInput(t *testing.T) {
	t.Parallel()
	codec := erasurecodec.New()
	good := erasure.Params{Data: 2, Parity: 2}

	if _, err := codec.Encode(good, nil); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Encode(empty) error = %v", err)
	}
	if _, err := codec.Encode(erasure.Params{Data: 0, Parity: 1}, []byte("x")); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Encode(bad params) error = %v", err)
	}
	if err := codec.Reconstruct(erasure.Params{Data: 0}, nil, false); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Reconstruct(bad params) error = %v", err)
	}
	if err := codec.Reconstruct(good, make([][]byte, 3), false); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Reconstruct(wrong chunk count) error = %v", err)
	}
	if err := codec.Reconstruct(good, [][]byte{{1, 2}, {3}, nil, nil}, false); !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("Reconstruct(unequal chunks) error = %v", err)
	}
	// Without parity nothing can be rebuilt.
	if err := codec.Reconstruct(erasure.Params{Data: 1}, [][]byte{nil}, true); !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("Reconstruct(1+0, missing) error = %v", err)
	}
	// A missing parity chunk does not matter to a read.
	chunks, err := codec.Encode(good, []byte("some data"))
	if err != nil {
		t.Fatal(err)
	}
	chunks[2], chunks[3] = nil, nil
	if err := codec.Reconstruct(good, chunks, true); err != nil || chunks[2] != nil {
		t.Errorf("Reconstruct(dataOnly, parity missing) = %v, parity rebuilt = %v", err, chunks[2] != nil)
	}
}

// FuzzCodec: for arbitrary data, parameters and losses, decoding returns the
// original or reports that too much is lost; it never returns other bytes.
func FuzzCodec(f *testing.F) {
	f.Add([]byte("hello world"), uint8(4), uint8(2), uint64(0b000101))
	f.Add([]byte{0}, uint8(1), uint8(0), uint64(0))
	f.Add(bytes.Repeat([]byte{7}, 300), uint8(12), uint8(4), uint64(0xF0F0))

	codec := erasurecodec.New()
	f.Fuzz(func(t *testing.T, stripe []byte, data, parity uint8, lossMask uint64) {
		if len(stripe) == 0 {
			return
		}
		params := erasure.Params{Data: int(data)%12 + 1, Parity: int(parity) % 5}
		chunks, err := codec.Encode(params, stripe)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		lost := 0
		for i := range chunks {
			if lossMask&(1<<i) != 0 {
				chunks[i] = nil
				lost++
			}
		}
		err = codec.Reconstruct(params, chunks, false)
		if lost > params.Parity {
			if !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("lost %d of %d+%d: error = %v", lost, params.Data, params.Parity, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("lost %d of %d+%d: %v", lost, params.Data, params.Parity, err)
		}
		if !bytes.Equal(join(params, chunks, len(stripe)), stripe) {
			t.Fatal("decoded data differs from the original")
		}
	})
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	codec := erasurecodec.New()
	done := make(chan error, 8)
	for worker := range 8 {
		go func() {
			params := erasure.Params{Data: 2 + worker%3, Parity: 2}
			for round := range 50 {
				stripe := make([]byte, 1+(worker*977+round*131)%5000)
				if _, err := rand.Read(stripe); err != nil {
					done <- err
					return
				}
				chunks, err := codec.Encode(params, stripe)
				if err != nil {
					done <- err
					return
				}
				chunks[0], chunks[len(chunks)-1] = nil, nil
				if err := codec.Reconstruct(params, chunks, true); err != nil {
					done <- err
					return
				}
				if !bytes.Equal(join(params, chunks, len(stripe)), stripe) {
					done <- errors.New("decoded data differs")
					return
				}
			}
			done <- nil
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
