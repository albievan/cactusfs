package noderpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Meta is what the server needs of this node's metadata store.
type Meta interface {
	// ProposeLocal commits an entry if this node leads the metadata group.
	ProposeLocal(ctx context.Context, entry []byte) ([]byte, error)
	// Subject says what an entry is about, for authorization: the node
	// whose record it sets (registers) or removes, or whose count of reads
	// it reports (counts), if it is such an entry.
	Subject(entry []byte) (registers, removes, counts string, err error)
	// ReadIndexLocal returns the read index if this node leads.
	ReadIndexLocal(ctx context.Context) (uint64, error)
	// Term returns the node's Raft term.
	Term() (uint64, error)
	// Join adds a node to the metadata group; Leave removes one, and
	// forgets it. Both need this node to lead.
	Join(ctx context.Context, nodeID, raftAddr string, voter bool) error
	Leave(ctx context.Context, nodeID string) error
}

// ServerConfig lists what a Server depends on.
type ServerConfig struct {
	// TLS must demand and verify a node certificate of every caller.
	TLS *tls.Config
	// PeerNodeID returns the node a verified connection belongs to.
	PeerNodeID func(tls.ConnectionState) (string, error)
	// Authz decides on every call, with the calling node as principal.
	Authz ports.Authorizer
	// Info describes this node.
	Info func(ctx context.Context) (NodeInfo, error)
	// InFlight reports whether this node has an unresolved write that uses
	// the stored data.
	InFlight func(object.ID) bool
	// Settings says how this node was configured, for the Settings call. A
	// nil one is a node that cannot say, and answers so.
	Settings ports.LocalSettings
	Meta     Meta
	// Drive returns one of this node's drives.
	Drive  func(driveID string) (ports.ShardStore, bool)
	Clock  ports.Clock
	Logger *slog.Logger
	// UnaryInterceptor and StreamInterceptor, when not nil, see every call
	// before it is authenticated: the node's tracing and metrics, which
	// count a call that is refused as well.
	UnaryInterceptor  grpc.UnaryServerInterceptor
	StreamInterceptor grpc.StreamServerInterceptor
	// OnHandshakeRefused, when not nil, is told of every connection whose
	// handshake this server did not complete, with one of the Refused
	// reasons.
	OnHandshakeRefused func(reason string)
}

// Server serves node RPC.
type Server struct {
	nodev1.UnimplementedNodeServiceServer
	nodev1.UnimplementedMetaServiceServer
	nodev1.UnimplementedStorageServiceServer

	cfg  ServerConfig
	grpc *grpc.Server
}

type principalKey struct{}

// Keepalive: a node that has gone silent - powered off, or cut off by a
// network that drops packets - is noticed within about ten seconds, by
// either side.
const (
	keepaliveTime    = 5 * time.Second
	keepaliveTimeout = 5 * time.Second
)

// NewServer returns a Server. Serve starts it.
func NewServer(cfg ServerConfig) *Server {
	s := &Server{cfg: cfg}
	unary, stream := []grpc.UnaryServerInterceptor{s.unaryAuth}, []grpc.StreamServerInterceptor{s.streamAuth}
	if cfg.UnaryInterceptor != nil {
		unary = []grpc.UnaryServerInterceptor{cfg.UnaryInterceptor, s.unaryAuth}
	}
	if cfg.StreamInterceptor != nil {
		stream = []grpc.StreamServerInterceptor{cfg.StreamInterceptor, s.streamAuth}
	}
	s.grpc = grpc.NewServer(
		grpc.Creds(observedCredentials{TransportCredentials: credentials.NewTLS(cfg.TLS), refused: cfg.OnHandshakeRefused}),
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: keepaliveTime, Timeout: keepaliveTimeout}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: keepaliveTime / 2, PermitWithoutStream: true}),
	)
	nodev1.RegisterNodeServiceServer(s.grpc, s)
	nodev1.RegisterMetaServiceServer(s.grpc, s)
	nodev1.RegisterStorageServiceServer(s.grpc, s)
	return s
}

