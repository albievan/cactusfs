package cluster

import (
	"fmt"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/erasure"
)

// Slot is one position in an erasure set. A slot outlives the drive in it:
// when a drive is replaced, the new drive takes over the slot, and data that
// was recorded under the old drive's ID is looked for, and rebuilt, on the
// new one.
type Slot struct {
	// DriveID is the drive currently in the slot.
	DriveID string
	// Retired lists the drives that were in the slot before.
	Retired []string
}

// Topology assigns the deployment's drives to erasure sets. It is replicated
// metadata: which drive belongs where is decided once and recorded, never
// derived from the order of command-line flags.
type Topology struct {
	// DeploymentID identifies the deployment. Every drive is claimed for it,
	// which is how a drive and the metadata that describes it are known to
	// belong together.
	DeploymentID string
	// Version counts the changes; it guards updates against races.
	Version uint64
	// Sets lists the slots of each erasure set. Every set has the same size.
	Sets [][]Slot
}

// Position is a slot's place in the topology.
type Position struct {
	Set  int
	Slot int
}

// DriveState describes a drive that is attached to the node.
type DriveState struct {
	// ID is the drive's generated identifier.
	ID string
	// Empty reports that the drive holds no shard.
	Empty bool
	// DeploymentID is the deployment the drive has been claimed for, or ""
	// when it belongs to none.
	DeploymentID string
}

// CheckNewDeployment reports whether the attached drives may form a new
// deployment: every one must be empty and unclaimed. Drives that hold data
// while the metadata knows of no deployment mean that the wrong metadata
// directory is in use - adopting them would make every shard on them look
// unreferenced, and the collector would then remove it. The error wraps
// domain.ErrInvalidArgument.
func CheckNewDeployment(attached []DriveState) error {
	for _, drive := range attached {
		switch {
		case drive.DeploymentID != "":
			return fmt.Errorf("%w: drive %s belongs to deployment %s, but the metadata directory has no record of any "+
				"deployment; check that --meta-dir points at this deployment's metadata",
				domain.ErrInvalidArgument, drive.ID, drive.DeploymentID)
		case !drive.Empty:
			return fmt.Errorf("%w: drive %s holds data, but the metadata directory has no record of any deployment; "+
				"check that --meta-dir points at this deployment's metadata", domain.ErrInvalidArgument, drive.ID)
		}
	}
	return nil
}

// NewTopology assigns drives to erasure sets in the order given. The error
// wraps domain.ErrInvalidArgument when the drives cannot form equal sets.
func NewTopology(driveIDs []string) (Topology, error) {
	sets, size, err := erasure.SetLayout(len(driveIDs))
	if err != nil {
		return Topology{}, err
	}
	seen := map[string]bool{}
	t := Topology{Sets: make([][]Slot, sets)}
	for i, id := range driveIDs {
		if id == "" || seen[id] {
			return Topology{}, fmt.Errorf("%w: drive ID %q is empty or listed twice", domain.ErrInvalidArgument, id)
		}
		seen[id] = true
		t.Sets[i/size] = append(t.Sets[i/size], Slot{DriveID: id})
	}
	return t, nil
}

// Empty reports whether no topology has been set.
func (t Topology) Empty() bool {
	return len(t.Sets) == 0
}

// SetSize returns the number of drives in each set.
func (t Topology) SetSize() int {
	if t.Empty() {
		return 0
	}
	return len(t.Sets[0])
}

// Validate reports whether the topology is well formed: equal sets within
// the size limits and no drive ID in two places. The error wraps
// domain.ErrInvalidArgument.
func (t Topology) Validate() error {
	if t.Empty() {
		return fmt.Errorf("%w: topology has no erasure sets", domain.ErrInvalidArgument)
	}
	size := t.SetSize()
	if size < 1 || size > erasure.MaxSetDrives {
		return fmt.Errorf("%w: erasure sets of %d drives", domain.ErrInvalidArgument, size)
	}
	seen := map[string]bool{}
	for _, set := range t.Sets {
		if len(set) != size {
			return fmt.Errorf("%w: erasure sets differ in size", domain.ErrInvalidArgument)
		}
		for _, slot := range set {
			for _, id := range append([]string{slot.DriveID}, slot.Retired...) {
				if id == "" || seen[id] {
					return fmt.Errorf("%w: drive ID %q is empty or appears twice", domain.ErrInvalidArgument, id)
				}
				seen[id] = true
			}
		}
	}
	return nil
}

// Locate returns the slot a drive ID belongs to, whether the drive is in it
// now or was in it before being replaced.
func (t Topology) Locate(driveID string) (Position, bool) {
	for s, set := range t.Sets {
		for i, slot := range set {
			if slot.DriveID == driveID || slices.Contains(slot.Retired, driveID) {
				return Position{Set: s, Slot: i}, true
			}
		}
	}
	return Position{}, false
}

