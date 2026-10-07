package cluster_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/erasure"
)

func TestParseRoles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: "gateway,meta,storage,console"},
		{in: "  ", want: "gateway,meta,storage,console"},
		{in: "storage", want: "storage"},
		{in: "meta, gateway", want: "meta,gateway"},
		{in: "storage,storage,meta", want: "storage,meta"},
		{in: "storage,janitor", wantErr: true},
		{in: "Storage", wantErr: true},
		{in: "storage,", wantErr: true},
	}
	for _, tt := range tests {
		roles, err := cluster.ParseRoles(tt.in)
		if tt.wantErr {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("ParseRoles(%q) = %v, %v; want %v", tt.in, roles, err, domain.ErrInvalidArgument)
			}
			continue
		}
		if err != nil || roles.String() != tt.want {
			t.Errorf("ParseRoles(%q) = %q, %v; want %q", tt.in, roles, err, tt.want)
		}
	}
	all := cluster.AllRoles()
	for _, role := range []cluster.Role{cluster.RoleGateway, cluster.RoleMeta, cluster.RoleStorage, cluster.RoleConsole} {
		if !all.Has(role) {
			t.Errorf("AllRoles lacks %s", role)
		}
	}
	if (cluster.Roles{cluster.RoleMeta}).Has(cluster.RoleStorage) {
		t.Error("Has reports a role that is not in the set")
	}
}

func TestNodeValidation(t *testing.T) {
	t.Parallel()
	valid := []string{"a", "n1", "node-1", "storage-eu-west-1a-07", strings.Repeat("a", 63), "0"}
	for _, id := range valid {
		if err := cluster.ValidateNodeID(id); err != nil {
			t.Errorf("ValidateNodeID(%q) = %v", id, err)
		}
	}
	invalid := []string{"", "Node", "node_1", "-node", "node-", "node.1", "a b", "nöde", strings.Repeat("a", 64), "a@b", "a/b", "../x"}
	for _, id := range invalid {
		if err := cluster.ValidateNodeID(id); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ValidateNodeID(%q) = %v, want %v", id, err, domain.ErrInvalidArgument)
		}
	}

	node := cluster.Node{ID: "n1", Roles: cluster.Roles{cluster.RoleMeta}}
	if err := node.Validate(); err != nil {
		t.Errorf("Validate = %v", err)
	}
	for name, bad := range map[string]cluster.Node{
		"bad ID":       {ID: "N1", Roles: cluster.AllRoles()},
		"no role":      {ID: "n1"},
		"unknown role": {ID: "n1", Roles: cluster.Roles{"janitor"}},
	} {
		if err := bad.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Validate = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
}

// drivesOf returns per drives on each of the nodes.
func drivesOf(per int, nodes ...string) []cluster.DriveState {
	var drives []cluster.DriveState
	for _, node := range nodes {
		for i := range per {
			drives = append(drives, cluster.DriveState{ID: fmt.Sprintf("%s-d%d", node, i), NodeID: node, Empty: true})
		}
	}
	return drives
}

// worst returns, over all erasure sets, the most slots that share a value of
// key.
func worst(t cluster.Topology, key func(cluster.Slot) string) int {
	most := 0
	for _, set := range t.Sets {
		count := map[string]int{}
		for _, slot := range set {
			count[key(slot)]++
			most = max(most, count[key(slot)])
		}
	}
	return most
}