// Serve accepts connections on listener until Stop. The listener is a plain
// one: the server does the TLS handshake itself.
func (s *Server) Serve(listener net.Listener) error {
	if err := s.grpc.Serve(listener); err != nil {
		return fmt.Errorf("noderpc.Serve: %w", err)
	}
	return nil
}

// Stop ends the server: calls in progress get the grace period to finish,
// then they are cut off.
func (s *Server) Stop(grace time.Duration) {
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		s.grpc.Stop()
		<-done
	}
}

// authenticate turns the caller's verified certificate into a principal and
// asks the authorizer whether that node may speak node RPC at all.
func (s *Server) authenticate(ctx context.Context) (context.Context, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "connection is not mutual TLS")
	}
	nodeID, err := s.cfg.PeerNodeID(info.State)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	principal := iam.Principal{NodeID: nodeID}
	if err := s.cfg.Authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionNodeRPC}); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return context.WithValue(ctx, principalKey{}, principal), nil
}

func (s *Server) unaryAuth(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context implements grpc.ServerStream.
func (a authenticatedStream) Context() context.Context { return a.ctx }

func (s *Server) streamAuth(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := s.authenticate(stream.Context())
	if err != nil {
		return err
	}
	return handler(srv, authenticatedStream{ServerStream: stream, ctx: ctx})
}

func caller(ctx context.Context) iam.Principal {
	principal, _ := ctx.Value(principalKey{}).(iam.Principal)
	return principal
}

// Info implements nodev1.NodeServiceServer.
func (s *Server) Info(ctx context.Context, _ *nodev1.InfoRequest) (*nodev1.InfoResponse, error) {
	info, err := s.cfg.Info(ctx)
	if err != nil {
		return &nodev1.InfoResponse{Error: toWire(err)}, nil
	}
	resp := &nodev1.InfoResponse{
		NodeId: info.Node.ID, RpcAddr: info.Node.RPCAddr, RaftAddr: info.Node.RaftAddr,
		Region: info.Node.Labels.Region, Zone: info.Node.Labels.Zone, Rack: info.Node.Labels.Rack, Host: info.Node.Labels.Host,
		Formed: info.Formed, DeploymentId: info.DeploymentID,
	}
	if v := info.Node.MetadataVersion; v > 0 && v <= maxShardIndex {
		resp.MetadataVersion = uint32(v)
	}
	for _, role := range info.Node.Roles {
		resp.Roles = append(resp.Roles, string(role))
	}
	for _, drive := range info.Drives {
		resp.Drives = append(resp.Drives, &nodev1.Drive{Id: drive.ID, Empty: drive.Empty, DeploymentId: drive.DeploymentID})
	}
	return resp, nil
}

// InFlight implements nodev1.NodeServiceServer.
func (s *Server) InFlight(_ context.Context, req *nodev1.InFlightRequest) (*nodev1.InFlightResponse, error) {
	dataID, err := dataIDFromWire(req.GetDataId())
	if err != nil {
		return &nodev1.InFlightResponse{Error: toWire(err)}, nil
	}
	return &nodev1.InFlightResponse{InFlight: s.cfg.InFlight(dataID)}, nil
}

// The answers of a node that cannot give its settings. They carry no code,
// so the node that asked knows only that it was reached and got nothing.
var (
	errNoSettings     = errors.New("this node does not say how it is configured")
	errSettingsFailed = errors.New("this node could not read how it is configured")
)

// Settings implements nodev1.NodeServiceServer. Like every call it has
// passed the authorizer as the node whose certificate the caller showed.
//
// What is sent is what ServerConfig.Settings says, normalized - which
// redacts every setting again, whatever made it - and bounded. A node that
// was given no source answers that it cannot say; that is not the answer of
// a release without the call, which is the RPC failing with UNIMPLEMENTED.
func (s *Server) Settings(ctx context.Context, _ *nodev1.SettingsRequest) (*nodev1.SettingsResponse, error) {
	if s.cfg.Settings == nil {
		return &nodev1.SettingsResponse{Error: toWire(errNoSettings)}, nil
	}
	settings, err := s.cfg.Settings(ctx)
	if err != nil {
		// Why is this node's business, and is in its log: an error of
		// reading a configuration can name the file it was read from.
		if s.cfg.Logger != nil {
			s.cfg.Logger.WarnContext(ctx, "the settings of this node could not be read for another node", slog.Any("error", err))
		}
		return &nodev1.SettingsResponse{Error: &nodev1.Error{Codes: domain.Codes(err), Message: errSettingsFailed.Error()}}, nil
	}
	bounded, dropped := boundSettings(settings.Normalized())
	if dropped > 0 && s.cfg.Logger != nil {
		s.cfg.Logger.WarnContext(ctx, "this node has more settings than are sent to another node; the excess is dropped",
			slog.Int("dropped", dropped))
	}
	return settingsToWire(bounded), nil
}

// DeliveryStatus implements nodev1.NodeServiceServer. The call is in the
// contract before the worker that delivers to targets (PLAN 8.0.14). Until
// that is written this node answers as a release without the call does -
// the RPC fails with UNIMPLEMENTED - so that a node that asks reads "cannot
// be asked" and never an answer that says nothing waits. Like every call it
// has passed the authorizer first.
func (s *Server) DeliveryStatus(context.Context, *nodev1.DeliveryStatusRequest) (*nodev1.DeliveryStatusResponse, error) {
	return nil, status.Error(codes.Unimplemented, "delivery status is not served by this release")
}

// Propose implements nodev1.MetaServiceServer.
func (s *Server) Propose(ctx context.Context, req *nodev1.ProposeRequest) (*nodev1.ProposeResponse, error) {
	// Any node may propose changes to buckets and objects: that is what a
	// gateway does. A node's record, though, is its own: the others find the
	// node by it and ask it about its writes in progress. So is what a node
	// counted of the reading of objects: a batch is numbered by its node,
	// and one sent under another node's name would use up that node's
	// numbers.
	registers, removes, counts, err := s.cfg.Meta.Subject(req.GetEntry())
	if err == nil && registers != "" {
		err = s.cfg.Authz.Authorize(ctx, iam.Request{
			Principal: caller(ctx), Action: iam.ActionRegisterNode, Resource: iam.Resource{Node: registers},
		})
	}
	if err == nil && removes != "" {
		err = s.cfg.Authz.Authorize(ctx, iam.Request{
			Principal: caller(ctx), Action: iam.ActionRemoveNode, Resource: iam.Resource{Node: removes},
		})
	}
	if err == nil && counts != "" {
		err = s.cfg.Authz.Authorize(ctx, iam.Request{
			Principal: caller(ctx), Action: iam.ActionReportReads, Resource: iam.Resource{Node: counts},
		})
	}
	if err != nil {
		return &nodev1.ProposeResponse{Error: toWire(err)}, nil
	}
	result, err := s.cfg.Meta.ProposeLocal(ctx, req.GetEntry())
	if err != nil {
		// The proposal was handed over. An error that does not say what
		// became of it - the caller's patience ending while it waited is
		// one - leaves the entry possibly in the log, and the caller must
		// be told that, whichever of the two notices the deadline first.
		if len(domain.Codes(err)) == 0 {
			err = fmt.Errorf("%w: %w", domain.ErrOutcomeUnknown, err)
		}
		return &nodev1.ProposeResponse{Error: toWire(err)}, nil
	}
	return &nodev1.ProposeResponse{Result: result}, nil
}

// ReadIndex implements nodev1.MetaServiceServer.
func (s *Server) ReadIndex(ctx context.Context, _ *nodev1.ReadIndexRequest) (*nodev1.ReadIndexResponse, error) {
	index, err := s.cfg.Meta.ReadIndexLocal(ctx)
	if err != nil {
		return &nodev1.ReadIndexResponse{Error: toWire(err)}, nil
	}
	return &nodev1.ReadIndexResponse{Index: index}, nil
}

// Term implements nodev1.MetaServiceServer.
func (s *Server) Term(context.Context, *nodev1.TermRequest) (*nodev1.TermResponse, error) {
	term, err := s.cfg.Meta.Term()
	if err != nil {
		return &nodev1.TermResponse{Error: toWire(err)}, nil
	}
	return &nodev1.TermResponse{Term: term}, nil
}

// Join implements nodev1.MetaServiceServer. A node joins as itself only: the
// name it asks to be added under must be the one its certificate carries.
func (s *Server) Join(ctx context.Context, req *nodev1.JoinRequest) (*nodev1.JoinResponse, error) {
	err := s.cfg.Authz.Authorize(ctx, iam.Request{
		Principal: caller(ctx), Action: iam.ActionJoinNode, Resource: iam.Resource{Node: req.GetNodeId()},
	})
	if err == nil {
		err = s.cfg.Meta.Join(ctx, req.GetNodeId(), req.GetRaftAddr(), req.GetVoter())
	}
	return &nodev1.JoinResponse{Error: toWire(err)}, nil
}

// Leave implements nodev1.MetaServiceServer. A node removes itself only.
func (s *Server) Leave(ctx context.Context, req *nodev1.LeaveRequest) (*nodev1.LeaveResponse, error) {
	err := s.cfg.Authz.Authorize(ctx, iam.Request{
		Principal: caller(ctx), Action: iam.ActionRemoveNode, Resource: iam.Resource{Node: req.GetNodeId()},
	})
	if err == nil {
		err = s.cfg.Meta.Leave(ctx, req.GetNodeId())
	}
	return &nodev1.LeaveResponse{Error: toWire(err)}, nil
}

func (s *Server) drive(driveID string) (ports.ShardStore, error) {
	drive, ok := s.cfg.Drive(driveID)
	if !ok {
		return nil, fmt.Errorf("%w: drive %s is not attached to this node", domain.ErrDriveOffline, driveID)
	}
	return drive, nil
}

// WriteShard implements nodev1.StorageServiceServer.
func (s *Server) WriteShard(stream nodev1.StorageService_WriteShardServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return stream.Send(&nodev1.WriteShardResponse{Error: toWire(fmt.Errorf("%w: %w", domain.ErrInvalidArgument, errNoHeader))})
	}
	writer, err := s.openWriter(ctx, header)
	if err != nil {
		return stream.Send(&nodev1.WriteShardResponse{Error: toWire(err)})
	}
	// Whatever happens from here, a shard that was not committed is not
	// left behind. Abort after Commit does nothing.
	defer func() { _ = writer.Abort() }()
	if err := stream.Send(&nodev1.WriteShardResponse{}); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			// The caller went away without committing.
			return nil
		}
		if err != nil {
			return err
		}
		switch step := msg.GetStep().(type) {
		case *nodev1.WriteShardRequest_Chunk:
			if _, err := writer.Write(step.Chunk); err != nil {
				return stream.Send(&nodev1.WriteShardResponse{Error: toWire(err)})
			}
		case *nodev1.WriteShardRequest_Commit:
			// The answer is sent only when the shard is on stable storage.
			return stream.Send(&nodev1.WriteShardResponse{Error: toWire(writer.Commit(ctx))})
		default:
			return stream.Send(&nodev1.WriteShardResponse{
				Error: toWire(fmt.Errorf("%w: unexpected message in a shard stream", domain.ErrInvalidArgument)),
			})
		}
	}
}

