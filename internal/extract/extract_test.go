package extract

import (
	"strings"
	"testing"
	"unicode/utf8"
)

var engines = []string{Trafilatura, Readability, Plain}

// article wraps body in a page with furniture around it. The text is long enough for the engines to see an article.
func article(body string) []byte {
	para := "<p>The Rotterdam hub opened on 3 November 2026 and now handles forty thousand parcels every hour for the whole region.</p>"
	return []byte("<!doctype html><html><head><title>Hub opens</title></head><body>" +
		"<header>Site header words</header><nav><ul><li>Products</li><li>Careers</li></ul></nav>" +
		"<main><article><h1>Hub opens</h1>" + para + body + para + para + "</article></main>" +
		"<aside>Related stories sidebar</aside><footer>Copyright footer words</footer></body></html>")
}

func mustExtract(t *testing.T, in []byte, o Options) Result {
	t.Helper()
	r, err := Extract(in, "https://example.com/news/hub", o)
	if err != nil {
		t.Fatalf("Extract(%+v): %v", o, err)
	}
	return r
}

func TestMarkdownKeepsHeadingsListsTablesCodeQuotesAndBreaks(t *testing.T) {
	doc := mustParse(t, `<body><h2>Prices <em>2027</em></h2>
<p>Line one<br>line two and <a href="https://example.com/secret-address">a link</a><img src="x.png" alt="Alt words"> end.</p>
<ul><li>first<ul><li>nested a</li><li>nested b</li></ul></li><li>second</li></ul>
<ol><li>one</li><li>two</li></ol>
<table><thead><tr><th>Zone</th><th>Price</th></tr></thead><tbody><tr><td>Zone 1</td><td>4.20</td></tr><tr><td>a|b</td><td colspan=2>7</td></tr></tbody></table>
<table><tr><td>plain</td><td>cells</td></tr></table>
<pre><code class="language-go">if a &lt; b {
    f()
}</code></pre>
<blockquote><p>quoted</p><p>twice</p></blockquote></body>`)
	got := ToMarkdown(doc)
	want := []string{
		"## Prices *2027*",
		"Line one  \nline two and a link end.",
		"- first\n  - nested a\n  - nested b\n- second",
		"1. one\n2. two",
		"| Zone | Price |  |\n| --- | --- | --- |\n| Zone 1 | 4.20 |  |\n| a\\|b | 7 |  |",
		"|  |  |\n| --- | --- |\n| plain | cells |",
		"```go\nif a < b {\n    f()\n}\n```",
		"> quoted\n>\n> twice",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("the Markdown lacks %q; it is:\n%s", w, got)
		}
	}
	for _, no := range []string{"secret-address", "Alt words", "http"} {
		if strings.Contains(got, no) {
			t.Errorf("the Markdown holds %q, but links lose their address and images are dropped:\n%s", no, got)
		}
	}
}

func TestACodeFenceIsLongerThanTheBackticksInTheCode(t *testing.T) {
	got := ToMarkdown(mustParse(t, "<body><pre>a ``` b\nc</pre></body>"))
	if got != "````\na ``` b\nc\n````" {
		t.Errorf("got %q", got)
	}
}

func TestEveryEngineKeepsTheArticleAndLeavesTheFurnitureOut(t *testing.T) {
	// a positive control for the hidden-content tests below: the same page, nothing hidden
	for _, e := range engines {
		r := mustExtract(t, article(""), Options{Engine: e, NoFallback: true})
		if !strings.Contains(r.Markdown, "forty thousand parcels") {
			t.Errorf("%s: the article text is missing:\n%s", e, r.Markdown)
		}
		if r.Engine != e {
			t.Errorf("%s: Engine is %q", e, r.Engine)
		}
		if strings.Contains(r.Markdown, "Copyright footer") || strings.Contains(r.Markdown, "Related stories") {
			t.Errorf("%s: page furniture was kept:\n%s", e, r.Markdown)
		}
		if r.Title == "" {
			t.Errorf("%s: no title", e)
		}
	}
}

