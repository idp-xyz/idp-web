package fetch

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"idp-web/internal/addr"
	"idp-web/internal/extract"
)

func TestAPageIsReadAsMarkdownWithItsTitleAndTypeAndTheReaderIdentifiesItself(t *testing.T) {
	e := newEnv(t, routes{"/a": html(articleBody)})
	p, err := e.r.Read(context.Background(), e.url("/a?x=1#frag"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "forty thousand parcels") || strings.Contains(p.Text, "Copyright footer") {
		t.Errorf("text:\n%s", p.Text)
	}
	if p.Title != "The Parcel Report" || p.Type != "text/html" || p.Engine != extract.Readability || p.Bytes != len(articleBody) {
		t.Errorf("%+v", p)
	}
	if p.FinalURL != e.url("/a?x=1") {
		t.Errorf("final URL %s", p.FinalURL)
	}
	// what the site sees
	h := e.hits.heads[len(e.hits.heads)-1]
	if h.Get("User-Agent") != "idp-web/test (+https://github.com/idp-xyz/idp-web)" || h.Get("Accept-Encoding") != "gzip, deflate" || h.Get("Cookie") != "" || h.Get("Referer") != "" {
		t.Errorf("request headers %v", h)
	}
	if e.hits.paths[0] != "/robots.txt" {
		t.Errorf("robots.txt was not asked first: %v", e.hits.paths)
	}
}

func TestPlainTextIsReturnedAsItIs(t *testing.T) {
	e := newEnv(t, routes{"/t": func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "  hello\nworld \n")
	}})
	p, err := e.r.Read(context.Background(), e.url("/t"))
	if err != nil || p.Text != "hello\nworld" || p.Engine != extract.Plain || p.Type != "text/plain" {
		t.Errorf("%+v %v", p, err)
	}
}

