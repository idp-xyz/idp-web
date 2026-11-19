package fetch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"idp-web/internal/extract"
)

// Every page here is written for the test. Each shows the shape that a rule is for, or the shape that must not be
// taken for it; none is a copy of a page from a site.

// judge reads a page as the reader does (the extractor with its fallback, then the checks that follow it).
func judge(t *testing.T, page string) (rule string, err error, res extract.Result) {
	t.Helper()
	res, xerr := extract.Extract([]byte(page), "https://example.org/a", extract.Options{})
	if xerr != nil {
		t.Fatal(xerr)
	}
	rule, err = notThePage(page, res)
	return rule, err, res
}

func wantRule(t *testing.T, name, page, rule string) {
	t.Helper()
	got, err, res := judge(t, page)
	if got != rule || !errors.Is(err, ErrNeedsScript) {
		t.Errorf("%s: rule %q error %v, want %q (text %q)", name, got, err, rule, res.Markdown)
	}
}

func wantPage(t *testing.T, name, page string) extract.Result {
	t.Helper()
	got, err, res := judge(t, page)
	if got != "" || err != nil {
		t.Errorf("%s: flagged by %q (%v), it is a page: %q", name, got, err, res.Markdown)
	}
	return res
}

// The nav that every frame carries, so that the pages are not empty documents.
const frameNav = `<header><a href="/">Home</a> <a href="/news">News</a> <a href="/about">About us</a></header>`
const frameFoot = `<footer>Copyright 2026 Example Media. All rights reserved.</footer>`

// RuleEmptyMount: the single-page application's frame, in each framework's own mount point.
func TestAnEmptyMountPointWithLittleTextIsAPageThatNeedsScript(t *testing.T) {
	for name, mount := range map[string]string{
		"react":     `<div id="root"></div>`,
		"vue":       `<div id="app"></div>`,
		"next":      `<div id="__next"></div>`,
		"nuxt":      `<div id="__nuxt"> </div>`,
		"gatsby":    `<div id="___gatsby"></div>`,
		"angular":   `<app-root></app-root>`,
		"a spinner": `<div id="root"><div class="spinner"><svg viewBox="0 0 8 8"><circle r="3"/></svg></div></div>`,
	} {
		wantRule(t, name, `<!doctype html><html><head><title>Example</title><script type="module" src="/assets/index.js"></script></head><body>`+frameNav+mount+frameFoot+`</body></html>`, RuleEmptyMount)
	}
}

func TestAMountPointThatHoldsTheServerRenderedPageIsAPage(t *testing.T) {
	para := "<p>" + strings.Repeat("The depot sorted forty thousand parcels on Tuesday. ", 25) + "</p>"
	wantPage(t, "ssr", `<html><head><script src="/a.js"></script></head><body><div id="root"><article><h1>Parcels</h1>`+para+`</article></div></body></html>`)
	// a short but real page in the mount point: text there is what the rule looks for
	wantPage(t, "short ssr", `<html><head><script src="/a.js"></script></head><body><div id="app"><h1>Closed</h1><p>The office is closed on Monday.</p></div></body></html>`)
	// an element that happens to have the id and no application: a table or an image is content
	wantPage(t, "a chart", `<html><body><div id="app"><img src="/chart.png" width="400" height="300"></div><p>Chart.</p></body></html>`)
}

// RulePlaceholder: the mount point holds a loading line and nothing else.
func TestAMountPointThatOnlyHoldsALoadingLineIsAPageThatNeedsScript(t *testing.T) {
	for name, mount := range map[string]string{
		"loading":          `<div id="root"><div id="page-loading" style="display:flex">loading...</div></div>`,
		"Loading":          `<div id="app"><p>Loading…</p></div>`,
		"please wait":      `<div id="__next">Please wait</div>`,
		"chinese":          `<div id="app"><span>正在加载...</span></div>`,
		"chinese, shorter": `<div id="root">加载中</div>`,
	} {
		wantRule(t, name, `<!doctype html><html><head><script src="/a.js"></script></head><body>`+mount+`<a href="/site-map.html">site map</a></body></html>`, RulePlaceholder)
	}
}

