package search

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// BraveBase is Brave's endpoint (platform ADR-0003 appendix A, section 2). It is not configurable: there is no regional
// endpoint to choose.
const BraveBase = "https://api.search.brave.com/res/v1/web/search"

// Brave searches with GET {base}?q=..&count=..&extra_snippets=true, the key in X-Subscription-Token. Freshness is
// Brave's pd, pw, pm and py. Brave has no domain parameter: the lists go into the query as the search operators site:
// and -site:. One included domain becomes "site:"; with several the operator is not used (the reference used the first
// only, which would hide the others), and the caller's own check keeps out what is not on the list.
type Brave struct {
	base   string
	key    string
	client *http.Client
	log    *slog.Logger
}

// NewBrave returns the Brave adapter; client may be nil (NewClient). The base URL is a parameter for tests only.
func NewBrave(base, key string, client *http.Client, log *slog.Logger) (*Brave, error) {
	if base == "" {
		base = BraveBase
	}
	if _, err := hostOf(base); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("no Brave key")
	}
	if client == nil {
		client = NewClient()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Brave{base: base, key: strings.TrimSpace(key), client: client, log: log}, nil
}

var braveFreshness = map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py"}

var tags = regexp.MustCompile(`<[^>]+>`)

// Search implements Provider.
func (b *Brave) Search(ctx context.Context, q Query) ([]Result, error) {
	text := q.Text
	if len(q.IncludeDomains) == 1 {
		text += " site:" + q.IncludeDomains[0]
	}
	for _, d := range q.ExcludeDomains {
		text += " -site:" + d
	}
	v := url.Values{"q": {text}, "count": {strconv.Itoa(q.Count)}, "extra_snippets": {"true"}}
	if f, ok := braveFreshness[q.Freshness]; ok {
		v.Set("freshness", f)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+"?"+v.Encode(), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("X-Subscription-Token", b.key)
	req.Header.Set("Accept", "application/json")
	var out struct {
		Web struct {
			Results []struct {
				URL           string   `json:"url"`
				Title         string   `json:"title"`
				Description   string   `json:"description"`
				PageAge       string   `json:"page_age"`
				ExtraSnippets []string `json:"extra_snippets"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := call(b.client, b.log, "brave", req, func(b []byte) error { return json.Unmarshal(b, &out) }); err != nil {
		return nil, err
	}
	var res []Result
	for _, r := range out.Web.Results {
		snippet := strings.Join(append([]string{r.Description}, r.ExtraSnippets...), " … ")
		res = append(res, Result{URL: r.URL, Title: oneLine(tags.ReplaceAllString(r.Title, ""), maxTitle), Published: date(r.PageAge),
			Snippet: oneLine(tags.ReplaceAllString(snippet, ""), maxSnippet)})
		if len(res) == q.Count {
			break
		}
	}
	return res, nil
}
