package extract

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The Go engines return an HTML node (or plain text), not Markdown, so the conversion is ours (ADR-0001 D5). It keeps
// headings with their level, paragraphs, nested lists, tables as pipe tables, code blocks, quotes and line breaks.
// Links keep their text and lose their address, because the service does not follow links (platform ADR-0003 D2);
// images are dropped with their alt text, which a visitor does not read as text.

// dropped are the elements that carry no text of the page.
var dropped = map[atom.Atom]bool{atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Svg: true, atom.Img: true, atom.Picture: true, atom.Source: true, atom.Iframe: true, atom.Head: true,
	atom.Object: true, atom.Embed: true, atom.Canvas: true, atom.Video: true, atom.Audio: true, atom.Math: true}

// containers are block elements that only group other content.
var containers = map[atom.Atom]bool{atom.Div: true, atom.Section: true, atom.Article: true, atom.Main: true,
	atom.Body: true, atom.Html: true, atom.Figure: true, atom.Figcaption: true, atom.Details: true, atom.Summary: true,
	atom.Dl: true, atom.Dd: true, atom.Dt: true, atom.Address: true, atom.Fieldset: true, atom.Header: true,
	atom.Footer: true, atom.Aside: true, atom.Nav: true, atom.Form: true, atom.Li: true, atom.Caption: true}

// ToMarkdown renders the content below root as Markdown.
func ToMarkdown(root *html.Node) string {
	if root == nil {
		return ""
	}
	normalizeAtoms(root)
	return strings.TrimSpace(blocks(root))
}

// normalizeAtoms sets DataAtom from the tag name: nodes the engines build themselves (go-trafilatura creates its
// output elements by name) carry no atom, and the converter looks elements up by it.
func normalizeAtoms(n *html.Node) {
	if n.Type == html.ElementNode && n.DataAtom == 0 {
		n.DataAtom = atom.Lookup([]byte(strings.ToLower(n.Data)))
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		normalizeAtoms(c)
	}
}

// isBlock reports whether n ends the paragraph that came before it.
func isBlock(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.DataAtom {
	case atom.P, atom.Ul, atom.Ol, atom.Table, atom.Pre, atom.Blockquote, atom.Hr:
		return true
	}
	if n.DataAtom == atom.Code {
		return isCodeBlock(n)
	}
	return headingLevel(n) > 0 || containers[n.DataAtom]
}

// isCodeBlock is a multi-line code element sitting between blocks: go-trafilatura writes a pre as such a code element,
// and a browser would show it as a block too. Short code inside a sentence stays inline.
func isCodeBlock(n *html.Node) bool {
	par := n.Parent
	if par == nil || !strings.Contains(strings.TrimSpace(textOf(n)), "\n") {
		return false
	}
	return containers[par.DataAtom] || par.DataAtom == atom.Blockquote
}

// blocks renders the children of n as blocks separated by a blank line: a run of inline content is one paragraph.
func blocks(n *html.Node) string {
	var out []string
	var run []*html.Node
	flush := func() {
		if len(run) == 0 {
			return
		}
		if s := inlineAll(run); s != "" {
			out = append(out, s)
		}
		run = nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && dropped[c.DataAtom] || c.Type == html.CommentNode {
			continue
		}
		if !isBlock(c) {
			run = append(run, c)
			continue
		}
		flush()
		if s := block(c); s != "" {
			out = append(out, s)
		}
	}
	flush()
	return strings.Join(out, "\n\n")
}

func block(n *html.Node) string {
	switch n.DataAtom {
	case atom.P:
		return blocks(n)
	case atom.Hr:
		return "---"
	case atom.Ul, atom.Ol:
		return list(n)
	case atom.Table:
		return table(n)
	case atom.Pre, atom.Code:
		return fenced(n)
	case atom.Blockquote:
		inner := blocks(n)
		if inner == "" {
			return ""
		}
		lines := strings.Split(inner, "\n")
		for i, l := range lines {
			lines[i] = strings.TrimRight("> "+l, " ")
		}
		return strings.Join(lines, "\n")
	}
	if lv := headingLevel(n); lv > 0 {
		t := oneLine(inlineAll(children(n)))
		if t == "" {
			return ""
		}
		return strings.Repeat("#", lv) + " " + t
	}
	return blocks(n)
}

func children(n *html.Node) []*html.Node {
	var cs []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		cs = append(cs, c)
	}
	return cs
}

