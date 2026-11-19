package tools

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-web/internal/mcp"
)

type counting struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *counting) Observe(tool, outcome string, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[tool+"/"+outcome]++
}

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}`

// call posts a tools/call as the given forwarded caller ("" forwards none) and returns the HTTP status and the result.
func call(t *testing.T, s *mcp.Server, caller, tool string, args any) (int, map[string]any) {
	t.Helper()
	a, _ := json.Marshal(args)
	m := meta
	if caller != "" {
		m += fmt.Sprintf(`,"io.idp.telos/caller":{"subject":%q,"tenant":"t1","roles":["member"]}`, caller)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + string(a) + `,` + m + `}}}`
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	res, _ := out["result"].(map[string]any)
	return w.Code, res
}

func server(met Metrics, require bool) *mcp.Server {
	return mcp.New("idp-web", "test", "", New(slog.New(slog.DiscardHandler), met, require))
}

func text(res map[string]any) string {
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestEveryToolHasASchemaTheGatewayCanPin(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range New(slog.New(slog.DiscardHandler), &counting{}, true) {
		names[tool.Name] = true
		if !strings.HasPrefix(tool.Name, "web_") || tool.Description == "" || tool.InputSchema["type"] != "object" || tool.InputSchema["additionalProperties"] != false {
			t.Errorf("%s: %v", tool.Name, tool.InputSchema)
		}
		if _, err := json.Marshal(tool.InputSchema); err != nil {
			t.Errorf("%s: %v", tool.Name, err)
		}
		if tool.Annotations["readOnlyHint"] != true || tool.Annotations["openWorldHint"] != true {
			t.Errorf("%s: annotations %v", tool.Name, tool.Annotations)
		}
	}
	for _, want := range []string{"web_search", "web_fetch", "web_open_url"} {
		if !names[want] {
			t.Errorf("%s is not declared", want)
		}
	}
	if len(names) != 3 {
		t.Errorf("the tools are %v", names)
	}
}

// The schemas are pinned by the gateway, so what they require is part of the contract (platform ADR-0003 D2).
func TestTheSchemasRequireWhatThePlanSays(t *testing.T) {
	want := map[string]struct {
		required []string
		props    []string
	}{
		"web_search":   {[]string{"query"}, []string{"query", "freshness", "count"}},
		"web_fetch":    {[]string{"url", "ref"}, []string{"url", "ref", "offset"}},
		"web_open_url": {[]string{"url"}, []string{"url", "offset"}},
	}
	for _, tool := range New(slog.New(slog.DiscardHandler), &counting{}, true) {
		w := want[tool.Name]
		if fmt.Sprint(tool.InputSchema["required"]) != fmt.Sprint(w.required) {
			t.Errorf("%s requires %v", tool.Name, tool.InputSchema["required"])
		}
		props := tool.InputSchema["properties"].(map[string]any)
		if len(props) != len(w.props) {
			t.Errorf("%s has properties %v", tool.Name, props)
		}
		for _, p := range w.props {
			if props[p] == nil {
				t.Errorf("%s has no property %s", tool.Name, p)
			}
		}
	}
	count := New(slog.New(slog.DiscardHandler), &counting{}, true)[0].InputSchema["properties"].(map[string]any)["count"].(map[string]any)
	if count["minimum"] != 1 || count["maximum"] != 10 {
		t.Errorf("count is %v", count)
	}
}

func TestEveryToolSaysItIsNotAvailableYetToAValidCall(t *testing.T) {
	s := server(&counting{}, true)
	for tool, args := range map[string]map[string]any{
		"web_search":   {"query": "idp", "count": 3, "freshness": "week"},
		"web_fetch":    {"url": "https://example.com/", "ref": "00000000.AAAAAAAAAAAAAAAA", "offset": 0},
		"web_open_url": {"url": "https://example.com/"},
	} {
		code, res := call(t, s, "alice", tool, args)
		if code != 200 || res["isError"] != true || text(res) != ErrNotAvailable.Error() {
			t.Errorf("%s: %d %v", tool, code, res)
		}
	}
}

func TestArgumentsAreDecodedStrictly(t *testing.T) {
	s := server(&counting{}, true)
	good := "https://example.com/"
	for name, c := range map[string]struct {
		tool string
		args map[string]any
	}{
		"an unknown field":                {"web_search", map[string]any{"query": "x", "tenant": "t2"}},
		"a subject as an argument":        {"web_fetch", map[string]any{"url": good, "ref": "r", "subject": "bob"}},
		"no query":                        {"web_search", map[string]any{}},
		"an empty query":                  {"web_search", map[string]any{"query": ""}},
		"a count of 0":                    {"web_search", map[string]any{"query": "x", "count": 0}},
		"a count of 11":                   {"web_search", map[string]any{"query": "x", "count": 11}},
		"a count that is not a number":    {"web_search", map[string]any{"query": "x", "count": "3"}},
		"a freshness that is not offered": {"web_search", map[string]any{"query": "x", "freshness": "decade"}},
		"a fetch without its ref":         {"web_fetch", map[string]any{"url": good}},
		"a fetch with an empty ref":       {"web_fetch", map[string]any{"url": good, "ref": ""}},
		"a negative offset":               {"web_open_url", map[string]any{"url": good, "offset": -1}},
		"an open_url with a ref":          {"web_open_url", map[string]any{"url": good, "ref": "r"}},
		"no address":                      {"web_open_url", map[string]any{}},
		"an address that is far too long": {"web_open_url", map[string]any{"url": "https://example.com/" + strings.Repeat("a", maxURL)}},
		"a query that is far too long":    {"web_search", map[string]any{"query": strings.Repeat("a", maxQuery+1)}},
	} {
		_, res := call(t, s, "alice", c.tool, c.args)
		if res["isError"] != true || !strings.HasPrefix(text(res), ErrInvalid.Error()) {
			t.Errorf("%s: %v", name, res)
		}
	}
}

// The tenant and the subject are the verified caller's. A call that arrives without one is refused with the sentence
// that tells the operator what is missing, and no handler is reached.
func TestACallWithoutACallerIsRefusedWhenOneIsRequired(t *testing.T) {
	met := &counting{}
	_, res := call(t, server(met, true), "", "web_search", map[string]any{"query": "x"})
	if res["isError"] != true || text(res) != ErrNoCaller.Error() {
		t.Fatalf("%v", res)
	}
	// with the requirement off (a development run) the call reaches the handler, which says it is not available
	_, res = call(t, server(met, false), "", "web_search", map[string]any{"query": "x"})
	if text(res) != ErrNotAvailable.Error() {
		t.Fatalf("%v", res)
	}
}

// With a verifier the HTTP layer refuses an unsigned tools/call with 403 before any tool is reached, and the metrics
// show no call; the listing and the discovery need no caller.
func TestWithAVerifierAnUnsignedCallGets403AndNoHandlerRuns(t *testing.T) {
	met := &counting{}
	s := server(met, true)
	v, err := mcp.NewCallerVerifier([]byte("-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEohuazOuwtXVv4bbe+34tuxPRZYT0\nMhfvC5orxYuBtlzQDkcbdyY8ArPLyQmVNAZRh6jp2RnXFF/+CLtd4X/dUA==\n-----END PUBLIC KEY-----\n"), "http://web:8580/mcp")
	if err != nil {
		t.Fatal(err)
	}
	s.Callers = v
	// even a forwarded caller that names somebody is not enough
	code, res := call(t, s, "alice", "web_search", map[string]any{"query": "x"})
	if code != http.StatusForbidden || res != nil || len(met.calls) != 0 {
		t.Fatalf("%d %v, handlers ran: %v", code, res, met.calls)
	}
	for id, method := range []string{"tools/list", "server/discover"} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{%s}}}`, id+1, method, meta)))
		r.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", method, w.Code, w.Body)
		}
		if method == "tools/list" {
			for _, n := range []string{"web_search", "web_fetch", "web_open_url"} {
				if !strings.Contains(w.Body.String(), n) {
					t.Errorf("tools/list does not name %s", n)
				}
			}
		}
	}
}

func TestTheAuditLineNamesTheCallerByHashOnly(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	s := mcp.New("idp-web", "test", "", New(log, &counting{}, true))
	call(t, s, "alice-subject-1234", "web_search", map[string]any{"query": "a secret question"})
	line := buf.String()
	if strings.Contains(line, "alice-subject-1234") || strings.Contains(line, "a secret question") || !strings.Contains(line, `"caller":"`+short("t1/alice-subject-1234")+`"`) || !strings.Contains(line, `"tool":"web_search"`) {
		t.Fatalf("%s", line)
	}
}