func TestHiddenContentNeverReachesTheModelInAnyEngine(t *testing.T) {
	hidden := `<!-- COMMENT-SECRET -->
<p hidden>HIDDEN-ATTR-SECRET</p>
<div aria-hidden="true">ARIA-SECRET</div>
<p style="color:red; DISPLAY : none !important">DISPLAY-SECRET</p>
<span style="visibility:hidden">VISIBILITY-SECRET</span>
<p>Visible words that stay. <img src="a.png" alt="ALT-SECRET" title="IMG-TITLE-SECRET"></p>
<p title="TITLE-SECRET" aria-label="LABEL-SECRET">A second visible paragraph that stays in the article.</p>`
	secrets := []string{"COMMENT-SECRET", "HIDDEN-ATTR-SECRET", "ARIA-SECRET", "DISPLAY-SECRET", "VISIBILITY-SECRET", "ALT-SECRET",
		"IMG-TITLE-SECRET", "TITLE-SECRET", "LABEL-SECRET"}
	for _, e := range engines {
		for _, noFallback := range []bool{true, false} {
			r := mustExtract(t, article(hidden), Options{Engine: e, NoFallback: noFallback})
			if !strings.Contains(r.Markdown, "forty thousand parcels") || !strings.Contains(r.Markdown, "A second visible paragraph") {
				t.Errorf("%s: the visible text is missing, so the check below proves nothing:\n%s", e, r.Markdown)
			}
			for _, s := range secrets {
				if strings.Contains(r.Markdown, s) || strings.Contains(r.Title, s) {
					t.Errorf("%s (no-fallback %v): hidden text %q reached the output:\n%s", e, noFallback, s, r.Markdown)
				}
			}
		}
	}
}

func TestHiddenTextInTheFallbackPathIsDroppedToo(t *testing.T) {
	in := []byte(`<html><head><title>T</title></head><body><div>Shown words.</div><div hidden>HIDDEN-SECRET</div><!-- C-SECRET --></body></html>`)
	r := mustExtract(t, in, Options{Engine: Plain})
	if r.Markdown != "Shown words." {
		t.Errorf("got %q", r.Markdown)
	}
}

func TestPlainReaderSkipsFurnitureAndMarksHeadings(t *testing.T) {
	in := []byte(`<html><head><title> My  page </title><style>.x{}</style></head><body><nav>Menu</nav><header>Top</header>
<h2>Heading two</h2><p>First para.</p><script>var s = "code";</script><form>Form words</form><svg>svg words</svg>
<template>template words</template><noscript>noscript words</noscript><p>Second&nbsp;para.</p><aside>Side</aside><footer>Foot</footer></body></html>`)
	r := mustExtract(t, in, Options{Engine: Plain})
	if r.Title != "My page" || r.Markdown != "## Heading two\nFirst para.\n\nSecond para." {
		t.Errorf("title %q, text %q", r.Title, r.Markdown)
	}
}

func TestTheFallbackRuleFiresWhenTheEngineFindsNothingOrTooLittle(t *testing.T) {
	plain := strings.Repeat("x", 1000)
	for _, c := range []struct {
		engineText string
		fallback   bool
	}{{"", true}, {strings.Repeat("y", 99), true}, {strings.Repeat("y", 100), false}, {strings.Repeat("y", 500), false}} {
		r := choose(Trafilatura, "", c.engineText, "t", plain, false)
		if (r.Engine == Plain) != c.fallback {
			t.Errorf("an engine text of %d characters against 1000: engine %q, want fallback %v", len(c.engineText), r.Engine, c.fallback)
		}
		if c.fallback && r.Markdown != plain || !c.fallback && r.Markdown != c.engineText {
			t.Errorf("an engine text of %d characters: the wrong text was chosen", len(c.engineText))
		}
	}
	// the length is counted in characters, not bytes: 100 Chinese characters are 300 bytes
	if r := choose(Readability, "", strings.Repeat("字", 100), "", strings.Repeat("x", 1000), false); r.Engine != Readability {
		t.Errorf("100 characters against 1000 must not fall back: %q", r.Engine)
	}
	if r := choose(Readability, "", "short", "", plain, true); r.Engine != Readability || r.Markdown != "short" {
		t.Errorf("with the fallback off the engine's text stays: %+v", r)
	}
}

