// Package render is idp-web's side of the renderer (idp-browser): the signed request, the answer, and the errors that
// say what went wrong (ADR-0001 D33). It decides nothing about which pages are rendered; internal/tools does.
//
// The renderer is a service of the platform on a private network, reached at the address the operator configures
// (IDP_WEB_RENDER_URL). That address is not a page address: it is not run through the address rules, and the client
// uses no proxy and follows no redirect. What it sends and takes back is bounded: a request body of one address and two
// numbers, an answer of at most the size a page of MaxBytes can have once JSON has escaped it.
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The reason codes of the renderer (idp-browser ADR-0001 D4), and the two this client adds for answers it cannot get.
const (
	RenderTimeout  = "render_timeout"
	RenderFailed   = "render_failed"
	BlockedBySite  = "blocked_by_site"
	RenderBusy     = "render_busy"
	InvalidRequest = "invalid_request"
	NotAllowed     = "not_allowed"
	Unauthorized   = "unauthorized"
	// Unreachable: no answer from the renderer at all (nothing listens, the connection broke before an answer).
	Unreachable = "render_unreachable"
)

// Defaults of the settings (IDP_WEB_RENDER_TIMEOUT_MS, IDP_WEB_RENDER_MAX_BYTES).
const (
	DefaultTimeoutMS = 15000
	DefaultMaxBytes  = 5 << 20
	// the renderer's own limits (idp-browser config): a request outside them is invalid_request there
	MinMaxBytes = 1024
	MaxMaxBytes = 20 << 20
)

// Error is a render that did not give a page. Sent says whether the renderer may have started work for it, which is what
// the daily quota counts (a request that was refused before any browser work is given back).
type Error struct {
	Code    string
	Message string
	Sent    bool
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "the renderer: " + e.Code
	}
	return "the renderer: " + e.Code + ": " + e.Message
}

// Result is the renderer's answer to a page (idp-browser's render.Result).
type Result struct {
	FinalURL  string `json:"finalUrl"`
	HTML      string `json:"html"`
	Status    int    `json:"status"`
	Title     string `json:"title"`
	Requests  int    `json:"requests"`
	Blocked   int    `json:"blocked"`
	Bytes     int64  `json:"bytes"`
	MS        int64  `json:"ms"`
	Truncated bool   `json:"truncated,omitempty"`
	Limit     string `json:"limit,omitempty"`
	Settled   bool   `json:"settled"`
}

// Config of a Client.
type Config struct {
	// URL is the render endpoint, for example http://idp-browser.idp-browser.svc.cluster.local:8590/render. It is also the
	// audience of every assertion, exactly as written here.
	URL    string
	Signer *Signer
	// TimeoutMS and MaxBytes are what each request asks of the renderer.
	TimeoutMS, MaxBytes int
	// Margin is how long past the renderer's own time limit this client waits for its answer (default 5 s): the renderer
	// answers render_timeout at the limit, and only a renderer that does not answer at all is cut off here.
	Margin time.Duration
	// HTTP replaces the client, for tests.
	HTTP *http.Client
}

// Client calls the renderer.
type Client struct {
	cfg  Config
	path string
	http *http.Client
}

// ParseURL checks the render endpoint: http or https, a host, a path, and no user information, query or fragment (the
// renderer answers a request with a query with 404, and a path other than the one it signs for would not verify).
func ParseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not an address", raw)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("%q is not an http or https address with a host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(raw, "?") || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("%q has user information, a query or a fragment", raw)
	}
	if u.Path == "" || u.Path != u.EscapedPath() {
		return nil, fmt.Errorf("%q has no path, or one that needs escaping (it is, for example, /render)", raw)
	}
	return u, nil
}