func (s *Server) openWriter(ctx context.Context, header *nodev1.WriteShardHeader) (ports.ShardWriter, error) {
	drive, err := s.drive(header.GetDriveId())
	if err != nil {
		return nil, err
	}
	id, err := shardFromWire(header.GetShard())
	if err != nil {
		return nil, err
	}
	if header.GetRewrite() {
		return drive.Rewrite(ctx, id)
	}
	return drive.Create(ctx, id)
}

func (s *Server) openReader(ctx context.Context, driveID string, shard *nodev1.ShardId) (ports.ShardReader, error) {
	drive, err := s.drive(driveID)
	if err != nil {
		return nil, err
	}
	id, err := shardFromWire(shard)
	if err != nil {
		return nil, err
	}
	return drive.Open(ctx, id)
}

// StatShard implements nodev1.StorageServiceServer.
func (s *Server) StatShard(ctx context.Context, req *nodev1.StatShardRequest) (*nodev1.StatShardResponse, error) {
	reader, err := s.openReader(ctx, req.GetDriveId(), req.GetShard())
	if err != nil {
		return &nodev1.StatShardResponse{Error: toWire(err)}, nil
	}
	defer func() { _ = reader.Close() }()
	return &nodev1.StatShardResponse{Size: reader.Size()}, nil
}