func TestAPageWithNothingForTheEngineFallsBackToThePlainReaderAndSaysSo(t *testing.T) {
	in := []byte(`<html><head></head><body><script>var a = 1;</script><div hidden>HIDDEN-SECRET</div></body></html>`)
	for _, e := range []string{Trafilatura, Readability} {
		r := mustExtract(t, in, Options{Engine: e})
		if r.Engine != Plain || r.Markdown != "" {
			t.Errorf("%s: %+v", e, r)
		}
	}
}

func TestTheSameInputGivesTheSameOutputTwice(t *testing.T) {
	in := article(`<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table><ul><li>x<ul><li>y</li></ul></li></ul>`)
	for _, e := range engines {
		for _, f := range []string{Balanced, Precision, Recall} {
			a := mustExtract(t, in, Options{Engine: e, Focus: f})
			b := mustExtract(t, in, Options{Engine: e, Focus: f})
			if a != b {
				t.Errorf("%s/%s: two runs differ", e, f)
			}
		}
	}
}

func TestExtractRefusesUnknownOptionsAndBadAddresses(t *testing.T) {
	if _, err := Extract(nil, "", Options{Engine: "magic"}); err == nil {
		t.Error("an unknown engine was accepted")
	}
	if _, err := Extract(nil, "", Options{Focus: "magic"}); err == nil {
		t.Error("an unknown focus was accepted")
	}
	if _, err := Extract(nil, "http://[::1", Options{}); err == nil {
		t.Error("a broken address was accepted")
	}
	if r, err := Extract(nil, "", Options{}); err != nil || r.Markdown != "" {
		t.Errorf("empty input: %+v, %v", r, err)
	}
}

func TestPagesCoverTheWholeTextAndNeverSplitACharacter(t *testing.T) {
	// 3-byte and 4-byte characters at every alignment against the part size
	for _, unit := range []string{"字", "é", "😀", "a", "字é😀a"} {
		text := strings.Repeat(unit, 20000/len(unit)+7)
		for _, limit := range []int{PageBytes, 4, 5, 7, 1000, 12289, 0} {
			var got strings.Builder
			off, parts := 0, 0
			for off >= 0 {
				part, next, total := Page(text, off, limit)
				if total != len(text) {
					t.Fatalf("total %d, want %d", total, len(text))
				}
				if !utf8.ValidString(part) {
					t.Fatalf("unit %q limit %d: a part splits a character", unit, limit)
				}
				if len(part) > PageBytes || (limit > 0 && len(part) > max(limit, 4)) {
					t.Fatalf("unit %q limit %d: a part of %d bytes", unit, limit, len(part))
				}
				if next >= 0 && next != off+len(part) {
					t.Fatalf("next %d is not where the part ends (%d)", next, off+len(part))
				}
				got.WriteString(part)
				off = next
				if parts++; parts > len(text) {
					t.Fatal("paging does not advance")
				}
			}
			if got.String() != text {
				t.Fatalf("unit %q limit %d: the parts are not the whole text", unit, limit)
			}
		}
	}
}

func TestPageEdges(t *testing.T) {
	if p, n, total := Page("", 0, 10); p != "" || n != -1 || total != 0 {
		t.Errorf("empty: %q %d %d", p, n, total)
	}
	if p, n, _ := Page("abc", 99, 10); p != "" || n != -1 {
		t.Errorf("past the end: %q %d", p, n)
	}
	if p, n, _ := Page("abc", -5, 10); p != "abc" || n != -1 {
		t.Errorf("negative offset: %q %d", p, n)
	}
	// an offset inside a character moves to the next boundary
	if p, _, _ := Page("字b", 1, 10); p != "b" {
		t.Errorf("offset inside a character: %q", p)
	}
}
