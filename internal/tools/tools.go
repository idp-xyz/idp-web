// Package tools implements idp-web's three MCP tools: web_search, web_fetch and web_open_url (ADR-0001 D3, D16 to D24,
// platform ADR-0003 D2). The schemas and annotations are final, because idp-mcp's gateway pins them at review.
//
// web_fetch reads an address only with the ref a search made for it, for this caller (platform ADR-0003 7.3 T1, T2);
// web_open_url reads an address the member gave, only for a tenant that turned it on. Both enforce the tenant's domain
// lists on the first address and on every redirect, read through the reader (internal/fetch), cut the text into parts
// and cite what they return. Every call is limited per caller, counted, and audited in one JSON line.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"idp-web/internal/fetch"
	"idp-web/internal/mcp"
	"idp-web/internal/refs"
	"idp-web/internal/search"
	"idp-web/internal/tenants"
)

// Metrics counts calls by tool and outcome, and refusals by reason (read by the /metrics endpoint).
type Metrics interface {
	Observe(tool, outcome string, d time.Duration)
	Refused(tool, reason string)
}

// Reader is the page reader (internal/fetch.Reader).
type Reader interface {
	Read(ctx context.Context, rawURL string) (fetch.Page, error)
}

// Limits of the arguments, in the schemas and checked again here: the gateway validates against the pinned schema, but
// this server does not rely on it.
const (
	maxQuery  = 400
	maxURL    = 2048
	maxRef    = 64
	maxCount  = 10
	maxOffset = 1 << 30
)

// defaultCount is how many results a search returns when the caller does not say (the schema says 5).
const defaultCount = 5

var freshnessValues = []string{"day", "week", "month", "year"}

// Limits per caller in a sliding window (appendix C 2 item 4): the defaults are 30 searches and 30 reads in 10 minutes.
type Limits struct {
	Searches, Reads int
	Window          time.Duration
}

// DefaultLimits are used for a zero field of Config.Limits.
var DefaultLimits = Limits{Searches: 30, Reads: 30, Window: 10 * time.Minute}

// Config is everything the tools need. Only Log, Metrics and RequireCaller are needed to declare them; the rest to call
// them (a missing Provider answers ErrNotAvailable, a missing Tenants set enables nobody).
type Config struct {
	Log           *slog.Logger
	Metrics       Metrics
	RequireCaller bool

	Provider search.Provider
	Reader   Reader
	Keys     *refs.Keys
	Tenants  *tenants.Set
	// TestOrigins are exempt from the address rules, for tests only (ADR-0001 D14); the same set the reader has.
	TestOrigins map[string]bool

	Limits Limits
	// Cache bounds: entries and bytes of decoded page text (defaults 256 pages, 64 MiB).
	CacheEntries, CacheBytes int
	Now                      func() time.Time
}

// handler does the work of one tool for a verified caller. The caller is never an argument: tenant and subject come
// from the gateway's assertion (ADR-0001 D2). It fills the audit record as it learns things.
type handler func(ctx context.Context, caller mcp.Caller, args map[string]json.RawMessage, au *audit) mcp.Result

type api struct {
	cfg     Config
	log     *slog.Logger
	met     Metrics
	sources *sources
	cache   *pageCache
	limits  *limiter
}

// audit is what one call says about itself (appendix C 4.7, ADR-0001 D23). It holds the query and the address in clear,
// because they are what leaves the enterprise, and never the ref, a key or page text.
type audit struct {
	tenant   string
	query    string
	url      string
	results  int
	dropped  int
	bytes    int
	cache    bool
	decision string
	reason   string
}

// decode reads the arguments strictly: a field the schema does not name is an error, not ignored.
func decode(args map[string]json.RawMessage, into any) error {
	b, _ := json.Marshal(args)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// failure turns an error into a tool error the model can act on; internals stay in the log.
func failure(err error) mcp.Result {
	o, _ := classify(err)
	return mcp.Result{Text: o.text, IsError: true}
}

// fail is failure for a handler: it also records the decision and the reason in the audit line, counts a refusal, and
// logs the detail of an error that no sentence is written for.
func (a *api) fail(tool string, au *audit, err error) mcp.Result {
	o, known := classify(err)
	au.decision, au.reason = o.decision, o.reason
	if !known {
		a.log.Error("tool failed", "tool", tool, "err", err)
	}
	if o.decision == decisionRefused {
		a.met.Refused(tool, o.reason)
	}
	return mcp.Result{Text: o.text, IsError: true}
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

// short is the caller as the audit line names it: a hash prefix, never the subject (ADR-0001 D4).
func short(id string) string {
	if id == "" {
		return ""
	}
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:4])
}

