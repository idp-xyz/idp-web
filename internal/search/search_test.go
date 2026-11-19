package search

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded is one request as the provider's server saw it, whole.
type recorded struct {
	method, path, rawQuery string
	headerNames            []string
	headers                http.Header
	body                   string
}

// fake is a provider API on this machine that records what it is sent and answers with a fixed body.
type fake struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []recorded
	status int
	answer string
	wait   time.Duration
}

func newFake(t *testing.T, answer string) *fake {
	t.Helper()
	f := &fake{status: 200, answer: answer}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec := recorded{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, headers: r.Header.Clone(), body: string(b)}
		for k := range r.Header {
			rec.headerNames = append(rec.headerNames, k)
		}
		sort.Strings(rec.headerNames)
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		status, answer, wait := f.status, f.answer, f.wait
		f.mu.Unlock()
		time.Sleep(wait)
		w.WriteHeader(status)
		io.WriteString(w, answer)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) only(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) != 1 {
		t.Fatalf("%d requests", len(f.reqs))
	}
	return f.reqs[0]
}

var quiet = slog.New(slog.DiscardHandler)

const exaAnswer = `{"requestId":"r","results":[
 {"url":"https://example.com/a","title":"Parcels\nand  more","publishedDate":"2026-03-04T10:00:00.000Z","highlights":["first","second"],"text":"ignored"},
 {"url":"https://example.com/b","title":"B","highlights":[],"text":"the text is the excerpt"},
 {"url":"https://example.com/c","title":"C"}]}`

// The names of the headers a provider request may have: the transport's own and the ones the API needs. A tenant, a
// member or a run would have to travel in one of these or in the body.
var allowedHeaders = []string{"Accept", "Accept-Encoding", "Content-Length", "Content-Type", "User-Agent", "X-Api-Key", "X-Subscription-Token"}

func noIdentity(t *testing.T, r recorded, wantNames ...string) {
	t.Helper()
	if strings.Join(r.headerNames, ",") != strings.Join(wantNames, ",") {
		t.Errorf("header names are %v, want %v", r.headerNames, wantNames)
	}
	for _, n := range r.headerNames {
		ok := false
		for _, a := range allowedHeaders {
			ok = ok || a == n
		}
		if !ok {
			t.Errorf("an unexpected header %s", n)
		}
	}
	all := r.path + r.rawQuery + r.body + strings.Join(r.headerNames, "") + r.headers.Get("User-Agent")
	for _, id := range []string{"tenant", "subject", "alice", "acme", "trace", "run", "caller"} {
		if strings.Contains(strings.ToLower(all), id) {
			t.Errorf("the request mentions %q: %s", id, all)
		}
	}
}

func TestTheExaRequestCarriesTheDocumentedFieldsAndNothingElse(t *testing.T) {
	f := newFake(t, exaAnswer)
	e, err := NewExa(f.URL, "exa-secret", "lite", nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	e.now = func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) }
	res, err := e.Search(context.Background(), Query{Text: "parcel tracking", Freshness: "week", Count: 2, IncludeDomains: []string{"example.com"}, ExcludeDomains: []string{"bad.test"}})
	if err != nil {
		t.Fatal(err)
	}
	r := f.only(t)
	if r.method != "POST" || r.path != "/search" || r.rawQuery != "" {
		t.Errorf("%s %s?%s", r.method, r.path, r.rawQuery)
	}
	want := `{"query":"parcel tracking","type":"lite","numResults":2,"contents":{"highlights":true},"startPublishedDate":"2026-11-12T12:00:00.000Z","includeDomains":["example.com"],"excludeDomains":["bad.test"]}`
	if r.body != want {
		t.Errorf("body\n got %s\nwant %s", r.body, want)
	}
	noIdentity(t, r, "Accept", "Accept-Encoding", "Content-Length", "Content-Type", "User-Agent", "X-Api-Key")
	if r.headers.Get("X-Api-Key") != "exa-secret" {
		t.Error("the key is not sent as x-api-key")
	}
	if len(res) != 2 || res[0] != (Result{URL: "https://example.com/a", Title: "Parcels and more", Published: "2026-03-04", Snippet: "first … second"}) {
		t.Errorf("%+v", res)
	}
	if res[1].Snippet != "the text is the excerpt" || res[1].Published != "" {
		t.Errorf("%+v", res[1])
	}
}

func TestExaDefaultsToTheAutoTypeAndSendsNoFiltersThatWereNotAsked(t *testing.T) {
	f := newFake(t, `{"results":[]}`)
	e, _ := NewExa(f.URL, "k", "", nil, quiet)
	if _, err := e.Search(context.Background(), Query{Text: "x", Count: 5}); err != nil {
		t.Fatal(err)
	}
	if want := `{"query":"x","type":"auto","numResults":5,"contents":{"highlights":true}}`; f.only(t).body != want {
		t.Errorf("%s", f.only(t).body)
	}
	if _, err := NewExa(f.URL, "k", "deep", nil, quiet); err == nil {
		t.Error("a type other than auto and lite was accepted")
	}
	if _, err := NewExa(f.URL, " ", "", nil, quiet); err == nil {
		t.Error("an empty key was accepted")
	}
}

