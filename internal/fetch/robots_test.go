package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRobotsRulesFollowRFC9309(t *testing.T) {
	type q struct {
		path string
		want bool
	}
	for _, c := range []struct {
		name, file string
		asks       []q
	}{
		{"no file content allows everything", ``, []q{{"/", true}, {"/a", true}}},
		{"disallow a prefix", "User-agent: *\nDisallow: /private", []q{{"/private", false}, {"/private/x", false}, {"/privateer", false}, {"/public", true}, {"/", true}}},
		{"an empty disallow allows everything", "User-agent: *\nDisallow:", []q{{"/", true}, {"/a", true}}},
		{"disallow everything", "User-agent: *\nDisallow: /", []q{{"/", false}, {"/a/b?c=d", false}}},
		{"the longest match wins: allow inside a disallowed directory", "User-agent: *\nDisallow: /a/\nAllow: /a/b/", []q{{"/a/x", false}, {"/a/b/x", true}, {"/a/b", false}}},
		{"the longest match wins: disallow inside an allowed directory", "User-agent: *\nAllow: /a/\nDisallow: /a/b/", []q{{"/a/x", true}, {"/a/b/x", false}}},
		{"allow wins a tie", "User-agent: *\nDisallow: /page\nAllow: /page", []q{{"/page", true}}},
		{"allow wins a tie whichever line comes first", "User-agent: *\nAllow: /page\nDisallow: /page", []q{{"/page", true}}},
		{"the rule does not depend on the order of the lines", "User-agent: *\nAllow: /a/b/\nDisallow: /a/", []q{{"/a/b/x", true}, {"/a/x", false}}},
		{"a wildcard matches any run of characters", "User-agent: *\nDisallow: /*/secret", []q{{"/x/secret", false}, {"/x/y/secret/z", false}, {"/secret", true}, {"/x/public", true}}},
		{"a wildcard at the end changes nothing", "User-agent: *\nDisallow: /tmp*", []q{{"/tmp", false}, {"/tmpfile", false}, {"/t", true}}},
		{"a dollar anchors the end", "User-agent: *\nDisallow: /*.pdf$", []q{{"/a.pdf", false}, {"/a/b.pdf", false}, {"/a.pdf?x=1", true}, {"/a.pdfx", true}, {"/a.html", true}}},
		{"a dollar without a wildcard", "User-agent: *\nDisallow: /exact$", []q{{"/exact", false}, {"/exact/", true}, {"/exactly", true}}},
		{"a wildcard and a dollar together", "User-agent: *\nDisallow: /a*b*c$", []q{{"/abc", false}, {"/aXbYc", false}, {"/aXbYcZ", true}, {"/ab", true}}},
		{"a more specific allow with a dollar", "User-agent: *\nDisallow: /dir/\nAllow: /dir/$", []q{{"/dir/", true}, {"/dir/x", false}}},
		{"the query is part of the path", "User-agent: *\nDisallow: /search?", []q{{"/search?q=1", false}, {"/search", true}}},
		{"case matters in paths", "User-agent: *\nDisallow: /Private", []q{{"/Private", false}, {"/private", true}}},
		{"keys and the user agent are case-insensitive", "USER-AGENT: *\nDISALLOW: /x", []q{{"/x", false}}},
		{"comments and blank lines are ignored", "# a comment\n\nUser-agent: *  # all\nDisallow: /x # no\n", []q{{"/x", false}, {"/y", true}}},
		{"CRLF line ends", "User-agent: *\r\nDisallow: /x\r\n", []q{{"/x", false}}},
		{"a byte order mark", "\xef\xbb\xbfUser-agent: *\nDisallow: /x", []q{{"/x", false}}},
		{"our group wins over the star group", "User-agent: *\nDisallow: /\n\nUser-agent: idp-web\nDisallow: /secret", []q{{"/", true}, {"/secret", false}}},
		{"our group wins whichever comes first", "User-agent: idp-web\nDisallow: /secret\n\nUser-agent: *\nDisallow: /", []q{{"/", true}, {"/secret", false}}},
		{"the token is matched without regard to case", "User-agent: IDP-Web\nDisallow: /secret\nUser-agent: *\nDisallow: /", []q{{"/", true}, {"/secret", false}}},
		{"an empty group of ours frees us of the star group", "User-agent: idp-web\nDisallow:\n\nUser-agent: *\nDisallow: /", []q{{"/", true}, {"/x", true}}},
		{"several user agents share one group", "User-agent: googlebot\nUser-agent: idp-web\nDisallow: /g\n\nUser-agent: *\nDisallow: /", []q{{"/g", false}, {"/x", true}}},
		{"two groups of ours are merged", "User-agent: idp-web\nDisallow: /a\n\nUser-agent: other\nDisallow: /\n\nUser-agent: idp-web\nDisallow: /b", []q{{"/a", false}, {"/b", false}, {"/c", true}}},
		{"another agent's group does not apply to us", "User-agent: googlebot\nDisallow: /", []q{{"/", true}}},
		{"a longer name is not our token", "User-agent: idp-web-extra\nDisallow: /", []q{{"/", true}}},
		{"unknown lines do not end a group", "User-agent: *\nCrawl-delay: 10\nSitemap: https://x/s.xml\nDisallow: /x", []q{{"/x", false}}},
		{"a rule before any user agent belongs to no group", "Disallow: /x\nUser-agent: *\nDisallow: /y", []q{{"/x", true}, {"/y", false}}},
		{"a user agent line after rules starts a new group", "User-agent: a\nDisallow: /a\nUser-agent: *\nDisallow: /star", []q{{"/a", true}, {"/star", false}}},
		{"percent-encoding is compared in its normal form", "User-agent: *\nDisallow: /a%2fb\nDisallow: /%7euser\nDisallow: /café", []q{{"/a%2Fb", false}, {"/a%2fb", false}, {"/~user", false}, {"/%7Euser", false}, {"/caf%C3%A9", false}, {"/a/b", true}}},
		{"a line without a colon is ignored", "User-agent: *\nthis is nothing\nDisallow: /x", []q{{"/x", false}}},
	} {
		r := parseRobots([]byte(c.file), ProductToken)
		for _, a := range c.asks {
			if got := r.allowed(a.path); got != a.want {
				t.Errorf("%s: %s allowed=%v, want %v", c.name, a.path, got, a.want)
			}
		}
	}
}

