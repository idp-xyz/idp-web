package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-web/internal/fetch"
	"idp-web/internal/search"
	"idp-web/internal/state"
)

// These tests are of the daily quota as the tools use it, with a fake State: which calls take a unit, in which order, which
// outcomes keep it and which give it back, what the model is told, and what happens when the database does not answer. The
// statement itself, on a real PostgreSQL, is tested in internal/state and in quota_pg_test.go.

// quotaTenants adds to the tenants of the end-to-end test: one with a limit of two of each, one with a limit of 0, one with a
// limit of one read that also excludes a domain, one with no limit.
const quotaTenants = tenantsYAML + `
  quota: {enabled: true, openUrl: true, dailySearches: 2, dailyReads: 2}
  zero: {enabled: true, openUrl: true, dailySearches: 0, dailyReads: 0}
  one: {enabled: true, openUrl: true, excludeDomains: [blocked.test], dailySearches: 1, dailyReads: 1}
  free: {enabled: true, openUrl: true}
`

// fakeState is a State in memory that counts what it is asked and can be made to fail, as a database that is down does.
type fakeState struct {
	mu       sync.Mutex
	used     map[string]int // tenant/kind/day
	takes    int
	releases int
	numbered int
	down     bool
	numbers  *sources
}

var errDown = errors.New("connection refused")

func key(tenant, kind string, now time.Time) string {
	return tenant + "/" + kind + "/" + now.UTC().Format("2006-01-02")
}

func (f *fakeState) Take(_ context.Context, tenant, kind string, now time.Time, limit int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takes++
	if f.down {
		return false, errDown
	}
	k := key(tenant, kind, now)
	if limit <= 0 || f.used[k] >= limit {
		return false, nil
	}
	f.used[k]++
	return true, nil
}

func (f *fakeState) Release(_ context.Context, tenant, kind string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	if f.down {
		return errDown
	}
	if k := key(tenant, kind, now); f.used[k] > 0 {
		f.used[k]--
	}
	return nil
}

func (f *fakeState) Number(ctx context.Context, caller string, addrs []string, now time.Time) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.numbered++
	if f.down {
		return nil, errDown
	}
	return f.numbers.Number(ctx, caller, addrs, now)
}

func (f *fakeState) usedBy(tenant, kind string, now time.Time) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used[key(tenant, kind, now)]
}

func (f *fakeState) setDown(d bool) { f.mu.Lock(); f.down = d; f.mu.Unlock() }

// quotaWorld is the end-to-end world with the quota tenants and a fake State that shares the world's clock.
func quotaWorld(t *testing.T, o options) (*world, *fakeState) {
	t.Helper()
	fs := &fakeState{used: map[string]int{}}
	o.tenants, o.state = quotaTenants, fs
	w := newWorld(t, o)
	fs.numbers = newSources(w.clk.now) // numbers as the process would without a database: what is asked of it is the test
	return w, fs
}

// pageReader answers with a page, or with err, and tells the call's context that it connected when dialed is true, the way
// the real reader does (fetch.Dialing).
type pageReader struct {
	mu     sync.Mutex
	calls  int
	err    error
	dialed bool
}

func (p *pageReader) Read(ctx context.Context, rawURL string) (fetch.Page, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.dialed {
		fetch.Dialing(ctx) // as the real reader does just before it connects
	}
	if p.err != nil {
		return fetch.Page{}, p.err
	}
	return fetch.Page{Title: "A page", FinalURL: rawURL, Text: "Some text of the page."}, nil
}

func (p *pageReader) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

func openURL(w *world, tenant, path string) map[string]any {
	return w.as(tenant, "alice", "web_open_url", map[string]any{"url": w.url("allowed.test", path)})
}