func TestAMountPointWithRealTextNextToALoadingWordIsAPage(t *testing.T) {
	// a sentence that mentions loading is text; so is a loading line next to the content it announces is not the whole mount
	wantPage(t, "a sentence", `<html><head><script src="/a.js"></script></head><body><div id="root"><p>Loading dock opening hours: weekdays from six to two.</p></div></body></html>`)
	wantPage(t, "a line and a body", `<html><head><script src="/a.js"></script></head><body><div id="root"><p>Loading...</p><p>The depot is open from six to two on weekdays.</p></div></body></html>`)
	// and the bound still applies: a long page whose mount says loading is a page
	para := "<p>" + strings.Repeat("The depot sorted forty thousand parcels on Tuesday. ", 25) + "</p>"
	wantPage(t, "a long page", `<html><head><script src="/a.js"></script></head><body><article>`+para+`</article><div id="root">loading...</div></body></html>`)
}

// RuleSkeleton: placeholder blocks stand where the content will be.
func TestPlaceholderBlocksInPlaceOfTheContentMakeAPageThatNeedsScript(t *testing.T) {
	menu := `<ul><li><a href="/">Home</a></li><li><a href="/hot">Hot</a></li><li><a href="/new">New</a></li></ul>`
	for name, block := range map[string]string{
		"a community's feed": strings.Repeat(`<div class="xitu-skeleton xitu-skeleton-animated"><div class="xitu-skeleton-line"></div></div>`, 2),
		"ant design":         strings.Repeat(`<div class="ant-skeleton ant-skeleton-active"></div>`, 3),
		"material":           strings.Repeat(`<span class="MuiSkeleton-root MuiSkeleton-text"></span>`, 4),
	} {
		wantRule(t, name, `<html><head><script src="/a.js"></script></head><body><nav>`+menu+`</nav><div id="feed">`+block+`</div></body></html>`, RuleSkeleton)
	}
}

func TestOneStrayPlaceholderOrALongPageWithPlaceholdersIsAPage(t *testing.T) {
	wantPage(t, "one stray", `<html><body><h1>Prices</h1><p>Prices are updated daily.</p><div class="skeleton"></div></body></html>`)
	wantPage(t, "two", `<html><body><h1>Prices</h1><p>Prices are updated daily.</p><div class="skeleton"></div><div class="skeleton"></div></body></html>`)
	para := "<p>" + strings.Repeat("The depot sorted forty thousand parcels on Tuesday. ", 25) + "</p>"
	wantPage(t, "a long page", `<html><body><article>`+para+`</article>`+strings.Repeat(`<div class="skeleton-row"></div>`, 5)+`</body></html>`)
}

// RuleNoscript: the notice that frames show to a reader without script.
func TestANoscriptNoticeAskingForJavaScriptWithLittleTextIsAPageThatNeedsScript(t *testing.T) {
	for name, notice := range map[string]string{
		"create-react-app": `You need to enable JavaScript to run this app.`,
		"requires":         `This site requires JavaScript to work.`,
		"disabled":         `JavaScript is disabled in your browser.`,
		"turned off":       `Please turn on JavaScript and reload the page.`,
		"chinese":          `您的浏览器未启用 JavaScript，请开启后重试。`,
		"chinese 2":        `请启用JavaScript以查看本页内容。`,
	} {
		wantRule(t, name, `<!doctype html><html><head><title>Example</title><script defer src="/bundle.js"></script></head><body>`+frameNav+`<noscript>`+notice+`</noscript>`+frameFoot+`</body></html>`, RuleNoscript)
	}
}

