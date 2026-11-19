package extract

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// plainSkip are the elements whose text is not the page's own: code, hidden templates and page furniture. This is the
// list of the reference reader (tools/web_eval/fetch.py, _Text), kept the same so that the fallback rule behaves as it
// was measured (platform ADR-0003 13.1).
var plainSkip = map[atom.Atom]bool{atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Svg: true, atom.Nav: true, atom.Footer: true, atom.Header: true, atom.Aside: true, atom.Form: true}

// plainBreak are the elements that start a new line in the plain text.
var plainBreak = map[atom.Atom]bool{atom.P: true, atom.Div: true, atom.Li: true, atom.Tr: true, atom.Br: true,
	atom.Section: true, atom.Article: true, atom.Table: true}

var (
	reBlanks = regexp.MustCompile(`[ \t]+`)
	reLines  = regexp.MustCompile(`\n\s*\n+`)
)

// plainText returns the title and the text of the page outside script, style, noscript, template, svg, nav, footer,
// header, aside and form, headings marked with # as in Markdown. It is the reader the fallback rule compares an
// engine with, and the result when the engine fails (ADR-0001 D5).
func plainText(doc *html.Node) (title, text string) {
	var sb strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			sb.WriteString(n.Data)
		case html.ElementNode:
			if plainSkip[n.DataAtom] {
				return
			}
			switch {
			case n.DataAtom == atom.Title:
				if title == "" {
					title = strings.Join(strings.Fields(textOf(n)), " ")
				}
				return
			case headingLevel(n) > 0:
				sb.WriteString("\n" + strings.Repeat("#", headingLevel(n)) + " ")
			case plainBreak[n.DataAtom]:
				sb.WriteString("\n")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	text = strings.ReplaceAll(sb.String(), " ", " ")
	text = reBlanks.ReplaceAllString(text, " ")
	text = strings.TrimSpace(reLines.ReplaceAllString(text, "\n\n"))
	return title, text
}

// headingLevel is 1 to 6 for h1 to h6 and 0 for any other node.
func headingLevel(n *html.Node) int {
	switch n.DataAtom {
	case atom.H1:
		return 1
	case atom.H2:
		return 2
	case atom.H3:
		return 3
	case atom.H4:
		return 4
	case atom.H5:
		return 5
	case atom.H6:
		return 6
	}
	return 0
}

// textOf is all the text below n.
func textOf(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}
