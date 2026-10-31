// Package mcp implements a minimal Model Context Protocol server
// (JSON-RPC 2.0 over HTTP POST) exposing affiliate-engine reads + safe
// actions as tools. Stdlib only; SSE omitted (streaming not needed for
// these tools). Auth reuses the admin API key (Bearer).
//
// Supported methods: initialize, tools/list, tools/call.
// Tools: earnings_summary, dashboard, fraud_queue, usage_meter.
//
// Output hygiene (redact.go): buyer/affiliate emails are masked, and
// user-controlled free text is emitted only inside labeled untrusted
// fields — the reader of this output is a model.
package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/gamify"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/money"
)

// Server is the MCP endpoint.
type Server struct {
	db       *sql.DB
	protocol string
}

// New builds the server.
func New(db *sql.DB) *Server { return &Server{db: db, protocol: "2024-11-05"} }

// ServeHTTP handles POST /mcp (JSON-RPC 2.0).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&rpc); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	if rpc.JSONRPC != "2.0" {
		writeRPCError(w, rpc.ID, -32600, "jsonrpc must be 2.0")
		return
	}

	switch rpc.Method {
	case "initialize":
		writeRPCResult(w, rpc.ID, map[string]any{
			"protocolVersion": s.protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "affiliate-engine", "version": "0.2.1"},
		})
	case "tools/list":
		writeRPCResult(w, rpc.ID, map[string]any{"tools": toolDefs()})
	case "tools/call":
		s.handleToolCall(w, r.Context(), rpc.ID, rpc.Params)
	default:
		writeRPCError(w, rpc.ID, -32601, "method not found: "+rpc.Method)
	}
}

func (s *Server) handleToolCall(w http.ResponseWriter, ctx context.Context, id json.RawMessage, params json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		writeRPCError(w, id, -32602, "invalid params")
		return
	}
	var result string
	var err error
	switch p.Name {
	case "earnings_summary":
		result, err = s.toolEarnings(ctx, p.Arguments)
	case "dashboard":
		result, err = s.toolDashboard(ctx)
	case "fraud_queue":
		result, err = s.toolFraudQueue(ctx)
	case "usage_meter":
		result, err = s.toolUsage(ctx, p.Arguments)
	case "program_digest":
		result, err = s.toolDigest(ctx, p.Arguments)
	default:
		writeRPCError(w, id, -32602, "unknown tool: "+p.Name)
		return
	}
	if err != nil {
		writeRPCResult(w, id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}},
			"isError": true,
		})
		return
	}
	writeRPCResult(w, id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": result}},
	})
}

func toolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "earnings_summary",
			"description": "Earnings summary for an affiliate: totals by commission status and recent commissions.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"affiliate_id": map[string]any{"type": "integer", "description": "affiliate id"},
				},
				"required": []string{"affiliate_id"},
			},
		},
		{
			"name":        "dashboard",
			"description": "Program-wide dashboard: clicks, conversions, commission totals, fraud queue depth, ledger integrity.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "fraud_queue",
			"description": "Open fraud verdicts and held commissions awaiting review. Emails are masked; user-supplied text is returned in untrusted fields.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "program_digest",
			"description": "Natural-language program health summary: traffic, conversions, top earner, fraud queue, ledger integrity, action hints.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"days": map[string]any{"type": "integer", "description": "lookback window (default 7)"},
				},
			},
		},
		{
			"name":        "usage_meter",
			"description": "Usage meter for a customer: units by day and unit type (tokens/credits).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"customer_id": map[string]any{"type": "string"},
				},
				"required": []string{"customer_id"},
			},
		},
	}
}

