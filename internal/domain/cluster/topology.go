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
	// Fresh reports that the drive was empty and has just been formatted.
	Fresh bool
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
// When they match it returns no replacement. When exactly one drive of the
// topology is absent and exactly one attached drive is unknown and fresh, the
// fresh drive replaces the absent one. Anything else is refused: a missing
// drive, an unknown drive that already holds data, or more than one change at
// a time needs a person to decide. The error wraps
// domain.ErrInvalidArgument.
func (t Topology) Reconcile(attached []DriveState) (*Replacement, error) {
	present := map[string]DriveState{}
	for _, drive := range attached {
		if _, dup := present[drive.ID]; dup {
			return nil, fmt.Errorf("%w: drive %s is attached twice", domain.ErrInvalidArgument, drive.ID)
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
	case len(missing) == 1 && len(unknown) == 1 && present[unknown[0]].Fresh:
		pos, _ := t.Locate(missing[0])
		return &Replacement{Position: pos, Old: missing[0], New: unknown[0]}, nil
	case len(missing) == 1 && len(unknown) == 1:
		return nil, fmt.Errorf("%w: drive %s is missing and drive %s is not part of this deployment and is not empty; "+
			"only an empty drive can replace a missing one", domain.ErrInvalidArgument, missing[0], unknown[0])
	default:
		return nil, fmt.Errorf("%w: attached drives do not match the deployment: missing [%s], unknown [%s]; "+
			"one empty drive may replace one missing drive at a time",
			domain.ErrInvalidArgument, strings.Join(missing, ", "), strings.Join(unknown, ", "))
	}
}

// Clone returns a deep copy of the topology.
func (t Topology) Clone() Topology {
	next := Topology{Version: t.Version, Sets: make([][]Slot, len(t.Sets))}
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