// The 20 addresses of the evaluation's SSRF list (tools/web_eval/e4.py), through the whole reader: none is contacted.
func TestTheTwentyAddressesOfTheSSRFListAreRefusedWithoutAConnection(t *testing.T) {
	e := newEnv(t, routes{})
	e.res.add("localhost", "127.0.0.1").add("keycloak", "10.0.0.7")
	e.trackDials()
	ssrf := []string{
		"http://127.0.0.1/", "http://localhost:8080/", "http://[::1]/", "http://0.0.0.0/", "http://2130706433/", "http://0x7f.1/",
		"http://169.254.169.254/latest/meta-data/", "http://[fd00:ec2::254]/", "http://10.0.0.1/", "http://172.16.5.4/", "http://192.168.1.1/",
		"http://100.64.0.1/", "http://[::ffff:127.0.0.1]/", "http://224.0.0.1/", "http://keycloak:7080/", "http://user:pass@example.com/",
		"file:///etc/passwd", "ftp://example.com/", "gopher://example.com/", "javascript:alert(1)",
	}
	// and the names of the list at the standard ports, which only the lookup can refuse
	ssrf = append(ssrf, "http://localhost/", "http://keycloak/", "https://keycloak/")
	for _, u := range ssrf {
		_, err := e.r.Read(context.Background(), u)
		if !errors.Is(err, addr.ErrRefused) && !errors.Is(err, ErrNotPublic) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if n := len(e.dials()); n != 0 {
		t.Errorf("%d connections were made", n)
	}
	if e.hits.total() != 0 {
		t.Error("the test server was reached")
	}
}

func TestANameWithAPrivateAnswerAmongItsPublicOnesIsRefusedWithoutAConnection(t *testing.T) {
	e := newEnv(t, routes{"/": html(articleBody)})
	e.res.add("mixed.test", "93.184.216.34, 10.0.0.1").add("fine.test", "93.184.216.34")
	e.trackDials()
	_, err := e.r.Read(context.Background(), "http://mixed.test/")
	is(t, err, ErrNotPublic)
	if len(e.dials()) != 0 {
		t.Errorf("a connection was made: %v", e.dials())
	}
	if strings.Contains(err.Error(), "10.0.0.1") {
		t.Errorf("the message tells the model what the name resolves to: %v", err)
	}
	// the control: the same name with only the public answer is read, from that address
	p, err := e.r.Read(context.Background(), "http://fine.test/")
	if err != nil || !strings.Contains(p.Text, "parcels") {
		t.Fatalf("%v", err)
	}
	if got := e.dials(); got[0] != "93.184.216.34:80" {
		t.Errorf("dialed %v", got)
	}
}

// DNS rebinding: the name answers a public address when it is checked and would answer a private one if it were asked
// again. The connection goes to the address that was checked, and the name is looked up exactly once.
func TestRebindingTheConnectionGoesToTheCheckedAddressAndTheNameIsLookedUpOnce(t *testing.T) {
	e := newEnv(t, routes{"/": html(articleBody)})
	e.res.add("rebind.test", "93.184.216.34", "127.0.0.1")
	e.trackDials()
	// robots.txt is its own connection with its own lookup; settle it so that the page is the only connection
	e.r.robotsMap["http://rebind.test:80"] = robotsEntry{r: &robots{}, until: time.Now().Add(time.Hour)}
	p, err := e.r.Read(context.Background(), "http://rebind.test/")
	if err != nil || !strings.Contains(p.Text, "parcels") {
		t.Fatalf("%v", err)
	}
	if got := e.dials(); len(got) != 1 || got[0] != "93.184.216.34:80" {
		t.Errorf("dialed %v, want only the checked address", got)
	}
	if n := e.res.lookups("rebind.test"); n != 1 {
		t.Errorf("%d lookups, want 1", n)
	}
}

func TestEveryConnectionIsResolvedOnceAndConnectedToWhatWasChecked(t *testing.T) {
	// robots.txt with a body, so that its connection could be reused for the page if the transport kept it
	e := newEnv(t, routes{"/": html(articleBody), "/robots.txt": func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "User-agent: *\nDisallow: /nothing\n")
	}})
	e.res.add("two.test", "93.184.216.34")
	e.trackDials()
	if _, err := e.r.Read(context.Background(), "http://two.test/"); err != nil {
		t.Fatal(err)
	}
	// robots.txt and the page
	if got := e.dials(); len(got) != 2 || e.res.lookups("two.test") != 2 {
		t.Errorf("dialed %v after %d lookups, want 2 and 2", got, e.res.lookups("two.test"))
	}
	for _, d := range e.dials() {
		if d != "93.184.216.34:80" {
			t.Errorf("dialed %s", d)
		}
	}
}

