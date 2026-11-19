package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
)

func TestTestOriginsAreParsedIntoTheFormTheReaderComparesWith(t *testing.T) {
	got, err := parseTestOrigins(" 127.0.0.1:8080, Localhost:9000 ,[::1]:7000,[0:0:0:0:0:0:0:2]:7001,,")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"127.0.0.1:8080": true, "localhost:9000": true, "[::1]:7000": true, "[::2]:7001": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
	if got, err := parseTestOrigins(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
}

func TestATestOriginThatCouldExemptMoreThanWasMeantIsAnError(t *testing.T) {
	for _, s := range []string{"127.0.0.1", ":8080", "*.example.com:80", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:x", "fe80::1%eth0:80", "http://127.0.0.1:80", "a:80,b"} {
		if got, err := parseTestOrigins(s); err == nil {
			t.Errorf("%q was accepted as %v", s, got)
		}
	}
}

func TestCheckTLSNeedsAnHTTPSAddressAndReportsAFailureWithoutAnInternalDetail(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	var out, errw bytes.Buffer
	if code := checkTLS(context.Background(), []string{"http://example.org/"}, &out, &errw, quiet); code != 2 {
		t.Errorf("exit %d", code)
	}
	out.Reset()
	if code := checkTLS(context.Background(), []string{"https://127.0.0.1/"}, &out, &errw, quiet); code != 1 || out.String() == "" {
		t.Errorf("exit %d, output %q", code, out.String())
	}
}
