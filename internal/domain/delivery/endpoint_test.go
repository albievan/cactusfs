package delivery_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
)

func TestCheckEndpoint(t *testing.T) {
	t.Parallel()
	accepted := []struct {
		kind     delivery.Kind
		endpoint string
		want     delivery.Endpoint
		address  string
	}{
		{delivery.KindWebhook, "https://hooks.example.com", delivery.Endpoint{Scheme: "https", Host: "hooks.example.com", Port: 443}, "hooks.example.com:443"},
		{delivery.KindWebhook, "https://hooks.example.com:8443/in/cactus", delivery.Endpoint{Scheme: "https", Host: "hooks.example.com", Port: 8443, Path: "/in/cactus"}, "hooks.example.com:8443"},
		{delivery.KindWebhook, "https://receiver/", delivery.Endpoint{Scheme: "https", Host: "receiver", Port: 443, Path: "/"}, "receiver:443"},
		{delivery.KindWebhook, "https://10.1.2.3/hook", delivery.Endpoint{Scheme: "https", Host: "10.1.2.3", Port: 443, Path: "/hook"}, "10.1.2.3:443"},
		{delivery.KindWebhook, "https://[2001:db8::1]:9443/hook", delivery.Endpoint{Scheme: "https", Host: "2001:db8::1", Port: 9443, Path: "/hook"}, "[2001:db8::1]:9443"},
		{delivery.KindWebhook, "https://my_service.internal/a%20b", delivery.Endpoint{Scheme: "https", Host: "my_service.internal", Port: 443, Path: "/a%20b"}, "my_service.internal:443"},
		// Loopback is an address a setting may admit: it is judged when it
		// is connected to, not here.
		{delivery.KindWebhook, "https://127.0.0.1:9999/hook", delivery.Endpoint{Scheme: "https", Host: "127.0.0.1", Port: 9999, Path: "/hook"}, "127.0.0.1:9999"},
		{delivery.KindWebhook, "https://localhost/hook", delivery.Endpoint{Scheme: "https", Host: "localhost", Port: 443, Path: "/hook"}, "localhost:443"},
		{delivery.KindNATS, "tls://nats.example.com", delivery.Endpoint{Scheme: "tls", Host: "nats.example.com", Port: 4222}, "nats.example.com:4222"},
		{delivery.KindNATS, "tls://nats.example.com:4443", delivery.Endpoint{Scheme: "tls", Host: "nats.example.com", Port: 4443}, "nats.example.com:4443"},
		{delivery.KindS3, "https://s3.site-b.example", delivery.Endpoint{Scheme: "https", Host: "s3.site-b.example", Port: 443}, "s3.site-b.example:443"},
		{delivery.KindS3, "https://s3.site-b.example:9000/", delivery.Endpoint{Scheme: "https", Host: "s3.site-b.example", Port: 9000}, "s3.site-b.example:9000"},
	}
	for _, tt := range accepted {
		got, err := delivery.CheckEndpoint(tt.kind, tt.endpoint)
		if err != nil || got != tt.want || got.Address() != tt.address {
			t.Errorf("CheckEndpoint(%s, %q) = %s with the path %q, %v; want %s with the path %q", tt.kind, tt.endpoint, got, got.Path, err, tt.want, tt.want.Path)
		}
		if got.Origin() != tt.want.Scheme+"://"+tt.address || got.PathSet() != (tt.want.Path != "" && tt.want.Path != "/") {
			t.Errorf("CheckEndpoint(%s, %q): Origin() = %q, PathSet() = %v", tt.kind, tt.endpoint, got.Origin(), got.PathSet())
		}
	}

	// The mark stands for what may be a credential in what was typed. It is
	// put together here, so that no line of this file reads as one.
	mark := strings.Join([]string{"MARK", "9d02"}, "-")
	refused := []struct {
		name     string
		kind     delivery.Kind
		endpoint string
	}{
		{"nothing", delivery.KindWebhook, ""},
		{"no kind", "", "https://hooks.example.com"},
		{"a kind there is not", "kafka", "https://hooks.example.com"},
		{"in the clear", delivery.KindWebhook, "http://hooks.example.com/" + mark},
		{"in the clear, to another deployment", delivery.KindS3, "http://s3.example.com"},
		{"NATS in the clear", delivery.KindNATS, "nats://nats.example.com"},
		{"NATS over HTTPS", delivery.KindNATS, "https://nats.example.com"},
		{"a webhook over the NATS scheme", delivery.KindWebhook, "tls://hooks.example.com"},
		{"the scheme in capitals", delivery.KindWebhook, "HTTPS://hooks.example.com"},
		{"a user", delivery.KindWebhook, "https://" + mark + "@hooks.example.com/hook"},
		{"a user and a password", delivery.KindWebhook, "https://user:" + mark + "@hooks.example.com/hook"},
		{"an empty user", delivery.KindWebhook, "https://@hooks.example.com/hook"},
		{"an at sign in the path", delivery.KindWebhook, "https://hooks.example.com/" + mark + "@x"},
		{"a query", delivery.KindWebhook, "https://hooks.example.com/hook?token=" + mark},
		{"an empty query", delivery.KindWebhook, "https://hooks.example.com/hook?"},
		{"a fragment", delivery.KindWebhook, "https://hooks.example.com/hook#" + mark},
		{"an empty fragment", delivery.KindWebhook, "https://hooks.example.com/hook#"},
		{"no host", delivery.KindWebhook, "https:///hook"},
		{"no host, a port", delivery.KindWebhook, "https://:443/hook"},
		{"a port that is none", delivery.KindWebhook, "https://hooks.example.com:0/hook"},
		{"a port beyond the last", delivery.KindWebhook, "https://hooks.example.com:65536/hook"},
		{"a port of letters", delivery.KindWebhook, "https://hooks.example.com:https/hook"},
		{"an empty port", delivery.KindWebhook, "https://hooks.example.com:/hook"},
		{"a space", delivery.KindWebhook, "https://hooks.example.com/a b"},
		{"a line break", delivery.KindWebhook, "https://hooks.example.com/a\nb"},
		{"a backslash", delivery.KindWebhook, "https://hooks.example.com\\@evil.example/"},
		{"not ASCII", delivery.KindWebhook, "https://hööks.example.com/"},
		{"a zone", delivery.KindWebhook, "https://[fe80::1%25eth0]/hook"},
		{"an escape in the host", delivery.KindWebhook, "https://hooks%2eexample.com/hook"},
		{"an empty label", delivery.KindWebhook, "https://hooks..example.com/hook"},
		{"a dot at the end", delivery.KindWebhook, "https://hooks.example.com./hook"},
		{"a host that ends in a number", delivery.KindWebhook, "https://127.1/hook"},
		{"a host that is a number", delivery.KindWebhook, "https://2130706433/hook"},
		{"a host that is a number in hexadecimal", delivery.KindWebhook, "https://0x7f000001/hook"},
		{"the metadata service", delivery.KindWebhook, "https://169.254.169.254/latest/meta-data/"},
		{"the metadata service over IPv6", delivery.KindWebhook, "https://[fd00:ec2::254]/latest"},
		{"the metadata service, mapped", delivery.KindWebhook, "https://[::ffff:169.254.169.254]/latest"},
		{"the metadata service of Google Cloud over IPv6", delivery.KindWebhook, "https://[fd20:ce::254]/computeMetadata/v1/"},
		{"the metadata service of Alibaba Cloud", delivery.KindWebhook, "https://100.100.100.200/latest/meta-data/"},
		{"the metadata service, IPv4-compatible", delivery.KindWebhook, "https://[::169.254.169.254]/latest"},
		{"the metadata service, by 6to4", delivery.KindWebhook, "https://[2002:a9fe:a9fe::1]/latest"},
		{"the unspecified address", delivery.KindWebhook, "https://0.0.0.0/hook"},
		{"the unspecified address of IPv6", delivery.KindWebhook, "https://[::]/hook"},
		{"multicast", delivery.KindWebhook, "https://224.0.0.1/hook"},
		{"too long", delivery.KindWebhook, "https://hooks.example.com/" + strings.Repeat("a", delivery.MaxEndpointLength)},
		{"another deployment with a path", delivery.KindS3, "https://s3.example.com/bucket"},
		{"a NATS server with a path", delivery.KindNATS, "tls://nats.example.com/subject"},
		{"a NATS server with a slash", delivery.KindNATS, "tls://nats.example.com/"},
		{"opaque", delivery.KindWebhook, "https:hooks.example.com"},
		{"a bracket that is not closed", delivery.KindWebhook, "https://[::1/hook"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := delivery.CheckEndpoint(tt.kind, tt.endpoint)
			if !errors.Is(err, domain.ErrInvalidArgument) || got != (delivery.Endpoint{}) {
				t.Fatalf("CheckEndpoint(%q) = %+v, %v; want %v", tt.endpoint, got, err, domain.ErrInvalidArgument)
			}
			// What was typed may hold a credential: the refusal repeats
			// none of it.
			if strings.Contains(err.Error(), mark) || strings.Contains(err.Error(), "example") {
				t.Errorf("the refusal repeats the endpoint: %v", err)
			}
		})
	}
}

