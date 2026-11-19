package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-web/internal/pdfread"
	"idp-web/internal/pdfreader"
	"idp-web/internal/pdftest"
)

// inProcess reads a PDF in the test's own process (the process isolation has its tests in internal/pdfread and
// cmd/idp-web) and records what it was given.
type inProcess struct {
	mu       sync.Mutex
	calls    int
	deadline time.Time
}

func (p *inProcess) Read(ctx context.Context, doc []byte) (pdfread.Output, error) {
	p.mu.Lock()
	p.calls++
	p.deadline, _ = ctx.Deadline()
	p.mu.Unlock()
	res, err := pdfreader.Read(doc, pdfreader.DefaultLimits())
	if err != nil {
		return pdfread.Output{}, err
	}
	return pdfread.Output{Title: res.Title, Pages: res.Pages, PagesRead: res.PagesRead, Text: res.Text}, nil
}

func served(ct string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if ct != "" {
			w.Header()["Content-Type"] = []string{ct}
		} else {
			w.Header()["Content-Type"] = nil
		}
		w.Write(body)
	}
}

var wordsPDF = pdftest.Titled("Rate guide 2026", "The fuel surcharge for zone 3 is 12.5 percent of the base rate.")

func newPDFEnv(t *testing.T, rt routes) (*env, *inProcess) {
	e := newEnv(t, rt)
	p := &inProcess{}
	e.r.cfg.PDF = p
	return e, p
}

