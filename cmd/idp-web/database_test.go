package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"idp-web/internal/pgtest"
)

// withTenants is a complete configuration whose tenants file is the given text.
func withTenants(t *testing.T, tenantsYAML string, extra map[string]string) func(string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tenants.yaml")
	if err := os.WriteFile(p, []byte(tenantsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if extra == nil {
		extra = map[string]string{}
	}
	extra["IDP_WEB_TENANTS_FILE"] = p
	return files(t, extra)
}

const limited = "default: {enabled: false}\ntenants:\n  acme: {enabled: true, dailySearches: 50}\n"

// A limit that cannot be counted is no limit: without a database the service does not start with one in its tenants file, and
// says which tenant and what to set. A file with no limits starts without a database, as before.
func TestAnyDailyLimitWithoutADatabaseIsAConfigurationError(t *testing.T) {
	_, err := loadSetup(withTenants(t, limited, nil), quiet)
	if err == nil || !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "dailySearches") || !strings.Contains(err.Error(), "IDP_WEB_DATABASE_URL") {
		t.Fatalf("%v", err)
	}
	_, err = loadSetup(withTenants(t, "default: {enabled: true, dailyReads: 0}\n", nil), quiet)
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("a limit of 0 in the default: %v", err)
	}
	s, err := loadSetup(withTenants(t, "default: {enabled: true}\ntenants:\n  acme: {enabled: true}\n", nil), quiet)
	if err != nil || s.store != nil || len(s.missing()) != 0 || s.unreachable() {
		t.Fatalf("a deployment with no limits and no database: %v", err)
	}
}

func TestTheDatabaseSettingsAreCheckedBeforeTheServiceStarts(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, nil, 0o600)
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"both the URL and its file":    {map[string]string{"IDP_WEB_DATABASE_URL": "postgres://x", "IDP_WEB_DATABASE_URL_FILE": empty}, "both set"},
		"a file that is missing":       {map[string]string{"IDP_WEB_DATABASE_URL_FILE": "/nonexistent/url"}, "IDP_WEB_DATABASE_URL_FILE"},
		"an empty file":                {map[string]string{"IDP_WEB_DATABASE_URL_FILE": empty}, "empty"},
		"a URL that is not one":        {map[string]string{"IDP_WEB_DATABASE_URL": "not a url ://"}, "IDP_WEB_DATABASE_URL"},
		"a database that is not there": {map[string]string{"IDP_WEB_DATABASE_URL": "postgres://u:p@127.0.0.1:1/none?connect_timeout=1"}, "IDP_WEB_DATABASE_URL"},
		"a timeout of zero":            {map[string]string{"IDP_WEB_DATABASE_URL": "postgres://u:p@127.0.0.1:1/none", "IDP_WEB_DATABASE_TIMEOUT_MS": "0"}, "IDP_WEB_DATABASE_TIMEOUT_MS"},
	} {
		_, err := loadSetup(files(t, c.env), quiet)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "postgres://u:p") {
			t.Errorf("%s: the error repeats the URL, which holds a password: %v", name, err)
		}
	}
}

// With a database, a tenant with a limit is accepted, the URL may come from a file (a mounted Secret), and /readyz reports the
// database: ready while it answers, not ready, and saying why, when it does not.
func TestReadyzReportsTheDatabaseAndTheURLMayComeFromAFile(t *testing.T) {
	_, url := pgtest.Scratch(t)
	file := filepath.Join(t.TempDir(), "database-url")
	os.WriteFile(file, []byte(url+"\n"), 0o600)
	s, err := loadSetup(withTenants(t, limited, map[string]string{"IDP_WEB_DATABASE_URL_FILE": file, "IDP_WEB_DATABASE_TIMEOUT_MS": "500"}), quiet)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	readyz(w, s)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	s.store.Close()
	w = httptest.NewRecorder()
	readyz(w, s)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "database not reachable") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	// and a configuration whose provider is missing says that as well as the database
	c := *s
	c.provider = nil
	w = httptest.NewRecorder()
	readyz(w, &c)
	if !strings.Contains(w.Body.String(), "search provider not loaded; database not reachable") {
		t.Errorf("%s", w.Body)
	}
}

// migrate and ensure-login, as the siblings have them: the owner's URL, then the role the service runs as.
func TestMigrateAndEnsureLoginPrepareTheDatabaseAndSayWhenTheyCannot(t *testing.T) {
	owner, _ := pgtest.Scratch(t) // migrated already: the second run applies nothing
	env := map[string]string{"IDP_WEB_MIGRATION_DATABASE_URL": owner, "IDP_WEB_APP_DB_PASSWORD": "another-password"}
	get := func(k string) string { return env[k] }
	defer pgtest.RoleLock(t)() // the role of ensure-login is the cluster's, and other packages' tests create theirs at the same time
	var out, errs bytes.Buffer
	if code := database(context.Background(), []string{"migrate"}, get, &out, &errs); code != 0 || !strings.Contains(out.String(), "OK: 0 migration(s) applied") {
		t.Fatalf("%d %s %s", code, out.String(), errs.String())
	}
	out.Reset()
	if code := database(context.Background(), []string{"ensure-login", "idp_web_cmd_svc"}, get, &out, &errs); code != 0 || !strings.Contains(out.String(), "idp_web_cmd_svc is a member of idp_web_app") {
		t.Fatalf("%d %s %s", code, out.String(), errs.String())
	}
	for name, c := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no owner URL":                {[]string{"migrate"}, nil, "IDP_WEB_MIGRATION_DATABASE_URL is not set"},
		"no user":                     {[]string{"ensure-login"}, env, "usage: ensure-login USER"},
		"no password":                 {[]string{"ensure-login", "u"}, map[string]string{"IDP_WEB_MIGRATION_DATABASE_URL": owner}, "IDP_WEB_APP_DB_PASSWORD is not set"},
		"a role name that is not one": {[]string{"ensure-login", "u; drop"}, env, "invalid role name"},
	} {
		errs.Reset()
		if code := database(context.Background(), c.args, func(k string) string { return c.env[k] }, &out, &errs); code != 1 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d %s", name, code, errs.String())
		}
	}
}
