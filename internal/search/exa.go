package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// DefaultExaBase is Exa's endpoint; the EU one is https://api.eu.exa.ai (zero data retention by default, appendix A 1.5).
const DefaultExaBase = "https://api.exa.ai"

// Exa searches with POST {base}/search (platform ADR-0003 appendix A, 1.5): the type auto or lite, numResults, the
// highlights of each page in the same call, a published-date filter for freshness, and the include and exclude
// domains.
type Exa struct {
	base   string
	key    string
	typ    string
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
}

// NewExa returns the Exa adapter. typ is "auto" or "lite" (empty means auto); client may be nil (NewClient).
func NewExa(base, key, typ string, client *http.Client, log *slog.Logger) (*Exa, error) {
	if base == "" {
		base = DefaultExaBase
	}
	u, err := hostOf(base)
	if err != nil {
		return nil, err
	}
	switch typ {
	case "":
		typ = "auto"
	case "auto", "lite":
	default:
		return nil, fmt.Errorf("the Exa type %q is not auto or lite", typ)
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("no Exa key")
	}
	if client == nil {
		client = NewClient()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Exa{base: u.String(), key: strings.TrimSpace(key), typ: typ, client: client, log: log, now: time.Now}, nil
}

type exaRequest struct {
	Query              string         `json:"query"`
	Type               string         `json:"type"`
	NumResults         int            `json:"numResults"`
	Contents           map[string]any `json:"contents"`
	StartPublishedDate string         `json:"startPublishedDate,omitempty"`
	IncludeDomains     []string       `json:"includeDomains,omitempty"`
	ExcludeDomains     []string       `json:"excludeDomains,omitempty"`
}

type exaResponse struct {
	Results []struct {
		URL           string   `json:"url"`
		Title         string   `json:"title"`
		PublishedDate string   `json:"publishedDate"`
		Highlights    []string `json:"highlights"`
		Text          string   `json:"text"`
	} `json:"results"`
}

var freshnessDays = map[string]int{"day": 1, "week": 7, "month": 31, "year": 366}

// Search implements Provider.
func (e *Exa) Search(ctx context.Context, q Query) ([]Result, error) {
	body := exaRequest{Query: q.Text, Type: e.typ, NumResults: q.Count, Contents: map[string]any{"highlights": true},
		IncludeDomains: q.IncludeDomains, ExcludeDomains: q.ExcludeDomains}
	if d, ok := freshnessDays[q.Freshness]; ok {
		body.StartPublishedDate = e.now().UTC().AddDate(0, 0, -d).Format("2006-01-02T15:04:05.000Z")
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+"/search", bytes.NewReader(b))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("x-api-key", e.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	var out exaResponse
	if err := call(e.client, e.log, "exa", req, func(b []byte) error { return json.Unmarshal(b, &out) }); err != nil {
		return nil, err
	}
	var res []Result
	for _, r := range out.Results {
		snippet := strings.Join(r.Highlights, " … ")
		if snippet == "" {
			snippet = r.Text
		}
		res = append(res, Result{URL: r.URL, Title: oneLine(r.Title, maxTitle), Published: date(r.PublishedDate), Snippet: oneLine(snippet, maxSnippet)})
		if len(res) == q.Count {
			break
		}
	}
	return res, nil
}
