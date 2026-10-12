package iam_test

import (
	"errors"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// TestNodesAndClusterActionsBelongToEachOther: a node of the cluster may
// speak node RPC and nothing else; nobody else may speak node RPC, root
// included; a node adds and removes itself only.
func TestNodesAndClusterActionsBelongToEachOther(t *testing.T) {
	t.Parallel()

	node := iam.Principal{NodeID: "n1"}
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	user := iam.Principal{AccessKeyID: "SOMEONE"}
	tests := []struct {
		name      string
		principal iam.Principal
		action    iam.Action
		resource  iam.Resource
		allowed   bool
	}{
		{"node speaks node RPC", node, iam.ActionNodeRPC, iam.Resource{}, true},
		{"node joins as itself", node, iam.ActionJoinNode, iam.Resource{Node: "n1"}, true},
		{"node removes itself", node, iam.ActionRemoveNode, iam.Resource{Node: "n1"}, true},
		{"node joins as another", node, iam.ActionJoinNode, iam.Resource{Node: "n2"}, false},
		{"node removes another", node, iam.ActionRemoveNode, iam.Resource{Node: "n2"}, false},
		{"node joins as nobody", node, iam.ActionJoinNode, iam.Resource{}, false},
		{"node reports what it counted", node, iam.ActionReportReads, iam.Resource{Node: "n1"}, true},
		{"node reports what another counted", node, iam.ActionReportReads, iam.Resource{Node: "n2"}, false},
		{"node reports what nobody counted", node, iam.ActionReportReads, iam.Resource{}, false},
		{"root reports what a node counted", root, iam.ActionReportReads, iam.Resource{Node: "n1"}, false},
		{"node reads an object", node, iam.ActionGetObject, iam.Resource{Bucket: "b", Key: "k"}, false},
		{"node writes an object", node, iam.ActionPutObject, iam.Resource{Bucket: "b", Key: "k"}, false},
		{"node reads metrics", node, iam.ActionGetMetrics, iam.Resource{}, false},
		{"node reads the settings", node, iam.ActionGetSettings, iam.Resource{Admin: "settings"}, false},
		{"node defines a target", node, iam.ActionPutTarget, iam.Resource{Admin: "target/hook"}, false},
		{"node reads a target", node, iam.ActionGetTarget, iam.Resource{Admin: "target/hook"}, false},
		{"node sets a quota", node, iam.ActionPutBucketQuota, iam.Resource{Bucket: "b"}, false},
		{"node makes a bucket a replica", node, iam.ActionPutBucketReplicaMode, iam.Resource{Bucket: "b"}, false},
		{"node sets notification rules", node, iam.ActionPutBucketNotification, iam.Resource{Bucket: "b"}, false},
		{"node sets replication", node, iam.ActionPutReplication, iam.Resource{Bucket: "b"}, false},
		{"root speaks node RPC", root, iam.ActionNodeRPC, iam.Resource{}, false},
		{"root joins a node", root, iam.ActionJoinNode, iam.Resource{Node: "n1"}, false},
		{"user speaks node RPC", user, iam.ActionNodeRPC, iam.Resource{}, false},
		{"anonymous speaks node RPC", iam.Principal{}, iam.ActionNodeRPC, iam.Resource{}, false},
		{"a principal that claims to be both", iam.Principal{NodeID: "n1", AccessKeyID: "ROOT", Root: true}, iam.ActionNodeRPC, iam.Resource{}, false},
		{"both, on an object", iam.Principal{NodeID: "n1", AccessKeyID: "ROOT", Root: true}, iam.ActionGetObject, iam.Resource{Bucket: "b"}, false},
		{"root reads an object", root, iam.ActionGetObject, iam.Resource{Bucket: "b", Key: "k"}, true},
	}
	for _, tt := range tests {
		err := iam.Decide(iam.Request{Principal: tt.principal, Action: tt.action, Resource: tt.resource})
		if tt.allowed && err != nil {
			t.Errorf("%s: refused: %v", tt.name, err)
		}
		if !tt.allowed && !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s: error = %v, want %v", tt.name, err, domain.ErrAccessDenied)
		}
	}

	if node.Anonymous() || node.Name() != "node/n1" || user.Name() != "SOMEONE" {
		t.Errorf("node principal: anonymous %v, name %q", node.Anonymous(), node.Name())
	}
	if got := (iam.Resource{Node: "n1"}).String(); got != "arn:cactusfs:cluster:::node/n1" {
		t.Errorf("node resource = %q", got)
	}
}
