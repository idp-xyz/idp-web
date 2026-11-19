package pdfreader

import (
	"errors"
	"testing"
	"unicode/utf8"

	"idp-web/internal/pdfread"
	"idp-web/internal/pdftest"
)

// FuzzRead feeds the parsing entry point arbitrary bytes, from a corpus of the documents the tests build. The
// function under test is read, not Read: Read turns a panic of the parser into an error, and a panic is exactly what
// the fuzzing is for. What may come out is a text or one of the errors; never a panic, never invalid text, never more
// pages than the document has. Crashers go to testdata/fuzz and run with the ordinary tests.
//
//	go test ./internal/pdfread -run '^$' -fuzz FuzzRead -fuzztime 10m
func FuzzRead(f *testing.F) {
	for _, seed := range seedCorpus() {
		f.Add(seed)
	}
	lim := DefaultLimits()
	lim.MaxStream, lim.MaxInflated, lim.MaxOutput = 2<<20, 8<<20, 1<<20
	f.Fuzz(func(t *testing.T, data []byte) {
		if !hasHeader(data) { // a mutation that breaks the header only tests the header check; keep the parser busy
			data = append([]byte("%PDF-1.7\n"), data...)
		}
		res, err := read(data, lim)
		if err != nil {
			for _, known := range []error{pdfread.ErrNoTextLayer, pdfread.ErrEncrypted, pdfread.ErrMalformed, pdfread.ErrTooLarge} {
				if errors.Is(err, known) {
					return
				}
			}
			t.Fatalf("an error that is none of the reader's own: %v", err)
		}
		if !utf8.ValidString(res.Text) || res.PagesRead < 1 || res.PagesRead > res.Pages || res.PagesRead > lim.MaxPages || res.Text == "" {
			t.Fatalf("a text that does not hold together: %d of %d pages, %d bytes", res.PagesRead, res.Pages, len(res.Text))
		}
	})
}

func seedCorpus() [][]byte {
	return [][]byte{
		pdftest.Text("A first page of plain text, with a line.", "A second page."),
		pdftest.TextFlate("A page with a compressed content stream, long enough to count."),
		pdftest.CID("本通知自发布之日起施行，请各单位遵照执行。"),
		pdftest.Cells([][]string{{"Zone", "Weight kg", "Price EUR"}, {"Zone 3", "0.5", "21.40"}, {"Zone 4", "1.0", "29.90"}}),
		pdftest.Titled("A title", "A page with enough words on it to count as text."),
		pdftest.Graphics(2),
		pdftest.Stamped(3, "ARCHIVES 1947"),
		pdftest.Encrypted(),
		pdftest.Many(4, "Paragraph of the document"),
		pdftest.Bomb(8),
		pdftest.PageTreeDeep(50),
		pdftest.PageTreeCycle(),
		pdftest.HTML(),
		[]byte("%PDF-1.7\n"),
		nil,
	}
}

// The seeds themselves are read with the ordinary tests, so that a seed that stops being valid is noticed.
func TestTheSeedsOfTheFuzzTargetHoldTogether(t *testing.T) {
	text := 0
	for i, seed := range seedCorpus() {
		res, err := Read(seed, DefaultLimits())
		if err == nil {
			text++
			_ = res
		} else if !errors.Is(err, pdfread.ErrNoTextLayer) && !errors.Is(err, pdfread.ErrEncrypted) && !errors.Is(err, pdfread.ErrMalformed) && !errors.Is(err, pdfread.ErrTooLarge) {
			t.Errorf("seed %d: %v", i, err)
		} else {
			t.Logf("seed %d: %v", i, err)
		}
	}
	if text < 6 {
		t.Errorf("only %d of the seeds are readable documents", text)
	}
}
