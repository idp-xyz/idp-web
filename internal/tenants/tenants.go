// Package tenants reads the file that says what each tenant may do with the web (ADR-0001 D18, platform ADR-0003 D6 and
// appendix C 4.7): whether the tools are on, which domains a search may return and a page may be read from, and whether
// web_open_url is on. The file is read once at start. The tenant asked about is always the verified caller's.
package tenants

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/net/idna"
	"gopkg.in/yaml.v3"
)

// The reasons a tenant may not do something, as sentences the model can act on.
var (
	ErrDisabled     = errors.New("web search and reading are not enabled for this tenant")
	ErrOpenDisabled = errors.New("web_open_url is not enabled for this tenant: an administrator must set openUrl: true for the tenant in the tenants file (IDP_WEB_TENANTS_FILE) and restart the service; until then only addresses listed by web_search can be read")
	ErrExcluded     = errors.New("the domain is excluded for this tenant")
	ErrNotIncluded  = errors.New("the domain is not on this tenant's list of domains that may be read")
)

// Policy is what one tenant may do.
type Policy struct {
	Enabled bool
	// IncludeDomains: when not empty, only these domains and their subdomains. Empty means every domain.
	IncludeDomains []string
	// ExcludeDomains: never these domains and their subdomains. It wins over IncludeDomains.
	ExcludeDomains []string
	// OpenURL allows web_open_url. Off unless the file says so (appendix C 2 item 2).
	OpenURL bool
	// DailySearches and DailyReads are how many searches and page reads that go out to the web the tenant may make in a UTC
	// day (ADR-0001 D28). nil means no limit, and then the database is not asked; 0 means none at all. Use Daily.
	DailySearches, DailyReads *int
}

// Daily returns the tenant's limit for the kind ("searches" or "reads") and whether there is one.
func (p Policy) Daily(kind string) (limit int, limited bool) {
	var l *int
	switch kind {
	case "searches":
		l = p.DailySearches
	case "reads":
		l = p.DailyReads
	}
	if l == nil {
		return 0, false
	}
	return *l, true
}

// MaxDaily is the largest daily limit the file may name: it is stored in a 32-bit column, and a limit this high is no
// limit in practice, which is what leaving the setting out says.
const MaxDaily = 1_000_000_000

// rawPolicy is the file's form of a Policy.
type rawPolicy struct {
	Enabled        bool         `yaml:"enabled"`
	IncludeDomains []string     `yaml:"includeDomains"`
	ExcludeDomains []string     `yaml:"excludeDomains"`
	OpenURL        bool         `yaml:"openUrl"`
	DailySearches  *wholeNumber `yaml:"dailySearches"`
	DailyReads     *wholeNumber `yaml:"dailyReads"`
}

// wholeNumber is a number in the file that must be written as a whole number: yaml.v3 would read 1.5 into an int as 1, and
// a limit that quietly becomes another one is worse than an error.
type wholeNumber int

func (w *wholeNumber) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!int" {
		return fmt.Errorf("line %d: %q is not a whole number (a daily limit is a whole number from 0 to %d; leave it out for no limit)", n.Line, n.Value, MaxDaily)
	}
	var v int
	if err := n.Decode(&v); err != nil {
		return fmt.Errorf("line %d: %q is not a whole number that fits", n.Line, n.Value)
	}
	*w = wholeNumber(v)
	return nil
}

func (w *wholeNumber) ptr() *int {
	if w == nil {
		return nil
	}
	v := int(*w)
	return &v
}

type rawFile struct {
	Default *rawPolicy           `yaml:"default"`
	Tenants map[string]rawPolicy `yaml:"tenants"`
}

// Set is the tenants file as loaded.
type Set struct {
	def     Policy // the zero Policy (disabled) when the file has no default
	tenants map[string]Policy
}

