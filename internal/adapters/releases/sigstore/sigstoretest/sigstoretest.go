// Package sigstoretest signs files the way a release of CactusFS is signed -
// a Sigstore bundle of a message signature, with a Fulcio certificate that
// carries an embedded SCT, a Rekor entry with its inclusion promise and
// proof, and an RFC 3161 timestamp - by a Sigstore of a test, and gives the
// trusted root that believes it. It is for tests only: the unit tests of
// package sigstore and the local release mirror of the integration tests
// (PLAN P.4, P.11).
//
// Rekor and the timestamp authority are sigstore-go's VirtualSigstore
// (pkg/testing/ca). Fulcio and the CT log are this package's, because a
// VirtualSigstore issues certificates without an SCT, and the verifier of
// releases asks for one (verify.WithSignedCertificateTimestamps(1)), as it
// would of a real release. The certificate authorities are made by
// VirtualSigstore's own helpers.
//
// Everything is made for the moment: the timestamp authority's certificate
// of a VirtualSigstore is valid for five minutes from a little before New, so
// a Sigstore signs for about that long.
package sigstoretest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"time"

	ct "github.com/google/certificate-transparency-go"
	cttls "github.com/google/certificate-transparency-go/tls"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	"github.com/google/certificate-transparency-go/x509util"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
)

// BundleMediaType is the media type of the bundles Sign makes: v0.3, as
// cosign writes them.
const BundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// Sigstore is a Sigstore of a test: Fulcio, a CT log, Rekor and a timestamp
// authority, all with keys made by New. It is safe for concurrent use.
type Sigstore struct {
	virtual     *ca.VirtualSigstore
	fulcioRoot  *x509.Certificate
	fulcioInter *x509.Certificate
	fulcioKey   *ecdsa.PrivateKey
	ctKey       *ecdsa.PrivateKey
	ctLogID     [sha256.Size]byte
	rekorLogID  []byte
	created     time.Time
}

// New makes a Sigstore of a test.
func New() (*Sigstore, error) {
	virtual, err := ca.NewVirtualSigstore()
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.New: %w", err)
	}
	fulcioRoot, fulcioRootKey, err := ca.GenerateRootCa()
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.New: Fulcio's root: %w", err)
	}
	fulcioInter, fulcioKey, err := ca.GenerateFulcioIntermediate(fulcioRoot, fulcioRootKey)
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.New: Fulcio's intermediate: %w", err)
	}
	ctKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.New: the CT log's key: %w", err)
	}
	ctDER, err := x509.MarshalPKIXPublicKey(ctKey.Public())
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.New: %w", err)
	}
	rekorLogID, err := virtual.RekorLogID()
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.New: Rekor's log ID: %w", err)
	}
	rawRekorLogID, err := hex.DecodeString(rekorLogID)
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.New: Rekor's log ID: %w", err)
	}
	return &Sigstore{
		virtual:     virtual,
		fulcioRoot:  fulcioRoot,
		fulcioInter: fulcioInter,
		fulcioKey:   fulcioKey,
		ctKey:       ctKey,
		ctLogID:     sha256.Sum256(ctDER),
		rekorLogID:  rawRekorLogID,
		created:     time.Now(),
	}, nil
}

// TrustedRoot returns the trusted_root.json that believes this Sigstore, and
// no other.
func (s *Sigstore) TrustedRoot() ([]byte, error) {
	start := s.created.Add(-time.Hour)
	fulcio := &root.FulcioCertificateAuthority{
		Root:                s.fulcioRoot,
		Intermediates:       []*x509.Certificate{s.fulcioInter},
		ValidityPeriodStart: start,
		URI:                 "https://fulcio.sigstoretest.invalid",
	}
	ctLog := &root.TransparencyLog{
		BaseURL:             "https://ctfe.sigstoretest.invalid",
		ID:                  s.ctLogID[:],
		ValidityPeriodStart: start,
		HashFunc:            crypto.SHA256,
		PublicKey:           s.ctKey.Public(),
		SignatureHashFunc:   crypto.SHA256,
	}
	rekorLogs := map[string]*root.TransparencyLog{}
	for id, log := range s.virtual.RekorLogs() {
		// The ID of a log is its raw key ID: the virtual one holds it as hex.
		copied := *log
		copied.ID = s.rekorLogID
		rekorLogs[id] = &copied
	}
	trusted, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		[]root.CertificateAuthority{fulcio},
		map[string]*root.TransparencyLog{hex.EncodeToString(s.ctLogID[:]): ctLog},
		s.virtual.TimestampingAuthorities(),
		rekorLogs)
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.TrustedRoot: %w", err)
	}
	out, err := trusted.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.TrustedRoot: %w", err)
	}
	return out, nil
}

