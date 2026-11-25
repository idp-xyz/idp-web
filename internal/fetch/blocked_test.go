package fetch

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The S4 cases of idp-browser's internal/render/detect_test.go (the visible text of each, and the near misses), so that the
// renderer and this reader agree on what a refusal page is.
func TestTheRefusalTextIsTheOneIdpBrowserReportsAsS4AndALongArticleThatQuotesItIsNotOne(t *testing.T) {
	long := strings.Repeat("This is a long article about web security and CAPTCHAs, access denied errors and more. ", 40)
	for _, tc := range []struct {
		name, text string
		blocked    bool
	}{
		{"akamai errors page (HTTP 200)", "Access Denied You don't have permission to access \"http://x/\" on this server. Reference #18.4a3d1c02.1795089600.abc123", true},
		{"fedex soft block (HTTP 200)", "We're sorry, we can't process your request right now. It appears you don't have permission to view this webpage. Incident Number: 0.11a6b4d.1795089600.5f2c", true},
		{"curly apostrophe and odd spacing", "We\u2019re sorry, we can\u2019t   process your request right now.\n It appears you don\u2019t have permission to view this webpage.", true},
		{"curly quotes and a modifier apostrophe", "\u201cYou don\u02bct have permission to access\u201d this on this server. Reference #18.4a3d1c02.1795089600.abc123", true},
		{"imperva", "Request unsuccessful. Incapsula incident ID: 1234-5678", true},
		{"a bare Akamai reference", "Reference #18.4a3d1c02.1795089600.abc123", true},
		{"chinese", "您的访问行为异常，请稍后再试", true},
		{"a long article that quotes the akamai message", long + " You don't have permission to access this on this server. Reference #18.4a3d1c02.1795089600.abc123", false},
		{"a long article about captchas", long, false},
		{"a normal short page", "Hello, world. This page is short and fine.", false},
		{"an empty page", "", false},
	} {
		if got := refusalSignal(tc.text) != ""; got != tc.blocked {
			t.Errorf("%s: blocked=%v, want %v", tc.name, got, tc.blocked)
		}
	}
}

const fedexPage = `<!doctype html><html><head><title>FedEx</title></head><body><div><h1>We're sorry, we can't process your request right now.</h1>
<p>It appears you don't have permission to view this webpage.</p><p>Incident Number: 0.11a6b4d.1795089600.5f2c</p></div></body></html>`

// The page found against www.fedex.com from the development network: HTTP 200 and a sentence of refusal. It used to be
// read as the page.
func TestAnAccessDeniedPageThatAnswers200IsBlockedBySiteNotAPage(t *testing.T) {
	status := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(code)
			io.WriteString(w, body)
		}
	}
	akamai := `<html><head><title>Access Denied</title></head><body><h1>Access Denied</h1>You don't have permission to access "http://x/" on this server.<p>Reference #18.4a3d1c02.1795089600.abc123</p></body></html>`
	quoting := `<html><head><title>Debugging 403s</title></head><body><article>` + strings.Repeat("<p>An article on what a web application firewall answers when it refuses a request, and how to read the logs afterwards.</p>", 20) +
		`<p>Some say: you don't have permission to access this on this server. Reference #18.4a3d1c02.1795089600.abc123</p></article></body></html>`
	e := newEnv(t, routes{"/fedex": status(200, fedexPage), "/akamai200": status(200, akamai), "/akamai403": status(403, akamai), "/plain403": status(403, "<html><body><p>Forbidden</p></body></html>"),
		"/quoting": status(200, quoting)})
	for _, p := range []string{"/fedex", "/akamai200", "/akamai403"} {
		_, err := e.r.Read(context.Background(), e.url(p))
		if !is2(err, ErrBlockedBySite) || is2(err, ErrBotCheck) || is2(err, ErrNeedsScript) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := e.r.Read(context.Background(), e.url("/plain403")); !is2(err, ErrStatus) {
		t.Errorf("a bare 403: %v", err)
	}
	if p, err := e.r.Read(context.Background(), e.url("/quoting")); err != nil || p.Text == "" {
		t.Errorf("an article that quotes the refusal: %v", err)
	}
}

// The renderer is asked only about pages that were HTML: WithHTML is how the tools know.
func TestWithHTMLIsCalledOnlyForAnAnswerReadAsAnHTMLPage(t *testing.T) {
	typed := func(ct string, code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(code)
			io.WriteString(w, body)
		}
	}
	e := newEnv(t, routes{
		"/html":    typed("text/html", 200, articleBody),
		"/xhtml":   typed("application/xhtml+xml", 200, articleBody),
		"/frame":   typed("text/html", 200, `<html><body><div id="root"></div><script src="/a.js"></script></body></html>`),
		"/pdf":     typed("application/pdf", 200, "%PDF-1.4 x"),
		"/pdfhtml": typed("text/html", 200, "%PDF-1.4 x"),
		"/octet":   typed("application/octet-stream", 200, "%PDF-1.4 x"),
		"/sniffed": typed("", 200, "%PDF-1.4 x"),
		"/text":    typed("text/plain", 200, "plain"),
		"/zip":     typed("application/zip", 200, "PK"),
		"/404":     typed("text/html", 404, articleBody),
	})
	for p, want := range map[string]bool{"/html": true, "/xhtml": true, "/frame": true, "/pdf": false, "/pdfhtml": false, "/octet": false, "/sniffed": false, "/text": false, "/zip": false, "/404": false} {
		var seen atomic.Bool
		e.r.Read(WithHTML(context.Background(), func() { seen.Store(true) }), e.url(p))
		if seen.Load() != want {
			t.Errorf("%s: html=%v, want %v", p, seen.Load(), want)
		}
	}
}

func TestFromRenderedExtractsLikeAFetchedPageAndJudgesItTheSameWay(t *testing.T) {
	e := newEnv(t, routes{})
	p, err := e.r.FromRendered("https://example.com/a", articleBody)
	if err != nil || !p.Rendered || p.FinalURL != "https://example.com/a" || p.Title != "The Parcel Report" || !strings.Contains(p.Text, "forty thousand parcels") || strings.Contains(p.Text, "Copyright footer") {
		t.Errorf("%+v %v", p, err)
	}
	_, err = e.r.FromRendered("https://example.com/a", `<html><body><div id="root"></div><script src="/a.js"></script></body></html>`)
	is(t, err, ErrNeedsScript)
	_, err = e.r.FromRendered("https://example.com/a", fedexPage)
	is(t, err, ErrBlockedBySite)
	_, err = e.r.FromRendered("https://example.com/a", challenges["cloudflare"])
	is(t, err, ErrBotCheck)
}

// Check makes the checks a read makes before it connects, and sends nothing but the robots.txt request.
func TestCheckAppliesTheAddressRulesTheCallsListsAndRobotsBeforeAnythingIsRendered(t *testing.T) {
	e := newEnv(t, routes{
		"/robots.txt": func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "User-agent: *\nDisallow: /private\n") },
		"/ok":         html(articleBody),
	})
	ctx := context.Background()
	if err := e.r.Check(ctx, e.url("/ok")); err != nil {
		t.Error(err)
	}
	is(t, e.r.Check(ctx, e.url("/private/x")), ErrRobots)
	if err := e.r.Check(ctx, "http://10.0.0.1/"); err == nil || !is2(err, ErrNotPublic) {
		t.Errorf("a private address: %v", err)
	}
	deny := WithAllow(ctx, func(h string) error { return ErrType })
	is(t, e.r.Check(deny, e.url("/ok")), ErrType)
	if e.hits.count("/ok") != 0 {
		t.Error("Check fetched the page")
	}
}