func TestAllowedAddress(t *testing.T) {
	t.Parallel()
	prefixes := func(texts ...string) []netip.Prefix {
		out := make([]netip.Prefix, 0, len(texts))
		for _, text := range texts {
			out = append(out, netip.MustParsePrefix(text))
		}
		return out
	}
	everything := prefixes("0.0.0.0/0", "::/0")
	tests := []struct {
		name  string
		addr  string
		allow []netip.Prefix
		want  bool
	}{
		{"a public address", "93.184.216.34", nil, true},
		{"a public address of IPv6", "2001:db8::1", nil, true},
		{"a private address", "10.1.2.3", nil, true},
		{"another private range", "192.168.1.72", nil, true},
		{"the third", "172.16.0.9", nil, true},
		{"a unique local address", "fd12:3456::1", nil, true},
		{"carrier-grade NAT", "100.64.0.1", nil, true},
		{"loopback", "127.0.0.1", nil, false},
		{"loopback, elsewhere in its range", "127.8.9.10", nil, false},
		{"loopback of IPv6", "::1", nil, false},
		{"loopback, mapped", "::ffff:127.0.0.1", nil, false},
		{"loopback, admitted", "127.0.0.1", prefixes("127.0.0.0/8"), true},
		{"loopback, admitted as one address", "127.0.0.1", prefixes("127.0.0.1/32"), true},
		{"loopback, another admitted", "127.0.0.2", prefixes("127.0.0.1/32"), false},
		{"loopback of IPv6, admitted", "::1", prefixes("::1/128"), true},
		{"loopback of IPv6, IPv4 admitted", "::1", prefixes("127.0.0.0/8"), false},
		{"loopback, mapped, admitted", "::ffff:127.0.0.1", prefixes("127.0.0.0/8"), true},
		{"loopback, a mapped prefix admitted", "127.0.0.1", prefixes("::ffff:127.0.0.0/104"), false},
		{"link-local", "169.254.10.20", nil, false},
		{"link-local, admitted", "169.254.10.20", prefixes("169.254.0.0/16"), true},
		{"link-local of IPv6", "fe80::1", nil, false},
		{"link-local of IPv6 with a zone", "fe80::1%eth0", nil, false},
		{"link-local of IPv6 with a zone, admitted", "fe80::1%eth0", prefixes("fe80::/10"), true},
		{"the metadata service", "169.254.169.254", nil, false},
		{"the metadata service, link-local admitted", "169.254.169.254", prefixes("169.254.0.0/16"), false},
		{"the metadata service, itself admitted", "169.254.169.254", prefixes("169.254.169.254/32"), false},
		{"the metadata service, everything admitted", "169.254.169.254", everything, false},
		{"the metadata service, mapped", "::ffff:169.254.169.254", everything, false},
		{"the metadata service, through NAT64", "64:ff9b::a9fe:a9fe", everything, false},
		{"the metadata service of IPv6", "fd00:ec2::254", nil, false},
		{"the metadata service of IPv6, everything admitted", "fd00:ec2::254", everything, false},
		{"its neighbour", "fd00:ec2::253", nil, true},
		{"the metadata service of Google Cloud over IPv6", "fd20:ce::254", nil, false},
		{"the metadata service of Google Cloud over IPv6, everything admitted", "fd20:ce::254", everything, false},
		{"the metadata service of Google Cloud over IPv6, itself admitted", "fd20:ce::254", prefixes("fd20:ce::254/128"), false},
		{"its neighbour, too", "fd20:ce::253", nil, true},
		{"the metadata service of Alibaba Cloud", "100.100.100.200", nil, false},
		{"the metadata service of Alibaba Cloud, everything admitted", "100.100.100.200", everything, false},
		{"the metadata service of Alibaba Cloud, its range admitted", "100.100.100.200", prefixes("100.64.0.0/10"), false},
		{"the metadata service of Alibaba Cloud, mapped", "::ffff:100.100.100.200", everything, false},
		{"the metadata service of Alibaba Cloud, through NAT64", "64:ff9b::6464:64c8", everything, false},
		{"its neighbour in carrier-grade NAT", "100.100.100.201", nil, true},
		// An IPv6 address that stands for an IPv4 one by a rule a gateway
		// decides is refused, unless a prefix names it - and what is never
		// connected to is not connected to this way either.
		{"IPv4-compatible", "::93.184.216.34", nil, false},
		{"IPv4-compatible, a private address", "::10.1.2.3", nil, false},
		{"IPv4-compatible, an IPv4 prefix admitted", "::10.1.2.3", prefixes("10.0.0.0/8"), false},
		{"IPv4-compatible, the range admitted", "::10.1.2.3", prefixes("::/96"), true},
		{"IPv4-compatible, everything admitted", "::10.1.2.3", everything, true},
		{"IPv4-compatible, the metadata service", "::169.254.169.254", prefixes("::/96"), false},
		{"IPv4-compatible, the metadata service of Alibaba Cloud", "::100.100.100.200", everything, false},
		{"IPv4-compatible, multicast", "::224.0.0.1", everything, false},
		{"IPv4-compatible, this network", "::0.0.0.9", everything, false},
		{"IPv4-compatible, broadcast", "::255.255.255.255", everything, false},
		{"IPv4-compatible, loopback of IPv4", "::127.0.0.1", nil, false},
		{"IPv4-compatible, loopback of IPv4, the range admitted", "::127.0.0.1", prefixes("::/96"), true},
		{"loopback of IPv6 is itself, not of that range", "::1", prefixes("::1/128"), true},
		{"the local-use NAT64 prefix", "64:ff9b:1::5db8:d822", nil, false},
		{"the local-use NAT64 prefix, elsewhere in it", "64:ff9b:1:ffff:ffff:ffff:ffff:ffff", nil, false},
		{"the local-use NAT64 prefix, admitted", "64:ff9b:1::5db8:d822", prefixes("64:ff9b:1::/48"), true},
		{"the local-use NAT64 prefix, an IPv4 prefix admitted", "64:ff9b:1::5db8:d822", prefixes("0.0.0.0/0"), false},
		{"the prefix after the local-use one", "64:ff9b:2::1", nil, true},
		{"6to4", "2002:5db8:d822::1", nil, false},
		{"6to4, a private address", "2002:0a01:0203::1", nil, false},
		{"6to4, admitted", "2002:5db8:d822::1", prefixes("2002::/16"), true},
		{"6to4, one network admitted", "2002:5db8:d822::1", prefixes("2002:5db8:d822::/48"), true},
		{"6to4, another network admitted", "2002:5db8:d823::1", prefixes("2002:5db8:d822::/48"), false},
		{"6to4, the metadata service", "2002:a9fe:a9fe::1", everything, false},
		{"6to4, the metadata service of Alibaba Cloud", "2002:6464:64c8::1", prefixes("2002::/16"), false},
		{"6to4, multicast", "2002:e000:0001::1", everything, false},
		{"6to4, this network", "2002:0001:0203::1", everything, false},
		{"the range after 6to4", "2003::1", nil, true},
		{"SIIT", "::ffff:0:5db8:d822", nil, false},
		{"SIIT, loopback of IPv4", "::ffff:0:7f00:1", nil, false},
		{"SIIT, admitted", "::ffff:0:5db8:d822", prefixes("::ffff:0:0:0/96"), true},
		{"SIIT, an IPv4 prefix admitted", "::ffff:0:5db8:d822", prefixes("0.0.0.0/0"), false},
		{"SIIT, the metadata service", "::ffff:0:a9fe:a9fe", everything, false},
		{"SIIT, multicast", "::ffff:0:e000:1", everything, false},
		{"mapped, not SIIT: judged as IPv4", "::ffff:5db8:d822", nil, true},
		{"the range before 6to4", "2001:ffff::1", nil, true},
		{"loopback through NAT64", "64:ff9b::7f00:1", nil, false},
		{"a public address through NAT64", "64:ff9b::5db8:d822", nil, true},
		{"unspecified", "0.0.0.0", everything, false},
		{"unspecified of IPv6", "::", everything, false},
		{"this network", "0.1.2.3", everything, false},
		{"multicast", "224.0.0.251", everything, false},
		{"multicast of IPv6", "ff02::1", everything, false},
		{"interface-local multicast", "ff01::1", everything, false},
		{"broadcast", "255.255.255.255", everything, false},
	}
	for _, tt := range tests {
		if got := delivery.AllowedAddress(netip.MustParseAddr(tt.addr), tt.allow); got != tt.want {
			t.Errorf("%s: AllowedAddress(%s, %v) = %v, want %v", tt.name, tt.addr, tt.allow, got, tt.want)
		}
	}
	if delivery.AllowedAddress(netip.Addr{}, everything) {
		t.Error("an address that is none is allowed")
	}
	// A prefix that is none admits nothing.
	if delivery.AllowedAddress(netip.MustParseAddr("127.0.0.1"), []netip.Prefix{{}}) {
		t.Error("a prefix that is none admits loopback")
	}
}