func TestARedirectToAPrivateAddressIsRefusedAndNeverFollowed(t *testing.T) {
	e := newEnv(t, routes{
		"/inward":  redirect("http://10.0.0.1/admin"),
		"/meta":    redirect("http://169.254.169.254/latest/meta-data/"),
		"/byname":  redirect("http://internal.test/"),
		"/v6":      redirect("http://[::1]:80/"),
		"/decimal": redirect("http://2130706433/"),
		"/scheme":  redirect("file:///etc/passwd"),
		"/creds":   redirect("http://u:p@example.com/"),
		"/port":    redirect("http://example.com:8080/"),
	})
	e.res.add("internal.test", "10.9.9.9")
	e.trackDials()
	for _, p := range []string{"/inward", "/meta", "/byname", "/v6", "/decimal", "/scheme", "/creds", "/port"} {
		_, err := e.r.Read(context.Background(), e.url(p))
		if !errors.Is(err, ErrNotPublic) && !errors.Is(err, addr.ErrRefused) {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, d := range e.dials() { // the first address of each read is the test origin; nothing else may be dialed
		if d != e.srv.Listener.Addr().String() {
			t.Errorf("dialed %s", d)
		}
	}
}

func chain(n int) routes {
	m := routes{"/end": html(articleBody)}
	for i := 0; i < n; i++ {
		next := fmt.Sprintf("/r%d", i+1)
		if i == n-1 {
			next = "/end"
		}
		m[fmt.Sprintf("/r%d", i)] = redirect(next)
	}
	return m
}

func TestFiveRedirectsAreFollowedAndTheFinalAddressIsReported(t *testing.T) {
	e := newEnv(t, chain(5))
	p, err := e.r.Read(context.Background(), e.url("/r0"))
	if err != nil || p.FinalURL != e.url("/end") {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestAChainOfSixRedirectsIsRefused(t *testing.T) {
	e := newEnv(t, chain(6))
	_, err := e.r.Read(context.Background(), e.url("/r0"))
	is(t, err, ErrTooManyRedirects)
	if e.hits.count("/end") != 0 {
		t.Error("the sixth redirect was followed")
	}
	loop := routes{"/loop": redirect("/loop")}
	e2 := newEnv(t, loop)
	_, err = e2.r.Read(context.Background(), e2.url("/loop"))
	is(t, err, ErrTooManyRedirects)
}

func TestARelativeRedirectIsResolvedAgainstTheAddressItCameFrom(t *testing.T) {
	e := newEnv(t, routes{"/d/start": redirect("../d/next?q=1"), "/d/next": html(articleBody)})
	p, err := e.r.Read(context.Background(), e.url("/d/start"))
	if err != nil || p.FinalURL != e.url("/d/next?q=1") {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestABodyOverFiveMiBIsRefusedWhetherOrNotItsLengthIsAnnounced(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxBytes+1)
	e := newEnv(t, routes{
		"/sized": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Write(big)
		},
		"/chunked": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.(http.Flusher).Flush() // no Content-Length
			w.Write(big)
		},
		"/exact": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Write(big[:MaxBytes])
		},
	})
	for _, p := range []string{"/sized", "/chunked"} {
		_, err := e.r.Read(context.Background(), e.url(p))
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if p, err := e.r.Read(context.Background(), e.url("/exact")); err != nil || p.Bytes != MaxBytes {
		t.Errorf("a page of exactly 5 MiB: %v", err)
	}
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func TestACompressionBombIsRefusedByItsDecodedSize(t *testing.T) {
	bomb := gz(t, bytes.Repeat([]byte("a"), 64<<20))
	if len(bomb) > MaxBytes/10 {
		t.Fatalf("the bomb is %d bytes: not a bomb", len(bomb))
	}
	justOver := gz(t, bytes.Repeat([]byte("a"), MaxBytes+1))
	justUnder := gz(t, bytes.Repeat([]byte("a"), MaxBytes))
	serve := func(b []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(b)
		}
	}
	e := newEnv(t, routes{"/bomb": serve(bomb), "/over": serve(justOver), "/under": serve(justUnder)})
	for _, p := range []string{"/bomb", "/over"} {
		if _, err := e.r.Read(context.Background(), e.url(p)); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if p, err := e.r.Read(context.Background(), e.url("/under")); err != nil || p.Bytes != MaxBytes {
		t.Errorf("a decoded size of exactly 5 MiB: %v", err)
	}
}

func TestABodyThatArrivesTooSlowlyIsRefusedAtTheDeadline(t *testing.T) {
	e := newEnv(t, routes{"/slow": func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}})
	e.r.timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := e.r.Read(context.Background(), e.url("/slow"))
	is(t, err, ErrTooSlow)
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestAServerThatNeverAnswersIsRefusedAtTheDeadline(t *testing.T) {
	e := newEnv(t, routes{"/hang": func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }})
	e.r.timeout = 300 * time.Millisecond
	_, err := e.r.Read(context.Background(), e.url("/hang"))
	is(t, err, ErrTooSlow)
}

func TestTheCallersCancellationIsReturnedAsItIs(t *testing.T) {
	e := newEnv(t, routes{"/hang": func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := e.r.Read(ctx, e.url("/hang"))
	is(t, err, context.Canceled)
}

func TestContentTypesThatAreNotTextAreRefusedWithTheirOwnMessages(t *testing.T) {
	typed := func(ct, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if ct != "" {
				w.Header()["Content-Type"] = []string{ct}
			} else {
				w.Header()["Content-Type"] = nil
			}
			io.WriteString(w, body)
		}
	}
	e := newEnv(t, routes{
		"/zip":  typed("application/zip", "PK\x03\x04"),
		"/json": typed("application/json", "{}"),
		"/none": typed("", "\x00\x01\x02\x03binary without a type"),
		"/xhtml": typed("application/xhtml+xml", `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>X</title></head><body><p>`+
			strings.Repeat("An xhtml page with words in it, long enough to be a paragraph of the article. ", 3)+`</p></body></html>`),
	})
	_, err := e.r.Read(context.Background(), e.url("/zip"))
	is(t, err, ErrType)
	if err.Error() != "content type application/zip is not read" {
		t.Errorf("message %q", err)
	}
	for _, p := range []string{"/json", "/none"} {
		if _, err := e.r.Read(context.Background(), e.url(p)); !errors.Is(err, ErrType) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if p, err := e.r.Read(context.Background(), e.url("/xhtml")); err != nil || p.Type != "application/xhtml+xml" || !strings.Contains(p.Text, "xhtml page") {
		t.Errorf("%+v %v", p, err)
	}
}

func TestOnlyGzipAndDeflateAreDecodedAndAnythingElseIsRefused(t *testing.T) {
	enc := func(name string, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Encoding", name)
			w.Write(body)
		}
	}
	want := "the decoded text of the page"
	var zl, raw bytes.Buffer
	zw := zlib.NewWriter(&zl)
	zw.Write([]byte(want))
	zw.Close()
	fw, _ := flate.NewWriter(&raw, flate.BestCompression)
	fw.Write([]byte(want))
	fw.Close()
	e := newEnv(t, routes{
		"/gzip": enc("gzip", gz(t, []byte(want))), "/xgzip": enc("X-Gzip", gz(t, []byte(want))),
		"/deflate": enc("deflate", zl.Bytes()), "/rawdeflate": enc("deflate", raw.Bytes()),
		"/identity": enc("identity", []byte(want)),
		"/br":       enc("br", []byte("whatever")), "/zstd": enc("zstd", []byte("whatever")),
		"/stack":  enc("gzip, br", gz(t, []byte(want))),
		"/broken": enc("gzip", []byte("this is not gzip")), "/brokendeflate": enc("deflate", []byte("this is not deflate at all, really")),
	})
	for _, p := range []string{"/gzip", "/xgzip", "/deflate", "/rawdeflate", "/identity"} {
		if page, err := e.r.Read(context.Background(), e.url(p)); err != nil || page.Text != want {
			t.Errorf("%s: %q %v", p, page.Text, err)
		}
	}
	for _, p := range []string{"/br", "/zstd", "/stack", "/broken", "/brokendeflate"} {
		if _, err := e.r.Read(context.Background(), e.url(p)); !errors.Is(err, ErrEncoding) {
			t.Errorf("%s: %v", p, err)
		}
	}
	// the request said what it accepts, and the transport did not add anything of its own
	h := e.hits.heads[len(e.hits.heads)-1]
	if h.Get("Accept-Encoding") != "gzip, deflate" {
		t.Errorf("Accept-Encoding %q", h.Get("Accept-Encoding"))
	}
}

func TestPagesAreDecodedToUTF8ByTheirDeclaredCharset(t *testing.T) {
	zh := "这是一个用于测试字符集的页面，里面有足够长的中文段落，用来确认解码之后文字没有变成乱码。国务院发布了新的通知。"
	page := func(head string) string {
		return `<!doctype html><html><head>` + head + `<title>通知</title></head><body><article><h1>通知</h1><p>` + zh + `</p><p>` + zh + `</p><p>` + zh + `</p></article></body></html>`
	}
	gbk := func(s string) []byte {
		b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	serve := func(ct string, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.Write(body)
		}
	}
	e := newEnv(t, routes{
		"/header": serve("text/html; charset=gbk", gbk(page(""))),
		"/meta":   serve("text/html", gbk(page(`<meta charset="gb2312">`))),
		"/meta2":  serve("text/html", gbk(page(`<meta http-equiv="Content-Type" content="text/html; charset=GBK">`))),
		"/utf8":   serve("text/html", []byte(page(""))), // nothing declared, valid UTF-8
		"/bom":    serve("text/html", append([]byte("\xef\xbb\xbf"), page("")...)),
		"/plain":  serve("text/plain; charset=gbk", gbk(zh)),
	})
	for _, p := range []string{"/header", "/meta", "/meta2", "/utf8", "/bom"} {
		page, err := e.r.Read(context.Background(), e.url(p))
		if err != nil || !strings.Contains(page.Text, zh) || page.Title != "通知" {
			t.Errorf("%s: %v\n%s", p, err, page.Text)
		}
	}
	if page, err := e.r.Read(context.Background(), e.url("/plain")); err != nil || page.Text != zh {
		t.Errorf("plain: %v %q", err, page.Text)
	}
}

func TestAnErrorStatusIsReportedWithItsCode(t *testing.T) {
	e := newEnv(t, routes{
		"/gone": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "gone", http.StatusGone) },
		"/boom": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
		"/403":  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "forbidden", http.StatusForbidden) },
	})
	for p, code := range map[string]int{"/gone": 410, "/boom": 500, "/403": 403, "/missing": 404} {
		_, err := e.r.Read(context.Background(), e.url(p))
		if !errors.Is(err, ErrStatus) || !strings.Contains(err.Error(), fmt.Sprint(code)) {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestResponseHeadersAreBounded(t *testing.T) {
	e := newEnv(t, routes{"/fat": func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Fat", strings.Repeat("h", 100<<10))
		io.WriteString(w, "x")
	}})
	_, err := e.r.Read(context.Background(), e.url("/fat"))
	is(t, err, ErrConnect)
}

func TestAConnectionThatCannotBeMadeIsReportedWithoutItsInternals(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := l.Addr().String()
	l.Close()
	e := newEnv(t, routes{})
	e.r.cfg.TestOrigins[dead] = true
	e.r.opts.TestOrigins[dead] = true
	e.r.robotsMap["http://"+dead] = robotsEntry{r: &robots{}, until: time.Now().Add(time.Hour)} // so that the page itself is tried
	_, err = e.r.Read(context.Background(), "http://"+dead+"/")
	is(t, err, ErrConnect)
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "refused") {
		t.Errorf("internals in %q", err)
	}
	_, err = e.r.Read(context.Background(), "http://nowhere.test/")
	is(t, err, ErrRobots)
}

// nothing but the listed host:port is exempt from the port and address rules
func TestTestOriginsExemptExactlyTheListedHostAndPortAndNothingElse(t *testing.T) {
	var other atomic.Int32
	e := newEnv(t, routes{
		"/to-same-host-other-port": redirect("http://127.0.0.1:1/"),
		"/to-loopback-80":          redirect("http://127.0.0.1/"),
		"/to-private":              redirect("http://10.1.1.1:" + portOf(t, "x") + "/"),
	})
	other2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { other.Add(1) }))
	defer other2.Close()
	e.res.add("same.test", "127.0.0.1").add("other.test", "127.0.0.1")
	port := portOf(t, e.srv.URL)
	for _, u := range []string{
		other2.URL + "/",                 // another listener on the same host, another port: not listed
		"http://127.0.0.1:1/",            // idem
		"http://127.0.0.1/",              // the same host at the standard port
		"http://same.test/",              // a name that resolves to the exempt address, at port 80
		"http://same.test:" + port + "/", // a name at the exempt port: the pair is not listed
		"http://localhost:" + port + "/", // idem
		e.url("/to-same-host-other-port"),
		e.url("/to-loopback-80"),
		e.url("/to-private"),
	} {
		_, err := e.r.Read(context.Background(), u)
		if err == nil {
			t.Errorf("%s was read", u)
		}
	}
	if other.Load() != 0 {
		t.Error("another listener on the same host was reached")
	}
	// the control: the listed pair itself is read
	if _, err := e.r.Read(context.Background(), e.url("/")); errors.Is(err, ErrNotPublic) || errors.Is(err, addr.ErrRefused) {
		t.Errorf("the test origin was refused: %v", err)
	}
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	if rawURL == "x" {
		return "9999"
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func TestTheAllowHookSeesTheFirstAddressAndEveryRedirectAndCanRefuseThem(t *testing.T) {
	var hitB atomic.Int32
	a := newEnv(t, routes{})
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		hitB.Add(1)
		html(articleBody)(w, r)
	}))
	defer b.Close()
	ap, bp := portOf(t, a.srv.URL), portOf(t, b.URL)
	a.srv.Config.Handler = routes{"/go": redirect("http://b.test:" + bp + "/page"), "/robots.txt": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }}
	a.res.add("a.test", "127.0.0.1").add("b.test", "127.0.0.1")
	a.r.cfg.TestOrigins["a.test:"+ap] = true
	a.r.opts.TestOrigins["a.test:"+ap] = true
	a.r.cfg.TestOrigins["b.test:"+bp] = true
	a.r.opts.TestOrigins["b.test:"+bp] = true
	var seen []string
	a.r.cfg.Allow = func(host string) error {
		seen = append(seen, host)
		return nil
	}
	p, err := a.r.Read(context.Background(), "http://a.test:"+ap+"/go")
	if err != nil || !strings.Contains(p.Text, "parcels") || p.FinalURL != "http://b.test:"+bp+"/page" {
		t.Fatalf("%+v %v", p, err)
	}
	if strings.Join(seen, ",") != "a.test,b.test" {
		t.Errorf("the hook saw %v", seen)
	}
	// a refusal by the hook on the redirect: nothing is sent to the second host
	hitB.Store(0)
	refused := errors.New("the domain is not on the tenant's list")
	a.r.cfg.Allow = func(host string) error {
		if host == "b.test" {
			return refused
		}
		return nil
	}
	a.r.robotsMap = map[string]robotsEntry{}
	_, err = a.r.Read(context.Background(), "http://a.test:"+ap+"/go")
	is(t, err, refused)
	if hitB.Load() != 0 {
		t.Error("the second host was contacted after the hook refused it")
	}
	// and on the first address
	a.r.cfg.Allow = func(host string) error { return refused }
	if _, err = a.r.Read(context.Background(), "http://a.test:"+ap+"/go"); !errors.Is(err, refused) {
		t.Errorf("%v", err)
	}
}

