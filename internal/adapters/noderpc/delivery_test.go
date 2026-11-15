package noderpc_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/albievan/cactusfs/internal/adapters/noderpc"
	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
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

// TestDeliveryStatusIsAuthorized: the call is one of the methods of node
// RPC - so it is counted and, like every call, authorized as the node whose
// certificate the caller showed - and a node that was given nothing to
// answer it from answers as a release without the call does: UNIMPLEMENTED,
// never an empty answer that would read as "nothing waits".
func TestDeliveryStatusIsAuthorized(t *testing.T) {
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
	// that the call is not served by a node without a source.
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

// leaderReader is a ports.DeliveryStatusReader that asks one node, as the
// composition root asks the node that leads.
type leaderReader struct {
	client *noderpc.Client
	leader string
}

func (r leaderReader) DeliveryStatus(ctx context.Context, targets []string) ([]ports.TargetStatus, error) {
	return r.client.DeliveryStatus(ctx, r.leader, targets)
}

// TestDeliveryStatusOverMutualTLS: the status of delivery asked of the node
// that leads, over mutual TLS, behaves as every reader of it must - its
// answer handed on in name order with nothing where a word belongs that is
// none, a node of a release without the call read as "not supported", one
// that cannot be reached as unavailable, one that does not answer within the
// time given as that.
func TestDeliveryStatusOverMutualTLS(t *testing.T) {
	t.Parallel()
	porttest.DeliveryStatusContract(t, func(t *testing.T, script porttest.DeliveryStatusScript) ports.DeliveryStatusReader {
		p := newPKI(t)
		addresses := map[string]string{}
		switch script.Leader {
		case porttest.DeliveryLeaderGone:
			// No node is at the address the leader is known by.
		case porttest.DeliveryLeaderOld:
			addresses["meta-1"] = serve(t, p, "meta-1").address
		default:
			reader := porttest.ScriptedDeliveryStatusReader(script)
			addresses["meta-1"] = serveWith(t, p, "meta-1", func(cfg *noderpc.ServerConfig) { cfg.Delivery = reader }).address
		}
		return leaderReader{client: clientFor(t, p.identity("gateway-1"), addresses), leader: "meta-1"}
	})
}

// notLeading is the delivery of a node that does not lead.
type notLeading struct{}

func (notLeading) DeliveryStatus(context.Context, []string) ([]ports.TargetStatus, error) {
	return nil, fmt.Errorf("%w: this node does not deliver (%s)", domain.ErrNotLeader, porttest.DeliveryMark)
}

// TestDeliveryStatusOfANodeThatDoesNotLead: the node asked has stepped down;
// the one that asked reads that there is no answer to be had there now, and
// is not told the words of the error.
func TestDeliveryStatusOfANodeThatDoesNotLead(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	node := serveWith(t, p, "meta-1", func(cfg *noderpc.ServerConfig) { cfg.Delivery = notLeading{} })
	client := clientFor(t, p.identity("gateway-1"), map[string]string{"meta-1": node.address})
	got, err := client.DeliveryStatus(t.Context(), "meta-1", nil)
	if !errors.Is(err, domain.ErrUnavailable) || !errors.Is(err, domain.ErrNotLeader) || len(got) != 0 {
		t.Errorf("DeliveryStatus of a node that does not lead = %+v, %v; want unavailable, not leader", got, err)
	}
	if strings.Contains(fmt.Sprint(err), "MARK-7c1e") {
		t.Errorf("the words of the other node's error came across: %v", err)
	}
}

// hostileNode answers DeliveryStatus with whatever it is given, as a node
// that is wrong, or is not what it says, could.
type hostileNode struct {
	nodev1.UnimplementedNodeServiceServer
	answer *nodev1.DeliveryStatusResponse
}

func (h hostileNode) DeliveryStatus(context.Context, *nodev1.DeliveryStatusRequest) (*nodev1.DeliveryStatusResponse, error) {
	return h.answer, nil
}

// TestDeliveryStatusTakesOnlyWhatItAskedFor: whatever the node that is asked
// sends, the one that asked takes the targets it asked about, each once,
// with a name a target can have, in name order, and nothing where a word
// belongs that is not one.
func TestDeliveryStatusTakesOnlyWhatItAskedFor(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	answer := &nodev1.DeliveryStatusResponse{Targets: []*nodev1.TargetDelivery{
		{Name: "site-b", Kind: "s3", State: "stalled", Pending: 4, OldestPendingUnix: 100, LastErrorClass: "auth", Lost: 1, Failed: 2, ResyncNeeded: true},
		{Name: "hook", Kind: "webhook", State: porttest.DeliveryMark, LastErrorClass: porttest.DeliveryMark, LastSuccessUnix: 200, Rejected: 3},
		{Name: "hook", Kind: "webhook", State: "ok"},
		{Name: "Not A Name", Kind: "webhook", State: "ok"},
		{Name: "unasked", Kind: "webhook", State: "ok"},
		{Name: "later", Kind: "carrier-pigeon", State: "draining", LastErrorClass: "throttled"},
	}}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(p.identity("meta-1").Server())))
	nodev1.RegisterNodeServiceServer(server, hostileNode{answer: answer})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	client := clientFor(t, p.identity("gateway-1"), map[string]string{"meta-1": listener.Addr().String()})
	got, err := client.DeliveryStatus(t.Context(), "meta-1", []string{"site-b", "hook", "later", "Not A Name"})
	if err != nil {
		t.Fatalf("DeliveryStatus: %v", err)
	}
	want := []ports.TargetStatus{
		{Name: "hook", Kind: delivery.KindWebhook, State: ports.DeliveryUnsupported, LastErrorClass: delivery.ClassInternal, LastSuccessAt: time.Unix(200, 0).UTC(), Rejected: 3},
		{Name: "later", State: "draining", LastErrorClass: "throttled"},
		{
			Name: "site-b", Kind: delivery.KindS3, State: ports.DeliveryStalled, Pending: 4, OldestPendingAt: time.Unix(100, 0).UTC(),
			LastErrorClass: delivery.ClassAuth, Lost: 1, Failed: 2, ResyncNeeded: true,
		},
	}
	if !slices.Equal(got, want) {
		t.Errorf("DeliveryStatus =\n%+v\nwant\n%+v", got, want)
	}
	if text := fmt.Sprintf("%+v", got); strings.Contains(text, "MARK-7c1e") {
		t.Errorf("what the node said where a word belongs is handed on: %s", text)
	}
}
