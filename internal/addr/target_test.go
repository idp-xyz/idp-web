package addr

import (
	"errors"
	"strings"
	"testing"
)

// The 20 addresses of the evaluation's SSRF list (tools/web_eval/e4.py, SSRF). Target does not resolve, so the names in
// it ("localhost", "keycloak") are the fetch package's to refuse; here they are refused by their port, as they are in the
// list. Every other entry is refused by the form of the address alone.
var ssrf = []string{
	"http://127.0.0.1/", "http://localhost:8080/", "http://[::1]/", "http://0.0.0.0/", "http://2130706433/", "http://0x7f.1/",
	"http://169.254.169.254/latest/meta-data/", "http://[fd00:ec2::254]/", "http://10.0.0.1/", "http://172.16.5.4/", "http://192.168.1.1/",
	"http://100.64.0.1/", "http://[::ffff:127.0.0.1]/", "http://224.0.0.1/", "http://keycloak:7080/", "http://user:pass@example.com/",
	"file:///etc/passwd", "ftp://example.com/", "gopher://example.com/", "javascript:alert(1)",
}

func TestTheTwentyAddressesOfTheSSRFListAreRefused(t *testing.T) {
	if len(ssrf) != 20 {
		t.Fatalf("%d addresses", len(ssrf))
	}
	for _, u := range ssrf {
		if _, err := Target(u); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestAddressesAreRefusedForTheReasonTheyAreWrong(t *testing.T) {
	for _, c := range []struct{ url, why string }{
		{"ftp://example.com/", "not http or https"},
		{"HTTP2://example.com/", "not http or https"},
		{"//example.com/", "not http or https"},
		{"example.com/page", "not http or https"},
		{"http:example.com", "no host"},
		{"http://user@example.com/", "user name"},
		{"http://@example.com/", "user name"},
		{"http://user:pass@example.com/", "user name"},
		{"http://example.com:8080/", "not 80 or 443"},
		{"https://example.com:22/", "not 80 or 443"},
		{"http://example.com:0/", "not 80 or 443"},
		{"http://example.com:65536/", "port is not valid"},
		{"http://example.com:080/", "port is not valid"},
		{"http://example.com:/", "port is not valid"},
		{"http:///page", "no host"},
		{"http://", "no host"},
		{"http://:80/", "no host"},
		{"http://example..com/", "not valid"},
		{"http://.example.com/", "not valid"},
		{"http://exa mple.com/", "not valid"},
		{"http://exa%20mple.com/", "not valid"},
		{"http://example.com\\@evil.com/", "not valid"},
		{"http://ex_ample.com@evil/", "user name"},
		{"http://" + strings.Repeat("a", 64) + ".com/", "not valid"},
		{"http://" + strings.Repeat("a.", 130) + "com/", "too long"},
		{"http://" + strings.Repeat("a", 2100) + ".com/", "longer than"},
		// IP literals in a spelling other than the canonical one
		{"http://2130706433/", "four decimal numbers"},
		{"http://0x7f000001/", "four decimal numbers"},
		{"http://0x7f.1/", "four decimal numbers"},
		{"http://0177.0.0.1/", "four decimal numbers"},
		{"http://127.1/", "four decimal numbers"},
		{"http://127.0.1/", "four decimal numbers"},
		{"http://1.2.3.4.5/", "four decimal numbers"},
		{"http://08.8.8.8/", "four decimal numbers"},
		{"http://8.8.8.08/", "four decimal numbers"},
		{"http://0x08.8.8.8/", "four decimal numbers"},
		{"http://8.8.8.8./", ""},                      // see the accepted table: a trailing dot is dropped
		{"http://１２７.０.０.１/", "four decimal numbers"}, // full-width digits that UTS 46 maps to 127.0.0.1
		{"http://127。0。0。1/", "four decimal numbers"}, // ideographic full stops
		{"http://[0:0:0:0:0:0:0:1]/", "canonical"},
		{"http://[::1]:80/", "not a public address"},
		{"http://[fe80::1%25eth0]/", "zone"},
		{"http://[2606:4700:4700::1111%25eth0]/", "zone"},
		{"http://[2606:4700:4700:0:0:0:0:1111]/", "canonical"},
		{"http://[::ffff:7f00:1]/", "canonical"},
		{"http://[1.2.3.4]/", "not valid"},
		// canonical IP literals that are not public
		{"http://127.0.0.1/", "not a public address"},
		{"https://169.254.169.254/latest/meta-data/", "not a public address"},
		{"http://[64:ff9b::7f00:1]/", "not a public address"},
		{"http://[2002:7f00:1::]/", "not a public address"},
		{"http://[2001:0:4136:e378:8000:63bf:3fff:fdd2]/", "not a public address"},
		{"http://[::ffff:10.0.0.1]/", "not a public address"},
		{"http://[4000::1]/", "not a public address"},
		// names IDNA refuses
		{"http://exa\u00a0mple.com/", "not valid"},
		{"http://exa\u2028mple.com/", "not valid"},
		{"http://a\u0000.com/", "not valid"},
		{"http://-bad-.com/", ""}, // accepted as a name here; the resolver refuses it. Listed in the accepted table.
	} {
		if c.why == "" {
			continue
		}
		_, err := Target(c.url)
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%q: %v, want a refusal that says %q", c.url, err, c.why)
		}
	}
}

func TestAddressesThatMayBeReadAreParsedOnceIntoTheirParts(t *testing.T) {
	for _, c := range []struct {
		in, scheme, host string
		port             int
		url              string
	}{
		{"http://example.com", "http", "example.com", 80, "http://example.com/"},
		{"HTTPS://Example.COM/Page?q=1#frag", "https", "example.com", 443, "https://example.com/Page?q=1"},
		{"https://example.com:443/a%20b", "https", "example.com", 443, "https://example.com/a%20b"},
		{"http://example.com:443/", "http", "example.com", 443, "http://example.com:443/"},
		{"https://example.com:80/", "https", "example.com", 80, "https://example.com:80/"},
		{"http://example.com./x", "http", "example.com", 80, "http://example.com/x"},
		{"http://keycloak/", "http", "keycloak", 80, "http://keycloak/"},
		{"http://localhost/", "http", "localhost", 80, "http://localhost/"}, // a name: the lookup refuses what it resolves to
		{"http://93.184.216.34/", "http", "93.184.216.34", 80, "http://93.184.216.34/"},
		{"https://[2606:4700:4700::1111]/dns", "https", "2606:4700:4700::1111", 443, "https://[2606:4700:4700::1111]/dns"},
		{"http://[::ffff:8.8.8.8]:80/", "http", "::ffff:8.8.8.8", 80, "http://[::ffff:8.8.8.8]/"},
		{"http://bücher.example/", "http", "xn--bcher-kva.example", 80, "http://xn--bcher-kva.example/"},
		{"http://例え.テスト/", "http", "xn--r8jz45g.xn--zckzah", 80, "http://xn--r8jz45g.xn--zckzah/"},
		{"http://faß.de/", "http", "xn--fa-hia.de", 80, "http://xn--fa-hia.de/"},
		{"http://-bad-.com/", "http", "-bad-.com", 80, "http://-bad-.com/"},
		{"http://my_host.example.com/", "http", "my_host.example.com", 80, "http://my_host.example.com/"},
		{"http://example.com?x=1", "http", "example.com", 80, "http://example.com/?x=1"},
		{"http://123.example/", "http", "123.example", 80, "http://123.example/"},
		{"http://0x7f.example/", "http", "0x7f.example", 80, "http://0x7f.example/"},
		{"http://8.8.8.8./", "http", "8.8.8.8", 80, "http://8.8.8.8/"},
	} {
		got, err := Target(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got.Scheme != c.scheme || got.Host != c.host || got.Port != c.port || got.URL() != c.url || got.Exempt {
			t.Errorf("%q: %+v URL %s, want %s %s %d %s", c.in, got, got.URL(), c.scheme, c.host, c.port, c.url)
		}
	}
}

// The URL a request is made to is built from the parts, so that nothing of the raw text that the parser ignored (the
// fragment, the case of the scheme) can differ between what was checked and what is requested.
func TestTheParsedAddressRoundTripsThroughItsOwnURL(t *testing.T) {
	for _, in := range []string{"https://example.com/a?b=c", "http://[2606:4700:4700::1111]:443/x", "http://93.184.216.34/", "http://xn--bcher-kva.example/"} {
		a, err := Target(in)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Target(a.URL())
		if err != nil || a != b {
			t.Errorf("%s: %+v then %+v (%v)", in, a, b, err)
		}
	}
}

// IDP_WEB_TEST_ORIGINS exempts exactly the host:port pairs it lists from the port rule and from the check on a literal
// address, and nothing else.
func TestATestOriginIsExemptAndNothingElseIs(t *testing.T) {
	o := Options{TestOrigins: map[string]bool{"127.0.0.1:41000": true, "localhost:42000": true, "[::1]:43000": true}}
	for _, in := range []string{"http://127.0.0.1:41000/x", "http://localhost:42000/", "http://[::1]:43000/"} {
		a, err := TargetWith(in, o)
		if err != nil || !a.Exempt {
			t.Errorf("%s: %+v %v", in, a, err)
		}
	}
	for _, in := range []string{
		"http://127.0.0.1:41001/",      // another port of the same host
		"http://127.0.0.1:80/",         // the standard port of an exempt host is not exempt
		"http://127.0.0.1/",            // idem
		"http://127.0.0.2:41000/",      // another host at the same port
		"http://localhost:41000/",      // a host of another entry at this entry's port
		"http://localhost:8080/",       // a name at a port that is not listed
		"http://[::1]:43001/",          // another port
		"http://[::2]:43000/",          // another address
		"http://10.0.0.1:41000/",       // a private address at the listed port
		"http://example.com:41000/",    // a name at the listed port
		"http://user@127.0.0.1:41000/", // userinfo is never exempt
		"ftp://127.0.0.1:41000/",       // nor another scheme
		"http://0x7f.1:41000/",         // nor a non-canonical spelling
	} {
		if a, err := TargetWith(in, o); err == nil {
			t.Errorf("%s was accepted as %+v", in, a)
		}
	}
	// without the option nothing is exempt
	if _, err := Target("http://127.0.0.1:41000/"); err == nil {
		t.Error("a test origin was accepted without the option")
	}
}

func FuzzTarget(f *testing.F) {
	for _, s := range ssrf {
		f.Add(s)
	}
	for _, s := range []string{"http://example.com/", "https://93.184.216.34:443/x?y#z", "http://[2606:4700:4700::1111]/", "http://bücher.example/",
		"http://0177.0.0.1/", "http://127.1/", "http://１２７.０.０.１/", "http://[::ffff:8.8.8.8]/", "http://a:80@b/", "http://%31%32%37.0.0.1/",
		"http://example.com%2f@127.0.0.1/", "http://ex\x00ample.com/", "http://[fe80::1%25en0]/", "http://8.8.8.8./", "HTTP://A.B/"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		a, err := Target(raw)
		if err != nil {
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("%q: an error that is not a refusal: %v", raw, err)
			}
			return
		}
		if a.Scheme != "http" && a.Scheme != "https" {
			t.Fatalf("%q: scheme %q", raw, a.Scheme)
		}
		if a.Port != 80 && a.Port != 443 {
			t.Fatalf("%q: port %d", raw, a.Port)
		}
		if a.Exempt {
			t.Fatalf("%q: exempt without a test origin", raw)
		}
		if a.Host == "" || strings.ContainsAny(a.Host, "@/\\?# %[]") && !a.IP.IsValid() {
			t.Fatalf("%q: host %q", raw, a.Host)
		}
		if a.IP.IsValid() {
			if !Public(a.IP) || a.IP.String() != a.Host {
				t.Fatalf("%q: host %q is not a public canonical IP", raw, a.Host)
			}
		} else if !isASCII(a.Host) || looksNumeric(a.Host) || hasUpperASCII(a.Host) {
			t.Fatalf("%q: host %q is not a name", raw, a.Host)
		}
		// no userinfo: the URL built from the parts has no "@" before the host
		if strings.Contains(strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(a.URL(), "https://"), "http://"), "/", 2)[0], "@") {
			t.Fatalf("%q: userinfo in %s", raw, a.URL())
		}
		// the address that was checked is the one that is requested. Percent-encoding what the parser had to encode (a
		// space in the path) can make the rebuilt address longer than MaxURL, which limits what is read, not what is
		// requested: the fuzzer found that, and the round trip is only asked of addresses within the limit.
		if len(a.URL()) > MaxURL {
			return
		}
		b, err := Target(a.URL())
		if err != nil || a != b {
			t.Fatalf("%q: %+v does not round-trip through %s: %+v %v", raw, a, a.URL(), b, err)
		}
	})
}

func hasUpperASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

// Found by the fuzzer: a path of spaces is percent-encoded when the address is rebuilt, which makes it three times
// longer. What is requested is the rebuilt address, and it is the one built from the parts that were checked.
func TestAnAddressThatGrowsWhenItsPathIsEncodedIsStillRequestedAsTheCheckedParts(t *testing.T) {
	a, err := Target("http://A/" + strings.Repeat(" ", 1500))
	if err != nil || a.Host != "a" || !strings.HasPrefix(a.URL(), "http://a/%20%20") || len(a.URL()) != len("http://a/")+3*1500 {
		t.Fatalf("%v %d", err, len(a.URL()))
	}
}
