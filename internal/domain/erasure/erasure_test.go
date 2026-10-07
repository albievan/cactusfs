package erasure_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"slices"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/erasure"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestParamsFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		drives      int
		data        int
		parity      int
		writeQuorum int
	}{
		{1, 1, 0, 1},
		{2, 1, 0, 1},
		{3, 1, 0, 1},
		{4, 2, 2, 3}, // data equals parity: one more than data
		{5, 3, 2, 3},
		{6, 3, 3, 4}, // data equals parity
		{7, 4, 3, 4},
		{8, 4, 4, 5}, // data equals parity
		{9, 5, 4, 5},
		{12, 8, 4, 8},
		{16, 12, 4, 12},
	}
	for _, tt := range tests {
		p, err := erasure.ParamsFor(tt.drives)
		if err != nil {
			t.Fatalf("ParamsFor(%d): %v", tt.drives, err)
		}
		if p.Data != tt.data || p.Parity != tt.parity || p.Total() > tt.drives {
			t.Errorf("ParamsFor(%d) = %d+%d, want %d+%d", tt.drives, p.Data, p.Parity, tt.data, tt.parity)
		}
		if tt.drives >= erasure.MinSetDrives && p.Total() != tt.drives {
			t.Errorf("ParamsFor(%d) uses %d drives, want all of them", tt.drives, p.Total())
		}
		if p.WriteQuorum() != tt.writeQuorum || p.ReadQuorum() != tt.data {
			t.Errorf("ParamsFor(%d): write quorum %d, read quorum %d; want %d, %d", tt.drives, p.WriteQuorum(), p.ReadQuorum(), tt.writeQuorum, tt.data)
		}
		// A write quorum and any later read quorum always share a shard,
		// and two write quorums always overlap.
		if p.WriteQuorum() > p.Total() || 2*p.WriteQuorum() <= p.Total() && p.Parity > 0 {
			t.Errorf("ParamsFor(%d): write quorum %d of %d does not guarantee overlap", tt.drives, p.WriteQuorum(), p.Total())
		}
		if err := p.Validate(); err != nil {
			t.Errorf("ParamsFor(%d) is not valid: %v", tt.drives, err)
		}
	}
	for _, drives := range []int{-1, 0, 17, 100} {
		if _, err := erasure.ParamsFor(drives); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ParamsFor(%d) error = %v, want %v", drives, err, domain.ErrInvalidArgument)
		}
	}
	for _, p := range []erasure.Params{{0, 2}, {-1, 0}, {2, -1}, {200, 100}} {
		if err := p.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%+v.Validate() = %v, want %v", p, err, domain.ErrInvalidArgument)
		}
	}
}

func TestSetLayout(t *testing.T) {
	t.Parallel()

	tests := []struct{ drives, sets, size int }{
		{1, 1, 1}, {3, 1, 3}, {4, 1, 4}, {16, 1, 16}, {18, 2, 9}, {32, 2, 16}, {33, 3, 11}, {48, 3, 16}, {64, 4, 16},
	}
	for _, tt := range tests {
		sets, size, err := erasure.SetLayout(tt.drives)
		if err != nil || sets != tt.sets || size != tt.size {
			t.Errorf("SetLayout(%d) = %d sets of %d, %v; want %d sets of %d", tt.drives, sets, size, err, tt.sets, tt.size)
		}
	}
	for _, drives := range []int{-4, 0, 17, 19, 34, 49} {
		if _, _, err := erasure.SetLayout(drives); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("SetLayout(%d) error = %v, want %v", drives, err, domain.ErrInvalidArgument)
		}
	}
	// Every layout that is accepted has equal sets within the limits.
	for drives := 1; drives <= 400; drives++ {
		sets, size, err := erasure.SetLayout(drives)
		if err != nil {
			continue
		}
		if sets*size != drives || size > erasure.MaxSetDrives || (drives > erasure.MaxSetDrives && size < erasure.MinSetDrives) {
			t.Errorf("SetLayout(%d) = %d sets of %d", drives, sets, size)
		}
	}
}

