package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"idp-web/internal/addr"
	"idp-web/internal/extract"
	"idp-web/internal/fetch"
	"idp-web/internal/mcp"
	"idp-web/internal/search"
	"idp-web/internal/state"
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
	N     int    `json:"n"`
	Title string `json:"title"`
	URL   string `json:"url"`
	// FinalURL is where a read ended when that is not the address that was asked for (ADR-0001 D19 and D20): the number
	// and "url" identify the address that was asked for, so that the member can match them with the search result, and a
	// runtime that keeps the first address it sees for a number never sees a second one. Absent when there was no redirect.
	FinalURL  string `json:"finalUrl,omitempty"`
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
	// From here the provider is called, and a search counts from this point whatever the provider answers: an error, a
	// timeout and a cancelled call all cost what a call costs (ADR-0001 D28).
	if _, err := a.take(ctx, pol, caller, state.Searches, "searches"); err != nil {
		return a.fail(tool, au, err)
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
	type kept struct{ title, url string }
	var keep []kept
	var keepResults []search.Result
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
		if len(keep) == count {
			break
		}
		title := r.Title
		if title == "" {
			title = u
		}
		keep = append(keep, kept{title, u})
		keepResults = append(keepResults, r)
	}
	// the numbers of all the addresses at once, in the order they are listed (one round trip to the database)
	addrs := make([]string, len(keep))
	for i, k := range keep {
		addrs[i] = k.url
	}
	nums, err := a.numbers.Number(ctx, caller.Owner(), addrs, a.cfg.Now())
	if err != nil {
		a.log.Error("numbering the sources failed", "err", err)
		return a.fail(tool, au, ErrNumberingUnavailable)
	}
	var blocks []string
	cites := []citation{}
	for i, k := range keep {
		r := keepResults[i]
		n := nums[i]
		b := fmt.Sprintf("[%d] %s\naddress: %s\nref: %s", n, k.title, k.url, a.cfg.Keys.Make(caller.Owner(), k.url))
		if r.Published != "" {
			b += "\npublished: " + r.Published
		}
		if r.Snippet != "" {
			b += "\nexcerpt: " + r.Snippet
		}
		blocks = append(blocks, b)
		cites = append(cites, citation{N: n, Title: k.title, URL: k.url})
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
		// The unit is taken before the read and given back if the read never sent anything: the reader says when it is about
		// to connect to an address that passed the address rules (fetch.WithDialed). So a read that fails after that (the
		// site is down, too slow, robots.txt says no, a redirect to a private address after the first request went out)
		// counts, and one refused before it connected (the address resolves inside, a name that does not resolve) does not.
		release, err := a.take(ctx, pol, caller, state.Reads, "page reads")
		if err != nil {
			return a.fail(tool, au, err)
		}
		var dialed atomic.Bool
		rctx := fetch.WithDialed(fetch.WithAllow(ctx, pol.AllowHost), func() { dialed.Store(true) })
		page, err := a.cfg.Reader.Read(rctx, u)
		if err != nil {
			if !dialed.Load() {
				release()
			}
			return a.fail(tool, au, err)
		}
		c = cached{page: page, fetchedAt: a.cfg.Now().UTC()}
		a.cache.put(caller.Tenant, u, c)
	}
	part, next, total := extract.Page(c.page.Text, offset, 0)
	au.bytes = len(part)
	nums, err := a.numbers.Number(ctx, caller.Owner(), []string{u}, a.cfg.Now())
	if err != nil {
		a.log.Error("numbering the sources failed", "err", err)
		return a.fail(tool, au, ErrNumberingUnavailable)
	}
	n := nums[0]
	title := strings.Join(strings.Fields(c.page.Title), " ")
	if title == "" {
		title = u
	}
	// Where the read ended, when a redirect took it elsewhere. The reader followed every hop through the address rules, so
	// this is an address like any other this service hands out; it is checked again here because it goes out in a
	// structured field (a value that fails is left out and logged, the page is still read).
	final, redirected := "", ""
	if c.page.FinalURL != "" && c.page.FinalURL != u {
		if ft, err := addr.TargetWith(c.page.FinalURL, addr.Options{TestOrigins: a.cfg.TestOrigins}); err == nil {
			final = ft.URL()
			redirected = " [the address redirected to " + final + "]"
		} else {
			a.log.Error("the address a read ended at fails the address rules", "err", err)
		}
	}
	start := total - len(part)
	if next >= 0 {
		start = next - len(part)
	}
	what := ""
	if c.page.Type == "application/pdf" {
		// the document's pages, and that a long one was cut, so that the model neither takes a cut document for the whole
		// nor has to open the text to find out (ADR-0001 D31)
		what = fmt.Sprintf("PDF, %d pages", c.page.Pages)
		if c.page.PagesRead < c.page.Pages {
			what += fmt.Sprintf(", only the first %d are read", c.page.PagesRead)
		}
		what += "; "
	}
	head := fmt.Sprintf("[%d] %s%s (%sbytes %d–%d of %d", n, title, redirected, what, start, start+len(part), total)
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
		N: n, Title: title, URL: u, FinalURL: final, FetchedAt: c.fetchedAt.Format(time.RFC3339), SHA256: hex.EncodeToString(sum[:])}}}}
}