// ReadShard implements nodev1.StorageServiceServer. The bytes it returns
// were verified by the drive as they were read; a block that fails is an
// error and none of it is sent.
func (s *Server) ReadShard(ctx context.Context, req *nodev1.ReadShardRequest) (*nodev1.ReadShardResponse, error) {
	if req.GetLength() > maxReadLength || req.GetOffset() < 0 {
		err := fmt.Errorf("%w: read of %d bytes at %d", domain.ErrInvalidArgument, req.GetLength(), req.GetOffset())
		return &nodev1.ReadShardResponse{Error: toWire(err)}, nil
	}
	reader, err := s.openReader(ctx, req.GetDriveId(), req.GetShard())
	if err != nil {
		return &nodev1.ReadShardResponse{Error: toWire(err)}, nil
	}
	defer func() { _ = reader.Close() }()

	length := min(int64(req.GetLength()), max(reader.Size()-req.GetOffset(), 0))
	data := make([]byte, length)
	n, err := reader.ReadAt(data, req.GetOffset())
	if err != nil && !errors.Is(err, io.EOF) {
		return &nodev1.ReadShardResponse{Error: toWire(err)}, nil
	}
	return &nodev1.ReadShardResponse{Data: data[:n], Size: reader.Size()}, nil
}

// DeleteShard implements nodev1.StorageServiceServer.
func (s *Server) DeleteShard(ctx context.Context, req *nodev1.DeleteShardRequest) (*nodev1.DeleteShardResponse, error) {
	drive, err := s.drive(req.GetDriveId())
	if err != nil {
		return &nodev1.DeleteShardResponse{Error: toWire(err)}, nil
	}
	id, err := shardFromWire(req.GetShard())
	if err != nil {
		return &nodev1.DeleteShardResponse{Error: toWire(err)}, nil
	}
	return &nodev1.DeleteShardResponse{Error: toWire(drive.Delete(ctx, id))}, nil
}

// listBatch is the number of shards in one message of a listing.
const listBatch = 512

// ListShards implements nodev1.StorageServiceServer.
func (s *Server) ListShards(req *nodev1.ListShardsRequest, stream nodev1.StorageService_ListShardsServer) error {
	ctx := stream.Context()
	drive, err := s.drive(req.GetDriveId())
	if err != nil {
		return stream.Send(&nodev1.ListShardsResponse{Error: toWire(err)})
	}
	now := s.cfg.Clock.Now()
	batch := &nodev1.ListShardsResponse{}
	err = drive.List(ctx, func(info ports.ShardInfo) error {
		batch.Shards = append(batch.Shards, &nodev1.ShardInfo{
			Shard: shardToWire(info.ID), ModifiedUnixNano: info.ModifiedAt.UnixNano(), AgeNanos: int64(now.Sub(info.ModifiedAt)),
		})
		if len(batch.Shards) < listBatch {
			return nil
		}
		err := stream.Send(batch)
		batch = &nodev1.ListShardsResponse{}
		return err
	})
	if err != nil {
		return stream.Send(&nodev1.ListShardsResponse{Error: toWire(err)})
	}
	if len(batch.Shards) > 0 {
		return stream.Send(batch)
	}
	return nil
}
