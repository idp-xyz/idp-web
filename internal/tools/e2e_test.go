package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-web/internal/fetch"
	"idp-web/internal/mcp"
	"idp-web/internal/refs"
	"idp-web/internal/search"
	"idp-web/internal/tenants"
)

// This file is the end-to-end test: the MCP server with the real tools, the fixture provider, a real page reader that
// reaches an httptest origin only through the test-origin exemption, and callers set as the verified caller would be.

const tenantsYAML = `
default: {enabled: false}
tenants:
  acme: {enabled: true, excludeDomains: [blocked.test]}
  opener: {enabled: true, openUrl: true}
  only: {enabled: true, includeDomains: [allowed.test]}
  off: {enabled: false}
`

var refKey = []byte(strings.Repeat("k", 32))

// syncBuf is a log sink that tests may read while the server writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// clock is a time source the tests move.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// resolver sends every name to this machine: the origin is the only server there is.
type resolver struct{}

func (resolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

// countingProvider counts the calls that reach the provider.
type countingProvider struct {
	search.Provider
	mu    sync.Mutex
	calls int
}

func (c *countingProvider) Search(ctx context.Context, q search.Query) ([]search.Result, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.Provider.Search(ctx, q)
}

type world struct {
	t      *testing.T
	srv    *mcp.Server
	origin *httptest.Server
	port   string
	log    *syncBuf
	met    *counting
	clk    *clock
	keys   *refs.Keys
	prov   *countingProvider
	cfg    Config

	mu   sync.Mutex
	hits map[string]int // by path
}

func (w *world) hitsOf(path string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hits[path]
}

// article has numbered paragraphs, long enough for two parts.
func article() string {
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><title>The Parcel Report</title></head><body><nav><a href=\"/\">Home</a></nav><article><h1>The Parcel Report</h1>")
	for i := 0; i < 70; i++ {
		fmt.Fprintf(&b, "<p>Paragraph %03d: the depot sorted forty thousand parcels on Tuesday, which is the most it has sorted on a single day since the new belt opened, and the managers said the night shift stays.</p>", i)
	}
	b.WriteString("</article><footer>Copyright footer</footer></body></html>")
	return b.String()
}

type options struct {
	tenants  string // the tenants file; tenantsYAML when empty
	state    State
	limits   Limits
	provider func(fixture search.Provider) search.Provider
	reader   Reader
}

