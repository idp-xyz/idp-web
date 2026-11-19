package pdfreader

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"strings"
	"testing"

	"idp-web/internal/pdfread"
	"idp-web/internal/pdftest"
)

func mustRead(t *testing.T, doc []byte) Result {
	t.Helper()
	res, err := Read(doc, DefaultLimits())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return res
}

func wantErr(t *testing.T, doc []byte, lim Limits, want error) {
	t.Helper()
	_, err := Read(doc, lim)
	if !errors.Is(err, want) {
		t.Fatalf("error %v, want %v", err, want)
	}
}

func TestEachPageHasAMarkerTheModelCanCite(t *testing.T) {
	res := mustRead(t, pdftest.Text("The first page says that the tariff is in force.", "The second page says the surcharge is waived."))
	if res.Pages != 2 || res.PagesRead != 2 {
		t.Errorf("pages %d read %d", res.Pages, res.PagesRead)
	}
	i1, i2 := strings.Index(res.Text, "[page 1]"), strings.Index(res.Text, "[page 2]")
	if i1 != 0 || i2 < 0 {
		t.Fatalf("markers at %d and %d in %q", i1, i2, res.Text)
	}
	if !strings.Contains(res.Text[i1:i2], "tariff is in force") || !strings.Contains(res.Text[i2:], "surcharge is waived") {
		t.Errorf("the text is not under its own page's marker: %q", res.Text)
	}
	if strings.Contains(res.Text, "only the first") {
		t.Errorf("a complete document says it was cut: %q", res.Text)
	}
}

func TestFlateCompressedContentIsRead(t *testing.T) {
	res := mustRead(t, pdftest.TextFlate("A page whose content stream is compressed, as nearly every real file's is."))
	if !strings.Contains(res.Text, "content stream is compressed") {
		t.Errorf("%q", res.Text)
	}
}

func TestTextInACIDFontWithAToUnicodeMapIsRead(t *testing.T) {
	// the shape of every Chinese document of the evaluation: two-byte codes, and only the map says what they are
	res := mustRead(t, pdftest.CID("本通知自发布之日起施行，请各单位遵照执行。", "第二页：运费按重量计算，每公斤十二元。"))
	for _, want := range []string{"本通知自发布之日起施行", "运费按重量计算，每公斤十二元"} {
		if !strings.Contains(strings.ReplaceAll(res.Text, " ", ""), want) {
			t.Errorf("%q is missing from %q", want, res.Text)
		}
	}
}

func TestTheCellsOfARowComeOutOnOneLineInTheOrderOfTheColumns(t *testing.T) {
	// what the reader gives for a table: its rows, as lines, the cells in column order and apart by a space or more;
	// not Markdown (the library's own Tables are not used, see ADR-0001 D31)
	res := mustRead(t, pdftest.Cells([][]string{{"Zone", "Weight kg", "Price EUR"}, {"Zone 3", "0.5", "21.40"}, {"Zone 4", "1.0", "29.90"}}))
	lines := strings.Split(res.Text, "\n")
	found := 0
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) >= 4 && f[0] == "Zone" && (f[1] == "3" || f[1] == "4") {
			found++
			if !(strings.Index(l, "0.5") < strings.Index(l, "21.40") || strings.Index(l, "1.0") < strings.Index(l, "29.90")) {
				t.Errorf("columns out of order: %q", l)
			}
		}
	}
	if found != 2 {
		t.Errorf("each row should be one line, got %q", res.Text)
	}
}

func TestADocumentTitleIsReadFromItsInfo(t *testing.T) {
	res := mustRead(t, pdftest.Titled("Rate guide 2026", "A page with enough words on it to count as text."))
	if res.Title != "Rate guide 2026" {
		t.Errorf("title %q", res.Title)
	}
	if got := mustRead(t, pdftest.Text("A page with enough words on it to count as text.")).Title; got != "" {
		t.Errorf("a document without one has title %q", got)
	}
}

func TestPagesWithoutAnyTextMakeADocumentWithNoTextLayer(t *testing.T) {
	wantErr(t, pdftest.Graphics(3), DefaultLimits(), pdfread.ErrNoTextLayer)
}

func TestAScanWithAStampOnOnePageHasNoTextLayer(t *testing.T) {
	// the archive's stamp of the evaluation: a dozen characters on one of 39 pages
	wantErr(t, pdftest.Stamped(39, "ARCHIVES 1947"), DefaultLimits(), pdfread.ErrNoTextLayer)
	// and a page's worth of text is a stamp too when it is one page in a hundred
	pages := make([]string, 100)
	pages[0] = "Short."
	wantErr(t, pdftest.Text(pages...), DefaultLimits(), pdfread.ErrNoTextLayer)
}

