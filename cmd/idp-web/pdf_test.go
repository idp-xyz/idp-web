package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"idp-web/internal/pdftest"
)

var (
	buildOnce sync.Once
	builtPath string
	builtErr  error
)

// pdfCommand builds cmd/idp-web-pdf as the image does (static, no cgo), once for all the tests of this package.
func pdfCommand(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "idp-web-pdf")
		if err != nil {
			builtErr = err
			return
		}
		builtPath = filepath.Join(dir, "idp-web-pdf")
		cmd := exec.Command("go", "build", "-o", builtPath, "idp-web/cmd/idp-web-pdf")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			builtErr = &buildError{string(out)}
		}
	})
	if builtErr != nil {
		t.Fatal(builtErr)
	}
	return builtPath
}

type buildError struct{ out string }

func (e *buildError) Error() string { return "building idp-web-pdf: " + e.out }

func settings(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func logs() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewTextHandler(&b, nil)), &b
}

func TestThePDFReaderStartsWhenItsCommandIsThereAndWorks(t *testing.T) {
	log, out := logs()
	r, err := startPDFReader(settings("IDP_WEB_PDF_READER", pdfCommand(t)), log)
	if err != nil || r == nil {
		t.Fatalf("%v %v %s", r, err, out)
	}
	got, err := r.Read(context.Background(), pdftest.Titled("Rate guide", "The fuel surcharge for zone 3 is 12.5 percent of the base rate."))
	if err != nil || got.Title != "Rate guide" || !strings.Contains(got.Text, "fuel surcharge") {
		t.Errorf("%+v %v", got, err)
	}
}

func TestWithoutItsCommandThePDFReaderIsUnavailableAndSaysWhyInTheLog(t *testing.T) {
	log, out := logs()
	r, err := startPDFReader(settings("IDP_WEB_PDF_READER", filepath.Join(t.TempDir(), "idp-web-pdf")), log)
	if err != nil || r != nil {
		t.Fatalf("%v %v", r, err)
	}
	if !strings.Contains(out.String(), "reading PDFs is not available") || !strings.Contains(out.String(), "not found") {
		t.Errorf("%s", out)
	}
	// next to the executable is where the image has it; the test binary has no such neighbour
	if r, err := startPDFReader(settings(), log); err != nil || r != nil {
		t.Errorf("%v %v", r, err)
	}
}

func TestACommandThatCannotReadFailsTheSelfTestAndTheReaderIsOff(t *testing.T) {
	// a program that starts and succeeds and knows nothing of PDFs
	log, out := logs()
	r, err := startPDFReader(settings("IDP_WEB_PDF_READER", "/bin/true"), log)
	if err != nil || r != nil || !strings.Contains(out.String(), "self test") {
		t.Errorf("%v %v %s", r, err, out)
	}
}

func TestPDFReadingCanBeSwitchedOffAndAWrongSettingIsAnError(t *testing.T) {
	log, _ := logs()
	if r, err := startPDFReader(settings("IDP_WEB_PDF", "off", "IDP_WEB_PDF_READER", pdfCommand(t)), log); err != nil || r != nil {
		t.Errorf("off: %v %v", r, err)
	}
	if r, err := startPDFReader(settings("IDP_WEB_PDF", "maybe"), log); err == nil || r != nil {
		t.Errorf("maybe: %v %v", r, err)
	}
	if _, err := startPDFReader(settings("IDP_WEB_PDF_CONCURRENCY", "0", "IDP_WEB_PDF_READER", pdfCommand(t)), log); err == nil {
		t.Error("a concurrency of 0 was accepted")
	}
}
