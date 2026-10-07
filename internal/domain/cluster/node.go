package cluster

import (
	"fmt"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// Role is one of the jobs a node can do. One binary does all of them; a node
// is told which with --roles.
type Role string

// The roles of a node.
const (
	// RoleGateway serves the S3 and admin APIs: authentication, erasure
	// encoding and decoding. It keeps no data of its own.
	RoleGateway Role = "gateway"
	// RoleMeta is a voter in the metadata Raft group.
	RoleMeta Role = "meta"
	// RoleStorage owns drives: it stores shards, and scrubs and heals them.
	RoleStorage Role = "storage"
	// RoleConsole serves the web console.
	RoleConsole Role = "console"
)

// AllRoles returns every role, which is what a node does by default.
func AllRoles() Roles {
	return Roles{RoleGateway, RoleMeta, RoleStorage, RoleConsole}
}

// Roles is a set of roles.
type Roles []Role

// ParseRoles parses a comma-separated list of roles. An empty list means all
// roles. The error wraps domain.ErrInvalidArgument.
func ParseRoles(list string) (Roles, error) {
	if strings.TrimSpace(list) == "" {
		return AllRoles(), nil
	}
	var roles Roles
	for name := range strings.SplitSeq(list, ",") {
		role := Role(strings.TrimSpace(name))
		if !slices.Contains(AllRoles(), role) {
			return nil, fmt.Errorf("%w: unknown role %q: want gateway, meta, storage or console", domain.ErrInvalidArgument, name)
		}
		if !roles.Has(role) {
			roles = append(roles, role)
		}
	}
	return roles, nil
}

// Has reports whether the set contains role.
func (r Roles) Has(role Role) bool {
	return slices.Contains(r, role)
}

// String returns the roles as a comma-separated list.
func (r Roles) String() string {
	names := make([]string, len(r))
	for i, role := range r {
		names[i] = string(role)
	}
	return strings.Join(names, ",")
}

// Labels say where a node is, from the widest failure domain to the
// narrowest. They steer placement: shards of one object are spread across
// zones, then racks, then nodes. All are optional.
type Labels struct {
	Region string
	Zone   string
	Rack   string
	Host   string
}

// Node is a member of the cluster.
type Node struct {
	// ID is the node's stable name, unique in the cluster.
	ID string
	// RPCAddr is where other nodes reach its node RPC.
	RPCAddr string
	// RaftAddr is where other nodes reach its Raft transport.
	RaftAddr string
	Roles    Roles
	Labels   Labels
	// MetadataVersion is the newest metadata entry format the node's binary
	// can apply. No node writes an entry newer than the oldest of these, so
	// that binaries of two releases can run side by side during an upgrade.
	MetadataVersion int
}

// maxNodeIDLength bounds a node ID.
const maxNodeIDLength = 63

// ValidateNodeID reports whether id may name a node: 1 to 63 lowercase
// letters, digits and hyphens, beginning and ending with a letter or digit,
// like a DNS label. The error wraps domain.ErrInvalidArgument.
func ValidateNodeID(id string) error {
	if id == "" || len(id) > maxNodeIDLength {
		return fmt.Errorf("%w: node ID %q: must be 1 to %d characters", domain.ErrInvalidArgument, id, maxNodeIDLength)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !alnum && (c != '-' || i == 0 || i == len(id)-1) {
			return fmt.Errorf("%w: node ID %q: lowercase letters, digits and inner hyphens only", domain.ErrInvalidArgument, id)
		}
	}
	return nil
}

// Validate reports whether the node is well formed. The error wraps
// domain.ErrInvalidArgument.
func (n Node) Validate() error {
	if err := ValidateNodeID(n.ID); err != nil {
		return err
	}
	if len(n.Roles) == 0 {
		return fmt.Errorf("%w: node %s has no role", domain.ErrInvalidArgument, n.ID)
	}
	for _, role := range n.Roles {
		if !slices.Contains(AllRoles(), role) {
			return fmt.Errorf("%w: node %s has unknown role %q", domain.ErrInvalidArgument, n.ID, role)
		}
	}
	return nil
}