// naive says of an address what AllowedAddress must, worked out bit by bit
// from the ranges the ADR names and with nothing of net/netip's own
// classification.
func naive(raw [16]byte, is4 bool, allow []netip.Prefix) bool {
	var v4 [4]byte
	mapped := [12]byte{10: 0xff, 11: 0xff}
	nat64 := [12]byte{0x00, 0x64, 0xff, 0x9b}
	switch {
	case is4:
		copy(v4[:], raw[12:])
	case [12]byte(raw[:12]) == mapped, [12]byte(raw[:12]) == nat64:
		is4 = true
		copy(v4[:], raw[12:])
	}
	admitted := func(addr netip.Addr) bool {
		for _, prefix := range allow {
			if prefix.Contains(addr) {
				return true
			}
		}
		return false
	}
	never4 := func(v4 [4]byte) bool {
		return v4 == [4]byte{169, 254, 169, 254} || v4 == [4]byte{100, 100, 100, 200} || v4[0] == 0 || (v4[0] >= 224 && v4[0] <= 239) ||
			v4 == [4]byte{255, 255, 255, 255}
	}
	if is4 {
		switch {
		case never4(v4):
			return false
		case v4[0] == 127, v4[0] == 169 && v4[1] == 254:
			return admitted(netip.AddrFrom4(v4))
		}
		return true
	}
	aws := [16]byte{0xfd, 0x00, 0x0e, 0xc2, 14: 0x02, 15: 0x54}
	google := [16]byte{0xfd, 0x20, 0x00, 0xce, 14: 0x02, 15: 0x54}
	loopback := [16]byte{15: 1}
	switch {
	case raw == aws, raw == google, raw == [16]byte{}, raw[0] == 0xff:
		return false
	case raw == loopback, raw[0] == 0xfe && raw[1]&0xc0 == 0x80:
		return admitted(netip.AddrFrom16(raw))
	case [12]byte(raw[:12]) == [12]byte{}: // IPv4-compatible, ::/96
		return !never4([4]byte(raw[12:])) && admitted(netip.AddrFrom16(raw))
	case [6]byte(raw[:6]) == [6]byte{0x00, 0x64, 0xff, 0x9b, 0x00, 0x01}: // 64:ff9b:1::/48
		return admitted(netip.AddrFrom16(raw))
	case raw[0] == 0x20 && raw[1] == 0x02: // 6to4, 2002::/16
		return !never4([4]byte(raw[2:6])) && admitted(netip.AddrFrom16(raw))
	case [12]byte(raw[:12]) == [12]byte{8: 0xff, 9: 0xff}: // SIIT, ::ffff:0:0:0/96
		return !never4([4]byte(raw[12:])) && admitted(netip.AddrFrom16(raw))
	}
	return true
}