func TestASearchOverTheDailyQuotaIsRefusedWithASentenceTheModelCanActOnAndTheProviderIsNotCalled(t *testing.T) {
	w, fs := quotaWorld(t, options{})
	for i := 1; i <= 2; i++ {
		if got := w.as("quota", "alice", "web_search", map[string]any{"query": "parcels"}); isErr(got) {
			t.Fatalf("search %d of 2: %s", i, text(got))
		}
	}
	got := w.as("quota", "alice", "web_search", map[string]any{"query": "parcels"})
	want := "the daily limit of searches for your organisation is used up; it resets at 00:00 UTC: do not try again today, and answer with what you have"
	if !isErr(got) || text(got) != want {
		t.Fatalf("%v", got)
	}
	if w.prov.calls != 2 {
		t.Errorf("the provider was called %d times for a quota of 2", w.prov.calls)
	}
	if !strings.Contains(w.log.String(), `"decision":"refused","reason":"quota_exceeded"`) {
		t.Errorf("not audited as quota_exceeded: %s", w.log.String())
	}
	if w.met.calls["refused/web_search/quota_exceeded"] != 1 {
		t.Errorf("%v", w.met.calls)
	}
	// another caller of the same tenant shares the tenant's count; another tenant has its own
	if got := w.as("quota", "bob", "web_search", map[string]any{"query": "parcels"}); !isErr(got) {
		t.Error("the quota is the tenant's, and a second caller of it got a search")
	}
	if got := w.as("free", "alice", "web_search", map[string]any{"query": "parcels"}); isErr(got) {
		t.Errorf("a tenant with no limit was refused: %s", text(got))
	}
	if fs.usedBy("quota", state.Searches, w.clk.now()) != 2 {
		t.Errorf("used %d", fs.usedBy("quota", state.Searches, w.clk.now()))
	}
}

// The day is the UTC day of the clock: at 00:00 UTC the limit is there again.
func TestTheQuotaIsThereAgainAtMidnightUTC(t *testing.T) {
	w, _ := quotaWorld(t, options{})
	search := func() bool {
		return !isErr(w.as("quota", "alice", "web_search", map[string]any{"query": "parcels"}))
	}
	w.clk.advance(11*time.Hour + 59*time.Minute + 59*time.Second) // 23:59:59 on the 19th
	if !search() || !search() || search() {
		t.Fatal("two searches of a day were not exactly two")
	}
	w.clk.advance(time.Second) // 00:00:00 on the 20th
	if !search() {
		t.Fatal("the next day did not start at zero")
	}
}

// A search counts from the moment the provider is called, whatever the provider answers: the outward request was made.
func TestASearchThatTheProviderAnswersWithAnErrorStillCounts(t *testing.T) {
	for name, err := range map[string]error{
		"busy":        fmt.Errorf("%w: status 429", search.ErrBusy),
		"unavailable": fmt.Errorf("%w: status 500", search.ErrUnavailable),
		"timeout":     context.DeadlineExceeded,
		"unknown":     errors.New("something unexpected"),
	} {
		t.Run(name, func(t *testing.T) {
			var calls int
			w, fs := quotaWorld(t, options{provider: func(search.Provider) search.Provider {
				return providerFunc(func() ([]search.Result, error) { calls++; return nil, err })
			}})
			for i := 0; i < 2; i++ {
				if got := w.as("quota", "alice", "web_search", map[string]any{"query": "parcels"}); !isErr(got) || strings.Contains(text(got), "daily limit") {
					t.Fatalf("search %d: %s", i+1, text(got))
				}
			}
			if got := w.as("quota", "alice", "web_search", map[string]any{"query": "parcels"}); !strings.Contains(text(got), "daily limit") {
				t.Fatalf("the third search after two provider errors was not refused for the quota: %s", text(got))
			}
			if calls != 2 || fs.usedBy("quota", state.Searches, w.clk.now()) != 2 {
				t.Errorf("provider called %d times, %d used", calls, fs.usedBy("quota", state.Searches, w.clk.now()))
			}
		})
	}
}

type providerFunc func() ([]search.Result, error)

func (f providerFunc) Search(context.Context, search.Query) ([]search.Result, error) { return f() }

// A read that went out counts, however it ended; one that sent nothing gives its unit back. "Sent" is the reader's word
// (fetch.WithDialed): it was about to connect to an address that had passed the address rules.
func TestAReadCountsWhenItWentOutWhateverItEndedWithAndNotWhenNothingLeftTheService(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		dialed bool
		used   int
	}{
		{"a page", nil, true, 1},
		{"the site answers with an error status", fetch.ErrStatus, true, 1},
		{"the connection fails after the first request", fetch.ErrConnect, true, 1},
		{"the site is too slow", fetch.ErrTooSlow, true, 1},
		{"robots.txt does not allow it", fetch.ErrRobots, true, 1},
		{"the page is too large", fetch.ErrTooLarge, true, 1},
		{"the page needs script", fetch.ErrNeedsScript, true, 1},
		{"a redirect to a private address after the first request went out", fetch.ErrNotPublic, true, 1},
		{"the address resolves inside: refused before connecting", fetch.ErrNotPublic, false, 0},
		{"the name does not resolve: nothing connected", fetch.ErrConnect, false, 0},
		{"the deadline passed before the first connection", fetch.ErrTooSlow, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			rd := &pageReader{err: c.err, dialed: c.dialed}
			w, fs := quotaWorld(t, options{reader: rd})
			openURL(w, "quota", "/article")
			if got := fs.usedBy("quota", state.Reads, w.clk.now()); got != c.used {
				t.Errorf("%d units used, want %d", got, c.used)
			}
			if fs.takes != 1 {
				t.Errorf("%d takes", fs.takes)
			}
			if (c.used == 0) != (fs.releases == 1) {
				t.Errorf("%d releases", fs.releases)
			}
		})
	}
}

