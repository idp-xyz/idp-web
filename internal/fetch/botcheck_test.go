package fetch

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The pages below are written to carry the markers a site's challenge carries, not copied from a site.
var challenges = map[string]string{
	"cloudflare": `<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title><meta http-equiv="refresh" content="360"></head>
<body><div class="main-wrapper"><h1>example.org</h1><h2 id="challenge-error-text">Enable JavaScript and cookies to continue</h2></div>
<script>window._cf_chl_opt={cvId:'3',cZone:'example.org'};</script></body></html>`,
	"cloudflare-opt-only":   `<html><head><title>example.org</title></head><body><script>window._cf_chl_opt = {};</script></body></html>`,
	"cloudflare-title-only": `<html><head><title>Just a moment...</title></head><body><p>Hold on.</p></body></html>`,
	"cloudflare-by-form":    `<html><head><title>Checking</title></head><body><form id="challenge-form"><input type="hidden" name="cf-chl-bypass" value="1"></form></body></html>`,
	"cloudflare-title":      `<html><head><title>Just a moment…</title></head><body><p>Checking your browser.</p></body></html>`,
	"cloudflare-block":      `<html><head><title>Attention Required! | Cloudflare</title></head><body><h1>Sorry, you have been blocked</h1></body></html>`,
	"aws-waf": `<html><head><title></title></head><body><div id="challenge-container"></div><script>window.awsWafCookieDomainList = [];
window.gokuProps = {"key":"AQIDAHj","iv":"A6x","context":"abc"};</script><noscript><h1>JavaScript is disabled</h1>In order to continue, we need to verify that you're not a robot.</noscript></body></html>`,
	"aws-waf-cookie-domain-only": `<html><body><script>window.awsWafCookieDomainList = ["example.org"];</script></body></html>`,
	"cloudflare-old":             `<html><body><div id="cf-browser-verification">Checking your browser before accessing the site.</div></body></html>`,
	"title-attention":            `<html><head><title>  ATTENTION REQUIRED! | CLOUDFLARE </title></head><body><p>Blocked.</p></body></html>`,
	"text-verify-youre-human":    `<html><body><p>Please verify you're human to continue.</p></body></html>`,
	"text-verifying":             `<html><body><p>Verifying you are human. This may take a few seconds.</p></body></html>`,
	"text-not-a-robot":           `<html><body><p>Tick the box: I'm not a robot.</p></body></html>`,
	"text-unusual-traffic":       `<html><body><p>Our systems have detected unusual traffic from your computer network.</p></body></html>`,
	"text-secure-check-cn":       `<html><body><p>请完成安全验证以继续。</p></body></html>`,
	"aws-waf-gokuprops-only":     `<html><body><script>window.gokuProps = {};</script></body></html>`,
	"bytedance-title":            `<html><head><title>Please wait...</title></head><body><div>Loading</div></body></html>`,
	"bytedance-text":             `<html><head><title>x</title></head><body><div>正在进行安全检测，请稍候...</div></body></html>`,
	"captcha-prompt":             `<html><head><title>Access</title></head><body><h1>Verify you are human</h1><p>Please complete the security check to continue.</p></body></html>`,
	"captcha-word":               `<html><head><title>Security</title></head><body><p>Please solve the CAPTCHA below to access this page.</p></body></html>`,
	"robot-question":             `<html><head><title>Hold on</title></head><body><p>Are you a robot? Press and hold the button.</p></body></html>`,
	"chinese-verification":       `<html><head><title>验证</title></head><body><p>请完成人机验证后继续访问。</p></body></html>`,
}

