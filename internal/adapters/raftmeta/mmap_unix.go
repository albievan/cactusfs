//go:build !windows

package raftmeta

// stateMmapSize is the least the state database is mapped at (see
// openState). Here the map is address space only: the file grows with what
// it holds, whatever the map.
const stateMmapSize = 1 << 30