// The same with the real reader and a real origin: a missing page counts; an address that resolves to a private address does
// not; a redirect to one, after the first request was made, does.
func TestTheRealReaderTellsWhichReadsSentSomething(t *testing.T) {
	names := namesTo{"intranet.example": "10.0.0.5", "metadata.example": "169.254.169.254", "allowed.test": "127.0.0.1"}
	var w *world
	var fs *fakeState
	rdr := func() Reader { // the reader is made once the origin's port is known: the world builds the origin
		ports := w.port
		return fetch.New(fetch.Config{Resolver: names, TestOrigins: map[string]bool{"allowed.test:" + ports: true}, Version: "test", Log: slog.New(slog.DiscardHandler)})
	}
	late := &lateReader{}
	w, fs = quotaWorld(t, options{reader: late})
	late.r = rdr()
	for _, c := range []struct {
		name, url string
		used      int
	}{
		{"a page that is not there", w.url("allowed.test", "/gone"), 1},
		{"a page", w.url("allowed.test", "/article"), 1},
		{"a name that resolves to a private address", "http://intranet.example/", 0},
		{"a redirect to the metadata address, after the first request went out", w.url("allowed.test", "/to?to=http://metadata.example/latest"), 1},
	} {
		w.clk.advance(24 * time.Hour) // a day of its own for each, so that the quota of two never gets in the way
		got := w.as("quota", "alice", "web_open_url", map[string]any{"url": c.url})
		after := fs.usedBy("quota", state.Reads, w.clk.now())
		if after != c.used {
			t.Errorf("%s: %d used (%s)", c.name, after, text(got))
		}
	}
}

// lateReader is a Reader whose reader is set after the world exists.
type lateReader struct{ r Reader }

func (l *lateReader) Read(ctx context.Context, u string) (fetch.Page, error) { return l.r.Read(ctx, u) }

// What a refusal or a failure before the outward call does not touch: no unit is taken for a call that is refused by its
// arguments, its tenant, its ref, the address rules and the tenant's lists, or the sliding window.
func TestACallThatIsRefusedBeforeItGoesOutDoesNotTouchTheQuota(t *testing.T) {
	w, fs := quotaWorld(t, options{limits: Limits{Searches: 1, Reads: 1, Window: 10 * time.Minute}})
	blocked := w.url("blocked.test", "/page")
	good := w.url("allowed.test", "/article")
	for name, got := range map[string]map[string]any{
		"an invalid argument":        w.as("quota", "alice", "web_search", map[string]any{"query": ""}),
		"a tenant that is not on":    w.as("off", "alice", "web_search", map[string]any{"query": "parcels"}),
		"a ref that does not match":  w.as("quota", "alice", "web_fetch", map[string]any{"url": good, "ref": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}),
		"an excluded domain":         w.as("one", "alice", "web_open_url", map[string]any{"url": blocked}),
		"an address that is refused": w.as("quota", "alice", "web_open_url", map[string]any{"url": "ftp://example.com/"}),
	} {
		if !isErr(got) {
			t.Errorf("%s was not refused", name)
		}
	}
	if fs.takes != 0 {
		t.Errorf("%d units were taken by calls that never went out", fs.takes)
	}
	// the sliding window is checked before the quota: the second search in the window is refused by it, and takes nothing
	w.as("quota", "carol", "web_search", map[string]any{"query": "parcels"})
	taken := fs.takes
	if got := w.as("quota", "carol", "web_search", map[string]any{"query": "parcels"}); !strings.Contains(text(got), "too many searches") {
		t.Fatalf("%s", text(got))
	}
	if fs.takes != taken {
		t.Error("a search refused by the sliding window took a unit of the daily quota")
	}
}

