// Package extract reads the main text of a web page as Markdown (ADR-0001 D5, platform ADR-0003 D4). Three engines can
// produce it: go-trafilatura, go-readability and our own plain reader. Which of the first two the service uses is
// decided by measurement on annotated real pages (tools/web_eval), so all three stay selectable here.
package extract

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/markusmobius/go-trafilatura"
	"golang.org/x/net/html"

	readability "codeberg.org/readeck/go-readability/v2"
)

// Engines. Result.Engine is one of these: the one that produced the text, which after a fallback is Plain whatever
// was asked for.
const (
	Trafilatura = "trafilatura"
	Readability = "readability"
	Plain       = "plain"
)

// Focus is go-trafilatura's trade between text kept and furniture left out; the other engines ignore it.
const (
	Balanced  = "balanced"
	Precision = "precision"
	Recall    = "recall"
)

// FallbackShare is the share of the plain reader's text (by length) that an engine must reach: below it the engine is
// taken to have failed and the plain reader's text is used. The rule and the number come from the evaluation in
// platform ADR-0003 13.1, where it rescued the pages (gov.cn documents) on which trafilatura returned nothing or only a
// title.
const FallbackShare = 0.10

// Options select the engine. The zero value is go-trafilatura, balanced, with the fallback rule.
type Options struct {
	Engine string // Trafilatura (default), Readability or Plain
	Focus  string // Balanced (default), Precision or Recall; go-trafilatura only
	// NoFallback returns what the engine found even when it is empty or below FallbackShare; the evaluation uses it
	// to see an engine alone.
	NoFallback bool
	// NoInnerFallback switches off go-trafilatura's own use of readability and dom-distiller on a page it cannot read.
	// It is on by default because the Python reference (trafilatura.extract) does the same.
	NoInnerFallback bool
}

// Result is the title and the main text as Markdown, and the engine that produced the text. The text may be empty:
// that is a result (a page with no text a plain reader can see), not an error.
type Result struct {
	Title    string
	Markdown string
	Engine   string
}

// Extract reads html (already decoded to UTF-8) fetched from pageURL (which may be empty).
func Extract(htmlBytes []byte, pageURL string, opts Options) (Result, error) {
	var u *url.URL
	if pageURL != "" {
		var err error
		if u, err = url.Parse(pageURL); err != nil {
			return Result{}, fmt.Errorf("the page address is not valid: %w", err)
		}
	}
	switch opts.Engine {
	case "", Trafilatura, Readability, Plain:
	default:
		return Result{}, fmt.Errorf("the engine %q is not known", opts.Engine)
	}
	switch opts.Focus {
	case "", Balanced, Precision, Recall:
	default:
		return Result{}, fmt.Errorf("the focus %q is not known", opts.Focus)
	}
	doc, err := html.Parse(bytes.NewReader(htmlBytes))
	if err != nil {
		return Result{}, fmt.Errorf("the page could not be parsed: %w", err)
	}
	stripHidden(doc) // before any engine, so that none of them can read what a visitor does not see (ADR-0001 D5)
	// the plain text comes first: the engines may rewrite the tree they are given
	plainTitle, plain := plainText(doc)
	if opts.Engine == Plain {
		return Result{Title: plainTitle, Markdown: toValid(plain), Engine: Plain}, nil
	}

	var title, md string
	switch opts.Engine {
	case Readability:
		title, md, err = viaReadability(doc, u)
	default:
		title, md, err = viaTrafilatura(doc, u, opts)
	}
	if err != nil {
		// an engine that cannot read the page is the same case as one that finds nothing (with NoFallback: an empty text)
		md = ""
	}
	engine := opts.Engine
	if engine == "" {
		engine = Trafilatura
	}
	return choose(engine, title, md, plainTitle, plain, opts.NoFallback), nil
}

// choose applies the fallback rule to what an engine found and what the plain reader found.
func choose(engine, title, md, plainTitle, plain string, noFallback bool) Result {
	if title == "" {
		title = plainTitle
	}
	if !noFallback && needsFallback(md, plain) {
		return Result{Title: title, Markdown: toValid(plain), Engine: Plain}
	}
	return Result{Title: title, Markdown: toValid(md), Engine: engine}
}

func viaTrafilatura(doc *html.Node, u *url.URL, opts Options) (title, md string, err error) {
	o := trafilatura.Options{
		OriginalURL:    u,
		EnableFallback: !opts.NoInnerFallback,
		// the evaluation reads no dates, and the date search is the slowest part
		HtmlDateMode: trafilatura.Disabled,
		// comments of the page are not its main text; tables are, links and images are dropped by the converter anyway
		ExcludeComments: true,
		ExcludeTables:   false,
	}
	switch opts.Focus {
	case Precision:
		o.Focus = trafilatura.FavorPrecision
	case Recall:
		o.Focus = trafilatura.FavorRecall
	default:
		o.Focus = trafilatura.Balanced
	}
	res, err := trafilatura.ExtractDocument(doc, o)
	if err != nil {
		return "", "", err
	}
	if res == nil {
		return "", "", errors.New("no result")
	}
	return strings.TrimSpace(res.Metadata.Title), ToMarkdown(res.ContentNode), nil
}

func viaReadability(doc *html.Node, u *url.URL) (title, md string, err error) {
	p := readability.NewParser()
	art, err := p.ParseDocument(doc, u)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(art.Title()), ToMarkdown(art.Node), nil
}

// toValid replaces bytes that are not UTF-8, so that paging can cut between characters.
func toValid(s string) string { return strings.ToValidUTF8(s, "�") }

// needsFallback is the fallback rule (ADR-0001 D5): the engine found nothing, or less than FallbackShare of what the
// plain reader found, measured in characters as the reference measures it.
func needsFallback(engineText, plain string) bool {
	if engineText == "" {
		return true
	}
	return float64(utf8.RuneCountInString(engineText)) < FallbackShare*float64(utf8.RuneCountInString(plain))
}
