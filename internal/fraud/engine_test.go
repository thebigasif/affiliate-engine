package fraud_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

func newDB(t *testing.T) *db.Handles {
	t.Helper()
	h := dbtest.Open(t)
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'Owner@Gmail.com')`)
	return h
}

// F1: self-referral detected via gmail alias tricks (dots + plus-tag).
func TestFraud_SelfReferralAliases(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	ctx := context.Background()
	for _, alias := range []string{
		"owner@gmail.com",
		"o.w.n.e.r@gmail.com",
		"owner+blog@gmail.com",
		"owner@googlemail.com",
	} {
		vs, err := e.Evaluate(ctx, fraud.Query{
			AffiliateID: 1, Email: alias, Now: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, v := range vs {
			if v.Rule == "self_referral" && v.Action == fraud.ActionHold {
				found = true
			}
		}
		if !found {
			t.Fatalf("alias %q not caught", alias)
		}
	}
	// A genuinely different email passes.
	vs, _ := e.Evaluate(ctx, fraud.Query{AffiliateID: 1, Email: "customer@other.com", Now: time.Now()})
	for _, v := range vs {
		if v.Rule == "self_referral" {
			t.Fatal("false positive on different email")
		}
	}
}

// F2: click velocity — 60 clicks/hour from one identity triggers hold and
// names the flagged clicks.
func TestFraud_ClickVelocity(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	ctx := context.Background()
	ip := []byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}
	now := time.Now().UTC()
	for i := 0; i < 60; i++ {
		h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, ip_hash)
			VALUES (?, ?, ?, ?, 1, 1, ?)`,
			fmt.Sprintf("v%03d", i), now.Add(-20*time.Minute).Format(time.RFC3339Nano),
			now.Format("20060102"), now.Format("200601"), ip)
	}
	vs, err := e.Evaluate(ctx, fraud.Query{AffiliateID: 1, IPHash: ip, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range vs {
		if v.Rule == "click_velocity" {
			found = true
			if len(v.ClickIDs) == 0 {
				t.Fatal("click_velocity verdict carries no flagged clicks")
			}
		}
	}
	if !found {
		t.Fatal("60 clicks/hour not flagged")
	}
}

// F3: verdicts bind to money — the evaluated commission is held, dry-run
// holds nothing.
func TestFraud_VerdictBindsToMoney(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	ctx := context.Background()
	// Give affiliate 1 an approved commission (id 1).
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('j1','conversion', CURRENT_TIMESTAMP)`)
	h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, matures_at)
		VALUES (1, 1, 500, '{}', 'approved', CURRENT_TIMESTAMP)`)

	// Dry run: no mutation.
	vs, err := e.Evaluate(ctx, fraud.Query{
		CommissionID: 1, AffiliateID: 1, Email: "owner@gmail.com", DryRun: true, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 {
		t.Fatal("dry-run must still produce verdicts")
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM commissions`).Scan(&status)
	if status != "approved" {
		t.Fatal("dry-run mutated money — must not")
	}
	var holds int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commission_holds`).Scan(&holds)
	if holds != 0 {
		t.Fatal("dry-run created a hold — must not")
	}

	// Real run: hold applied (payment blocked, status untouched).
	if _, err := e.Evaluate(ctx, fraud.Query{
		CommissionID: 1, AffiliateID: 1, Email: "owner@gmail.com", Now: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	h.DB.QueryRow(`SELECT COUNT(*) FROM commission_holds WHERE resolved_at IS NULL`).Scan(&holds)
	if holds != 1 {
		t.Fatalf("verdict did not bind to money: holds=%d", holds)
	}
	h.DB.QueryRow(`SELECT status FROM commissions`).Scan(&status)
	if status != "approved" {
		t.Fatalf("hold must not mutate commission state, got %s", status)
	}
	// Self-referral holds but does not freeze (only coupon-hijack freezes).
	var affStatus string
	h.DB.QueryRow(`SELECT status FROM affiliates WHERE id=1`).Scan(&affStatus)
	if affStatus != "active" {
		t.Fatalf("self-referral must hold, not freeze: %s", affStatus)
	}
}

// F4: clean traffic produces zero verdicts.
func TestFraud_CleanPass(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	vs, err := e.Evaluate(context.Background(), fraud.Query{
		AffiliateID: 1, Email: "someone@else.com", Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("false positives: %+v", vs)
	}
}
