package noderpc

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
)

// Methods returns every method of node RPC as gRPC names it,
// "/package.Service/Method": the fixed set that calls are counted by.
func Methods() []string {
	var methods []string
	for _, service := range []grpc.ServiceDesc{
		nodev1.NodeService_ServiceDesc, nodev1.MetaService_ServiceDesc, nodev1.StorageService_ServiceDesc,
	} {
		for _, method := range service.Methods {
			methods = append(methods, "/"+service.ServiceName+"/"+method.MethodName)
		}
		for _, stream := range service.Streams {
			methods = append(methods, "/"+service.ServiceName+"/"+stream.StreamName)
		}
	}
	return methods
}

// Reasons a handshake was refused, as ServerConfig.OnHandshakeRefused is
// told them.
const (
	// RefusedCertificate: the caller showed no certificate, or one the node
	// CA did not issue.
	RefusedCertificate = "certificate"
	// RefusedTimeout: the caller did not finish the handshake in time.
	RefusedTimeout = "timeout"
	// RefusedOther is any other failure: a connection closed half-way, a
	// caller that speaks no TLS 1.3.
	RefusedOther = "other"
)

// observedCredentials is the server's TLS with one addition: a handshake
// that fails is reported. What is accepted, and how, is the wrapped
// credentials' business alone.
type observedCredentials struct {
	credentials.TransportCredentials
	refused func(reason string)
}

// ServerHandshake implements credentials.TransportCredentials.
func (c observedCredentials) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.TransportCredentials.ServerHandshake(raw)
	if err != nil && c.refused != nil {
		c.refused(refusalReason(err))
	}
	return conn, info, err
}

// Clone implements credentials.TransportCredentials.
func (c observedCredentials) Clone() credentials.TransportCredentials {
	return observedCredentials{TransportCredentials: c.TransportCredentials.Clone(), refused: c.refused}
}

// refusalReason sorts the error of a failed handshake into one of the
// Refused reasons. A caller without a certificate is told apart by the text
// of crypto/tls, which has no type for it; should the text change, such a
// refusal is counted as "other" and still counted.
func refusalReason(err error) string {
	var verification *tls.CertificateVerificationError
	var timeout net.Error
	switch {
	case errors.As(err, &verification), strings.Contains(err.Error(), "certificate"):
		return RefusedCertificate
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.As(err, &timeout) && timeout.Timeout():
		return RefusedTimeout
	default:
		return RefusedOther
	}
}
