package noderpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Timeouts of the client. A call that carries no deadline of its own gets
// callTimeout; connectWait is how long a call waits for a connection that is
// not up before it gives the node up as unreachable.
const (
	callTimeout       = 30 * time.Second
	connectWait       = 3 * time.Second
	minConnectTimeout = 3 * time.Second
	maxBackoff        = 2 * time.Second
	// failFast is how long a node that could not be reached is taken to be
	// unreachable without trying again. A request that needs shards from a
	// node that is down must not wait for it once per shard; the connection
	// keeps trying in the background and is used again as soon as it is up.
	failFast = 2 * time.Second
)

// Client reaches the other nodes of the cluster.
type Client struct {
	// tlsFor returns the TLS configuration for a connection to a node.
	tlsFor func(nodeID string) *tls.Config
	// resolve returns the address of a node's RPC listener.
	resolve func(ctx context.Context, nodeID string) (address string, ok bool)
	clock   ports.Clock
	// unary and stream, when not nil, see every call this node makes.
	unary  grpc.UnaryClientInterceptor
	stream grpc.StreamClientInterceptor
	// logger, when not nil, is told what was dropped of an answer.
	logger *slog.Logger

	mu     sync.Mutex
	closed bool
	conns  map[string]*connection
}

type connection struct {
	address string
	conn    *grpc.ClientConn
	// failedAt is when a call last found the node unreachable, in
	// nanoseconds since the Unix epoch; zero when it never did.
	failedAt atomic.Int64
}

// NewClient returns a Client. tlsFor returns the configuration for a
// connection to a given node: it must present this node's certificate and
// accept only a certificate that names that node. resolve finds a node's RPC
// address; it is asked on every call, so a node whose address changes is
// found again.
func NewClient(
	tlsFor func(nodeID string) *tls.Config, resolve func(ctx context.Context, nodeID string) (string, bool), clock ports.Clock,
) *Client {
	return &Client{tlsFor: tlsFor, resolve: resolve, clock: clock, conns: map[string]*connection{}}
}

// WithInterceptors has every call the client makes pass through the given
// interceptors - the node's tracing and metrics - and returns the client. It
// is for the composition root, before the first call.
func (c *Client) WithInterceptors(unary grpc.UnaryClientInterceptor, stream grpc.StreamClientInterceptor) *Client {
	c.unary, c.stream = unary, stream
	return c
}

// Close closes every connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	var errs []error
	for id, conn := range c.conns {
		errs = append(errs, conn.conn.Close())
		delete(c.conns, id)
	}
	return errors.Join(errs...)
}

// unreachable is the error for a node that could not be asked. It is
// definite: nothing was sent.
func unreachable(nodeID string, err error) error {
	return fmt.Errorf("%w: node %s cannot be reached: %w", domain.ErrUnavailable, nodeID, err)
}

// conn returns the connection to a node, opening it when there is none or
// the node has moved.
func (c *Client) conn(ctx context.Context, nodeID string) (*connection, error) {
	address, ok := c.resolve(ctx, nodeID)
	if !ok {
		return nil, unreachable(nodeID, errors.New("its address is not known"))
	}
	return c.connAt(nodeID, address)
}

func (c *Client) connAt(nodeID, address string) (*connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, domain.ErrShuttingDown
	}
	if existing, ok := c.conns[nodeID]; ok {
		if existing.address == address {
			return existing, nil
		}
		_ = existing.conn.Close()
		delete(c.conns, nodeID)
	}
	// "passthrough" hands the address to the dialer as it is: the name is
	// resolved by the operating system at every connection attempt, so a
	// node that comes back at another address is found. gRPC's own DNS
	// resolver also asks for TXT records with a service configuration, which
	// nothing here uses and which can stall a first connection for seconds.
	var observed []grpc.DialOption
	if c.unary != nil {
		observed = append(observed, grpc.WithChainUnaryInterceptor(c.unary))
	}
	if c.stream != nil {
		observed = append(observed, grpc.WithChainStreamInterceptor(c.stream))
	}
	conn, err := grpc.NewClient("passthrough:///"+address, append(observed,
		grpc.WithDisableServiceConfig(),
		// The certificate must name the node that is meant, whatever the
		// address is.
		grpc.WithTransportCredentials(credentials.NewTLS(c.tlsFor(nodeID))),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: keepaliveTime, Timeout: keepaliveTimeout, PermitWithoutStream: true}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 100 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: maxBackoff},
			MinConnectTimeout: minConnectTimeout,
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxReadLength+(1<<16))),
	)...)
	if err != nil {
		return nil, unreachable(nodeID, err)
	}
	opened := &connection{address: address, conn: conn}
	c.conns[nodeID] = opened
	return opened, nil
}

