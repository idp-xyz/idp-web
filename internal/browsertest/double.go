package browsertest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// GenerateKey makes a P-256 key pair as PEM (private PKCS#8, public PKIX).
func GenerateKey(t testing.TB) (privPEM, pubPEM []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pub, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})
}

// Request is what the renderer is asked (idp-browser's render.Request).
type Request struct {
	URL       string `json:"url"`
	TimeoutMS int    `json:"timeoutMs,omitempty"`
	MaxBytes  int    `json:"maxBytes,omitempty"`
}

// Answer is what a script says: a page, or an error code.
type Answer struct {
	// Error is a reason code (invalid_request, not_allowed, render_failed, blocked_by_site, render_busy, render_timeout);
	// the HTTP status is the one the renderer uses for it.
	Error string
	// Page fields, for a success.
	FinalURL, HTML, Title string
	Status                int // default 200
	MS                    int64
	// Hang holds the answer until the request is cancelled (the client's deadline) or Release is called.
	Hang bool
	// Raw replaces the body (with HTTP status RawStatus), for answers the renderer would never give.
	Raw       string
	RawStatus int
}

// Double is an HTTP server that answers POST /render the way idp-browser does: it verifies the assertion first, before
// it reads the request, with the renderer's own verifier; an unauthorised request is a 401 {"error":{"code":"unauthorized"}}
// and is never shown to the script.
type Double struct {
	srv *httptest.Server
	// Respond is the script: called for each verified request.
	Respond func(Request) Answer

	mu       sync.Mutex
	v        *Verifier
	pub      []byte
	aud      string
	now      func() time.Time
	served   []Request
	refused  []string // why, in the verifier's words
	release  chan struct{}
	inflight int
}

// New starts a double that holds the public key pub (what idp-browser holds of idp-web) and whose audience is its own
// address plus /render, unless SetAudience says otherwise. It is closed with the test.
func New(t testing.TB, pub []byte, respond func(Request) Answer) *Double {
	t.Helper()
	d := &Double{Respond: respond, pub: pub, now: time.Now, release: make(chan struct{})}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(func() { d.Release(); d.srv.Close() })
	d.SetAudience(d.URL())
	return d
}

// URL is the render endpoint.
func (d *Double) URL() string { return d.srv.URL + "/render" }

// SetAudience changes the audience the double verifies for (and restarts its replay table).
func (d *Double) SetAudience(aud string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.aud = aud
	d.rebuild()
}

// SetKey changes the public key the double holds.
func (d *Double) SetKey(pub []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pub = pub
	d.rebuild()
}

// SetClock moves the verifier's clock (to show that an assertion expires).
func (d *Double) SetClock(now func() time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = now
	d.rebuild()
}

func (d *Double) rebuild() {
	v, err := NewVerifier(d.pub, d.aud, "idp-web")
	if err != nil {
		panic(err)
	}
	v.SetClock(d.now)
	d.v = v
}

// Release lets the requests held by Hang go.
func (d *Double) Release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.release:
	default:
		close(d.release)
	}
}

// Served is the requests that were verified and reached the script, in order.
func (d *Double) Served() []Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Request(nil), d.served...)
}

// Refused is why each request that failed verification did.
func (d *Double) Refused() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.refused...)
}

// Calls is every request that arrived, verified or not.
func (d *Double) Calls() int { return len(d.Served()) + len(d.Refused()) }

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"scripted"}}`))
}

var statusOf = map[string]int{"invalid_request": 400, "unauthorized": 401, "not_allowed": 422, "render_busy": 503, "render_timeout": 504, "render_failed": 502, "blocked_by_site": 502}

func (d *Double) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/render" || r.URL.RawQuery != "" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		writeError(w, 413, "invalid_request")
		return
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		bearer = ""
	}
	d.mu.Lock()
	v := d.v
	d.mu.Unlock()
	if err := v.Verify(bearer, r.Method, r.URL.Path, body); err != nil {
		d.mu.Lock()
		d.refused = append(d.refused, err.Error())
		d.mu.Unlock()
		writeError(w, 401, "unauthorized")
		return
	}
	var req Request
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeError(w, 400, "invalid_request")
		return
	}
	d.mu.Lock()
	d.served = append(d.served, req)
	d.mu.Unlock()
	a := d.Respond(req)
	if a.Hang {
		select {
		case <-r.Context().Done():
			return
		case <-d.release:
		}
	}
	switch {
	case a.Raw != "":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.RawStatus)
		_, _ = w.Write([]byte(a.Raw))
	case a.Error != "":
		writeError(w, statusOf[a.Error], a.Error)
	default:
		status := a.Status
		if status == 0 {
			status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"finalUrl": a.FinalURL, "html": a.HTML, "status": status, "title": a.Title,
			"requests": 3, "blocked": 0, "bytes": len(a.HTML), "ms": a.MS, "settled": true})
	}
}

// Close stops the server (the renderer is down).
func (d *Double) Close() { d.Release(); d.srv.Close() }

// VerifyOnce checks an assertion with the double's verifier, as the double would, and records nothing else.
func (d *Double) VerifyOnce(assertion, method, path string, body []byte) error {
	d.mu.Lock()
	v := d.v
	d.mu.Unlock()
	return v.Verify(assertion, method, path, body)
}
