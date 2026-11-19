package pdfreader

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"idp-web/internal/pdftest"
)

// The tests of the process limits start this test binary again as the command: the first argument names the part it
// plays. "__command__" is the real ChildMain; "__hostile:<what>__" is ChildMain with the document reader replaced by
// one that misbehaves after the limits are applied, which is the situation the limits are for. The limits are not
// the Go runtime's: each test shows that the process is stopped by the kernel or by the Runner, and removing the
// control makes the test fail (ADR-0001 D31's table).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "__") {
		os.Exit(playCommand(os.Args[1], os.Args[2:]))
	}
	// The processes that play the command are this package's tests built again as the shipped command is: static (no
	// cgo, whose threads the memory limit would count) and without the race detector (whose shadow memory, terabytes
	// of address space, no limit allows). The same code, the way idp-web-pdf is built.
	dir, err := os.MkdirTemp("", "pdfread")
	if err != nil {
		panic(err)
	}
	childBinary = dir + "/pdfreader.test"
	cmd := exec.Command("go", "test", "-c", "-o", childBinary, "idp-web/internal/pdfreader")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// childBinary is the test binary that plays the command: this one, or the same package built without the race detector.
var childBinary string

func playCommand(role string, args []string) int {
	switch role {
	case "__command__":
		return ChildMain(args, os.Stdin, os.Stdout, os.Stderr, os.Exit)
	case "__unchecked__":
		// standard output is a file that the command cannot see is one (a writer that hides Stat): the check at the start
		// does not stop it, so the write meets the limit
		return ChildMain(args, os.Stdin, onlyWrite{os.Stdout}, os.Stderr, os.Exit)
	case "__noprefilght__":
		skipPreflight = true
		return ChildMain(args, os.Stdin, os.Stdout, os.Stderr, os.Exit)
	case "__deaf__":
		// ignores its deadline and has a child of its own: what the kill of the whole process group is for
		child := exec.Command(os.Args[0], "__sleep__")
		child.Start()
		os.WriteFile(args[0], []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		time.Sleep(time.Hour)
		return 0
	case "__sleep__":
		time.Sleep(time.Hour)
		return 0
	}
	what := strings.TrimSuffix(strings.TrimPrefix(role, "__hostile:"), "__")
	readDoc = func([]byte, Limits) (Result, error) {
		switch what {
		case "spin":
			for i := 0; ; i++ {
				_ = i
			}
		case "alloc":
			var keep [][]byte
			for {
				b := make([]byte, 16<<20)
				for i := 0; i < len(b); i += 4096 {
					b[i] = 1
				}
				keep = append(keep, b)
				fmt.Fprintf(os.Stderr, "held %d MiB\n", 16*len(keep))
				if len(keep) > 64 { // 1 GiB: a process without its limit stops here, so that no test can eat the machine
					return Result{}, errors.New("nothing stopped the allocation")
				}
			}
		case "flood":
			chunk := bytes.Repeat([]byte("x"), 64<<10)
			for {
				if _, err := os.Stdout.Write(chunk); err != nil {
					return Result{}, err
				}
			}
		case "panic":
			panic("the parser fell over")
		case "silent":
			return Result{}, nil // exit 0 with nothing to say how
		case "net":
			var report []string
			if c, err := net.DialTimeout("tcp", "127.0.0.1:9", time.Second); err == nil {
				c.Close()
				report = append(report, "connected")
			} else {
				report = append(report, "dial: "+err.Error())
			}
			if err := exec.Command("/bin/true").Run(); err == nil {
				report = append(report, "started a program")
			} else {
				report = append(report, "exec: "+err.Error())
			}
			return Result{Pages: 1, PagesRead: 1, Text: strings.Join(report, "\n")}, nil
		case "files":
			entries, _ := os.ReadDir("/proc/self/fd")
			var fds []string
			for _, e := range entries {
				target, _ := os.Readlink("/proc/self/fd/" + e.Name())
				fds = append(fds, e.Name()+"="+target)
			}
			wd, _ := os.Getwd()
			return Result{Pages: 1, PagesRead: 1, Text: fmt.Sprintf("env=%d wd=%s fds=%s", len(os.Environ()), wd, strings.Join(fds, ","))}, nil
		}
		return Result{}, errors.New("unknown")
	}
	return ChildMain(args, os.Stdin, os.Stdout, os.Stderr, os.Exit)
}

func runChild(role string, stdin []byte, flags ...string) (stdout, stderr string, exit string, mib int64) {
	cmd := exec.Command(childBinary, append([]string{role}, flags...)...)
	cmd.Env = []string{}
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(stdin), &out, &errb
	err := cmd.Run()
	if err != nil {
		exit = err.Error()
	}
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		mib = int64(ru.Maxrss) / 1024
	}
	return out.String(), errb.String(), exit, mib
}

