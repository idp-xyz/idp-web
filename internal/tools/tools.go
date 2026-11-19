// Package tools declares idp-web's three MCP tools: web_search, web_fetch and web_open_url (ADR-0001 D3, platform
// ADR-0003 D2). The schemas and annotations here are final, because idp-mcp's gateway pins them at review. The handlers
// are not: each answers with a fixed "not available yet" error until the later tasks (search, fetch) replace it.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"idp-web/internal/mcp"
)

// The sentinel errors are what a model is told, so each is a sentence it can act on. Anything else that goes wrong
// is logged and the model gets a fixed sentence (ADR-0001 D3: internals stay in the log).
var (
	ErrNotAvailable = errors.New("web search is not available yet")
	ErrInvalid      = errors.New("the arguments are not valid")
	ErrNoCaller     = errors.New("no caller identity: this server must be registered with identity=forward")
)

// Metrics counts calls by tool and outcome (read by the /metrics endpoint).
type Metrics interface {
	Observe(tool, outcome string, d time.Duration)
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

var freshnessValues = []string{"day", "week", "month", "year"}

// handler does the work of one tool for a verified caller. The caller is never an argument: tenant and subject come
// from the gateway's assertion (ADR-0001 D2).
type handler func(ctx context.Context, caller mcp.Caller, args map[string]json.RawMessage) mcp.Result

type api struct {
	log           *slog.Logger
	met           Metrics
	requireCaller bool
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
	switch {
	case errors.Is(err, ErrNotAvailable), errors.Is(err, ErrInvalid), errors.Is(err, ErrNoCaller):
		return mcp.Result{Text: err.Error(), IsError: true}
	}
	return mcp.Result{Text: "the web tool failed to complete the request", IsError: true}
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

// wrap adds what every tool shares: the caller check, the metrics and one audit line. The audit line names the tool, the
// outcome and the caller's hash, never the arguments' content.
func (a *api) wrap(name string, h handler) func(*http.Request, map[string]json.RawMessage) mcp.Result {
	return func(r *http.Request, args map[string]json.RawMessage) mcp.Result {
		start := time.Now()
		caller := mcp.CallerFrom(r)
		var res mcp.Result
		if a.requireCaller && caller.Subject == "" {
			res = failure(ErrNoCaller)
		} else {
			res = h(r.Context(), caller, args)
		}
		outcome := "ok"
		if res.IsError {
			outcome = "error"
		}
		a.met.Observe(name, outcome, time.Since(start))
		a.log.Info("tool", "tool", name, "outcome", outcome, "ms", time.Since(start).Milliseconds(), "caller", short(caller.Owner()))
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

// notAvailable is the handler of every tool in this skeleton: it checks the arguments as the real handler will and
// then says the service is not there yet. Later tasks replace the body, not the schemas.
func notAvailable(check func(map[string]json.RawMessage) error) handler {
	return func(_ context.Context, _ mcp.Caller, args map[string]json.RawMessage) mcp.Result {
		if err := check(args); err != nil {
			return failure(err)
		}
		return failure(ErrNotAvailable)
	}
}

// New returns the tools. With requireCaller a call without a caller is refused (ADR-0001 D2).
func New(log *slog.Logger, met Metrics, requireCaller bool) []mcp.Tool {
	a := &api{log: log, met: met, requireCaller: requireCaller}
	return []mcp.Tool{
		{Name: "web_search",
			Description: "Search the web and return a numbered list of results: title, address, a reference (ref) for the address, the publication date when known and an excerpt. Read a result with web_fetch, giving its address and its ref exactly as listed.",
			InputSchema: obj(map[string]any{
				"query":     map[string]any{"type": "string", "minLength": 1, "maxLength": maxQuery, "description": "What to search for."},
				"freshness": map[string]any{"type": "string", "enum": freshnessValues, "description": "Only results published within the last day, week, month or year."},
				"count":     map[string]any{"type": "integer", "minimum": 1, "maximum": maxCount, "description": "How many results to return (default 5)."},
			}, "query"),
			Annotations: readOnlyOpenWorld(),
			Call: a.wrap("web_search", notAvailable(func(args map[string]json.RawMessage) error {
				var in searchArgs
				if err := decode(args, &in); err != nil {
					return err
				}
				if in.Freshness != "" && !contains(freshnessValues, in.Freshness) {
					return fmt.Errorf("%w: freshness must be day, week, month or year", ErrInvalid)
				}
				if in.Count != nil && (*in.Count < 1 || *in.Count > maxCount) {
					return fmt.Errorf("%w: count must be 1 to %d", ErrInvalid, maxCount)
				}
				return checkQuery(in.Query)
			}))},
		{Name: "web_fetch",
			Description: "Read a page that web_search returned. Give the address and the ref listed next to it; an address without its matching ref is refused. Returns the page text as Markdown in pieces: pass offset to continue.",
			InputSchema: obj(map[string]any{
				"url":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxURL, "description": "The address of a search result, exactly as listed."},
				"ref":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxRef, "description": "The ref listed with that address, exactly as listed."},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Where to continue in the page text (default 0)."},
			}, "url", "ref"),
			Annotations: readOnlyOpenWorld(),
			Call: a.wrap("web_fetch", notAvailable(func(args map[string]json.RawMessage) error {
				var in fetchArgs
				if err := decode(args, &in); err != nil {
					return err
				}
				if in.Ref == "" || len(in.Ref) > maxRef {
					return fmt.Errorf("%w: ref must be 1 to %d characters", ErrInvalid, maxRef)
				}
				if err := checkURL(in.URL); err != nil {
					return err
				}
				return checkOffset(in.Offset)
			}))},
		{Name: "web_open_url",
			Description: "Read a page at an address the member gave. Unlike web_fetch this needs no ref, so the member is asked to confirm the address first. Returns the page text as Markdown in pieces: pass offset to continue.",
			InputSchema: obj(map[string]any{
				"url":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxURL, "description": "The address the member gave."},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Where to continue in the page text (default 0)."},
			}, "url"),
			Annotations: readOnlyOpenWorld(),
			Call: a.wrap("web_open_url", notAvailable(func(args map[string]json.RawMessage) error {
				var in openArgs
				if err := decode(args, &in); err != nil {
					return err
				}
				if err := checkURL(in.URL); err != nil {
					return err
				}
				return checkOffset(in.Offset)
			}))},
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