// Current returns the ID of the drive now in the slot that driveID belongs
// to. For a drive that was never replaced this is driveID itself.
func (t Topology) Current(driveID string) (string, bool) {
	pos, ok := t.Locate(driveID)
	if !ok {
		return "", false
	}
	return t.Sets[pos.Set][pos.Slot].DriveID, true
}

// DriveIDs returns the current drive of every slot, set by set.
func (t Topology) DriveIDs() []string {
	var ids []string
	for _, set := range t.Sets {
		for _, slot := range set {
			ids = append(ids, slot.DriveID)
		}
	}
	return ids
}

// Replacement is a drive taking over a slot.
type Replacement struct {
	Position Position
	Old      string
	New      string
}

// Reconcile compares the topology with the drives that are attached now.
//
// When they match it returns no replacement. A drive that is claimed for
// another deployment, a drive that was retired earlier, and an unknown drive
// that holds data are always refused.
//
// When exactly one drive of the topology is absent and exactly one attached
// drive is unknown and empty, the empty drive may take over the slot - but
// only where there is parity to rebuild its contents from, and only when the
// operator has confirmed it with allowReplacement. A missing mount looks
// exactly like a new empty drive, and a replacement retires the old drive for
// good, so it is never assumed.
//
// Every refusal wraps domain.ErrInvalidArgument.
func (t Topology) Reconcile(attached []DriveState, allowReplacement bool) (*Replacement, error) {
	present := map[string]DriveState{}
	for _, drive := range attached {
		if _, dup := present[drive.ID]; dup {
			return nil, fmt.Errorf("%w: drive %s is attached twice", domain.ErrInvalidArgument, drive.ID)
		}
		if drive.DeploymentID != "" && drive.DeploymentID != t.DeploymentID {
			return nil, fmt.Errorf("%w: drive %s belongs to deployment %s, not to this one (%s)",
				domain.ErrInvalidArgument, drive.ID, drive.DeploymentID, t.DeploymentID)
		}
		present[drive.ID] = drive
	}

	var missing, unknown, stale []string
	for _, id := range t.DriveIDs() {
		if _, ok := present[id]; !ok {
			missing = append(missing, id)
		}
	}
	for _, drive := range attached {
		current, known := t.Current(drive.ID)
		switch {
		case !known && !drive.Empty:
			return nil, fmt.Errorf("%w: drive %s is not part of this deployment and holds data; "+
				"only an empty drive can be added", domain.ErrInvalidArgument, drive.ID)
		case !known:
			unknown = append(unknown, drive.ID)
		case current != drive.ID:
			stale = append(stale, drive.ID)
		}
	}

	switch {
	case len(stale) > 0:
		return nil, fmt.Errorf("%w: drive %s was replaced earlier and must not be attached again",
			domain.ErrInvalidArgument, strings.Join(stale, ", "))
	case len(missing) == 0 && len(unknown) == 0:
		return nil, nil
	case len(missing) != 1 || len(unknown) != 1:
		return nil, fmt.Errorf("%w: attached drives do not match the deployment: missing [%s], unknown [%s]; "+
			"one empty drive may replace one missing drive at a time",
			domain.ErrInvalidArgument, strings.Join(missing, ", "), strings.Join(unknown, ", "))
	}

	params, err := erasure.ParamsFor(t.SetSize())
	if err != nil {
		return nil, err
	}
	if params.Parity == 0 {
		return nil, fmt.Errorf("%w: drive %s is missing and an empty drive is attached in its place, but this deployment "+
			"has no parity: what was on the missing drive cannot be rebuilt; attach the original drive",
			domain.ErrInvalidArgument, missing[0])
	}
	if !allowReplacement {
		return nil, fmt.Errorf("%w: drive %s is missing and empty drive %s is attached in its place; if the original "+
			"drive is gone for good, confirm the replacement (it is permanent: the original drive will be refused "+
			"afterwards, and its contents are rebuilt on the new drive); if it is only not mounted, mount it",
			domain.ErrInvalidArgument, missing[0], unknown[0])
	}
	pos, _ := t.Locate(missing[0])
	return &Replacement{Position: pos, Old: missing[0], New: unknown[0]}, nil
}

// Clone returns a deep copy of the topology.
func (t Topology) Clone() Topology {
	next := Topology{DeploymentID: t.DeploymentID, Version: t.Version, Sets: make([][]Slot, len(t.Sets))}
	for s, set := range t.Sets {
		next.Sets[s] = make([]Slot, len(set))
		for i, slot := range set {
			next.Sets[s][i] = Slot{DriveID: slot.DriveID, Retired: slices.Clone(slot.Retired)}
		}
	}
	return next
}

// Replace returns the topology with a drive replaced. The receiver is not
// modified.
func (t Topology) Replace(r Replacement) Topology {
	next := t.Clone()
	slot := &next.Sets[r.Position.Set][r.Position.Slot]
	slot.Retired = append(slot.Retired, slot.DriveID)
	slot.DriveID = r.New
	return next
}
