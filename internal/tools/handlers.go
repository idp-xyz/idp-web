package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"idp-web/internal/addr"
	"idp-web/internal/extract"
	"idp-web/internal/fetch"
	"idp-web/internal/mcp"
	"idp-web/internal/search"
	"idp-web/internal/tenants"
)

// policy is the verified caller's tenant's policy. The tenant is never taken from an argument.
func (a *api) policy(c mcp.Caller) tenants.Policy {
	if a.cfg.Tenants == nil {
		return tenants.Policy{}
	}
	return a.cfg.Tenants.For(c.Tenant)
}

// citation is the structured record of a source the model was given (platform ADR-0003 D9).
type citation struct {
	N         int    `json:"n"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	FetchedAt string `json:"fetchedAt,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// search is web_search: the results of the provider that the tenant's rules allow, each with its number and its ref.
func (a *api) search(ctx context.Context, caller mcp.Caller, args map[string]json.RawMessage, au *audit) mcp.Result {
	const tool = "web_search"
	var in searchArgs
	if err := decode(args, &in); err != nil {
		return a.fail(tool, au, err)
	}
	if in.Freshness != "" && !contains(freshnessValues, in.Freshness) {
		return a.fail(tool, au, fmt.Errorf("%w: freshness must be day, week, month or year", ErrInvalid))
	}
	if in.Count != nil && (*in.Count < 1 || *in.Count > maxCount) {
		return a.fail(tool, au, fmt.Errorf("%w: count must be 1 to %d", ErrInvalid, maxCount))
	}
	if err := checkQuery(in.Query); err != nil {
		return a.fail(tool, au, err)
	}
	au.query = in.Query
	if a.cfg.Provider == nil || a.cfg.Keys == nil {
		return a.fail(tool, au, ErrNotAvailable)
	}
	pol := a.policy(caller)
	if !pol.Enabled {
		return a.fail(tool, au, tenants.ErrDisabled)
	}
	if wait := a.limits.allow(caller.Owner(), "searches"); wait > 0 {
		return a.fail(tool, au, &rateLimited{"searches", wait})
	}
	count := defaultCount
	if in.Count != nil {
		count = *in.Count
	}
	found, err := a.cfg.Provider.Search(ctx, search.Query{Text: in.Query, Freshness: in.Freshness, Count: count,
		IncludeDomains: pol.IncludeDomains, ExcludeDomains: pol.ExcludeDomains})
	if err != nil {
		return a.fail(tool, au, err)
	}
	var blocks []string
	cites := []citation{}
	seen := map[string]bool{}
	for _, r := range found {
		// Every result passes the address rules and the tenant's lists before it gets a ref (ADR-0001 D20): a result
		// that fails is dropped, never returned without one, so that the list never shows an address that cannot be read.
		t, err := addr.TargetWith(r.URL, addr.Options{TestOrigins: a.cfg.TestOrigins})
		if err == nil {
			err = pol.AllowHost(t.Host)
		}
		if err != nil {
			au.dropped++
			continue
		}
		u := t.URL()
		if seen[u] {
			continue
		}
		seen[u] = true
		if len(cites) == count {
			break
		}
		n := a.sources.number(caller.Owner(), u)
		title := r.Title
		if title == "" {
			title = u
		}
		b := fmt.Sprintf("[%d] %s\naddress: %s\nref: %s", n, title, u, a.cfg.Keys.Make(caller.Owner(), u))
		if r.Published != "" {
			b += "\npublished: " + r.Published
		}
		if r.Snippet != "" {
			b += "\nexcerpt: " + r.Snippet
		}
		blocks = append(blocks, b)
		cites = append(cites, citation{N: n, Title: title, URL: u})
	}
	au.results = len(cites)
	text := "no results"
	if len(blocks) > 0 {
		text = strings.Join(blocks, "\n\n")
	}
	return mcp.Result{Text: text, Structured: map[string]any{"citations": cites}}
}

// fetch is web_fetch: the page at an address of a search result, proven by its ref.
func (a *api) fetch(ctx context.Context, caller mcp.Caller, args map[string]json.RawMessage, au *audit) mcp.Result {
	const tool = "web_fetch"
	var in fetchArgs
	if err := decode(args, &in); err != nil {
		return a.fail(tool, au, err)
	}
	if in.Ref == "" || len(in.Ref) > maxRef {
		return a.fail(tool, au, fmt.Errorf("%w: ref must be 1 to %d characters", ErrInvalid, maxRef))
	}
	if err := checkURL(in.URL); err != nil {
		return a.fail(tool, au, err)
	}
	if err := checkOffset(in.Offset); err != nil {
		return a.fail(tool, au, err)
	}
	au.url = in.URL
	pol := a.policy(caller)
	if !pol.Enabled {
		return a.fail(tool, au, tenants.ErrDisabled)
	}
	if a.cfg.Keys == nil {
		return a.fail(tool, au, ErrNotAvailable)
	}
	// T1 and T2: an address is read only with the ref a search made for it, for this caller. This is the one check that
	// stands between the model's own words and a request to the web.
	if err := a.cfg.Keys.Check(caller.Owner(), in.URL, in.Ref); err != nil {
		return a.fail(tool, au, err)
	}
	return a.read(ctx, tool, caller, pol, au, in.URL, in.Offset)
}

// open is web_open_url: the page at an address the member gave, for a tenant that allows it. The HOST asks the member
// to confirm the address first (appendix C 2 item 2); here the tenant's switch is the control.
func (a *api) open(ctx context.Context, caller mcp.Caller, args map[string]json.RawMessage, au *audit) mcp.Result {
	const tool = "web_open_url"
	var in openArgs
	if err := decode(args, &in); err != nil {
		return a.fail(tool, au, err)
	}
	if err := checkURL(in.URL); err != nil {
		return a.fail(tool, au, err)
	}
	if err := checkOffset(in.Offset); err != nil {
		return a.fail(tool, au, err)
	}
	au.url = in.URL
	pol := a.policy(caller)
	if !pol.Enabled {
		return a.fail(tool, au, tenants.ErrDisabled)
	}
	if !pol.OpenURL {
		return a.fail(tool, au, tenants.ErrOpenDisabled)
	}
	return a.read(ctx, tool, caller, pol, au, in.URL, in.Offset)
}

// read is what web_fetch and web_open_url share once the address is allowed to be asked for: the address rules and the
// tenant's domain lists on the first address, the cache, the rate limit, the reader (which applies the lists again on
// every redirect through the call's Allow rule), and the part at offset.
func (a *api) read(ctx context.Context, tool string, caller mcp.Caller, pol tenants.Policy, au *audit, rawURL string, offset int) mcp.Result {
	if a.cfg.Reader == nil {
		return a.fail(tool, au, ErrNotAvailable)
	}
	t, err := addr.TargetWith(rawURL, addr.Options{TestOrigins: a.cfg.TestOrigins})
	if err == nil {
		err = pol.AllowHost(t.Host)
	}
	if err != nil {
		return a.fail(tool, au, err)
	}
	u := t.URL()
	c, hit := a.cache.get(caller.Tenant, u)
	au.cache = hit
	if !hit {
		// only a read that goes out counts against the limit: a later part of a page already read goes nowhere
		if wait := a.limits.allow(caller.Owner(), "reads"); wait > 0 {
			return a.fail(tool, au, &rateLimited{"page reads", wait})
		}
		page, err := a.cfg.Reader.Read(fetch.WithAllow(ctx, pol.AllowHost), u)
		if err != nil {
			return a.fail(tool, au, err)
		}
		c = cached{page: page, fetchedAt: a.cfg.Now().UTC()}
		a.cache.put(caller.Tenant, u, c)
	}
	part, next, total := extract.Page(c.page.Text, offset, 0)
	au.bytes = len(part)
	n := a.sources.number(caller.Owner(), u)
	title := strings.Join(strings.Fields(c.page.Title), " ")
	if title == "" {
		title = c.page.FinalURL
	}
	start := total - len(part)
	if next >= 0 {
		start = next - len(part)
	}
	head := fmt.Sprintf("[%d] %s (bytes %d–%d of %d", n, title, start, start+len(part), total)
	if next >= 0 {
		head += fmt.Sprintf("; the rest: %s with offset %d)", tool, next)
	} else {
		head += ")"
	}
	body := part
	if body == "" {
		body = "(no text found on this page)"
		if offset >= total && total > 0 {
			body = "(nothing at this offset: the page text is shorter)"
		}
	}
	sum := sha256.Sum256([]byte(part))
	return mcp.Result{Text: head + "\n\n" + body, Structured: map[string]any{"citations": []citation{{
		N: n, Title: title, URL: c.page.FinalURL, FetchedAt: c.fetchedAt.Format(time.RFC3339), SHA256: hex.EncodeToString(sum[:])}}}}
}