func TestTheCommandSpeaksTheProtocolOfTheEvaluation(t *testing.T) {
	out, errs, exit, _ := runChild("__command__", pdftest.Titled("A title", "A page with enough words on it to count as text."))
	if exit != "" || !strings.HasPrefix(out, "[page 1]\n") || !strings.Contains(errs, `pdf pages=1 read=1 title="A title"`) {
		t.Errorf("exit %q stdout %q stderr %q", exit, out, errs)
	}
	for _, c := range []struct {
		name string
		doc  []byte
		word string
	}{
		{"scan", pdftest.Graphics(2), "no_text_layer"},
		{"password", pdftest.Encrypted(), "encrypted"},
		{"damaged", pdftest.HTML(), "malformed"},
		{"bomb", pdftest.Bomb(256), "too_large"},
	} {
		out, errs, exit, _ := runChild("__command__", c.doc)
		if exit == "" || out != "" || !strings.HasPrefix(errs, c.word+":") {
			t.Errorf("%s: exit %q stdout %q stderr %q", c.name, exit, out, errs)
		}
	}
}

func TestTheCommandEndsItselfAtItsDeadline(t *testing.T) {
	start := time.Now()
	_, errs, exit, _ := runChild("__hostile:spin__", pdftest.Text("x"), "-deadline", "400ms")
	if took := time.Since(start); took > 1500*time.Millisecond || !strings.HasPrefix(errs, "too_slow:") || exit == "" {
		t.Errorf("took %s, exit %q, stderr %q", took, exit, errs)
	}
}

func TestTheMemoryLimitStopsAProcessThatAllocatesWithoutEnd(t *testing.T) {
	_, errs, exit, mib := runChild("__hostile:alloc__", pdftest.Text("x"), "-memory-mib", "160")
	var held int
	for _, l := range strings.Split(errs, "\n") {
		fmt.Sscanf(l, "held %d MiB", &held)
	}
	if exit == "" || held < 32 || held >= 160 || !strings.Contains(errs, "fatal error:") {
		t.Errorf("held %d MiB, exit %q stderr %.300q", held, exit, errs)
	}
	if mib > 160+32 {
		t.Errorf("peak %d MiB with a limit of 160", mib)
	}
}

func TestTheMemoryLimitStopsTheBombThatThePreflightWouldHaveRefused(t *testing.T) {
	// the preflight is the first line; with it off (__noprefilght__) the parser inflates, and the kernel's limit is
	// what is left: the process aborts before 256 MiB, which is what the evaluation allows a document
	_, errs, exit, mib := runChild("__noprefilght__", pdftest.Bomb(256))
	if exit == "" || strings.HasPrefix(errs, "no_text_layer") {
		t.Errorf("the bomb was read to the end: exit %q stderr %.200q", exit, errs)
	}
	if mib > 256 {
		t.Errorf("peak %d MiB", mib)
	}
}

func TestTheCommandRefusesToRunWithoutItsLimits(t *testing.T) {
	// bad settings are refused before anything is read; and where the limits cannot be applied the command does not run
	_, errs, exit, _ := runChild("__command__", pdftest.Text("x"), "-memory-mib", "8")
	if exit == "" || !strings.HasPrefix(errs, "unavailable:") {
		t.Errorf("%q %q", exit, errs)
	}
}

func TestTheProcessCannotConnectOrStartAProgram(t *testing.T) {
	out, errs, exit, _ := runChild("__hostile:net__", pdftest.Text("x"))
	if exit != "" {
		t.Fatalf("%s %s", exit, errs)
	}
	if strings.Contains(out, "connected") || strings.Contains(out, "started a program") ||
		!strings.Contains(out, "dial: ") || !strings.Contains(out, "exec: ") || strings.Count(out, "operation not permitted") != 2 {
		t.Errorf("%q", out)
	}
}

