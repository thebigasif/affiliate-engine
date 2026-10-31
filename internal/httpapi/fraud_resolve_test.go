package httpapi_test

// Fraud resolve endpoint: release vs uphold, money-safety at each step.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

func fraudEnv(t *testing.T) (*httptest.Server, *db.Handles, *ledger.Poster, string) {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)
	poster := ledger.NewPoster(h.Ledger)
	if err := poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
		"PAYOUT_CLEARING:USD":     "clearing",
	}); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		if _, err := h.DB.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v (%s)", err, q)
		}
	}
	exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`)

	// Two commissions: one will be released, one upheld.
	// (journal entries first for FK)
	exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('j1','conversion', CURRENT_TIMESTAMP)`)
	exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('j2','conversion', CURRENT_TIMESTAMP)`)
	exec(`INSERT INTO commissions (id, affiliate_id, journal_id, amount, rate_snapshot, status, matures_at)
	      VALUES (10, 1, 1, 500, '{}', 'approved', CURRENT_TIMESTAMP)`)
	exec(`INSERT INTO commissions (id, affiliate_id, journal_id, amount, rate_snapshot, status, matures_at)
	      VALUES (11, 1, 2, 700, '{}', 'pending', '` + dbtest.At(30*24*time.Hour) + `')`)
	exec(`INSERT INTO commission_holds (commission_id, reason) VALUES (10, 'self-referral')`)
	exec(`INSERT INTO commission_holds (commission_id, reason) VALUES (11, 'click velocity')`)
	exec(`INSERT INTO fraud_verdicts (rule, subject, reason, action) VALUES ('self_referral','1','test',1)`)

	// API key: create directly (hash known).
	key := "ae_TestKeyFraudResolve0000000000000000"
	hash := sha256hex(key)
	exec(`INSERT INTO api_keys (prefix, key_hash, scope, name) VALUES ('ae_TestKey', ?, 'admin', 'test')`, hash)

	srv := httptest.NewServer(httpapi.NewAPIHandler(httpapi.APIDeps{
		DB:           h.DB,
		RateLimit:    600,
		LedgerPoster: poster,
	}))
	t.Cleanup(srv.Close)
	return srv, h, poster, key
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// FR1: release path — hold resolved, commission re-enters payout flow.
func TestFraudResolve_Release(t *testing.T) {
	srv, h, _, key := fraudEnv(t)
	body := `{"commission_id": 10, "decision": "release", "note": "reviewed: legit"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/admin/fraud/resolve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var resolved string
	if err := h.DB.QueryRow(
		`SELECT resolution FROM commission_holds WHERE commission_id=10`).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != "released" {
		t.Fatalf("resolution=%s", resolved)
	}
	// Commission untouched (still approved, amount intact).
	var status string
	var amount int64
	h.DB.QueryRow(`SELECT status, amount FROM commissions WHERE id=10`).Scan(&status, &amount)
	if status != "approved" || amount != 500 {
		t.Fatalf("release mutated money: status=%s amount=%d", status, amount)
	}
}

// FR2: uphold path — commission cancelled, ledger clawback posted, balanced.
func TestFraudResolve_Uphold(t *testing.T) {
	srv, h, poster, key := fraudEnv(t)
	// Commission 11 is pending with amount 700 sitting in HOLD.
	// Seed the ledger side so clawback balances: expense->hold original.
	if _, err := poster.Post(context.Background(), "seed:j2", "conversion", "", time.Now(),
		[]ledger.Line{{Account: "EXPENSE:COMMISSION:USD", Amount: 700}, {Account: "HOLD:USD", Amount: -700}}); err != nil {
		t.Fatal(err)
	}
	body := `{"commission_id": 11, "decision": "uphold", "note": "bot farm"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/admin/fraud/resolve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(respBody))
	}
	var out struct {
		ClawbackCents int64 `json:"clawback_cents"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, string(respBody))
	}
	if out.ClawbackCents != 700 {
		t.Fatalf("clawback=%d want 700", out.ClawbackCents)
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM commissions WHERE id=11`).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("status=%s want cancelled", status)
	}
	// Ledger: hold drained back to 0, integrity holds.
	bal, _ := poster.AccountBalance(context.Background(), "HOLD:USD")
	if bal != 0 {
		t.Fatalf("hold=%d want 0 (clawed back)", bal)
	}
	if s, _ := poster.GlobalIntegrity(context.Background()); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}

// FR3: double-resolve rejected (409-style conflict), paid protection.
func TestFraudResolve_Conflicts(t *testing.T) {
	srv, _, _, key := fraudEnv(t)
	post := func(body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/admin/fraud/resolve", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Unknown commission.
	if code := post(`{"commission_id": 999, "decision": "release"}`); code != http.StatusNotFound {
		t.Fatalf("unknown: %d", code)
	}
	// Bad decision.
	if code := post(`{"commission_id": 10, "decision": "maybe"}`); code != http.StatusBadRequest {
		t.Fatalf("bad decision: %d", code)
	}
	// Resolve then re-resolve -> conflict.
	if code := post(`{"commission_id": 10, "decision": "release"}`); code != http.StatusOK {
		t.Fatalf("first: %d", code)
	}
	if code := post(`{"commission_id": 10, "decision": "release"}`); code != http.StatusConflict {
		t.Fatalf("double: %d", code)
	}
}
