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
// A configuration error is written to stderr and the exit status is 2. The settings for the search service, the
// reference key, the tenants and the reader come with the tasks that need them (platform ADR-0003 appendix C, 4.8).
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
	"sync"
	"syscall"
	"time"

	"idp-web/internal/mcp"
	"idp-web/internal/tools"
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
	mu     sync.Mutex
	counts map[[2]string]int64
	nanos  map[string]int64
}

func (m *metrics) Observe(tool, outcome string, d time.Duration) {
	m.mu.Lock()
	m.counts[[2]string{tool, outcome}]++
	m.nanos[tool] += d.Nanoseconds()
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

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
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

	met := &metrics{counts: map[[2]string]int64{}, nanos: map[string]int64{}}
	srv := mcp.New("idp-web", version,
		"Searches the web and reads pages: web_search returns results with a ref, web_fetch reads a result with its ref, web_open_url reads an address the member gave and needs the member's confirmation.",
		tools.New(log, met, requireCaller))
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
	// nothing is behind the service yet (no search service, no reader), so it is ready when it is up
	ops.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ready") })
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