func (s *Server) toolEarnings(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		AffiliateID int64 `json:"affiliate_id"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.AffiliateID <= 0 {
		return "", fmt.Errorf("affiliate_id (integer) required")
	}
	// Per currency: amounts in different currencies are never added.
	type bucket struct{ pending, approved, paid int64 }
	by := map[string]*bucket{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT currency, status, COALESCE(SUM(amount),0) FROM commissions
		WHERE affiliate_id = ? AND status IN ('pending','approved','paid')
		GROUP BY currency, status`, a.AffiliateID)
	if err != nil {
		return "", fmt.Errorf("commission sums: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cur, status string
		var v int64
		if err := rows.Scan(&cur, &status, &v); err != nil {
			return "", fmt.Errorf("commission sums: %w", err)
		}
		cur = money.Normalize(cur)
		if by[cur] == nil {
			by[cur] = &bucket{}
		}
		switch status {
		case "pending":
			by[cur].pending += v
		case "approved":
			by[cur].approved += v
		case "paid":
			by[cur].paid += v
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("commission sums: %w", err)
	}
	var clicks int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(clicks),0) FROM daily_agg WHERE affiliate_id=?`, a.AffiliateID).Scan(&clicks); err != nil {
		return "", fmt.Errorf("clicks sum: %w", err)
	}
	if len(by) == 0 {
		by[money.Default] = &bucket{}
	}
	curs := make([]string, 0, len(by))
	for c := range by {
		curs = append(curs, c)
	}
	sort.Strings(curs)
	var sb strings.Builder
	fmt.Fprintf(&sb, "affiliate %d: clicks=%d", a.AffiliateID, clicks)
	for _, c := range curs {
		b := by[c]
		fmt.Fprintf(&sb, " | %s pending_cents=%d approved_cents=%d paid_cents=%d lifetime_cents=%d (%s owed)",
			c, b.pending, b.approved, b.paid, b.approved+b.paid, money.Display(b.approved, c))
	}
	return sb.String(), nil
}

func (s *Server) toolDashboard(ctx context.Context) (string, error) {
	var clicks, convs int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0) FROM daily_agg`).Scan(&clicks, &convs); err != nil {
		return "", fmt.Errorf("daily_agg sum: %w", err)
	}
	credited, _, err := gamify.CreditedByCurrency(ctx, s.db, time.Unix(0, 0))
	if err != nil {
		return "", err
	}
	// Ledger integrity drives the OK/IMBALANCED verdict — an unreadable
	// ledger MUST fail the tool, never report zeros as "OK". Each
	// currency must balance on its own.
	integrity, err := ledger.CurrencyIntegrity(ctx, s.db)
	if err != nil {
		return "", fmt.Errorf("ledger integrity sum: %w", err)
	}
	var fraudOpen, holds int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fraud_verdicts WHERE resolved=FALSE`).Scan(&fraudOpen); err != nil {
		return "", fmt.Errorf("fraud verdict count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commission_holds WHERE resolved_at IS NULL`).Scan(&holds); err != nil {
		return "", fmt.Errorf("holds count: %w", err)
	}
	status, sum := "OK", "0"
	if !ledger.Balanced(integrity) {
		status, sum = "IMBALANCED", integrity.String()
	}
	return fmt.Sprintf("program: clicks=%d conversions=%d commissions=%s | ledger_sum=%s (%s) | fraud_open=%d holds=%d",
		clicks, convs, credited, sum, status, fraudOpen, holds), nil
}

// toolFraudQueue lists open verdicts. Verdict subjects (coupon codes, ip
// hashes) and reasons (which quote buyer emails verbatim) are user- or
// attacker-influenced: emitted masked, inside untrusted fields.
func (s *Server) toolFraudQueue(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rule, subject, reason FROM fraud_verdicts WHERE resolved=FALSE ORDER BY id DESC LIMIT 20`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	sb.WriteString(untrustedNotice)
	sb.WriteString("open verdicts (latest 20):\n")
	n := 0
	for rows.Next() {
		var rule, subject, reason string
		if err := rows.Scan(&rule, &subject, &reason); err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "- [%s] subject: %s reason: %s\n", rule, untrusted(subject), untrusted(reason))
		n++
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate: %w", err)
	}
	if n == 0 {
		sb.WriteString("  (empty)\n")
	}
	var holds int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commission_holds WHERE resolved_at IS NULL`).Scan(&holds); err != nil {
		return "", fmt.Errorf("holds count: %w", err)
	}
	fmt.Fprintf(&sb, "held commissions: %d", holds)
	return sb.String(), nil
}