// Only a read that goes out counts: the parts of a page that was read are served from the cache and cost nothing.
func TestACacheHitDoesNotCountAgainstTheDailyReads(t *testing.T) {
	w, fs := quotaWorld(t, options{})
	u := w.url("allowed.test", "/article")
	first := w.as("one", "alice", "web_open_url", map[string]any{"url": u})
	if isErr(first) || !strings.Contains(text(first), "the rest: web_open_url with offset") {
		t.Fatalf("the article should have a second part: %s", text(first)[:200])
	}
	for i, off := range []int{12000, 12000, 0} {
		if got := w.as("one", "alice", "web_open_url", map[string]any{"url": u, "offset": off}); isErr(got) {
			t.Fatalf("read %d from the cache was refused: %s", i, text(got))
		}
	}
	if fs.usedBy("one", state.Reads, w.clk.now()) != 1 || fs.takes != 1 {
		t.Errorf("%d used, %d takes: the cache hits were counted", fs.usedBy("one", state.Reads, w.clk.now()), fs.takes)
	}
	if w.hitsOf("/article") != 1 {
		t.Errorf("the origin was asked %d times", w.hitsOf("/article"))
	}
	// the limit of one is used: another address is refused, and the cached one is still served
	if got := w.as("one", "alice", "web_open_url", map[string]any{"url": w.url("allowed.test", "/other")}); !strings.Contains(text(got), "daily limit of page reads") {
		t.Errorf("%s", text(got))
	}
	if got := w.as("one", "alice", "web_open_url", map[string]any{"url": u, "offset": 12000}); isErr(got) {
		t.Errorf("a page already read was refused for the quota: %s", text(got))
	}
	if w.met.calls["refused/web_open_url/quota_exceeded"] != 1 {
		t.Errorf("%v", w.met.calls)
	}
}

// A limit of 0 allows none, and says so without asking the database.
func TestALimitOfZeroAllowsNoneAndDoesNotAskTheDatabase(t *testing.T) {
	w, fs := quotaWorld(t, options{})
	for tool, args := range map[string]any{"web_search": map[string]any{"query": "parcels"}, "web_open_url": map[string]any{"url": w.url("allowed.test", "/article")}} {
		got := w.as("zero", "alice", tool, args)
		if !isErr(got) || !strings.Contains(text(got), "daily limit") {
			t.Errorf("%s: %s", tool, text(got))
		}
	}
	if fs.takes != 0 || w.prov.calls != 0 || w.hitsOf("/article") != 0 {
		t.Errorf("takes %d, provider %d, origin %d", fs.takes, w.prov.calls, w.hitsOf("/article"))
	}
}

// The database does not answer: the call is refused with its own reason and nothing goes out. A tenant with no limit does
// not ask the database for the quota at all.
func TestWhenTheDatabaseDoesNotAnswerACallUnderALimitIsRefusedAndNothingGoesOut(t *testing.T) {
	rd := &pageReader{}
	w, fs := quotaWorld(t, options{reader: rd})
	fs.setDown(true)
	got := w.as("quota", "alice", "web_search", map[string]any{"query": "parcels"})
	want := "the daily limit of searches could not be checked just now, so the call was not made: try again in a little while, or answer with what you have"
	if !isErr(got) || text(got) != want {
		t.Fatalf("%v", got)
	}
	got = openURL(w, "quota", "/article")
	if !isErr(got) || !strings.Contains(text(got), "daily limit of page reads could not be checked") {
		t.Fatalf("%s", text(got))
	}
	if w.prov.calls != 0 || rd.count() != 0 {
		t.Errorf("the provider was called %d times and the reader %d times with no way to count them", w.prov.calls, rd.count())
	}
	if w.met.calls["refused/web_search/quota_unavailable"] != 1 || w.met.calls["refused/web_open_url/quota_unavailable"] != 1 {
		t.Errorf("%v", w.met.calls)
	}
	if !strings.Contains(w.log.String(), `"decision":"refused","reason":"quota_unavailable"`) {
		t.Errorf("not audited as quota_unavailable: %s", w.log.String())
	}
	// the database comes back and the same call goes through: nothing was counted while it was down
	fs.setDown(false)
	if got := w.as("quota", "alice", "web_search", map[string]any{"query": "parcels"}); isErr(got) {
		t.Errorf("%s", text(got))
	}
	if fs.usedBy("quota", state.Searches, w.clk.now()) != 1 {
		t.Errorf("used %d", fs.usedBy("quota", state.Searches, w.clk.now()))
	}
}