// FuzzAllowedAddress: the rule equals one written by hand from the ranges,
// and - the property the plan names - loopback, link-local, unspecified,
// multicast and both metadata addresses are refused, the last two kinds and
// the metadata addresses whatever is admitted.
func FuzzAllowedAddress(f *testing.F) {
	seed := func(addr string, bits uint8, is4 bool) {
		raw := netip.MustParseAddr(addr).As16()
		f.Add(raw[:], is4, raw[:], bits)
	}
	seed("169.254.169.254", 96, true)
	seed("fd00:ec2::254", 0, false)
	seed("127.0.0.1", 104, true)
	seed("::1", 128, false)
	seed("fe80::1", 10, false)
	seed("224.0.0.1", 0, true)
	seed("64:ff9b::a9fe:a9fe", 96, false)
	seed("10.0.0.1", 8, true)
	seed("::", 0, false)
	seed("fd20:ce::254", 128, false)
	seed("100.100.100.200", 106, true)
	seed("::10.1.2.3", 96, false)
	seed("::169.254.169.254", 96, false)
	seed("64:ff9b:1::5db8:d822", 48, false)
	seed("2002:5db8:d822::1", 16, false)
	seed("2002:a9fe:a9fe::1", 16, false)
	seed("::ffff:0:a9fe:a9fe", 96, false)
	seed("::ffff:0:5db8:d822", 96, false)
	f.Fuzz(func(t *testing.T, address []byte, is4 bool, prefix []byte, bits uint8) {
		if len(address) != 16 || len(prefix) != 16 {
			return
		}
		raw, of := [16]byte(address), [16]byte(prefix)
		addr := netip.AddrFrom16(raw)
		if is4 {
			addr = netip.AddrFrom4([4]byte(raw[12:]))
		}
		// One prefix of the fuzzer's making, of either family, and the
		// two that admit everything.
		var made netip.Prefix
		if bits <= 32 {
			made = netip.PrefixFrom(netip.AddrFrom4([4]byte(of[12:])), int(bits))
		} else {
			made = netip.PrefixFrom(netip.AddrFrom16(of), int(bits%129))
		}
		everything := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
		for _, allow := range [][]netip.Prefix{nil, {made}, everything, {made, everything[0], everything[1]}} {
			got := delivery.AllowedAddress(addr, allow)
			if want := naive(raw, is4, allow); got != want {
				t.Fatalf("AllowedAddress(%s, %v) = %v, want %v", addr, allow, got, want)
			}
			plain := addr.Unmap()
			never := plain.IsUnspecified() || plain.IsMulticast() ||
				plain == netip.MustParseAddr("169.254.169.254") || plain == netip.MustParseAddr("fd00:ec2::254") ||
				plain == netip.MustParseAddr("fd20:ce::254") || plain == netip.MustParseAddr("100.100.100.200")
			if never && got {
				t.Fatalf("%s is allowed with %v", addr, allow)
			}
			if (plain.IsLoopback() || plain.IsLinkLocalUnicast()) && len(allow) == 0 && got {
				t.Fatalf("%s is allowed without being admitted", addr)
			}
			// An address that stands for an IPv4 one by the rule of a
			// gateway is allowed only where a prefix names it.
			for _, text := range []string{"::/96", "64:ff9b:1::/48", "2002::/16", "::ffff:0:0:0/96"} {
				if plain.Is6() && netip.MustParsePrefix(text).Contains(plain) && got && !slices.ContainsFunc(allow, func(p netip.Prefix) bool { return p.Contains(plain) }) {
					t.Fatalf("%s of %s is allowed with %v, which does not name it", addr, text, allow)
				}
			}
		}
		// Admitting more never refuses what was allowed.
		if delivery.AllowedAddress(addr, nil) && !delivery.AllowedAddress(addr, everything) {
			t.Fatalf("%s is allowed, and refused when everything is admitted", addr)
		}
	})
}

