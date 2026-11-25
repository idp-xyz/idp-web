package main

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"idp-web/internal/browsertest"
	"idp-web/internal/tenants"
)

func renderSetup(t *testing.T, env map[string]string, tenantsFile string) (func(string) string, *tenants.Set, *bytes.Buffer) {
	t.Helper()
	ts, err := tenants.Parse([]byte(tenantsFile))
	if err != nil {
		t.Fatal(err)
	}
	return func(k string) string { return env[k] }, ts, &bytes.Buffer{}
}

func TestRenderingIsOffWithoutTheURLAndSaysSoIgnoringTheOtherSettingsWithAWarning(t *testing.T) {
	get, ts, buf := renderSetup(t, map[string]string{"IDP_WEB_RENDER_KEY_FILE": "/nonexistent", "IDP_WEB_RENDER_TIMEOUT_MS": "nonsense"}, "tenants:\n  a: {enabled: true, alwaysRender: [x.test]}\n")
	r, err := startRenderer(get, ts, slog.New(slog.NewTextHandler(buf, nil)))
	if err != nil || r != nil {
		t.Fatalf("%v %v", r, err)
	}
	for _, want := range []string{"rendering is off", "IDP_WEB_RENDER_KEY_FILE is set without IDP_WEB_RENDER_URL", "alwaysRender is set in the tenants file but no renderer"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the log lacks %q: %s", want, buf)
		}
	}
}

func TestAURLWithoutAUsableKeyOrWithAnUnusableSettingIsAStartupError(t *testing.T) {
	priv, _ := browsertest.GenerateKey(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "k.pem")
	os.WriteFile(good, priv, 0o600)
	junk := filepath.Join(dir, "junk.pem")
	os.WriteFile(junk, []byte("not a key"), 0o600)
	const u = "http://idp-browser.idp-browser.svc.cluster.local:8590/render"
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no key", map[string]string{"IDP_WEB_RENDER_URL": u}, "IDP_WEB_RENDER_KEY_FILE is not set"},
		{"missing key file", map[string]string{"IDP_WEB_RENDER_URL": u, "IDP_WEB_RENDER_KEY_FILE": filepath.Join(dir, "none")}, "IDP_WEB_RENDER_KEY_FILE"},
		{"junk key", map[string]string{"IDP_WEB_RENDER_URL": u, "IDP_WEB_RENDER_KEY_FILE": junk}, "IDP_WEB_RENDER_KEY_FILE"},
		{"bad URL", map[string]string{"IDP_WEB_RENDER_URL": "idp-browser:8590", "IDP_WEB_RENDER_KEY_FILE": good}, "IDP_WEB_RENDER_URL"},
		{"URL with a query", map[string]string{"IDP_WEB_RENDER_URL": u + "?x=1", "IDP_WEB_RENDER_KEY_FILE": good}, "IDP_WEB_RENDER_URL"},
		{"bad timeout", map[string]string{"IDP_WEB_RENDER_URL": u, "IDP_WEB_RENDER_KEY_FILE": good, "IDP_WEB_RENDER_TIMEOUT_MS": "0"}, "IDP_WEB_RENDER_TIMEOUT_MS"},
		{"too large a page", map[string]string{"IDP_WEB_RENDER_URL": u, "IDP_WEB_RENDER_KEY_FILE": good, "IDP_WEB_RENDER_MAX_BYTES": "99999999999"}, "IDP_WEB_RENDER"},
	} {
		get, ts, buf := renderSetup(t, tc.env, "default: {enabled: true}\n")
		if _, err := startRenderer(get, ts, slog.New(slog.NewTextHandler(buf, nil))); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	get, ts, buf := renderSetup(t, map[string]string{"IDP_WEB_RENDER_URL": u, "IDP_WEB_RENDER_KEY_FILE": good}, "tenants:\n  a: {enabled: true, alwaysRender: [x.test]}\n")
	r, err := startRenderer(get, ts, slog.New(slog.NewTextHandler(buf, nil)))
	if err != nil || r == nil {
		t.Fatalf("%v %v", r, err)
	}
	for _, want := range []string{"rendering pages that need script", "timeoutMs=15000", "maxBytes=5242880", "keyId=", "alwaysRender=[a]"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the start line lacks %q: %s", want, buf)
		}
	}
}

func TestRendersAreCountedByOutcomeInTheMetrics(t *testing.T) {
	m := &metrics{counts: map[[2]string]int64{}, nanos: map[string]int64{}, refused: map[[2]string]int64{}, failed: map[[2]string]int64{}}
	m.Rendered("ok", 2e9)
	m.Rendered("ok", 1e9)
	m.Rendered("render_busy", 0)
	w := httptest.NewRecorder()
	m.write(w)
	for _, want := range []string{`idp_web_renders_total{outcome="ok"} 2`, `idp_web_renders_total{outcome="render_busy"} 1`, "idp_web_render_seconds_total 3"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("no %q in\n%s", want, w.Body)
		}
	}
}
