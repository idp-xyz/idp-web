// Package pdftest builds small PDF files byte by byte, for the tests of the PDF reader and of everything that hands it
// a document (ADR-0001 D31). No document from the web and no text from one is in this repository: every file the tests
// read is made here. The functions are plain and the files they make are valid unless the name says otherwise; they
// are the seed corpus of the fuzz target too.
package pdftest

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
)

// File writes a PDF from numbered objects, with a correct cross-reference table. Object 1 must be the catalog.
// trailer is added inside the trailer dictionary.
func File(objects map[int][]byte, trailer string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	nums := make([]int, 0, len(objects))
	for n := range objects {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	offs := map[int]int{}
	for _, n := range nums {
		offs[n] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", n)
		out.Write(objects[n])
		out.WriteString("\nendobj\n")
	}
	size := nums[len(nums)-1] + 1
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", size)
	for n := 1; n < size; n++ {
		if off, ok := offs[n]; ok {
			fmt.Fprintf(&out, "%010d 00000 n \n", off)
		} else {
			out.WriteString("0000000000 65535 f \n")
		}
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R %s >>\nstartxref\n%d\n%%%%EOF\n", size, trailer, xref)
	return out.Bytes()
}

// Stream is a stream object; extra goes into its dictionary (for example "/Filter /FlateDecode").
func Stream(body []byte, extra string) []byte {
	return append([]byte(fmt.Sprintf("<< /Length %d %s >>\nstream\n", len(body), extra)), append(body, []byte("\nendstream")...)...)
}

// Flate compresses body the way a producer does.
func Flate(body []byte) []byte {
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.BestCompression)
	w.Write(body)
	w.Close()
	return b.Bytes()
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(s)
}

// Text is a document of one page per argument, in Helvetica; the lines of a page are separated by "\n", and an empty
// string is a page with drawing and no text. Only characters of Latin-1 are written (the font has the standard WinAnsi
// encoding).
func Text(pages ...string) []byte {
	return textDoc(false, pages)
}

// TextFlate is Text with Flate-compressed content streams, which is how nearly every real file is written.
func TextFlate(pages ...string) []byte {
	return textDoc(true, pages)
}

func textDoc(flate bool, pages []string) []byte {
	objs := map[int][]byte{1: []byte("<< /Type /Catalog /Pages 2 0 R >>")}
	var kids []string
	for i, p := range pages {
		pageObj, contObj := 10+2*i, 11+2*i
		kids = append(kids, fmt.Sprintf("%d 0 R", pageObj))
		var c strings.Builder
		if p == "" {
			c.WriteString("0 0 m 612 792 l S") // a page of drawing and no text: what a scanned page is to an extractor
		} else {
			c.WriteString("BT /F1 12 Tf 14 TL 50 740 Td\n")
			for _, line := range strings.Split(p, "\n") {
				fmt.Fprintf(&c, "(%s) Tj T*\n", escape(line))
			}
			c.WriteString("ET")
		}
		objs[pageObj] = []byte(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", contObj))
		if flate {
			objs[contObj] = Stream(Flate([]byte(c.String())), "/Filter /FlateDecode")
		} else {
			objs[contObj] = Stream([]byte(c.String()), "")
		}
	}
	objs[2] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	objs[3] = []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	return File(objs, "")
}

// Titled is Text with a document title in the Info dictionary.
func Titled(title string, pages ...string) []byte {
	objs := map[int][]byte{}
	objs[1] = []byte("<< /Type /Catalog /Pages 2 0 R >>")
	var kids []string
	for i, p := range pages {
		pageObj, contObj := 10+2*i, 11+2*i
		kids = append(kids, fmt.Sprintf("%d 0 R", pageObj))
		objs[pageObj] = []byte(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", contObj))
		objs[contObj] = Stream([]byte(fmt.Sprintf("BT /F1 12 Tf 50 740 Td (%s) Tj ET", escape(p))), "")
	}
	objs[2] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	objs[3] = []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	objs[4] = []byte(fmt.Sprintf("<< /Title (%s) >>", escape(title)))
	return File(objs, "/Info 4 0 R")
}

// CID is a document whose text is in a Type 0 font with the Identity-H encoding, as every Chinese document of the
// evaluation is: the content stream holds two-byte glyph codes and only the font's /ToUnicode map says what they
// mean. One page per argument; lines separated by "\n". The font program is not embedded (the reader needs only the
// map).
func CID(pages ...string) []byte {
	codes := map[rune]int{}
	var order []rune
	for _, p := range pages {
		for _, r := range p {
			if r == '\n' {
				continue
			}
			if _, ok := codes[r]; !ok {
				codes[r] = len(order) + 1
				order = append(order, r)
			}
		}
	}
	var cmap strings.Builder
	cmap.WriteString("/CIDInit /ProcSet findresource begin 12 dict begin begincmap\n/CMapName /Adobe-Identity-UCS def /CMapType 2 def\n1 begincodespacerange <0000> <FFFF> endcodespacerange\n")
	for i := 0; i < len(order); i += 100 {
		end := min(i+100, len(order))
		fmt.Fprintf(&cmap, "%d beginbfchar\n", end-i)
		for _, r := range order[i:end] {
			u := utf16.Encode([]rune{r})
			var hex strings.Builder
			for _, w := range u {
				fmt.Fprintf(&hex, "%04X", w)
			}
			fmt.Fprintf(&cmap, "<%04X> <%s>\n", codes[r], hex.String())
		}
		cmap.WriteString("endbfchar\n")
	}
	cmap.WriteString("endcmap CMapName currentdict /CMap defineresource pop end end")
	objs := map[int][]byte{1: []byte("<< /Type /Catalog /Pages 2 0 R >>")}
	var kids []string
	for i, p := range pages {
		pageObj, contObj := 10+2*i, 11+2*i
		kids = append(kids, fmt.Sprintf("%d 0 R", pageObj))
		var c strings.Builder
		c.WriteString("BT /F1 12 Tf 14 TL 50 740 Td\n")
		for _, line := range strings.Split(p, "\n") {
			c.WriteString("<")
			for _, r := range line {
				fmt.Fprintf(&c, "%04X", codes[r])
			}
			c.WriteString("> Tj T*\n")
		}
		c.WriteString("ET")
		objs[pageObj] = []byte(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", contObj))
		objs[contObj] = Stream([]byte(c.String()), "")
	}
	objs[2] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	objs[3] = []byte("<< /Type /Font /Subtype /Type0 /BaseFont /TestSong /Encoding /Identity-H /DescendantFonts [4 0 R] /ToUnicode 6 0 R >>")
	objs[4] = []byte("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /TestSong /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /DW 1000 /FontDescriptor 5 0 R >>")
	objs[5] = []byte("<< /Type /FontDescriptor /FontName /TestSong /Flags 4 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 >>")
	objs[6] = Stream([]byte(cmap.String()), "")
	return File(objs, "")
}

// Graphics is n pages that draw lines and no text at all: what a scan looks like to a text extractor (the picture
// itself is left out, it does not matter).
func Graphics(n int) []byte { return Text(make([]string, n)...) }

// Stamped is n pages of drawing, with one short line of text (an archive's stamp) on the first.
func Stamped(n int, stamp string) []byte {
	pages := make([]string, n)
	pages[0] = stamp
	return Text(pages...)
}

// Cells is one page of a table: each row of the argument on one baseline, its cells 150 points apart.
func Cells(rows [][]string) []byte {
	var c strings.Builder
	for i, row := range rows {
		for j, cell := range row {
			fmt.Fprintf(&c, "BT /F1 12 Tf %d %d Td (%s) Tj ET\n", 50+150*j, 740-16*i, escape(cell))
		}
	}
	return File(map[int][]byte{
		1:  []byte("<< /Type /Catalog /Pages 2 0 R >>"),
		2:  []byte("<< /Type /Pages /Kids [10 0 R] /Count 1 >>"),
		3:  []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"),
		10: []byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 11 0 R /Resources << /Font << /F1 3 0 R >> >> >>"),
		11: Stream([]byte(c.String()), ""),
	}, "")
}

// Many is n pages of the same one line, for the page limit.
func Many(n int, line string) []byte {
	pages := make([]string, n)
	for i := range pages {
		pages[i] = fmt.Sprintf("%s number %d", line, i+1)
	}
	return Text(pages...)
}

// Encrypted is a document with a standard-handler Encrypt dictionary whose made-up O and U no password opens.
func Encrypted() []byte {
	o, u := bytes.Repeat([]byte{'A'}, 32), bytes.Repeat([]byte{'B'}, 32)
	return File(map[int][]byte{
		1: []byte("<< /Type /Catalog /Pages 2 0 R >>"),
		2: []byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		3: []byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R /Resources << >> >>"),
		4: Stream(bytes.Repeat([]byte{7}, 40), ""),
		5: []byte(fmt.Sprintf("<< /Filter /Standard /V 1 /R 2 /Length 40 /P -44 /O <%x> /U <%x> >>", o, u)),
	}, "/Encrypt 5 0 R /ID [<00112233445566778899aabbccddeeff> <00112233445566778899aabbccddeeff>]")
}

// Bomb is a one-page document whose content stream is Flate data that inflates to mib MiB of spaces: a few hundred
// kilobytes on disk.
func Bomb(mib int) []byte {
	bombMu.Lock()
	defer bombMu.Unlock()
	if doc, ok := bombs[mib]; ok {
		return doc
	}
	doc := makeBomb(mib)
	bombs[mib] = doc
	return doc
}

var (
	bombMu sync.Mutex
	bombs  = map[int][]byte{} // compressing 512 MiB takes seconds, and the tests ask for it more than once
)

func makeBomb(mib int) []byte {
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.BestSpeed)
	chunk := bytes.Repeat([]byte{' '}, 1<<20)
	for i := 0; i < mib; i++ {
		w.Write(chunk)
	}
	w.Close()
	return File(map[int][]byte{
		1: []byte("<< /Type /Catalog /Pages 2 0 R >>"),
		2: []byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		3: []byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R /Resources << >> >>"),
		4: Stream(b.Bytes(), "/Filter /FlateDecode"),
	}, "")
}

// PageTreeDeep is a page tree nested depth levels deep with one empty page at the bottom.
func PageTreeDeep(depth int) []byte {
	objs := map[int][]byte{1: []byte("<< /Type /Catalog /Pages 2 0 R >>")}
	for n := 2; n < 2+depth; n++ {
		objs[n] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", n+1))
	}
	objs[2+depth] = []byte("<< /Type /Page /MediaBox [0 0 200 200] /Resources << >> >>")
	return File(objs, "")
}

// PageTreeCycle is a page tree whose root lists itself.
func PageTreeCycle() []byte {
	return File(map[int][]byte{
		1: []byte("<< /Type /Catalog /Pages 2 0 R >>"),
		2: []byte("<< /Type /Pages /Kids [2 0 R 3 0 R] /Count 2 >>"),
		3: []byte("<< /Type /Pages /Kids [2 0 R] /Count 1 /Parent 2 0 R >>"),
	}, "")
}

// HTML is an error page, as a server that labels it application/pdf would send it.
func HTML() []byte {
	return []byte("<!doctype html><html><head><title>Moved</title></head><body><h1>Error 404</h1><p>The page you requested has moved to a new address, which is not this one.</p></body></html>\n")
}