// TestAllowAdmits: a range that could admit nothing is told apart from one
// that could.
func TestAllowAdmits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prefix string
		want   bool
	}{
		{"127.0.0.1/32", true},
		{"127.0.0.0/8", true},
		{"::1/128", true},
		{"fe80::/10", true},
		{"169.254.0.0/16", true},
		{"169.254.170.2/32", true},
		{"10.0.0.0/8", true},
		{"0.0.0.0/0", true},
		{"::/0", true},
		{"::/96", true},
		{"::/127", true},
		{"2002::/16", true},
		{"64:ff9b:1::/48", true},
		{"::ffff:0:0:0/96", true},
		{"224.0.0.0/3", true},
		{"0.0.0.0/8", false},
		{"0.0.0.0/32", false},
		{"224.0.0.0/4", false},
		{"239.255.0.0/16", false},
		{"255.255.255.255/32", false},
		{"ff00::/8", false},
		{"ff02::/16", false},
		{"::/128", false},
		{"169.254.169.254/32", false},
		{"100.100.100.200/32", false},
		{"fd00:ec2::254/128", false},
		{"::ffff:127.0.0.0/104", false},
		{"64:ff9b::/96", false},
		{"64:ff9b::7f00:0/120", false},
		{"::0.0.0.0/104", true}, // holds ::1, loopback
		{"::0.1.0.0/112", false},
		{"::224.0.0.0/100", false},
		{"2002:e000::/20", false},
		{"2002:a9fe:a9fe::/48", false},
		{"::ffff:0:e000:0/100", false},
		{"::ffff:0:a9fe:a9fe/128", false},
	}
	for _, tt := range tests {
		if got := delivery.AllowAdmits(netip.MustParsePrefix(tt.prefix)); got != tt.want {
			t.Errorf("AllowAdmits(%s) = %v, want %v", tt.prefix, got, tt.want)
		}
	}
	if delivery.AllowAdmits(netip.Prefix{}) {
		t.Error("a prefix that is none admits")
	}
}