func robotsServer(t *testing.T, robots http.HandlerFunc) *env {
	return newEnv(t, routes{"/robots.txt": robots, "/ok": html(articleBody), "/private/page": html(articleBody)})
}

func serveRobots(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func TestRobotsTxtDecidesWhetherAPageIsRead(t *testing.T) {
	e := robotsServer(t, serveRobots(200, "User-agent: *\nDisallow: /private\n"))
	_, err := e.r.Read(context.Background(), e.url("/private/page"))
	is(t, err, ErrRobots)
	if e.hits.count("/private/page") != 0 {
		t.Error("the page was requested although robots.txt disallows it")
	}
	if _, err := e.r.Read(context.Background(), e.url("/ok")); err != nil {
		t.Errorf("an allowed page: %v", err)
	}
}

func TestRobotsTxtOfFourHundredsMeansNoRestrictionsAndOfFiveHundredsOrNoAnswerMeansNoReading(t *testing.T) {
	for _, c := range []struct {
		name    string
		robots  http.HandlerFunc
		allowed bool
	}{
		{"404", serveRobots(404, "User-agent: *\nDisallow: /"), true},
		{"403", serveRobots(403, "User-agent: *\nDisallow: /"), true},
		{"401", serveRobots(401, ""), true},
		{"410", serveRobots(410, ""), true},
		{"500", serveRobots(500, "User-agent: *\nDisallow:"), false},
		{"503", serveRobots(503, ""), false},
		{"502", serveRobots(502, ""), false},
		{"the connection is cut", func(w http.ResponseWriter, _ *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
		}, false},
		{"a corrupt answer", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			fmt.Fprint(w, "not gzip")
		}, false},
		{"empty file", serveRobots(200, ""), true},
	} {
		e := robotsServer(t, c.robots)
		_, err := e.r.Read(context.Background(), e.url("/ok"))
		if c.allowed && err != nil || !c.allowed && !errors.Is(err, ErrRobots) {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.allowed && e.hits.count("/ok") != 0 {
			t.Errorf("%s: the page was requested", c.name)
		}
	}
}

func TestRobotsTxtIsFetchedThroughTheSameCheckedPath(t *testing.T) {
	// the file redirects to another place on the same origin: followed
	e := newEnv(t, routes{"/robots.txt": redirect("/rules.txt"), "/rules.txt": serveRobots(200, "User-agent: *\nDisallow: /ok"), "/ok": html(articleBody)})
	_, err := e.r.Read(context.Background(), e.url("/ok"))
	is(t, err, ErrRobots)
	// and one that redirects inward is not followed: the origin is then not read
	e2 := newEnv(t, routes{"/robots.txt": redirect("http://10.0.0.1/robots.txt"), "/ok": html(articleBody)})
	_, err = e2.r.Read(context.Background(), e2.url("/ok"))
	is(t, err, ErrNotPublic)
	if e2.hits.count("/ok") != 0 {
		t.Error("the page was requested")
	}
	// a robots.txt of a name that resolves privately: the page's own address is refused with the true reason
	e3 := newEnv(t, routes{})
	e3.res.add("private.test", "10.2.2.2")
	_, err = e3.r.Read(context.Background(), "http://private.test/")
	is(t, err, ErrNotPublic)
}

func TestRobotsTxtIsCachedPerOriginForAnHourAndFailuresForAMinute(t *testing.T) {
	var calls int
	e := robotsServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		serveRobots(200, "User-agent: *\nDisallow: /nothing")(w, r)
	})
	now := time.Now()
	e.r.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, err := e.r.Read(context.Background(), e.url("/ok")); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("robots.txt was fetched %d times for 3 pages", calls)
	}
	now = now.Add(59 * time.Minute)
	e.r.Read(context.Background(), e.url("/ok"))
	if calls != 1 {
		t.Errorf("fetched again before the hour: %d", calls)
	}
	now = now.Add(2 * time.Minute)
	e.r.Read(context.Background(), e.url("/ok"))
	if calls != 2 {
		t.Errorf("not fetched again after the hour: %d", calls)
	}
	// an unreachable robots.txt is remembered for a minute only
	var fails int
	f := robotsServer(t, func(w http.ResponseWriter, r *http.Request) { fails++; serveRobots(503, "")(w, r) })
	f.r.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		f.r.Read(context.Background(), f.url("/ok"))
	}
	if fails != 1 {
		t.Errorf("a failing robots.txt was asked %d times for 3 pages", fails)
	}
	now = now.Add(61 * time.Second)
	f.r.Read(context.Background(), f.url("/ok"))
	if fails != 2 {
		t.Errorf("a failing robots.txt was not asked again after a minute: %d", fails)
	}
}