// New returns a Client.
func New(cfg Config) (*Client, error) {
	u, err := ParseURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.Signer == nil {
		return nil, errors.New("no signing key")
	}
	if cfg.TimeoutMS == 0 {
		cfg.TimeoutMS = DefaultTimeoutMS
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.TimeoutMS < 1 || cfg.MaxBytes < MinMaxBytes || cfg.MaxBytes > MaxMaxBytes {
		return nil, fmt.Errorf("the render time limit must be at least 1 ms and the byte limit from %d to %d", MinMaxBytes, MaxMaxBytes)
	}
	if cfg.Margin == 0 {
		cfg.Margin = 5 * time.Second
	}
	c := &Client{cfg: cfg, path: u.Path, http: cfg.HTTP}
	if c.http == nil {
		c.http = &http.Client{
			Transport: &http.Transport{
				Proxy: nil, // the renderer is reached directly, whatever the environment says
				// the assertion is single use and the renderer's answer is large: one connection per request is no cost here
				DisableKeepAlives:     true,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: time.Duration(cfg.TimeoutMS)*time.Millisecond + cfg.Margin,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return c, nil
}

// maxAnswer bounds what is read of an answer: a page of MaxBytes with every byte escaped as \u00xx, and the rest of the
// fields.
func (c *Client) maxAnswer() int64 { return int64(c.cfg.MaxBytes)*6 + 64<<10 }

type request struct {
	URL       string `json:"url"`
	TimeoutMS int    `json:"timeoutMs"`
	MaxBytes  int    `json:"maxBytes"`
}

// Render asks the renderer for the page at rawURL. The address has been through the address rules and the tenant's
// lists and robots.txt by the caller. On any failure the error is an *Error.
func (c *Client) Render(ctx context.Context, rawURL string) (Result, error) {
	body, _ := json.Marshal(request{URL: rawURL, TimeoutMS: c.cfg.TimeoutMS, MaxBytes: c.cfg.MaxBytes})
	assertion, err := c.cfg.Signer.Sign(c.cfg.URL, http.MethodPost, c.path, body)
	if err != nil {
		return Result{}, &Error{Code: RenderFailed, Message: "the request could not be signed"}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.TimeoutMS)*time.Millisecond+c.cfg.Margin)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return Result{}, &Error{Code: RenderFailed, Message: "the request could not be made"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+assertion)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				// no answer in the renderer's limit and the margin: work may have started
				return Result{}, &Error{Code: RenderTimeout, Message: "the renderer did not answer in time", Sent: true}
			}
			return Result{}, ctx.Err() // the call was cancelled
		}
		return Result{}, &Error{Code: Unreachable, Message: "no answer from the renderer"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxAnswer()+1))
	if err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{}, &Error{Code: RenderTimeout, Message: "the renderer did not finish its answer in time", Sent: true}
		}
		return Result{}, &Error{Code: Unreachable, Message: "the answer of the renderer broke off", Sent: true}
	}
	if int64(len(raw)) > c.maxAnswer() {
		return Result{}, &Error{Code: RenderFailed, Message: "the answer of the renderer is larger than it may be", Sent: true}
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Code, Message string }
		}
		if json.Unmarshal(raw, &e) != nil || e.Error.Code == "" {
			return Result{}, &Error{Code: RenderFailed, Message: fmt.Sprintf("the renderer answered HTTP %d with no reason code", resp.StatusCode), Sent: true}
		}
		code := e.Error.Code
		switch code {
		case RenderTimeout, RenderFailed, BlockedBySite, RenderBusy, InvalidRequest, NotAllowed, Unauthorized:
		default:
			code = RenderFailed
		}
		// refused before any browser work: given back by the quota
		sent := code != Unauthorized && code != InvalidRequest && code != NotAllowed && code != RenderBusy
		return Result{}, &Error{Code: code, Message: clip(e.Error.Message), Sent: sent}
	}
	var res Result
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&res); err != nil || res.FinalURL == "" {
		return Result{}, &Error{Code: RenderFailed, Message: "the answer of the renderer is not a page", Sent: true}
	}
	if len(res.HTML) > c.cfg.MaxBytes {
		return Result{}, &Error{Code: RenderFailed, Message: "the page is larger than the limit sent", Sent: true}
	}
	return res, nil
}

func clip(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, s)
}
