// Package noderpc is the node-to-node adapter: gRPC over mutual TLS 1.3, by
// the contract in api/proto/cactusfs/node/v1. It is inbound and outbound at
// once. The Server lets other nodes reach this node's drives, its membership
// of the metadata group and its work in progress; the Client reaches the
// same on other nodes, and makes a drive on another node look like any other
// ports.ShardStore.
//
// Every call is authenticated by the caller's node certificate and passes
// the authorizer as that node. Nothing here decides anything about objects:
// the adapter moves bytes and answers.
package noderpc
