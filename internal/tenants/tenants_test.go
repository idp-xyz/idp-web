package tenants

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const file = `
default: {enabled: false}
tenants:
  acme:
    enabled: true
    includeDomains: [Example.COM, gov.cn.]
    excludeDomains: [private.example.com]
  open:
    enabled: true
    openUrl: true
  empty: {enabled: true}
`

func parse(t *testing.T, s string) *Set {
	t.Helper()
	set, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestATenantNotNamedGetsTheDefaultAndWithoutADefaultNothingIsEnabled(t *testing.T) {
	s := parse(t, file)
	if !s.For("acme").Enabled {
		t.Fatal("a named tenant is not enabled") // the positive case first
	}
	if s.For("stranger").Enabled {
		t.Error("an unnamed tenant is enabled by a default that says it is not")
	}
	s = parse(t, "default: {enabled: true}\ntenants: {x: {enabled: false}}")
	if !s.For("stranger").Enabled || s.For("x").Enabled {
		t.Error("the default does not apply to the unnamed or is applied over a named one")
	}
	s = parse(t, "tenants: {x: {enabled: true}}")
	if s.For("stranger").Enabled || s.For("").Enabled {
		t.Error("without a default an unnamed tenant is enabled")
	}
	if parse(t, "").For("x").Enabled {
		t.Error("an empty file enables a tenant")
	}
}

func TestOpenURLIsOffUnlessTheFileTurnsItOn(t *testing.T) {
	s := parse(t, file)
	if !s.For("open").OpenURL {
		t.Fatal("openUrl: true was not read")
	}
	if s.For("acme").OpenURL || s.For("empty").OpenURL || s.For("stranger").OpenURL {
		t.Error("openUrl is on without being asked for")
	}
}

func TestADomainEntryCoversTheNameAndItsSubdomainsButNotALookalike(t *testing.T) {
	p := parse(t, file).For("acme")
	for host, want := range map[string]error{
		"example.com":            nil,
		"www.example.com":        nil,
		"a.b.example.com":        nil,
		"gov.cn":                 nil,
		"notexample.com":         ErrNotIncluded,
		"example.com.evil.test":  ErrNotIncluded,
		"example.org":            ErrNotIncluded,
		"private.example.com":    ErrExcluded,
		"x.private.example.com":  ErrExcluded,
		"private.example.com.cn": ErrNotIncluded,
	} {
		if err := p.AllowHost(host); !errors.Is(err, want) && err != want {
			t.Errorf("%s: %v, want %v", host, err, want)
		}
	}
}

func TestExcludeWinsOverIncludeAndAnEmptyIncludeListMeansEveryDomain(t *testing.T) {
	p := parse(t, "tenants: {a: {enabled: true, includeDomains: [example.com], excludeDomains: [example.com]}}").For("a")
	if err := p.AllowHost("example.com"); !errors.Is(err, ErrExcluded) {
		t.Errorf("%v", err)
	}
	all := parse(t, file).For("empty")
	if err := all.AllowHost("anything.test"); err != nil {
		t.Errorf("an empty include list refused a domain: %v", err)
	}
	only := Policy{ExcludeDomains: []string{"bad.test"}}
	if err := only.AllowHost("bad.test"); !errors.Is(err, ErrExcluded) {
		t.Errorf("%v", err)
	}
	if err := only.AllowHost("good.test"); err != nil {
		t.Errorf("%v", err)
	}
}

func TestInternationalisedNamesAreMatchedInTheirASCIIForm(t *testing.T) {
	p := parse(t, "tenants: {a: {enabled: true, excludeDomains: [bücher.example]}}").For("a")
	if err := p.AllowHost("xn--bcher-kva.example"); !errors.Is(err, ErrExcluded) {
		t.Errorf("%v", err)
	}
}

func TestAKeyTheFileDoesNotKnowIsAnErrorAndSoIsADomainThatIsNotAName(t *testing.T) {
	for name, s := range map[string]string{
		"a misspelt key":            "tenants: {a: {enabled: true, excludeDomain: [x.test]}}",
		"an unknown top-level key":  "defualt: {enabled: true}",
		"a url as a domain":         "tenants: {a: {enabled: true, excludeDomains: ['https://x.test/']}}",
		"a path as a domain":        "tenants: {a: {enabled: true, includeDomains: ['x.test/news']}}",
		"a wildcard":                "tenants: {a: {enabled: true, includeDomains: ['*.x.test']}}",
		"an empty domain":           "tenants: {a: {enabled: true, includeDomains: ['']}}",
		"a port":                    "tenants: {a: {enabled: true, includeDomains: ['x.test:8080']}}",
		"a flag that is not a bool": "tenants: {a: {enabled: maybe}}",
		"an empty tenant name":      "tenants: {'': {enabled: true}}",
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := Parse([]byte(file)); err != nil {
		t.Errorf("the good file: %v", err)
	}
}

func TestTheFileIsReadFromDisk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tenants.yaml")
	if err := os.WriteFile(p, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil || s.Count() != 3 {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := Load(p + ".missing"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("%v", err)
	}
}