func TestANoscriptNoticeIsNotEnoughWithoutScriptToLoadOrWithABodyOfText(t *testing.T) {
	// a static page that says what its comment widget needs: no script is loaded, nothing is produced by script
	wantPage(t, "no script at all", `<html><body><h1>Notice</h1><p>The library opens at nine.</p><noscript>Please enable JavaScript to see the comments.</noscript></body></html>`)
	// a real article whose widget needs script
	para := "<p>" + strings.Repeat("The depot sorted forty thousand parcels on Tuesday. ", 25) + "</p>"
	wantPage(t, "article", `<html><head><script src="/a.js"></script></head><body><article>`+para+`</article><noscript>Please enable JavaScript to see the comments.</noscript></body></html>`)
	// the word is not enough, the sentence has to say it is needed
	wantPage(t, "mention", `<html><head><script src="/a.js"></script></head><body><p>JavaScript tutorials.</p><noscript>Our JavaScript tutorials are listed below.</noscript></body></html>`)
}

// RuleScriptProse: the article is a string in a script and is written into an empty box.
func TestAnArticleThatOnlyExistsInAScriptStringIsAPageThatNeedsScript(t *testing.T) {
	zh := strings.Repeat("今年的重点工作是推进安全生产的治本攻坚，压实各方责任，守牢安全底线。", 12)
	en := strings.Repeat("The ministry said that the new rules would apply to every depot from the first of the month. ", 8)
	for name, page := range map[string]string{
		"a CMS variable, Chinese": `<html><head><title>News</title></head><body>` + frameNav + `<h1>Safety checks begin</h1><div id="text_area"></div>
<script>var contentdate = '<p style="text-indent: 2em;">` + zh + `</p>';
document.getElementById('text_area').innerHTML = contentdate;</script>` + frameFoot + `</body></html>`,
		"a CMS variable, English": `<html><head><title>News</title></head><body>` + frameNav + `<h1>New rules</h1><div class="body"></div>
<script>var article = "<p>` + en + `</p>"; document.querySelector('.body').innerHTML = article;</script></body></html>`,
		"a JSON blob with the body": `<html><head><title>News</title></head><body>` + frameNav + `<h1>New rules</h1><div class="story"></div>
<script type="application/json" id="data">{"story":{"headline":"New rules","body":"` + en + `"}}</script></body></html>`,
		"escaped, as JSON.parse reads it": `<html><body>` + frameNav + `<h1>x</h1><script>window.__DATA__ = JSON.parse("{\"body\":\"` + strings.ReplaceAll(en, " ", ` `) + `\"}");</script></body></html>`,
	} {
		wantRule(t, name, page, RuleScriptProse)
	}
}

func TestTextInAScriptThatThePageAlsoShowsIsAPageNotAFrame(t *testing.T) {
	en := strings.Repeat("The ministry said that the new rules would apply to every depot from the first of the month. ", 8)
	// server-rendered: the article is in the HTML and again in the state the application starts from; short, so only
	// the second condition (the text is not on the page) keeps it from being flagged
	short := strings.Repeat("The ministry said that the new rules would apply to every depot from the first of the month. ", 8)
	wantPage(t, "state copy", `<html><head><script src="/a.js"></script></head><body><article><p>`+short+`</p></article>
<script id="__NEXT_DATA__" type="application/json">{"props":{"body":"`+en+`"}}</script></body></html>`)
	// the same article as JSON-LD, which nothing runs
	wantPage(t, "json-ld", `<html><body><p>Parcel news.</p><script type="application/ld+json">{"@type":"NewsArticle","articleBody":"`+en+`"}</script></body></html>`)
	// code, base64 and styles are long strings and are not prose
	code := strings.Repeat("a1b2c3d4e5f6g7h8", 60)
	wantPage(t, "base64 and css", `<html><body><p>Parcel news.</p><script>var img = "data:image/png;base64,`+code+`"; var css = ".a{color:red;margin:0;padding:0}`+strings.Repeat(".b{margin:0;padding:0;}", 40)+`";</script></body></html>`)
	// a short string is a label
	wantPage(t, "labels", `<html><body><p>Parcel news.</p><script>var t = {"menu":"Home","more":"Read more of the news from the depot every day"};</script></body></html>`)
}

