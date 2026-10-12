package noderpc_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/albievan/cactusfs/internal/adapters/noderpc"
	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

const deliveryStatusMethod = "/cactusfs.node.v1.NodeService/DeliveryStatus"

// deliveryStatus calls DeliveryStatus on a node as gRPC delivers it, with
// the certificate of the given identity.
func deliveryStatus(t *testing.T, creds credentials.TransportCredentials, address string) (*nodev1.DeliveryStatusResponse, error) {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return nodev1.NewNodeServiceClient(conn).DeliveryStatus(ctx, &nodev1.DeliveryStatusRequest{})
}

// TestDeliveryStatusIsInTheContractAndNotServedYet: the call exists before
// the worker it reports on (PLAN 8.0.3). It is one of the methods of node
// RPC - so it is counted and, like every call, authorized as the node whose
// certificate the caller showed - and a node of this release answers it as a
// release without the call does: UNIMPLEMENTED, never an empty answer that
// would read as "nothing waits". The step that serves it (PLAN 8.0.14)
// replaces the last assertion.
func TestDeliveryStatusIsInTheContractAndNotServedYet(t *testing.T) {
	t.Parallel()
	if !slices.Contains(noderpc.Methods(), deliveryStatusMethod) {
		t.Fatalf("%s is not among the methods of node RPC: %v", deliveryStatusMethod, noderpc.Methods())
	}
	p, foreign := newPKI(t), newPKI(t)
	authz := &switchAuthorizer{}
	node := serveWith(t, p, "storage-1", func(cfg *noderpc.ServerConfig) { cfg.Authz = authz })

	// A caller without a certificate of the node CA gets no connection.
	_, err := deliveryStatus(t, credentials.NewTLS(foreign.identity("gateway-1").Client("storage-1")), node.address)
	if status.Code(err) != codes.Unavailable {
		t.Errorf("DeliveryStatus with a certificate of another CA = %v, want the node to be unreachable", err)
	}
	if len(authz.inner.Requests()) != 0 {
		t.Fatal("a stranger's call reached the authorizer")
	}

	// A member is asked about, as the node its certificate names, and told
	// that the call is not served.
	member := credentials.NewTLS(p.identity("gateway-7").Client("storage-1"))
	resp, err := deliveryStatus(t, member, node.address)
	if status.Code(err) != codes.Unimplemented || resp != nil {
		t.Errorf("DeliveryStatus = %v, %v; want UNIMPLEMENTED and no answer", resp, err)
	}
	requests := authz.inner.Requests()
	if len(requests) != 1 || requests[0].Principal.NodeID != "gateway-7" || requests[0].Action != iam.ActionNodeRPC {
		t.Errorf("authorization requests = %+v, want one of gateway-7 for node RPC", requests)
	}

	// What the authorizer refuses is refused, and not as "not served".
	authz.deny.Store(true)
	if _, err := deliveryStatus(t, member, node.address); status.Code(err) != codes.PermissionDenied {
		t.Errorf("DeliveryStatus although the authorizer refused = %v, want PERMISSION_DENIED", err)
	}
}