// The service connects directly. A proxy from the environment would resolve and connect by itself, which would make
// the check on the address meaningless (plan ADR-0003 appendix C, 2 item 8).
func TestProxyEnvironmentVariablesAreIgnored(t *testing.T) {
	var viaProxy atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viaProxy.Add(1)
		http.Error(w, "the proxy was used", http.StatusBadGateway)
	}))
	defer proxy.Close()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(k, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	e := newEnv(t, routes{"/": html(articleBody)})
	// a name that is not loopback, which the standard library would hand to the proxy
	port := portOf(t, e.srv.URL)
	e.res.add("origin.test", "127.0.0.1")
	e.r.cfg.TestOrigins["origin.test:"+port] = true
	e.r.opts.TestOrigins["origin.test:"+port] = true
	p, err := e.r.Read(context.Background(), "http://origin.test:"+port+"/")
	if err != nil || !strings.Contains(p.Text, "parcels") {
		t.Fatalf("%v", err)
	}
	if viaProxy.Load() != 0 {
		t.Errorf("%d requests went through the proxy", viaProxy.Load())
	}
	if e.r.transport.Proxy != nil {
		t.Error("the transport has a proxy function")
	}
}

func TestTLSVerifiesTheHostNameAndTheChain(t *testing.T) {
	srv := httptest.NewTLSServer(routes{"/": html(articleBody)})
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	port := portOf(t, srv.URL)
	mk := func(roots *x509.CertPool) *env {
		e := newEnv(t, routes{})
		e.r = New(Config{Resolver: e.res, TestOrigins: map[string]bool{"127.0.0.1:" + port: true, "wrong.test:" + port: true}, RootCAs: roots, Log: e.r.log})
		e.res.add("wrong.test", "127.0.0.1")
		return e
	}
	// robots.txt is its own connection; settle it so that the page is what meets the certificate
	settle := func(e *env) *env {
		for _, o := range []string{"https://127.0.0.1:" + port, "https://wrong.test:" + port} {
			e.r.robotsMap[o] = robotsEntry{r: &robots{}, until: time.Now().Add(time.Hour)}
		}
		return e
	}
	e := settle(mk(pool))
	if p, err := e.r.Read(context.Background(), srv.URL+"/"); err != nil || !strings.Contains(p.Text, "parcels") {
		t.Fatalf("a valid certificate: %v", err)
	}
	// the certificate is for 127.0.0.1 and example.com, not for this name
	_, err := e.r.Read(context.Background(), "https://wrong.test:"+port+"/")
	is(t, err, ErrConnect)
	// and a chain that does not lead to the roots
	_, err = settle(mk(x509.NewCertPool())).r.Read(context.Background(), srv.URL+"/")
	is(t, err, ErrConnect)
}

