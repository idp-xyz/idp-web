package tools

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idp-web/internal/browsertest"
	"idp-web/internal/pdfread"
	"idp-web/internal/render"
)

// The tests of the renderer's side (ADR-0001 D33): the real tools and the real reader, a renderer double that verifies the
// signature exactly as idp-browser does and answers what the test scripts, and the test origin of e2e_test.go.

const renderTenants = `
default: {enabled: false}
tenants:
  acme: {enabled: true, openUrl: true, excludeDomains: [blocked.test], alwaysRender: [rendered.test]}
  quota: {enabled: true, openUrl: true, dailyReads: 2}
  plain: {enabled: true, openUrl: true}
`

const needsScriptSentence = "the content of this page is produced by script and cannot be read now: use another source, or the excerpt of the search result"
const blockedSentence = "the site refused to show this page to an automated reader: use another source"

type renderRig struct {
	w *world
	d *browsertest.Double
	c *render.Client
}

// shows is the script of a renderer that runs the page and gets the long article: the page the plain reader could not see.
func shows(_ *world, r browsertest.Request) browsertest.Answer {
	return browsertest.Answer{FinalURL: r.URL, HTML: article(), Title: "The Parcel Report", MS: 123}
}

func newRenderRig(t *testing.T, script func(*world, browsertest.Request) browsertest.Answer, o ...options) *renderRig {
	t.Helper()
	priv, pub := browsertest.GenerateKey(t)
	signer, err := render.NewSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	var w *world
	d := browsertest.New(t, pub, func(r browsertest.Request) browsertest.Answer { return script(w, r) })
	c, err := render.New(render.Config{URL: d.URL(), Signer: signer, TimeoutMS: 500, MaxBytes: 1 << 20, Margin: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var opt options
	if len(o) > 0 {
		opt = o[0]
	}
	opt.tenants, opt.renderer = renderTenants, c
	w = newWorld(t, opt)
	return &renderRig{w, d, c}
}

func (r *renderRig) open(tenant, host, path string) map[string]any {
	return r.w.as(tenant, "alice", "web_open_url", map[string]any{"url": r.w.url(host, path)})
}

// With no renderer a page that needs script is answered as before, and the renderer is never called.
func TestWithNoRendererAPageThatNeedsScriptIsAnsweredAsBeforeAndNothingIsCalled(t *testing.T) {
	r := newRenderRig(t, shows)
	w2 := newWorld(t, options{tenants: renderTenants})
	res := w2.as("acme", "alice", "web_open_url", map[string]any{"url": w2.url("allowed.test", "/spa")})
	if !isErr(res) || text(res) != needsScriptSentence {
		t.Fatalf("%s", text(res))
	}
	if !strings.Contains(w2.log.String(), `"reason":"needs_script"`) || !strings.Contains(w2.log.String(), `"rendered":false`) {
		t.Errorf("%s", w2.log.String())
	}
	if r.d.Calls() != 0 || w2.met.calls["rendered/ok"] != 0 {
		t.Errorf("the renderer was called %d times", r.d.Calls())
	}
	// and a tenant's alwaysRender does nothing: the listed host is read as any other
	res = w2.as("acme", "alice", "web_open_url", map[string]any{"url": w2.url("rendered.test", "/article")})
	if isErr(res) || strings.Contains(text(res), "rendered;") {
		t.Errorf("%.200s", text(res))
	}
}

// A page that needs script is rendered once, by a request the renderer's own verifier accepts, and what comes back is read
// like a fetched page: the text, the citation under the address that was asked for with where the read ended, the hash of
// the part, and a mark that it was rendered, in the header and in the structured field.
func TestAPageThatNeedsScriptIsRenderedOnceAndReadCitedAndMarkedRendered(t *testing.T) {
	r := newRenderRig(t, func(w *world, q browsertest.Request) browsertest.Answer {
		return browsertest.Answer{FinalURL: w.url("allowed.test", "/spa-final"), HTML: article(), Title: "The Parcel Report", MS: 123}
	})
	asked := r.w.url("allowed.test", "/spa")
	res := r.open("acme", "allowed.test", "/spa")
	if isErr(res) {
		t.Fatal(text(res))
	}
	body := text(res)
	head, part, _ := strings.Cut(body, "\n\n")
	if !strings.HasPrefix(head, "[1] The Parcel Report [the address redirected to "+r.w.url("allowed.test", "/spa-final")+"] (rendered; bytes 0–12288 of ") || !strings.Contains(part, "Paragraph 000") {
		t.Fatalf("%.300s", body)
	}
	c := citations(t, res)
	if len(c) != 1 || c[0]["url"] != asked || c[0]["finalUrl"] != r.w.url("allowed.test", "/spa-final") || c[0]["rendered"] != true || c[0]["sha256"] != fmt.Sprintf("%x", sha(part)) {
		t.Errorf("citation %v", c)
	}
	if got := r.d.Served(); len(got) != 1 || got[0].URL != asked || got[0].TimeoutMS != 500 || got[0].MaxBytes != 1<<20 {
		t.Errorf("the renderer was asked %+v", got)
	}
	if len(r.d.Refused()) != 0 {
		t.Errorf("refused: %v", r.d.Refused())
	}
	log := r.w.log.String()
	for _, want := range []string{`"rendered":true`, `"render":"ok"`, `"renderMs":123`, `"decision":"allowed","reason":"ok"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the audit line lacks %s: %s", want, log)
		}
	}
	if r.w.met.calls["rendered/ok"] != 1 {
		t.Errorf("%v", r.w.met.calls)
	}
	// a page that did not need a render is not marked
	if plain := r.open("acme", "allowed.test", "/article"); strings.Contains(text(plain), "rendered") || citations(t, plain)[0]["rendered"] != nil {
		t.Errorf("a fetched page is marked rendered: %.200s", text(plain))
	}
}

// The renderer ran the page and it is still a frame (D27's rules, applied to what the browser made): the model reads the
// needs_script sentence and the audit says render_failed. It is asked once.
func TestAPageThatIsStillAFrameAfterRenderingIsNeedsScriptWithTheReasonRenderFailed(t *testing.T) {
	frame := `<!doctype html><html><body><div id="root"></div><script src="/app.js"></script></body></html>`
	for _, host := range []string{"allowed.test", "rendered.test"} { // the second is listed: rendered first, and the plain read does not do better
		r := newRenderRig(t, func(_ *world, q browsertest.Request) browsertest.Answer {
			return browsertest.Answer{FinalURL: q.URL, HTML: frame}
		})
		res := r.open("acme", host, "/spa")
		if !isErr(res) || text(res) != needsScriptSentence {
			t.Fatalf("%s: %s", host, text(res))
		}
		if !strings.Contains(r.w.log.String(), `"decision":"failed","reason":"render_failed"`) || !strings.Contains(r.w.log.String(), `"render":"thin"`) {
			t.Errorf("%s: %s", host, r.w.log.String())
		}
		if n := len(r.d.Served()); n != 1 {
			t.Errorf("%s: the renderer was called %d times for one read", host, n)
		}
		if r.w.met.calls["rendered/thin"] != 1 || r.w.met.calls["failed/web_open_url/render_failed"] != 1 {
			t.Errorf("%v", r.w.met.calls)
		}
	}
}

// Each answer of the renderer has its outcome: the code is the audit reason, the model reads the needs_script sentence
// (blocked_by_site has a sentence of its own), and the read is still counted or not as the renderer started work or not.
func TestEachRendererErrorIsTheRightOutcomeAndAuditReason(t *testing.T) {
	for _, tc := range []struct {
		name, code, reason, sentence string
	}{
		{"busy", "render_busy", "render_busy", needsScriptSentence},
		{"timeout", "render_timeout", "render_timeout", needsScriptSentence},
		{"failed", "render_failed", "render_failed", needsScriptSentence},
		{"not allowed", "not_allowed", "not_allowed", needsScriptSentence},
		{"invalid", "invalid_request", "invalid_request", needsScriptSentence},
		{"blocked", "blocked_by_site", "blocked_by_site", blockedSentence},
	} {
		r := newRenderRig(t, func(*world, browsertest.Request) browsertest.Answer { return browsertest.Answer{Error: tc.code} })
		res := r.open("acme", "allowed.test", "/spa")
		if !isErr(res) || text(res) != tc.sentence {
			t.Errorf("%s: %s", tc.name, text(res))
		}
		if !strings.Contains(r.w.log.String(), `"decision":"failed","reason":"`+tc.reason+`"`) || !strings.Contains(r.w.log.String(), `"render":"`+tc.code+`"`) {
			t.Errorf("%s: %s", tc.name, r.w.log.String())
		}
		if r.w.met.calls["failed/web_open_url/"+tc.reason] != 1 || r.w.met.calls["rendered/"+tc.code] != 1 {
			t.Errorf("%s: %v", tc.name, r.w.met.calls)
		}
	}
	// the key the renderer holds is not ours: the renderer says unauthorized, and so does the audit
	r := newRenderRig(t, shows)
	_, other := browsertest.GenerateKey(t)
	r.d.SetKey(other)
	res := r.open("acme", "allowed.test", "/spa")
	if !isErr(res) || text(res) != needsScriptSentence || !strings.Contains(r.w.log.String(), `"reason":"unauthorized"`) {
		t.Errorf("%s: %s", text(res), r.w.log.String())
	}
	if len(r.d.Served()) != 0 || len(r.d.Refused()) != 1 {
		t.Errorf("served %v refused %v", r.d.Served(), r.d.Refused())
	}
	// no renderer at all: the same sentence, the reason says it
	r = newRenderRig(t, shows)
	r.d.Close()
	res = r.open("acme", "allowed.test", "/spa")
	if !isErr(res) || text(res) != needsScriptSentence || !strings.Contains(r.w.log.String(), `"reason":"render_unreachable"`) {
		t.Errorf("%s: %s", text(res), r.w.log.String())
	}
}

// A renderer that is down is never a reason to fail the other reads, and a listed host whose render fails is still read.
func TestARendererThatIsDownOrFailingFailsNothingElse(t *testing.T) {
	r := newRenderRig(t, shows)
	r.d.Close()
	for _, p := range []string{"/article", "/other"} {
		if res := r.open("acme", "allowed.test", p); isErr(res) {
			t.Errorf("%s: %s", p, text(res))
		}
	}
	// a listed host: the render fails, the page as fetched is returned, not marked
	res := r.open("acme", "rendered.test", "/article")
	if isErr(res) || strings.Contains(text(res), "rendered;") || citations(t, res)[0]["rendered"] != nil {
		t.Errorf("%.200s", text(res))
	}
	if !strings.Contains(r.w.log.String(), `"rendered":false,"render":"render_unreachable"`) {
		t.Errorf("the failed render is not in the audit: %s", r.w.log.String())
	}
}

// A renderer that hangs is cut off at its limit and the margin, and the read answers needs_script; another read goes on.
func TestARendererThatHangsIsBoundedByTheTimeoutAndTheReadAnswersNeedsScript(t *testing.T) {
	r := newRenderRig(t, func(_ *world, q browsertest.Request) browsertest.Answer {
		if strings.HasSuffix(q.URL, "/spa") {
			return browsertest.Answer{Hang: true}
		}
		return shows(nil, q)
	})
	start := time.Now()
	res := r.open("acme", "allowed.test", "/spa")
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("the read took %v", el)
	}
	if !isErr(res) || text(res) != needsScriptSentence || !strings.Contains(r.w.log.String(), `"reason":"render_timeout"`) {
		t.Errorf("%s: %s", text(res), r.w.log.String())
	}
	if res := r.open("acme", "allowed.test", "/article"); isErr(res) {
		t.Errorf("the next read: %s", text(res))
	}
}

// A refusal in words on the plain path is blocked_by_site, and the renderer is not asked: a browser would be shown the
// same sentence. The same sentence comes from the renderer's own blocked_by_site.
func TestASoftBlockOnThePlainPathIsBlockedBySiteAndIsNotRendered(t *testing.T) {
	r := newRenderRig(t, shows)
	res := r.open("acme", "allowed.test", "/fedex")
	if !isErr(res) || text(res) != blockedSentence {
		t.Fatalf("%s", text(res))
	}
	if !strings.Contains(r.w.log.String(), `"decision":"failed","reason":"blocked_by_site"`) || r.w.met.calls["failed/web_open_url/blocked_by_site"] != 1 {
		t.Errorf("%s %v", r.w.log.String(), r.w.met.calls)
	}
	if r.d.Calls() != 0 {
		t.Errorf("the renderer was called for a refusal page")
	}
	// also with no renderer configured
	w := newWorld(t, options{tenants: renderTenants})
	if res := w.as("acme", "alice", "web_open_url", map[string]any{"url": w.url("allowed.test", "/fedex")}); text(res) != blockedSentence {
		t.Errorf("%s", text(res))
	}
}

// alwaysRender: the listed host (and its subdomains) is rendered although the plain reader had a page; no other host is.
func TestAlwaysRenderRendersTheListedHostFirstAndNoOtherHost(t *testing.T) {
	r := newRenderRig(t, shows)
	res := r.open("acme", "rendered.test", "/article")
	if isErr(res) || !strings.Contains(text(res), "(rendered; bytes") || citations(t, res)[0]["rendered"] != true {
		t.Fatalf("%.200s", text(res))
	}
	if n := len(r.d.Served()); n != 1 {
		t.Fatalf("%d renders", n)
	}
	for _, host := range []string{"allowed.test", "other.test", "127.0.0.1"} {
		if res := r.open("acme", host, "/article"); isErr(res) || strings.Contains(text(res), "rendered;") {
			t.Errorf("%s: %.200s", host, text(res))
		}
	}
	// another tenant that lists nothing: the same host is read plain
	if res := r.open("plain", "rendered.test", "/article"); isErr(res) || strings.Contains(text(res), "rendered;") {
		t.Errorf("%.200s", text(res))
	}
	if n := len(r.d.Served()); n != 1 {
		t.Errorf("%d renders for the 1 listed read", n)
	}
}

// A browser given a file answers render_failed, so only HTML is sent to it: a PDF, plain text and another type on a listed
// host are read by the plain reader, and a PDF that is not a page never goes to the renderer on any path.
func TestOnlyHTMLIsEverSentToTheRenderer(t *testing.T) {
	pdf := pdfFake{pdfread.Output{Title: "Rate guide", Pages: 2, PagesRead: 2, Text: "[page 1]\nTariff text."}}
	r := newRenderRig(t, shows, options{pdf: pdf})
	res := r.open("acme", "rendered.test", "/file.pdf")
	if isErr(res) || !strings.Contains(text(res), "(PDF, 2 pages; bytes") || strings.Contains(text(res), "rendered;") {
		t.Errorf("a PDF of a listed host: %.200s", text(res))
	}
	res = r.open("acme", "rendered.test", "/notes.txt")
	if isErr(res) || !strings.Contains(text(res), "Plain notes") || strings.Contains(text(res), "rendered;") {
		t.Errorf("plain text of a listed host: %.200s", text(res))
	}
	res = r.open("acme", "rendered.test", "/blob")
	if !isErr(res) || !strings.Contains(text(res), "content type") {
		t.Errorf("another type of a listed host: %.200s", text(res))
	}
	// an error status page of a listed host is not rendered either
	res = r.open("acme", "rendered.test", "/gone")
	if !isErr(res) || !strings.Contains(text(res), "HTTP status 404") {
		t.Errorf("%.200s", text(res))
	}
	// and not on the needs_script path: a PDF there is the PDF reader's, whatever it answers
	for _, p := range []string{"/file.pdf", "/notes.txt", "/blob"} {
		r.open("acme", "allowed.test", p)
	}
	if n := r.d.Calls(); n != 0 {
		t.Errorf("the renderer was called %d times for files: %v", n, r.d.Served())
	}
	// without a PDF reader the PDF of a listed host is the PDF reader's refusal, not a render
	r2 := newRenderRig(t, shows)
	if res := r2.open("acme", "rendered.test", "/file.pdf"); !isErr(res) || !strings.Contains(text(res), "PDF") || r2.d.Calls() != 0 {
		t.Errorf("%s (%d renders)", text(res), r2.d.Calls())
	}
}

type pdfFake struct{ out pdfread.Output }

func (p pdfFake) Read(context.Context, []byte) (pdfread.Output, error) { return p.out, nil }

// A render is a read: one unit of the daily quota and one of the window, however many requests it took.
func TestARenderCountsAsOneReadOnceInTheDailyQuotaAndTheWindow(t *testing.T) {
	fs := &fakeState{used: map[string]int{}}
	fs.numbers = newSources(time.Now)
	r := newRenderRig(t, shows, options{state: fs, limits: Limits{Searches: 30, Reads: 50, Window: 10 * time.Minute}})
	for _, p := range []string{"/spa", "/spa?x=1"} {
		if res := r.open("quota", "allowed.test", p); isErr(res) {
			t.Fatalf("%s: %s", p, text(res))
		}
	}
	if n := len(r.d.Served()); n != 2 {
		t.Errorf("%d renders", n)
	}
	if fs.takes != 2 || fs.releases != 0 {
		t.Errorf("two renders took %d units and gave %d back", fs.takes, fs.releases)
	}
	if got := r.open("quota", "allowed.test", "/spa?x=2"); !isErr(got) || !strings.Contains(text(got), "daily limit") {
		t.Errorf("the third read of a limit of two: %s", text(got))
	}
	if n := len(r.d.Served()); n != 2 {
		t.Errorf("a refused read rendered: %d", n)
	}
	// the window is counted once as well: a window of two reads, one render of a listed host (plain read and render) is one
	r = newRenderRig(t, shows, options{limits: Limits{Searches: 30, Reads: 2, Window: 10 * time.Minute}})
	for _, p := range []string{"/article", "/other"} {
		if res := r.open("acme", "rendered.test", p); isErr(res) {
			t.Fatalf("%s: %s", p, text(res))
		}
	}
}

// The rendered result is cached like a fetched one, by the address that was asked for and the tenant, with its mark: the
// next part is served from the cache, marked rendered, with no second render and no second unit.
func TestARenderedPageIsCachedWithItsMarkAndTheNextPartDoesNotRenderAgain(t *testing.T) {
	fs := &fakeState{used: map[string]int{}}
	fs.numbers = newSources(time.Now)
	r := newRenderRig(t, shows, options{state: fs})
	first := r.open("quota", "allowed.test", "/spa")
	if isErr(first) {
		t.Fatal(text(first))
	}
	res := r.w.as("quota", "alice", "web_open_url", map[string]any{"url": r.w.url("allowed.test", "/spa"), "offset": 12288})
	if isErr(res) || !strings.Contains(text(res), "(rendered; bytes 12288–13158 of 13158") || citations(t, res)[0]["rendered"] != true {
		t.Fatalf("%.200s", text(res))
	}
	if n := len(r.d.Served()); n != 1 || fs.takes != 1 {
		t.Errorf("%d renders, %d units", n, fs.takes)
	}
	log := r.w.log.String()
	last := log[strings.LastIndex(log, `"msg":"audit"`):]
	if !strings.Contains(last, `"cache":true`) || !strings.Contains(last, `"rendered":true`) {
		t.Errorf("the audit line of the cache hit: %s", last)
	}
	// another tenant does not get it
	if other := r.open("plain", "allowed.test", "/spa"); isErr(other) && len(r.d.Served()) != 2 {
		t.Errorf("another tenant was served from the first one's cache")
	}
}

// The browser followed redirects of its own: where it ended passes the rules again, before a word of the page is read.
func TestAPageThatEndedWhereTheTenantMayNotReadIsRefusedAndNothingOfItIsKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		to   func(w *world) string
		want string
	}{
		{"an excluded domain", func(w *world) string { return w.url("blocked.test", "/page") }, "not allowed for your organisation"},
		{"robots.txt of the final host", func(w *world) string { return w.url("robots.test", "/page") }, "robots.txt"},
		{"a private address", func(*world) string { return "http://10.0.0.1/x" }, "not"},
	} {
		r := newRenderRig(t, func(w *world, q browsertest.Request) browsertest.Answer {
			return browsertest.Answer{FinalURL: tc.to(w), HTML: article()}
		})
		res := r.open("acme", "allowed.test", "/spa")
		if !isErr(res) || !strings.Contains(text(res), tc.want) || strings.Contains(text(res), "Paragraph") {
			t.Errorf("%s: %.200s", tc.name, text(res))
		}
		if r.w.met.calls["rendered/refused"] != 1 {
			t.Errorf("%s: %v", tc.name, r.w.met.calls)
		}
		if again := r.open("acme", "allowed.test", "/spa"); !isErr(again) || len(r.d.Served()) != 2 {
			t.Errorf("%s: a refused page was cached", tc.name)
		}
	}
}

// The robots.txt of a listed host is obeyed before anything is sent to the renderer, and the tenant's lists as well.
func TestTheChecksOfAPlainReadComeBeforeTheRenderer(t *testing.T) {
	r := newRenderRig(t, shows)
	if res := r.open("acme", "robots.test", "/spa"); !isErr(res) || !strings.Contains(text(res), "robots.txt") {
		t.Errorf("%s", text(res))
	}
	if res := r.open("acme", "blocked.test", "/spa"); !isErr(res) {
		t.Errorf("%s", text(res))
	}
	if r.d.Calls() != 0 {
		t.Errorf("the renderer was called for a page that may not be read")
	}
}

// One render per call, even when both the listed host and the plain reader's needs_script would ask for one.
func TestAToolCallRendersAtMostOnce(t *testing.T) {
	var asked atomic.Int32
	r := newRenderRig(t, func(_ *world, q browsertest.Request) browsertest.Answer {
		asked.Add(1)
		return browsertest.Answer{Error: "render_failed"}
	})
	r.open("acme", "rendered.test", "/spa") // listed, and a frame: the render fails, the plain read says needs_script
	if asked.Load() != 1 {
		t.Errorf("%d renders for one call", asked.Load())
	}
}

// A page the browser ended on with an error status is the status error, as a fetched one would be.
func TestARenderedPageWithAnErrorStatusIsTheStatusError(t *testing.T) {
	r := newRenderRig(t, func(_ *world, q browsertest.Request) browsertest.Answer {
		return browsertest.Answer{FinalURL: q.URL, HTML: article(), Status: 404}
	})
	res := r.open("acme", "allowed.test", "/spa")
	if !isErr(res) || !strings.Contains(text(res), "HTTP status 404") || !strings.Contains(r.w.log.String(), `"reason":"http_status"`) || r.w.met.calls["rendered/http_status"] != 1 {
		t.Errorf("%s %s", text(res), r.w.log.String())
	}
}
