package cluster_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
)

func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("drive-%02d", i)
	}
	return out
}

func attached(driveIDs ...string) []cluster.DriveState {
	out := make([]cluster.DriveState, len(driveIDs))
	for i, id := range driveIDs {
		out[i] = cluster.DriveState{ID: id}
	}
	return out
}

func TestNewTopology(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 3, 4, 6, 16, 32} {
		topology, err := cluster.NewTopology(ids(n))
		if err != nil {
			t.Fatalf("NewTopology(%d drives): %v", n, err)
		}
		if err := topology.Validate(); err != nil {
			t.Errorf("%d drives: Validate: %v", n, err)
		}
		if !slices.Equal(topology.DriveIDs(), ids(n)) || topology.Empty() {
			t.Errorf("%d drives: DriveIDs = %v", n, topology.DriveIDs())
		}
	}
	two, _ := cluster.NewTopology(ids(32))
	if len(two.Sets) != 2 || two.SetSize() != 16 {
		t.Errorf("32 drives: %d sets of %d", len(two.Sets), two.SetSize())
	}
	if pos, ok := two.Locate("drive-17"); !ok || pos != (cluster.Position{Set: 1, Slot: 1}) {
		t.Errorf("Locate(drive-17) = %+v, %v", pos, ok)
	}

	for name, bad := range map[string][]string{
		"none": nil, "uneven": ids(17), "duplicate": {"a", "b", "a", "c"}, "empty ID": {"a", "", "c", "d"},
	} {
		if _, err := cluster.NewTopology(bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: error = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	var none cluster.Topology
	if !none.Empty() || none.SetSize() != 0 || none.Validate() == nil {
		t.Error("the zero topology is not reported as empty and invalid")
	}
}

func TestValidateRejectsMalformedTopologies(t *testing.T) {
	t.Parallel()

	for name, topology := range map[string]cluster.Topology{
		"unequal sets":      {Sets: [][]cluster.Slot{{{DriveID: "a"}, {DriveID: "b"}}, {{DriveID: "c"}}}},
		"drive twice":       {Sets: [][]cluster.Slot{{{DriveID: "a"}, {DriveID: "a"}}}},
		"retired is active": {Sets: [][]cluster.Slot{{{DriveID: "a", Retired: []string{"b"}}, {DriveID: "b"}}}},
		"empty drive ID":    {Sets: [][]cluster.Slot{{{DriveID: ""}}}},
		"empty set":         {Sets: [][]cluster.Slot{{}}},
	} {
		if err := topology.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Validate = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
}

func TestReconcile(t *testing.T) {
	t.Parallel()
	topology, err := cluster.NewTopology(ids(6))
	if err != nil {
		t.Fatal(err)
	}
	fresh := cluster.DriveState{ID: "new-drive", Fresh: true}
	without := func(skip ...string) []cluster.DriveState {
		var out []cluster.DriveState
		for _, id := range ids(6) {
			if !slices.Contains(skip, id) {
				out = append(out, cluster.DriveState{ID: id})
			}
		}
		return out
	}

	tests := []struct {
		name     string
		attached []cluster.DriveState
		want     *cluster.Replacement
		wantErr  bool
	}{
		{name: "all present", attached: attached(ids(6)...)},
		{name: "order does not matter", attached: attached("drive-05", "drive-00", "drive-03", "drive-01", "drive-04", "drive-02")},
		{
			name: "one empty drive replaces one missing drive", attached: append(without("drive-02"), fresh),
			want: &cluster.Replacement{Position: cluster.Position{Set: 0, Slot: 2}, Old: "drive-02", New: "new-drive"},
		},
		{name: "a drive is missing", attached: without("drive-02"), wantErr: true},
		{name: "an extra drive", attached: append(attached(ids(6)...), fresh), wantErr: true},
		{name: "the unknown drive already holds data", attached: append(without("drive-02"), cluster.DriveState{ID: "foreign"}), wantErr: true},
		{name: "two drives replaced at once", attached: append(without("drive-02", "drive-03"), fresh, cluster.DriveState{ID: "new-2", Fresh: true}), wantErr: true},
		{name: "the same drive twice", attached: append(attached(ids(6)...), cluster.DriveState{ID: "drive-00"}), wantErr: true},
		{name: "nothing attached", wantErr: true},
	}
	for _, tt := range tests {
		got, err := topology.Reconcile(tt.attached)
		if tt.wantErr {
			if !errors.Is(err, domain.ErrInvalidArgument) || got != nil {
				t.Errorf("%s: Reconcile = %+v, %v; want %v", tt.name, got, err, domain.ErrInvalidArgument)
			}
			continue
		}
		if err != nil || (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("%s: Reconcile = %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}
}

func TestReplace(t *testing.T) {
	t.Parallel()
	original, err := cluster.NewTopology(ids(6))
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := original.Reconcile(append(attached("drive-00", "drive-01", "drive-03", "drive-04", "drive-05"), cluster.DriveState{ID: "new-a", Fresh: true}))
	if err != nil || replacement == nil {
		t.Fatalf("Reconcile: %+v, %v", replacement, err)
	}

	replaced := original.Replace(*replacement)
	if err := replaced.Validate(); err != nil {
		t.Fatalf("Validate after Replace: %v", err)
	}
	// The receiver is untouched.
	if current, _ := original.Current("drive-02"); current != "drive-02" {
		t.Errorf("Replace modified its receiver: drive-02 is now %s", current)
	}
	// Data recorded under the old drive is found in the same slot, on the
	// new drive.
	if current, ok := replaced.Current("drive-02"); !ok || current != "new-a" {
		t.Errorf("Current(drive-02) = %q, %v; want new-a", current, ok)
	}
	oldPos, _ := original.Locate("drive-02")
	if newPos, ok := replaced.Locate("new-a"); !ok || newPos != oldPos {
		t.Errorf("the new drive is in slot %+v, want %+v", newPos, oldPos)
	}
	if _, ok := replaced.Current("never-seen"); ok {
		t.Error("an unknown drive ID was located")
	}

	// The retired drive must not come back, and the slot can be replaced
	// again.
	back := append(attached("drive-00", "drive-01", "drive-03", "drive-04", "drive-05"), cluster.DriveState{ID: "drive-02"})
	if _, err := replaced.Reconcile(back); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("re-attaching a replaced drive: error = %v, want %v", err, domain.ErrInvalidArgument)
	}
	second, err := replaced.Reconcile(append(attached("drive-00", "drive-01", "drive-03", "drive-04", "drive-05"), cluster.DriveState{ID: "new-b", Fresh: true}))
	if err != nil || second == nil || second.Old != "new-a" {
		t.Fatalf("second replacement: %+v, %v", second, err)
	}
	twice := replaced.Replace(*second)
	for _, old := range []string{"drive-02", "new-a"} {
		if current, _ := twice.Current(old); current != "new-b" {
			t.Errorf("Current(%s) = %q after two replacements, want new-b", old, current)
		}
	}
	if err := twice.Validate(); err != nil {
		t.Errorf("Validate after two replacements: %v", err)
	}
}
