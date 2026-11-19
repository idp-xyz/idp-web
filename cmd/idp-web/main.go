// idp-web: an MCP server that lets agents search the web and read pages (ADR-0001; platform ADR-0003).
//
// It is meant to sit behind the idp-mcp gateway as an upstream on a private network: the gateway authenticates,
// authorizes and audits every call, and signs who the caller is. Configuration is by environment:
//
//	IDP_WEB_LISTEN            address of /mcp (default 127.0.0.1:8580); a non-loopback address needs
//	                          IDP_WEB_PRIVATE_NETWORK=true (the operator's statement that only the gateway reaches it)
//	IDP_WEB_METRICS           address of /healthz /readyz /metrics (default 127.0.0.1:9580)
//	IDP_WEB_REQUIRE_CALLER    default true: a request must carry the caller the gateway forwards (register the server
//	                          in idp-mcp with identity=forward); false only for a standalone development run
//	IDP_WEB_CALLER_KEYS_FILE  PEM public key(s) of the gateway (P-256): every tools/call must then carry the gateway's
//	                          assertion over the caller (ADR-0001 D2; the gateway's IDP_GATEWAY_CALLER_KEY_FILE), signed for
//	IDP_WEB_CALLER_AUDIENCE   this server's endpoint as registered in idp-mcp, for example http://web:8580/mcp.
//	                          Set both or neither. Neither: the forwarded caller is taken as it arrives, and only the
//	                          private network vouches for it
//
//	IDP_WEB_PROVIDER          the search service: fixture, exa or brave. Required
//	IDP_WEB_EXA_KEY_FILE      exa: file holding the API key (required for exa)
//	IDP_WEB_EXA_BASE_URL      exa: the API's address, https only (default https://api.exa.ai; the EU endpoint is
//	                          https://api.eu.exa.ai)
//	IDP_WEB_EXA_TYPE          exa: auto (default) or lite
//	IDP_WEB_BRAVE_KEY_FILE    brave: file holding the API key (required for brave)
//	IDP_WEB_FIXTURE_FILE      fixture: the YAML file of query matches and results, for development and acceptance
//	                          (no network, no key)
//	IDP_WEB_REF_KEY_FILE      the keys that sign and verify refs, one a line, each at least 32 bytes: the first signs,
//	                          all verify, so a key is rotated by adding the new one in front. Required
//	IDP_WEB_TENANTS_FILE      the YAML file that says which tenants may use the tools, with which domain lists, and
//	                          whether web_open_url is on. Read once at start. Required
//	IDP_WEB_LIMIT_SEARCHES    searches a caller may make in the window (default 30)
//	IDP_WEB_LIMIT_READS       page reads that go out to the web a caller may make in the window (default 30)
//	IDP_WEB_LIMIT_WINDOW_S    the window in seconds (default 600)
//
//	IDP_WEB_TEST_ORIGINS      for tests only: comma-separated host:port pairs the reader may contact although they are
//	                          not public addresses or not on port 80 or 443 (ADR-0001 D14). Nothing else is exempted
//
// `idp-web -check-tls https://host/` reads one page through the reader and exits; it shows that TLS verifies from
// inside the image, which has no CA certificates of its own (ADR-0001 D13).
//
// Keys are read from files, never from the environment. A configuration error, including a missing provider, key file
// or tenants file, is written to stderr and the exit status is 2 (platform ADR-0003 appendix C, 4.8). /readyz answers
// 200 only when the provider, the ref key and the tenants file are loaded.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"idp-web/internal/mcp"
	"idp-web/internal/tools"

	// The image is FROM scratch and holds no CA certificates. This import registers Mozilla's roots as the fallback of
	// crypto/x509, which Go uses when the system has none (ADR-0001 D13).
	_ "golang.org/x/crypto/x509roots/fallback"
)

