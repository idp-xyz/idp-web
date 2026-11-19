package fetch

import (
	"strings"
	"unicode/utf8"
)

// Bot-check pages are not the page (ADR-0001 D12). A site that takes the reader for a robot answers with a challenge,
// often with status 200, and the text of a challenge in the model's hands is worse than an error: it would be
// summarised as if it were the page. So a page is reported as a bot check when it carries a marker of a known challenge
// AND has little visible text. The second condition is what keeps real articles safe: a long article that mentions
// CAPTCHAs, or a normal page of a site that runs a bot-detection script, has markers and a lot of text, and is a page.
//
// The visible text is the one extract.Extract found (after its own fallback to the plain reader), so it is the text the
// model would have been given.
const (
	// A challenge page holds a sentence or two of text (the longest seen: 2035 bytes of script and a noscript line).
	// Above this many characters of visible text a page is a page whatever it contains.
	maxChallengeText = 1000
	// A page that only says "verify you are human" in so many words is a challenge only when it says little else.
	maxPromptText = 300
)

// htmlMarkers are found in the page's source, lower-cased. Each says where it was seen.
var htmlMarkers = []string{
	"_cf_chl_opt",             // Cloudflare managed challenge ("Just a moment..."): the page's configuration object; seen on theregister.com (phase 0 E4)
	"cf-chl-",                 // Cloudflare challenge form and widgets ("cf-chl-bypass", "cf-chl-widget-...")
	"cf-browser-verification", // Cloudflare's older "checking your browser" interstitial
	"awswafcookiedomainlist",  // AWS WAF JavaScript challenge: eur-lex.europa.eu answered HTTP 200 with a 2035-byte challenge page (phase 0 E4)
	"gokuprops",               // AWS WAF challenge, the same page: window.gokuProps
}

// titleMarkers are whole page titles, lower-cased and trimmed.
var titleMarkers = []string{
	"just a moment...",                 // Cloudflare managed challenge
	"just a moment…",                   // idem, with the ellipsis character
	"attention required! | cloudflare", // Cloudflare block page
	"please wait...",                   // ByteDance and Volcano Engine's security check (phase 0)
}

// textMarkers are found in the visible text, lower-cased, and count only on a page of at most maxPromptText characters.
var textMarkers = []string{
	"正在进行安全检测", // ByteDance and Volcano Engine's security check page (phase 0)
	"verify you are human",
	"verify you're human",
	"verifying you are human",
	"are you a robot",
	"i'm not a robot",
	"captcha",
	"unusual traffic from your computer",
	"人机验证",
	"请完成安全验证",
}

// isBotCheck reports whether a page is a challenge page. rawHTML is the decoded source, title and text what extraction
// found.
func isBotCheck(rawHTML, title, text string) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(text))
	if n > maxChallengeText {
		return false
	}
	lowerHTML := strings.ToLower(rawHTML)
	for _, m := range htmlMarkers {
		if strings.Contains(lowerHTML, m) {
			return true
		}
	}
	t := strings.ToLower(strings.TrimSpace(title))
	for _, m := range titleMarkers {
		if t == m {
			return true
		}
	}
	if n <= maxPromptText {
		lowerText := strings.ToLower(text)
		for _, m := range textMarkers {
			if strings.Contains(lowerText, m) {
				return true
			}
		}
	}
	return false
}
