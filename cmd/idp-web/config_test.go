package main

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// files writes the files a complete configuration points at and returns its environment.
func files(t *testing.T, extra map[string]string) func(string) string {
	t.Helper()
	dir := t.TempDir()
	w := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	env := map[string]string{
		"IDP_WEB_REF_KEY_FILE":   w("ref.key", strings.Repeat("r", 32)+"\n"),
		"IDP_WEB_TENANTS_FILE":   w("tenants.yaml", "default: {enabled: true}\n"),
		"IDP_WEB_PROVIDER":       "fixture",
		"IDP_WEB_FIXTURE_FILE":   w("fixture.yaml", "entries: []\n"),
		"IDP_WEB_EXA_KEY_FILE":   w("exa.key", "exa-key\n"),
		"IDP_WEB_BRAVE_KEY_FILE": w("brave.key", "brave-key\n"),
	}
	for k, v := range extra {
		if v == "-" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	return func(k string) string { return env[k] }
}

var quiet = slog.New(slog.DiscardHandler)

func TestACompleteConfigurationLoadsForEachProviderAndIsReady(t *testing.T) {
	for _, p := range []string{"fixture", "exa", "brave"} {
		s, err := loadSetup(files(t, map[string]string{"IDP_WEB_PROVIDER": p}), quiet)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if m := s.missing(); len(m) != 0 {
			t.Errorf("%s: missing %v", p, m)
		}
		if s.limits.Searches != 30 || s.limits.Reads != 30 || s.limits.Window != 10*time.Minute {
			t.Errorf("%s: limits %+v", p, s.limits)
		}
	}
	w := httptest.NewRecorder()
	s, _ := loadSetup(files(t, nil), quiet)
	readyz(w, s)
	if w.Code != 200 {
		t.Errorf("/readyz %d %s", w.Code, w.Body)
	}
}

func TestEachMissingOrInvalidSettingIsAConfigurationError(t *testing.T) {
	for name, c := range map[string]struct {
		extra map[string]string
		want  string
	}{
		"no provider":                  {map[string]string{"IDP_WEB_PROVIDER": "-"}, "IDP_WEB_PROVIDER is not set"},
		"an unknown provider":          {map[string]string{"IDP_WEB_PROVIDER": "bing"}, "not fixture, exa or brave"},
		"exa without its key file":     {map[string]string{"IDP_WEB_PROVIDER": "exa", "IDP_WEB_EXA_KEY_FILE": "-"}, "IDP_WEB_EXA_KEY_FILE is not set"},
		"exa with a missing key file":  {map[string]string{"IDP_WEB_PROVIDER": "exa", "IDP_WEB_EXA_KEY_FILE": "/nonexistent/key"}, "IDP_WEB_EXA_KEY_FILE"},
		"exa with an http base":        {map[string]string{"IDP_WEB_PROVIDER": "exa", "IDP_WEB_EXA_BASE_URL": "http://api.exa.ai"}, "https"},
		"exa with an unknown type":     {map[string]string{"IDP_WEB_PROVIDER": "exa", "IDP_WEB_EXA_TYPE": "deep"}, "auto or lite"},
		"brave without its key file":   {map[string]string{"IDP_WEB_PROVIDER": "brave", "IDP_WEB_BRAVE_KEY_FILE": "-"}, "IDP_WEB_BRAVE_KEY_FILE is not set"},
		"fixture without its file":     {map[string]string{"IDP_WEB_FIXTURE_FILE": "-"}, "IDP_WEB_FIXTURE_FILE is not set"},
		"fixture with a missing file":  {map[string]string{"IDP_WEB_FIXTURE_FILE": "/nonexistent/f.yaml"}, "IDP_WEB_FIXTURE_FILE"},
		"no ref key file":              {map[string]string{"IDP_WEB_REF_KEY_FILE": "-"}, "IDP_WEB_REF_KEY_FILE is not set"},
		"a ref key that is too short":  {map[string]string{"IDP_WEB_REF_KEY_FILE": "/dev/null"}, "IDP_WEB_REF_KEY_FILE"},
		"no tenants file":              {map[string]string{"IDP_WEB_TENANTS_FILE": "-"}, "IDP_WEB_TENANTS_FILE is not set"},
		"a missing tenants file":       {map[string]string{"IDP_WEB_TENANTS_FILE": "/nonexistent/t.yaml"}, "IDP_WEB_TENANTS_FILE"},
		"a limit that is not a number": {map[string]string{"IDP_WEB_LIMIT_SEARCHES": "many"}, "IDP_WEB_LIMIT_SEARCHES"},
		"a limit of zero":              {map[string]string{"IDP_WEB_LIMIT_READS": "0"}, "IDP_WEB_LIMIT_READS"},
	} {
		_, err := loadSetup(files(t, c.extra), quiet)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a message with %q", name, err, c.want)
		}
	}
}

