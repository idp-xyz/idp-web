package pdfreader

import (
	"flag"
	"fmt"
	"idp-web/internal/pdfread"
	"io"
	"os"
	"strconv"
	"time"
)

// ChildMain is the command idp-web-pdf: the PDF is read from standard input, the text written to standard output, and
// standard error says how it ended. It speaks the protocol of the evaluation (tools/web_eval/e4pdf.py) so that what is
// measured is what runs:
//
//	exit 0: standard output is the text; standard error has one line, "pdf pages=<n> read=<n> title=<quoted>"
//	any other exit: the first line of standard error starts with a reason word (no_text_layer, encrypted, malformed,
//	too_slow, too_large, unavailable) and a detail for the log
//
// Before it reads anything it limits itself (limits_linux.go): address space, CPU time, files, and the system calls it
// has no use for. A deadline of its own ends it with too_slow, a second line behind the one the service keeps.
func ChildMain(args []string, in io.Reader, out, errw io.Writer, exit func(int)) int {
	closeInheritedFiles() // before anything is opened: only standard input, output and error are the process's own
	fs := flag.NewFlagSet("idp-web-pdf", flag.ContinueOnError)
	fs.SetOutput(errw)
	deadline := fs.Duration("deadline", pdfread.DefaultDeadline, "wall-clock time for the whole document")
	memory := fs.Int("memory-mib", pdfread.DefaultMemoryMiB, "memory the process may hold (RLIMIT_DATA); the address space is limited to this plus 4 GiB")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *memory < 64 || *deadline < 100*time.Millisecond || *deadline > 60*time.Second {
		fmt.Fprintf(errw, "%s: memory-mib must be at least 64 and the deadline between 100ms and 60s\n", pdfread.ReasonUnavailable)
		return 2
	}
	if err := applyProcessLimits(processLimits{MemoryBytes: uint64(*memory) << 20, AddressBytes: uint64(*memory)<<20 + addressSlack, CPUSeconds: uint64(*deadline/time.Second) + 2}); err != nil {
		fmt.Fprintf(errw, "%s: the limits could not be applied: %v\n", pdfread.ReasonUnavailable, err)
		return 1
	}
	timer := time.AfterFunc(*deadline, func() {
		fmt.Fprintf(errw, "%s: not finished in %s\n", pdfread.ReasonTooSlow, *deadline)
		exit(1)
	})
	defer timer.Stop()
	lim := DefaultLimits()
	data, err := io.ReadAll(io.LimitReader(in, pdfread.MaxInput+1))
	if err != nil {
		fmt.Fprintf(errw, "%s: cannot read the document: %v\n", pdfread.ReasonMalformed, err)
		return 1
	}
	if len(data) > pdfread.MaxInput {
		fmt.Fprintf(errw, "%s: the document is larger than %d bytes\n", pdfread.ReasonTooLarge, pdfread.MaxInput)
		return 1
	}
	res, err := readDoc(data, lim)
	if err != nil {
		fmt.Fprintf(errw, "%s: %v\n", pdfread.ReasonOf(err), err)
		return 1
	}
	if _, err := io.WriteString(out, res.Text); err != nil {
		fmt.Fprintf(errw, "%s: cannot write the text: %v\n", pdfread.ReasonMalformed, err)
		return 1
	}
	fmt.Fprintf(errw, "pdf pages=%d read=%d title=%s\n", res.Pages, res.PagesRead, strconv.Quote(res.Title))
	return 0
}

// readDoc is Read; tests replace it, in the processes they start as the command, with hostile readers.
var readDoc = Read

const addressSlack = 4 << 30 // what the Go runtime reserves and never touches

// Main is main() of the command.
func Main() {
	os.Exit(ChildMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Exit))
}
