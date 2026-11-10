package release

import (
	"slices"
	"time"
)

// Mode is how a node installs a release (the setting upgrade-mode). Its
// values are part of the management API's contract: never change one, only
// add.
type Mode string

// The modes.
const (
	// ModeOff installs nothing: the console shows what to do instead.
	ModeOff Mode = "off"
	// ModeBinary replaces the running binary of a cluster of one.
	ModeBinary Mode = "binary"
	// ModeKubernetes moves the StatefulSets of the deployment to the
	// release's image, one node at a time.
	ModeKubernetes Mode = "kubernetes"
)

// State is where an upgrade stands. Its values are part of the management
// API's contract: never change one, only add.
type State string

// The states of an upgrade.
const (
	// StateIdle: no upgrade has been started since the node started.
	StateIdle State = "idle"
	// StateVerifying: the artifact is being fetched and verified.
	StateVerifying State = "verifying"
	// StateRunning: nodes are being moved to the target.
	StateRunning State = "running"
	// StateRestartRequired: the binary was replaced, and the node must be
	// restarted by whoever runs it (Windows).
	StateRestartRequired State = "restart-required"
	// StateFailed: the upgrade stopped; Progress.Nodes say where and why.
	StateFailed State = "failed"
	// StateDone: every node runs the target.
	StateDone State = "done"
)

// Active reports whether an upgrade in this state is still under way, so
// that no other may be started.
func (s State) Active() bool { return s == StateVerifying || s == StateRunning }

// Ended reports whether an upgrade in this state has ended, one way or
// another.
func (s State) Ended() bool {
	return s == StateRestartRequired || s == StateFailed || s == StateDone
}

// NodeState is where one node stands in an upgrade. Its values are part of
// the management API's contract: never change one, only add.
type NodeState string

// The states of a node.
const (
	// NodePending: the node has not been moved yet.
	NodePending NodeState = "pending"
	// NodeUpgrading: the node is being moved, or is coming up again.
	NodeUpgrading NodeState = "upgrading"
	// NodeDone: the node runs the target and is ready.
	NodeDone NodeState = "done"
	// NodeFailed: the node did not come up on the target.
	NodeFailed NodeState = "failed"
	// NodeRestored: the node failed and was put back where it was.
	NodeRestored NodeState = "restored"
)

// NodeProgress is one node in an upgrade, as read from the cluster.
type NodeProgress struct {
	// ID is the node's ID.
	ID string
	// Version is the release the node runs now, as it reports it; empty
	// when it cannot be asked.
	Version string
	// Build is the node's build number, as it reports it.
	Build string
	// State is where the node stands.
	State NodeState
	// Reason says why the node failed or was restored, in words of the
	// upgrader that name no secret; empty otherwise.
	Reason string
}

// Progress is where an upgrade stands. The zero Progress is StateIdle in
// all but name; Normalized makes it say so.
type Progress struct {
	// State is where the upgrade stands.
	State State
	// Target is the version being installed; zero when idle.
	Target Version
	// StartedBy is the actor who started it, as the audit trail names
	// actors ("user/alice").
	StartedBy string
	// StartedAt is when it was started.
	StartedAt time.Time
	// Nodes are the nodes of the deployment, in the order they are moved.
	Nodes []NodeProgress
}

// Normalized returns a copy of p that shares nothing with it, with an
// empty state read as StateIdle.
func (p Progress) Normalized() Progress {
	out := p
	if out.State == "" {
		out.State = StateIdle
	}
	out.Nodes = slices.Clone(p.Nodes)
	return out
}