func TestEachBotCheckMarkerMakesAShortPageABotCheck(t *testing.T) {
	routesMap := routes{}
	for name, body := range challenges {
		routesMap["/"+name] = html(body)
	}
	e := newEnv(t, routesMap)
	for name := range challenges {
		_, err := e.r.Read(context.Background(), e.url("/"+name))
		if !is2(err, ErrBotCheck) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := ErrBotCheck.Error(); err != "the site answered with a bot check instead of the page" {
		t.Errorf("message %q", err)
	}
}

func is2(err, want error) bool { return errors.Is(err, want) }

// A long article that mentions CAPTCHAs, a bot-detection script or a Cloudflare page in its text is a page: the markers
// count only on a page with little visible text.
func TestLongRealArticlesThatMentionBotChecksAreNotBotChecks(t *testing.T) {
	para := "The newsroom spent a year studying how websites tell people from programs. Many sites show a CAPTCHA, ask the visitor to verify you are human, " +
		"or print Just a moment... while a script runs, and some answer with Are you a robot? in large letters. The researchers found that the pages worked for " +
		"most readers, and that the checks mattered mostly to the people who run scrapers. "
	article := func(extraHead string) string {
		return `<!doctype html><html><head><title>How websites spot robots</title>` + extraHead + `</head><body><article><h1>How websites spot robots</h1>` +
			strings.Repeat("<p>"+para+"</p>", 12) + `</article></body></html>`
	}
	e := newEnv(t, routes{
		"/long":    html(article("")),
		"/withjs":  html(article(`<script>window._cf_chl_opt={}; var cf-chl-widget = 1; window.gokuProps = {}; window.awsWafCookieDomainList=[];</script>`)),
		"/cftitle": html(strings.Replace(article(""), "How websites spot robots</title>", "Just a moment...</title>", 1)),
		// a site behind Cloudflare that injects its detection script into every page
		"/cdn": html(article(`<script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script>`)),
	})
	for _, p := range []string{"/long", "/withjs", "/cftitle", "/cdn"} {
		page, err := e.r.Read(context.Background(), e.url(p))
		if err != nil || !strings.Contains(page.Text, "newsroom") {
			t.Errorf("%s: %v", p, err)
		}
	}
	// and a short page that is simply short, with none of the markers
	e2 := newEnv(t, routes{"/short": html(`<html><head><title>Status</title></head><body><p>All systems are operating normally today.</p></body></html>`)})
	if _, err := e2.r.Read(context.Background(), e2.url("/short")); err != nil {
		t.Errorf("a short page without markers: %v", err)
	}
}

func TestTheBoundOnVisibleTextIsWhatKeepsAMarkerFromBeingAVerdict(t *testing.T) {
	marker := `<script>window.gokuProps = {};</script>`
	short := "<html><body>" + marker + "<p>" + strings.Repeat("word ", 20) + "</p></body></html>"
	long := "<html><body>" + marker + "<article>" + strings.Repeat("<p>"+strings.Repeat("word ", 40)+"</p>", 12) + "</article></body></html>"
	if !isBotCheck(short, "", strings.Repeat("word ", 20)) {
		t.Error("a short page with a marker was not a bot check")
	}
	if isBotCheck(long, "", strings.Repeat("word ", 480)) {
		t.Error("a long page with a marker was a bot check")
	}
	if !isBotCheck("<html></html>", "", "Please verify you are human") || isBotCheck("<html></html>", "", "Please verify you are human "+strings.Repeat("word ", 100)) { // 500 characters of other text: over the prompt bound, under the other
		t.Error("the prompt bound does not hold")
	}
}

func TestAChallengeAnsweredWithAnErrorStatusOrAHeaderIsABotCheckNotAStatus(t *testing.T) {
	cf := challenges["cloudflare"]
	status := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(code)
			w.Write([]byte(body))
		}
	}
	e := newEnv(t, routes{
		"/403":      status(403, cf),
		"/503":      status(503, cf),
		"/429":      status(429, challenges["aws-waf"]),
		"/202":      status(202, challenges["aws-waf"]),
		"/plain403": status(403, "<html><body><p>Forbidden</p></body></html>"),
		"/header": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(403)
			w.Write([]byte("<html><body><p>anything</p></body></html>"))
		},
	})
	for _, p := range []string{"/403", "/503", "/429", "/202", "/header"} {
		if _, err := e.r.Read(context.Background(), e.url(p)); !is2(err, ErrBotCheck) {
			t.Errorf("%s: %v", p, err)
		}
	}
	_, err := e.r.Read(context.Background(), e.url("/plain403"))
	is(t, err, ErrStatus)
}

func TestARedirectToAnUnblockHostIsABotCheck(t *testing.T) {
	e := newEnv(t, routes{"/doc": redirect("https://unblock.gazette.example/?ref=doc")})
	_, err := e.r.Read(context.Background(), e.url("/doc"))
	is(t, err, ErrBotCheck)
	// another host that merely contains the word is followed (and refused here only because it is not a test origin)
	e2 := newEnv(t, routes{"/doc": redirect("http://notunblock.example/")})
	e2.res.add("notunblock.example", "10.1.1.1")
	if _, err := e2.r.Read(context.Background(), e2.url("/doc")); is2(err, ErrBotCheck) {
		t.Errorf("%v", err)
	}
}
