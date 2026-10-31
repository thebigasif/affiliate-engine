package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// With email on, approving a payout queues one "you've been paid" email;
// a repeated approve (already paid) queues nothing more.
func TestPayoutApprove_QueuesPaidEmail(t *testing.T) {
	f := setupAPI(t)
	ctx := context.Background()
	srv := httptest.NewServer(NewAPIHandler(APIDeps{DB: f.db.DB, PayoutRunner: payouts.NewRunner(f.db.DB), MailOn: true}))
	t.Cleanup(srv.Close)

	if _, err := ledger.NewPoster(f.db.Ledger).Post(ctx, "pm-1", "conversion", "test", time.Now().UTC(),
		[]ledger.Line{{Account: "EXPENSE:COMMISSION:USD", Amount: 3000}, {Account: "AFFILIATE_PAYABLE:2:USD", Amount: -3000}}); err != nil {
		t.Fatal(err)
	}
	var jid int64
	f.db.DB.QueryRow(`SELECT id FROM journal_entries WHERE idem_key = 'pm-1'`).Scan(&jid)
	now := time.Now().UTC()
	if _, err := f.db.DB.Exec(`INSERT INTO commissions
		(affiliate_id, campaign_id, journal_id, amount, currency, rate_snapshot, status, matures_at, approved_at, created_at)
		VALUES (2, 1, ?, 3000, 'USD', '{}', 'approved', ?, ?, ?)`,
		jid, now.Add(-time.Minute).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if code, out := paPost(t, srv.URL+"/api/v1/admin/payouts/run", `{"min_threshold_cents":2000,"method":"manual"}`, f.adminKey); code != 200 {
		t.Fatalf("run: %d %v", code, out)
	}
	var payoutID int64
	if err := f.db.DB.QueryRow(`SELECT id FROM payouts WHERE affiliate_id = 2 AND status = 'requested'`).Scan(&payoutID); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"payout_id":%d}`, payoutID)
	for i := 0; i < 2; i++ {
		if code, out := paPost(t, srv.URL+"/api/v1/admin/payouts/approve", body, f.adminKey); code != 200 {
			t.Fatalf("approve %d: %d %v", i, code, out)
		}
	}
	var n int
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE topic = 'mail.payout_paid'`).Scan(&n)
	var payload string
	f.db.DB.QueryRow(`SELECT payload FROM outbox_events WHERE topic = 'mail.payout_paid'`).Scan(&payload)
	var p struct {
		PayoutID int64 `json:"payout_id"`
	}
	_ = json.Unmarshal([]byte(payload), &p) // Postgres stores JSONB, so compare the value, not the text
	if n != 1 || p.PayoutID != payoutID {
		t.Fatalf("paid emails queued: %d (%s), want 1", n, payload)
	}
}
