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
	dial := r.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	var last error
	for _, ip := range ips {
		c, err := dial(ctx, "tcp", netip.AddrPortFrom(ip.Unmap(), uint16(port)).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, fmt.Errorf("%w: %v", ErrConnect, last)
}