// FuzzAllowAdmits: a prefix that is said to admit nothing changes nothing:
// an address of it - its first, or one inside it - is allowed with the
// prefix exactly when it is allowed without it.
func FuzzAllowAdmits(f *testing.F) {
	f.Add(netip.MustParseAddr("::ffff:0:e000:0").AsSlice(), uint8(100), uint32(7))
	f.Add(netip.MustParseAddr("2002:e000::").AsSlice(), uint8(20), uint32(1))
	f.Add([]byte{224, 0, 0, 0}, uint8(4), uint32(9))
	f.Fuzz(func(t *testing.T, raw []byte, bits uint8, offset uint32) {
		addr, ok := netip.AddrFromSlice(raw)
		if !ok {
			return
		}
		prefix, err := addr.Prefix(int(bits) % (addr.BitLen() + 1))
		if err != nil || delivery.AllowAdmits(prefix) {
			return
		}
		inside := prefix.Addr().As16()
		var low [4]byte
		binary.BigEndian.PutUint32(low[:], offset)
		for i, b := range low {
			inside[12+i] |= b
		}
		candidates := []netip.Addr{prefix.Addr()}
		if prefix.Addr().Is4() {
			candidates = append(candidates, netip.AddrFrom4([4]byte(inside[12:])))
		} else {
			candidates = append(candidates, netip.AddrFrom16(inside))
		}
		for _, candidate := range candidates {
			if prefix.Contains(candidate) &&
				delivery.AllowedAddress(candidate, []netip.Prefix{prefix}) != delivery.AllowedAddress(candidate, nil) {
				t.Fatalf("%s admits nothing, and admits %s", prefix, candidate)
			}
		}
	})
}

