package tools

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"idp-web/internal/fetch"
)

func TestThePageCacheIsBoundedInEntriesAndBytesAndDropsTheLeastRecentlyUsed(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	page := func(n int) cached {
		return cached{page: fetch.Page{Text: strings.Repeat("x", n)}, fetchedAt: clk.now()}
	}
	c := newPageCache(clk.now, 3, 10_000)
	for i := 0; i < 3; i++ {
		c.put("t", fmt.Sprint(i), page(100))
	}
	c.get("t", "0") // 0 is now the most recently used, 1 the least
	c.put("t", "3", page(100))
	if _, ok := c.get("t", "1"); ok {
		t.Error("the least recently used page was kept over the limit of entries")
	}
	if _, ok := c.get("t", "0"); !ok {
		t.Error("a page that was just used was dropped")
	}
	big := newPageCache(clk.now, 100, 2000)
	for i := 0; i < 10; i++ {
		big.put("t", fmt.Sprint(i), page(700))
	}
	if big.bytes > 2000 || big.ll.Len() > 2 {
		t.Errorf("%d bytes in %d entries", big.bytes, big.ll.Len())
	}
	big.put("t", "huge", page(5000))
	if _, ok := big.get("t", "huge"); ok {
		t.Error("a page larger than the whole cache was kept")
	}
	if _, ok := c.get("other", "0"); ok {
		t.Error("a tenant was served another tenant's page")
	}
	clk.advance(time.Hour + time.Second)
	if _, ok := c.get("t", "0"); ok {
		t.Error("a page older than an hour was served")
	}
}

func TestANumberIsNeverGivenToTwoAddressesEvenWhenOneIsForgotten(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	s := newSources(clk.now)
	s.maxURLs = 3
	seen := map[int]string{}
	for i := 0; i < 10; i++ {
		u := fmt.Sprint("https://example.com/", i)
		n := s.number("t/a", u)
		if old, ok := seen[n]; ok && old != u {
			t.Fatalf("the number %d names %s and %s", n, old, u)
		}
		seen[n] = u
	}
	s.maxCallers = 2
	s.number("t/b", "u")
	s.number("t/c", "u")
	s.number("t/d", "u")
	if len(s.callers) > 2 {
		t.Errorf("%d callers are kept", len(s.callers))
	}
}

func TestTheLimiterSlidesAndRefusesTheCallerOverTheLimitWithoutCountingTheRefusal(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	l := newLimiter(clk.now, 10*time.Minute, map[string]int{"searches": 2, "reads": 1})
	for i := 0; i < 2; i++ {
		if w := l.allow("a", "searches"); w != 0 {
			t.Fatal(w)
		}
		clk.advance(time.Minute)
	}
	if w := l.allow("a", "searches"); w != 8*time.Minute {
		t.Errorf("wait %v", w)
	}
	if w := l.allow("a", "reads"); w != 0 {
		t.Errorf("reads share the quota of searches: %v", w)
	}
	clk.advance(8 * time.Minute)
	if w := l.allow("a", "searches"); w != 0 {
		t.Errorf("after the wait: %v", w)
	}
}
