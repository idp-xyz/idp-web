package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Fixture answers from a file, for development and acceptance: no network and no key (platform ADR-0003 D3). The file
// is a list of entries; the first whose match is contained in the query (ignoring case) gives the results, else the
// default does, else there are none. The results point where the file says, usually at a local test origin.
//
//	entries:
//	  - match: parcel
//	    results:
//	      - {url: "http://127.0.0.1:9000/a", title: "A", published: "2026-01-02", snippet: "..."}
//	default: [ ... ]          # optional
//
// It ignores the domain lists and freshness, like a provider whose filters do not work: the caller enforces the
// lists itself, and a fixture that obeyed them would let a test pass without that being true.
type Fixture struct {
	entries []fixtureEntry
	def     []Result
}

type fixtureResult struct {
	URL       string `yaml:"url"`
	Title     string `yaml:"title"`
	Published string `yaml:"published"`
	Snippet   string `yaml:"snippet"`
}

type fixtureEntry struct {
	Match   string          `yaml:"match"`
	Results []fixtureResult `yaml:"results"`
}

type fixtureFile struct {
	Entries []fixtureEntry  `yaml:"entries"`
	Default []fixtureResult `yaml:"default"`
}

func results(in []fixtureResult) []Result {
	var out []Result
	for _, r := range in {
		out = append(out, Result{URL: r.URL, Title: r.Title, Published: r.Published, Snippet: r.Snippet})
	}
	return out
}

// LoadFixture reads the file at path.
func LoadFixture(path string) (*Fixture, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseFixture(b)
}

// ParseFixture reads the YAML of a fixture file; a key it does not know is an error.
func ParseFixture(b []byte) (*Fixture, error) {
	var f fixtureFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("the fixture file: %w", err)
	}
	fx := &Fixture{def: results(f.Default)}
	for i, e := range f.Entries {
		if strings.TrimSpace(e.Match) == "" {
			return nil, fmt.Errorf("the fixture file: entry %d has an empty match, which would match every query", i+1)
		}
		fx.entries = append(fx.entries, fixtureEntry{Match: strings.ToLower(e.Match), Results: e.Results})
	}
	return fx, nil
}

// Search implements Provider.
func (f *Fixture) Search(_ context.Context, q Query) ([]Result, error) {
	text := strings.ToLower(q.Text)
	res := f.def
	for _, e := range f.entries {
		if strings.Contains(text, e.Match) {
			res = results(e.Results)
			break
		}
	}
	if q.Count > 0 && len(res) > q.Count {
		res = res[:q.Count]
	}
	out := make([]Result, len(res))
	for i, r := range res {
		out[i] = Result{URL: r.URL, Title: oneLine(r.Title, maxTitle), Published: r.Published, Snippet: oneLine(r.Snippet, maxSnippet)}
	}
	return out, nil
}
