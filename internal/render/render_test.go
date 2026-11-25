package render

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"idp-web/internal/browsertest"
)

func rig(t *testing.T, respond func(browsertest.Request) browsertest.Answer) (*Client, *browsertest.Double, *Signer) {
	t.Helper()
	priv, pub := browsertest.GenerateKey(t)
	s, err := NewSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	d := browsertest.New(t, pub, respond)
	c, err := New(Config{URL: d.URL(), Signer: s, TimeoutMS: 2000, MaxBytes: 1 << 20, Margin: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c, d, s
}

func page(u string) func(browsertest.Request) browsertest.Answer {
	return func(r browsertest.Request) browsertest.Answer {
		return browsertest.Answer{FinalURL: u, HTML: "<html><body><p>Hello</p></body></html>", Title: "T", MS: 42}
	}
}

func code(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("not a render error: %v", err)
	}
	return e
}

// What the signer makes is verified by the renderer's own verifier (a copy, internal/browsertest), and the request the
// renderer sees is the one that was asked for.
func TestASignedRequestIsAcceptedByTheRenderersVerifierAndCarriesTheTimeLimitsAsked(t *testing.T) {
	c, d, _ := rig(t, page("https://example.com/a"))
	res, err := c.Render(context.Background(), "https://example.com/a")
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalURL != "https://example.com/a" || res.MS != 42 || !strings.Contains(res.HTML, "Hello") {
		t.Errorf("%+v", res)
	}
	got := d.Served()
	if len(got) != 1 || got[0].URL != "https://example.com/a" || got[0].TimeoutMS != 2000 || got[0].MaxBytes != 1<<20 {
		t.Errorf("the renderer was asked %+v", got)
	}
}

// The signature must be the one the verifier wants, and each way of being wrong is refused by the double before any
// script runs: this is what proves that the signer and the verifier agree, and that the agreement is not vacuous.
func TestTheRenderersVerifierRefusesAWrongAudienceAWrongKeyAndAnExpiredAssertion(t *testing.T) {
	for _, tc := range []struct {
		name string
		bend func(d *browsertest.Double, c *Client)
		why  string
	}{
		{"another audience", func(d *browsertest.Double, _ *Client) { d.SetAudience("http://elsewhere.example:8590/render") }, "made for"},
		{"another key", func(d *browsertest.Double, _ *Client) { _, other := browsertest.GenerateKey(t); d.SetKey(other) }, "does not hold"},
		{"expired", func(d *browsertest.Double, _ *Client) {
			d.SetClock(func() time.Time { return time.Now().Add(10 * time.Minute) })
		}, "expired"},
		{"the signer's clock is behind", func(_ *browsertest.Double, c *Client) {
			c.cfg.Signer.Now = func() time.Time { return time.Now().Add(-10 * time.Minute) }
		}, "expired"},
	} {
		c, d, _ := rig(t, page("https://example.com/"))
		tc.bend(d, c)
		_, err := c.Render(context.Background(), "https://example.com/")
		if e := code(t, err); e.Code != Unauthorized || e.Sent {
			t.Errorf("%s: %+v", tc.name, e)
		}
		if r := d.Refused(); len(r) != 1 || !strings.Contains(r[0], tc.why) {
			t.Errorf("%s: the double refused for %q, want %q", tc.name, r, tc.why)
		}
		if len(d.Served()) != 0 {
			t.Errorf("%s: a refused request reached the script", tc.name)
		}
	}
}

// An assertion is single use, and one made for one body does not do for another.
func TestAnAssertionCannotBeReplayedOrMovedToAnotherRequest(t *testing.T) {
	c, d, s := rig(t, page("https://example.com/"))
	body := []byte(`{"url":"https://example.com/","timeoutMs":2000,"maxBytes":1048576}`)
	a, _ := s.Sign(c.cfg.URL, "POST", "/render", body)
	if err := browsertestVerify(d, a, body); err != nil {
		t.Fatal(err)
	}
	if err := browsertestVerify(d, a, body); err == nil || !strings.Contains(err.Error(), "presented before") {
		t.Errorf("replay: %v", err)
	}
	b, _ := s.Sign(c.cfg.URL, "POST", "/render", body)
	if err := browsertestVerify(d, b, []byte(`{"url":"http://169.254.169.254/"}`)); err == nil || !strings.Contains(err.Error(), "another request") {
		t.Errorf("another body: %v", err)
	}
}

// browsertestVerify checks an assertion against a fresh verifier built like the double's.
func browsertestVerify(d *browsertest.Double, assertion string, body []byte) error {
	return d.VerifyOnce(assertion, "POST", "/render", body)
}

// The vectors of idp-browser pin the format: the digest, the header and the claims layout, and the verifier's decisions.
type vectorFile struct {
	Audience string `json:"audience"`
	Subject  string `json:"subject"`
	Keys     []struct{ Name, Kid, PEM string }
	Cases    []struct {
		Name  string
		Steps []struct {
			Method, Path, Body, Assertion, Now, Digest, Reason string
			Accept                                             bool
		}
	}
}

func TestVectors(t *testing.T) {
	raw, err := os.ReadFile("../browsertest/testdata/caller-assertion-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	if vf.Subject != Subject {
		t.Errorf("the vectors' caller is %q, this signer says %q", vf.Subject, Subject)
	}
	for _, c := range vf.Cases {
		v, err := browsertest.NewVerifier([]byte(vf.Keys[0].PEM), vf.Audience, vf.Subject)
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range c.Steps {
			if got := Digest(s.Method, s.Path, []byte(s.Body)); got != s.Digest {
				t.Errorf("%s step %d: digest %s, the vector says %s", c.Name, i, got, s.Digest)
			}
			now, _ := time.Parse(time.RFC3339, s.Now)
			v.SetClock(func() time.Time { return now })
			err := v.Verify(s.Assertion, s.Method, s.Path, []byte(s.Body))
			if (err == nil) != s.Accept || (s.Reason != "" && (err == nil || !strings.Contains(err.Error(), s.Reason))) {
				t.Errorf("%s step %d: verifier said %v, the vector says accept=%v %q", c.Name, i, err, s.Accept, s.Reason)
			}
		}
	}
	// the layout of what this signer makes equals the layout of the vector's good assertion (the key and the random parts apart)
	good := vf.Cases[0].Steps[0].Assertion
	priv, _ := browsertest.GenerateKey(t)
	s, _ := NewSigner(priv)
	mine, _ := s.Sign(vf.Audience, "POST", "/render", []byte(vf.Cases[0].Steps[0].Body))
	shape := func(a string) (string, string) {
		p := strings.Split(a, ".")
		h, _ := base64.RawURLEncoding.DecodeString(p[0])
		c, _ := base64.RawURLEncoding.DecodeString(p[1])
		h2 := regexp.MustCompile(`"kid":"[^"]*"`).ReplaceAllString(string(h), `"kid":"K"`)
		c2 := regexp.MustCompile(`:("[^"]*"|\d+)`).ReplaceAllString(string(c), `:V`)
		if len(p) != 3 {
			t.Error("not a compact JWS")
		}
		return h2, c2
	}
	gh, gc := shape(good)
	mh, mc := shape(mine)
	if gh != mh || gc != mc {
		t.Errorf("layout differs:\nvector %s %s\nmine   %s %s", gh, gc, mh, mc)
	}
	// aud is the audience exactly as given, sub is idp-web, and the lifetime is 30 seconds
	cl := strings.Split(mine, ".")[1]
	b, _ := base64.RawURLEncoding.DecodeString(cl)
	var m claims
	_ = json.Unmarshal(b, &m)
	if m.Audience != vf.Audience || m.Subject != "idp-web" || m.Expires-m.Issued != 30 || m.ID == "" {
		t.Errorf("%+v", m)
	}
}

func TestEachCodeOfTheRendererIsAnErrorThatSaysWhetherAnythingWasStarted(t *testing.T) {
	for _, tc := range []struct {
		answer string
		code   string
		sent   bool
	}{
		{"blocked_by_site", BlockedBySite, true}, {"render_failed", RenderFailed, true}, {"render_timeout", RenderTimeout, true},
		{"render_busy", RenderBusy, false}, {"not_allowed", NotAllowed, false}, {"invalid_request", InvalidRequest, false},
	} {
		c, _, _ := rig(t, func(browsertest.Request) browsertest.Answer { return browsertest.Answer{Error: tc.answer} })
		_, err := c.Render(context.Background(), "https://example.com/")
		if e := code(t, err); e.Code != tc.code || e.Sent != tc.sent {
			t.Errorf("%s: %+v", tc.answer, e)
		}
	}
	// a code that is not in the renderer's vocabulary, an answer that is not a page, and a page that is larger than asked
	for name, a := range map[string]browsertest.Answer{
		"unknown code":   {Raw: `{"error":{"code":"teapot"}}`, RawStatus: 418},
		"no reason":      {Raw: `oops`, RawStatus: 500},
		"not a page":     {Raw: `{"html":"x"}`, RawStatus: 200},
		"too large html": {FinalURL: "https://example.com/", HTML: strings.Repeat("x", 2<<20)},
	} {
		a := a
		c, _, _ := rig(t, func(browsertest.Request) browsertest.Answer { return a })
		_, err := c.Render(context.Background(), "https://example.com/")
		if e := code(t, err); e.Code != RenderFailed {
			t.Errorf("%s: %+v", name, e)
		}
	}
}

func TestARendererThatHangsIsCutOffAtTheLimitAndTheMargin(t *testing.T) {
	c, d, _ := rig(t, func(browsertest.Request) browsertest.Answer { return browsertest.Answer{Hang: true} })
	c.cfg.TimeoutMS = 100
	c.cfg.Margin = 100 * time.Millisecond
	start := time.Now()
	_, err := c.Render(context.Background(), "https://example.com/")
	if e := code(t, err); e.Code != RenderTimeout || !e.Sent {
		t.Errorf("%+v", e)
	}
	if el := time.Since(start); el < 150*time.Millisecond || el > 2*time.Second {
		t.Errorf("returned after %v", el)
	}
	d.Release()
}

func TestARendererThatIsNotThereIsUnreachableAndASentCallIsCancelledNotMistakenForOne(t *testing.T) {
	c, d, _ := rig(t, page("https://example.com/"))
	d.Close()
	_, err := c.Render(context.Background(), "https://example.com/")
	if e := code(t, err); e.Code != Unreachable || e.Sent {
		t.Errorf("%+v", e)
	}
	c2, _, _ := rig(t, func(browsertest.Request) browsertest.Answer { return browsertest.Answer{Hang: true} })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err = c2.Render(ctx, "https://example.com/")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
}

func TestTheEndpointAddressIsChecked(t *testing.T) {
	for _, u := range []string{"", "idp-browser:8590/render", "ftp://x/render", "http://x:8590", "http://x:8590/render?a=1", "http://u:p@x/render", "http://x/render#f", "http://x/a b"} {
		if _, err := ParseURL(u); err == nil {
			t.Errorf("%q was accepted", u)
		}
	}
	if _, err := ParseURL("http://idp-browser.idp-browser.svc.cluster.local:8590/render"); err != nil {
		t.Error(err)
	}
}
