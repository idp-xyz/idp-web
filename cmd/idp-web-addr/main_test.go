package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestOneAnswerPerLineAndNoNameIsResolved(t *testing.T) {
	in := "http://example.com/\nhttp://127.0.0.1/\nhttp://2130706433/\n\nhttp://93.184.216.34/\nhttp://localhost/\nhttp://[::1]/\nfile:///etc/passwd\nhttp://a\x00b/\n"
	var out bytes.Buffer
	if code := run(strings.NewReader(in), &out); code != 0 {
		t.Fatal(code)
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []string{"allow", "refuse 127.0.0.1 is not a public address", "refuse an IP address must be written as four decimal numbers, like 93.184.216.34", `refuse the scheme "" is not http or https`, "allow",
		"allow", // a name: whether it resolves to something public is not judged without a network
		"refuse ::1 is not a public address", `refuse the scheme "file" is not http or https`, "refuse the address is not valid"}
	if len(got) != len(want) {
		t.Fatalf("%d lines: %q", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: %q, want %q", i+1, got[i], want[i])
		}
	}
}
