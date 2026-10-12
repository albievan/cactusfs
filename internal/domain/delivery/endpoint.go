package delivery

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// MaxEndpointLength bounds the endpoint of a target, in bytes.
const MaxEndpointLength = 2048

// The ports an endpoint means when it names none.
const (
	defaultHTTPSPort = 443
	defaultNATSPort  = 4222
)

// Endpoint is the endpoint of a target, taken apart.
//
// Its path may be a credential - the URL of an incoming webhook is one - so
// every printed form of an endpoint, with any verb of fmt and in a log, is
// its Origin. The path is read from the field by what connects, and by
// nothing else. As with Target, a value reached through a field that is not
// exported is printed by fmt without being asked.
type Endpoint struct {
	// Scheme is "https" or "tls".
	Scheme string
	// Host is a DNS name, or an IP address without brackets.
	Host string
	// Port is the port the endpoint names, or the one its scheme means.
	Port uint16
	// Path is the path of a webhook; empty for the other kinds. It is
	// never shown.
	Path string
}

// Address returns the endpoint as a dialer is given it: "host:port".
func (e Endpoint) Address() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(int(e.Port)))
}

// Origin returns the scheme, the host and the port of the endpoint - always
// with the port, also the one its scheme means - and nothing of its path.
func (e Endpoint) Origin() string {
	return e.Scheme + "://" + e.Address()
}

// PathSet reports whether the endpoint has a path other than a single
// slash. It is all that is ever told of the path.
func (e Endpoint) PathSet() bool {
	return e.Path != "" && e.Path != "/"
}

// String returns the Origin of the endpoint.
func (e Endpoint) String() string {
	return e.Origin()
}

// Format implements fmt.Formatter, so that no verb prints more of an
// endpoint than String does: "%+v" and "%#v" would otherwise print its path.
func (e Endpoint) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(e.Origin()))
}

// LogValue implements slog.LogValuer: an endpoint in a log is its Origin.
func (e Endpoint) LogValue() slog.Value {
	return slog.StringValue(e.Origin())
}

// MarshalJSON implements json.Marshaler: an endpoint as JSON is its Origin,
// as text.
func (e Endpoint) MarshalJSON() ([]byte, error) {
	return appendJSONString(nil, e.Origin()), nil
}

func invalidEndpoint(reason string) error {
	return fmt.Errorf("%w: endpoint: %s", domain.ErrInvalidArgument, reason)
}

// neverAddresses are addresses nothing admits, whatever the operator allows:
// where the instance metadata service of a cloud answers. Whoever reaches
// one reads the credentials of the machine.
//
// The list is best effort. It holds the addresses that are known today; a
// cloud that answers elsewhere is not in it, and the operator's own network
// policy is what stands behind it. Three of the four are link-local or
// private addresses that nothing else would refuse once a range around them
// is allowed, which is why they are named one by one.
func neverAddresses() []netip.Addr {
	return []netip.Addr{
		// AWS, Azure, Google Cloud, Oracle Cloud, DigitalOcean, OpenStack.
		netip.AddrFrom4([4]byte{169, 254, 169, 254}),
		// AWS, over IPv6: fd00:ec2::254.
		netip.AddrFrom16([16]byte{0xfd, 0x00, 0x0e, 0xc2, 14: 0x02, 15: 0x54}),
		// Google Cloud, over IPv6: fd20:ce::254.
		netip.AddrFrom16([16]byte{0xfd, 0x20, 0x00, 0xce, 14: 0x02, 15: 0x54}),
		// Alibaba Cloud: 100.100.100.200.
		netip.AddrFrom4([4]byte{100, 100, 100, 200}),
	}
}

// nat64 is the well-known prefix under which an IPv6 address stands for an
// IPv4 one (RFC 6052): the last four bytes are that address.
func nat64() netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom16([16]byte{0x00, 0x64, 0xff, 0x9b}), 96)
}

// A translated range is a range of IPv6 addresses that stand for IPv4
// addresses in a way a gateway, not this node, decides.
type translated struct {
	prefix netip.Prefix
	// embedded returns the IPv4 address an address of the range stands
	// for, where the range fixes it.
	embedded func(raw [16]byte) (netip.Addr, bool)
}

