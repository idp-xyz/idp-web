package addr

import (
	"net/netip"
	"testing"
)

// first, last and a middle address of a prefix, so that the table tests are generated from the table itself: a block
// removed from it fails here, whatever the block is.
func edges(p netip.Prefix) []netip.Addr {
	first := p.Masked().Addr()
	last := first
	b := first.AsSlice()
	mid := append([]byte(nil), b...)
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	last, _ = netip.AddrFromSlice(b)
	if p.Bits() < len(mid)*8 {
		mid[(p.Bits())/8] |= 0x80 >> (p.Bits() % 8) // the first host bit set: the middle of the block
	}
	m, _ := netip.AddrFromSlice(mid)
	return []netip.Addr{first, m, last}
}

func TestEveryBlockOfTheRegistryTableIsNotPublicAtItsFirstMiddleAndLastAddress(t *testing.T) {
	if len(registry) != 25 {
		t.Fatalf("the table has %d entries; the IANA registries as of 2026-11-19 give 25 (14 IPv4, 11 IPv6): update this test with the table", len(registry))
	}
	for _, e := range registry {
		for _, a := range edges(netip.MustParsePrefix(e.cidr)) {
			if Public(a) {
				t.Errorf("%s (%s) is in %s, %s, and was public", a, "table", e.cidr, e.what)
			}
		}
	}
}

func TestAddressesThatAreNotPublicAreNotPublic(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.9", "172.31.255.255", "192.168.1.1", "169.254.169.254", "169.254.0.1",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "fe80::1", "fc00::1", "fd00:ec2::254", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "::",
		"0.1.2.3", "0.255.255.255", "240.0.0.1", "250.1.1.1", "255.255.255.254", "255.255.255.255", // 0.0.0.0/8 beyond 0.0.0.0, 240.0.0.0/4
		"198.18.0.1", "198.19.255.254", "192.0.0.1", "192.0.0.9", "192.0.0.170", // benchmarking, IETF protocol assignments
		"192.0.2.1", "198.51.100.7", "203.0.113.20", "2001:db8::1", "2001:db8:ffff::1", // documentation
		"192.88.99.1",                                                           // deprecated 6to4 relay anycast
		"64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b::808:808", "64:ff9b:1::1", // NAT64, 808:808 is public IPv4 and still refused
		"2002:7f00:1::", "2002:a00:1::1", "2002:808:808::", // 6to4
		"2001:0:4136:e378:8000:63bf:3fff:fdd2",                    // Teredo
		"2001:10::1", "2001:2::1", "100::1", "5f00::1", "3fff::1", // ORCHID, benchmarking, discard-only, SRv6, documentation
		"fec0::1", "fd00::1", "ff02::1", "::ffff:0:102:304", "::102:304", "2001:1::1", "2001:20::1", // site-local, unique-local, multicast, translated, compatible, inside 2001::/23
		"4000::1", "8000::1", "c000::1", "1000::1", // reserved IPv6 space outside global unicast
	} {
		if Public(netip.MustParseAddr(a)) {
			t.Errorf("%s was public", a)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("the zero address was public")
	}
}

// A zone names a local interface ("fe80::1%eth0"), and nothing a page is read from is addressed that way, so every
// zoned address is refused, global ones included. netip.Prefix.Contains is false for any zoned address, so a table
// lookup alone would let these through.
func TestEveryZonedAddressIsNotPublic(t *testing.T) {
	for _, a := range []string{"fe80::1%eth0", "fd00::5%eth0", "::1%lo", "ff02::1%eth0", "2606:4700:4700::1111%eth0", "2001:db8::1%eth0"} {
		if Public(netip.MustParseAddr(a)) {
			t.Errorf("%s was public", a)
		}
	}
}

func TestPublicAddressesNextToTheSpecialPurposeBlocksStayPublic(t *testing.T) {
	for _, a := range []string{"1.1.1.1", "8.8.8.8", "9.255.255.255", "11.0.0.1", "93.184.216.34", "100.63.255.255", "100.128.0.1", "126.255.255.255", "128.0.0.1",
		"172.15.255.255", "172.32.0.1", "192.0.1.1", "192.0.3.1", "192.88.98.1", "192.167.255.255", "192.169.0.1", "198.17.255.255", "198.20.0.1",
		"198.51.99.1", "198.51.101.1", "203.0.112.1", "203.0.114.1", "223.255.255.255", "169.253.255.255", "169.255.0.0",
		"::ffff:8.8.8.8", // IPv4-mapped is judged as the IPv4 address it carries
		"2606:4700:4700::1111", "2a00:1450:4001::1", "2001:4860:4860::8888", "2620:fe::fe", "2001:200::1", "192.31.196.1", "192.52.193.1", "192.175.48.1",
		"2400:cb00::1", "2a03:2880::1", "2000::1", "3ffe::1", "3fff:1000::1"} {
		if !Public(netip.MustParseAddr(a)) {
			t.Errorf("%s was not public", a)
		}
	}
}

func TestEmbeddedIPv4ShowsWhatATranslatedAddressWouldReach(t *testing.T) {
	for _, c := range []struct{ v6, v4 string }{
		{"64:ff9b::7f00:1", "127.0.0.1"},
		{"64:ff9b::a00:1", "10.0.0.1"},
		{"2002:7f00:1::", "127.0.0.1"},
		{"2002:a9fe:a9fe::1", "169.254.169.254"},
		{"::ffff:10.0.0.1", "10.0.0.1"},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", "192.0.2.45"}, // the Teredo client address is the bitwise complement
	} {
		got, ok := EmbeddedIPv4(netip.MustParseAddr(c.v6))
		if !ok || got != netip.MustParseAddr(c.v4) {
			t.Errorf("%s: embedded %v (%v), want %s", c.v6, got, ok, c.v4)
		}
		if Public(netip.MustParseAddr(c.v6)) {
			t.Errorf("%s would reach %s and was public", c.v6, c.v4)
		}
	}
	for _, a := range []string{"2606:4700:4700::1111", "1.1.1.1"} {
		if got, ok := EmbeddedIPv4(netip.MustParseAddr(a)); ok {
			t.Errorf("%s: embedded %v", a, got)
		}
	}
}