func TestTheRuleForATextLayerIsTwentyCharactersOnOneTenthOfThePages(t *testing.T) {
	real := "This page holds a full line of ordinary text."
	nineteen := "abcdefghi jklmnopqr" // 18 characters without the space
	twenty := "abcdefghij klmnopqrst"
	mk := func(withText int, text string, total int) []byte {
		pages := make([]string, total)
		for i := 0; i < withText; i++ {
			pages[i] = text
		}
		return pdftest.Text(pages...)
	}
	// 1 page of 10 is enough, 1 of 11 is not
	res := mustRead(t, mk(1, real, 10))
	if !strings.Contains(res.Text, "(no text on this page)") {
		t.Errorf("a page without text is not said to be: %q", res.Text)
	}
	wantErr(t, mk(1, real, 11), DefaultLimits(), pdfread.ErrNoTextLayer)
	// below the minimum a page does not count, at it it does
	wantErr(t, mk(1, nineteen, 1), DefaultLimits(), pdfread.ErrNoTextLayer)
	mustRead(t, mk(1, twenty, 1))
}

func TestAnEncryptedDocumentIsRefusedOnItsOwn(t *testing.T) {
	wantErr(t, pdftest.Encrypted(), DefaultLimits(), pdfread.ErrEncrypted)
}

func TestADocumentOfMoreThanTheLimitIsReadToTheLimitAndSaysSoAtTheTop(t *testing.T) {
	res := mustRead(t, pdftest.Many(201, "Paragraph of the long document"))
	if res.Pages != 201 || res.PagesRead != 200 {
		t.Fatalf("pages %d read %d", res.Pages, res.PagesRead)
	}
	if !strings.HasPrefix(res.Text, "[PDF, 201 pages; only the first 200 are read]\n") {
		t.Errorf("the top of the text: %q", res.Text[:80])
	}
	if !strings.Contains(res.Text, "[page 200]") || strings.Contains(res.Text, "[page 201]") {
		t.Error("page 200 must be the last")
	}
	res = mustRead(t, pdftest.Many(200, "Paragraph of the long document"))
	if res.PagesRead != 200 || strings.Contains(res.Text, "only the first") {
		t.Errorf("exactly 200 pages are all read: %d, %.60q", res.PagesRead, res.Text)
	}
	// the limit is a setting of the limits, not of one document
	lim := DefaultLimits()
	lim.MaxPages = 3
	res, err := Read(pdftest.Many(10, "Paragraph of the long document"), lim)
	if err != nil || res.PagesRead != 3 {
		t.Errorf("%v %d", err, res.PagesRead)
	}
}

func TestTheOutputIsBoundedAndTheDocumentIsCutAtAPageAndSaysSo(t *testing.T) {
	line := strings.Repeat("A line of text that fills the page. ", 20)
	lim := DefaultLimits()
	lim.MaxOutput = 3000
	res, err := Read(pdftest.Many(50, line), lim)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > lim.MaxOutput+200 || res.PagesRead >= 50 || res.PagesRead < 1 {
		t.Fatalf("%d bytes, %d pages read", len(res.Text), res.PagesRead)
	}
	if !strings.Contains(res.Text[:120], fmt.Sprintf("only the first %d are read", res.PagesRead)) {
		t.Errorf("the cut is not said: %.120q", res.Text)
	}
	if strings.Contains(res.Text, fmt.Sprintf("[page %d]", res.PagesRead+1)) {
		t.Error("a page beyond the cut is in the text")
	}
}

func TestAFirstPageThatIsMoreThanTheWholeBoundIsCutAndNotTakenForNoText(t *testing.T) {
	big := strings.TrimSpace(strings.Repeat("A line of text that fills the page and then some. \n", 200))
	lim := DefaultLimits()
	lim.MaxOutput = 3000
	res, err := Read(pdftest.Text(big, "A second page."), lim)
	if err != nil || res.PagesRead != 1 || len(res.Text) > lim.MaxOutput+200 || !strings.Contains(res.Text, "only the first 1 are read") {
		t.Fatalf("%v pages read %d, %d bytes", err, res.PagesRead, len(res.Text))
	}
}

