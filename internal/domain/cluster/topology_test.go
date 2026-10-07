package cluster_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
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

func TestCheckNewDeployment(t *testing.T) {
	t.Parallel()

	empty := []cluster.DriveState{{ID: "a", Empty: true}, {ID: "b", Empty: true}}
	if err := cluster.CheckNewDeployment(empty); err != nil {
		t.Errorf("empty, unclaimed drives: %v", err)
	}
	// Drives with data and no record of a deployment: the metadata
	// directory is the wrong one. Adopting them would let the collector
	// remove everything on them.
	for name, drives := range map[string][]cluster.DriveState{
		"a drive holds data":                {{ID: "a", Empty: true}, {ID: "b"}},
		"a drive is claimed":                {{ID: "a", Empty: true}, {ID: "b", Empty: true, DeploymentID: "other"}},
		"a drive holds data and is claimed": {{ID: "a", DeploymentID: "other"}},
	} {
		if err := cluster.CheckNewDeployment(drives); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: error = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
}

func TestReconcile(t *testing.T) {
	t.Parallel()
	topology, err := cluster.NewTopology(ids(6))
	if err != nil {
		t.Fatal(err)
	}
	topology.DeploymentID = "this-deployment"
	empty := cluster.DriveState{ID: "new-drive", Empty: true}
	without := func(skip ...string) []cluster.DriveState {
		var out []cluster.DriveState
		for _, id := range ids(6) {
			if !slices.Contains(skip, id) {
				out = append(out, cluster.DriveState{ID: id, DeploymentID: "this-deployment"})
			}
		}
		return out
	}
	replaces := &cluster.Replacement{Position: cluster.Position{Set: 0, Slot: 2}, Old: "drive-02", New: "new-drive"}

	tests := []struct {
		name     string
		attached []cluster.DriveState
		allow    bool
		want     *cluster.Replacement
		wantErr  string
	}{
		{name: "all present", attached: without()},
		{name: "order does not matter", attached: attached("drive-05", "drive-00", "drive-03", "drive-01", "drive-04", "drive-02")},
		{name: "drives not claimed yet", attached: attached(ids(6)...)},
		{name: "a confirmed replacement", attached: append(without("drive-02"), empty), allow: true, want: replaces},
		{
			name:     "an empty drive claimed for this deployment, as after an interrupted start",
			attached: append(without("drive-02"), cluster.DriveState{ID: "new-drive", Empty: true, DeploymentID: "this-deployment"}),
			allow:    true, want: replaces,
		},
		// A mount that is missing looks exactly like this.
		{name: "a replacement that was not confirmed", attached: append(without("drive-02"), empty), wantErr: "confirm the replacement"},
		{name: "a drive is missing", attached: without("drive-02"), allow: true, wantErr: "do not match the deployment"},
		{name: "an extra drive", attached: append(without(), empty), allow: true, wantErr: "do not match the deployment"},
		{name: "the unknown drive holds data", attached: append(without("drive-02"), cluster.DriveState{ID: "foreign"}), allow: true, wantErr: "holds data"},
		{
			name:     "a drive of another deployment",
			attached: append(without("drive-02"), cluster.DriveState{ID: "foreign", Empty: true, DeploymentID: "other"}),
			allow:    true, wantErr: "belongs to deployment other",
		},
		{
			name:     "a known drive ID claimed for another deployment",
			attached: append(without("drive-02"), cluster.DriveState{ID: "drive-02", DeploymentID: "other"}),
			wantErr:  "belongs to deployment other",
		},
		{
			name:     "two drives replaced at once",
			attached: append(without("drive-02", "drive-03"), empty, cluster.DriveState{ID: "new-2", Empty: true}),
			allow:    true, wantErr: "at a time",
		},
		{name: "the same drive twice", attached: append(without(), cluster.DriveState{ID: "drive-00"}), wantErr: "attached twice"},
		{name: "nothing attached", allow: true, wantErr: "do not match the deployment"},
	}
	for _, tt := range tests {
		got, err := topology.Reconcile(tt.attached, tt.allow)
		if tt.wantErr != "" {
			if !errors.Is(err, domain.ErrInvalidArgument) || got != nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: Reconcile = %+v, %v; want an error containing %q", tt.name, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil || (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("%s: Reconcile = %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}
}

// TestNoReplacementWithoutParity: with fewer than four drives nothing can be
// rebuilt. An empty directory where a drive should be - a mount that is
// missing - must never take the drive's place, confirmed or not: the drive
// would be retired for good with the only copy of its objects on it.
func TestNoReplacementWithoutParity(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 2, 3} {
		topology, err := cluster.NewTopology(ids(n))
		if err != nil {
			t.Fatal(err)
		}
		present := attached(ids(n)[1:]...)
		present = append(present, cluster.DriveState{ID: "empty-mount-point", Empty: true})
		for _, allow := range []bool{false, true} {
			got, err := topology.Reconcile(present, allow)
			if !errors.Is(err, domain.ErrInvalidArgument) || got != nil || !strings.Contains(err.Error(), "no parity") {
				t.Errorf("%d drives, confirmed=%v: Reconcile = %+v, %v; want a refusal naming the missing parity", n, allow, got, err)
			}
		}
	}
}

func TestReplace(t *testing.T) {
	t.Parallel()
	original, err := cluster.NewTopology(ids(6))
	if err != nil {
		t.Fatal(err)
	}
	original.DeploymentID = "this-deployment"
	newDrive := func(id string) cluster.DriveState { return cluster.DriveState{ID: id, Empty: true} }
	rest := func() []cluster.DriveState {
		return attached("drive-00", "drive-01", "drive-03", "drive-04", "drive-05")
	}
	replacement, err := original.Reconcile(append(rest(), newDrive("new-a")), true)
	if err != nil || replacement == nil {
		t.Fatalf("Reconcile: %+v, %v", replacement, err)
	}

	replaced := original.Replace(*replacement)
	if err := replaced.Validate(); err != nil {
		t.Fatalf("Validate after Replace: %v", err)
	}
	if replaced.DeploymentID != "this-deployment" {
		t.Errorf("Replace lost the deployment ID: %q", replaced.DeploymentID)
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
	if _, err := replaced.Reconcile(append(rest(), cluster.DriveState{ID: "drive-02"}), true); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("re-attaching a replaced drive: error = %v, want %v", err, domain.ErrInvalidArgument)
	}
	second, err := replaced.Reconcile(append(rest(), newDrive("new-b")), true)
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
