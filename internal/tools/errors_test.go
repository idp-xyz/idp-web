package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"idp-web/internal/fetch"
)

// namesTo answers every lookup with the one address, the way a name that points inside would.
type namesTo map[string]string

func (n namesTo) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := n[host]; ok {
		return []netip.Addr{netip.MustParseAddr(a)}, nil
	}
	return nil, io.EOF
}

// An operator reading the audit must be able to tell "the form of the address was wrong" (address_refused) from "the
// address points inside" (address_not_public). addr.ErrNotPublic also satisfies addr.ErrRefused, so the order of the
// rows decides: this walks each situation through the real reader and the real mapping.
func TestEachWayAnAddressIsRefusedGetsItsOwnAuditReason(t *testing.T) {
	// an origin that is allowed to be on loopback (a test origin) and sends the reader to the address in the query
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if to := r.URL.Query().Get("to"); to != "" {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer origin.Close()
	host := strings.TrimPrefix(origin.URL, "http://")
	rd := fetch.New(fetch.Config{
		Resolver:    namesTo{"intranet.example": "10.0.0.5", "metadata.example": "169.254.169.254"},
		TestOrigins: map[string]bool{host: true},
	})
	for _, c := range []struct{ name, url, reason string }{
		{"a private IP literal", "http://10.0.0.1/", "address_not_public"},
		{"the metadata address", "http://169.254.169.254/latest/meta-data/", "address_not_public"},
		{"a name that resolves to a private address", "http://intranet.example/", "address_not_public"},
		{"a redirect to a private IP literal", origin.URL + "/?to=http://192.168.1.1/", "address_not_public"},
		{"a redirect to a name that resolves to a private address", origin.URL + "/?to=http://metadata.example/", "address_not_public"},
		{"a malformed address", "http://exa mple.com/", "address_refused"},
		{"an IP literal in a spelling that is not canonical", "http://2130706433/", "address_refused"},
		{"a forbidden port", "http://example.com:8080/", "address_refused"},
		{"a forbidden scheme", "ftp://example.com/", "address_refused"},
		{"a user name in the address", "http://user@example.com/", "address_refused"},
	} {
		_, err := rd.Read(context.Background(), c.url)
		if err == nil {
			t.Errorf("%s: read", c.name)
			continue
		}
		o, known := classify(err)
		if !known || o.reason != c.reason || o.decision != decisionRefused {
			t.Errorf("%s: reason %q (known %v), want %q: %v", c.name, o.reason, known, c.reason, err)
		}
	}
}