func TestTheProcessKeepsNoFileOfItsParentAndIsInTheRootDirectory(t *testing.T) {
	// a file the parent hands down on purpose, as a library's descriptor that was not close-on-exec would be: the
	// command closes everything above the three standard ones before it does anything else
	f, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command(childBinary, "__hostile:files__")
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{f, f, f} // descriptors 3, 4 and 5; the ones the Go runtime opens itself point elsewhere
	var out bytes.Buffer
	cmd.Stdin, cmd.Stdout = bytes.NewReader(pdftest.Text("x")), &out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "wd=/ ") {
		t.Errorf("%q", out.String())
	}
	for _, fd := range strings.Split(out.String()[strings.Index(out.String(), "fds=")+4:], ",") {
		if !strings.HasPrefix(fd, "0=") && !strings.HasPrefix(fd, "1=") && !strings.HasPrefix(fd, "2=") && strings.HasSuffix(fd, "/dev/null") {
			t.Errorf("a descriptor of the parent is open in the command: %q", out.String())
		}
	}
}

// onlyWrite is a writer that is not recognised as a file.
type onlyWrite struct{ w io.Writer }

func (o onlyWrite) Write(p []byte) (int, error) { return o.w.Write(p) }

// runChildOn starts the command with the given standard output and error, which the test chooses.
func runChildOn(t *testing.T, role string, stdin []byte, stdout, stderr *os.File) (exit int, signaled bool) {
	t.Helper()
	cmd := exec.Command(childBinary, role)
	cmd.Env = []string{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(stdin), stdout, stderr
	err := cmd.Run()
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if err != nil && !ws.Exited() {
		return -1, ws.Signaled()
	}
	return ws.ExitStatus(), false
}

// The limit on the size of a file is a control (the reader must not be able to write one), so output to a file is not
// made to work: it is refused with a message that says what to do, instead of a silent exit 1 (ADR-0001 D31).
func TestOutputToAFileIsRefusedWithAMessageThatSaysToPipeIt(t *testing.T) {
	doc := pdftest.Titled("A title", "A page with enough words on it to count as text.")
	dir := t.TempDir()
	outFile, err := os.Create(dir + "/out.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	var errb bytes.Buffer
	cmd := exec.Command(childBinary, "__command__")
	cmd.Env = []string{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(doc), outFile, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("exit %v, stderr %q", err, errb.String())
	}
	msg := errb.String()
	if !strings.HasPrefix(msg, "unavailable: standard output is a file") || !strings.Contains(msg, "pipe the output") || !strings.Contains(msg, "on purpose") || strings.Count(msg, "\n") != 1 {
		t.Errorf("stderr %q", msg)
	}
	if fi, _ := os.Stat(dir + "/out.txt"); fi.Size() != 0 {
		t.Errorf("%d bytes were written to the file", fi.Size())
	}
	// standard error in a file: the message is what the file holds, and nothing else
	errFile, err := os.Create(dir + "/err.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer errFile.Close()
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	code, _ := runChildOn(t, "__command__", doc, pw, errFile)
	pw.Close()
	if code != 2 {
		t.Errorf("exit %d", code)
	}
	if b, _ := os.ReadFile(dir + "/err.txt"); !strings.HasPrefix(string(b), "unavailable: standard error is a file") || strings.Count(string(b), "\n") != 1 {
		t.Errorf("the file holds %q", b)
	}
	if b, _ := io.ReadAll(pr); len(b) != 0 {
		t.Errorf("standard output has %q", b)
	}
}

func TestOutputToAPipeIsReadAsBefore(t *testing.T) {
	out, errs, exit, _ := runChild("__command__", pdftest.Titled("A title", "A page with enough words on it to count as text."))
	if exit != "" || !strings.HasPrefix(out, "[page 1]\n") || !strings.Contains(errs, "pdf pages=1") || strings.Contains(errs, "unavailable") {
		t.Errorf("exit %q stdout %q stderr %q", exit, out, errs)
	}
}

// A write to a file that got past the check meets the limit: the process is not killed by SIGXFSZ and does not blame the
// document, it says what happened.
func TestAWriteToAFileThatMeetsTheLimitIsReportedAsTheReaderNotTheDocument(t *testing.T) {
	outFile, err := os.Create(t.TempDir() + "/out.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	code, signaled := runChildOn(t, "__unchecked__", pdftest.Titled("A title", "A page with enough words on it to count as text."), outFile, pw)
	pw.Close()
	b, _ := io.ReadAll(pr)
	if signaled || code != 2 || !strings.HasPrefix(string(b), "unavailable: standard output is a file") || strings.Contains(string(b), "malformed") {
		t.Errorf("exit %d (killed by a signal: %v), stderr %q", code, signaled, b)
	}
	if fi, _ := outFile.Stat(); fi.Size() != 0 {
		t.Errorf("%d bytes were written", fi.Size())
	}
}