// Little text alone is a result, not a verdict.
func TestShortButNormalPagesAreNotFlagged(t *testing.T) {
	wantPage(t, "a short notice", `<html><head><title>Notice</title></head><body>`+frameNav+`<h1>Closed</h1><p>The office is closed on Monday for the holiday.</p>`+frameFoot+`</body></html>`)
	wantPage(t, "a stub", `<html><head><title>Stub</title></head><body><h1>Parcel depot</h1><p>This article is a stub. You can help by expanding it.</p></body></html>`)
	wantPage(t, "an error page served with 200", `<html><head><title>Not found</title><script src="/a.js"></script></head><body><h1>404</h1><p>The page you asked for does not exist. Try the <a href="/">home page</a>.</p></body></html>`)
	wantPage(t, "a redirect notice", `<html><head><title>Moved</title><meta http-equiv="refresh" content="0; url=/new"></head><body><p>This page has moved. If you are not sent on, follow <a href="/new">the new address</a>.</p></body></html>`)
	wantPage(t, "a script redirect notice", `<html><head><title>Moved</title></head><body><p>Redirecting, please wait.</p><script>setTimeout(function(){ location.href = "/new"; }, 100);</script></body></html>`)
	wantPage(t, "nothing at all", `<html><head><title>Empty</title></head><body></body></html>`)
	// a short page that loads a bundle and has a big script, but no sign that the script writes the article
	wantPage(t, "a short page with a large script", `<html><head><script src="/a.js"></script></head><body><p>Latest prices.</p><script>`+strings.Repeat("var a=function(x){return x*2};", 2000)+`</script></body></html>`)
}

// The text of a long page is in a table: the text is long, whatever the extractor makes of the rest.
func TestAPageWhoseLongTextIsInATableIsNotFlagged(t *testing.T) {
	var rows strings.Builder
	for i := 0; i < 40; i++ {
		rows.WriteString("<tr><td>Parcels sorted on the day of the week</td><td>forty thousand</td><td>no delays</td></tr>")
	}
	res := wantPage(t, "table", `<html><head><script src="/a.js"></script></head><body><div id="root"><table>`+rows.String()+`</table></div><noscript>Please enable JavaScript.</noscript></body></html>`)
	if len(res.Markdown) <= maxShellText {
		t.Errorf("the table page is not long (%d bytes): the test does not test what it says", len(res.Markdown))
	}
}

// The unit of the bound is bytes, so that it is as high for a Chinese page as for an English one in what is said.
func TestTheBoundOnVisibleTextIsInBytesSoAChinesePageIsNotHeldToAHigherBar(t *testing.T) {
	frame := func(body string) string {
		return `<html><head><script src="/a.js"></script></head><body><main><p>` + body + `</p></main><div id="root"></div></body></html>`
	}
	for _, c := range []struct {
		name string
		text string
		rule string
	}{
		{"English, 900 bytes", strings.Repeat("a", 900), RuleEmptyMount},
		{"English, 1100 bytes", strings.Repeat("a", 1100), ""},
		{"Chinese, 250 characters (750 bytes)", strings.Repeat("字", 250), RuleEmptyMount},
		{"Chinese, 400 characters (1200 bytes)", strings.Repeat("字", 400), ""},
	} {
		res, _ := extract.Extract([]byte(frame(c.text)), "", extract.Options{})
		if got := needsScript(frame(c.text), res.Markdown); got != c.rule {
			t.Errorf("%s: %q, want %q (text %d bytes)", c.name, got, c.rule, len(res.Markdown))
		}
	}
}

