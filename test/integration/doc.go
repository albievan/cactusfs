// Package integration holds the black-box tests that run against the real
// cactusfs binary. They are compiled only with -tags=integration:
//
//	go test -race -tags=integration ./...
//
// From milestone 3 these tests start a multi-node cluster, Vault and NATS with
// testcontainers-go.
package integration