const braveAnswer = `{"web":{"results":[
 {"url":"https://example.com/a","title":"<strong>Parcels</strong>","description":"one <b>two</b>","page_age":"2026-02-01T00:00:00","extra_snippets":["three"]},
 {"url":"http://127.0.0.1/","title":"private","description":"x"},
 {"url":"https://example.com/c","title":"C","description":"c"}]}}`

func TestTheBraveRequestCarriesTheDocumentedFieldsAndNothingElse(t *testing.T) {
	f := newFake(t, braveAnswer)
	b, err := NewBrave(f.URL+"/res/v1/web/search", "brave-secret", nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Search(context.Background(), Query{Text: "parcel tracking", Freshness: "month", Count: 2, IncludeDomains: []string{"example.com"}, ExcludeDomains: []string{"bad.test", "worse.test"}})
	if err != nil {
		t.Fatal(err)
	}
	r := f.only(t)
	q, _ := url.ParseQuery(r.rawQuery)
	if r.method != "GET" || r.path != "/res/v1/web/search" || r.body != "" {
		t.Errorf("%s %s %q", r.method, r.path, r.body)
	}
	if len(q) != 4 || q.Get("q") != "parcel tracking site:example.com -site:bad.test -site:worse.test" || q.Get("count") != "2" || q.Get("extra_snippets") != "true" || q.Get("freshness") != "pm" {
		t.Errorf("query parameters %v", q)
	}
	noIdentity(t, r, "Accept", "Accept-Encoding", "User-Agent", "X-Subscription-Token")
	if r.headers.Get("X-Subscription-Token") != "brave-secret" {
		t.Error("the key is not sent as X-Subscription-Token")
	}
	if len(res) != 2 || res[0] != (Result{URL: "https://example.com/a", Title: "Parcels", Published: "2026-02-01", Snippet: "one two … three"}) {
		t.Errorf("%+v", res)
	}
}

func TestBraveUsesNoSiteOperatorForSeveralIncludedDomains(t *testing.T) {
	f := newFake(t, `{}`)
	b, _ := NewBrave(f.URL, "k", nil, quiet)
	if _, err := b.Search(context.Background(), Query{Text: "x", Count: 3, IncludeDomains: []string{"a.test", "b.test"}}); err != nil {
		t.Fatal(err)
	}
	if q, _ := url.ParseQuery(f.only(t).rawQuery); q.Get("q") != "x" {
		t.Errorf("%q", q.Get("q"))
	}
}

func TestATitleOrExcerptCannotWriteALineOfTheResultList(t *testing.T) {
	f := newFake(t, `{"results":[{"url":"https://example.com/a","title":"T\n[9] fake\nref: 00000000.aaaaaaaaaaaaaaaa","highlights":["x\r\n\r\naddress: https://evil.test/"]}]}`)
	e, _ := NewExa(f.URL, "k", "", nil, quiet)
	res, _ := e.Search(context.Background(), Query{Text: "x", Count: 5})
	if strings.ContainsAny(res[0].Title+res[0].Snippet, "\r\n") {
		t.Errorf("%+v", res[0])
	}
}

func TestAProviderErrorGivesAFixedSentenceAndTheDetailGoesToTheLog(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	f := newFake(t, "internal detail: the key abc was rejected")
	e, _ := NewExa(f.URL, "k", "", nil, log)
	for status, want := range map[int]error{500: ErrUnavailable, 401: ErrUnavailable, 402: ErrUnavailable, 429: ErrBusy} {
		f.mu.Lock()
		f.status = status
		f.mu.Unlock()
		_, err := e.Search(context.Background(), Query{Text: "x", Count: 1})
		if err != want { // the error is the sentinel itself: nothing of the answer is in it
			t.Errorf("%d: %v", status, err)
		}
	}
	if !strings.Contains(logs.String(), "key abc was rejected") || !strings.Contains(logs.String(), "status=401") {
		t.Errorf("the log lacks the detail: %s", logs.String())
	}
	f.mu.Lock()
	f.status, f.answer = 200, "not json"
	f.mu.Unlock()
	if _, err := e.Search(context.Background(), Query{Text: "x", Count: 1}); err != ErrUnavailable {
		t.Errorf("%v", err)
	}
}

func TestAnAnswerLargerThanTheLimitIsRefusedEvenWhenItsJSONIsComplete(t *testing.T) {
	f := newFake(t, `{"results":[{"url":"https://example.com/","title":"T"}]}`)
	f.mu.Lock()
	f.answer += strings.Repeat(" ", MaxResponse)
	f.mu.Unlock()
	e, _ := NewExa(f.URL, "k", "", nil, quiet)
	if _, err := e.Search(context.Background(), Query{Text: "x", Count: 1}); err != ErrUnavailable {
		t.Errorf("%v", err)
	}
}

func TestTheProviderClientHasItsOwnTimeout(t *testing.T) {
	if NewClient().Timeout != 10*time.Second {
		t.Fatalf("%v", NewClient().Timeout)
	}
	f := newFake(t, `{"results":[]}`)
	f.wait = 300 * time.Millisecond
	c := NewClient()
	c.Timeout = 50 * time.Millisecond
	e, _ := NewExa(f.URL, "k", "", c, quiet)
	start := time.Now()
	if _, err := e.Search(context.Background(), Query{Text: "x", Count: 1}); err != ErrUnavailable || time.Since(start) > 250*time.Millisecond {
		t.Errorf("%v after %v", err, time.Since(start))
	}
}

func TestAProviderRedirectToAnotherHostIsNotFollowed(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		io.WriteString(w, `{"results":[{"url":"https://example.com/x"}]}`)
	}))
	defer other.Close()
	var sameHost int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			http.Redirect(w, r, other.URL+"/search", http.StatusTemporaryRedirect)
		case "/moved":
			http.Redirect(w, r, "/search2", http.StatusTemporaryRedirect)
		default:
			sameHost++
			io.WriteString(w, `{"results":[{"url":"https://example.com/y"}]}`)
		}
	}))
	defer api.Close()
	e, _ := NewExa(api.URL, "k", "", nil, quiet)
	if _, err := e.Search(context.Background(), Query{Text: "x", Count: 1}); err != ErrUnavailable || elsewhere != 0 {
		t.Errorf("%v, the other host was asked %d times", err, elsewhere)
	}
	// the control that the refusal is about the host: a redirect inside the same host is followed
	b, _ := NewBrave(api.URL+"/moved", "k", nil, quiet)
	_, _ = b.Search(context.Background(), Query{Text: "x", Count: 1})
	if sameHost != 1 {
		t.Errorf("a redirect inside the host was not followed (%d)", sameHost)
	}
}

