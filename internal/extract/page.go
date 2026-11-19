package extract

import "unicode/utf8"

// PageBytes is the most one part of a page may hold: what the model gets from one web_fetch call (platform
// ADR-0003 D4).
const PageBytes = 12 << 10

// Page returns the part of text that starts at byte offset and holds at most limit bytes (and never more than
// PageBytes), the offset to ask for next (-1 when this part reaches the end) and the total length in bytes. Parts are
// cut only between characters, so no part holds half of a UTF-8 sequence, and the parts of one text laid end to end are
// the whole text. An offset inside a character moves on to the next character boundary; one past the end gives an
// empty part. The offsets are bytes, as in the reference (tools/web_eval/fetch.py, page).
func Page(text string, offset, limit int) (part string, next int, total int) {
	total = len(text)
	if limit <= 0 || limit > PageBytes {
		limit = PageBytes
	}
	if limit < utf8.UTFMax {
		limit = utf8.UTFMax // a smaller limit could not hold every character, and the paging would never advance
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	for offset < total && !utf8.RuneStart(text[offset]) {
		offset++
	}
	end := offset + limit
	if end >= total {
		return text[offset:], -1, total
	}
	for end > offset && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[offset:end], end, total
}
