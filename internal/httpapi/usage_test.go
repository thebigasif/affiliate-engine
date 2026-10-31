package httpapi_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// US1: batch ingest via admin API + inline settlement.
func TestAdmin_UsageIngestAndSettle(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	poster := ledger.NewPoster(h.Ledger)
	poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD": "liability", "EXPENSE:COMMISSION:USD": "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
	})
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'ai@t.dev')`)
	h.DB.Exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id)
		VALUES (1, 'cus_api', 1)`)
	key := "ae_TestKeyUsageEndp00000000000000000"
	h.DB.Exec(`INSERT INTO api_keys (prefix, key_hash, scope, name) VALUES ('ae_TestKey', ?, 'admin', 'test')`, sha256hex(key))

	srvT := httptest.NewServer(httpapi.NewAPIHandler(httpapi.APIDeps{DB: h.DB, RateLimit: 1000, LedgerPoster: poster}))
	defer srvT.Close()
	srv := srvT
	defer srv.Close()

	body := `{"events":[
		{"external_id":"e1","customer_id":"cus_api","units":50000,"unit_type":"tokens","occurred_at":"2026-09-15T10:00:00Z"},
		{"external_id":"e1","customer_id":"cus_api","units":50000,"unit_type":"tokens","occurred_at":"2026-09-15T10:00:00Z"}
	],"settle_day":20260915,"settle_rate_bp":300}`
	code, out := postJSON(t, srv.URL+"/api/v1/admin/usage", body, key)
	if code != 200 {
		t.Fatalf("ingest: %d %v", code, out)
	}
	if out["accepted"].(float64) != 1 || out["duplicates"].(float64) != 1 {
		t.Fatalf("counts: %v", out)
	}
	if out["settlements"].(float64) != 1 {
		t.Fatalf("settlements: %v", out)
	}
	var amount int64
	h.DB.QueryRow(`SELECT amount FROM commissions`).Scan(&amount)
	if amount != 1500 { // 50k tokens @ 0.30/1k
		t.Fatalf("commission=%d want 1500", amount)
	}
	// Ledger balanced.
	if s, _ := poster.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
	_ = time.Now
}
