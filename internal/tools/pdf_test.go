package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"idp-web/internal/fetch"
	"idp-web/internal/pdfread"
)

// stubReader answers with a fixed page or error, for the tests of what the tools make of a PDF.
type stubReader struct {
	page fetch.Page
	err  error
}

func (s stubReader) Read(context.Context, string) (fetch.Page, error) { return s.page, s.err }

func longPDFText(pages int) string {
	var b strings.Builder
	for i := 1; i <= pages; i++ {
		fmt.Fprintf(&b, "\n[page %d]\n%s\n", i, strings.Repeat(fmt.Sprintf("Line of page %d with a sentence about the tariff. ", i), 20))
	}
	return strings.TrimSpace(b.String())
}

func TestAPDFIsHeadedAsOneAndPagedLikeAnyPageAndCitedByItsPart(t *testing.T) {
	text1 := longPDFText(30)
	w := newWorld(t, options{reader: stubReader{page: fetch.Page{Title: "Rate guide", Text: text1, Type: "application/pdf", Engine: "pdf", Pages: 30, PagesRead: 30}}})
	res := w.as("opener", "alice", "web_open_url", map[string]any{"url": w.url("allowed.test", "/guide.pdf")})
	if isErr(res) {
		t.Fatal(text(res))
	}
	body := text(res)
	head, part, _ := strings.Cut(body, "\n\n")
	if !strings.HasPrefix(head, "[1] Rate guide (PDF, 30 pages; bytes 0–12288 of ") || !strings.Contains(head, "the rest: web_open_url with offset 12288)") {
		t.Errorf("head %q", head)
	}
	if !strings.HasPrefix(part, "[page 1]") || len(part) != 12288 {
		t.Errorf("part of %d bytes starts %.20q", len(part), part)
	}
	c := citations(t, res)
	sum := sha256.Sum256([]byte(part))
	if len(c) != 1 || c[0]["sha256"] != hex.EncodeToString(sum[:]) || c[0]["url"] != w.url("allowed.test", "/guide.pdf") {
		t.Errorf("citation %v", c)
	}
	// the next part: the page marker that the model cites is in whichever part holds the text
	res = w.as("opener", "alice", "web_open_url", map[string]any{"url": w.url("allowed.test", "/guide.pdf"), "offset": 12288})
	if !strings.Contains(text(res), "PDF, 30 pages; bytes 12288–24576") {
		t.Errorf("%.200q", text(res))
	}
}

func TestACutPDFSaysItIsCutInTheHeadAndTheModelDoesNotHaveToOpenTheText(t *testing.T) {
	w := newWorld(t, options{reader: stubReader{page: fetch.Page{Title: "Long", Text: "[PDF, 560 pages; only the first 200 are read]\n\n[page 1]\nOne short page of text.", Type: "application/pdf", Engine: "pdf", Pages: 560, PagesRead: 200}}})
	res := w.as("opener", "alice", "web_open_url", map[string]any{"url": w.url("allowed.test", "/long.pdf")})
	if h := text(res); !strings.HasPrefix(h, "[1] Long (PDF, 560 pages, only the first 200 are read; bytes 0–") || !strings.Contains(h, "[PDF, 560 pages; only the first 200 are read]") {
		t.Errorf("%.300q", h)
	}
}

func TestAWebPageIsHeadedAsBefore(t *testing.T) {
	w := newWorld(t)
	res := w.as("opener", "alice", "web_open_url", map[string]any{"url": w.url("allowed.test", "/article")})
	if h, _, _ := strings.Cut(text(res), "\n\n"); !strings.HasPrefix(h, "[1] The Parcel Report (bytes 0–") {
		t.Errorf("%q", h)
	}
}

// Each refusal of the PDF reader is a sentence the model can act on and has its own reason in the audit and the counter.
func TestEachRefusalOfThePDFReaderHasItsOwnSentenceAndReason(t *testing.T) {
	for _, c := range []struct {
		err    error
		reason string
		in     string
	}{
		{pdfread.ErrNoTextLayer, "pdf_no_text_layer", "character recognition"},
		{pdfread.ErrEncrypted, "pdf_encrypted", "password"},
		{pdfread.ErrMalformed, "pdf_malformed", "damaged"},
		{pdfread.ErrTooLarge, "pdf_too_large", "more data than can be read"},
		{pdfread.ErrTooSlow, "pdf_too_slow", "within the time allowed"},
		{pdfread.ErrUnavailable, "pdf_reader_unavailable", "not available"},
		{pdfread.ErrBusy, "pdf_busy", "busy"},
	} {
		w := newWorld(t, options{reader: stubReader{err: fmt.Errorf("%w: detail for the log only", c.err)}})
		res := w.as("opener", "alice", "web_open_url", map[string]any{"url": w.url("allowed.test", "/a.pdf")})
		msg := text(res)
		if !isErr(res) || !strings.Contains(msg, c.in) || strings.Contains(msg, "detail for the log") || !strings.Contains(msg, "another source") && c.reason != "pdf_busy" {
			t.Errorf("%s: %q", c.reason, msg)
		}
		if w.met.calls["failed/web_open_url/"+c.reason] != 1 {
			t.Errorf("%s: counted %v", c.reason, w.met.calls)
		}
	}
	if errors.Is(fetch.ErrPDFNoText, fetch.ErrTooLarge) {
		t.Error("the PDF's own errors must not be taken for the page's")
	}
}