func TestAPDFIsReadAsTextWithAMarkerForEachPage(t *testing.T) {
	e, p := newPDFEnv(t, routes{"/files/guide.pdf": served("application/pdf", wordsPDF)})
	page, err := e.r.Read(context.Background(), e.url("/files/guide.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if page.Type != "application/pdf" || page.Engine != "pdf" || page.Title != "Rate guide 2026" || page.Pages != 1 || page.PagesRead != 1 ||
		!strings.HasPrefix(page.Text, "[page 1]") || !strings.Contains(page.Text, "fuel surcharge for zone 3") || page.Bytes != len(wordsPDF) {
		t.Errorf("%+v", page)
	}
	// the reader works inside the call's 15 seconds
	if d := time.Until(p.deadline); d <= 0 || d > Timeout {
		t.Errorf("the reader's deadline is %s from now", d)
	}
}

func TestADocumentWithoutATitleIsCalledByItsFileName(t *testing.T) {
	e, _ := newPDFEnv(t, routes{"/files/guide-2026.pdf": served("application/pdf", pdftest.Text("The fuel surcharge for zone 3 is 12.5 percent."))})
	page, err := e.r.Read(context.Background(), e.url("/files/guide-2026.pdf"))
	if err != nil || page.Title != "guide-2026.pdf" {
		t.Errorf("%q %v", page.Title, err)
	}
}

func TestABodyThatStartsWithTheHeaderIsAPDFWhateverTheServerCalledIt(t *testing.T) {
	// servers send documents as octet-stream, as text, as HTML, and with no type at all
	e, p := newPDFEnv(t, routes{
		"/octet": served("application/octet-stream", wordsPDF),
		"/text":  served("text/plain; charset=utf-8", wordsPDF),
		"/html":  served("text/html", wordsPDF),
		"/none":  served("", wordsPDF),
		"/xpdf":  served("application/x-pdf", wordsPDF),
	})
	for _, path := range []string{"/octet", "/text", "/html", "/none", "/xpdf"} {
		page, err := e.r.Read(context.Background(), e.url(path))
		if err != nil || page.Type != "application/pdf" || !strings.Contains(page.Text, "fuel surcharge") {
			t.Errorf("%s: %+v %v", path, page, err)
		}
	}
	if p.calls != 5 {
		t.Errorf("the reader was called %d times", p.calls)
	}
}

func TestABodyLabelledPDFThatIsAnHTMLPageIsReadAsTheHTMLPageItIs(t *testing.T) {
	article := `<html><head><title>Moved</title></head><body><article><h1>The page has moved</h1><p>` +
		strings.Repeat("The report you asked for has been moved to the archive and can be found there with its new name. ", 3) +
		`</p></article></body></html>`
	e, p := newPDFEnv(t, routes{"/report.pdf": served("application/pdf", []byte(article)), "/junk.pdf": served("application/pdf", []byte("\x00\x01 neither a PDF nor a page"))})
	page, err := e.r.Read(context.Background(), e.url("/report.pdf"))
	if err != nil || page.Type != "text/html" || !strings.Contains(page.Text, "has been moved to the archive") {
		t.Errorf("%+v %v", page, err)
	}
	_, err = e.r.Read(context.Background(), e.url("/junk.pdf"))
	is(t, err, ErrPDFMalformed)
	if p.calls != 0 {
		t.Errorf("the reader was started for something that is no PDF")
	}
}

func TestTypesThatCannotHoldAPDFAreRefusedUnreadWhateverTheBody(t *testing.T) {
	e, p := newPDFEnv(t, routes{"/zip": served("application/zip", wordsPDF), "/json": served("application/json", wordsPDF)})
	for _, path := range []string{"/zip", "/json"} {
		_, err := e.r.Read(context.Background(), e.url(path))
		is(t, err, ErrType)
	}
	if p.calls != 0 || e.hits.count("/zip") != 1 {
		t.Errorf("calls %d", p.calls)
	}
}

func TestWithoutAPDFReaderAPDFIsAnsweredThatReadingIsNotAvailable(t *testing.T) {
	e := newEnv(t, routes{"/a.pdf": served("application/pdf", wordsPDF)})
	_, err := e.r.Read(context.Background(), e.url("/a.pdf"))
	is(t, err, ErrPDFUnavailable)
}

func TestTheReadersRefusalsReachTheCallerAsTheirOwnErrors(t *testing.T) {
	e, _ := newPDFEnv(t, routes{
		"/scan.pdf": served("application/pdf", pdftest.Graphics(3)),
		"/locked":   served("application/pdf", pdftest.Encrypted()),
		"/broken":   served("application/pdf", []byte("%PDF-1.7\nthis is where it ends")),
		"/bomb.pdf": served("application/pdf", pdftest.Bomb(256)),
	})
	for path, want := range map[string]error{"/scan.pdf": ErrPDFNoText, "/locked": ErrPDFEncrypted, "/broken": ErrPDFMalformed, "/bomb.pdf": ErrPDFTooLarge} {
		_, err := e.r.Read(context.Background(), e.url(path))
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", path, err, want)
		}
	}
}

func TestAPDFOverTheDownloadLimitIsRefusedLikeAnyPage(t *testing.T) {
	big := append(append([]byte{}, wordsPDF...), bytes.Repeat([]byte{' '}, MaxBytes)...)
	e, p := newPDFEnv(t, routes{"/big.pdf": served("application/pdf", big)})
	_, err := e.r.Read(context.Background(), e.url("/big.pdf"))
	is(t, err, ErrTooLarge)
	if p.calls != 0 {
		t.Error("the reader saw a document over the limit")
	}
}

func TestACompressedPDFIsDecodedWithTheSameLimitAndThenRead(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(wordsPDF)
	w.Close()
	e, _ := newPDFEnv(t, routes{"/z.pdf": func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Encoding", "gzip")
		io.Copy(w, bytes.NewReader(gz.Bytes()))
	}})
	page, err := e.r.Read(context.Background(), e.url("/z.pdf"))
	if err != nil || !strings.Contains(page.Text, "fuel surcharge") {
		t.Errorf("%v %q", err, page.Text)
	}
}

func TestTheReaderIsNotStartedForAnErrorStatus(t *testing.T) {
	e, p := newPDFEnv(t, routes{"/gone.pdf": func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusNotFound)
		w.Write(wordsPDF)
	}})
	_, err := e.r.Read(context.Background(), e.url("/gone.pdf"))
	is(t, err, ErrStatus)
	if p.calls != 0 {
		t.Error("a 404 with a PDF body was read")
	}
}