func TestTheRobotsCacheIsBounded(t *testing.T) {
	e := newEnv(t, routes{})
	now := time.Now()
	e.r.now = func() time.Time { return now }
	for i := 0; i < robotsMaxCache*3; i++ {
		e.r.robotsMu.Lock()
		if len(e.r.robotsMap) >= robotsMaxCache {
			e.r.evictRobots()
		}
		e.r.robotsMap[fmt.Sprintf("http://h%d.test:80", i)] = robotsEntry{r: &robots{}, until: now.Add(time.Hour + time.Duration(i)*time.Second)}
		e.r.robotsMu.Unlock()
		if len(e.r.robotsMap) > robotsMaxCache {
			t.Fatalf("%d entries after %d origins", len(e.r.robotsMap), i+1)
		}
	}
	// through the real path: many origins in a row never grow the map past the bound
	e2 := newEnv(t, routes{"/ok": html(articleBody)})
	for i := 0; i < robotsMaxCache; i++ { // the cache is full
		e2.r.robotsMu.Lock()
		e2.r.robotsMap[fmt.Sprintf("http://fill%d.test:80", i)] = robotsEntry{r: &robots{}, until: time.Now().Add(time.Hour)}
		e2.r.robotsMu.Unlock()
	}
	if _, err := e2.r.Read(context.Background(), e2.url("/ok")); err != nil {
		t.Fatal(err)
	}
	if n := len(e2.r.robotsMap); n > robotsMaxCache {
		t.Errorf("%d entries, bound %d", n, robotsMaxCache)
	}
}

func TestRobotsTxtIsReadUpToItsLimitAndNotRefusedForBeingLarger(t *testing.T) {
	// a rule in the first 512 KiB counts; what comes after is not read
	filler := strings.Repeat("# padding padding padding padding padding\n", 14000) // about 600 KiB
	e := robotsServer(t, serveRobots(200, "User-agent: *\nDisallow: /private\n"+filler+"Disallow: /ok\n"))
	_, err := e.r.Read(context.Background(), e.url("/private/page"))
	is(t, err, ErrRobots)
	if _, err := e.r.Read(context.Background(), e.url("/ok")); err != nil {
		t.Errorf("a rule beyond the first 512 KiB was applied: %v", err)
	}
}

func TestRobotsTxtIsAskedWithTheProductTokenOfTheUserAgent(t *testing.T) {
	e := robotsServer(t, serveRobots(404, ""))
	e.r.Read(context.Background(), e.url("/ok"))
	if ua := e.hits.heads[0].Get("User-Agent"); !strings.HasPrefix(ua, ProductToken+"/") {
		t.Errorf("user agent %q does not start with the token robots.txt groups address", ua)
	}
}
