package nodetls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// Authority is a node CA that can issue node certificates. It exists for
// development and tests: "cactusfs dev-init" makes a throw-away one. A
// production cluster brings certificates from its own PKI.
type Authority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	entropy     io.Reader
	// CertPEM is the CA certificate, which every node needs.
	CertPEM []byte
}

const serialBits = 127

// NewAuthority creates a node CA valid from now for the given time. entropy
// must be cryptographically secure.
func NewAuthority(entropy io.Reader, now time.Time, validity time.Duration) (*Authority, error) {
	return NewNamedAuthority(entropy, now, validity, "CactusFS node CA (development)")
}

// NewNamedAuthority creates a CA with the given common name. Development
// setups use a second one, apart from the node CA, for the certificate the
// S3 and admin listeners show to clients: the node CA must never sign
// anything but node certificates, because whatever it signs is a node.
func NewNamedAuthority(entropy io.Reader, now time.Time, validity time.Duration, name string) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), entropy)
	if err != nil {
		return nil, fmt.Errorf("nodetls.NewAuthority: %w", err)
	}
	serial, err := newSerial(entropy)
	if err != nil {
		return nil, fmt.Errorf("nodetls.NewAuthority: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(entropy, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("nodetls.NewAuthority: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("nodetls.NewAuthority: %w", err)
	}
	return &Authority{
		certificate: certificate, key: key, entropy: entropy,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

func newSerial(entropy io.Reader) (*big.Int, error) {
	raw := make([]byte, serialBits/8+1)
	if _, err := io.ReadFull(entropy, raw); err != nil {
		return nil, err
	}
	raw[0] &= 0x7F
	return new(big.Int).SetBytes(raw), nil
}

// Issue returns a certificate and key, in PEM, that name the node with the
// given ID. The certificate is good for both ends of a connection and
// expires with the CA.
func (a *Authority) Issue(nodeID string, now time.Time) (certPEM, keyPEM []byte, err error) {
	if err := cluster.ValidateNodeID(nodeID); err != nil {
		return nil, nil, fmt.Errorf("nodetls.Issue: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), a.entropy)
	if err != nil {
		return nil, nil, fmt.Errorf("nodetls.Issue: %w", err)
	}
	serial, err := newSerial(a.entropy)
	if err != nil {
		return nil, nil, fmt.Errorf("nodetls.Issue: %w", err)
	}
	if !now.Before(a.certificate.NotAfter) {
		return nil, nil, fmt.Errorf("%w: the node CA has expired", domain.ErrInvalidArgument)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID},
		DNSNames:     []string{nodeID},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     a.certificate.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(a.entropy, template, a.certificate, &key.PublicKey, a.key)
	if err != nil {
		return nil, nil, fmt.Errorf("nodetls.Issue: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("nodetls.Issue: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// IssueServer returns a certificate and key, in PEM, for a server that
// clients reach under the given DNS names and IP addresses. It is good for
// serving only; it is not a node certificate.
func (a *Authority) IssueServer(dnsNames []string, ips []net.IP, now time.Time) (certPEM, keyPEM []byte, err error) {
	if len(dnsNames)+len(ips) == 0 {
		return nil, nil, fmt.Errorf("nodetls.IssueServer: %w: no name to issue for", domain.ErrInvalidArgument)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), a.entropy)
	if err != nil {
		return nil, nil, fmt.Errorf("nodetls.IssueServer: %w", err)
	}
	serial, err := newSerial(a.entropy)
	if err != nil {
		return nil, nil, fmt.Errorf("nodetls.IssueServer: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "CactusFS (development)"},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     a.certificate.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(a.entropy, template, a.certificate, &key.PublicKey, a.key)
	if err != nil {
		return nil, nil, fmt.Errorf("nodetls.IssueServer: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("nodetls.IssueServer: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}
