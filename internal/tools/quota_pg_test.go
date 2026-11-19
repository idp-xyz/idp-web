package tools

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"idp-web/internal/mcp"
	"idp-web/internal/pgtest"
	"idp-web/internal/state"
)

// The tools with the real State on a real PostgreSQL (make test-integration; skipped without one): two replicas of the
// service, each with its own store, one database.

const pgTenants = tenantsYAML + `
  big: {enabled: true, openUrl: true, dailySearches: 30, dailyReads: 2}
  free: {enabled: true, openUrl: true}
`

func TestTwoReplicasOfTheServiceShareOneTenantsDailySearchesExactly(t *testing.T) {
	_, url := pgtest.Scratch(t)
	a, b := pgtest.Open(t, url+"&pool_max_conns=16", state.Options{}), pgtest.Open(t, url+"&pool_max_conns=16", state.Options{})
	w := newWorld(t, options{tenants: pgTenants, state: a})
	replicas := []*mcp.Server{w.srv, w.replica(b)}
	for day := 0; day < 3; day++ { // three days, each a fresh quota of 30, so the same race is run three times
		before := w.prov.calls
		var ok, refused atomic.Int64
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < 40; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < 4; i++ {
					// a caller of its own for each call: the sliding window is per caller and per replica and not what is tested here
					res := w.asOn(replicas[(g+i)%2], "big", fmt.Sprintf("caller-%d-%d", g, i), "web_search", map[string]any{"query": "parcels"})
					switch {
					case !isErr(res):
						ok.Add(1)
					case strings.Contains(text(res), "daily limit of searches for your organisation is used up"):
						refused.Add(1)
					default:
						t.Errorf("%s", text(res))
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		if ok.Load() != 30 || refused.Load() != 130 || w.prov.calls-before != 30 {
			t.Fatalf("day %d: %d searches allowed, %d refused, the provider called %d times; the quota is 30", day, ok.Load(), refused.Load(), w.prov.calls-before)
		}
		w.clk.advance(24 * time.Hour)
	}
}

func TestTheNumbersAreTheSameOnAnotherReplicaAndAfterARestart(t *testing.T) {
	_, url := pgtest.Scratch(t)
	a, b := pgtest.Open(t, url, state.Options{}), pgtest.Open(t, url, state.Options{})
	w := newWorld(t, options{tenants: pgTenants, state: a})
	one := w.as("free", "alice", "web_search", map[string]any{"query": "second"})
	address, ref := listed(t, one, 1)
	// the other replica reads what the first listed: the citation carries the number the first gave it
	page := w.asOn(w.replica(b), "free", "alice", "web_fetch", map[string]any{"url": address, "ref": ref})
	if c := citations(t, page); len(c) != 1 || c[0]["n"] != float64(2) {
		t.Fatalf("%v: %s", c, text(page))
	}
	// another caller starts at 1; the same caller's new address gets the next number, whichever replica it meets it on
	if got := w.asOn(w.replica(b), "free", "bob", "web_search", map[string]any{"query": "parcels"}); !strings.Contains(text(got), "[1] ") {
		t.Fatalf("%s", text(got))
	}
	third := w.asOn(w.replica(b), "free", "alice", "web_search", map[string]any{"query": "parcels"})
	if !strings.Contains(text(third), "[3] Blocked parcels") && !strings.Contains(text(third), "[3] Parcels, a history") {
		t.Fatalf("%s", text(third))
	}
	// a restart: the first replica's process ends and a new one starts on the same database
	a.Close()
	c := pgtest.Open(t, url, state.Options{})
	again := w.asOn(w.replica(c), "free", "alice", "web_search", map[string]any{"query": "second"})
	if !strings.Contains(text(again), "[1] Parcels again") || !strings.Contains(text(again), "[2] Another page") {
		t.Fatalf("numbering changed across the restart: %s", text(again))
	}
}

// Failed reads count in the database as they do in the fake: the count is what the real statement keeps.
func TestReadsThatWentOutButFailedAreCountedInTheDatabaseAndTheLimitHolds(t *testing.T) {
	_, url := pgtest.Scratch(t)
	w := newWorld(t, options{tenants: pgTenants, state: pgtest.Open(t, url, state.Options{})})
	for i := 0; i < 2; i++ {
		if got := openURL(w, "big", "/gone"); !isErr(got) || strings.Contains(text(got), "daily limit") {
			t.Fatalf("read %d: %s", i, text(got))
		}
	}
	got := openURL(w, "big", "/article")
	if !isErr(got) || !strings.Contains(text(got), "daily limit of page reads") {
		t.Fatalf("a third read after two that failed at the site: %s", text(got))
	}
	if w.hitsOf("/article") != 0 {
		t.Error("the page was fetched over the limit")
	}
}

// The database goes away: the call under a limit is refused at once with its own reason and nothing goes out; a tenant with no
// limit gets as far as the numbering and no further.
func TestWhenTheDatabaseIsGoneACallUnderALimitIsRefusedAndNothingGoesOut(t *testing.T) {
	_, url := pgtest.Scratch(t)
	st := pgtest.Open(t, url, state.Options{Timeout: 500 * time.Millisecond})
	w := newWorld(t, options{tenants: pgTenants, state: st})
	if got := w.as("big", "alice", "web_search", map[string]any{"query": "parcels"}); isErr(got) {
		t.Fatalf("%s", text(got))
	}
	st.Close()
	start := time.Now()
	got := w.as("big", "alice", "web_search", map[string]any{"query": "parcels"})
	if !isErr(got) || !strings.Contains(text(got), "could not be checked just now, so the call was not made") {
		t.Fatalf("%s", text(got))
	}
	if got := openURL(w, "big", "/article"); !isErr(got) || !strings.Contains(text(got), "page reads could not be checked") {
		t.Fatalf("%s", text(got))
	}
	if w.prov.calls != 1 || w.hitsOf("/article") != 0 {
		t.Errorf("provider %d, origin %d: something went out with no way to count it", w.prov.calls, w.hitsOf("/article"))
	}
	if got := w.as("free", "alice", "web_search", map[string]any{"query": "parcels"}); !isErr(got) || !strings.Contains(text(got), "could not be numbered") {
		t.Fatalf("%s", text(got))
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if w.met.calls["refused/web_search/quota_unavailable"] != 1 || w.met.calls["failed/web_search/numbering_unavailable"] != 1 {
		t.Errorf("%v", w.met.calls)
	}
}
