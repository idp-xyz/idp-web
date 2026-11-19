// Package fetch reads one web page for the model, safely (ADR-0001 D6 to D14, platform ADR-0003 D4 and T3, T7).
//
// Safely means: only addresses addr.Public accepts are contacted, and the connection goes to the address that was
// checked (dial.go); every redirect passes the same checks again; the page is at most 5 MiB (also after decoding) and
// arrives within 15 seconds; only HTML and plain text are read; robots.txt is obeyed (robots.go); a bot-check page is
// reported as such rather than handed to the model as the page (botcheck.go). What is left is decoded to UTF-8 and
// reduced to its main text as Markdown by internal/extract. Cutting that text into parts is the caller's
// (extract.Page).
package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"idp-web/internal/addr"
	"idp-web/internal/extract"
	"idp-web/internal/pdfread"
)

// The limits of one read (platform ADR-0003 D4; ADR-0001 D9).
const (
	MaxBytes     = 5 << 20          // of the body, before and after decoding
	Timeout      = 15 * time.Second // connect, headers and body of everything one Read fetches
	MaxRedirects = 5
	maxHeader    = 64 << 10 // of the response headers
	maxRobots    = 512 << 10
	maxErrorPage = 256 << 10 // of the body of an error answer that is looked at for a bot check
)

// The errors Read returns are sentences a model can act on. They say what happened to the page and never what the
// service looks like inside (resolved addresses, dial errors): that goes to the log. A caller tests them with
// errors.Is; the message of a returned error may add what the model needs (the status, the content type).
var (
	ErrNotPublic = addr.ErrNotPublic
	ErrRobots    = errors.New("the site's robots.txt does not allow reading this page")
	ErrTooLarge  = errors.New("the page is larger than 5 MiB")
	ErrTooSlow   = errors.New("the site did not deliver the page within 15 seconds")
	ErrType      = errors.New("the content type is not read")
	// The PDF reader's errors (ADR-0001 D31): the same values, so that errors.Is works across the packages.
	ErrPDFNoText        = pdfread.ErrNoTextLayer
	ErrPDFEncrypted     = pdfread.ErrEncrypted
	ErrPDFMalformed     = pdfread.ErrMalformed
	ErrPDFTooLarge      = pdfread.ErrTooLarge
	ErrPDFTooSlow       = pdfread.ErrTooSlow
	ErrPDFUnavailable   = pdfread.ErrUnavailable
	ErrPDFBusy          = pdfread.ErrBusy
	ErrEncoding         = errors.New("the content encoding is not read")
	ErrBotCheck         = errors.New("the site answered with a bot check instead of the page")
	ErrNeedsScript      = errors.New("the content of this page is produced by script and cannot be read now")
	ErrStatus           = errors.New("the site answered with an HTTP error status")
	ErrTooManyRedirects = errors.New("the address redirects more than 5 times")
	ErrConnect          = errors.New("cannot connect to the site, or its answer was not a valid web page response")
	ErrUnreadable       = errors.New("the page could not be read as text")
)

// detail is a sentinel with a message that says more (the status, the content type) and never anything internal.
type detail struct {
	kind error
	msg  string
}

func (d *detail) Error() string { return d.msg }
func (d *detail) Unwrap() error { return d.kind }

