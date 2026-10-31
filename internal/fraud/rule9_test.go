package fraud_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

// Rule 9 pin: buyer emails shared across affiliates' commissions -> hold.
func TestPaymentFingerprint_Farm(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Journal for FKs + a third affiliate.
	h.DB.Exec(`INSERT INTO journal_entries (id, idem_key, source, effective_at)
		VALUES (1, 'r9j', 'conversion', CURRENT_TIMESTAMP) ON CONFLICT DO NOTHING`)
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (2, 'two@t.dev') ON CONFLICT DO NOTHING`)
	mk := func(aff int64, buyer string) {
		h.DB.Exec(`INSERT INTO commissions
			(affiliate_id, journal_id, amount, rate_snapshot, status, created_at, buyer_email)
			VALUES (?, 1, 100, '{}', 'approved', ?, ?)`, aff, now.Format(time.RFC3339), buyer)
	}
	// Buyer family 1 + 2 spanning affiliates 1 and 2.
	mk(1, "buyer1@x.dev")
	mk(2, "buyer1@x.dev")
	mk(1, "buyer2@x.dev")
	mk(2, "buyer2@x.dev")
	// Distinct buyer (no overlap) for control.
	mk(1, "solo@x.dev")

	vs, err := fraud.PaymentFingerprint{DB: h.DB}.Evaluate(ctx, fraud.Query{AffiliateID: 1, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Action != fraud.ActionHold {
		t.Fatalf("verdicts=%+v want 1 hold", vs)
	}
	// Below threshold: one shared buyer family only.
	h.DB.Exec(`DELETE FROM commissions WHERE buyer_email = 'buyer2@x.dev'`)
	vs2, _ := fraud.PaymentFingerprint{DB: h.DB}.Evaluate(ctx, fraud.Query{AffiliateID: 1, Now: now})
	if len(vs2) != 0 {
		t.Fatalf("single shared buyer family must not flag: %+v", vs2)
	}
}