func (s *Server) toolUsage(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.CustomerID == "" {
		return "", fmt.Errorf("customer_id required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT day, unit_type, units, settled_cents FROM usage_daily
		WHERE customer_ext_id = ? ORDER BY day DESC LIMIT 30`, a.CustomerID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	fmt.Fprintf(&sb, "usage for %s (latest 30 days):\n", a.CustomerID)
	n := 0
	for rows.Next() {
		var day, units, settled int64
		var ut string
		if err := rows.Scan(&day, &ut, &units, &settled); err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "- day %d: %d %s (settled_cents=%d)\n", day, units, ut, settled)
		n++
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate: %w", err)
	}
	if n == 0 {
		sb.WriteString("  (no usage recorded)\n")
	}
	return sb.String(), nil
}

func (s *Server) toolDigest(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Days int `json:"days"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &a)
	}
	if a.Days <= 0 {
		a.Days = 7
	}
	var clicks, convs int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0)
		FROM daily_agg WHERE day >= ?`, lookbackDay(a.Days)).Scan(&clicks, &convs); err != nil {
		return "", fmt.Errorf("daily_agg sums: %w", err)
	}
	// Credited money and top earners per currency (never summed across
	// currencies). The name falls back to the email (portal sign-up
	// data): never shown raw.
	credited, top, err := gamify.CreditedByCurrency(ctx, s.db, time.Now().UTC().AddDate(0, 0, -a.Days))
	if err != nil {
		return "", err
	}
	var fraudOpen, holds int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fraud_verdicts WHERE resolved=FALSE`).Scan(&fraudOpen); err != nil {
		return "", fmt.Errorf("fraud verdict count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commission_holds WHERE resolved_at IS NULL`).Scan(&holds); err != nil {
		return "", fmt.Errorf("holds count: %w", err)
	}
	integrity, err := ledger.CurrencyIntegrity(ctx, s.db)
	if err != nil {
		return "", fmt.Errorf("ledger integrity sum: %w", err)
	}
	ledgerStatus := "balanced"
	if !ledger.Balanced(integrity) {
		ledgerStatus = fmt.Sprintf("IMBALANCED (%s)", integrity)
	}
	cr := 0.0
	if clicks > 0 {
		cr = float64(convs) / float64(clicks) * 100
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Program digest — last %d days\n\n", a.Days)
	fmt.Fprintf(&sb, "Traffic: %d clicks, %d conversions (%.2f%% CVR)\n", clicks, convs, cr)
	fmt.Fprintf(&sb, "Commissions credited: %s\n", credited)
	if len(top) > 0 {
		sb.WriteString(untrustedNotice)
	}
	for _, t := range top {
		fmt.Fprintf(&sb, "Top earner: affiliate %d name: %s (%s)\n",
			t.AffiliateID, untrusted(t.Name), money.Display(t.Minor, t.Currency))
	}
	fmt.Fprintf(&sb, "Fraud queue: %d open verdicts, %d held commissions\n", fraudOpen, holds)
	fmt.Fprintf(&sb, "Ledger: %s\n", ledgerStatus)
	if holds > 0 {
		sb.WriteString("\nAction: review the fraud queue — held commissions block payouts.\n")
	}
	return sb.String(), nil
}

func lookbackDay(days int) int64 {
	t := time.Now().UTC().AddDate(0, 0, -days)
	var n int64
	for _, c := range t.Format("20060102") {
		n = n*10 + int64(c-'0')
	}
	return n
}

// --- JSON-RPC plumbing ---

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": idRaw(id), "result": result,
	})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": idRaw(id),
		"error": map[string]any{"code": code, "message": msg},
	})
}

func idRaw(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return json.RawMessage(id)
}