func newWorld(t *testing.T, o ...options) *world {
	t.Helper()
	var opt options
	if len(o) > 0 {
		opt = o[0]
	}
	w := &world{t: t, hits: map[string]int{}, log: &syncBuf{}, met: &counting{}, clk: &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}}
	page := article()
	w.origin = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.hits[r.URL.Path]++
		w.mu.Unlock()
		switch r.URL.Path {
		case "/robots.txt", "/missing-robots":
			http.NotFound(rw, r)
		case "/go":
			http.Redirect(rw, r, "http://blocked.test:"+w.port+"/page", http.StatusFound)
		case "/gone":
			http.NotFound(rw, r)
		case "/to": // a redirect to wherever the query says
			http.Redirect(rw, r, r.URL.Query().Get("to"), http.StatusFound)
		default:
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(rw, page)
		}
	}))
	t.Cleanup(w.origin.Close)
	w.port = w.origin.Listener.Addr().String()[strings.LastIndex(w.origin.Listener.Addr().String(), ":")+1:]
	origins := map[string]bool{}
	for _, h := range []string{"allowed.test", "blocked.test", "other.test", "127.0.0.1"} {
		origins[h+":"+w.port] = true
	}
	log := slog.New(slog.NewJSONHandler(w.log, nil))
	var reader Reader = fetch.New(fetch.Config{Resolver: resolver{}, TestOrigins: origins, Version: "test", Log: slog.New(slog.DiscardHandler)})
	if opt.reader != nil {
		reader = opt.reader
	}
	u := func(host, path string) string { return fmt.Sprintf("http://%s:%s%s", host, w.port, path) }
	fx, err := search.ParseFixture([]byte(fmt.Sprintf(`
entries:
  - match: parcels
    results:
      - {url: %q, title: "Parcels, a history", published: "2026-01-02", snippet: "All about parcels"}
      - {url: %q, title: "Blocked parcels"}
      - {url: "http://127.0.0.1:1/internal", title: "Private"}
      - {url: "not a url", title: "Malformed"}
  - match: second
    results:
      - {url: %q, title: "Parcels again"}
      - {url: %q, title: "Another page"}
  - match: redirect
    results:
      - {url: %q, title: "Goes elsewhere"}
  - match: gone
    results:
      - {url: %q, title: "Not there"}
`, u("allowed.test", "/article"), u("blocked.test", "/page"), u("allowed.test", "/article"), u("allowed.test", "/other"), u("allowed.test", "/go"), u("allowed.test", "/gone"))))
	if err != nil {
		t.Fatal(err)
	}
	w.prov = &countingProvider{Provider: fx}
	var prov search.Provider = w.prov
	if opt.provider != nil {
		prov = opt.provider(fx)
	}
	file := tenantsYAML
	if opt.tenants != "" {
		file = opt.tenants
	}
	set, err := tenants.Parse([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := refs.New(refKey)
	if err != nil {
		t.Fatal(err)
	}
	w.keys = keys.WithClock(w.clk.now)
	w.cfg = Config{Log: log, Metrics: w.met, RequireCaller: true, Provider: prov, Reader: reader, Keys: w.keys,
		Tenants: set, State: opt.state, TestOrigins: origins, Limits: opt.limits, Now: w.clk.now}
	w.srv = mcp.New("idp-web", "test", "", New(w.cfg))
	return w
}

func (w *world) url(host, path string) string {
	return fmt.Sprintf("http://%s:%s%s", host, w.port, path)
}

// as calls a tool as the verified caller tenant/subject and returns the result.
func (w *world) as(tenant, subject, tool string, args any) map[string]any {
	w.t.Helper()
	return w.asOn(w.srv, tenant, subject, tool, args)
}

// replica is another instance of the service over the same world (the same provider, the same origin and keys, the same
// clock) with a State of its own, as a second pod would be; it has its own cache and its own sliding window.
func (w *world) replica(st State) *mcp.Server {
	cfg := w.cfg
	cfg.State = st
	return mcp.New("idp-web", "test", "", New(cfg))
}

func (w *world) asOn(srv *mcp.Server, tenant, subject, tool string, args any) map[string]any {
	w.t.Helper()
	a, _ := json.Marshal(args)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s,%s,"io.idp.telos/caller":{"subject":%q,"tenant":%q,"roles":["member"]}}}}`,
		tool, a, meta, subject, tenant)
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
	r.Header.Set("Mcp-Method", "tools/call")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		w.t.Error(err)
	}
	res, _ := out["result"].(map[string]any)
	if res == nil {
		w.t.Errorf("no result: %s", rec.Body)
		return map[string]any{"isError": true, "content": []any{map[string]any{"text": ""}}}
	}
	return res
}

func isErr(res map[string]any) bool { return res["isError"] == true }

func citations(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	sc, _ := res["structuredContent"].(map[string]any)
	list, _ := sc["citations"].([]any)
	var out []map[string]any
	for _, c := range list {
		out = append(out, c.(map[string]any))
	}
	return out
}

var (
	refRe  = regexp.MustCompile(`(?m)^ref: (\S+)$`)
	addrRe = regexp.MustCompile(`(?m)^address: (\S+)$`)
)

// listed returns the address and ref of the i-th result in the text of a search.
func listed(t *testing.T, res map[string]any, i int) (address, ref string) {
	t.Helper()
	a, r := addrRe.FindAllStringSubmatch(text(res), -1), refRe.FindAllStringSubmatch(text(res), -1)
	if len(a) <= i || len(r) <= i {
		t.Fatalf("no result %d in %q", i, text(res))
	}
	return a[i][1], r[i][1]
}

func TestSearchThenFetchReturnsThePageWithACitationWhoseNMatchesTheSearchResult(t *testing.T) {
	w := newWorld(t)
	res := w.as("acme", "alice", "web_search", map[string]any{"query": "tell me about Parcels"})
	if isErr(res) {
		t.Fatal(text(res))
	}
	address, ref := listed(t, res, 0)
	want := fmt.Sprintf("[1] Parcels, a history\naddress: %s\nref: %s\npublished: 2026-01-02\nexcerpt: All about parcels", w.url("allowed.test", "/article"), ref)
	if text(res) != want {
		t.Fatalf("the result list is\n%s\nwant\n%s", text(res), want)
	}
	sc := citations(t, res)
	if len(sc) != 1 || sc[0]["n"] != float64(1) || sc[0]["url"] != address || sc[0]["title"] != "Parcels, a history" {
		t.Fatalf("citations %v", sc)
	}
	page := w.as("acme", "alice", "web_fetch", map[string]any{"url": address, "ref": ref})
	if isErr(page) {
		t.Fatal(text(page))
	}
	body := text(page)
	if !strings.HasPrefix(body, "[1] The Parcel Report (bytes 0–") || !strings.Contains(body, "; the rest: web_fetch with offset ") || !strings.Contains(body, "Paragraph 000") || strings.Contains(body, "Copyright footer") {
		t.Fatalf("%.300s", body)
	}
	pc := citations(t, page)
	if len(pc) != 1 || pc[0]["n"] != float64(1) || pc[0]["url"] != address {
		t.Fatalf("citations %v", pc)
	}
	part := body[strings.Index(body, "\n\n")+2:]
	if got := fmt.Sprintf("%x", sha(part)); pc[0]["sha256"] != got {
		t.Errorf("the citation's sha256 is %v, the part's is %s", pc[0]["sha256"], got)
	}
	if pc[0]["fetchedAt"] != "2026-11-19T12:00:00Z" {
		t.Errorf("fetchedAt %v", pc[0]["fetchedAt"])
	}
	if pc[0]["title"] != "The Parcel Report" {
		t.Errorf("title %v", pc[0]["title"])
	}
}

func TestResultsThatFailTheAddressRulesOrTheTenantsListsAreDroppedAndCounted(t *testing.T) {
	w := newWorld(t)
	res := w.as("acme", "alice", "web_search", map[string]any{"query": "parcels"})
	if strings.Contains(text(res), "blocked.test") || strings.Contains(text(res), "127.0.0.1") || strings.Contains(text(res), "not a url") || len(citations(t, res)) != 1 {
		t.Fatalf("%s", text(res))
	}
	// the tenant that only allows allowed.test: the same results, and what is not on its list is gone too
	res = w.as("only", "alice", "web_search", map[string]any{"query": "parcels"})
	if len(citations(t, res)) != 1 {
		t.Fatalf("%s", text(res))
	}
	if !strings.Contains(w.log.String(), `"results":1,"dropped":3`) {
		t.Errorf("the audit line does not count the dropped results:\n%s", w.log.String())
	}
	// a tenant that excludes nothing gets the excluded domain's result: the drop is the tenant's rule, not the fixture's
	res = w.as("opener", "alice", "web_search", map[string]any{"query": "parcels"})
	if len(citations(t, res)) != 2 || !strings.Contains(text(res), "blocked.test") {
		t.Errorf("%s", text(res))
	}
}

func sha(s string) []byte {
	h := sha256Sum(s)
	return h[:]
}

// T1: an address that did not come from a search cannot be read, whatever ref comes with it, or none.
func TestAnAddressThatWasNotInASearchResultIsRefusedWithAnyRefAndWithNone(t *testing.T) {
	w := newWorld(t)
	res := w.as("acme", "alice", "web_search", map[string]any{"query": "parcels"})
	address, ref := listed(t, res, 0)
	leak := w.url("allowed.test", "/leak?d=SECRET")
	// the good case first: the listed address with its ref reaches the origin, so the refusals below are about the address
	if isErr(w.as("acme", "alice", "web_fetch", map[string]any{"url": address, "ref": ref})) || w.hitsOf("/article") != 1 {
		t.Fatal("the listed address was not read")
	}
	for name, args := range map[string]map[string]any{
		"the ref of the listed address":     {"url": leak, "ref": ref},
		"a well-formed ref nobody made":     {"url": leak, "ref": "ffffffff.aaaaaaaaaaaaaaaa"},
		"a ref that is not a ref":           {"url": leak, "ref": "please"},
		"no ref (an empty one)":             {"url": leak, "ref": ""},
		"no ref (the field missing)":        {"url": leak},
		"the listed address plus a query":   {"url": address + "?d=SECRET", "ref": ref},
		"the listed address with a suffix":  {"url": address + "/x", "ref": ref},
		"an address on a host never listed": {"url": w.url("other.test", "/leak?d=SECRET"), "ref": ref},
	} {
		got := w.as("acme", "alice", "web_fetch", args)
		if !isErr(got) {
			t.Errorf("%s: read", name)
		}
	}
	if w.hitsOf("/leak") != 0 {
		t.Fatalf("the origin saw %d requests for an address the model made up", w.hitsOf("/leak"))
	}
	// the sentence tells the model what to do
	got := w.as("acme", "alice", "web_fetch", map[string]any{"url": leak, "ref": "ffffffff.aaaaaaaaaaaaaaaa"})
	if !strings.Contains(text(got), "did not come from a search result") || !strings.Contains(text(got), "search first") {
		t.Errorf("%s", text(got))
	}
}

// T2: a forged, moved or expired ref is refused.
func TestAnAlteredRefAnotherAddressesRefAnotherCallersRefAndAnExpiredRefAreRefused(t *testing.T) {
	w := newWorld(t)
	res := w.as("acme", "alice", "web_search", map[string]any{"query": "second"})
	addrA, refA := listed(t, res, 0)
	addrB, refB := listed(t, res, 1)
	if isErr(w.as("acme", "alice", "web_fetch", map[string]any{"url": addrB, "ref": refB})) {
		t.Fatal("the good ref was refused")
	}
	altered := refA[:12] + map[bool]string{true: "b", false: "a"}[refA[12] == 'a'] + refA[13:]
	for name, c := range map[string]struct {
		caller string
		url    string
		ref    string
	}{
		"a ref altered in one character": {"alice", addrA, altered},
		"another address's ref":          {"alice", addrB, refA},
		"another caller's ref":           {"bob", addrA, refA},
	} {
		got := w.as("acme", c.caller, "web_fetch", map[string]any{"url": c.url, "ref": c.ref})
		if !isErr(got) || !strings.Contains(text(got), "did not come from a search result") {
			t.Errorf("%s: %v", name, got)
		}
	}
	if w.hitsOf("/article") != 0 {
		t.Errorf("the origin was asked for %d refused reads", w.hitsOf("/article"))
	}
	w.clk.advance(refs.Lifetime + time.Minute)
	got := w.as("acme", "alice", "web_fetch", map[string]any{"url": addrA, "ref": refA})
	if !isErr(got) || !strings.Contains(text(got), "expired") || w.hitsOf("/article") != 0 {
		t.Errorf("an expired ref: %v", got)
	}
	// each refusal is counted by its reason
	for _, reason := range []string{"ref_mismatch", "ref_expired"} {
		if w.met.calls["refused/web_fetch/"+reason] == 0 {
			t.Errorf("no refusal counted as %s: %v", reason, w.met.calls)
		}
	}
}

func TestATenantThatIsNotEnabledGetsNothing(t *testing.T) {
	w := newWorld(t)
	good := w.as("acme", "alice", "web_search", map[string]any{"query": "second"})
	address, _ := listed(t, good, 0)
	for _, tenant := range []string{"off", "stranger", ""} {
		got := w.as(tenant, "alice", "web_search", map[string]any{"query": "second"})
		if !isErr(got) || !strings.Contains(text(got), "not enabled") {
			t.Errorf("%q: %v", tenant, got)
		}
		// even a ref that is valid for this caller reads nothing for a tenant that is off
		ref := w.keys.Make(tenant+"/alice", address)
		got = w.as(tenant, "alice", "web_fetch", map[string]any{"url": address, "ref": ref})
		if !isErr(got) || !strings.Contains(text(got), "not enabled") {
			t.Errorf("%q fetch: %v", tenant, got)
		}
	}
	if w.prov.calls != 1 {
		t.Errorf("the provider was asked %d times, once was for the enabled tenant", w.prov.calls)
	}
	if w.hitsOf("/article") != 0 {
		t.Error("the origin was reached for a tenant that is off")
	}
}

func TestAnExcludedDomainIsRefusedOnFetchAndOnARedirectToIt(t *testing.T) {
	w := newWorld(t)
	blocked := w.url("blocked.test", "/page")
	// a ref that is valid for the address: only the domain rule can refuse it
	got := w.as("acme", "alice", "web_fetch", map[string]any{"url": blocked, "ref": w.keys.Make("acme/alice", blocked)})
	if !isErr(got) || !strings.Contains(text(got), "not allowed for your organisation") || w.hitsOf("/page") != 0 {
		t.Fatalf("%v (%d requests)", got, w.hitsOf("/page"))
	}
	// the control: the tenant that excludes nothing reads the same page
	if isErr(w.as("opener", "alice", "web_fetch", map[string]any{"url": blocked, "ref": w.keys.Make("opener/alice", blocked)})) {
		t.Fatal("the page cannot be read at all")
	}
	// a listed address on an allowed domain that redirects to the excluded one
	res := w.as("acme", "alice", "web_search", map[string]any{"query": "redirect"})
	address, ref := listed(t, res, 0)
	before := w.hitsOf("/page")
	got = w.as("acme", "alice", "web_fetch", map[string]any{"url": address, "ref": ref})
	if !isErr(got) || !strings.Contains(text(got), "not allowed for your organisation") {
		t.Fatalf("%v", got)
	}
	if w.hitsOf("/go") != 1 || w.hitsOf("/page") != before {
		t.Errorf("/go %d, /page %d (was %d): the redirect was followed", w.hitsOf("/go"), w.hitsOf("/page"), before)
	}
	// a tenant with an include list refuses what is not on it
	other := w.url("other.test", "/article")
	got = w.as("only", "alice", "web_fetch", map[string]any{"url": other, "ref": w.keys.Make("only/alice", other)})
	if !isErr(got) || !strings.Contains(text(got), "not among those") {
		t.Errorf("%v", got)
	}
}

func TestWebOpenURLIsRefusedUnlessTheTenantAllowsItAndThenNeedsNoRef(t *testing.T) {
	w := newWorld(t)
	address := w.url("allowed.test", "/article")
	got := w.as("opener", "alice", "web_open_url", map[string]any{"url": address})
	if isErr(got) || !strings.Contains(text(got), "Paragraph 000") || !strings.Contains(text(got), "web_open_url with offset ") {
		t.Fatalf("a tenant that allows it: %v", got)
	}
	hits := w.hitsOf("/article")
	for _, tenant := range []string{"acme", "only", "stranger"} {
		got = w.as(tenant, "alice", "web_open_url", map[string]any{"url": address})
		if !isErr(got) {
			t.Errorf("%s: read", tenant)
		}
	}
	got = w.as("acme", "alice", "web_open_url", map[string]any{"url": address})
	if !strings.Contains(text(got), "openUrl: true") || !strings.Contains(text(got), "IDP_WEB_TENANTS_FILE") {
		t.Errorf("the sentence does not say what an administrator must change: %s", text(got))
	}
	if w.hitsOf("/article") != hits {
		t.Error("the origin was reached for a tenant that may not open addresses")
	}
	if w.met.calls["refused/web_open_url/open_url_disabled"] == 0 {
		t.Errorf("the refusal is not counted: %v", w.met.calls)
	}
	// it still obeys the address rules and the domain lists
	for _, u := range []string{"http://169.254.169.254/latest/meta-data/", "http://127.0.0.1:1/", "ftp://allowed.test/x"} {
		if got = w.as("opener", "alice", "web_open_url", map[string]any{"url": u}); !isErr(got) {
			t.Errorf("%s was read", u)
		}
	}
}

func TestTheSameAddressKeepsItsNumberAcrossSearchesAndANewOneGetsTheNext(t *testing.T) {
	w := newWorld(t)
	first := w.as("acme", "alice", "web_search", map[string]any{"query": "parcels"})
	second := w.as("acme", "alice", "web_search", map[string]any{"query": "second"})
	if first["structuredContent"] == nil || !strings.HasPrefix(text(second), "[1] Parcels again\n") || !strings.Contains(text(second), "\n\n[2] Another page\n") {
		t.Fatalf("%s", text(second))
	}
	third := w.as("acme", "alice", "web_search", map[string]any{"query": "second", "count": 1})
	if !strings.HasPrefix(text(third), "[1] ") || strings.Contains(text(third), "[2]") {
		t.Fatalf("%s", text(third))
	}
	// reading a page cites it with the number the search gave it
	address, ref := listed(t, second, 1)
	page := w.as("acme", "alice", "web_fetch", map[string]any{"url": address, "ref": ref})
	if c := citations(t, page); len(c) != 1 || c[0]["n"] != float64(2) || !strings.HasPrefix(text(page), "[2] ") {
		t.Fatalf("%v %.80s", c, text(page))
	}
	// numbers are per caller: Bob starts at 1 with the same address, and does not see Alice's numbering
	bob := w.as("acme", "bob", "web_search", map[string]any{"query": "redirect"})
	if !strings.HasPrefix(text(bob), "[1] Goes elsewhere\n") {
		t.Fatalf("%s", text(bob))
	}
	// after an hour without use the numbering is forgotten
	w.clk.advance(time.Hour + time.Minute)
	again := w.as("acme", "alice", "web_search", map[string]any{"query": "second"})
	if !strings.HasPrefix(text(again), "[1] Parcels again\n") {
		t.Fatalf("%s", text(again))
	}
}

func TestOffsetContinuesALongPageFromTheCacheWithoutAnotherRequestToTheOrigin(t *testing.T) {
	w := newWorld(t)
	res := w.as("acme", "alice", "web_search", map[string]any{"query": "second"})
	address, ref := listed(t, res, 0)
	first := text(w.as("acme", "alice", "web_fetch", map[string]any{"url": address, "ref": ref}))
	m := regexp.MustCompile(`of (\d+); the rest: web_fetch with offset (\d+)\)`).FindStringSubmatch(first)
	if m == nil || m[2] != fmt.Sprint(12<<10) {
		t.Fatalf("%.200s", first)
	}
	next := w.as("acme", "alice", "web_fetch", map[string]any{"url": address, "ref": ref, "offset": 12 << 10})
	body := text(next)
	if isErr(next) || !strings.HasPrefix(body, fmt.Sprintf("[1] The Parcel Report (bytes %d–", 12<<10)) {
		t.Fatalf("%.200s", body)
	}
	if w.hitsOf("/article") != 1 {
		t.Errorf("the origin was asked %d times for one page", w.hitsOf("/article"))
	}
	if !strings.Contains(w.log.String(), `"cache":true`) {
		t.Error("the audit line does not say the second part came from the cache")
	}
	// the parts end to end are the page: the last paragraph is in the last part
	if !strings.Contains(body, "Paragraph 069") {
		t.Errorf("the second part does not reach the end of the page")
	}
	// another tenant has its own cache: it reads the page itself
	tn := w.as("opener", "carol", "web_open_url", map[string]any{"url": address})
	if isErr(tn) || w.hitsOf("/article") != 2 {
		t.Errorf("the origin was asked %d times: a tenant was served from another's cache", w.hitsOf("/article"))
	}
	// an hour later the cache has let go of it
	w.clk.advance(time.Hour + time.Minute)
	ref = w.keys.Make("acme/alice", address)
	w.as("acme", "alice", "web_fetch", map[string]any{"url": address, "ref": ref})
	if w.hitsOf("/article") != 3 {
		t.Errorf("the page was served from a cache that is more than an hour old (%d)", w.hitsOf("/article"))
	}
	// search results are never cached
	before := w.prov.calls
	w.as("acme", "alice", "web_search", map[string]any{"query": "second"})
	w.as("acme", "alice", "web_search", map[string]any{"query": "second"})
	if w.prov.calls != before+2 {
		t.Errorf("the provider was asked %d times for two searches", w.prov.calls-before)
	}
}

func TestTheRateLimitIsPerCallerAndTellsTheModelWhenToTryAgain(t *testing.T) {
	w := newWorld(t, options{limits: Limits{Searches: 3, Reads: 2, Window: 10 * time.Minute}})
	for i := 0; i < 3; i++ {
		if isErr(w.as("acme", "alice", "web_search", map[string]any{"query": "second"})) {
			t.Fatalf("search %d was refused", i+1)
		}
	}
	calls := w.prov.calls
	w.clk.advance(4 * time.Minute)
	got := w.as("acme", "alice", "web_search", map[string]any{"query": "second"})
	if !isErr(got) || !strings.Contains(text(got), "try again in 360 seconds") || w.prov.calls != calls {
		t.Fatalf("%s (provider calls %d, was %d)", text(got), w.prov.calls, calls)
	}
	if isErr(w.as("acme", "bob", "web_search", map[string]any{"query": "second"})) {
		t.Error("Bob was limited by Alice's searches")
	}
	w.clk.advance(7 * time.Minute)
	if isErr(w.as("acme", "alice", "web_search", map[string]any{"query": "second"})) {
		t.Error("the window did not slide")
	}
	// reads: two pages, then the third is refused, but the other parts of a page already read are not counted
	a := w.as("acme", "carol", "web_search", map[string]any{"query": "second"})
	addr1, ref1 := listed(t, a, 0)
	addr2, ref2 := listed(t, a, 1)
	for i := 0; i < 3; i++ {
		if isErr(w.as("acme", "carol", "web_fetch", map[string]any{"url": addr1, "ref": ref1, "offset": i * 100})) {
			t.Fatalf("part %d of a cached page was refused", i)
		}
	}
	if isErr(w.as("acme", "carol", "web_fetch", map[string]any{"url": addr2, "ref": ref2})) {
		t.Fatal("the second page was refused")
	}
	third := w.url("allowed.test", "/third")
	got = w.as("acme", "carol", "web_fetch", map[string]any{"url": third, "ref": w.keys.Make("acme/carol", third)})
	if !isErr(got) || !strings.Contains(text(got), "try again in") || w.hitsOf("/third") != 0 {
		t.Fatalf("%v", got)
	}
	if w.met.calls["refused/web_fetch/rate_limited"] != 1 {
		t.Errorf("%v", w.met.calls)
	}
}

func TestTheAuditLineHasTheQueryAndTheAddressAndNeitherTheRefNorAKeyNorPageText(t *testing.T) {
	w := newWorld(t)
	res := w.as("acme", "alice-subject-1234", "web_search", map[string]any{"query": "a secret question about Parcels"})
	address, ref := listed(t, res, 0)
	w.as("acme", "alice-subject-1234", "web_fetch", map[string]any{"url": address, "ref": ref})
	w.as("acme", "alice-subject-1234", "web_fetch", map[string]any{"url": w.url("allowed.test", "/leak?d=SECRET"), "ref": "ffffffff.aaaaaaaaaaaaaaaa"})
	logs := w.log.String()
	for _, must := range []string{"a secret question about Parcels", address, "/leak?d=SECRET", `"tool":"web_search"`, `"tool":"web_fetch"`,
		`"tenant":"acme"`, `"caller":"` + short("acme/alice-subject-1234") + `"`, `"decision":"allowed"`, `"decision":"refused"`, `"reason":"ref_mismatch"`, `"results":1,"dropped":3`} {
		if !strings.Contains(logs, must) {
			t.Errorf("the audit lines lack %s:\n%s", must, logs)
		}
	}
	for _, never := range []string{ref, "ffffffff.aaaaaaaaaaaaaaaa", string(refKey), "alice-subject-1234", "Paragraph 000", "depot sorted"} {
		if strings.Contains(logs, never) {
			t.Errorf("the audit lines contain %q", never)
		}
	}
	// one line per call
	if n := strings.Count(logs, `"msg":"audit"`); n != 3 {
		t.Errorf("%d audit lines for 3 calls", n)
	}
}

// the Exa API is reached for real here, on this machine: what it is sent is what the tools made of the call.
func TestWhatTheProviderIsSentHasTheQueryAndTheTenantsDomainListsButNotWhoAsked(t *testing.T) {
	var mu sync.Mutex
	var raw []string
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		raw = append(raw, r.Method+" "+r.URL.String()+"\n"+fmt.Sprint(r.Header)+"\n"+string(b))
		mu.Unlock()
		io.WriteString(rw, `{"results":[]}`)
	}))
	defer api.Close()
	w := newWorld(t, options{provider: func(search.Provider) search.Provider {
		e, err := search.NewExa(api.URL, "exa-secret", "auto", nil, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}})
	got := w.as("acme", "alice-subject-1234", "web_search", map[string]any{"query": "parcel tracking", "freshness": "week", "count": 3})
	if isErr(got) || text(got) != "no results" {
		t.Fatalf("%v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(raw) != 1 || !strings.Contains(raw[0], `"query":"parcel tracking"`) || !strings.Contains(raw[0], `"excludeDomains":["blocked.test"]`) || !strings.Contains(raw[0], `"numResults":3`) {
		t.Fatalf("%v", raw)
	}
	for _, id := range []string{"acme", "alice", "subject", "1234", "tenant", "caller", "roles", "member"} {
		if strings.Contains(raw[0], id) {
			t.Errorf("the request to the provider contains %q:\n%s", id, raw[0])
		}
	}
}

func TestAProviderFailureReachesTheModelAsOneFixedSentenceAndTheDetailStaysInTheLog(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "SECRET-DETAIL key exa-secret is not valid for acme", http.StatusUnauthorized)
	}))
	defer api.Close()
	var providerLog syncBuf
	w := newWorld(t, options{provider: func(search.Provider) search.Provider {
		e, _ := search.NewExa(api.URL, "exa-secret", "", nil, slog.New(slog.NewJSONHandler(&providerLog, nil)))
		return e
	}})
	got := w.as("acme", "alice", "web_search", map[string]any{"query": "x"})
	if !isErr(got) || text(got) != "the search failed: try again with other words, or answer with what you have" {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(providerLog.String(), "SECRET-DETAIL") || !strings.Contains(providerLog.String(), `"status":401`) {
		t.Errorf("the detail is not in the log: %s", providerLog.String())
	}
}

type brokenReader struct{ err error }

func (b brokenReader) Read(context.Context, string) (fetch.Page, error) { return fetch.Page{}, b.err }

func TestAnErrorNoSentenceIsWrittenForIsLoggedAndTheModelGetsTheGenericSentence(t *testing.T) {
	w := newWorld(t, options{reader: brokenReader{errors.New("dial tcp 10.1.2.3:443: connection refused (internal detail)")}})
	address := w.url("allowed.test", "/article")
	got := w.as("opener", "alice", "web_open_url", map[string]any{"url": address})
	if !isErr(got) || text(got) != "the web tool failed to complete the request" {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(w.log.String(), "internal detail") || !strings.Contains(w.log.String(), `"reason":"internal"`) {
		t.Errorf("%s", w.log.String())
	}
}

func TestTheSitesRefusalsReachTheModelAsSentencesItCanActOn(t *testing.T) {
	w := newWorld(t)
	res := w.as("acme", "alice", "web_search", map[string]any{"query": "gone"})
	address, ref := listed(t, res, 0)
	got := w.as("acme", "alice", "web_fetch", map[string]any{"url": address, "ref": ref})
	if !isErr(got) || text(got) != "the site answered with HTTP status 404: use another source" {
		t.Fatalf("%v", got)
	}
	if w.met.calls["refused/web_fetch/http_status"] != 0 {
		t.Error("a site's error was counted as one of our refusals")
	}
}

// Every error a package can hand the tools has a sentence for the model. When a sentinel is added to a package, it is
// added here, and this fails until a row of mappings exists for it.
func TestEverySentinelHasAFixedSentence(t *testing.T) {
	for _, err := range []error{
		ErrInvalid, ErrNoCaller, ErrNotAvailable, &rateLimited{"searches", time.Minute},
		&quotaExceeded{"searches"}, &quotaUnavailable{"page reads"}, ErrNumberingUnavailable,
		refs.ErrMalformed, refs.ErrExpired, refs.ErrNotForIt,
		tenants.ErrDisabled, tenants.ErrOpenDisabled, tenants.ErrExcluded, tenants.ErrNotIncluded,
		search.ErrUnavailable, search.ErrBusy,
		fetch.ErrNotPublic, fetch.ErrRobots, fetch.ErrTooLarge, fetch.ErrTooSlow, fetch.ErrType, fetch.ErrEncoding,
		fetch.ErrPDFNoText, fetch.ErrPDFEncrypted, fetch.ErrPDFMalformed, fetch.ErrPDFTooLarge, fetch.ErrPDFTooSlow, fetch.ErrPDFUnavailable, fetch.ErrPDFBusy,
		fetch.ErrBotCheck, fetch.ErrNeedsScript, fetch.ErrStatus, fetch.ErrTooManyRedirects, fetch.ErrConnect, fetch.ErrUnreadable,
		fmt.Errorf("%w: the scheme %q is not http or https", errAddrRefused(), "ftp"),
	} {
		o, known := classify(err)
		if !known || o.text == genericText || o.text == "" || o.reason == "internal" {
			t.Errorf("%v has no sentence of its own: %+v", err, o)
		}
	}
	if o, known := classify(errors.New("something else")); known || o.text != genericText {
		t.Errorf("%+v", o)
	}
}

// The gateway pins the schemas, descriptions and annotations at review (ADR-0001 D3). This is their digest as the
// skeleton declared them (task W1): implementing the handlers must not move it.
func TestTheDeclaredSchemasAreTheOnesTheGatewayPinned(t *testing.T) {
	var out []map[string]any
	for _, tool := range declared() {
		out = append(out, map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": tool.InputSchema, "annotations": tool.Annotations})
	}
	b, _ := json.Marshal(out)
	if got := fmt.Sprintf("%x", sha256Sum(string(b))); got != "2275083def9f2057be9fd7ccff385b2936d83af396da7a1aed014715fdfd0bf5" {
		t.Errorf("the declarations changed: digest %s", got)
	}
}

// A read that a rule refuses before it goes out costs the caller nothing of the quota: only what reaches the web is
// counted, so a model that keeps asking for what it may not read is told why instead of being rate limited.
func TestARefusedReadDoesNotUseTheReadQuota(t *testing.T) {
	w := newWorld(t, options{limits: Limits{Searches: 30, Reads: 1, Window: 10 * time.Minute}})
	blocked := w.url("blocked.test", "/page")
	for i := 0; i < 3; i++ {
		got := w.as("acme", "alice", "web_fetch", map[string]any{"url": blocked, "ref": w.keys.Make("acme/alice", blocked)})
		if !isErr(got) || !strings.Contains(text(got), "not allowed for your organisation") {
			t.Fatalf("refusal %d: %s", i+1, text(got))
		}
	}
	good := w.url("allowed.test", "/article")
	if got := w.as("acme", "alice", "web_fetch", map[string]any{"url": good, "ref": w.keys.Make("acme/alice", good)}); isErr(got) {
		t.Fatalf("the one read of the quota was used up by refusals: %s", text(got))
	}
}

// A page that script writes is told to the model in a sentence it can act on, audited as needs_script, and counted by
// itself among the calls the web did not answer, not among our refusals (ADR-0001 D27): its share of web_fetch calls
// is the number that decides the renderer of platform ADR-0003 appendix D.
func TestAPageThatNeedsScriptIsTheModelsSentenceTheAuditReasonAndItsOwnCount(t *testing.T) {
	w := newWorld(t, options{reader: brokenReader{fetch.ErrNeedsScript}})
	got := w.as("opener", "alice", "web_open_url", map[string]any{"url": w.url("allowed.test", "/article")})
	want := "the content of this page is produced by script and cannot be read now: use another source, or the excerpt of the search result"
	if !isErr(got) || text(got) != want {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(w.log.String(), `"decision":"failed","reason":"needs_script"`) {
		t.Errorf("not audited as needs_script: %s", w.log.String())
	}
	if w.met.calls["failed/web_open_url/needs_script"] != 1 || w.met.calls["refused/web_open_url/needs_script"] != 0 {
		t.Errorf("%v", w.met.calls)
	}
}
