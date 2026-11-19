// Command idp-web-addr is the address rule of idp-web (internal/addr) on its own, for the differential test against the
// reference check in tools/web_eval/fetch.py (check_address).
//
//	idp-web-addr < urls.txt
//
// It reads one URL per line on standard input and writes one line per URL: "allow", or "refuse " and the reason. It
// never resolves a name: it judges the form of the URL and any IP literal in it, which is the part of the rule that does
// not depend on the network. A name that Target accepts is "allow" here although the reader may still refuse what it
// resolves to. Blank lines are answered too (they are refused: there is no address), so that line n of the output is
// line n of the input. The exit status is 0.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"idp-web/internal/addr"
)

func main() { os.Exit(run(os.Stdin, os.Stdout)) }

func run(in io.Reader, out io.Writer) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	w := bufio.NewWriter(out)
	defer w.Flush()
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if _, err := addr.Target(line); err != nil {
			// the reason is one line whatever the input held
			fmt.Fprintf(w, "refuse %s\n", strings.Join(strings.Fields(strings.TrimPrefix(err.Error(), addr.ErrRefused.Error()+": ")), " "))
		} else {
			fmt.Fprintln(w, "allow")
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "idp-web-addr:", err)
		return 2
	}
	return 0
}