// A tenant that has no limit never asks for the quota. What it still needs the database for is the numbering of the sources
// (ADR-0001 D29): with the database down its search says so, and a page it has read is kept, so that the retry goes out
// to the site no second time.
func TestATenantWithNoLimitNeverAsksForTheQuotaAndNeedsTheDatabaseOnlyToNumberTheSources(t *testing.T) {
	w, fs := quotaWorld(t, options{})
	if got := w.as("free", "alice", "web_search", map[string]any{"query": "parcels"}); isErr(got) {
		t.Fatalf("%s", text(got))
	}
	if fs.takes != 0 {
		t.Fatalf("a tenant with no limit asked for the quota %d times", fs.takes)
	}
	fs.setDown(true)
	got := w.as("free", "alice", "web_search", map[string]any{"query": "parcels"})
	if !isErr(got) || !strings.Contains(text(got), "could not be numbered") {
		t.Fatalf("%v", got)
	}
	if fs.takes != 0 {
		t.Errorf("%d takes", fs.takes)
	}
	if !strings.Contains(w.log.String(), `"decision":"failed","reason":"numbering_unavailable"`) {
		t.Errorf("not audited as numbering_unavailable: %s", w.log.String())
	}
	got = openURL(w, "free", "/article")
	if !isErr(got) || !strings.Contains(text(got), "could not be numbered") || w.hitsOf("/article") != 1 {
		t.Fatalf("%s (origin asked %d times)", text(got), w.hitsOf("/article"))
	}
	fs.setDown(false)
	if got := openURL(w, "free", "/article"); isErr(got) || w.hitsOf("/article") != 1 {
		t.Errorf("the retry: %s (origin asked %d times, once is right: the page was cached)", text(got), w.hitsOf("/article"))
	}
}

// With no database configured at all, a tenant with a limit is refused rather than served without one (the service
// does not start in that case: cmd/idp-web), and numbering is the in-memory one.
func TestWithNoDatabaseATenantWithALimitIsRefusedAndOthersAreNumberedInMemory(t *testing.T) {
	w := newWorld(t, options{tenants: quotaTenants})
	if got := w.as("quota", "alice", "web_search", map[string]any{"query": "parcels"}); !strings.Contains(text(got), "could not be checked") {
		t.Errorf("%s", text(got))
	}
	if w.prov.calls != 0 {
		t.Error("the provider was called with no way to count")
	}
	res := w.as("free", "alice", "web_search", map[string]any{"query": "second"})
	if isErr(res) || !strings.Contains(text(res), "[1] ") || !strings.Contains(text(res), "[2] ") {
		t.Errorf("%s", text(res))
	}
}

// The numbers the model reads are the State's, asked once per search for all the listed addresses in the order listed, for
// the caller's tenant and subject.
func TestTheNumbersComeFromTheStateInTheOrderListed(t *testing.T) {
	numbering := &fixedNumbers{from: 40}
	w := newWorld(t, options{tenants: quotaTenants, state: numbering})
	got := w.as("free", "alice", "web_search", map[string]any{"query": "second"})
	if isErr(got) || !strings.Contains(text(got), "[40] Parcels again") || !strings.Contains(text(got), "[41] Another page") {
		t.Fatalf("%s", text(got))
	}
	if len(numbering.addrs) != 1 || len(numbering.addrs[0]) != 2 || numbering.callers[0] != "free/alice" {
		t.Errorf("asked %v for %v", numbering.addrs, numbering.callers)
	}
	for _, a := range numbering.addrs[0] {
		if !strings.HasPrefix(a, "http://allowed.test:") {
			t.Errorf("asked for %q", a)
		}
	}
	// a read of one of them asks for the number of that one address and cites what the State said
	address, ref := listed(t, got, 1)
	page := w.as("free", "alice", "web_fetch", map[string]any{"url": address, "ref": ref})
	if c := citations(t, page); len(c) != 1 || c[0]["n"] != float64(40) || len(numbering.addrs) != 2 || len(numbering.addrs[1]) != 1 {
		t.Errorf("%v %v: %s", c, numbering.addrs, text(page))
	}
}

// fixedNumbers is a State that numbers from a number of its own and records what it was asked. It takes nothing.
type fixedNumbers struct {
	fakeState
	from    int
	callers []string
	addrs   [][]string
}

func (f *fixedNumbers) Number(_ context.Context, caller string, addrs []string, _ time.Time) ([]int, error) {
	f.callers = append(f.callers, caller)
	f.addrs = append(f.addrs, addrs)
	out := make([]int, len(addrs))
	for i := range addrs {
		out[i] = f.from + i
	}
	return out, nil
}
