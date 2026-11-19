// Package pdfread is the service's side of reading PDFs (ADR-0001 D31): the errors, and the Runner that starts the
// command idp-web-pdf once for each document. The parser is in internal/pdfreader, which only that command links: the
// service process holds no PDF parser.
package pdfread

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The errors of reading a PDF are sentences a model can act on (ADR-0001 D31, D15): what happened to the document and
// what to do, never how the reader works inside; the detail goes to the log. internal/fetch re-exports them, and
// internal/tools gives each its audit reason.
var (
	ErrNoTextLayer = errors.New("the PDF has no text layer (it is a scan or pictures only) and this service does not do character recognition")
	ErrEncrypted   = errors.New("the PDF is protected with a password")
	ErrMalformed   = errors.New("the PDF is damaged, or not a valid PDF")
	ErrTooLarge    = errors.New("the PDF holds more data than can be read")
	ErrTooSlow     = errors.New("the PDF could not be read within the time allowed")
	ErrUnavailable = errors.New("reading PDFs is not available in this service")
	ErrBusy        = errors.New("the PDF reader is busy with other documents")
)

// The reason words of the command protocol (tools/web_eval/e4pdf.py): the first line of standard error of a reader
// that fails starts with one of them. The command (internal/pdfreader) says them and the Runner here turns them back
// into the errors above. The scorer knows one more, too_many_pages, which this reader never says: it reads the first
// pages instead.
const (
	ReasonNoText      = "no_text_layer"
	ReasonEncrypted   = "encrypted"
	ReasonMalformed   = "malformed"
	ReasonTooSlow     = "too_slow"
	ReasonTooLarge    = "too_large"
	ReasonUnavailable = "unavailable"
)

var reasonErrors = map[string]error{
	ReasonNoText:      ErrNoTextLayer,
	ReasonEncrypted:   ErrEncrypted,
	ReasonMalformed:   ErrMalformed,
	ReasonTooSlow:     ErrTooSlow,
	ReasonTooLarge:    ErrTooLarge,
	ReasonUnavailable: ErrUnavailable,
}

// ReasonOf is the reason word for err; an error that is none of the above is malformed.
func ReasonOf(err error) string {
	for word, e := range reasonErrors {
		if errors.Is(err, e) {
			return word
		}
	}
	return ReasonMalformed
}

// The settings the Runner passes to the command and the command takes as its defaults.
const (
	DefaultDeadline  = 4 * time.Second // of the command alone: inside the 5 seconds the evaluation allows a document
	DefaultMemoryMiB = 192             // below the 256 MiB the evaluation allows; ADR-0001 D31 says what it was measured against
	MaxOutputBytes   = 4 << 20         // of text; a document that has more is cut at a page and says so
	MaxInput         = 8 << 20         // of the document; the service refuses a body over 5 MiB before it gets here
)

// ParseInfo reads the line of standard error of a reader that succeeded: "pdf pages=<n> read=<n> title=<quoted>".
func parseInfo(stderr string) (pages, read int, title string, ok bool) {
	for _, line := range strings.Split(stderr, "\n") {
		rest, found := strings.CutPrefix(line, "pdf pages=")
		if !found {
			continue
		}
		var p, r int
		if _, err := fmt.Sscanf(rest, "%d read=%d title=", &p, &r); err != nil {
			continue
		}
		_, q, _ := strings.Cut(rest, "title=")
		t, err := strconv.Unquote(q)
		if err != nil {
			return 0, 0, "", false
		}
		return p, r, t, true
	}
	return 0, 0, "", false
}