// ready returns the connection to a node once it is established, or an
// error - definite, nothing was sent - when it is not within connectWait.
func (c *Client) ready(ctx context.Context, nodeID string) (*grpc.ClientConn, error) {
	conn, err := c.conn(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return conn.conn, c.waitReady(ctx, nodeID, conn)
}

func (c *Client) waitReady(ctx context.Context, nodeID string, conn *connection) error {
	if conn.conn.GetState() == connectivity.Ready {
		return nil
	}
	if failed := conn.failedAt.Load(); failed != 0 && time.Since(time.Unix(0, failed)) < failFast {
		conn.conn.Connect()
		return unreachable(nodeID, errors.New("it could not be reached a moment ago"))
	}
	ctx, cancel := context.WithTimeout(ctx, connectWait)
	defer cancel()
	for {
		state := conn.conn.GetState()
		switch state {
		case connectivity.Ready:
			conn.failedAt.Store(0)
			return nil
		case connectivity.Idle:
			conn.conn.Connect()
		case connectivity.TransientFailure, connectivity.Shutdown:
			// The attempt has failed; the next is the connection's
			// business, not this call's.
			conn.failedAt.Store(time.Now().UnixNano())
			return unreachable(nodeID, fmt.Errorf("connection is %s", state))
		case connectivity.Connecting:
		}
		if !conn.conn.WaitForStateChange(ctx, state) {
			conn.failedAt.Store(time.Now().UnixNano())
			return unreachable(nodeID, fmt.Errorf("connection is %s", state))
		}
	}
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, callTimeout)
}

// answered turns the two ways a call can fail into one error: the transport
// error when the node did not answer, and otherwise what the node answered.
func answered(nodeID string, rpcErr error, reported *nodev1.Error) error {
	if rpcErr != nil {
		return unreachable(nodeID, rpcErr)
	}
	return fromWire(nodeID, reported)
}

