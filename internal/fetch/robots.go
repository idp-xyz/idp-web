package fetch

import (
	"context"
	"errors"
	"strings"
	"time"

	"idp-web/internal/addr"
)

// robots.txt, as RFC 9309 specifies it (ADR-0001 D11). The Python reference (urllib.robotparser) is laxer in the
// places that matter: it takes any unreadable robots.txt as "everything allowed", while the RFC says an unreachable one
// (5xx, network failure) means "assume complete disallow" (2.3.1.4) and only a missing one (4xx) means no restrictions
// (2.3.1.3). A reader that obeys robots.txt only when it is easy to read is a reader a site cannot rely on; the RFC is
// followed here.

const (
	robotsTTL      = time.Hour
	robotsFailTTL  = time.Minute // an answer of "unreachable" is cached briefly, so that a failing site is not asked on every page
	robotsMaxCache = 1024        // origins; bounded so that a stream of distinct hosts cannot grow the cache without limit
)

// robots holds the rules that apply to this reader: the merged groups of the most specific user agent that matched.
type robots struct {
	rules []robotsRule
	// denyAll is the answer for an unreachable robots.txt.
	denyAll bool
}

type robotsRule struct {
	allow   bool
	pattern string // normalized, as written: with * and a final $
}

// parseRobots reads a robots.txt for the product token (matched case-insensitively against the user-agent lines
// that name it exactly). Groups of the token are merged and the "*" groups ignored when any group names the token;
// otherwise the "*" groups apply (RFC 9309 2.2.1). Lines other than user-agent, allow and disallow (sitemap,
// crawl-delay, anything unknown) are ignored and do not end a group.
func parseRobots(body []byte, token string) *robots {
	text := strings.TrimPrefix(string(body), "\xef\xbb\xbf")
	token = strings.ToLower(token)
	var specific, star []robotsRule
	var specificSeen bool
	var agents []string
	inRules := false // a rule line was seen since the last user-agent line: the next user-agent starts a new group
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if inRules {
				agents, inRules = nil, false
			}
			agents = append(agents, strings.ToLower(value))
			for _, a := range agents {
				if a == token {
					specificSeen = true
				}
			}
		case "allow", "disallow":
			inRules = true
			if len(agents) == 0 {
				continue // a rule before any user-agent line belongs to no group
			}
			rule := robotsRule{allow: key == "allow", pattern: normalizePath(value)}
			if rule.pattern == "" {
				continue // an empty Disallow allows everything and an empty Allow says nothing
			}
			for _, a := range agents {
				if a == token {
					specific = append(specific, rule)
					break
				}
			}
			for _, a := range agents {
				if a == "*" {
					star = append(star, rule)
					break
				}
			}
		}
	}
	if specificSeen {
		return &robots{rules: specific}
	}
	return &robots{rules: star}
}

// allowed reports whether the path (with its query) may be fetched. The longest matching pattern decides, and Allow
// wins a tie (RFC 9309 2.2.2); no matching rule means allowed.
func (r *robots) allowed(path string) bool {
	if r.denyAll {
		return false
	}
	path = normalizePath(path)
	best, allow := -1, true
	for _, rule := range r.rules {
		if len(rule.pattern) < best || !matches(rule.pattern, path) {
			continue
		}
		if len(rule.pattern) > best || rule.allow {
			best, allow = len(rule.pattern), rule.allow
		}
	}
	return allow
}

// matches reports whether path starts with pattern, where * matches any run of characters and a final $ anchors the
// pattern at the end of the path.
func matches(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	pattern = strings.TrimSuffix(pattern, "$")
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	if len(parts) == 1 { // no wildcard
		return !anchored || path == pattern
	}
	pos := len(parts[0])
	for _, part := range parts[1 : len(parts)-1] {
		j := strings.Index(path[pos:], part) // the first occurrence leaves the most path for the pieces that follow
		if j < 0 {
			return false
		}
		pos += j + len(part)
	}
	last := parts[len(parts)-1]
	if anchored {
		return len(path)-pos >= len(last) && strings.HasSuffix(path, last)
	}
	return strings.Contains(path[pos:], last)
}

