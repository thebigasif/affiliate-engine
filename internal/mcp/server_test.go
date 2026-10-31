package mcp_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/mcp"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _ := newServerDB(t)
	return srv
}

// newServerDB is newServer plus the seeded handle (for tests that add rows).
func newServerDB(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	h := dbtest.Open(t)
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'm@t.dev')`)
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('mj','conversion', CURRENT_TIMESTAMP)`)
	h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status)
		VALUES (1, 1, 1200, '{}', 'approved')`)
	h.DB.Exec(`INSERT INTO usage_events (source, external_id, customer_ext_id, units, unit_type, occurred_at)
		VALUES ('api','ue1','cus_m', 42000, 'tokens', CURRENT_TIMESTAMP)`)
	var day int64 = 20260915
	h.DB.Exec(`INSERT INTO usage_daily (day, customer_ext_id, unit_type, units) VALUES (?, 'cus_m', 'tokens', 42000)`, day)
	srv := httptest.NewServer(mcp.New(h.DB))
	t.Cleanup(srv.Close)
	return srv, h.DB
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func call(t *testing.T, srv *httptest.Server, id int, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// MCP1: initialize handshake.
func TestMCP_Initialize(t *testing.T) {
	srv := newServer(t)
	out := call(t, srv, 1, "initialize", map[string]any{"protocolVersion": "2024-11-05"})
	res := out["result"].(map[string]any)
	if res["serverInfo"].(map[string]any)["name"] != "affiliate-engine" {
		t.Fatalf("init: %v", res)
	}
}

// MCP2: tools/list exposes all four tools with schemas.
func TestMCP_ToolsList(t *testing.T) {
	srv := newServer(t)
	out := call(t, srv, 2, "tools/list", nil)
	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("tools=%d want 4", len(tools))
	}
}

// MCP3: earnings tool returns the seeded commission.
func TestMCP_Earnings(t *testing.T) {
	srv := newServer(t)
	out := call(t, srv, 3, "tools/call", map[string]any{
		"name": "earnings_summary", "arguments": map[string]any{"affiliate_id": 1},
	})
	text := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !contains(text, "approved_cents=1200") {
		t.Fatalf("earnings text: %s", text)
	}
}

// MCP4: dashboard reports ledger OK + integrity.
func TestMCP_Dashboard(t *testing.T) {
	srv := newServer(t)
	out := call(t, srv, 4, "tools/call", map[string]any{"name": "dashboard", "arguments": map[string]any{}})
	text := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !contains(text, "ledger_sum=0 (OK)") {
		t.Fatalf("dashboard: %s", text)
	}
}

// MCP5: usage meter tool.
func TestMCP_Usage(t *testing.T) {
	srv := newServer(t)
	out := call(t, srv, 5, "tools/call", map[string]any{
		"name": "usage_meter", "arguments": map[string]any{"customer_id": "cus_m"},
	})
	text := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !contains(text, "42000 tokens") {
		t.Fatalf("usage: %s", text)
	}
}

// MCP6: unknown method + unknown tool get proper JSON-RPC errors.
func TestMCP_Errors(t *testing.T) {
	srv := newServer(t)
	out := call(t, srv, 6, "resources/list", nil)
	if out["error"] == nil {
		t.Fatal("unknown method not errored")
	}
	out = call(t, srv, 7, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
	if out["error"] == nil {
		t.Fatal("unknown tool not errored")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