// version is stamped by the build (deploy/build.sh) and reported in serverInfo.
var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(args ...any) {
	fmt.Fprintln(os.Stderr, args...)
	os.Exit(2)
}

type metrics struct {
	mu      sync.Mutex
	counts  map[[2]string]int64
	nanos   map[string]int64
	refused map[[2]string]int64
	failed  map[[2]string]int64
}

func (m *metrics) Observe(tool, outcome string, d time.Duration) {
	m.mu.Lock()
	m.counts[[2]string{tool, outcome}]++
	m.nanos[tool] += d.Nanoseconds()
	m.mu.Unlock()
}

// Refused counts a call that a rule refused, by tool and reason (ADR-0001 D24).
func (m *metrics) Refused(tool, reason string) {
	m.mu.Lock()
	m.refused[[2]string{tool, reason}]++
	m.mu.Unlock()
}

// Failed counts a call that the web or the search service did not answer, by tool and reason (ADR-0001 D24, D27). Its
// needs_script share of web_fetch is the number that decides whether a renderer is built.
func (m *metrics) Failed(tool, reason string) {
	m.mu.Lock()
	if m.failed == nil {
		m.failed = map[[2]string]int64{}
	}
	m.failed[[2]string{tool, reason}]++
	m.mu.Unlock()
}

func (m *metrics) write(w http.ResponseWriter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// sorted, so that two scrapes of the same state read the same
	keys := make([][2]string, 0, len(m.counts))
	for k := range m.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	fmt.Fprintln(w, "# TYPE idp_web_calls_total counter")
	for _, k := range keys {
		fmt.Fprintf(w, "idp_web_calls_total{tool=%q,outcome=%q} %d\n", k[0], k[1], m.counts[k])
	}
	fmt.Fprintln(w, "# TYPE idp_web_refused_total counter")
	rk := make([][2]string, 0, len(m.refused))
	for k := range m.refused {
		rk = append(rk, k)
	}
	sort.Slice(rk, func(i, j int) bool {
		if rk[i][0] != rk[j][0] {
			return rk[i][0] < rk[j][0]
		}
		return rk[i][1] < rk[j][1]
	})
	for _, k := range rk {
		fmt.Fprintf(w, "idp_web_refused_total{tool=%q,reason=%q} %d\n", k[0], k[1], m.refused[k])
	}
	fmt.Fprintln(w, "# TYPE idp_web_failed_total counter")
	fk := make([][2]string, 0, len(m.failed))
	for k := range m.failed {
		fk = append(fk, k)
	}
	sort.Slice(fk, func(i, j int) bool {
		if fk[i][0] != fk[j][0] {
			return fk[i][0] < fk[j][0]
		}
		return fk[i][1] < fk[j][1]
	})
	for _, k := range fk {
		fmt.Fprintf(w, "idp_web_failed_total{tool=%q,reason=%q} %d\n", k[0], k[1], m.failed[k])
	}
	fmt.Fprintln(w, "# TYPE idp_web_call_seconds_total counter")
	tn := make([]string, 0, len(m.nanos))
	for k := range m.nanos {
		tn = append(tn, k)
	}
	sort.Strings(tn)
	for _, k := range tn {
		fmt.Fprintf(w, "idp_web_call_seconds_total{tool=%q} %g\n", k, float64(m.nanos[k])/1e9)
	}
}

