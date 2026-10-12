package delivery

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// Kind is what a target is.
type Kind string

// The kinds of target.
const (
	// KindWebhook is an HTTPS endpoint that is posted to.
	KindWebhook Kind = "webhook"
	// KindNATS is a NATS server with JetStream.
	KindNATS Kind = "nats"
	// KindS3 is another CactusFS deployment, for replication.
	KindS3 Kind = "s3"
)

// Valid reports whether k is a kind of target there is.
func (k Kind) Valid() bool {
	return k == KindWebhook || k == KindNATS || k == KindS3
}

// scheme returns the scheme the endpoint of a target of this kind has:
// nothing is sent in the clear.
func (k Kind) scheme() string {
	if k == KindNATS {
		return "tls"
	}
	return "https"
}

// AuthMode is how the deployment proves who it is to a target.
type AuthMode string

// The ways of proving it.
const (
	// AuthNone shows nothing. A target of no mode at all is of this one.
	AuthNone AuthMode = "none"
	// AuthBearer sends the secret as "Authorization: Bearer" to a webhook.
	AuthBearer AuthMode = "bearer"
	// AuthBasic gives a NATS server a user, Auth.ID, and the secret as
	// the password.
	AuthBasic AuthMode = "basic"
	// AuthToken gives a NATS server the secret as its token.
	AuthToken AuthMode = "token"
	// AuthSigV4 signs requests to another deployment with the access key
	// Auth.ID, whose secret key is the secret.
	AuthSigV4 AuthMode = "sigv4"
)

// Auth is how the deployment proves who it is to a target, without the
// secret it proves it with.
type Auth struct {
	Mode AuthMode
	// ID is the user of AuthBasic or the access key ID of AuthSigV4. It
	// is no secret.
	ID string
}

// Limits of a target.
const (
	// MaxTargetBuckets is how many names and patterns of buckets a target
	// can list.
	MaxTargetBuckets = 1000
	// MaxCAPEMBytes bounds the certificates a target is verified against.
	MaxCAPEMBytes = 64 << 10
	maxAuthID     = 128
	maxRegion     = 64
	maxSubject    = 255
	maxPattern    = 63
)

// Target is a place outside the deployment that it sends to (ADR-0023). It
// is defined by who administers the deployment and by nobody else; the
// configuration of a bucket names a target and never an address.
//
// Every printed form of a target - with any verb of fmt, in a log, and as
// JSON - is its name and its kind. The endpoint is not part of it, and
// neither is anything of the two secrets, sealed as they are. What may be
// shown of a target to who may read targets is Shown, and nothing else.
//
// One case is not covered, and cannot be: fmt does not call the methods of a
// value it reaches through a field that is not exported, so "%+v" of a
// struct that holds a Target in such a field prints every field of the
// target, endpoint and sealed secrets among them. Keep a target in an
// exported field, or print the target and not what holds it.
type Target struct {
	// Name names the target (ValidateTargetName).
	Name string
	Kind Kind
	// Endpoint is where it is (CheckEndpoint): the whole URL. The path of
	// a webhook may be a credential - the URL of an incoming webhook is
	// one - so no answer, log or status holds it: Origin is what is shown.
	Endpoint string
	// Region is what requests to another deployment are signed for. Kind
	// KindS3 only.
	Region string
	// Subject is what every message is published to. Kind KindNATS only.
	Subject string
	// CAPEM, when not empty, is the certificates the target's own must
	// chain to, in PEM. Certificates are public; no key is part of a
	// target.
	CAPEM []byte
	// Buckets are the buckets whose rules may name the target: names, or
	// patterns with "*". An empty list admits none.
	Buckets []string
	// Audit says that every record of the audit trail is sent to the
	// target as well (ADR-0025).
	Audit bool
	Auth  Auth
	// Secret is the bearer token, the password, the token or the secret
	// key, sealed through the KMS with SecretAAD. The zero value is no
	// secret.
	Secret iam.SealedSecret
	// SignKey is the key requests to a webhook are signed with, sealed
	// with SignKeyAAD. Kind KindWebhook only.
	SignKey iam.SealedSecret
	// Refs is how many bucket configurations name the target. The store
	// counts it; a target with any cannot be deleted.
	Refs      int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SecretAAD returns the additional data the secret of the target of that
// name is sealed with, so that a sealed secret moved to another target or to
// another field does not open.
func SecretAAD(name string) []byte {
	return []byte("target/" + name + "/secret")
}

// SignKeyAAD returns the additional data the signing key of the target of
// that name is sealed with.
func SignKeyAAD(name string) []byte {
	return []byte("target/" + name + "/sign_key")
}

func sealed(s iam.SealedSecret) bool {
	return len(s.Ciphertext) > 0 || len(s.WrappedKey) > 0 || len(s.Nonce) > 0 || s.KMSKeyID != ""
}

// SecretSet reports whether the target has a secret. It is all that is ever
// told of it.
func (t Target) SecretSet() bool { return sealed(t.Secret) }

// SignKeySet reports whether the target has a key that requests to it are
// signed with.
func (t Target) SignKeySet() bool { return sealed(t.SignKey) }

// String returns the name and the kind of the target, and nothing else.
func (t Target) String() string {
	return "target " + t.Name + " (" + string(t.Kind) + ")"
}

// Format implements fmt.Formatter, so that no verb prints more of a target
// than String does: "%+v" and "%#v" would otherwise print its endpoint and
// its sealed secrets.
func (t Target) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(t.String()))
}

