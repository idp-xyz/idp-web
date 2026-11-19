// Command idp-web-extract reads HTML (already decoded to UTF-8) from standard input and writes its main text as
// Markdown to standard output. It exists so that the evaluation (tools/web_eval, WEB_EVAL_EXTRACTORS) can score the
// extraction of idp-web on the same annotated pages as the Python reference (ADR-0001 D5).
//
//	idp-web-extract [-url ADDRESS] [-engine trafilatura|readability|plain] [-focus balanced|precision|recall] [-no-fallback] < page.html
//
// Standard error gets one line, "engine=<what produced the text> title=<title>". The exit status is 0 even when the
// text is empty; it is 2 for a flag or input that cannot be used.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"idp-web/internal/extract"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, in io.Reader, out, errw io.Writer) int {
	fs := flag.NewFlagSet("idp-web-extract", flag.ContinueOnError)
	fs.SetOutput(errw)
	pageURL := fs.String("url", "", "the address the page was read from")
	engine := fs.String("engine", extract.Trafilatura, "trafilatura, readability or plain")
	focus := fs.String("focus", extract.Balanced, "balanced, precision or recall (trafilatura only)")
	noFallback := fs.Bool("no-fallback", false, "do not replace an empty or too short result by the plain reader's text")
	noInner := fs.Bool("no-inner-fallback", false, "switch off go-trafilatura's own readability and dom-distiller fallback")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	body, err := io.ReadAll(in)
	if err != nil {
		fmt.Fprintln(errw, "idp-web-extract: cannot read the page:", err)
		return 2
	}
	res, err := extract.Extract(body, *pageURL, extract.Options{Engine: *engine, Focus: *focus, NoFallback: *noFallback, NoInnerFallback: *noInner})
	if err != nil {
		fmt.Fprintln(errw, "idp-web-extract:", err)
		return 2
	}
	fmt.Fprintf(errw, "engine=%s title=%s\n", res.Engine, strings.Join(strings.Fields(res.Title), " "))
	if _, err := io.WriteString(out, res.Markdown); err != nil {
		fmt.Fprintln(errw, "idp-web-extract: cannot write the text:", err)
		return 2
	}
	return 0
}
