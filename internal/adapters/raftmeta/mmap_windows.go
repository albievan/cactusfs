//go:build windows

package raftmeta

// stateMmapSize is the least the state database is mapped at (see
// openState). On Windows bbolt makes the file as large as its map, so the
// map is held to what a node may well spend on disk for its metadata.
const stateMmapSize = 16 << 20
