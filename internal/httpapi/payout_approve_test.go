package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

func paPost(t *testing.T, url, body, key string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// P1-5 pin: the payout lifecycle has a production caller — run -> approve ->
// paid, commissions transition to paid, ledger settles, books balance.
func TestPayoutApprove_Lifecycle(t *testing.T) {
	f := setupAPI(t)
	ctx := context.Background()

	// Post the credit journals first (payable bal = 2 x 1000).
	for i := 0; i < 2; i++ {
		if _, err := ledger.NewPoster(f.db.Ledger).Post(ctx,
			fmt.Sprintf("plcp-%d", i), "conversion", "test", time.Now().UTC(),
			[]ledger.Line{
				{Account: "EXPENSE:COMMISSION:USD", Amount: 1000},
				{Account: "AFFILIATE_PAYABLE:2:USD", Amount: -1000},
			}); err != nil {
			t.Fatal(err)
		}
	}
	// Two approved commissions referencing those journals.
	for i := 0; i < 2; i++ {
		var jid int64
		if err := f.db.DB.QueryRow(`SELECT id FROM journal_entries WHERE idem_key = ?`,
			fmt.Sprintf("plcp-%d", i)).Scan(&jid); err != nil || jid == 0 {
			t.Fatalf("seed: journal plcp-%d (err=%v)", i, err)
		}
		if _, err := f.db.DB.Exec(`INSERT INTO commissions
			(affiliate_id, campaign_id, journal_id, amount, currency, rate_snapshot, status, matures_at, approved_at, created_at)
			VALUES (2, 1, ?, 1000, 'USD', '{}', 'approved', ?, ?, ?)`,
			jid,
			time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
			time.Now().UTC().Format(time.RFC3339Nano),
			time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	// Create the batch.
	code, out := paPost(t, f.srv.URL+"/api/v1/admin/payouts/run",
		`{"min_threshold_cents":2000,"method":"manual"}`, f.adminKey)
	if code != 200 {
		t.Fatalf("run: %d %v", code, out)
	}
	if v, ok := out["payouts_created"].(float64); !ok || v < 1 {
		t.Fatalf("no payouts created: %v", out)
	}
	var payoutID int64
	if err := f.db.DB.QueryRow(`SELECT p.id FROM payouts p
		JOIN payout_lines pl ON pl.payout_id = p.id
		JOIN commissions c ON c.id = pl.commission_id
		WHERE p.status='requested' AND c.affiliate_id = 2
		LIMIT 1`).Scan(&payoutID); err != nil {
		t.Fatalf("no requested payout for affiliate 2: %v", err)
	}

	// Approve it through the endpoint.
	body := fmt.Sprintf(`{"payout_id":%d}`, payoutID)
	code, out = paPost(t, f.srv.URL+"/api/v1/admin/payouts/approve", body, f.adminKey)
	if code != 200 {
		t.Fatalf("approve: %d %v", code, out)
	}
	var status string
	f.db.DB.QueryRow(`SELECT status FROM payouts WHERE id=?`, payoutID).Scan(&status)
	if status != "paid" {
		t.Fatalf("payout status=%s want paid", status)
	}
	var n int
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM commissions WHERE status='paid' AND amount=1000 AND affiliate_id=2`).Scan(&n)
	var n2 int
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM commissions WHERE status='paid'`).Scan(&n2)
	t.Logf("paid aff2=%d total=%d", n, n2)
	if n != 2 {
		rows, _ := f.db.DB.Query(`SELECT id, affiliate_id, amount, status FROM commissions`)
		for rows.Next() {
			var id, aff, amt int64
			var st string
			rows.Scan(&id, &aff, &amt, &st)
			t.Logf("comm id=%d aff=%d amt=%d st=%s", id, aff, amt, st)
		}
		rows.Close()
		t.Fatalf("my paid commissions=%d want 2", n)
	}
	// Books balance.
	var s int64
	f.db.DB.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries`).Scan(&s)
	if s != 0 {
		t.Fatalf("ledger sum=%d", s)
	}
	// Approve again (double click / retry): idempotent 200, nothing moves.
	var journalsBefore int
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&journalsBefore)
	code, out = paPost(t, f.srv.URL+"/api/v1/admin/payouts/approve", body, f.adminKey)
	if code != 200 || out["already_paid"] != true {
		t.Fatalf("re-approve: %d %v want 200 already_paid", code, out)
	}
	var journalsAfter int
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&journalsAfter)
	if journalsAfter != journalsBefore {
		t.Fatalf("re-approve posted journals: %d -> %d", journalsBefore, journalsAfter)
	}
}
