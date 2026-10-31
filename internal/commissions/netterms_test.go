package commissions_test

import (
	"context"
	"testing"
	"time"
)

// Per-program NET terms pin: a campaign with net_terms_days=7 must mature
// commissions 7 days out, not the 30d default. (Was the repo's only TODO.)
func TestCommission_PerProgramNetTerms(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Campaign 2 with 7-day NET terms (seed only creates campaign rows via
	// rules; ensure the campaign row exists with the override).
	if _, err := e.h.DB.Exec(`INSERT INTO campaigns (id, name, net_terms_days)
		VALUES (2, 'fastpay', 7)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		VALUES (2, NULL, 'purchase', 'percent', 1000)`); err != nil {
		t.Fatal(err)
	}
	ev := purchase("net-evt", 10000)
	ev.CampaignID = 2
	cid, err := e.e.ProcessPurchase(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	var matures string
	e.h.DB.QueryRow(`SELECT matures_at FROM commissions WHERE id = ?`, cid).Scan(&matures)
	mt, err := time.Parse(time.RFC3339Nano, matures)
	if err != nil {
		t.Fatalf("matures_at=%q: %v", matures, err)
	}
	age := time.Until(mt)
	if age < 6*24*time.Hour || age > 8*24*time.Hour {
		t.Fatalf("matures in %v; want ~7 days (per-program NET terms)", age)
	}
}

// Hold-days pins (program settings): net_terms_days = 0 is "no hold"
// (was silently treated as 30), and a commission whose program row is
// missing falls back to program_settings.default_hold_days, not a
// hard-coded 30.
func TestCommission_HoldDaysZeroAndDefault(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO campaigns (id, name, net_terms_days) VALUES (2, 'instant', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`UPDATE program_settings SET default_hold_days = 12 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []int64{2, 3} {
		if _, err := e.h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
			VALUES (?, NULL, 'purchase', 'percent', 1000)`, c); err != nil {
			t.Fatalf("campaign %d rule seed: %v", c, err)
		}
	}
	matures := func(evID string, camp int64) time.Duration {
		t.Helper()
		ev := purchase(evID, 10000)
		ev.CampaignID = camp
		cid, err := e.e.ProcessPurchase(ctx, ev)
		if err != nil {
			t.Fatal(err)
		}
		var s string
		e.h.DB.QueryRow(`SELECT matures_at FROM commissions WHERE id = ?`, cid).Scan(&s)
		mt, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("matures_at=%q: %v", s, err)
		}
		return time.Until(mt)
	}
	if d := matures("hold-zero", 2); d > time.Hour {
		t.Fatalf("net_terms_days=0 matures in %v; want now", d)
	}
	if d := matures("hold-default", 3); d < 11*24*time.Hour || d > 13*24*time.Hour {
		t.Fatalf("missing program matures in %v; want ~12 days (default_hold_days)", d)
	}
}

// FR-33 hold floor (MO-10): with HoldFloor set, a program hold below it
// (even 0, "no hold") is raised to the floor; a longer hold is kept.
// Without HoldFloor (self-host) nothing changes (the test above).
func TestCommission_HoldFloorClamps(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.e.HoldFloor = func(context.Context) int { return 14 }
	for id, days := range map[int64]int{2: 0, 3: 40} {
		if _, err := e.h.DB.Exec(`INSERT INTO campaigns (id, name, net_terms_days) VALUES (?, 'p', ?)`, id, days); err != nil {
			t.Fatal(err)
		}
		if _, err := e.h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
			VALUES (?, NULL, 'purchase', 'percent', 1000)`, id); err != nil {
			t.Fatal(err)
		}
	}
	for camp, want := range map[int64]int{2: 14, 3: 40} {
		ev := purchase("floor-"+string(rune('a'+camp)), 10000)
		ev.CampaignID = camp
		cid, err := e.e.ProcessPurchase(ctx, ev)
		if err != nil {
			t.Fatal(err)
		}
		var s string
		if err := e.h.DB.QueryRow(`SELECT matures_at FROM commissions WHERE id = ?`, cid).Scan(&s); err != nil {
			t.Fatal(err)
		}
		mt, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Until(mt); d < time.Duration(want-1)*24*time.Hour || d > time.Duration(want+1)*24*time.Hour {
			t.Fatalf("campaign %d matures in %v; want ~%d days", camp, d, want)
		}
	}
}
