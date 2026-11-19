package pdfreader

import (
	"bytes"
	"compress/zlib"
	"idp-web/internal/pdfread"
	"io"
)

// preflight refuses a document with a Flate stream that would inflate beyond the limits, before the parser inflates
// anything. The parser (go-pdfkit/reader) caps one stream at 1 GiB and that cap cannot be set from outside, so a
// 5 MiB file of Flate bombs would make it allocate gigabytes; this pass inflates every candidate stream into a counter
// that keeps nothing, and stops at the limit. It does not trust the file's own structure (no xref, no /Length): it
// finds streams by the keyword, takes the filter from the dictionary text in front, and starts inflating at the data
// whatever the declared length. A stream the parser will read differently (a lying /Length) is no worse: the data is
// then read from the same start and ends at the end of the Flate data.
//
// Image streams are skipped: the parser is never asked to decode a picture for text, and a large picture legitimately
// inflates to tens of megabytes. Streams whose filter is a chain (ASCII85 then Flate) or is named by an indirect
// object are not probed; for them the address-space limit of the child process is the control (limits_linux.go).
func preflight(data []byte, perStream, total int64) error {
	var sum int64
	for pos := 0; ; {
		i := bytes.Index(data[pos:], []byte("stream"))
		if i < 0 {
			return nil
		}
		at := pos + i
		pos = at + len("stream")
		if at >= 3 && string(data[at-3:at]) == "end" {
			continue
		}
		start := pos
		if start < len(data) && data[start] == '\r' {
			start++
		}
		if start < len(data) && data[start] == '\n' {
			start++
		} else if start == pos {
			continue // "stream" not followed by an end of line: not the keyword
		}
		dict := data[max(0, at-2048):at]
		if j := bytes.LastIndex(dict, []byte("obj")); j >= 0 {
			dict = dict[j+3:]
		}
		if !bytes.Contains(dict, []byte("/Fl")) || bytes.Contains(dict, []byte("/Image")) {
			continue
		}
		n, err := inflatedSize(data[start:], perStream+1)
		if err == errSkip {
			continue
		}
		sum += n
		if n > perStream || sum > total {
			return pdfread.ErrTooLarge
		}
	}
}

type skip struct{}

func (skip) Error() string { return "not a zlib stream" }

var errSkip error = skip{}

// inflatedSize inflates up to limit bytes of a zlib stream and returns how many it produced; errSkip when the data is
// no zlib stream at all (the parser then reports it itself, or reads it as something else).
func inflatedSize(b []byte, limit int64) (int64, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return 0, errSkip
	}
	defer zr.Close()
	n, _ := io.Copy(io.Discard, io.LimitReader(zr, limit)) // a damaged stream is the parser's to report
	return n, nil
}