func wrap(kind error, format string, args ...any) error {
	return &detail{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// Page is what Read returns.
type Page struct {
	FinalURL string // after the redirects
	Title    string
	Text     string // the main text as Markdown
	Bytes    int    // the decoded body, before extraction
	Type     string // the media type of the answer
	Engine   string // what produced Text: an engine of internal/extract, or "pdf"
	// For a PDF: its pages and how many of them Text holds (the first ones; a long document is cut, ADR-0001 D31).
	Pages, PagesRead int
}

// PDFReader reads a PDF that was downloaded: internal/pdfread's Runner, which does it in a process of its own. A nil
// reader means PDFs are not read, and a PDF is answered with ErrPDFUnavailable.
type PDFReader interface {
	Read(ctx context.Context, pdf []byte) (pdfread.Output, error)
}

// Resolver is the part of net.Resolver the reader uses; tests replace it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Config of a Reader. The zero value is usable: Go's own resolver, no Allow hook, no test origins.
type Config struct {
	// Resolver answers the one lookup made for each connection. Default: Go's own resolver, never the system's, so
	// that a name is read the way this service reads it and no other parser sees a different address (ADR-0001 D7).
	Resolver Resolver
	// Allow is called with the host of the first address and of every redirect, before anything is sent there; an
	// error refuses it and is returned as it is. The tenant's domain lists enforce themselves through it (task W4).
	Allow func(host string) error
	// TestOrigins are exact "host:port" pairs that are exempt from the port rule and the public-address rule, for
	// tests only (ADR-0001 D14).
	TestOrigins map[string]bool
	// Version goes into the User-Agent.
	Version string
	Log     *slog.Logger
	// Engine of internal/extract. Default: extract.Readability (ADR-0001 3.7).
	Engine string
	// RootCAs replaces the system roots, for tests with a certificate of their own.
	RootCAs *x509.CertPool
	// PDF reads the PDFs; nil: reading PDFs is not available (ADR-0001 D31).
	PDF PDFReader
}

// Reader reads pages. It is safe for concurrent use.
type Reader struct {
	cfg       Config
	opts      addr.Options
	client    *http.Client
	transport *http.Transport
	userAgent string
	log       *slog.Logger

	// limits, lowered by tests
	timeout  time.Duration
	maxBytes int64
	// dial replaces the TCP dial after the check, for tests that must see which address is connected to
	dial func(ctx context.Context, network, address string) (net.Conn, error)

	now       func() time.Time
	robotsMu  sync.Mutex
	robotsMap map[string]robotsEntry
}

// ProductToken is the name robots.txt groups address (RFC 9309 2.2.1).
const ProductToken = "idp-web"

// New returns a Reader.
func New(cfg Config) *Reader {
	if cfg.Resolver == nil {
		cfg.Resolver = &net.Resolver{PreferGo: true}
	}
	if cfg.Engine == "" {
		cfg.Engine = extract.Readability
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	r := &Reader{
		cfg:       cfg,
		opts:      addr.Options{TestOrigins: cfg.TestOrigins},
		userAgent: ProductToken + "/" + cfg.Version + " (+https://github.com/idp-xyz/idp-web)",
		log:       cfg.Log,
		timeout:   Timeout,
		maxBytes:  MaxBytes,
		now:       time.Now,
		robotsMap: map[string]robotsEntry{},
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	r.transport = &http.Transport{
		// The service connects directly: the proxy variables of the environment are ignored on purpose, because a
		// proxy would resolve and connect itself and the check on the address would then mean nothing (ADR-0001 D8,
		// plan 2 item 8).
		Proxy:       nil,
		DialContext: r.checkedDial,
		// The reader sends Accept-Encoding itself and decodes with its own limit; the transport's transparent
		// decompression would apply none (ADR-0001 D10).
		DisableCompression: true,
		// One connection per request, so that every request is resolved and checked, and none reuses a connection
		// made for an earlier answer.
		DisableKeepAlives:      true,
		TLSClientConfig:        &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		MaxResponseHeaderBytes: maxHeader,
	}
	r.client = &http.Client{
		Transport:     r.transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, // follow() does it
	}
	return r
}

type allowKey struct{}

// WithAllow returns a context whose Read also calls allow with the host of the first address and of every redirect,
// after Config.Allow and before anything is sent there. The Reader is shared by every tenant, so a rule that belongs
// to one call (the tenant's domain lists, ADR-0001 D18) travels with the call and not in the Config.
func WithAllow(ctx context.Context, allow func(host string) error) context.Context {
	return context.WithValue(ctx, allowKey{}, allow)
}

// Read fetches rawURL and returns its main text.
func (r *Reader) Read(ctx context.Context, rawURL string) (Page, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	p, err := r.read(ctx, rawURL)
	if err != nil {
		err = r.classify(parent, ctx, err)
		r.log.Info("read failed", "err", err)
	}
	return p, err
}

// classify turns an error that came with the deadline or the caller's cancellation into what the caller needs to
// know; every other error already is a sentinel.
func (r *Reader) classify(parent, ctx context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if ctx.Err() != nil { // the 15 seconds are ours
		return ErrTooSlow
	}
	return err
}

// gate is called with every address of a chain (index 0 is the first one) after it was parsed and before anything is
// sent to it.
type gate func(ctx context.Context, hop int, t addr.Address) error

func (r *Reader) read(ctx context.Context, rawURL string) (Page, error) {
	resp, final, err := r.follow(ctx, rawURL, func(ctx context.Context, hop int, t addr.Address) error {
		if r.cfg.Allow != nil {
			if err := r.cfg.Allow(t.Host); err != nil {
				return err
			}
		}
		if allow, _ := ctx.Value(allowKey{}).(func(string) error); allow != nil {
			if err := allow(t.Host); err != nil {
				return err
			}
		}
		ok, err := r.robotsAllow(ctx, t)
		if err != nil {
			return err
		}
		if !ok {
			return ErrRobots
		}
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	return r.page(ctx, resp, final)
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// follow makes the request and follows the redirects itself, so that every address of the chain goes through the same
// parse (addr.TargetWith), the same gate and the same checked dial (ADR-0001 D9). It returns the final response, not
// judged: its status is the caller's to read. At most MaxRedirects redirects are followed.
func (r *Reader) follow(ctx context.Context, rawURL string, g gate) (*http.Response, addr.Address, error) {
	cur := rawURL
	for hop := 0; ; hop++ {
		t, err := addr.TargetWith(cur, r.opts)
		if err != nil {
			return nil, addr.Address{}, err
		}
		if err := g(ctx, hop, t); err != nil {
			return nil, t, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL(), nil)
		if err != nil {
			return nil, t, ErrConnect
		}
		req.Header.Set("User-Agent", r.userAgent)
		req.Header.Set("Accept", "text/html, application/xhtml+xml, text/plain;q=0.9, */*;q=0.1")
		req.Header.Set("Accept-Encoding", "gzip, deflate")
		resp, err := r.client.Do(req)
		if err != nil {
			r.log.Debug("request failed", "host", t.Host, "err", err)
			switch {
			case errors.Is(err, ErrNotPublic):
				return nil, t, ErrNotPublic
			case ctx.Err() != nil:
				return nil, t, err
			}
			return nil, t, ErrConnect
		}
		if !isRedirect(resp.StatusCode) || resp.Header.Get("Location") == "" {
			return resp, t, nil
		}
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if hop >= MaxRedirects {
			return nil, t, ErrTooManyRedirects
		}
		base, err := url.Parse(t.URL())
		if err != nil {
			return nil, t, ErrConnect
		}
		next, err := base.Parse(loc)
		if err != nil {
			return nil, t, wrap(ErrConnect, "the redirect address is not valid")
		}
		// The federal gazette answers a client it takes for a robot with a redirect to unblock.<host> (phase 0).
		if strings.HasPrefix(strings.ToLower(next.Hostname()), "unblock.") {
			return nil, t, ErrBotCheck
		}
		cur = next.String()
	}
}