func FuzzCheckEndpoint(f *testing.F) {
	f.Add("webhook", "https://hooks.example.com:8443/in/cactus")
	f.Add("nats", "tls://nats.example.com:4222")
	f.Add("s3", "https://s3.site-b.example/")
	f.Add("webhook", strings.Join([]string{"https://user", "word@hooks.example.com/hook?token=x#y"}, ":"))
	f.Add("webhook", "https://[::ffff:169.254.169.254]/latest")
	f.Add("webhook", "https://0x7f.1/")
	f.Add("s3", "http://127.0.0.1")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, kind, endpoint string) {
		got, err := delivery.CheckEndpoint(delivery.Kind(kind), endpoint)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) || got != (delivery.Endpoint{}) {
				t.Fatalf("CheckEndpoint(%q) = %+v, %v", endpoint, got, err)
			}
			// Nothing of what was typed is repeated: no run of it that
			// is long enough to be a credential.
			for i := 0; i+12 <= len(endpoint); i++ {
				if part := endpoint[i : i+12]; strings.Contains(err.Error(), part) {
					t.Fatalf("the refusal of %q repeats %q: %v", endpoint, part, err)
				}
			}
			return
		}
		// What is accepted is encrypted, names a host and a port, and
		// holds nothing a credential or a second address could hide in.
		scheme := "https://"
		if kind == string(delivery.KindNATS) {
			scheme = "tls://"
		}
		if !strings.HasPrefix(endpoint, scheme) || strings.ContainsAny(endpoint, "@?#\\ \t\r\n") || got.Host == "" || got.Port == 0 {
			t.Fatalf("CheckEndpoint(%s, %q) = %+v", kind, endpoint, got)
		}
		if kind != string(delivery.KindWebhook) && got.Path != "" {
			t.Fatalf("a %s target with the path %q", kind, got.Path)
		}
		if addr, err := netip.ParseAddr(got.Host); err == nil {
			everything := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
			if !delivery.AllowedAddress(addr, everything) || addr.Zone() != "" {
				t.Fatalf("%q names %s, which nothing admits", endpoint, addr)
			}
		}
		// A target with this endpoint is one the state machine keeps.
		// It keeps the origin; the path of a webhook is sealed beside it.
		origin, path := delivery.SplitEndpoint(endpoint)
		target := delivery.Target{Name: "t", Kind: delivery.Kind(kind), Endpoint: origin, Subject: "s", Region: "r"}
		if got.PathSet() != (path != "" && path != "/") {
			t.Fatalf("%q: a path by CheckEndpoint %v, the cut gives %q", endpoint, got.PathSet(), path)
		}
		if got.PathSet() {
			target.SealedPath.Ciphertext = []byte{1}
		}
		switch target.Kind {
		case delivery.KindWebhook:
			target.Subject, target.Region = "", ""
		case delivery.KindNATS:
			target.Region = ""
		case delivery.KindS3:
			target.Subject = ""
			target.Auth = delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKID"}
			target.Secret.Ciphertext = []byte{1}
		}
		if err := target.Validate(); err != nil {
			t.Fatalf("an endpoint that was accepted makes no target: %v", err)
		}
	})
}

