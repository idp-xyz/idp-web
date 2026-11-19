// Package search is the small interface idp-web calls a search service through (ADR-0001 D17, platform ADR-0003 D3),
// and its three adapters: exa, brave and fixture.
//
// A request to a search service carries the fields of Query and nothing else: no tenant, no member, no run or trace
// identifier, in the body or in a header. The query is what leaves the enterprise (channel 1); who asked is not sent
// with it. The API key belongs to the deployment, not to a tenant.
package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/http/httpproxy"
)

// Query is everything a provider is told.
type Query struct {
	Text      string
	Freshness string // "", "day", "week", "month" or "year"
	Count     int
	// IncludeDomains and ExcludeDomains are the tenant's lists. The provider applies them as far as it can; the caller
	// enforces them again on every result, because a provider's filter is not a guarantee (platform ADR-0003 D6).
	IncludeDomains []string
	ExcludeDomains []string
}

// Result is one hit. Published and Snippet may be empty.
type Result struct {
	URL       string
	Title     string
	Published string // YYYY-MM-DD when the provider says
	Snippet   string // the provider's excerpt of the page
}

// Provider searches the web.
type Provider interface {
	Search(ctx context.Context, q Query) ([]Result, error)
}

// The errors a provider returns. The detail (status, body, network error) goes to the log; the model gets a fixed
// sentence (ADR-0001 D17).
var (
	ErrUnavailable = errors.New("the search service did not give an answer")
	ErrBusy        = errors.New("the search service is busy")
)

// Limits of a provider call.
const (
	Timeout     = 10 * time.Second
	MaxResponse = 4 << 20
	maxSnippet  = 1200 // runes
	maxTitle    = 300
)

var white = regexp.MustCompile(`\s+`)

// oneLine makes provider text safe to put in a line of the result list: a title or an excerpt with a line break could
// otherwise write a line that looks like the list's own ("ref: ...", or the start of the next result).
func oneLine(s string, max int) string {
	s = strings.TrimSpace(white.ReplaceAllString(strings.ToValidUTF8(s, ""), " "))
	if utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = string(r[:max]) + "…"
	}
	return s
}

// date keeps the day of an ISO 8601 date-time.
func date(s string) string {
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		return s[:10]
	}
	return ""
}

// NewClient is the HTTP client of a provider's API. It is not the page reader's client: the API host is a fixed,
// trusted destination, so the proxy variables of the environment apply (an enterprise egress proxy is how such a
// host is reached), where the reader dials the addresses it checked itself and ignores them (ADR-0001 D8). It has its
// own timeout, and it follows a redirect only to the same host and never from https to http: the API key is a header
// that must not travel anywhere else.
func NewClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// the environment is read here, once, when the client is made: the same rules as http.ProxyFromEnvironment
	// (which reads it at the first request of the process and never again)
	proxy := httpproxy.FromEnvironment().ProxyFunc()
	t.Proxy = func(r *http.Request) (*url.URL, error) { return proxy(r.URL) }
	return &http.Client{Transport: t, Timeout: Timeout, CheckRedirect: sameHostOnly}
}

func sameHostOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	first := via[0].URL
	if req.URL.Host != first.Host || req.URL.Scheme != first.Scheme {
		return fmt.Errorf("redirect from %s to %s refused", first.Host, req.URL.Host)
	}
	return nil
}

// call sends req and hands the whole answer, at most MaxResponse bytes, to into. Every failure is logged in full and
// returned as ErrUnavailable or ErrBusy. The answer is read to its end before it is looked at, so that a limit on its
// size is a limit and not a side effect of where a JSON decoder stops reading.
func call(client *http.Client, log *slog.Logger, name string, req *http.Request, into func([]byte) error) error {
	resp, err := client.Do(req)
	if err != nil {
		// the error of net/http names the URL, which for a GET holds the query: it is logged as the audit logs it, in clear
		log.Warn("search provider request failed", "provider", name, "err", err)
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Warn("search provider answered with an error", "provider", name, "status", resp.StatusCode, "body", string(b))
		if resp.StatusCode == http.StatusTooManyRequests {
			return ErrBusy
		}
		return ErrUnavailable
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		log.Warn("search provider answer could not be read", "provider", name, "err", err)
		return ErrUnavailable
	}
	if len(b) > MaxResponse {
		log.Warn("search provider answer is too large", "provider", name, "limit", MaxResponse)
		return ErrUnavailable
	}
	if err := into(b); err != nil {
		log.Warn("search provider answer could not be read", "provider", name, "err", err)
		return ErrUnavailable
	}
	return nil
}

func hostOf(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%q is not a base URL", base)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}