// Sign returns the bundle (<file>.sigstore.json) of a signature over
// artifact, by a certificate for identity (its URI SAN) issued to an OIDC
// issuer, as `cosign sign-blob --bundle` writes it in a release.
func (s *Sigstore) Sign(identity, issuer string, artifact []byte) ([]byte, error) {
	if _, err := url.Parse(identity); err != nil {
		return nil, fmt.Errorf("sigstoretest.Sign: the identity: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.Sign: %w", err)
	}
	now := time.Now()
	leaf, err := s.issue(identity, issuer, &key.PublicKey, now)
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.Sign: %w", err)
	}
	digest := sha256.Sum256(artifact)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.Sign: %w", err)
	}
	timestamp, err := s.virtual.TimestampResponse(signature)
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.Sign: the timestamp: %w", err)
	}
	entry, err := s.logEntry(leaf, digest[:], signature, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.Sign: the Rekor entry: %w", err)
	}
	pb := &protobundle.Bundle{
		MediaType: BundleMediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{RawBytes: leaf.Raw},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{entry},
			TimestampVerificationData: &protobundle.TimestampVerificationData{
				Rfc3161Timestamps: []*protocommon.RFC3161SignedTimestamp{{SignedTimestamp: timestamp}},
			},
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{
					Algorithm: protocommon.HashAlgorithm_SHA2_256,
					Digest:    digest[:],
				},
				Signature: signature,
			},
		},
	}
	b, err := bundle.NewBundle(pb)
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.Sign: the bundle: %w", err)
	}
	out, err := b.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("sigstoretest.Sign: %w", err)
	}
	return out, nil
}

// issue returns a Fulcio certificate for identity with an embedded SCT of
// this Sigstore's CT log: a precertificate is logged, and the certificate is
// the precertificate without its poison and with the SCT (RFC 6962, 3.1).
func (s *Sigstore) issue(identity, issuer string, public *ecdsa.PublicKey, now time.Time) (*x509.Certificate, error) {
	san, err := url.Parse(identity)
	if err != nil {
		return nil, err
	}
	// The extensions that matter to Certificate Transparency (RFC 6962,
	// section 3) and to Fulcio (the issuer of the identity).
	oidCTPoison := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 3}
	oidCTSCTList := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 2}
	oidOIDCIssuer := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	// The precertificate and the certificate differ in their last
	// extension only.
	template := func(last pkix.Extension) *x509.Certificate {
		return &x509.Certificate{
			SerialNumber: serial,
			NotBefore:    now.Add(-time.Minute),
			NotAfter:     now.Add(10 * time.Minute),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
			URIs:         []*url.URL{san},
			ExtraExtensions: []pkix.Extension{
				{Id: oidOIDCIssuer, Value: []byte(issuer)},
				last,
			},
		}
	}
	poison := pkix.Extension{Id: oidCTPoison, Critical: true, Value: asn1.NullBytes}
	preDER, err := x509.CreateCertificate(rand.Reader, template(poison), s.fulcioInter, public, s.fulcioKey)
	if err != nil {
		return nil, fmt.Errorf("the precertificate: %w", err)
	}
	pre, err := ctx509.ParseCertificate(preDER)
	if err != nil {
		return nil, fmt.Errorf("the precertificate: %w", err)
	}
	issuerCT, err := ctx509.ParseCertificate(s.fulcioInter.Raw)
	if err != nil {
		return nil, fmt.Errorf("the intermediate: %w", err)
	}
	stamp, err := ctTimestamp(now)
	if err != nil {
		return nil, err
	}
	leaf, err := ct.MerkleTreeLeafFromChain([]*ctx509.Certificate{pre, issuerCT}, ct.PrecertLogEntryType, stamp)
	if err != nil {
		return nil, fmt.Errorf("the CT leaf: %w", err)
	}
	sct := ct.SignedCertificateTimestamp{
		SCTVersion: ct.V1,
		LogID:      ct.LogID{KeyID: s.ctLogID},
		Timestamp:  stamp,
	}
	input, err := ct.SerializeSCTSignatureInput(sct, ct.LogEntry{Leaf: *leaf})
	if err != nil {
		return nil, fmt.Errorf("the SCT: %w", err)
	}
	inputDigest := sha256.Sum256(input)
	sctSignature, err := ecdsa.SignASN1(rand.Reader, s.ctKey, inputDigest[:])
	if err != nil {
		return nil, fmt.Errorf("the SCT: %w", err)
	}
	sct.Signature = ct.DigitallySigned{
		Algorithm: cttls.SignatureAndHashAlgorithm{Hash: cttls.SHA256, Signature: cttls.ECDSA},
		Signature: sctSignature,
	}
	list, err := x509util.MarshalSCTsIntoSCTList([]*ct.SignedCertificateTimestamp{&sct})
	if err != nil {
		return nil, fmt.Errorf("the SCT list: %w", err)
	}
	rawList, err := cttls.Marshal(*list)
	if err != nil {
		return nil, fmt.Errorf("the SCT list: %w", err)
	}
	value, err := asn1.Marshal(rawList)
	if err != nil {
		return nil, fmt.Errorf("the SCT list: %w", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template(pkix.Extension{Id: oidCTSCTList, Value: value}), s.fulcioInter, public, s.fulcioKey)
	if err != nil {
		return nil, fmt.Errorf("the certificate: %w", err)
	}
	return x509.ParseCertificate(der)
}