// LogValue implements slog.LogValuer: a target in a log is its name and its
// kind.
func (t Target) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", t.Name), slog.String("kind", string(t.Kind)))
}

// MarshalJSON implements json.Marshaler: a target as JSON is its name and
// its kind, like every other printed form of it. The stored record of a
// target is a type of the store's own, and an answer of the API is made from
// Shown.
func (t Target) MarshalJSON() ([]byte, error) {
	out := make([]byte, 0, len(t.Name)+len(t.Kind)+24)
	out = append(out, `{"name":`...)
	out = appendJSONString(out, t.Name)
	out = append(out, `,"kind":`...)
	out = appendJSONString(out, string(t.Kind))
	return append(out, '}'), nil
}

// appendJSONString appends s as a string of JSON: the quotation mark and the
// backslash escaped, the control characters as \u00XX, and a byte that is
// no UTF-8 as U+FFFD, so that what is written is always JSON.
func appendJSONString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			dst = utf8.AppendRune(append(dst, '\\'), r)
		case r < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hex[r>>4], hex[r&0x0f])
		default:
			dst = utf8.AppendRune(dst, r)
		}
	}
	return append(dst, '"')
}

// Clone returns a copy that shares nothing with the target.
func (t Target) Clone() Target {
	t.CAPEM = slices.Clone(t.CAPEM)
	t.Buckets = slices.Clone(t.Buckets)
	for _, s := range []*iam.SealedSecret{&t.Secret, &t.SignKey} {
		s.WrappedKey, s.Nonce, s.Ciphertext = slices.Clone(s.WrappedKey), slices.Clone(s.Nonce), slices.Clone(s.Ciphertext)
	}
	return t
}

// Origin returns the scheme, the host and the port of the endpoint, as they
// were given: everything before the path. It is what is shown of where a
// target is.
//
// It cuts by bytes and trusts no parser: whatever follows the first slash
// after "://" is left out, so that nothing of a path is ever returned,
// whatever the endpoint holds.
func (t Target) Origin() string {
	origin, _ := cutEndpoint(t.Endpoint)
	return origin
}

// PathSet reports whether the endpoint has a path - anything after the host
// and the port but a single slash. It is all that is ever told of the path.
func (t Target) PathSet() bool {
	_, path := cutEndpoint(t.Endpoint)
	return path != "" && path != "/"
}