// translatedRanges are the ranges of IPv6 that reach IPv4 addresses by a
// rule other than the well-known NAT64 prefix. An address in one is refused
// outright - what it reaches cannot be judged from here - unless a prefix of
// the allow list names it.
func translatedRanges() []translated {
	last := func(raw [16]byte) (netip.Addr, bool) { return netip.AddrFrom4([4]byte(raw[12:])), true }
	return []translated{
		// IPv4-compatible addresses, ::a.b.c.d (RFC 4291, deprecated).
		// "::1" is of this range and is loopback; "::" is the unspecified
		// address. Both are judged as what they are, before this.
		{netip.PrefixFrom(netip.IPv6Unspecified(), 96), last},
		// The local-use NAT64 prefix 64:ff9b:1::/48 (RFC 8215): where in
		// an address the IPv4 one is depends on the prefix a network
		// carved from it.
		{netip.PrefixFrom(netip.AddrFrom16([16]byte{0x00, 0x64, 0xff, 0x9b, 0x00, 0x01}), 48), nil},
		// 6to4, 2002:a.b.c.d::/48 (RFC 3056).
		{netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20, 0x02}), 16), func(raw [16]byte) (netip.Addr, bool) {
			return netip.AddrFrom4([4]byte(raw[2:6])), true
		}},
	}
}

// never reports whether addr is one nothing admits: an address that is
// none, one of neverAddresses, the unspecified address and the rest of
// 0.0.0.0/8, multicast, or the IPv4 broadcast address. addr has no zone and
// is not an IPv4 address written as IPv6.
func never(addr netip.Addr) bool {
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsMulticast() || slices.Contains(neverAddresses(), addr) {
		return true
	}
	if addr.Is4() {
		raw := addr.As4()
		return raw[0] == 0 || raw == [4]byte{255, 255, 255, 255}
	}
	return false
}

// allowed reports whether a prefix of the allow list names addr.
func allowed(addr netip.Addr, allow []netip.Prefix) bool {
	for _, prefix := range allow {
		if prefix.IsValid() && prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// AllowedAddress reports whether the deployment may connect to addr on
// behalf of a target (ADR-0023). It is asked of the address a name resolved
// to, at every connect - not of the name, and not once.
//
//   - Never: an address that is none; the unspecified address and the rest
//     of 0.0.0.0/8; multicast and the IPv4 broadcast address; and the
//     addresses where the metadata service of a cloud answers -
//     169.254.169.254, fd00:ec2::254, fd20:ce::254 and 100.100.100.200 -
//     whatever allow holds. That list is best effort (neverAddresses).
//   - Only where allow names it: loopback and link-local. allow is the
//     operator's setting, for a receiver on the node itself. A range that is
//     allowed is open with everything in it but the addresses above: to
//     allow 169.254.0.0/16 is to allow whatever else answers there.
//   - Only where allow names it, too: an IPv6 address that stands for an
//     IPv4 one by a rule a gateway decides - IPv4-compatible (::/96), the
//     local-use NAT64 prefix 64:ff9b:1::/48, and 6to4 (2002::/16). They are
//     refused outright otherwise; and where the range fixes which IPv4
//     address is meant (::/96, 2002::/16), one that is never connected to is
//     not connected to this way either.
//   - Everything else is allowed, private ranges among it: on premises that
//     is where targets are.
//
// An IPv4 address written as IPv6 - mapped, or under the well-known NAT64
// prefix 64:ff9b::/96 - is judged as the IPv4 address it reaches, so a range
// of IPv4 addresses is admitted by an IPv4 prefix and by no other; a zone is
// not looked at.
func AllowedAddress(addr netip.Addr, allow []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() && nat64().Contains(addr) {
		raw := addr.As16()
		addr = netip.AddrFrom4([4]byte(raw[12:]))
	}
	if never(addr) {
		return false
	}
	if addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return allowed(addr, allow)
	}
	if addr.Is6() {
		for _, r := range translatedRanges() {
			if !r.prefix.Contains(addr) {
				continue
			}
			if r.embedded != nil {
				if inner, ok := r.embedded(addr.As16()); ok && never(inner) {
					return false
				}
			}
			return allowed(addr, allow)
		}
	}
	return true
}

// everywhere is an allow list that admits whatever an allow list can admit.
func everywhere() []netip.Prefix {
	return []netip.Prefix{netip.PrefixFrom(netip.IPv4Unspecified(), 0), netip.PrefixFrom(netip.IPv6Unspecified(), 0)}
}

// hostName reports whether host can be a DNS name: labels of letters, digits,
// hyphens and underscores, none empty, at most 253 bytes in all - and not a
// name that ends in a number, which some resolvers read as an IPv4 address.
func hostName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	last := host
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := range len(label) {
			c := label[i]
			letterOrDigit := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !letterOrDigit && c != '-' && c != '_' {
				return false
			}
		}
		last = label
	}
	if _, err := strconv.ParseUint(last, 0, 64); err == nil {
		return false
	}
	return true
}

