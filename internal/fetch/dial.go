package fetch

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"idp-web/internal/addr"
)

type dialedKey struct{}

// WithDialed returns a context whose reads call fn each time the reader is about to connect to an address that passed
// the address rules: from then on a request is on its way to the web. A read that ends without fn ever called sent nothing
// (refused by the address rules, a name that did not resolve, a deadline before the first connection), which is what the
// daily quota of idp-web uses to give the unit back (ADR-0001 D28). It may be called from the transport's goroutines.
func WithDialed(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, dialedKey{}, fn)
}

type htmlKey struct{}

// WithHTML returns a context whose reads call fn when the final answer is a success whose body is read as an HTML page
// (text/html or application/xhtml+xml, a body that merely starts with %PDF- excluded), before it is extracted. A PDF, plain
// text, another type, an error status and a read that failed earlier never call it. idp-web renders only what called it: the
// renderer is a browser, and a browser given a file it would download answers render_failed (D33).
func WithHTML(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, htmlKey{}, fn)
}

// Dialing tells the call's WithDialed function, if it has one, that a connection to the web is about to be made. The reader
// does it; anything that stands in for the reader (a test double) does it when it means to have gone out.
func Dialing(ctx context.Context) {
	if fn, _ := ctx.Value(dialedKey{}).(func()); fn != nil {
		fn()
	}
}

// checkedDial is the only way the reader makes a connection (ADR-0001 D8). It resolves the host once, refuses the
// connection when ANY address that came back is not public (the Python reference does the same: a name that has one
// private answer among its public ones is a name an attacker controls), and connects to an address it checked, given
// as an IP, so that nothing resolves the name a second time between the check and the connection. TLS is done by the
// transport on top of this connection and verifies the certificate against the host name of the request, not against
// the address.
//
// The address arrives as the transport has it from the request URL, which addr.Target built; the port and the literal
// are checked again here all the same, so that a mistake in one place does not open the other.
func (r *Reader) checkedDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnect, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("%w: port %q", ErrConnect, portText)
	}
	host = strings.ToLower(host)
	exempt := r.cfg.TestOrigins[net.JoinHostPort(host, portText)]
	if !exempt && port != 80 && port != 443 {
		return nil, fmt.Errorf("%w: port %d", ErrNotPublic, port)
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		if ips, err = r.cfg.Resolver.LookupNetIP(ctx, "ip", host); err != nil {
			r.log.Debug("lookup failed", "host", host, "err", err)
			return nil, fmt.Errorf("%w: lookup: %v", ErrConnect, err)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: no address for %s", ErrConnect, host)
	}
	if !exempt {
		for _, ip := range ips {
			if !addr.Public(ip) {
				r.log.Info("address refused", "host", host, "resolved", ip.String())
				return nil, fmt.Errorf("%w: %s resolves to %s", ErrNotPublic, host, ip)
			}
		}
	}
	Dialing(ctx)
	dial := r.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	c, err := dialRace(ctx, dial, r.cfg.DialFamily, ips, port)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnect, err)
	}
	return c, nil
}