// TestPlanTopologySpreadsEverySetAcrossFailureDomains: an object's shards go
// to the drives of one erasure set, so how a set is spread is how many
// failures an object survives. Whatever the shape of the cluster, no set may
// hold more drives of one node, rack or zone than an even deal gives it.
func TestPlanTopologySpreadsEverySetAcrossFailureDomains(t *testing.T) {
	t.Parallel()

	for nodes := 1; nodes <= 24; nodes++ {
		for per := 1; per <= 12; per++ {
			for zones := 1; zones <= 4; zones++ {
				if zones > nodes {
					continue
				}
				var ids []string
				labels := map[string]cluster.Labels{}
				for n := range nodes {
					id := fmt.Sprintf("n%02d", n)
					ids = append(ids, id)
					labels[id] = cluster.Labels{Region: "r", Zone: fmt.Sprintf("z%d", n%zones), Rack: fmt.Sprintf("k%d", n%(2*zones))}
				}
				drives := drivesOf(per, ids...)
				topology, err := cluster.PlanTopology(drives, labels)
				if _, _, layoutErr := erasure.SetLayout(len(drives)); layoutErr != nil {
					if !errors.Is(err, domain.ErrInvalidArgument) {
						t.Fatalf("%d nodes x %d drives: PlanTopology = %v, want the layout's refusal", nodes, per, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%d nodes x %d drives: %v", nodes, per, err)
				}
				if err := topology.Validate(); err != nil {
					t.Fatalf("%d nodes x %d drives: invalid topology: %v", nodes, per, err)
				}
				if got := topology.DriveIDs(); len(got) != len(drives) {
					t.Fatalf("%d nodes x %d drives: %d drives placed", nodes, per, len(got))
				}
				size := topology.SetSize()
				ceil := func(a, b int) int { return (a + b - 1) / b }
				// The even share of a set, rounded up, plus what cannot
				// be avoided when the deal wraps round.
				if got, limit := worst(topology, func(s cluster.Slot) string { return s.NodeID }), ceil(size, nodes); got > limit {
					t.Errorf("%d nodes x %d drives, %d zones: a set of %d has %d drives of one node, want at most %d",
						nodes, per, zones, size, got, limit)
				}
				// A zone gets its share of every set: in proportion to the
				// drives it has, rounded up.
				inZone := map[string]int{}
				for _, id := range ids {
					inZone[labels[id].Zone] += per
				}
				for _, set := range topology.Sets {
					count := map[string]int{}
					for _, slot := range set {
						count[labels[slot.NodeID].Zone]++
					}
					for zone, got := range count {
						if limit := ceil(size*inZone[zone], len(drives)); got > limit {
							t.Errorf("%d nodes x %d drives, %d zones: a set of %d has %d drives in zone %s, which has %d of %d drives; want at most %d",
								nodes, per, zones, size, got, zone, inZone[zone], len(drives), limit)
						}
					}
				}
				params, _ := erasure.ParamsFor(size)
				if want := params.Parity / worst(topology, func(s cluster.Slot) string { return s.NodeID }); topology.NodeFailuresTolerated() != want {
					t.Errorf("%d nodes x %d drives: NodeFailuresTolerated = %d, want %d", nodes, per, topology.NodeFailuresTolerated(), want)
				}
				if !slices.Equal(topology.NodeIDs(), ids) {
					t.Errorf("NodeIDs = %v", topology.NodeIDs())
				}
			}
		}
	}
}

func TestPlanTopologyExamples(t *testing.T) {
	t.Parallel()

	// Four nodes with two drives each, one node per zone: one set of eight,
	// two shards per node, so any two nodes can be lost.
	labels := map[string]cluster.Labels{"a": {Zone: "1"}, "b": {Zone: "2"}, "c": {Zone: "1"}, "d": {Zone: "2"}}
	topology, err := cluster.PlanTopology(drivesOf(2, "a", "b", "c", "d"), labels)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, slot := range topology.Sets[0] {
		order = append(order, slot.NodeID)
	}
	// Zones alternate, and so do the nodes within each.
	if got := strings.Join(order, ""); got != "abcdabcd" {
		t.Errorf("deal order = %s, want abcdabcd", got)
	}
	if topology.NodeFailuresTolerated() != 2 {
		t.Errorf("NodeFailuresTolerated = %d, want 2", topology.NodeFailuresTolerated())
	}

	// The same input gives the same plan.
	again, _ := cluster.PlanTopology(drivesOf(2, "a", "b", "c", "d"), labels)
	if fmt.Sprint(again) != fmt.Sprint(topology) {
		t.Error("PlanTopology is not deterministic")
	}

	// One node: no node can be lost, and with fewer than four drives there
	// is no parity at all.
	single, err := cluster.PlanTopology(drivesOf(6, "only"), nil)
	if err != nil || single.NodeFailuresTolerated() != 0 {
		t.Errorf("one node: tolerates %d node failures, %v", single.NodeFailuresTolerated(), err)
	}
	small, err := cluster.PlanTopology(drivesOf(1, "a", "b", "c"), nil)
	if err != nil || small.NodeFailuresTolerated() != 0 {
		t.Errorf("three drives: tolerates %d node failures, %v", small.NodeFailuresTolerated(), err)
	}
	if (cluster.Topology{}).NodeFailuresTolerated() != 0 {
		t.Error("an empty topology tolerates failures")
	}

	for name, drives := range map[string][]cluster.DriveState{
		"none":        nil,
		"duplicate":   {{ID: "x", NodeID: "a"}, {ID: "x", NodeID: "b"}},
		"empty ID":    {{ID: "", NodeID: "a"}},
		"seventeen":   drivesOf(17, "a"),
		"uneven sets": drivesOf(1, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s"),
	} {
		if _, err := cluster.PlanTopology(drives, nil); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: PlanTopology = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
}

// TestANodeAnswersForItsOwnDrivesOnly: at start a node compares its drives
// with its slots of the topology. Another node's drives are not there, and
// that is not this node's concern - a node that is down must not keep the
// rest from starting. A drive that belongs to another node is refused.
func TestANodeAnswersForItsOwnDrivesOnly(t *testing.T) {
	t.Parallel()

	topology, err := cluster.PlanTopology(drivesOf(2, "a", "b", "c", "d"), nil)
	if err != nil {
		t.Fatal(err)
	}
	topology.DeploymentID = "dep"
	own := func(node string, change func([]cluster.DriveState) []cluster.DriveState) []cluster.DriveState {
		var drives []cluster.DriveState
		for _, d := range drivesOf(2, node) {
			d.Empty, d.DeploymentID = false, "dep"
			drives = append(drives, d)
		}
		if change != nil {
			drives = change(drives)
		}
		return drives
	}

	for _, node := range []string{"a", "b", "c", "d"} {
		if replacement, err := topology.Reconcile(node, own(node, nil), false); err != nil || replacement != nil {
			t.Errorf("node %s with its own drives: %v, %v", node, replacement, err)
		}
	}
	// A node without drives - a gateway - has nothing to reconcile.
	if replacement, err := topology.Reconcile("gateway-1", nil, false); err != nil || replacement != nil {
		t.Errorf("a node without drives: %v, %v", replacement, err)
	}
	// Node a has a drive of node b.
	moved := own("a", func(d []cluster.DriveState) []cluster.DriveState { return append(d, own("b", nil)[0]) })
	if _, err := topology.Reconcile("a", moved, true); !errors.Is(err, domain.ErrInvalidArgument) || !strings.Contains(err.Error(), "belongs to node b") {
		t.Errorf("a drive of another node: %v", err)
	}
	// One of its own is missing: only that node minds.
	one := own("c", func(d []cluster.DriveState) []cluster.DriveState { return d[:1] })
	if _, err := topology.Reconcile("c", one, true); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a missing drive: %v", err)
	}
	// It is replaced by an empty one, confirmed: the slot stays node c's.
	fresh := own("c", func(d []cluster.DriveState) []cluster.DriveState {
		return append(d[:1], cluster.DriveState{ID: "new", NodeID: "c", Empty: true})
	})
	replacement, err := topology.Reconcile("c", fresh, true)
	if err != nil || replacement == nil || replacement.Old != "c-d1" || replacement.New != "new" {
		t.Fatalf("confirmed replacement on node c: %+v, %v", replacement, err)
	}
	replaced := topology.Replace(*replacement)
	position, _ := replaced.Locate("new")
	if slot := replaced.Sets[position.Set][position.Slot]; slot.NodeID != "c" || slot.DriveID != "new" {
		t.Errorf("the slot after the replacement = %+v", slot)
	}
	if clone := replaced.Clone(); clone.Sets[position.Set][position.Slot].NodeID != "c" {
		t.Error("Clone lost the node of a slot")
	}
}