// Load reads and checks the file at path.
func Load(path string) (*Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse reads the YAML of the tenants file. A key it does not know is an error, because a misspelt "excludeDomain" must
// not silently leave a domain readable; so is a domain entry that is not a host name.
func Parse(b []byte) (*Set, error) {
	var raw rawFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("the tenants file: %w", err)
	}
	s := &Set{tenants: map[string]Policy{}}
	if raw.Default != nil {
		p, err := raw.Default.policy()
		if err != nil {
			return nil, fmt.Errorf("the tenants file, default: %w", err)
		}
		s.def = p
	}
	for id, rp := range raw.Tenants {
		if id == "" {
			return nil, errors.New("the tenants file: a tenant has an empty name")
		}
		p, err := rp.policy()
		if err != nil {
			return nil, fmt.Errorf("the tenants file, tenant %q: %w", id, err)
		}
		s.tenants[id] = p
	}
	return s, nil
}

func (r rawPolicy) policy() (Policy, error) {
	p := Policy{Enabled: r.Enabled, OpenURL: r.OpenURL, DailySearches: r.DailySearches.ptr(), DailyReads: r.DailyReads.ptr()}
	for _, f := range []struct {
		name string
		l    *int
	}{{"dailySearches", p.DailySearches}, {"dailyReads", p.DailyReads}} {
		if f.l != nil && (*f.l < 0 || *f.l > MaxDaily) {
			return p, fmt.Errorf("%s must be a whole number from 0 to %d (0 allows none), not %d; leave it out for no limit", f.name, MaxDaily, *f.l)
		}
	}
	var err error
	if p.IncludeDomains, err = normalize(r.IncludeDomains); err != nil {
		return p, fmt.Errorf("includeDomains: %w", err)
	}
	if p.ExcludeDomains, err = normalize(r.ExcludeDomains); err != nil {
		return p, fmt.Errorf("excludeDomains: %w", err)
	}
	return p, nil
}

// normalize turns the entries into lower-case ASCII host names. An entry that has a scheme, a path, a port or a
// wildcard is refused rather than guessed at: "example.com/news" would otherwise look like a path filter, which this
// service does not offer.
func normalize(in []string) ([]string, error) {
	var out []string
	for _, d := range in {
		h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if h == "" || strings.ContainsAny(h, "/:*?#@ ") {
			return nil, fmt.Errorf("%q is not a domain name (no scheme, path, port or wildcard: a domain covers its subdomains)", d)
		}
		a, err := idna.Lookup.ToASCII(h)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid domain name", d)
		}
		out = append(out, a)
	}
	return out, nil
}

// For returns the policy of a tenant: its own entry, else the default, else a disabled one.
func (s *Set) For(tenant string) Policy {
	if p, ok := s.tenants[tenant]; ok {
		return p
	}
	return s.def
}

// DailyLimited names the entries of the file that set a daily limit ("default" for the default), sorted: a deployment with
// any needs the database (ADR-0001 D28), and the service refuses to start without one.
func (s *Set) DailyLimited() []string {
	var out []string
	set := func(p Policy) bool { return p.DailySearches != nil || p.DailyReads != nil }
	if set(s.def) {
		out = append(out, "default")
	}
	for id, p := range s.tenants {
		if set(p) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Count is the number of tenants named in the file.
func (s *Set) Count() int { return len(s.tenants) }

func covers(domain, host string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// AllowHost says whether a page may be read from host (a lower-case ASCII name or an IP literal, as addr.Address gives
// it), and for a search result whether it may be returned. An excluded domain is refused first; then, if the tenant has
// an include list, a host outside it. The tenant's domain lists are enforced here on every address and every
// redirect, whatever the search service did with the lists it was sent.
func (p Policy) AllowHost(host string) error {
	for _, d := range p.ExcludeDomains {
		if covers(d, host) {
			return ErrExcluded
		}
	}
	if len(p.IncludeDomains) == 0 {
		return nil
	}
	for _, d := range p.IncludeDomains {
		if covers(d, host) {
			return nil
		}
	}
	return ErrNotIncluded
}
