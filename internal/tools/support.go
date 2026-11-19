package tools

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"idp-web/internal/fetch"
)

// sources numbers the addresses a caller has been shown, so that an answer's [n] names one source whatever search
// or page it came from (ADR-0001 D19). A number is given to an address the first time the caller meets it and the
// address keeps it; a new address gets the next free number. Numbers are never reused, so a number never names two
// addresses, even after an address has been forgotten.
//
// This is the numbering when no database is configured: the table lives in this process's memory, per caller, forgotten
// after an hour without use and bounded in callers and in addresses per caller. That is right for one replica; with
// several, or after a restart, the numbering would diverge. With a database the same rules are kept there, for every
// replica and across restarts (internal/state, ADR-0001 D29).
type sources struct {
	mu         sync.Mutex
	now        func() time.Time
	ttl        time.Duration
	maxCallers int
	maxURLs    int
	callers    map[string]*numbering
}

type numbering struct {
	next int
	urls map[string]*numbered
	last time.Time
}

type numbered struct {
	n    int
	used time.Time
}

func newSources(now func() time.Time) *sources {
	return &sources{now: now, ttl: time.Hour, maxCallers: 10000, maxURLs: 1000, callers: map[string]*numbering{}}
}

// Number is state.Store.Number for a process with no database: the numbers of the addresses, in the order asked.
func (s *sources) Number(_ context.Context, caller string, addresses []string, _ time.Time) ([]int, error) {
	out := make([]int, len(addresses))
	for i, a := range addresses {
		out[i] = s.number(caller, a)
	}
	return out, nil
}

// number returns the caller's number for the address, giving it one when it has none.
func (s *sources) number(caller, address string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	c := s.callers[caller]
	if c != nil && now.Sub(c.last) > s.ttl {
		c = nil // an hour without use: forgotten, the numbering starts again
	}
	if c == nil {
		if len(s.callers) >= s.maxCallers {
			s.evictCallers(now)
		}
		c = &numbering{next: 1, urls: map[string]*numbered{}}
		s.callers[caller] = c
	}
	c.last = now
	if e := c.urls[address]; e != nil {
		e.used = now
		return e.n
	}
	if len(c.urls) >= s.maxURLs { // forget the address that was used longest ago; its number is not given again
		var oldest string
		var at time.Time
		for u, e := range c.urls {
			if oldest == "" || e.used.Before(at) {
				oldest, at = u, e.used
			}
		}
		delete(c.urls, oldest)
	}
	e := &numbered{n: c.next, used: now}
	c.next++
	c.urls[address] = e
	return e.n
}

// evictCallers makes room: first the callers unused for an hour, then, if that frees nothing, the one unused longest.
func (s *sources) evictCallers(now time.Time) {
	var oldest string
	var at time.Time
	for id, c := range s.callers {
		if now.Sub(c.last) > s.ttl {
			delete(s.callers, id)
		} else if oldest == "" || c.last.Before(at) {
			oldest, at = id, c.last
		}
	}
	if len(s.callers) >= s.maxCallers && oldest != "" {
		delete(s.callers, oldest)
	}
}

// cached is a page as it was read, with the moment it was.
type cached struct {
	page      fetch.Page
	fetchedAt time.Time
}

// pageCache keeps pages already read, per tenant and by the address that was asked for, for an hour, so that the
// next part of a long page does not read it again (platform ADR-0003 D10, appendix C 4.5). It is bounded in entries and
// in bytes, and drops the least recently used first. Search results are never kept in it: the providers' terms
// generally forbid storing them.
type pageCache struct {
	mu         sync.Mutex
	now        func() time.Time
	ttl        time.Duration
	maxEntries int
	maxBytes   int
	bytes      int
	ll         *list.List // front is the most recently used
	byKey      map[string]*list.Element
}

type cacheEntry struct {
	key  string
	size int
	val  cached
}

func newPageCache(now func() time.Time, maxEntries, maxBytes int) *pageCache {
	return &pageCache{now: now, ttl: time.Hour, maxEntries: maxEntries, maxBytes: maxBytes, ll: list.New(), byKey: map[string]*list.Element{}}
}

func cacheKey(tenant, address string) string {
	return fmt.Sprintf("%d:%s\x00%s", len(tenant), tenant, address)
}

func (c *pageCache) get(tenant, address string) (cached, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.byKey[cacheKey(tenant, address)]
	if el == nil {
		return cached{}, false
	}
	e := el.Value.(*cacheEntry)
	if c.now().Sub(e.val.fetchedAt) > c.ttl {
		c.remove(el)
		return cached{}, false
	}
	c.ll.MoveToFront(el)
	return e.val, true
}

func (c *pageCache) put(tenant, address string, v cached) {
	size := len(v.page.Text) + len(v.page.Title) + len(v.page.FinalURL) + len(address) + 256
	if size > c.maxBytes {
		return // a page that alone would fill the cache is not kept
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey(tenant, address)
	if el := c.byKey[key]; el != nil {
		c.remove(el)
	}
	c.byKey[key] = c.ll.PushFront(&cacheEntry{key: key, size: size, val: v})
	c.bytes += size
	for c.ll.Len() > c.maxEntries || c.bytes > c.maxBytes {
		c.remove(c.ll.Back())
	}
}

func (c *pageCache) remove(el *list.Element) {
	e := c.ll.Remove(el).(*cacheEntry)
	delete(c.byKey, e.key)
	c.bytes -= e.size
}

// limiter is a sliding window per caller and kind of call, in memory (appendix C 2 item 4): at most max calls in any
// window. It cannot count per run, because the caller's assertion carries no run number; the HOST's step limit bounds
// a run.
type limiter struct {
	mu     sync.Mutex
	now    func() time.Time
	window time.Duration
	max    map[string]int
	hits   map[string][]time.Time
}

func newLimiter(now func() time.Time, window time.Duration, max map[string]int) *limiter {
	return &limiter{now: now, window: window, max: max, hits: map[string][]time.Time{}}
}

// allow records a call of kind by caller and returns 0, or the time to wait when the caller is over the limit (the
// call is then not recorded).
func (l *limiter) allow(caller, kind string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := kind + "\x00" + caller
	h := l.hits[key]
	i := 0
	for i < len(h) && now.Sub(h[i]) >= l.window {
		i++
	}
	h = h[i:]
	if len(h) >= l.max[kind] {
		l.hits[key] = h
		return h[0].Add(l.window).Sub(now)
	}
	if len(l.hits) > 10000 { // bound the table: drop the callers whose calls are all out of the window
		for k, hs := range l.hits {
			if len(hs) == 0 || now.Sub(hs[len(hs)-1]) >= l.window {
				delete(l.hits, k)
			}
		}
	}
	l.hits[key] = append(h, now)
	return 0
}