// readyz answers 200 only when the search provider, the ref key and the tenants file are loaded; otherwise 503 and
// what is missing.
func readyz(w http.ResponseWriter, s *setup) {
	if m := s.missing(); len(m) > 0 {
		http.Error(w, "not ready: "+strings.Join(m, ", ")+" not loaded", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ready")
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) > 1 && os.Args[1] == "-check-tls" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code := checkTLS(ctx, os.Args[2:], os.Stdout, os.Stderr, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
		stop()
		os.Exit(code)
	}
	testOrigins, err := parseTestOrigins(os.Getenv("IDP_WEB_TEST_ORIGINS"))
	if err != nil {
		fatal("invalid IDP_WEB_TEST_ORIGINS:", err)
	}
	if len(testOrigins) > 0 {
		log.Warn("IDP_WEB_TEST_ORIGINS exempts these origins from the address rules: for tests only", "origins", len(testOrigins))
	}
	listen := env("IDP_WEB_LISTEN", "127.0.0.1:8580")
	if host, _, err := net.SplitHostPort(listen); err != nil {
		fatal("invalid IDP_WEB_LISTEN:", err)
	} else if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && os.Getenv("IDP_WEB_PRIVATE_NETWORK") != "true" {
		fatal("refusing to listen on", listen, "without IDP_WEB_PRIVATE_NETWORK=true: this server has no authentication of its own, only the gateway may reach it")
	}
	requireCaller := os.Getenv("IDP_WEB_REQUIRE_CALLER") != "false"
	metricsAddr := env("IDP_WEB_METRICS", "127.0.0.1:9580")
	if _, _, err := net.SplitHostPort(metricsAddr); err != nil {
		fatal("invalid IDP_WEB_METRICS:", err)
	}

	cfg, err := loadSetup(os.Getenv, log)
	if err != nil {
		fatal(err)
	}
	log.Info("configured", "provider", cfg.providerName, "tenants", cfg.tenants.Count(), "searches", cfg.limits.Searches, "reads", cfg.limits.Reads, "windowSeconds", int(cfg.limits.Window.Seconds()))
	met := &metrics{counts: map[[2]string]int64{}, nanos: map[string]int64{}, refused: map[[2]string]int64{}, failed: map[[2]string]int64{}}
	srv := mcp.New("idp-web", version,
		"Searches the web and reads pages: web_search returns results with a ref, web_fetch reads a result with its ref, web_open_url reads an address the member gave and needs the member's confirmation.",
		tools.New(tools.Config{Log: log, Metrics: met, RequireCaller: requireCaller, Provider: cfg.provider, Reader: newReader(log, testOrigins),
			Keys: cfg.keys, Tenants: cfg.tenants, TestOrigins: testOrigins, Limits: cfg.limits}))
	srv.Log = log

	keysFile, audience := os.Getenv("IDP_WEB_CALLER_KEYS_FILE"), os.Getenv("IDP_WEB_CALLER_AUDIENCE")
	switch {
	case keysFile != "":
		b, err := os.ReadFile(keysFile)
		if err == nil {
			srv.Callers, err = mcp.NewCallerVerifier(b, audience)
		}
		if err != nil {
			fatal("IDP_WEB_CALLER_KEYS_FILE / IDP_WEB_CALLER_AUDIENCE:", err)
		}
		log.Info("callers must be signed by the gateway", "audience", audience, "keys", srv.Callers.KeyIDs())
	case audience != "":
		fatal("IDP_WEB_CALLER_AUDIENCE is set without IDP_WEB_CALLER_KEYS_FILE")
	case requireCaller:
		log.Warn("the caller the gateway forwards is not verified: anything that reaches this port can name any caller (set IDP_WEB_CALLER_KEYS_FILE and IDP_WEB_CALLER_AUDIENCE)")
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", srv)
	main := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 90 * time.Second}
	ops := http.NewServeMux()
	ops.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	ops.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { readyz(w, cfg) })
	ops.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { met.write(w) })
	opsSrv := &http.Server{Addr: metricsAddr, Handler: ops, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 2)
	go func() { errc <- main.ListenAndServe() }()
	go func() { errc <- opsSrv.ListenAndServe() }()
	log.Info("idp-web listening", "mcp", listen, "ops", opsSrv.Addr, "version", version)
	exit := 0
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			exit = 1
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = main.Shutdown(shutdown)
	_ = opsSrv.Shutdown(shutdown)
	if exit != 0 {
		cancel()
		os.Exit(exit)
	}
}