// The check in the dial does not rest on the parser having refused first: asked directly, it refuses too.
func TestTheCheckedDialRefusesWhatTheParserWouldHaveRefusedItself(t *testing.T) {
	e := newEnv(t, routes{})
	e.res.add("internal.test", "10.0.0.9").add("fine.test", "93.184.216.34")
	e.trackDials()
	for _, a := range []string{"127.0.0.1:80", "10.0.0.1:443", "[::1]:80", "169.254.169.254:80", "internal.test:80", "fine.test:22", "fine.test:8080", "127.0.0.1:" + portOf(t, e.srv.URL) + "0"} {
		if c, err := e.r.checkedDial(context.Background(), "tcp", a); !errors.Is(err, ErrNotPublic) {
			if c != nil {
				c.Close()
			}
			t.Errorf("%s: %v", a, err)
		}
	}
	if len(e.dials()) != 0 {
		t.Errorf("dialed %v", e.dials())
	}
	// the listed test origin is the one thing it connects to without the check
	c, err := e.r.checkedDial(context.Background(), "tcp", e.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// The Reader is shared by every tenant, so the tenant's rule arrives with the call: a refusal on the redirect means
// nothing is sent to the second host, and a call without the rule is unaffected.
func TestAnAllowRuleOfTheCallSeesEveryAddressAndCanRefuseARedirect(t *testing.T) {
	var hitB atomic.Int32
	a := newEnv(t, routes{})
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		hitB.Add(1)
		html(articleBody)(w, r)
	}))
	defer b.Close()
	ap, bp := portOf(t, a.srv.URL), portOf(t, b.URL)
	a.srv.Config.Handler = routes{"/go": redirect("http://b.test:" + bp + "/page"), "/robots.txt": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }}
	a.res.add("a.test", "127.0.0.1").add("b.test", "127.0.0.1")
	for _, h := range []string{"a.test:" + ap, "b.test:" + bp} {
		a.r.cfg.TestOrigins[h] = true
		a.r.opts.TestOrigins[h] = true
	}
	refused := errors.New("the domain is not on the tenant's list")
	ctx := WithAllow(context.Background(), func(host string) error {
		if host == "b.test" {
			return refused
		}
		return nil
	})
	_, err := a.r.Read(ctx, "http://a.test:"+ap+"/go")
	is(t, err, refused)
	if hitB.Load() != 0 {
		t.Error("the second host was contacted after the rule refused it")
	}
	if _, err = a.r.Read(context.Background(), "http://a.test:"+ap+"/go"); err != nil {
		t.Errorf("a call without the rule: %v", err)
	}
}
