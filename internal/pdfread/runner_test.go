package pdfread

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"idp-web/internal/pdftest"
)

// The Runner is tested against this package's counterpart: the test binary of internal/pdfreader, built as the shipped
// command is (static, no race detector), which plays the command ("__command__") or a hostile one ("__hostile:<what>__",
// "__deaf__") when the Runner starts it with that first argument. See internal/pdfreader/child_test.go.
var childBinary string

func TestMain(m *testing.M) {
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

func testRunner(role string, extra ...string) *Runner {
	r := NewRunner(childBinary, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Args = append([]string{role}, extra...)
	return r
}

func TestTheRunnerReturnsTheTextAndTheSentencesOfTheCommand(t *testing.T) {
	r := testRunner("__command__")
	out, err := r.Read(context.Background(), pdftest.Titled("A title", "A page with enough words on it to count as text."))
	if err != nil || out.Title != "A title" || out.Pages != 1 || !strings.Contains(out.Text, "[page 1]") {
		t.Fatalf("%+v %v", out, err)
	}
	for _, c := range []struct {
		doc  []byte
		want error
	}{{pdftest.Graphics(1), ErrNoTextLayer}, {pdftest.Encrypted(), ErrEncrypted}, {pdftest.HTML(), ErrMalformed}, {pdftest.Bomb(256), ErrTooLarge}} {
		if _, err := r.Read(context.Background(), c.doc); !errors.Is(err, c.want) {
			t.Errorf("%v, want %v", err, c.want)
		}
	}
}

func TestACommandThatIgnoresItsDeadlineIsKilledWithItsProcessGroup(t *testing.T) {
	pidFile := t.TempDir() + "/pid"
	r := testRunner("__deaf__", pidFile)
	r.Deadline = 500 * time.Millisecond
	start := time.Now()
	_, err := r.Read(context.Background(), pdftest.Text("x"))
	took := time.Since(start)
	if !errors.Is(err, ErrTooSlow) || took > 4*time.Second {
		t.Fatalf("%v after %s", err, took)
	}
	b, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, _ := strconv.Atoi(string(b))
	deadline := time.Now().Add(2 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the process the command started (%d) is still running after the kill", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alive: a process that is gone, or is a zombie waiting for a parent that does not reap (a container's init may not).
func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	return !strings.HasPrefix(s[strings.LastIndex(s, ")")+2:], "Z")
}

func TestTheDeadlineOfACallIsTheRemainingTimeOfTheCall(t *testing.T) {
	r := testRunner("__deaf__", t.TempDir()+"/pid")
	r.Deadline = 6 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Read(ctx, pdftest.Text("x"))
	if !errors.Is(err, ErrTooSlow) || time.Since(start) > 2500*time.Millisecond {
		t.Errorf("%v after %s", err, time.Since(start))
	}
	// less than half a second left: the reader is not even started
	ctx2, cancel2 := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel2()
	start = time.Now()
	if _, err := r.Read(ctx2, pdftest.Text("x")); !errors.Is(err, ErrTooSlow) || time.Since(start) > 200*time.Millisecond {
		t.Errorf("%v after %s", err, time.Since(start))
	}
}

func TestAProcessThatWritesWithoutEndIsKilledAtTheOutputBound(t *testing.T) {
	r := testRunner("__hostile:flood__")
	r.MaxOutput = 1 << 20
	r.Deadline = 5 * time.Second
	start := time.Now()
	_, err := r.Read(context.Background(), pdftest.Text("x"))
	if !errors.Is(err, ErrTooLarge) || time.Since(start) > 3*time.Second {
		t.Errorf("%v after %s", err, time.Since(start))
	}
}

func TestACrashOfTheParserCostsOneCallAndTheNextOneWorks(t *testing.T) {
	r := testRunner("__hostile:panic__")
	if _, err := r.Read(context.Background(), pdftest.Text("x")); !errors.Is(err, ErrMalformed) {
		t.Errorf("%v", err)
	}
	r = testRunner("__hostile:silent__")
	if _, err := r.Read(context.Background(), pdftest.Text("x")); !errors.Is(err, ErrMalformed) {
		t.Errorf("a reader that says nothing about how it read: %v", err)
	}
	ok := testRunner("__command__")
	if _, err := ok.Read(context.Background(), pdftest.Text("A page with enough words on it to count as text.")); err != nil {
		t.Errorf("%v", err)
	}
}

func TestWhenAllTheSlotsAreTakenACallWaitsAndThenSaysBusy(t *testing.T) {
	r := NewRunner(childBinary, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Args = []string{"__deaf__", t.TempDir() + "/pid"}
	r.Deadline = 2 * time.Second
	r.Wait = 300 * time.Millisecond
	done := make(chan error, 1)
	go func() { _, err := r.Read(context.Background(), pdftest.Text("x")); done <- err }()
	time.Sleep(300 * time.Millisecond) // the first call has its slot
	start := time.Now()
	_, err := r.Read(context.Background(), pdftest.Text("x"))
	if !errors.Is(err, ErrBusy) || time.Since(start) < 250*time.Millisecond || time.Since(start) > time.Second {
		t.Errorf("%v after %s", err, time.Since(start))
	}
	if err := <-done; !errors.Is(err, ErrTooSlow) {
		t.Errorf("the first call: %v", err)
	}
	// the slot is free again
	r2 := NewRunner(childBinary, 1, nil)
	r2.Args = []string{"__command__"}
	for i := 0; i < 2; i++ {
		if _, err := r2.Read(context.Background(), pdftest.Text("A page with enough words on it to count as text.")); err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
}

func TestACallThatIsCancelledReturnsTheCancellationAndFreesItsSlot(t *testing.T) {
	r := NewRunner(childBinary, 1, nil)
	r.Args = []string{"__deaf__", t.TempDir() + "/pid"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	if _, err := r.Read(ctx, pdftest.Text("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
	r.Args = []string{"__command__"}
	if _, err := r.Read(context.Background(), pdftest.Text("A page with enough words on it to count as text.")); err != nil {
		t.Errorf("after the cancelled call: %v", err)
	}
}

func TestAMissingCommandIsFoundAtStartAndSaysUnavailable(t *testing.T) {
	if _, err := FindCommand("/nonexistent/idp-web-pdf"); err == nil {
		t.Error("a path that does not exist was accepted")
	}
	dir := t.TempDir()
	if _, err := FindCommand(dir); err == nil {
		t.Error("a directory was accepted")
	}
	plain := dir + "/not-executable"
	os.WriteFile(plain, []byte("x"), 0o644)
	if _, err := FindCommand(plain); err == nil {
		t.Error("a file that cannot be executed was accepted")
	}
	r := NewRunner("/nonexistent/idp-web-pdf", 1, nil)
	if _, err := r.Read(context.Background(), pdftest.Text("x")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("%v", err)
	}
	if err := r.Check(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("the self test: %v", err)
	}
	if err := testRunner("__command__").Check(context.Background()); err != nil {
		t.Errorf("the self test of a working command: %v", err)
	}
	if err := testRunner("__hostile:silent__").Check(context.Background()); err == nil {
		t.Error("a command that does not read passed the self test")
	}
	var zero Runner
	if _, err := zero.Read(context.Background(), nil); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a Runner that was not made by NewRunner: %v", err)
	}
}

func TestTheProcessGetsNoEnvironmentFromTheService(t *testing.T) {
	t.Setenv("IDP_WEB_SECRET_FOR_THE_TEST", "do-not-pass")
	r := testRunner("__hostile:files__")
	out, err := r.Read(context.Background(), pdftest.Text("x"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Text, "env=0 wd=/ ") {
		t.Errorf("%q", out.Text)
	}
}

func TestAProcessAbortedAtItsMemoryLimitCostsOneCall(t *testing.T) {
	r := testRunner("__hostile:alloc__")
	r.MemoryMiB = 160
	if _, err := r.Read(context.Background(), pdftest.Text("x")); !errors.Is(err, ErrMalformed) {
		t.Errorf("%v", err)
	}
}
