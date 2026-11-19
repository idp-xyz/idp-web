// Command idp-web-pdf reads a PDF from standard input and writes its text, a marker for each page, to standard output.
// It is the process that idp-web starts for each PDF it is given (ADR-0001 D31) and the command the PDF evaluation
// scores (tools/web_eval/e4pdf.py, --cmd): what is measured is what runs.
//
//	idp-web-pdf [-deadline 4s] [-memory-mib 512] < document.pdf
//
// Exit status 0 is a text; otherwise standard error starts with a reason word (no_text_layer, encrypted, malformed,
// too_slow, too_large, unavailable). See internal/pdfread.
package main

import "idp-web/internal/pdfreader"

func main() { pdfreader.Main() }
