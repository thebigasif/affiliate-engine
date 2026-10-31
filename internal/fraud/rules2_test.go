package fraud_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

// seedCommissions inserts n commissions at the given hour-of-day.
func seedCommissions(t *testing.T, h *db.Handles, aff int64, hour int, n int) {
	t.Helper()
	// journal + affiliate rows for the FKs.
	h.DB.Exec(`INSERT INTO journal_entries (id, idem_key, source, effective_at)
		VALUES (1, 'r2j', 'conversion', CURRENT_TIMESTAMP) ON CONFLICT DO NOTHING`)
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (2, 'two@t.dev') ON CONFLICT DO NOTHING`)
	for i := 0; i < n; i++ {
		ts := time.Now().UTC().AddDate(0, 0, -1)
		ts = time.Date(ts.Year(), ts.Month(), ts.Day(), hour, 0, 0, 0, time.UTC)
		if _, err := h.DB.Exec(`INSERT INTO commissions
			(affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
			VALUES (?, 1, 100, '{}', 'approved', ?)`,
			aff, ts.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
}

// FA1: night-heavy affiliate flagged vs diurnal program baseline.
func TestFraud_ConversionTimeAnomaly(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	// Program baseline: 30 commissions at 14:00 (day).
	seedCommissions(t, h, 2, 14, 30)
	// Suspect: 25 commissions at 02:00.
	seedCommissions(t, h, 1, 2, 25)

	r := fraud.ConversionTimeAnomaly{DB: h.DB}
	vs, err := r.Evaluate(ctx, fraud.Query{AffiliateID: 1, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Action != fraud.ActionHold {
		t.Fatalf("night-heavy not flagged: %+v", vs)
	}
	// The diurnal affiliate is clean.
	vs2, _ := r.Evaluate(ctx, fraud.Query{AffiliateID: 2, Now: time.Now()})
	if len(vs2) != 0 {
		t.Fatalf("diurnal flagged (false positive): %+v", vs2)
	}
}

// FA2: small samples don't flag.
func TestFraud_TimeAnomalyMinSample(t *testing.T) {
	h := newDB(t)
	seedCommissions(t, h, 1, 2, 5) // < 20
	r := fraud.ConversionTimeAnomaly{DB: h.DB}
	vs, _ := r.Evaluate(context.Background(), fraud.Query{AffiliateID: 1, Now: time.Now()})
	if len(vs) != 0 {
		t.Fatalf("min-sample violated: %+v", vs)
	}
}
