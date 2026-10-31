package gamify_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
)

func mustExec(t *testing.T, h *db.Handles, q string) {
	t.Helper()
	if _, err := h.DB.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// daySeq returns n consecutive YYYYMMDD ints ending at end (inclusive).
func daySeq(end time.Time, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = end.AddDate(0, 0, -i).Format("20060102")
	}
	return out
}

func insertDays(t *testing.T, h *db.Handles, aff int64, days []string, clicks int64) {
	t.Helper()
	for _, d := range days {
		mustExec(t, h, `INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks)
			VALUES (`+d+`, `+itoa(aff)+`, 1, `+itoa(clicks)+`)`)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// G5: click-block XP from daily_agg, idempotent across ticks, resumable.
func TestGamify_ClickBlocks(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	// 250 lifetime clicks -> 2 blocks now, 3rd at 300.
	mustExec(t, h, `INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks) VALUES
		(20260901, 1, 1, 150), (20260902, 1, 1, 100)`)
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := e.Profile(ctx, 1)
	if p["xp"].(int64) != 20 { // 2 blocks x XPPer100Clicks(10)
		t.Fatalf("xp=%v want 20", p["xp"])
	}
	// Tick again (replay): no double grant.
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ = e.Profile(ctx, 1)
	if p["xp"].(int64) != 20 {
		t.Fatalf("replay xp=%v want 20 (idempotence broken)", p["xp"])
	}
	// Grow to 340 clicks -> exactly one new block (3 total).
	mustExec(t, h, `INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks)
		VALUES (20260903, 1, 1, 90)`)
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ = e.Profile(ctx, 1)
	if p["xp"].(int64) != 30 {
		t.Fatalf("resumed xp=%v want 30", p["xp"])
	}
	// No badges from clicks alone below thresholds.
	if bs := p["badges"].([]map[string]any); len(bs) != 0 {
		t.Fatalf("badges=%v want none", bs)
	}
}

// G6: 30-day streak ending today earns streak_30; a gap does not.
func TestGamify_StreakBadge(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	today := time.Now().UTC()
	insertDays(t, h, 1, daySeq(today, 30), 5)
	// Affiliate 2: 40 days but a gap 5 days ago.
	var gapped []string
	for i := 0; i < 40; i++ {
		if i == 5 {
			continue
		}
		gapped = append(gapped, today.AddDate(0, 0, -i).Format("20060102"))
	}
	insertDays(t, h, 2, gapped, 5)
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	b1, _ := e.Badges(ctx, 1)
	if len(b1) != 1 || b1[0]["code"] != "streak_30" {
		t.Fatalf("aff1 badges=%v want streak_30", b1)
	}
	b2, _ := e.Badges(ctx, 2)
	if len(b2) != 0 {
		t.Fatalf("aff2 badges=%v want none (gap breaks streak)", b2)
	}
	// Replay: still exactly one streak badge.
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	b1b, _ := e.Badges(ctx, 1)
	if len(b1b) != 1 {
		t.Fatalf("replay badges=%v want 1", b1b)
	}
}

// G7: streak ending YESTERDAY counts (today not over yet must not break it);
// a streak ending the day before yesterday is genuinely stale.
func TestGamify_StreakGraceDay(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	insertDays(t, h, 1, daySeq(now.AddDate(0, 0, -1), 30), 5) // ends yesterday
	insertDays(t, h, 2, daySeq(now.AddDate(0, 0, -2), 30), 5) // ends day-before
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	b1, _ := e.Badges(ctx, 1)
	if len(b1) != 1 || b1[0]["code"] != "streak_30" {
		t.Fatalf("aff1 (ends yesterday) badges=%v want streak_30", b1)
	}
	b2, _ := e.Badges(ctx, 2)
	if len(b2) != 0 {
		t.Fatalf("aff2 (stale) badges=%v want none", b2)
	}
}

// G8: 10,000 lifetime clicks earns traffic_master; 9,999 does not.
func TestGamify_TrafficBadge(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	mustExec(t, h, `INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks)
		VALUES (20260901, 1, 1, 6000), (20260902, 1, 1, 4000)`)
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := e.Badges(ctx, 1)
	if len(b) != 1 || b[0]["code"] != "traffic_master" {
		t.Fatalf("badges=%v want traffic_master", b)
	}
	mustExec(t, h, `INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks)
		VALUES (20260901, 2, 1, 9999)`)
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	b2, _ := e.Badges(ctx, 2)
	if len(b2) != 0 {
		t.Fatalf("aff2 badges=%v want none (9999 < 10000)", b2)
	}
}

// G10: click-block XP coexists with commission XP (no interference),
// and level reflects the combined total.
func TestGamify_ClicksAndCommissions(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	mustExec(t, h, `INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks)
		VALUES (20260901, 1, 1, 350)`)
	commission(t, h, 30, 1, 500)
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.OnCommission(ctx, 1, 30, 500); err != nil {
		t.Fatal(err)
	}
	if err := e.TickActivity(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := e.Profile(ctx, 1)
	// 30 click XP + 500 first + 100 conversion = 630
	if p["xp"].(int64) != 630 {
		t.Fatalf("xp=%v want 630", p["xp"])
	}
}
