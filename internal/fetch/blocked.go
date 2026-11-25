package fetch

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// A page that says in words that the site will not show it to this reader is not the page either (ADR-0001 D33). The
// bot check of D12 knows the challenges (a script, a widget, a title); this is the soft block that answers HTTP 200
// with a sentence and nothing else: FedEx behind Akamai says "We're sorry, we can't process your request right now. It
// appears you don't have permission to view this webpage. Incident Number: ..." (found 2026-11-23 against www.fedex.com
// from the development network, where the read used to succeed and hand the model that sentence as the page).
//
// COPIED from idp-browser (internal/render/detect.go, rule S4 "refusal text", with refusalMarkers and akamaiRef and the
// bound maxChallengeText), so that the renderer and this reader agree on what a refusal page is; the cases of
// idp-browser's detect_test.go for S4 are in blocked_test.go. Of that file's five rules only S4 is taken: it needs
// nothing but the visible text, and a marker counts only on a page with at most maxChallengeText characters, so an
// article that quotes the sentence is a page. S1 to S3 are the challenge widgets, which D12 already answers as bot_check
// (their markers are the ones of botcheck.go; the further ones of idp-browser, HUMAN, DataDome, Imperva, Alibaba and
// GeeTest, were not copied: a plain read of such a page is the challenge script and an empty box, which D27 and D12
// already catch, and the renderer is where they are told apart). S5, a bare 403 or 429, is the HTTP status error that
// errorAnswer already gives.
var refusalMarkers = []string{
	"it appears you don't have permission to view this webpage", // FedEx, behind Akamai: HTTP 200 (found 2026-11-23)
	"you don't have permission to access",                       // Akamai: "You don't have permission to access ... on this server. Reference #"
	"we can't process your request right now",
	"your request has been blocked",
	"request unsuccessful. incapsula incident id",
	"access to this page has been denied",
	"automated access to", "automated queries",
	"访问过于频繁", "请求被拦截", "访问被拒绝", "您的访问行为异常",
}

var akamaiRef = regexp.MustCompile(`reference #\d+\.[0-9a-f]+\.\d+`)

// foldText lower-cases text and makes the quotes and the white space of a refusal page comparable with the markers: curly
// and modifier apostrophes and primes become ', curly double quotes become ", runs of white space one space. A site that
// writes "don’t" with U+2019 is the same refusal. The same function is in idp-browser (internal/render/detect.go); keep them identical.
func foldText(s string) string {
	s = strings.NewReplacer("\u2019", "'", "\u2018", "'", "\u02bc", "'", "\u2032", "'", "\u201c", "\"", "\u201d", "\"").Replace(strings.ToLower(s))
	return strings.Join(strings.Fields(s), " ")
}

// refusalSignal says which marker of the refusal text a page's visible text holds, "" if none (S4 of idp-browser).
func refusalSignal(text string) string {
	if utf8.RuneCountInString(strings.TrimSpace(text)) > maxChallengeText {
		return ""
	}
	lower := foldText(text)
	for _, m := range refusalMarkers {
		if strings.Contains(lower, m) {
			return m
		}
	}
	if akamaiRef.MatchString(lower) {
		return "Akamai reference"
	}
	return ""
}