// TestEndpointsAreShownWithoutTheirPath: the path of a webhook may be a
// credential, and no way of printing an endpoint that was taken apart shows
// it.
func TestEndpointsAreShownWithoutTheirPath(t *testing.T) {
	t.Parallel()
	mark := strings.Join([]string{"MARK", "path", "4b7e"}, "-")
	endpoint, err := delivery.CheckEndpoint(delivery.KindWebhook, "https://hooks.example.com:8443/services/"+mark)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(endpoint.Path, mark) || !endpoint.PathSet() {
		t.Fatalf("the path is not kept for what connects: %q", endpoint.Path)
	}
	var logged bytes.Buffer
	ctx := context.Background()
	slog.New(slog.NewJSONHandler(&logged, nil)).InfoContext(ctx, "connecting", slog.Any("endpoint", endpoint), slog.Any("pointer", &endpoint))
	slog.New(slog.NewTextHandler(&logged, nil)).InfoContext(ctx, "connecting", slog.Any("endpoint", endpoint))
	encoded, err := json.Marshal(map[string]any{"one": endpoint, "pointer": &endpoint, "list": []delivery.Endpoint{endpoint}})
	if err != nil {
		t.Fatal(err)
	}
	shown := []string{
		endpoint.String(), endpoint.Origin(), endpoint.Address(),
		fmt.Sprint(endpoint), fmt.Sprint(&endpoint),
		fmt.Sprintf("%v|%+v|%#v|%s|%q|%x|%d", endpoint, endpoint, endpoint, endpoint, endpoint, endpoint, endpoint),
		fmt.Sprintf("%v|%+v|%#v", &endpoint, []delivery.Endpoint{endpoint}, map[string]delivery.Endpoint{"e": endpoint}),
		fmt.Sprintf("%+v", struct{ Endpoint delivery.Endpoint }{endpoint}),
		fmt.Errorf("deliver to %v: %w", endpoint, domain.ErrUnavailable).Error(),
		logged.String(), string(encoded),
	}
	for _, text := range shown {
		if strings.Contains(text, mark) || strings.Contains(text, "services") {
			t.Errorf("an endpoint is shown with its path: %s", text)
		}
	}
	const origin = "https://hooks.example.com:8443"
	if endpoint.String() != origin || fmt.Sprintf("%+v", endpoint) != origin || !strings.Contains(logged.String(), origin) || !strings.Contains(string(encoded), `"`+origin+`"`) {
		t.Errorf("an endpoint is not shown as its origin: %s, %s, %s", endpoint, logged.String(), encoded)
	}
}