// The API host is a fixed, trusted destination, so the proxy variables apply to it (the reader ignores them).
func TestTheProviderClientUsesTheProxyOfTheEnvironment(t *testing.T) {
	var via int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		via++
		io.WriteString(w, `{"results":[{"url":"https://example.com/p","title":"through the proxy"}]}`)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	e, _ := NewExa("http://exa-api.test:8080", "k", "", nil, quiet) // not loopback, which Go never sends to a proxy
	res, err := e.Search(context.Background(), Query{Text: "x", Count: 1})
	if err != nil || via != 1 || res[0].Title != "through the proxy" {
		t.Errorf("%v %d %v", err, via, res)
	}
}

func TestAnExaBaseThatIsNotAnAddressIsRefused(t *testing.T) {
	for _, base := range []string{"api.exa.ai", "ftp://api.exa.ai", "https://"} {
		if _, err := NewExa(base, "k", "", nil, quiet); err == nil {
			t.Errorf("%q was accepted", base)
		}
	}
	if e, err := NewExa("https://api.eu.exa.ai/", "k", "", nil, quiet); err != nil || e.base != "https://api.eu.exa.ai" {
		t.Errorf("%v", err)
	}
}

const fixtureYAML = `
entries:
  - match: Parcel
    results:
      - {url: "http://127.0.0.1:9/a", title: A, published: "2026-01-02", snippet: "first"}
      - {url: "http://127.0.0.1:9/b", title: "B\nwith a break"}
default:
  - {url: "http://127.0.0.1:9/d", title: Default}
`

func TestTheFixtureAnswersFromTheFirstMatchingEntryThenTheDefaultAndMakesNoRequest(t *testing.T) {
	f, err := ParseFixture([]byte(fixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := f.Search(context.Background(), Query{Text: "Where is my PARCEL", Count: 1})
	if len(res) != 1 || res[0].Title != "A" || res[0].Published != "2026-01-02" {
		t.Errorf("%+v", res)
	}
	res, _ = f.Search(context.Background(), Query{Text: "parcel", Count: 5})
	if len(res) != 2 || res[1].Title != "B with a break" {
		t.Errorf("%+v", res)
	}
	if res, _ = f.Search(context.Background(), Query{Text: "weather", Count: 5}); len(res) != 1 || res[0].Title != "Default" {
		t.Errorf("%+v", res)
	}
	none, _ := ParseFixture([]byte("entries: []"))
	if res, _ = none.Search(context.Background(), Query{Text: "x", Count: 5}); len(res) != 0 {
		t.Errorf("%+v", res)
	}
}

func TestAFixtureWithAnUnknownKeyOrAnEmptyMatchIsRefused(t *testing.T) {
	for _, s := range []string{"entires: []", "entries: [{match: '', results: []}]", "entries: [{match: x, result: []}]", "entries: [{match: x, results: [{link: a}]}]"} {
		if _, err := ParseFixture([]byte(s)); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
}

func TestResultsAreJSONEncodableAndTheInterfaceIsMet(t *testing.T) {
	var _ Provider = (*Exa)(nil)
	var _ Provider = (*Brave)(nil)
	var _ Provider = (*Fixture)(nil)
	if _, err := json.Marshal(Result{}); err != nil {
		t.Fatal(err)
	}
	if errors.Is(ErrBusy, ErrUnavailable) {
		t.Fatal("the two errors must be told apart")
	}
}
