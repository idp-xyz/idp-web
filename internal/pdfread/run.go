package pdfread

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Output is what a reader process returned for one document.
type Output struct {
	Title     string
	Pages     int // in the document
	PagesRead int
	Text      string
}

// Runner starts the command idp-web-pdf once for each document (ADR-0001 D31). It is safe for concurrent use.
//
// What the process is given: the document on standard input, nothing in its environment, no other open file, the root
// directory as its working directory, its own process group. What it is not given is anything to do with the
// service's secrets or sockets: Go's os/exec closes every descriptor but 0, 1 and 2 in the child.
type Runner struct {
	// Path of the command. Args come before the flags the Runner adds (tests use them to start the test binary as the
	// command).
	Path string
	Args []string
	// Deadline is how long one document may take (the command is told it too, and ends itself a little earlier);
	// MemoryMiB is the memory limit the command applies to itself. Zero takes the defaults.
	Deadline  time.Duration
	MemoryMiB int
	// MaxOutput bounds the bytes read from the command's standard output; a process that writes more is killed.
	MaxOutput int
	// Wait is how long a call waits for one of the slots when all are busy.
	Wait time.Duration
	Log  *slog.Logger

	slots chan struct{}
	once  sync.Once
}

// ServiceDeadline is the time the service gives one document: the call's 15 seconds also hold the download, and the
// remaining time of the call always caps it.
const (
	ServiceDeadline = 6 * time.Second
	slotWait        = 2 * time.Second
	killGrace       = time.Second // between the command's own deadline and the kill from outside
	stderrKeep      = 4 << 10
)

// NewRunner returns a Runner for the command at path that runs at most concurrency documents at once.
func NewRunner(path string, concurrency int, log *slog.Logger) *Runner {
	if concurrency < 1 {
		concurrency = 1
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{Path: path, Deadline: ServiceDeadline, MemoryMiB: DefaultMemoryMiB, MaxOutput: MaxOutputBytes + 64<<10,
		Wait: slotWait, Log: log, slots: make(chan struct{}, concurrency)}
}

// FindCommand returns the path of idp-web-pdf: the value of explicit when it is not empty, else the file of that name
// next to the running executable (in the image, both are in the root directory).
func FindCommand(explicit string) (string, error) {
	path := explicit
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		path = filepath.Join(filepath.Dir(exe), "idp-web-pdf")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable file", path)
	}
	return path, nil
}

// Read gives the document to a new process and returns its text. At most Deadline of the call's remaining time is
// spent; when all the slots are taken the call waits up to Wait for one and then fails with ErrBusy.
func (r *Runner) Read(ctx context.Context, pdf []byte) (Output, error) {
	if r.slots == nil {
		return Output{}, ErrUnavailable
	}
	if r.Wait > 0 {
		timer := time.NewTimer(r.Wait)
		defer timer.Stop()
		select {
		case r.slots <- struct{}{}:
		case <-timer.C:
			return Output{}, ErrBusy
		case <-ctx.Done():
			return Output{}, ctx.Err()
		}
	} else {
		select {
		case r.slots <- struct{}{}:
		default:
			return Output{}, ErrBusy
		}
	}
	defer func() { <-r.slots }()

	deadline := r.Deadline
	if deadline <= 0 {
		deadline = ServiceDeadline
	}
	if dl, ok := ctx.Deadline(); ok {
		// the call's own end, less what it takes to answer
		if left := time.Until(dl) - 500*time.Millisecond; left < deadline {
			deadline = left
		}
	}
	if deadline < 500*time.Millisecond {
		return Output{}, ErrTooSlow
	}
	mem := r.MemoryMiB
	if mem == 0 {
		mem = DefaultMemoryMiB
	}
	max := r.MaxOutput
	if max <= 0 {
		max = MaxOutputBytes + 64<<10
	}

	// the command ends itself at the deadline it is told; the kill from outside comes one grace later
	rctx, cancel := context.WithTimeout(ctx, deadline+killGrace)
	defer cancel()
	args := append(append([]string{}, r.Args...), "-deadline", deadline.String(), "-memory-mib", strconv.Itoa(mem))
	cmd := exec.CommandContext(rctx, r.Path, args...)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(pdf)
	out := &capped{max: max, over: cancel}
	errb := &head{max: stderrKeep}
	cmd.Stdout, cmd.Stderr = out, errb
	setProcAttr(cmd)
	cmd.WaitDelay = 2 * time.Second
	start := time.Now()
	err := cmd.Run()
	took := time.Since(start)

	switch {
	case ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded):
		return Output{}, ctx.Err() // the caller gave up
	case out.exceeded:
		r.Log.Warn("the PDF reader wrote more than it may", "limit", max, "took", took)
		return Output{}, ErrTooLarge
	case rctx.Err() != nil:
		r.Log.Info("the PDF reader was stopped at its deadline", "took", took)
		return Output{}, ErrTooSlow
	case err == nil:
		pages, read, title, ok := parseInfo(errb.String())
		if !ok || !utf8.Valid(out.buf.Bytes()) || read < 1 || read > pages {
			r.Log.Error("the PDF reader succeeded without saying how", "stderr", errb.String())
			return Output{}, ErrMalformed
		}
		return Output{Title: title, Pages: pages, PagesRead: read, Text: out.buf.String()}, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		// the process could not be started at all: the binary is gone or not executable
		r.Log.Error("the PDF reader could not be started", "path", r.Path, "err", err)
		return Output{}, ErrUnavailable
	}
	stderr := errb.String()
	word, detail, _ := strings.Cut(firstLine(stderr), ":")
	if reasonErr, known := reasonErrors[word]; known {
		r.Log.Info("the PDF reader refused the document", "reason", word, "detail", strings.TrimSpace(detail), "took", took)
		return Output{}, reasonErr
	}
	// no reason word: it died (a signal, an abort of the runtime at its memory limit, a crash)
	r.Log.Warn("the PDF reader died", "status", exit.String(), "took", took, "stderr", firstLine(stderr))
	return Output{}, ErrMalformed
}

// Check runs the command on a document built here, so that a missing, broken or unlimited reader is found when the
// service starts and not when a model asks for a PDF: the service then says reading PDFs is not available.
func (r *Runner) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := r.Read(ctx, selfTestPDF())
	if err != nil {
		return err
	}
	if !strings.Contains(out.Text, "[page 1]") || !strings.Contains(out.Text, selfTestWord) {
		return fmt.Errorf("the PDF reader returned %q for the test document", out.Text)
	}
	return nil
}

const selfTestWord = "readability"

func selfTestPDF() []byte {
	body := "BT /F1 12 Tf 50 700 Td (A short document for the reader's self test: reading " + selfTestWord + " of text.) Tj ET"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(body), body),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return b.Bytes()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// capped collects standard output up to max bytes; past that it calls over (which kills the process) and drops the
// rest, and exceeded says so.
type capped struct {
	max      int
	buf      bytes.Buffer
	exceeded bool
	over     func()
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		if !c.exceeded {
			c.exceeded = true
			c.over()
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// head keeps the first max bytes written to it.
type head struct {
	max int
	buf bytes.Buffer
}

func (h *head) Write(p []byte) (int, error) {
	if room := h.max - h.buf.Len(); room > 0 {
		h.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (h *head) String() string { return h.buf.String() }
