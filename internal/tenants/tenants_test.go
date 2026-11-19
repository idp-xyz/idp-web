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

// A daily limit is absent (no limit), a number (that many), or 0 (none at all): the three are different and the file keeps
// them apart. The default and a tenant's own entry are not merged, as with every other setting: a named tenant has what its
// entry says, so an entry with no limit has none even when the default has one.
func TestADailyLimitIsAbsentANumberOrZeroAndATenantsEntryReplacesTheDefaultsLimits(t *testing.T) {
	s := parse(t, `
default: {enabled: true, dailySearches: 100, dailyReads: 300}
tenants:
  acme: {enabled: true, dailySearches: 10}
  none: {enabled: true, dailySearches: 0, dailyReads: 0}
  free: {enabled: true}
  nulls: {enabled: true, dailySearches: ~}
`)
	for tenant, want := range map[string]struct {
		kind    string
		limit   int
		limited bool
	}{
		"stranger/searches": {"searches", 100, true},
		"stranger/reads":    {"reads", 300, true},
		"acme/searches":     {"searches", 10, true},
		"acme/reads":        {"reads", 0, false},
		"none/searches":     {"searches", 0, true},
		"none/reads":        {"reads", 0, true},
		"free/searches":     {"searches", 0, false},
		"nulls/searches":    {"searches", 0, false},
	} {
		name, _, _ := strings.Cut(tenant, "/")
		if l, ok := s.For(name).Daily(want.kind); l != want.limit || ok != want.limited {
			t.Errorf("%s: limit %d, limited %v; want %d, %v", tenant, l, ok, want.limit, want.limited)
		}
	}
	if got := strings.Join(s.DailyLimited(), ","); got != "acme,default,none" {
		t.Errorf("the entries with a daily limit: %s", got)
	}
	if got := parse(t, file).DailyLimited(); len(got) != 0 {
		t.Errorf("a file with no limits names %v", got)
	}
	if _, ok := (Policy{}).Daily("other"); ok {
		t.Error("a kind nobody knows is limited")
	}
}

func TestADailyLimitThatIsNegativeTooLargeOrNotAWholeNumberIsAnErrorThatNamesIt(t *testing.T) {
	for name, c := range map[string]struct{ yaml, want string }{
		"negative":    {"tenants: {a: {enabled: true, dailySearches: -1}}", "dailySearches must be a whole number from 0 to 1000000000"},
		"too large":   {"tenants: {a: {enabled: true, dailyReads: 1000000001}}", "dailyReads must be a whole number"},
		"a fraction":  {"tenants: {a: {enabled: true, dailyReads: 1.5}}", "not a whole number"},
		"a string":    {"tenants: {a: {enabled: true, dailySearches: many}}", "not a whole number"},
		"the default": {"default: {enabled: true, dailySearches: -5}", "default"},
		"a misspelt":  {"tenants: {a: {enabled: true, dailySearch: 5}}", "dailySearch"},
	} {
		if _, err := Parse([]byte(c.yaml)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Parse([]byte("tenants: {a: {enabled: true, dailySearches: 1000000000}}")); err != nil {
		t.Errorf("the largest limit: %v", err)
	}
}