// ctTimestamp is t in milliseconds since 1970, as an SCT holds it.
func ctTimestamp(t time.Time) (uint64, error) {
	ms := t.UnixMilli()
	if ms < 0 {
		return 0, errors.New("a time before 1970")
	}
	return uint64(ms), nil
}

// hashedRekord is the body of a Rekor entry of kind hashedrekord v0.0.1, its
// fields in the order of canonical JSON.
type hashedRekord struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Data struct {
			Hash struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"value"`
			} `json:"hash"`
		} `json:"data"`
		Signature struct {
			Content   string `json:"content"`
			PublicKey struct {
				Content string `json:"content"`
			} `json:"publicKey"`
		} `json:"signature"`
	} `json:"spec"`
}

// logEntry returns the Rekor entry of a signature, with the inclusion
// promise (the signed entry timestamp) and an inclusion proof under a
// checkpoint, both signed by this Sigstore's Rekor.
func (s *Sigstore) logEntry(leaf *x509.Certificate, digest, signature []byte, integratedTime int64) (*protorekor.TransparencyLogEntry, error) {
	var body hashedRekord
	body.APIVersion = "0.0.1"
	body.Kind = "hashedrekord"
	body.Spec.Data.Hash.Algorithm = "sha256"
	body.Spec.Data.Hash.Value = hex.EncodeToString(digest)
	body.Spec.Signature.Content = base64.StdEncoding.EncodeToString(signature)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	body.Spec.Signature.PublicKey.Content = base64.StdEncoding.EncodeToString(certPEM)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	proof, err := s.virtual.GetInclusionProof(raw)
	if err != nil {
		return nil, err
	}
	if proof.LogIndex == nil || proof.TreeSize == nil || proof.RootHash == nil || proof.Checkpoint == nil {
		return nil, errors.New("the inclusion proof is incomplete")
	}
	logIndex := *proof.LogIndex
	set, err := s.virtual.RekorSignPayload(tlog.RekorPayload{
		Body:           base64.StdEncoding.EncodeToString(raw),
		IntegratedTime: integratedTime,
		LogIndex:       logIndex,
		LogID:          hex.EncodeToString(s.rekorLogID),
	})
	if err != nil {
		return nil, err
	}
	rootHash, err := hex.DecodeString(*proof.RootHash)
	if err != nil {
		return nil, err
	}
	hashes := make([][]byte, 0, len(proof.Hashes))
	for _, h := range proof.Hashes {
		decoded, err := hex.DecodeString(h)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, decoded)
	}
	return &protorekor.TransparencyLogEntry{
		LogIndex:         logIndex,
		LogId:            &protocommon.LogId{KeyId: s.rekorLogID},
		KindVersion:      &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"},
		IntegratedTime:   integratedTime,
		InclusionPromise: &protorekor.InclusionPromise{SignedEntryTimestamp: set},
		InclusionProof: &protorekor.InclusionProof{
			LogIndex:   logIndex,
			RootHash:   rootHash,
			TreeSize:   *proof.TreeSize,
			Hashes:     hashes,
			Checkpoint: &protorekor.Checkpoint{Envelope: *proof.Checkpoint},
		},
		CanonicalizedBody: raw,
	}, nil
}