// normalizePath puts a path or a pattern into the one form both are compared in (RFC 9309 2.2.2): bytes outside ASCII
// are percent-encoded, escapes of unreserved characters are decoded and the hex digits of the others are upper case.
func normalizePath(s string) string {
	var b strings.Builder
	const hexd = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			v := unhex(s[i+1])<<4 | unhex(s[i+2])
			if isUnreserved(v) {
				b.WriteByte(v)
			} else {
				b.WriteByte('%')
				b.WriteByte(hexd[v>>4])
				b.WriteByte(hexd[v&15])
			}
			i += 2
		case c >= 0x80 || c <= 0x20 || c == 0x7f:
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&15])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

type robotsEntry struct {
	r     *robots
	until time.Time
}

// robotsAllow says whether the page at t may be read, fetching the origin's robots.txt through the same checked path
// as a page (follow) unless it is cached.
func (r *Reader) robotsAllow(ctx context.Context, t addr.Address) (bool, error) {
	origin := t.Scheme + "://" + t.HostPort()
	r.robotsMu.Lock()
	e, ok := r.robotsMap[origin]
	r.robotsMu.Unlock()
	if !ok || !r.now().Before(e.until) {
		var err error
		if e, err = r.loadRobots(ctx, t, origin); err != nil {
			return false, err
		}
	}
	if e.r.denyAll {
		return false, wrap(ErrRobots, "the site's robots.txt could not be read (no answer or a server error), so the page was not read")
	}
	return e.r.allowed(t.Path), nil
}

func (r *Reader) loadRobots(ctx context.Context, t addr.Address, origin string) (robotsEntry, error) {
	rt := t
	rt.Path = "/robots.txt"
	resp, _, err := r.follow(ctx, rt.URL(), func(ctx context.Context, hop int, n addr.Address) error {
		// the first address is the origin the page is on, which the caller's gate has just passed
		if hop > 0 && r.cfg.Allow != nil {
			return r.cfg.Allow(n.Host)
		}
		return nil
	})
	entry := robotsEntry{until: r.now().Add(robotsTTL)}
	switch {
	case err != nil && ctx.Err() != nil:
		return entry, ctx.Err()
	case errors.Is(err, ErrTooManyRedirects):
		entry.r = &robots{} // RFC 9309 2.3.1.2: treated as unavailable, like a 4xx
	case errors.Is(err, ErrNotPublic), errors.Is(err, addr.ErrRefused), errors.Is(err, ErrBotCheck):
		// the origin may not be contacted at all, which is the answer, and not a statement about its robots.txt
		return entry, err
	case err != nil:
		// RFC 9309 2.3.1.4: a robots.txt that cannot be reached means the site is not to be crawled
		r.log.Info("robots.txt unreachable, reading nothing from the origin", "origin", origin, "err", err)
		entry.r, entry.until = &robots{denyAll: true}, r.now().Add(robotsFailTTL)
	default:
		defer resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			body, rerr := readBody(resp, maxRobots, true)
			if rerr != nil {
				if ctx.Err() != nil {
					return entry, ctx.Err()
				}
				r.log.Info("robots.txt unreadable, reading nothing from the origin", "origin", origin, "err", rerr)
				entry.r, entry.until = &robots{denyAll: true}, r.now().Add(robotsFailTTL)
			} else {
				entry.r = parseRobots(body, ProductToken)
			}
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			entry.r = &robots{} // RFC 9309 2.3.1.3: no robots.txt, no restrictions
		default: // 5xx, and anything else that is not a file
			entry.r, entry.until = &robots{denyAll: true}, r.now().Add(robotsFailTTL)
		}
	}
	r.robotsMu.Lock()
	defer r.robotsMu.Unlock()
	if len(r.robotsMap) >= robotsMaxCache {
		r.evictRobots()
	}
	r.robotsMap[origin] = entry
	return entry, nil
}

// evictRobots drops the expired entries and, when the cache is still full, the one that expires first. The caller holds the lock.
func (r *Reader) evictRobots() {
	now := r.now()
	var oldest string
	var oldestAt time.Time
	for k, e := range r.robotsMap {
		if !now.Before(e.until) {
			delete(r.robotsMap, k)
			continue
		}
		if oldest == "" || e.until.Before(oldestAt) {
			oldest, oldestAt = k, e.until
		}
	}
	if len(r.robotsMap) >= robotsMaxCache && oldest != "" {
		delete(r.robotsMap, oldest)
	}
}