// wrap adds what every tool shares: the caller check, the metrics and one audit line (ADR-0001 D23).
func (a *api) wrap(name string, h handler) func(*http.Request, map[string]json.RawMessage) mcp.Result {
	return func(r *http.Request, args map[string]json.RawMessage) mcp.Result {
		start := a.cfg.Now()
		caller := mcp.CallerFrom(r)
		au := &audit{tenant: caller.Tenant, decision: "allowed", reason: "ok"}
		var res mcp.Result
		if a.cfg.RequireCaller && caller.Subject == "" {
			res = a.fail(name, au, ErrNoCaller)
		} else {
			res = h(r.Context(), caller, args, au)
		}
		outcome := "ok"
		if res.IsError {
			outcome = "error"
		}
		d := a.cfg.Now().Sub(start)
		a.met.Observe(name, outcome, d)
		a.log.Info("audit", "tool", name, "outcome", outcome, "ms", d.Milliseconds(), "tenant", au.tenant,
			"caller", short(caller.Owner()), "query", au.query, "url", au.url, "results", au.results, "dropped", au.dropped,
			"bytes", au.bytes, "cache", au.cache, "decision", au.decision, "reason", au.reason)
		return res
	}
}

// readOnlyOpenWorld is the annotation of all three tools: they change nothing, and what they read is the open web.
func readOnlyOpenWorld() map[string]any {
	return map[string]any{"readOnlyHint": true, "openWorldHint": true}
}

type searchArgs struct {
	Query     string `json:"query"`
	Freshness string `json:"freshness"`
	Count     *int   `json:"count"`
}

type fetchArgs struct {
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	Offset int    `json:"offset"`
}

type openArgs struct {
	URL    string `json:"url"`
	Offset int    `json:"offset"`
}

func checkQuery(q string) error {
	if q == "" || utf8.RuneCountInString(q) > maxQuery || !utf8.ValidString(q) {
		return fmt.Errorf("%w: query must be 1 to %d characters", ErrInvalid, maxQuery)
	}
	return nil
}

func checkURL(u string) error {
	if u == "" || len(u) > maxURL {
		return fmt.Errorf("%w: url must be 1 to %d bytes", ErrInvalid, maxURL)
	}
	return nil
}

func checkOffset(o int) error {
	if o < 0 || o > maxOffset {
		return fmt.Errorf("%w: offset must not be negative", ErrInvalid)
	}
	return nil
}

// New returns the tools. With RequireCaller a call without a caller is refused (ADR-0001 D2).
func New(cfg Config) []mcp.Tool {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Limits.Searches == 0 {
		cfg.Limits.Searches = DefaultLimits.Searches
	}
	if cfg.Limits.Reads == 0 {
		cfg.Limits.Reads = DefaultLimits.Reads
	}
	if cfg.Limits.Window == 0 {
		cfg.Limits.Window = DefaultLimits.Window
	}
	if cfg.CacheEntries == 0 {
		cfg.CacheEntries = 256
	}
	if cfg.CacheBytes == 0 {
		cfg.CacheBytes = 64 << 20
	}
	a := &api{cfg: cfg, log: cfg.Log, met: cfg.Metrics,
		sources: newSources(cfg.Now), cache: newPageCache(cfg.Now, cfg.CacheEntries, cfg.CacheBytes),
		limits: newLimiter(cfg.Now, cfg.Limits.Window, map[string]int{"searches": cfg.Limits.Searches, "reads": cfg.Limits.Reads})}
	return []mcp.Tool{
		{Name: "web_search",
			Description: "Search the web and return a numbered list of results: title, address, a reference (ref) for the address, the publication date when known and an excerpt. Read a result with web_fetch, giving its address and its ref exactly as listed.",
			InputSchema: obj(map[string]any{
				"query":     map[string]any{"type": "string", "minLength": 1, "maxLength": maxQuery, "description": "What to search for."},
				"freshness": map[string]any{"type": "string", "enum": freshnessValues, "description": "Only results published within the last day, week, month or year."},
				"count":     map[string]any{"type": "integer", "minimum": 1, "maximum": maxCount, "description": "How many results to return (default 5)."},
			}, "query"),
			Annotations: readOnlyOpenWorld(),
			Call:        a.wrap("web_search", a.search)},
		{Name: "web_fetch",
			Description: "Read a page that web_search returned. Give the address and the ref listed next to it; an address without its matching ref is refused. Returns the page text as Markdown in pieces: pass offset to continue.",
			InputSchema: obj(map[string]any{
				"url":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxURL, "description": "The address of a search result, exactly as listed."},
				"ref":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxRef, "description": "The ref listed with that address, exactly as listed."},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Where to continue in the page text (default 0)."},
			}, "url", "ref"),
			Annotations: readOnlyOpenWorld(),
			Call:        a.wrap("web_fetch", a.fetch)},
		{Name: "web_open_url",
			Description: "Read a page at an address the member gave. Unlike web_fetch this needs no ref, so the member is asked to confirm the address first. Returns the page text as Markdown in pieces: pass offset to continue.",
			InputSchema: obj(map[string]any{
				"url":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxURL, "description": "The address the member gave."},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Where to continue in the page text (default 0)."},
			}, "url"),
			Annotations: readOnlyOpenWorld(),
			Call:        a.wrap("web_open_url", a.open)},
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
