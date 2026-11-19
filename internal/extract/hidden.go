package extract

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// hiddenStyle matches the two inline declarations that take an element out of sight. Text hidden by colour, size or
// position cannot be told apart from visible text without rendering, so it is left (ADR-0001 D5).
var hiddenStyle = regexp.MustCompile(`(?i)(display\s*:\s*none|visibility\s*:\s*hidden)`)

// textAttrs are attributes whose text a page shows to assistive technology or on hover but not in the flow of the
// page: an agent that reads them reads text no visitor sees, which is where a hidden instruction would go.
var textAttrs = map[string]bool{"alt": true, "title": true, "aria-label": true, "aria-description": true,
	"aria-roledescription": true, "placeholder": true, "label": true}

// stripHidden removes, in place, what a visitor does not see (ADR-0001 D5): comments, elements with the hidden
// attribute or aria-hidden="true" or an inline style that hides them, and the text attributes. It runs on the parsed
// document before any engine looks at it, so the three engines see the same page and the rule cannot differ between them.
func stripHidden(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		switch {
		case c.Type == html.CommentNode:
			n.RemoveChild(c)
		case c.Type == html.ElementNode && isHidden(c):
			n.RemoveChild(c)
		default:
			if c.Type == html.ElementNode {
				kept := c.Attr[:0]
				for _, a := range c.Attr {
					if !textAttrs[strings.ToLower(a.Key)] {
						kept = append(kept, a)
					}
				}
				c.Attr = kept
			}
			stripHidden(c)
		}
		c = next
	}
}

func isHidden(n *html.Node) bool {
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "hidden":
			return true
		case "aria-hidden":
			if strings.EqualFold(strings.TrimSpace(a.Val), "true") {
				return true
			}
		case "style":
			if hiddenStyle.MatchString(a.Val) {
				return true
			}
		}
	}
	return false
}
