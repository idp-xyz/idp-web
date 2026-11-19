package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// fakeResolver answers from a table, counts every lookup and can change its answer on later lookups (a rebinding
// attacker's name).
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr // per name: the answer of the first lookup, of the second, ...; the last repeats
	calls   map[string]int
}

func answers(host string, rounds ...string) *fakeResolver {
	f := &fakeResolver{answers: map[string][][]netip.Addr{}, calls: map[string]int{}}
	f.add(host, rounds...)
	return f
}

// add registers a name; each round is a comma-separated list of addresses.
func (f *fakeResolver) add(host string, rounds ...string) *fakeResolver {
	for _, r := range rounds {
		var as []netip.Addr
		for _, a := range strings.Split(r, ",") {
			as = append(as, netip.MustParseAddr(strings.TrimSpace(a)))
		}
		f.answers[host] = append(f.answers[host], as)
	}
	return f
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rounds, ok := f.answers[host]
	if !ok {
		return nil, fmt.Errorf("no such host %s", host)
	}
	i := f.calls[host]
	f.calls[host]++
	if i >= len(rounds) {
		i = len(rounds) - 1
	}
	return rounds[i], nil
}

func (f *fakeResolver) lookups(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[host]
}

// env is a reader wired to a test server, which is its test origin: a real listener on 127.0.0.1 that the reader may
// reach only because of IDP_WEB_TEST_ORIGINS, and nothing else.
type env struct {
	t      *testing.T
	r      *Reader
	srv    *httptest.Server
	res    *fakeResolver
	hits   *hitCounter
	mu     sync.Mutex
	dialed []string
}

type hitCounter struct {
	mu    sync.Mutex
	paths []string
	heads []http.Header
}

func (h *hitCounter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.URL.RequestURI())
		h.heads = append(h.heads, r.Header.Clone())
		h.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (h *hitCounter) count(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.paths {
		if p == path {
			n++
		}
	}
	return n
}

func (h *hitCounter) total() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.paths)
}

// newEnv serves h (robots.txt answers 404 unless h says otherwise).
func newEnv(t *testing.T, h http.Handler) *env {
	t.Helper()
	e := &env{t: t, res: &fakeResolver{answers: map[string][][]netip.Addr{}, calls: map[string]int{}}, hits: &hitCounter{}}
	e.srv = httptest.NewServer(e.hits.wrap(h))
	t.Cleanup(e.srv.Close)
	e.r = New(Config{
		Resolver:    e.res,
		TestOrigins: map[string]bool{e.srv.Listener.Addr().String(): true},
		Version:     "test",
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return e
}

// routes is a handler of fixed answers by path; robots.txt is a 404 when it has no entry.
type routes map[string]http.HandlerFunc

func (m routes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h, ok := m[r.URL.Path]; ok {
		h(w, r)
		return
	}
	http.NotFound(w, r)
}

func (e *env) url(path string) string { return e.srv.URL + path }

// trackDials makes the reader connect to the test server whatever address was checked, and records the address the
// reader asked for.
func (e *env) trackDials() {
	e.r.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		e.mu.Lock()
		e.dialed = append(e.dialed, address)
		e.mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, e.srv.Listener.Addr().String())
	}
}

func (e *env) dials() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.dialed...)
}

func html(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, body)
	}
}

func redirect(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) }
}

func is(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error %v, want %v", err, want)
	}
}

const articleBody = `<!doctype html><html><head><title>The Parcel Report</title></head><body>
<nav><a href="/">Home</a> <a href="/about">About</a></nav>
<article><h1>The Parcel Report</h1>
<p>The depot sorted forty thousand parcels on Tuesday, which is the most it has sorted in a single day since the opening of the new belt.</p>
<p>Managers said the second belt was the reason, and that the night shift would stay at its current size for the rest of the quarter.</p>
<p>Drivers reported fewer delays at the loading bays, and customers who track their parcels saw the new status pages quickly.</p>
</article><footer>Copyright footer</footer></body></html>`