// cutEndpoint cuts an endpoint into what is before its path and the path.
// An endpoint without "://" has no origin that can be shown: all of it is
// taken for path.
func cutEndpoint(endpoint string) (origin, path string) {
	scheme := strings.Index(endpoint, "://")
	if scheme < 0 {
		return "", endpoint
	}
	authority := endpoint[scheme+3:]
	if slash := strings.IndexByte(authority, '/'); slash >= 0 {
		return endpoint[:scheme+3+slash], authority[slash:]
	}
	return endpoint, ""
}

// ShownTarget is a target as it may be shown to who may read targets
// (Target.Shown). It has no field that could hold a secret, a sealed secret
// or the path of the endpoint, so that whatever is made from it - an answer
// of the API, a line of a log - cannot hold one by accident.
type ShownTarget struct {
	Name string
	Kind Kind
	// Origin is the scheme, the host and the port of the endpoint
	// (Target.Origin).
	Origin string
	// PathSet says that the endpoint has a path, which is not shown.
	PathSet bool
	Region  string
	Subject string
	// CAPEM is the certificates the target is verified against, which are
	// public.
	CAPEM   []byte
	Buckets []string
	Audit   bool
	Auth    Auth
	// SecretSet and SignKeySet say that the target has a secret, and a
	// key that requests to it are signed with. Nothing else is told of
	// either.
	SecretSet  bool
	SignKeySet bool
	Refs       int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Shown returns the target as it may be shown: without its secrets, of which
// it says only whether they are set, and without the path of its endpoint,
// of which it says only whether there is one. It shares nothing with t.
func (t Target) Shown() ShownTarget {
	return ShownTarget{
		Name: t.Name, Kind: t.Kind, Origin: t.Origin(), PathSet: t.PathSet(),
		Region: t.Region, Subject: t.Subject, CAPEM: slices.Clone(t.CAPEM), Buckets: slices.Clone(t.Buckets),
		Audit: t.Audit, Auth: t.Auth, SecretSet: t.SecretSet(), SignKeySet: t.SignKeySet(),
		Refs: t.Refs, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func invalidTarget(format string, args ...any) error {
	return fmt.Errorf("%w: target: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// printable reports whether s is of printable ASCII without spaces.
func printable(s string) bool {
	for i := range len(s) {
		if c := s[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// validPattern reports whether p can name buckets: 1 to 63 of the characters
// of a bucket name, and "*".
func validPattern(p string) bool {
	if p == "" || len(p) > maxPattern {
		return false
	}
	for i := range len(p) {
		c := p[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '-' && c != '*' {
			return false
		}
	}
	return true
}

// Validate reports whether the target can be kept. The error wraps
// domain.ErrInvalidArgument, and repeats neither the endpoint nor anything
// that may be part of a credential.
//
// It is part of the replicated format: the state machine of every member
// refuses a target by it, and members are built with different releases of
// Go. So it decides by rules of bytes that are written out in this package -
// lengths, sets of characters, prefixes - and calls no clock, no resolver,
// and no parser of the standard library whose answer a release may change:
// not net/url, not encoding/pem, not encoding/base64, not crypto/x509. Of
// the endpoint it asks the scheme of the kind and what no endpoint may hold;
// of the certificates their length and their framing (checkCAFraming).
// Whether the endpoint is one that can be connected to is CheckEndpoint's to
// say, and whether the certificates are certificates is CheckCA's: both are
// asked by the use case before a target is proposed, and the address is
// judged again at every connect. Apply judges bytes; the use case judges
// what they mean. So a target that Validate takes may hold certificates
// CheckCA would refuse - one that no use case proposed - and such a target
// is kept by every member alike and fails when it is connected to.
func (t Target) Validate() error {
	if err := ValidateTargetName(t.Name); err != nil {
		return err
	}
	if !t.Kind.Valid() {
		return invalidTarget("a target is of kind webhook, nats or s3")
	}
	if t.Endpoint == "" || len(t.Endpoint) > MaxEndpointLength || !printable(t.Endpoint) ||
		!strings.HasPrefix(t.Endpoint, t.Kind.scheme()+"://") || strings.ContainsAny(t.Endpoint, "@?#\\") {
		return invalidTarget("the endpoint of a %s target begins with %s:// and has no user, query or fragment", t.Kind, t.Kind.scheme())
	}
	if t.Refs < 0 {
		return invalidTarget("a number of references below none")
	}
	if len(t.Buckets) > MaxTargetBuckets {
		return invalidTarget("more than %d buckets", MaxTargetBuckets)
	}
	for i, pattern := range t.Buckets {
		if !validPattern(pattern) || slices.Contains(t.Buckets[:i], pattern) {
			return invalidTarget("bucket %d of the list is no name or pattern of a bucket, or is there twice", i)
		}
	}
	if _, err := checkCAFraming(t.CAPEM); err != nil {
		return err
	}
	if len(t.Auth.ID) > maxAuthID || !printable(t.Auth.ID) {
		return invalidTarget("the user or access key is at most %d printable characters", maxAuthID)
	}
	mode := t.Auth.Mode
	if mode == "" {
		mode = AuthNone
	}
	var modes []AuthMode
	switch t.Kind {
	case KindWebhook:
		modes = []AuthMode{AuthNone, AuthBearer}
		if t.Region != "" || t.Subject != "" {
			return invalidTarget("a webhook has neither a region nor a subject")
		}
	case KindNATS:
		modes = []AuthMode{AuthNone, AuthToken, AuthBasic}
		if t.Region != "" {
			return invalidTarget("a NATS server has no region")
		}
		if !validSubject(t.Subject) {
			return invalidTarget("a NATS server needs a subject without wildcards, of at most %d bytes", maxSubject)
		}
	case KindS3:
		modes = []AuthMode{AuthSigV4}
		if t.Subject != "" || t.Audit {
			return invalidTarget("another deployment has no subject and is not sent the audit trail")
		}
		if t.Region == "" || len(t.Region) > maxRegion || !validPattern(t.Region) || strings.ContainsAny(t.Region, "*.") {
			return invalidTarget("another deployment needs a region of at most %d lower-case letters, digits and hyphens", maxRegion)
		}
	}
	if !slices.Contains(modes, mode) {
		return invalidTarget("a %s target does not prove itself that way", t.Kind)
	}
	switch {
	case mode == AuthNone && t.SecretSet():
		return invalidTarget("a secret without a way to use it")
	case mode != AuthNone && !t.SecretSet():
		return invalidTarget("the way it proves itself needs a secret")
	case (mode == AuthBasic || mode == AuthSigV4) != (t.Auth.ID != ""):
		return invalidTarget("a user or an access key is given where there is a use for one, and only there")
	case mode == AuthSigV4 && iam.ValidateAccessKeyID(t.Auth.ID) != nil:
		return invalidTarget("the access key is not one")
	case t.Kind != KindWebhook && t.SignKeySet():
		return invalidTarget("only requests to a webhook are signed with a key of the target")
	}
	return nil
}

// validSubject reports whether s is a NATS subject a message can be
// published to: tokens separated by dots, none empty, no wildcard.
func validSubject(s string) bool {
	if s == "" || len(s) > maxSubject || !printable(s) || strings.ContainsAny(s, "*>") {
		return false
	}
	for token := range strings.SplitSeq(s, ".") {
		if token == "" {
			return false
		}
	}
	return true
}

// The two lines that frame a certificate in PEM (RFC 7468).
const (
	pemBeginCertificate = "-----BEGIN CERTIFICATE-----"
	pemEndCertificate   = "-----END CERTIFICATE-----"
	// pemLineLength is the longest line of the body of a block.
	pemLineLength = 64
)

// base64Character reports whether c is of the alphabet of base64, without
// the padding.
func base64Character(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/'
}

// checkCAFraming reports whether data is framed as certificates in PEM and
// as nothing else - above all, as no key - and returns how many blocks there
// are. It is a rule of bytes, written out here, and asks no parser:
//
//   - at most MaxCAPEMBytes; no data is no certificates, and valid;
//   - lines end with a line feed, or a carriage return and a line feed; the
//     last may end with neither;
//   - outside a block there are only empty lines;
//   - a block is the line "-----BEGIN CERTIFICATE-----", then one or more
//     lines of 1 to 64 characters of the alphabet of base64, then the line
//     "-----END CERTIFICATE-----". There is no header, no space and no
//     other label;
//   - "=" pads the last line of the body only, once or twice at its end,
//     and the body has a multiple of four characters in all.
//
// What the bytes between the two lines are - whether they are a certificate
// at all - it does not ask: that is CheckCA's.
func checkCAFraming(data []byte) (blocks int, err error) {
	if len(data) > MaxCAPEMBytes {
		return 0, invalidTarget("more than %d bytes of certificates", MaxCAPEMBytes)
	}
	refused := invalidTarget("the certificates are PEM blocks of certificates, and nothing else")
	inside, padded, body := false, false, 0
	for rest := string(data); rest != ""; {
		line := rest
		if end := strings.IndexByte(rest, '\n'); end >= 0 {
			line, rest = rest[:end], rest[end+1:]
		} else {
			rest = ""
		}
		line = strings.TrimSuffix(line, "\r")
		switch {
		case !inside && line == "":
		case !inside && line == pemBeginCertificate:
			inside, padded, body = true, false, 0
		case !inside:
			return 0, refused
		case line == pemEndCertificate:
			if body == 0 || body%4 != 0 {
				return 0, refused
			}
			inside = false
			blocks++
		case padded || line == "" || len(line) > pemLineLength:
			return 0, refused
		default:
			text := strings.TrimRight(line, "=")
			if len(line)-len(text) > 2 {
				return 0, refused
			}
			for i := range len(text) {
				if !base64Character(text[i]) {
					return 0, refused
				}
			}
			padded = len(text) != len(line)
			body += len(line)
		}
	}
	if inside {
		return 0, refused
	}
	return blocks, nil
}

// CheckCA reports whether data is what a target can be verified against: PEM
// blocks of certificates that can be read, and nothing else. Empty data is
// valid and means the roots of the system. The error wraps
// domain.ErrInvalidArgument.
//
// It is for the use case that defines a target. The state machine does not
// ask it: what encoding/pem and crypto/x509 take may differ from one release
// of Go to the next, and what one member takes every member must take.
// Whatever CheckCA takes, Target.Validate takes too.
func CheckCA(data []byte) error {
	blocks, err := checkCAFraming(data)
	if err != nil {
		return err
	}
	rest := data
	for range blocks {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) > 0 {
			return invalidTarget("a certificate cannot be read")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return invalidTarget("a certificate cannot be read")
		}
	}
	// Every block the framing counted was read, and there is no other.
	if block, _ := pem.Decode(rest); block != nil {
		return invalidTarget("a certificate cannot be read")
	}
	return nil
}

// Admits reports whether the rules of the bucket named may name the target:
// whether one of Buckets is its name, or a pattern its name fits, in which
// "*" stands for any run of characters.
func (t Target) Admits(bucketName string) bool {
	for _, pattern := range t.Buckets {
		if globMatch(pattern, bucketName) {
			return true
		}
	}
	return false
}

// Serves reports whether a target of this kind can be the target of a
// consumer of that kind: a webhook and a NATS server are told of changes,
// another deployment is sent versions.
func (k Kind) Serves(consumer ConsumerKind) bool {
	switch consumer {
	case ConsumerNotification:
		return k == KindWebhook || k == KindNATS
	case ConsumerReplication:
		return k == KindS3
	}
	return false
}

// globMatch reports whether s fits pattern, in which '*' stands for any run
// of characters. It never backtracks further than the last star.
func globMatch(pattern, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case p < len(pattern) && pattern[p] == s[i]:
			p++
			i++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
