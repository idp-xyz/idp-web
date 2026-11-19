package addr

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// MaxURL is the longest address Target reads, as in the tool schemas (internal/tools).
const MaxURL = 2048

// ErrRefused is wrapped by every refusal of Target; the message of the error says why, as a sentence a model can act
// on. The reasons name the rule, never the internals.
var ErrRefused = errors.New("the address may not be read")

// ErrNotPublic is wrapped by the refusal of an IP literal that is not public, and by the fetch package's refusal of a
// name that resolves to such an address, so that one test covers both.
var ErrNotPublic = errors.New("the address is not a public one, so it was not contacted")

type notPublic struct{ ip netip.Addr }

func (e notPublic) Error() string {
	return fmt.Sprintf("%s: %s is not a public address", ErrRefused, e.ip)
}
func (e notPublic) Is(target error) bool {
	return target == ErrRefused || target == ErrNotPublic
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Address is an address parsed once, the result of Target. The scheme, the host and the port are what every later step uses: the checks,
// the Allow hook, the lookup, the dial, the Host header and the TLS server name all take them from here and never
// from the raw text again, so that two parsers cannot disagree about where a request goes (ADR-0001 D7).
type Address struct {
	Scheme string // "http" or "https"
	// Host is the host as it will be resolved: lower case, ASCII (an internationalised name as its punycode form),
	// without the brackets of an IPv6 literal and without a trailing dot.
	Host string
	Port int // 80 or 443 (anything else only for a test origin)
	// IP is set when Host is an IP literal; it is public (Public) unless the target is a test origin.
	IP netip.Addr
	// Exempt is true for a test origin (Options.TestOrigins): the only case in which the port and the address are not
	// checked.
	Exempt bool
	// Path is the path and query to request, "/" at least, as the parser read them. The fragment is dropped.
	Path string
}

// HostPort is "host:port", with brackets for an IPv6 literal. It is the key of Options.TestOrigins and the address the
// transport dials.
func (t Address) HostPort() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// URL is the address as the request must be made: the parts of the Target and nothing else. The default port is left
// out, so the Host header is the one a browser would send.
func (t Address) URL() string {
	host := t.Host
	if t.IP.Is6() {
		host = "[" + host + "]"
	}
	if t.Port != defaultPort(t.Scheme) {
		host += ":" + strconv.Itoa(t.Port)
	}
	return t.Scheme + "://" + host + t.Path
}

func defaultPort(scheme string) int {
	if scheme == "https" {
		return 443
	}
	return 80
}

// Options of TargetWith.
type Options struct {
	// TestOrigins are exact "host:port" pairs (Address.HostPort of the target) that are exempt from the port rule and,
	// in the fetch package, from the public-address rule. For tests only: nothing else is exempted by them, not
	// another port on the same host, not another host at the same port.
	TestOrigins map[string]bool
}

// Target parses rawURL with no exemptions. See TargetWith.
func Target(rawURL string) (Address, error) { return TargetWith(rawURL, Options{}) }

// TargetWith parses rawURL once and refuses what may not be read (ADR-0001 D7):
//
//   - a scheme other than http and https; an address without "//" (an opaque URL);
//   - userinfo, even an empty one, because it hides the real host from a reader of the address and no page needs it;
//   - a port other than 80 and 443;
//   - an empty host, and a host that is not a valid name after IDNA conversion (internationalised names are converted
//     to their ASCII form; names IDNA rejects are refused);
//   - an IP literal in any spelling but the canonical one: "2130706433", "0x7f.1", "0177.0.0.1" and "127.1" would be
//     taken for names by Go's own resolver and for 127.0.0.1 by the C library and by a browser, so a parser difference
//     must never get as far as a lookup. A canonical IP literal is allowed when it is public (Public), so a model can
//     read "http://93.184.216.34/"; a non-public one is refused here, before any connection. An IPv6 literal with a
//     zone is refused.
//
// It does not resolve names: whether a name resolves to something public is the fetch package's check, made at the
// connection.
func TargetWith(rawURL string, o Options) (Address, error) {
	if len(rawURL) > MaxURL {
		return Address{}, refuse("the address is longer than %d characters", MaxURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Address{}, refuse("the address is not valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Address{}, refuse("the scheme %q is not http or https", u.Scheme)
	}
	if u.Opaque != "" {
		return Address{}, refuse("the address has no host")
	}
	if u.User != nil {
		return Address{}, refuse("the address contains a user name or password")
	}
	host, ip, err := normalizeHost(u.Hostname())
	if err != nil {
		return Address{}, err
	}
	port := defaultPort(u.Scheme)
	if ps := u.Port(); ps != "" {
		p, err := strconv.Atoi(ps)
		if err != nil || p < 0 || p > 65535 || (len(ps) > 1 && ps[0] == '0') {
			return Address{}, refuse("the port is not valid")
		}
		port = p
	} else if strings.HasSuffix(u.Host, ":") {
		// "http://host:/" names the default port in browsers; we do not guess, the address is malformed
		return Address{}, refuse("the port is not valid")
	}
	t := Address{Scheme: u.Scheme, Host: host, Port: port, IP: ip, Path: u.EscapedPath()}
	if t.Path == "" {
		t.Path = "/"
	}
	if u.ForceQuery || u.RawQuery != "" {
		t.Path += "?" + u.RawQuery
	}
	if o.TestOrigins[t.HostPort()] {
		t.Exempt = true
		return t, nil
	}
	if port != 80 && port != 443 {
		return Address{}, refuse("the port %d is not 80 or 443", port)
	}
	if ip.IsValid() && !Public(ip) {
		return Address{}, notPublic{ip}
	}
	return t, nil
}

// normalizeHost returns the host as it will be resolved and, when it is an IP literal, the address.
func normalizeHost(h string) (string, netip.Addr, error) {
	if h == "" {
		return "", netip.Addr{}, refuse("the address has no host")
	}
	// An IPv6 literal is whatever netip reads; a zone is refused with the rest of the zoned space.
	if strings.Contains(h, ":") {
		ip, err := netip.ParseAddr(h)
		if err != nil || !ip.Is6() {
			return "", netip.Addr{}, refuse("the host is not a valid IPv6 address")
		}
		if ip.Zone() != "" {
			return "", netip.Addr{}, refuse("an IPv6 address with a zone is not read")
		}
		if ip.String() != h {
			return "", netip.Addr{}, refuse("write the IPv6 address in its canonical form, %s", ip)
		}
		return h, ip, nil
	}
	h = strings.TrimSuffix(h, ".")
	if h == "" || strings.HasPrefix(h, ".") || strings.Contains(h, "..") {
		return "", netip.Addr{}, refuse("the host name is not valid")
	}
	ascii := isASCII(h)
	if !ascii {
		// UTS 46 mapping also turns full-width and other look-alike digits and dots into ASCII ones, which is why the
		// IP check below runs on the converted name as well, and refuses any such result.
		c, err := idna.Lookup.ToASCII(h)
		if err != nil || c == "" {
			return "", netip.Addr{}, refuse("the host name is not valid")
		}
		h = c
	}
	h = strings.ToLower(h)
	if len(h) > 253 {
		return "", netip.Addr{}, refuse("the host name is too long")
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 {
			return "", netip.Addr{}, refuse("the host name is not valid")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", netip.Addr{}, refuse("the host name is not valid")
			}
		}
	}
	if looksNumeric(h) {
		ip, err := netip.ParseAddr(h)
		if err != nil || !ascii || !ip.Is4() || ip.String() != h {
			return "", netip.Addr{}, refuse("an IP address must be written as four decimal numbers, like 93.184.216.34")
		}
		return h, ip, nil
	}
	return h, netip.Addr{}, nil
}

// looksNumeric is the WHATWG test for "this host is an IPv4 address": its last label is a decimal or hexadecimal
// number. No real top-level domain is one, so no name is lost, while every spelling that some other parser would read
// as an address ("2130706433", "0x7f.1", "0177.0.0.1", "127.1", "1.2.3.4.5") is caught.
func looksNumeric(h string) bool {
	last := h[strings.LastIndexByte(h, '.')+1:]
	if last == "" {
		return false
	}
	if len(last) > 1 && (last[:2] == "0x" || last[:2] == "0X") {
		return true // 0x with or without digits
	}
	for i := 0; i < len(last); i++ {
		if last[i] < '0' || last[i] > '9' {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