// CheckEndpoint reports whether endpoint can be the endpoint of a target of
// the kind given, and returns it taken apart.
//
//   - "https://" for a webhook and for another deployment, "tls://" for a
//     NATS server: nothing is sent in the clear.
//   - No userinfo, no query and no fragment: a credential belongs in the
//     target's sealed secret, never in an address that is shown and logged.
//   - A host that is a DNS name or an IP address. An address nothing admits
//     (AllowedAddress) is refused here already; whether any other address
//     may be connected to is decided when it is, of the address the name
//     resolved to.
//   - A path only for a webhook: another deployment is addressed
//     path-style from its root, and a NATS server has none.
//
// The error wraps domain.ErrInvalidArgument. It says what is wrong and never
// repeats the endpoint or any part of it: what was typed may hold a
// credential.
func CheckEndpoint(kind Kind, endpoint string) (Endpoint, error) {
	if !kind.Valid() {
		return Endpoint{}, invalidEndpoint("a target is of kind webhook, nats or s3")
	}
	scheme, port := kind.scheme(), uint16(defaultHTTPSPort)
	if kind == KindNATS {
		port = defaultNATSPort
	}
	if endpoint == "" || len(endpoint) > MaxEndpointLength {
		return Endpoint{}, invalidEndpoint(fmt.Sprintf("needs 1 to %d bytes", MaxEndpointLength))
	}
	for i := range len(endpoint) {
		if c := endpoint[i]; c <= ' ' || c > '~' || c == '\\' {
			return Endpoint{}, invalidEndpoint("only printable ASCII without spaces and backslashes")
		}
	}
	if !strings.HasPrefix(endpoint, scheme+"://") {
		return Endpoint{}, invalidEndpoint("a " + string(kind) + " target begins with " + scheme + "://")
	}
	// The error of the parser repeats what it was given, and is not passed
	// on.
	u, err := url.Parse(endpoint)
	if err != nil || u.Opaque != "" {
		return Endpoint{}, invalidEndpoint("not a URL")
	}
	switch {
	case u.User != nil || strings.Contains(endpoint, "@"):
		return Endpoint{}, invalidEndpoint("no user and no password: a credential is the target's secret")
	case u.RawQuery != "" || u.ForceQuery || strings.Contains(endpoint, "?"):
		return Endpoint{}, invalidEndpoint("no query")
	case u.Fragment != "" || strings.Contains(endpoint, "#"):
		return Endpoint{}, invalidEndpoint("no fragment")
	case strings.Contains(u.Host, "%"):
		return Endpoint{}, invalidEndpoint("no zone and nothing escaped in the host")
	}
	out := Endpoint{Scheme: scheme, Host: u.Hostname(), Port: port, Path: u.EscapedPath()}
	if text := u.Port(); text != "" {
		number, err := strconv.ParseUint(text, 10, 16)
		if err != nil || number == 0 || number > math.MaxUint16 {
			return Endpoint{}, invalidEndpoint("a port from 1 to 65535")
		}
		out.Port = uint16(number)
	} else if strings.HasSuffix(u.Host, ":") {
		return Endpoint{}, invalidEndpoint("a port from 1 to 65535")
	}
	if addr, err := netip.ParseAddr(out.Host); err == nil {
		if !AllowedAddress(addr, everywhere()) {
			return Endpoint{}, invalidEndpoint("an address the deployment never connects to")
		}
		out.Host = addr.String()
	} else if !hostName(out.Host) {
		return Endpoint{}, invalidEndpoint("the host is a DNS name or an IP address")
	}
	switch {
	case kind == KindWebhook:
	case kind == KindS3 && (out.Path == "" || out.Path == "/"):
		out.Path = ""
	case kind == KindNATS && out.Path == "":
	default:
		return Endpoint{}, invalidEndpoint("a " + string(kind) + " target has no path")
	}
	return out, nil
}
