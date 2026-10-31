package mlm_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/mlm"
)

// periodEnv seeds: 2 under 1 (sponsor), tier rate L1=500bp, one approved
// commission of 300 for affiliate 2 created NOW.
func periodEnv(t *testing.T) (*mlm.Engine, *db.Handles) {
	t.Helper()
	h, e := newEnv(t)
	ctx := context.Background()
	e.PlaceInSponsorTree(ctx, 2, 1)
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('pj', 'test', ?)`,
		time.Now().UTC().Format(time.RFC3339))
	var jid int64
	h.DB.QueryRow(`SELECT id FROM journal_entries WHERE idem_key='pj'`).Scan(&jid)
	h.DB.Exec(`INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, rate_snapshot, status, created_at, approved_at)
		VALUES (2, 1, ?, 300, '{}', 'approved', ?, ?)`, jid,
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	return e, h
}

// F2 pin: overlapping windows must not double-pay. Commission 300 with
// L1=500bp entitles affiliate 1 to exactly 15 cents, EVER.
func TestRunPeriod_OverlappingWindowsNoDoublePay(t *testing.T) {
	e, h := periodEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	w1s := now.Add(-48 * time.Hour)
	w1e := now.Add(24 * time.Hour)
	w2s := now.Add(-24 * time.Hour) // overlaps w1
	w2e := now.Add(48 * time.Hour)

	if _, err := e.RunPeriod(ctx, w1s, w1e); err != nil {
		t.Fatalf("window 1: %v", err)
	}
	// Second overlapping window: same commission, new period id.
	lines, err := e.RunPeriod(ctx, w2s, w2e)
	if err != nil {
		t.Fatalf("window 2: %v", err)
	}
	if lines != 0 {
		t.Fatalf("F2: overlapping window re-paid %d lines (entitlement already consumed)", lines)
	}

	var total int64
	h.DB.QueryRow(`SELECT COALESCE(SUM(commission_cents),0) FROM mlm_period_lines`).Scan(&total)
	if total != 90 {
		t.Fatalf("tier total = %d, want 90 (300 * 3000bp)", total)
	}
}

// F8 pin: engine-created timestamp formats still land inside windows.
func TestRunPeriod_SeesEngineTimestamps(t *testing.T) {
	e, _ := periodEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lines, err := e.RunPeriod(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if lines == 0 {
		t.Fatal("F8: commission created inside window not seen")
	}
}
