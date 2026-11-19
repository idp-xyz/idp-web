// Package pdfreader turns a PDF with a text layer into text with a marker for each page (ADR-0001 D31).
//
// There are two halves. Read, here, is the parser's side: bytes in, text out, with the limits that can be enforced
// from inside (pages, inflated size, output size). child.go is the command idp-web-pdf that runs it in its own process
// with limits the operating system enforces; internal/pdfread is the service's side, which starts that process for
// each document. Only the command links this package, so the service process holds no PDF parser. The parser is
// github.com/go-pdfkit/extract on go-pdfkit/reader; the choice is ADR-0001 D31.
package pdfreader

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/go-pdfkit/extract"
	"github.com/go-pdfkit/reader"

	"idp-web/internal/pdfread"
)

// Limits of one document.
type Limits struct {
	MaxPages    int   // pages read; a longer document is read up to here and says so
	MaxStream   int64 // one Flate stream may inflate to this many bytes
	MaxInflated int64 // all the Flate streams of a document together
	MaxOutput   int   // bytes of text returned; pages are left out beyond it
}

// DefaultLimits are the limits of the service and of the command.
func DefaultLimits() Limits {
	return Limits{MaxPages: 200, MaxStream: 48 << 20, MaxInflated: 160 << 20, MaxOutput: pdfread.MaxOutputBytes}
}

// A page counts as having a text layer when it holds at least MinPageRunes characters that are not white space, and a
// document when at least one tenth of the pages read do (and never when none does). Twenty characters are a line of
// a sentence in any script; fewer are a page number, a stamp or a watermark. A scan with a few stray characters on it
// (an archive's stamp on the first page) has no text layer; a document that is mostly pictures with some real pages
// has one, and the pages without text say so in the output.
const (
	MinPageRunes = 20
	minTextShare = 10 // one page in this many
)

// Result is what Read returns.
type Result struct {
	Title     string
	Pages     int // in the document
	PagesRead int // the first PagesRead pages are in Text
	Text      string
}

// Read extracts the text of the PDF in data. A panic of the parser is an pdfread.ErrMalformed, not a crash.
func Read(data []byte, lim Limits) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = Result{}, fmt.Errorf("%w: the parser panicked: %v", pdfread.ErrMalformed, r)
		}
	}()
	return readFn(data, lim)
}

// readFn is read; a test replaces it to show that Read turns a panic of the parser into an error.
var readFn = read

// read is Read without the recovery, which the fuzz target calls so that a panic of the parser is a finding.
func read(data []byte, lim Limits) (Result, error) {
	if !hasHeader(data) {
		return Result{}, fmt.Errorf("%w: no %%PDF header", pdfread.ErrMalformed)
	}
	if !skipPreflight {
		if err := preflight(data, lim.MaxStream, lim.MaxInflated); err != nil {
			return Result{}, err
		}
	}
	doc, err := reader.Open(data)
	if err != nil {
		return Result{}, openError(err)
	}
	total := doc.PageCount()
	if total == 0 {
		return Result{}, fmt.Errorf("%w: no pages", pdfread.ErrMalformed)
	}
	n := min(total, lim.MaxPages)
	var (
		sb        strings.Builder
		textPages int
		read      int
	)
	for i := 1; i <= n; i++ {
		runs, err := extract.Runs(doc, i)
		page := ""
		undecoded := false
		if err == nil {
			page = strings.TrimSpace(extract.Assemble(runs))
			for _, r := range runs {
				if r.Unreadable {
					undecoded = true
				}
			}
		}
		page = clean(page)
		var b strings.Builder
		fmt.Fprintf(&b, "\n[page %d]\n", i)
		switch {
		case page != "":
			b.WriteString(page)
			b.WriteByte('\n')
		default:
			b.WriteString("(no text on this page)\n")
		}
		if undecoded {
			b.WriteString("(part of the text of this page could not be decoded)\n")
		}
		if sb.Len()+b.Len() > lim.MaxOutput {
			if read > 0 {
				break
			}
			// the first page alone is more than the whole bound: it is cut, and the document says so
			cut := strings.ToValidUTF8(b.String()[:lim.MaxOutput], "")
			b.Reset()
			b.WriteString(cut)
		}
		if countRunes(page) >= MinPageRunes {
			textPages++
		}
		sb.WriteString(b.String())
		read = i
	}
	if read == 0 || textPages == 0 || textPages*minTextShare < read {
		return Result{}, pdfread.ErrNoTextLayer
	}
	text := sb.String()
	if read < total {
		note := fmt.Sprintf("[PDF, %d pages; only the first %d are read]\n", total, read)
		if read < n {
			note = fmt.Sprintf("[PDF, %d pages; only the first %d are read, the text of the rest is too long]\n", total, read)
		}
		text = note + text
	}
	return Result{Title: title(doc), Pages: total, PagesRead: read, Text: strings.TrimSpace(text)}, nil
}

// skipPreflight is set by tests that show the process limits catching what the preflight would have stopped.
var skipPreflight bool

func hasHeader(b []byte) bool { return len(b) >= 8 && string(b[:5]) == "%PDF-" }

func openError(err error) error {
	if isEncrypted(err) {
		return fmt.Errorf("%w: %v", pdfread.ErrEncrypted, err)
	}
	return fmt.Errorf("%w: %v", pdfread.ErrMalformed, err)
}

func isEncrypted(err error) bool {
	return errorsIs(err, reader.ErrWrongPassword) || errorsIs(err, reader.ErrUnsupportedEncryption)
}

func countRunes(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// clean makes page text safe to hand on: valid UTF-8, no control characters other than the line break and the tab, no
// runs of blank lines.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

// title is the document's /Info /Title, when it has a usable one.
func title(doc *reader.Document) string {
	info, ok := doc.GetDict(doc.Trailer(), "Info")
	if !ok {
		return ""
	}
	obj, err := doc.Resolve(info["Title"])
	if err != nil {
		return ""
	}
	t, ok := reader.TextString(obj)
	if !ok {
		return ""
	}
	t = strings.Join(strings.Fields(clean(t)), " ")
	if len(t) > 200 {
		t = strings.ToValidUTF8(t[:200], "")
	}
	return t
}