// A challenge page is short and full of script, and has the frame's signs too: it keeps its own reason (D12).
func TestAChallengePageKeepsItsOwnReasonWhateverElseItShows(t *testing.T) {
	for name, page := range map[string]string{
		"cloudflare with a frame":  `<html><head><title>Just a moment...</title><script src="/c.js"></script></head><body><div id="root"></div><noscript>Enable JavaScript and cookies to continue</noscript><script>window._cf_chl_opt={};</script></body></html>`,
		"aws waf with a mount":     `<html><body><div id="app"></div><noscript>You need to enable JavaScript to run this app.</noscript><script src="/w.js"></script><script>window.gokuProps = {}; window.awsWafCookieDomainList = [];</script></body></html>`,
		"a prompt with a noscript": `<html><head><script src="/a.js"></script></head><body><h1>Verify you are human</h1><noscript>JavaScript is required to continue.</noscript></body></html>`,
	} {
		rule, err, _ := judge(t, page)
		if !errors.Is(err, ErrBotCheck) || errors.Is(err, ErrNeedsScript) || rule != "" {
			t.Errorf("%s: rule %q, error %v", name, rule, err)
		}
	}
}

// The fallback of D5 and the detection (D27) are two answers to little text, and neither may hide the other.
func TestTheFallbackNeitherMasksTheDetectionNorIsOverruledByIt(t *testing.T) {
	// an index of links, which the engine takes for furniture and the plain reader reads whole (D5): the fallback
	// rescued it, so the text it gives is long, so it is a page, though it has an empty mount point and a notice
	var links strings.Builder
	for i := 0; i < 40; i++ {
		links.WriteString(`<li><a href="/n` + strings.Repeat("x", i) + `">Notice of the committee on the transport of parcels, in force from the first of the month.</a></li>`)
	}
	rescued := `<html><head><script src="/a.js"></script></head><body><article><p>Notices of the committee, newest first.</p></article><ul>` + links.String() + `</ul><div id="root"></div><noscript>Please enable JavaScript.</noscript></body></html>`
	if res := wantPage(t, "rescued", rescued); res.Engine != extract.Plain || len(res.Markdown) <= maxShellText {
		t.Errorf("the page was not rescued by the plain reader (%s, %d bytes): the test does not test what it says", res.Engine, len(res.Markdown))
	}
	// a frame whose only text is its menus: whichever of the two readers gives them, the model would be handed the
	// menus (D5 never returns less than the plain reader's text), and they are not a body, so the detection still sees a frame
	frame := `<html><head><script src="/a.js"></script></head><body><ul><li><a href="/">Home</a></li><li><a href="/news">News</a></li><li><a href="/about">About us</a></li></ul><div id="root"></div></body></html>`
	rule, err, res := judge(t, frame)
	if !strings.Contains(res.Markdown, "About us") || rule != RuleEmptyMount || !errors.Is(err, ErrNeedsScript) {
		t.Errorf("text %q, rule %q, error %v", res.Markdown, rule, err)
	}
}

// The whole path: a reader that meets such a page answers with the error, and a page that is only short is read.
func TestTheReaderAnswersAPageThatNeedsScriptWithItsErrorAndReadsAShortOne(t *testing.T) {
	e := newEnv(t, routes{
		"/spa":   html(`<html><head><script src="/a.js"></script></head><body>` + frameNav + `<div id="root"></div></body></html>`),
		"/short": html(`<html><head><title>Notice</title></head><body><p>The office is closed on Monday.</p></body></html>`),
	})
	_, err := e.r.Read(context.Background(), e.url("/spa"))
	is(t, err, ErrNeedsScript)
	if got := err.Error(); got != "the content of this page is produced by script and cannot be read now" {
		t.Errorf("%q", got)
	}
	page, err := e.r.Read(context.Background(), e.url("/short"))
	if err != nil || !strings.Contains(page.Text, "closed on Monday") {
		t.Errorf("%v %q", err, page.Text)
	}
}
