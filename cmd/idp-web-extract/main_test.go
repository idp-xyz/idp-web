package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTheCommandWritesMarkdownAndOneStatusLine(t *testing.T) {
	var out, errw bytes.Buffer
	in := strings.NewReader("<html><head><title>T  x</title></head><body><h1>Head</h1><div>Body words.</div></body></html>")
	if code := run([]string{"-engine", "plain", "-url", "https://example.com/"}, in, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if out.String() != "# Head\nBody words." || errw.String() != "engine=plain title=T x\n" {
		t.Errorf("stdout %q, stderr %q", out.String(), errw.String())
	}
}

func TestAnEmptyTextIsStillExitStatusZero(t *testing.T) {
	var out, errw bytes.Buffer
	if code := run(nil, strings.NewReader("<html><body></body></html>"), &out, &errw); code != 0 || out.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errw.String())
	}
	if !strings.HasPrefix(errw.String(), "engine=") {
		t.Errorf("stderr %q", errw.String())
	}
}

func TestAnUnknownEngineOrFlagIsStatusTwo(t *testing.T) {
	for _, args := range [][]string{{"-engine", "magic"}, {"-nope"}} {
		var out, errw bytes.Buffer
		if code := run(args, strings.NewReader("<p>x</p>"), &out, &errw); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
