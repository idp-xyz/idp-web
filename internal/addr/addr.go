// Package addr decides which addresses idp-web may contact (ADR-0001 D6 to D8, platform ADR-0003 D4 and T3).
//
// Two things live here and nothing else. Public says whether an IP address is one a page may be read from.
// Target parses a URL exactly once and returns the parts every later step uses, refusing everything that is not a
// plain http or https address of a name or a public IP literal. The fetch package resolves the name, applies Public to
// every answer and connects to the address it checked; this package never resolves anything, so a check on a URL
// form alone can be run without a network (cmd/idp-web-addr).
package addr

import "net/netip"

// notGloballyReachable is every block the service refuses to connect to (ADR-0001 D6). It is not a list someone
// remembered: it is the IANA IPv4 Special-Purpose Address Registry and the IANA IPv6 Special-Purpose Address Registry,
// as of 2026-11-19, every entry with "Globally Reachable: False" (or "N/A"), plus the blocks that are not in those
// tables but are not unicast destinations either (multicast, the reserved 224.0.0.0/3, site-local fec0::/10).
// It is adapted from idp-sandbox's egress proxy (D22 there); the repositories share no code, so a change to one table
// has to be made in the other by hand.
//
// A block that has small globally reachable exceptions is refused whole: 192.0.0.0/24 (192.0.0.9 and 192.0.0.10) and
// 2001::/23 (PCP and TURN anycast, AMT, AS112, drone remote ID). Nothing a page is read from lives there, and one rule
// without holes is easier to check than a rule with them. The other globally reachable special-purpose blocks
// (192.31.196.0/24, 192.52.193.0/24, 192.175.48.0/24, 2620:4f:8000::/48) are left alone: they are public anycast.
//
// IPv6 blocks that embed an IPv4 address (IPv4-mapped, SIIT and NAT64, 6to4, Teredo) are refused whole, for the
// address they would reach, whatever it is: 64:ff9b::7f00:1 and 2002:7f00:1:: reach 127.0.0.1 where such translation
// exists. The one exception is IPv4-mapped (::ffff:a.b.c.d), which Public judges as the IPv4 address it carries,
// because that is what a dual-stack socket would connect to. EmbeddedIPv4 shows the address each would reach.
var notGloballyReachable = func() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range registry {
		out = append(out, netip.MustParsePrefix(c.cidr))
	}
	return out
}()

// registry is the table as data, so that the tests can generate their cases from it.
var registry = []struct{ cidr, what string }{
	// IPv4 Special-Purpose Address Registry
	{"0.0.0.0/8", `"this network" (RFC 791)`},
	{"10.0.0.0/8", "private use (RFC 1918)"},
	{"100.64.0.0/10", "shared address space, carrier-grade NAT (RFC 6598)"},
	{"127.0.0.0/8", "loopback (RFC 1122)"},
	{"169.254.0.0/16", "link local, which holds the cloud metadata address 169.254.169.254 (RFC 3927)"},
	{"172.16.0.0/12", "private use (RFC 1918)"},
	{"192.0.0.0/24", "IETF protocol assignments, DS-Lite, NAT64 discovery, whole (RFC 6890)"},
	{"192.0.2.0/24", "documentation TEST-NET-1 (RFC 5737)"},
	{"192.88.99.0/24", "deprecated 6to4 relay anycast (RFC 7526); the registry marks it deprecated, refused as a transition block"},
	{"192.168.0.0/16", "private use (RFC 1918)"},
	{"198.18.0.0/15", "benchmarking (RFC 2544)"},
	{"198.51.100.0/24", "documentation TEST-NET-2 (RFC 5737)"},
	{"203.0.113.0/24", "documentation TEST-NET-3 (RFC 5737)"},
	{"224.0.0.0/3", "multicast 224.0.0.0/4 and reserved 240.0.0.0/4 with limited broadcast 255.255.255.255 (RFC 1112, RFC 919)"},
	// IPv6 Special-Purpose Address Registry
	{"::/8", "reserved by IETF: unspecified ::, loopback ::1, IPv4-compatible ::/96, IPv4-mapped ::ffff:0:0/96, SIIT ::ffff:0:0:0/96, NAT64 64:ff9b::/96 and 64:ff9b:1::/48"},
	{"100::/64", "discard-only (RFC 6666)"},
	{"2001::/23", "IETF protocol assignments, whole: Teredo 2001::/32, benchmarking 2001:2::/48, ORCHID, PCP, AMT, AS112 (RFC 6890)"},
	{"2001:db8::/32", "documentation (RFC 3849)"},
	{"2002::/16", "6to4 (RFC 3056)"},
	{"3fff::/20", "documentation (RFC 9637)"},
	{"5f00::/16", "segment routing SIDs (RFC 9602)"},
	{"fc00::/7", "unique local (RFC 4193)"},
	{"fe80::/10", "link local (RFC 4291)"},
	{"fec0::/10", "deprecated site-local (RFC 3879)"},
	{"ff00::/8", "multicast (RFC 4291)"},
}

// globalUnicastV6 is the only IPv6 space in use for global unicast addresses (IANA IPv6 Address Space registry,
// 2000::/3). An IPv6 address outside it and outside every table entry is reserved space nobody can have been given
// a web server in, so it is refused too: the table above only lists what is marked special, not what is unassigned.
var globalUnicastV6 = netip.MustParsePrefix("2000::/3")

// EmbeddedIPv4 returns the IPv4 address an IPv6 address would reach through a translation mechanism: the IPv4-mapped
// ::ffff:a.b.c.d, NAT64 64:ff9b::/96 and 64:ff9b:1::/48 (RFC 6052, RFC 8215), 6to4 2002::/16 and the client address of
// Teredo 2001::/32. Public does not depend on it (it refuses those blocks whole, IPv4-mapped apart); it shows what
// each would have reached.
func EmbeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	a = a.WithZone("") // Prefix.Contains is false for a zoned address
	if a.Is4In6() {
		return a.Unmap(), true
	}
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	switch {
	case netip.MustParsePrefix("64:ff9b::/96").Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case netip.MustParsePrefix("64:ff9b:1::/48").Contains(a): // the /48 layout of RFC 6052: bits 64 to 71 are zero
		return netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}), true
	case netip.MustParsePrefix("2002::/16").Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	case netip.MustParsePrefix("2001::/32").Contains(a): // the client's address is stored inverted
		return netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}), true
	}
	return netip.Addr{}, false
}

// Public reports whether a page may be read from the address: it is globally reachable unicast. An IPv4-mapped IPv6
// address is judged as the IPv4 address it carries. Anything else that is not provably public is not: the zero Addr,
// every zoned address, every block of the table, and IPv6 space outside global unicast.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	// A zone names a local interface and Prefix.Contains is false for a zoned address, so a zoned address would pass
	// every entry of the table: refuse it outright.
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	if a.Is6() && !globalUnicastV6.Contains(a) {
		return false
	}
	for _, p := range notGloballyReachable {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