func TestAFlateBombIsRefusedBeforeItIsInflated(t *testing.T) {
	// 512 MiB of spaces in a few hundred kilobytes: refused by looking at how much would come out, with a counter that
	// keeps nothing. The file is the one the evaluation's generator makes.
	doc := pdftest.Bomb(256)
	if len(doc) > 1<<20 {
		t.Fatalf("the bomb is %d bytes", len(doc))
	}
	wantErr(t, doc, DefaultLimits(), pdfread.ErrTooLarge)
}

func TestManySmallerStreamsAreRefusedByTheirSum(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxStream, lim.MaxInflated = 4<<20, 10<<20
	one := pdftest.Bomb(3)
	if _, err := Read(one, lim); errors.Is(err, pdfread.ErrTooLarge) {
		t.Fatalf("3 MiB under a limit of 4 MiB was refused")
	}
	// the same stream five times in one file: each is under the limit, together they are over the sum
	objs := map[int][]byte{1: []byte("<< /Type /Catalog /Pages 2 0 R >>"), 2: []byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		3: []byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << >> >>")}
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.BestCompression)
	w.Write(bytes.Repeat([]byte{' '}, 3<<20))
	w.Close()
	for i := 0; i < 5; i++ {
		objs[10+i] = pdftest.Stream(b.Bytes(), "/Filter /FlateDecode")
	}
	wantErr(t, pdftest.File(objs, ""), lim, pdfread.ErrTooLarge)
}

func TestAPictureThatInflatesToALotIsNotAStreamThatIsRead(t *testing.T) {
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.BestCompression)
	w.Write(bytes.Repeat([]byte{0}, 80<<20)) // an 80 MiB raster
	w.Close()
	objs := map[int][]byte{1: []byte("<< /Type /Catalog /Pages 2 0 R >>"), 2: []byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		3: []byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>"),
		4: pdftest.Stream([]byte("BT /F1 12 Tf 10 100 Td (A page with a big picture and a sentence of text.) Tj ET"), ""),
		5: []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
		6: pdftest.Stream(b.Bytes(), "/Type /XObject /Subtype /Image /Width 8000 /Height 10000 /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /FlateDecode")}
	res, err := Read(pdftest.File(objs, ""), DefaultLimits())
	if err != nil || !strings.Contains(res.Text, "sentence of text") {
		t.Fatalf("%v %q", err, res.Text)
	}
}

func TestDamagedAndForeignFilesAreMalformedAndNeverAPanic(t *testing.T) {
	good := pdftest.Text("A page with enough words on it to count as text.")
	cases := map[string][]byte{
		"empty":       nil,
		"html":        pdftest.HTML(),
		"header only": []byte("%PDF-1.7\n"),
		"garbage":     append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0xff, 0x00, 0x13}, 5000)...),
		"truncated":   good[:len(good)/2],
		"page cycle":  pdftest.PageTreeCycle(),
		"deep tree":   pdftest.PageTreeDeep(20000),
	}
	for name, doc := range cases {
		_, err := Read(doc, DefaultLimits())
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if !errors.Is(err, pdfread.ErrMalformed) && !errors.Is(err, pdfread.ErrNoTextLayer) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheOutputIsValidTextWithoutControlCharacters(t *testing.T) {
	res := mustRead(t, pdftest.Text("A line with a bell \a and a form feed \f in it, long enough to count."))
	if strings.ContainsAny(res.Text, "\a\f") {
		t.Errorf("%q", res.Text)
	}
}

func TestAPanicOfTheParserIsAnErrorAndNotACrash(t *testing.T) {
	old := readFn
	defer func() { readFn = old }()
	readFn = func([]byte, Limits) (Result, error) { var m map[string]int; m["x"] = 1; return Result{}, nil }
	_, err := Read(pdftest.Text("irrelevant"), DefaultLimits())
	if !errors.Is(err, pdfread.ErrMalformed) {
		t.Errorf("%v", err)
	}
}

func TestPreflightIgnoresWhatIsNotAZlibStream(t *testing.T) {
	doc := pdftest.File(map[int][]byte{1: []byte("<< /Type /Catalog /Pages 2 0 R >>"), 2: []byte("<< /Type /Pages /Kids [] /Count 0 >>"),
		3: pdftest.Stream([]byte("this is not zlib data at all"), "/Filter /FlateDecode")}, "")
	if err := preflight(doc, 1<<20, 1<<20); err != nil {
		t.Errorf("%v", err)
	}
}