// list renders ul and ol, nesting by indenting the lines of an item under its marker.
func list(n *html.Node) string {
	var items []string
	i := 0
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		var body string
		switch {
		case c.Type == html.ElementNode && c.DataAtom == atom.Li:
			body = strings.ReplaceAll(blocks(c), "\n\n", "\n")
		case c.Type == html.ElementNode && (c.DataAtom == atom.Ul || c.DataAtom == atom.Ol):
			// a list directly inside a list (invalid but common) hangs on the previous item
			body = list(c)
			if len(items) > 0 {
				items[len(items)-1] += "\n" + indent(body, "  ")
				continue
			}
		default:
			continue
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		marker := "- "
		if n.DataAtom == atom.Ol {
			i++
			marker = strconv.Itoa(i) + ". "
		}
		pad := strings.Repeat(" ", len(marker))
		items = append(items, marker+strings.TrimPrefix(indent(body, pad), pad))
	}
	return strings.Join(items, "\n")
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// fenced renders pre as a fenced code block with its text as written; the fence is longer than any run of backticks
// in the code, so the code cannot close it.
func fenced(n *html.Node) string {
	var sb strings.Builder
	lang := ""
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			sb.WriteString(n.Data)
		case n.Type == html.ElementNode && n.DataAtom == atom.Br:
			sb.WriteString("\n")
		case n.Type == html.ElementNode && dropped[n.DataAtom]:
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Code && lang == "" {
			for _, a := range n.Attr {
				if a.Key == "class" {
					for _, cl := range strings.Fields(a.Val) {
						if l, ok := strings.CutPrefix(cl, "language-"); ok {
							lang = l
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	code := strings.Trim(strings.ReplaceAll(sb.String(), "\r\n", "\n"), "\n")
	if strings.TrimSpace(code) == "" {
		return ""
	}
	fence, run := "```", 0
	for _, r := range code {
		if r == '`' {
			run++
			for len(fence) <= run {
				fence += "`"
			}
		} else {
			run = 0
		}
	}
	return fence + lang + "\n" + code + "\n" + fence
}

// table renders a table as a pipe table. The first row is the header when the table has a thead or that row has th
// cells; otherwise the header row is empty, because a pipe table cannot do without one.
func table(n *html.Node) string {
	type row struct {
		cells  []string
		header bool
	}
	var rows []row
	inHead := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.DataAtom {
			case atom.Thead:
				inHead = true
				walk(c)
				inHead = false
			case atom.Tbody, atom.Tfoot:
				walk(c)
			case atom.Tr:
				r := row{header: inHead}
				for d := c.FirstChild; d != nil; d = d.NextSibling {
					if d.Type != html.ElementNode || (d.DataAtom != atom.Td && d.DataAtom != atom.Th) {
						continue
					}
					if d.DataAtom == atom.Th {
						r.header = true
					}
					cell := strings.ReplaceAll(oneLine(inlineAll(cellNodes(d))), "|", `\|`)
					r.cells = append(r.cells, cell)
					for k := colspan(d); k > 1; k-- {
						r.cells = append(r.cells, "") // keep the columns below aligned
					}
				}
				if len(r.cells) > 0 {
					rows = append(rows, r)
				}
			}
		}
	}
	walk(n)
	if len(rows) == 0 {
		return ""
	}
	width := 0
	for _, r := range rows {
		width = max(width, len(r.cells))
	}
	line := func(cells []string) string {
		for len(cells) < width {
			cells = append(cells, "")
		}
		return "| " + strings.Join(cells, " | ") + " |"
	}
	var out []string
	head, body := make([]string, width), rows
	if rows[0].header {
		head, body = rows[0].cells, rows[1:]
	}
	out = append(out, line(head), line(repeat("---", width)))
	for _, r := range body {
		out = append(out, line(r.cells))
	}
	return strings.Join(out, "\n")
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func colspan(n *html.Node) int {
	for _, a := range n.Attr {
		if a.Key == "colspan" {
			if k, err := strconv.Atoi(strings.TrimSpace(a.Val)); err == nil && k > 1 && k <= 50 {
				return k
			}
		}
	}
	return 1
}

// cellNodes are the content of a cell; a table or list inside it is flattened to its text, since a pipe table cell
// holds one line.
func cellNodes(n *html.Node) []*html.Node { return children(n) }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// inlineAll renders a run of inline nodes (and, in a cell or heading, whatever is inside them) as one text.
func inlineAll(ns []*html.Node) string {
	var sb strings.Builder
	for _, n := range ns {
		inline(&sb, n)
	}
	return tidy(sb.String())
}

// inline writes n as running text. A block element met here (inside a cell, a heading, or a paragraph that the parser
// left open) contributes its text between spaces.
func inline(sb *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		sb.WriteString(collapse(n.Data))
		return
	case html.ElementNode:
	default:
		return
	}
	if dropped[n.DataAtom] {
		return
	}
	switch n.DataAtom {
	case atom.Br:
		sb.WriteString("\x00") // a hard break; spaces around it are trimmed by tidy
		return
	case atom.Code, atom.Kbd, atom.Samp, atom.Tt:
		t := strings.TrimSpace(textOf(n))
		if t == "" {
			return
		}
		fence := "`"
		for strings.Contains(t, fence) {
			fence += "`"
		}
		sb.WriteString(fence + t + fence)
		return
	case atom.Strong, atom.B, atom.Em, atom.I:
		var in strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			inline(&in, c)
		}
		s := in.String()
		t := strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
		if t == "" {
			sb.WriteString(s)
			return
		}
		mark := "*"
		if n.DataAtom == atom.Strong || n.DataAtom == atom.B {
			mark = "**"
		}
		lead := s[:len(s)-len(strings.TrimLeft(s, " "))]
		trail := s[len(strings.TrimRight(s, " ")):]
		sb.WriteString(lead + mark + strings.TrimSpace(s) + mark + trail)
		return
	}
	pad := isBlock(n) || n.DataAtom == atom.Li
	if pad {
		sb.WriteString(" ")
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		inline(sb, c)
	}
	if pad {
		sb.WriteString(" ")
	}
}

// collapse folds runs of white space (not breaks written as \x00) to one space, as HTML renders them.
func collapse(s string) string {
	s = strings.ReplaceAll(s, " ", " ")
	var sb strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' {
			if !space {
				sb.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		sb.WriteRune(r)
	}
	return sb.String()
}

// tidy trims the text, folds the spaces that two neighbouring nodes left, and turns the break marks into Markdown
// hard breaks.
func tidy(s string) string {
	lines := strings.Split(s, "\x00")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	var out []string
	for i, l := range lines {
		if l == "" && (i == 0 || i == len(lines)-1) {
			continue
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "  \n"))
}