// Propose implements raftmeta.LeaderClient.
func (c *Client) Propose(ctx context.Context, leaderID string, entry []byte) ([]byte, error) {
	// Until the request is handed to an established connection, nothing
	// has been sent and a failure is definite.
	conn, err := c.ready(ctx, leaderID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := nodev1.NewMetaServiceClient(conn).Propose(ctx, &nodev1.ProposeRequest{Entry: entry})
	if err != nil {
		// It may have arrived, and the entry may be in the log.
		return nil, fmt.Errorf("%w: no answer from node %s: %w", domain.ErrOutcomeUnknown, leaderID, err)
	}
	if err := fromWire(leaderID, resp.GetError()); err != nil {
		return nil, err
	}
	return resp.GetResult(), nil
}

// ReadIndex implements raftmeta.LeaderClient.
func (c *Client) ReadIndex(ctx context.Context, leaderID string) (uint64, error) {
	conn, err := c.ready(ctx, leaderID)
	if err != nil {
		return 0, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := nodev1.NewMetaServiceClient(conn).ReadIndex(ctx, &nodev1.ReadIndexRequest{})
	if err := answered(leaderID, err, resp.GetError()); err != nil {
		return 0, err
	}
	return resp.GetIndex(), nil
}

// Term implements raftmeta.LeaderClient.
func (c *Client) Term(ctx context.Context, nodeID string) (uint64, error) {
	conn, err := c.ready(ctx, nodeID)
	if err != nil {
		return 0, err
	}
	resp, err := nodev1.NewMetaServiceClient(conn).Term(ctx, &nodev1.TermRequest{})
	if err := answered(nodeID, err, resp.GetError()); err != nil {
		return 0, err
	}
	return resp.GetTerm(), nil
}

// Join asks the node via to add a node to the metadata group. The error
// wraps domain.ErrNotLeader when via does not lead it.
func (c *Client) Join(ctx context.Context, via, nodeID, raftAddr string, voter bool) error {
	conn, err := c.ready(ctx, via)
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := nodev1.NewMetaServiceClient(conn).Join(ctx, &nodev1.JoinRequest{NodeId: nodeID, RaftAddr: raftAddr, Voter: voter})
	return answered(via, err, resp.GetError())
}

// Leave asks the node via to remove a node from the cluster. The error wraps
// domain.ErrNotLeader when via does not lead the metadata group.
func (c *Client) Leave(ctx context.Context, via, nodeID string) error {
	conn, err := c.ready(ctx, via)
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := nodev1.NewMetaServiceClient(conn).Leave(ctx, &nodev1.LeaveRequest{NodeId: nodeID})
	return answered(via, err, resp.GetError())
}

// Info asks a node, reached at address, to describe itself. It is how a node
// meets its peers before the cluster has a record of them.
func (c *Client) Info(ctx context.Context, nodeID, address string) (NodeInfo, error) {
	opened, err := c.connAt(nodeID, address)
	if err != nil {
		return NodeInfo{}, err
	}
	if err := c.waitReady(ctx, nodeID, opened); err != nil {
		return NodeInfo{}, err
	}
	conn := opened.conn
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := nodev1.NewNodeServiceClient(conn).Info(ctx, &nodev1.InfoRequest{})
	if err := answered(nodeID, err, resp.GetError()); err != nil {
		return NodeInfo{}, err
	}
	if resp.GetNodeId() != nodeID {
		return NodeInfo{}, fmt.Errorf("%w: the node at %s calls itself %q, not %q", domain.ErrInvalidArgument, address, resp.GetNodeId(), nodeID)
	}
	info := NodeInfo{
		Node: cluster.Node{
			ID: resp.GetNodeId(), RPCAddr: resp.GetRpcAddr(), RaftAddr: resp.GetRaftAddr(),
			Labels:          cluster.Labels{Region: resp.GetRegion(), Zone: resp.GetZone(), Rack: resp.GetRack(), Host: resp.GetHost()},
			MetadataVersion: int(resp.GetMetadataVersion()),
		},
		Formed: resp.GetFormed(), DeploymentID: resp.GetDeploymentId(),
	}
	for _, role := range resp.GetRoles() {
		info.Node.Roles = append(info.Node.Roles, cluster.Role(role))
	}
	for _, drive := range resp.GetDrives() {
		info.Drives = append(info.Drives, cluster.DriveState{
			ID: drive.GetId(), NodeID: nodeID, Empty: drive.GetEmpty(), DeploymentID: drive.GetDeploymentId(),
		})
	}
	return info, nil
}

// InFlight asks a node whether it has an unresolved write that uses the
// stored data. An error means the question could not be answered - which a
// collector must read as "perhaps".
func (c *Client) InFlight(ctx context.Context, nodeID string, dataID object.ID) (bool, error) {
	conn, err := c.ready(ctx, nodeID)
	if err != nil {
		return false, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := nodev1.NewNodeServiceClient(conn).InFlight(ctx, &nodev1.InFlightRequest{DataId: dataID[:]})
	if err := answered(nodeID, err, resp.GetError()); err != nil {
		return false, err
	}
	return resp.GetInFlight(), nil
}

var _ ports.NodeSettingsReader = (*Client)(nil)

// WithLogger has the client log what it drops of an answer that exceeds the
// bounds of Settings, and returns the client. It is for the composition
// root, before the first call; without it nothing is logged.
func (c *Client) WithLogger(logger *slog.Logger) *Client {
	c.logger = logger
	return c
}

// NodeSettings implements ports.NodeSettingsReader: it asks the node for its
// settings. The answer is bounded and normalized here, whatever the node
// sent - a node that is wrong, or is not what it says, gets no secret value
// and no endless list through.
//
// The error wraps domain.ErrNotSupported when the node's release has no such
// call, context.DeadlineExceeded when the node was reached and did not answer
// in the time of ctx, domain.ErrUnavailable when it could not be reached, and
// none of them when it was reached and could not, or may not, say.
func (c *Client) NodeSettings(ctx context.Context, nodeID string) (cluster.NodeSettings, error) {
	conn, err := c.ready(ctx, nodeID)
	if err != nil {
		return cluster.NodeSettings{}, fmt.Errorf("noderpc.NodeSettings: %w", err)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := nodev1.NewNodeServiceClient(conn).Settings(ctx, &nodev1.SettingsRequest{})
	if err != nil {
		return cluster.NodeSettings{}, fmt.Errorf("noderpc.NodeSettings: %w", settingsCallError(nodeID, err))
	}
	if err := fromWire(nodeID, resp.GetError()); err != nil {
		return cluster.NodeSettings{}, fmt.Errorf("noderpc.NodeSettings: %w", err)
	}
	if resp.GetNodeId() != nodeID {
		return cluster.NodeSettings{}, fmt.Errorf("noderpc.NodeSettings: %w: node %s answers as %q",
			domain.ErrInvalidArgument, nodeID, clipName(resp.GetNodeId()))
	}
	bounded, dropped := boundSettings(settingsFromWire(resp))
	if dropped > 0 && c.logger != nil {
		c.logger.WarnContext(ctx, "a node sent more settings than are taken; the excess is dropped",
			slog.String("node", nodeID), slog.Int("dropped", dropped))
	}
	return bounded.Normalized(), nil
}

// settingsCallError sorts a call that got no answer by how gRPC says it
// failed.
func settingsCallError(nodeID string, err error) error {
	switch status.Code(err) {
	case codes.Unimplemented:
		// The node was reached and does not know the call: it runs a
		// release from before the call existed.
		return fmt.Errorf("%w: node %s: %w", domain.ErrNotSupported, nodeID, err)
	case codes.DeadlineExceeded:
		return fmt.Errorf("%w: node %s did not answer in time: %w", context.DeadlineExceeded, nodeID, err)
	case codes.Canceled:
		return fmt.Errorf("%w: node %s: %w", context.Canceled, nodeID, err)
	case codes.Unavailable:
		return unreachable(nodeID, err)
	default:
		return fmt.Errorf("node %s: %w", nodeID, err)
	}
}