func TestPlacement(t *testing.T) {
	t.Parallel()

	usage := map[[2]int]int{}
	for range 4000 {
		var id object.ID
		if _, err := rand.Read(id[:]); err != nil {
			t.Fatal(err)
		}
		set, positions, err := erasure.Placement(id, 3, 8, 8)
		if err != nil {
			t.Fatalf("Placement: %v", err)
		}
		if set < 0 || set >= 3 || len(positions) != 8 {
			t.Fatalf("Placement = set %d, %d positions", set, len(positions))
		}
		sorted := slices.Clone(positions)
		slices.Sort(sorted)
		if !slices.Equal(sorted, []int{0, 1, 2, 3, 4, 5, 6, 7}) {
			t.Fatalf("shards share a drive or miss one: %v", positions)
		}
		usage[[2]int{set, positions[0]}]++

		again, positionsAgain, _ := erasure.Placement(id, 3, 8, 8)
		if again != set || !slices.Equal(positions, positionsAgain) {
			t.Fatal("Placement is not deterministic")
		}
	}
	// Shard 0 lands on every drive of every set: load is spread.
	if len(usage) != 24 {
		t.Errorf("shard 0 used %d of 24 drives", len(usage))
	}

	// One shard on a small set: every drive is used, one at a time.
	seen := map[int]bool{}
	for i := range 64 {
		_, positions, err := erasure.Placement(object.ID{8: byte(i), 15: byte(i)}, 1, 3, 1)
		if err != nil || len(positions) != 1 {
			t.Fatalf("Placement: %v, %v", positions, err)
		}
		seen[positions[0]] = true
	}
	if len(seen) != 3 {
		t.Errorf("single shards used %d of 3 drives", len(seen))
	}

	for _, bad := range [][3]int{{0, 4, 4}, {1, 0, 1}, {1, 4, 0}, {1, 4, 5}} {
		if _, _, err := erasure.Placement(object.ID{}, bad[0], bad[1], bad[2]); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Placement(%v) error = %v, want %v", bad, err, domain.ErrInvalidArgument)
		}
	}
}

func TestGeometry(t *testing.T) {
	t.Parallel()

	g := erasure.Geometry{Params: erasure.Params{Data: 4, Parity: 2}, StripeSize: 100}
	tests := []struct {
		stream    int64
		stripes   int64
		shardSize int64
	}{
		{0, 0, 0}, {1, 1, 1}, {4, 1, 1}, {5, 1, 2}, {100, 1, 25}, {101, 2, 26}, {200, 2, 50}, {250, 3, 63}, {399, 4, 100},
	}
	for _, tt := range tests {
		if got := g.Stripes(tt.stream); got != tt.stripes {
			t.Errorf("Stripes(%d) = %d, want %d", tt.stream, got, tt.stripes)
		}
		if got := g.ShardSize(tt.stream); got != tt.shardSize {
			t.Errorf("ShardSize(%d) = %d, want %d", tt.stream, got, tt.shardSize)
		}
	}
	if g.Chunk(100) != 25 || g.Chunk(99) != 25 || g.Chunk(1) != 1 || g.ShardOffset(3) != 75 || g.StripeLen(250, 2) != 50 || g.StripeLen(250, 0) != 100 {
		t.Error("chunk, offset or stripe length is wrong")
	}

	if err := g.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	for _, bad := range []erasure.Geometry{
		{Params: erasure.Params{Data: 0, Parity: 1}, StripeSize: 100},
		{Params: erasure.Params{Data: 1}, StripeSize: 0},
		{Params: erasure.Params{Data: 1}, StripeSize: erasure.MaxStripeSize + 1},
	} {
		if err := bad.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%+v.Validate() = %v, want %v", bad, err, domain.ErrInvalidArgument)
		}
	}
}

// FuzzGeometry checks the property the storage layer relies on: cutting a
// stream into stripes and chunks and concatenating the chunks per shard gives
// shards of exactly ShardSize bytes, with each stripe's chunk at ShardOffset.
func FuzzGeometry(f *testing.F) {
	f.Add(uint16(250), uint8(4), uint8(100))
	f.Add(uint16(0), uint8(1), uint8(1))
	f.Add(uint16(65535), uint8(12), uint8(7))
	f.Fuzz(func(t *testing.T, size uint16, data, stripe uint8) {
		g := erasure.Geometry{Params: erasure.Params{Data: int(data)%16 + 1}, StripeSize: int(stripe) + 1}
		stream := bytes.Repeat([]byte{1}, int(size))

		var shard int64
		for n := int64(0); n < g.Stripes(int64(size)); n++ {
			if g.ShardOffset(n) != shard {
				t.Fatalf("stripe %d: ShardOffset = %d, but %d bytes precede it", n, g.ShardOffset(n), shard)
			}
			length := g.StripeLen(int64(size), n)
			if length < 1 || length > g.StripeSize {
				t.Fatalf("stripe %d holds %d bytes", n, length)
			}
			if g.Chunk(length)*g.Data < length {
				t.Fatalf("stripe %d: %d chunks of %d cannot hold %d bytes", n, g.Data, g.Chunk(length), length)
			}
			shard += int64(g.Chunk(length))
			stream = stream[length:]
		}
		if len(stream) != 0 {
			t.Fatalf("%d stream bytes are in no stripe", len(stream))
		}
		if shard != g.ShardSize(int64(size)) {
			t.Fatalf("shards hold %d bytes, ShardSize says %d", shard, g.ShardSize(int64(size)))
		}
	})
}