func TestAShortRefKeyAndABadTenantsFileAreRefusedByTheirContent(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(short, []byte("too short\n"), 0o600)
	os.WriteFile(bad, []byte("tenants: {a: {enabled: true, excludeDomain: [x.test]}}\n"), 0o600)
	if _, err := loadSetup(files(t, map[string]string{"IDP_WEB_REF_KEY_FILE": short}), quiet); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Errorf("%v", err)
	}
	if _, err := loadSetup(files(t, map[string]string{"IDP_WEB_TENANTS_FILE": bad}), quiet); err == nil || !strings.Contains(err.Error(), "excludeDomain") {
		t.Errorf("%v", err)
	}
}

func TestReadyzIsNotReadyUntilTheProviderTheRefKeyAndTheTenantsAreLoaded(t *testing.T) {
	w := httptest.NewRecorder()
	readyz(w, &setup{})
	if w.Code != 503 || !strings.Contains(w.Body.String(), "search provider") || !strings.Contains(w.Body.String(), "ref key") || !strings.Contains(w.Body.String(), "tenants file") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	full, _ := loadSetup(files(t, nil), quiet)
	for name, drop := range map[string]func(*setup){
		"provider": func(s *setup) { s.provider = nil },
		"keys":     func(s *setup) { s.keys = nil },
		"tenants":  func(s *setup) { s.tenants = nil },
	} {
		c := *full
		drop(&c)
		w := httptest.NewRecorder()
		readyz(w, &c)
		if w.Code != 503 {
			t.Errorf("ready without the %s", name)
		}
	}
}

func TestTheRefusedCounterIsWrittenByToolAndReason(t *testing.T) {
	m := &metrics{counts: map[[2]string]int64{}, nanos: map[string]int64{}, refused: map[[2]string]int64{}, failed: map[[2]string]int64{}}
	m.Refused("web_fetch", "ref_mismatch")
	m.Refused("web_fetch", "ref_mismatch")
	m.Refused("web_search", "rate_limited")
	w := httptest.NewRecorder()
	m.write(w)
	for _, want := range []string{`idp_web_refused_total{tool="web_fetch",reason="ref_mismatch"} 2`, `idp_web_refused_total{tool="web_search",reason="rate_limited"} 1`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s in\n%s", want, w.Body)
		}
	}
}

func TestTheFailedCounterIsWrittenByToolAndReasonSoThatNeedsScriptHasItsOwnLine(t *testing.T) {
	m := &metrics{counts: map[[2]string]int64{}, nanos: map[string]int64{}, refused: map[[2]string]int64{}, failed: map[[2]string]int64{}}
	m.Failed("web_fetch", "needs_script")
	m.Failed("web_fetch", "needs_script")
	m.Failed("web_fetch", "bot_check")
	w := httptest.NewRecorder()
	m.write(w)
	for _, want := range []string{`idp_web_failed_total{tool="web_fetch",reason="needs_script"} 2`, `idp_web_failed_total{tool="web_fetch",reason="bot_check"} 1`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s in\n%s", want, w.Body)
		}
	}
}
