package fetch

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
)

// Pages whose article is written by script (ADR-0001 D27). A reader that does not run script sees such a page as a
// frame: the site's navigation and footer, an empty box where the article goes, perhaps a "loading" line. Handing that
// to the model is worse than an error, in the way a challenge page is (D12): it would be read as the page, and the
// model would answer that the page says nothing. So a page is reported as needing script when it shows little text
// AND carries a positive sign that script would have produced the content. Little text alone is not enough: a short
// notice or a stub is a normal result, and a short page without any sign is given to the model as it is.
//
// Each sign is a rule of its own, named in the audit log's detail and in the measurement test, so that a page that
// is flagged can be traced to the one rule that flagged it, and so that a rule that turns out to be wrong can be
// removed without touching the others.
const (
	// maxShellText is the most visible text, in bytes of UTF-8, that a page may show and still be taken for a frame.
	// Bytes and not characters, on purpose: a Chinese character is three bytes and says about as much as three Latin
	// letters, so a count of characters would hold a Chinese page to a bar three times as high as an English one
	// (500 characters is a long paragraph in Chinese and a short one in English). 1000 bytes is about 170 words of
	// English or 330 Chinese characters. The frame of a site (menus, footer, "loading") is a few hundred bytes of
	// text, the shortest genuine pages seen were 590 to 920 bytes (a spokesman's answer, a courier's notice), and each
	// rule below needs its own sign, so the bound only has to separate "a frame" from "a page with a body".
	maxShellText = 1000
	// minScriptProse is how much text must sit in a script string, and be absent from the page, for rule
	// RuleScriptProse: about a short article's worth, so a title or a menu label in a data blob does not count.
	minScriptProse = 600
)

// The rules, by name.
const (
	RuleEmptyMount  = "empty_mount"
	RulePlaceholder = "placeholder_mount"
	RuleSkeleton    = "skeleton"
	RuleNoscript    = "noscript_notice"
	RuleScriptProse = "script_prose"
)

// needsScript names the rule that finds the page to be one that script writes, or "" when none does. rawHTML is the
// decoded source and text the visible text that extraction found (after the fallback of D5, so that a page the plain
// reader rescued is judged by what the model would be given, and a page with a body is never a frame).
func needsScript(rawHTML, text string) string {
	if len(strings.TrimSpace(text)) > maxShellText {
		return "" // a page with a body, whatever else it has: the cheap test first, so that no large page is parsed twice
	}
	doc, err := xhtml.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return ""
	}
	s := scan(doc)
	switch {
	case emptyMount(s):
		return RuleEmptyMount
	case placeholderMount(s):
		return RulePlaceholder
	case skeletonPage(s):
		return RuleSkeleton
	case noscriptNotice(s):
		return RuleNoscript
	case scriptProse(s, text):
		return RuleScriptProse
	}
	return ""
}

// signs is what one walk over the page collects for the rules.
type signs struct {
	mounts    []*xhtml.Node // elements that are the mount point of an application
	noscripts []string      // the text of each noscript element
	scripts   []string      // the text of each script that runs (not data that nothing reads, such as JSON-LD)
	external  bool          // some script is loaded from a file
	skeletons int           // elements that are loading placeholders by their class
}

func scan(doc *xhtml.Node) signs {
	var s signs
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			switch n.Data {
			case "script":
				if attr(n, "src") != "" {
					s.external = true
				}
				if t := strings.ToLower(attr(n, "type")); t != "application/ld+json" {
					s.scripts = append(s.scripts, textOf(n))
				}
				return
			case "noscript":
				s.noscripts = append(s.noscripts, textOf(n))
				return
			}
			if isMount(n) {
				s.mounts = append(s.mounts, n)
			}
			if strings.Contains(strings.ToLower(attr(n, "class")), "skeleton") {
				s.skeletons++
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return s
}

func attr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// textOf is the text below n (for a script or a noscript, which the parser keeps as text).
func textOf(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// mountIDs are the ids that front-end frameworks give the element they render the application into.
var mountIDs = map[string]bool{
	"root":      true, // React (create-react-app, Vite), Preact, Solid
	"app":       true, // Vue (Vue CLI, Vite)
	"__next":    true, // Next.js, pages router
	"__nuxt":    true, // Nuxt 2 and 3
	"___gatsby": true, // Gatsby
	"q-app":     true, // Quasar
	"svelte":    true, // SvelteKit's template
}

// isMount is true for the elements an application is rendered into: those ids, and the root element of an Angular or
// Ionic application, which is a custom element of its own.
func isMount(n *xhtml.Node) bool {
	if n.Data == "app-root" || n.Data == "ion-app" {
		return true
	}
	return mountIDs[strings.ToLower(attr(n, "id"))]
}

// emptyMount is rule RuleEmptyMount. For the single-page application whose server sends a frame and a script bundle:
// the element the application renders into is in the page and has no text in it. A server-rendered page has the
// same element filled, which is why the rule asks for it to be empty; the loading spinner that such frames show is
// markup without text and does not make the element non-empty.
func emptyMount(s signs) bool {
	for _, m := range s.mounts {
		if strings.TrimSpace(textOf(m)) == "" && !hasMedia(m) {
			return true
		}
	}
	return false
}

// loadingLine is all that the mount point of some frames holds until the application starts: "loading...",
// "正在加载", "please wait".
var loadingLine = regexp.MustCompile(`(?i)^[\p{P}\s]*(loading|please wait|just a moment|加载中|正在加载|载入中|请稍候|请稍等)[\p{P}\s]*$`)

// placeholderMount is rule RulePlaceholder, a stated widening of RuleEmptyMount: the mount point's only text is a
// loading line. For frames that print a "loading..." into the box the application replaces (seen on a logistics
// company's page: a root element with one line, "loading...", and a link to a site map outside it).
func placeholderMount(s signs) bool {
	for _, m := range s.mounts {
		if t := strings.TrimSpace(textOf(m)); t != "" && len(t) <= 60 && loadingLine.MatchString(t) {
			return true
		}
	}
	return false
}

// minSkeletons is how many placeholder elements make a page one that is waiting for its content.
const minSkeletons = 3

// skeletonPage is rule RuleSkeleton. For the server-rendered frame that sends the menus and, where the feed or the
// article goes, grey placeholder blocks that script replaces once the data arrives (class names such as
// "xitu-skeleton", "ant-skeleton", "MuiSkeleton-root", "skeleton-loader": a developer community's home page was seen
// with its navigation and tabs and nine such blocks in place of the feed). One stray element is not enough: it
// takes minSkeletons, and the usual text bound still applies.
func skeletonPage(s signs) bool { return s.skeletons >= minSkeletons }

// hasMedia is true when an element holds an image, a picture, a video or a table: content that is not text but is
// there, so the element is not empty.
func hasMedia(n *xhtml.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == xhtml.ElementNode {
			switch c.Data {
			case "img", "picture", "video", "table", "canvas", "iframe":
				return true
			}
		}
		if hasMedia(c) {
			return true
		}
	}
	return false
}

// jsNotice finds the sentence a frame shows to a reader without script: "You need to enable JavaScript to run this
// app" (create-react-app), "This site requires JavaScript", "JavaScript is disabled", "请开启 JavaScript". Written
// to be narrow: the word JavaScript next to a word that says it is needed or off.
var jsNotice = regexp.MustCompile(`(?i)(enable|requires?|required|need|needs|turn on|activate)\b[^.。]{0,60}javascript|javascript[^.。]{0,60}(is |are )?(required|disabled|turned off|not (enabled|supported|available)|must be)|(启用|开启|打开|需要|请)[^。]{0,12}javascript|javascript[^。]{0,12}(未启用|已禁用|被禁用|未开启|未打开)`)

// noscriptNotice is rule RuleNoscript. For the frame that tells a visitor without script, in a noscript element,
// that it needs script, and that does load script from a file (a notice on a page with no script at all is not the
// frame of an application). Create-react-app's template, the notice of most Vue and Angular starters, and sites
// that serve the same frame to every page.
func noscriptNotice(s signs) bool {
	if !s.external {
		return false
	}
	for _, t := range s.noscripts {
		if jsNotice.MatchString(t) {
			return true
		}
	}
	return false
}

// scriptProse is rule RuleScriptProse. For the page whose article is a string inside a script, which the script
// writes into an empty box: the article of a CMS in a variable (`var content = '<p>…</p>'`), or a data blob that holds
// the body as JSON, that the application renders. The text of such a string is prose (many words, or many Chinese
// characters) of at least minScriptProse bytes, and is NOT in the text the page shows. The second condition is what
// keeps a server-rendered application safe: its blob (Next.js's flight data, Nuxt's state, JSON-LD) holds the
// article that is also in the HTML, and such a page is a page.
func scriptProse(s signs, visible string) bool {
	vis := letters(visible)
	for _, sc := range s.scripts {
		for _, lit := range stringLiterals(sc) {
			p := proseOf(lit)
			if len(p) < minScriptProse {
				continue
			}
			if !isProse(p) {
				continue
			}
			if !seenIn(vis, letters(p)) {
				return true
			}
		}
	}
	return false
}

// stringLiterals are the quoted strings of a script, as written (escapes not yet decoded); a quote that is not closed
// on its line is dropped, so that an apostrophe in a comment cannot swallow the code after it.
func stringLiterals(src string) []string {
	var out []string
	for i := 0; i < len(src); i++ {
		q := src[i]
		if q != '\'' && q != '"' && q != '`' {
			continue
		}
		j := i + 1
		for j < len(src) && src[j] != q {
			if src[j] == '\\' {
				j++
			} else if src[j] == '\n' && q != '`' {
				break
			}
			j++
		}
		if j < len(src) && src[j] == q {
			if j-i > 200 {
				out = append(out, src[i+1:j])
			}
			i = j
		}
	}
	return out
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// proseOf decodes the escapes of a literal, drops its tags and entities, and collapses the white space.
func proseOf(lit string) string {
	s := unescapeJS(lit)
	s = tagRe.ReplaceAllString(s, " ")
	s = xhtmlUnescape(s)
	return strings.Join(strings.Fields(s), " ")
}

func xhtmlUnescape(s string) string { return xhtml.UnescapeString(s) }

func unescapeJS(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n', 't', 'r':
			b.WriteByte(' ')
		case 'u':
			if i+4 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 4
					continue
				}
			}
		case 'x':
			if i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+3], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 2
					continue
				}
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// isProse tells text that people wrote from code, identifiers, base64 and CSS: mostly Chinese (or other
// unspaced-script) characters, or words of ordinary length in a share that code does not reach.
func isProse(p string) bool {
	han, letters, words, wordChars := 0, 0, 0, 0
	for _, w := range strings.Fields(p) {
		n := 0
		for _, r := range w {
			if unicode.IsLetter(r) {
				n++
				if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
					han++
				}
			}
		}
		letters += n
		if n >= 1 && n <= 15 {
			words++
			wordChars += n
		}
	}
	total := utf8.RuneCountInString(p)
	if han*2 >= total/2 && han >= 150 {
		return true
	}
	return words >= 80 && letters*10 >= total*6 && wordChars*10 >= letters*8
}

// letters keeps the letters and digits of s, lower-cased: what is left to compare when the markup, the punctuation
// and the spacing differ between the script's copy of a text and the page's.
func letters(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// seenIn is true when a stretch of the prose is in the visible text. Three probes, from the middle of the prose
// where a title or a lead-in is not: an article that is on the page is found by any of them; a text that is only in the
// script by none.
func seenIn(vis, prose string) bool {
	rs := []rune(prose)
	const probe = 16
	if len(rs) < 3*probe {
		return strings.Contains(vis, prose)
	}
	for _, at := range []int{len(rs) / 4, len(rs) / 2, len(rs) * 3 / 4} {
		if strings.Contains(vis, string(rs[at:at+probe])) {
			return true
		}
	}
	return false
}
