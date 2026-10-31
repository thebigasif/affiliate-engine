package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// The screen-backfill subcommand and the startup pass, on either engine,
// against commissions credited the way releases before screened_at did
// (credited, never screened).

func backfillTestDB(t *testing.T) (*db.Handles, string, func(id, email string) int64) {
	t.Helper()
	h, dsn := dbtest.OpenDSN(t)
	for _, q := range []string{
		`INSERT INTO affiliates (id, email) VALUES (1, 'owner@gmail.com')`,
		`INSERT INTO commission_rules (campaign_id, affiliate_id, rate_type, rate_value) VALUES (1, NULL, 'percent', 1000)`,
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	inner := commissions.NewEngine(h.DB, ledger.NewPoster(h.Ledger))
	credit := func(id, email string) int64 {
		cid, err := inner.ProcessPurchase(context.Background(), commissions.Event{
			Source: "stripe", ExternalID: id, EventType: "purchase",
			AmountMinor: 10000, Currency: "USD", AffiliateID: 1, CampaignID: 1,
			CustomerEmail: email, OccurredAt: time.Now().UTC(),
		})
		if err != nil || cid <= 0 {
			t.Fatalf("credit %s: cid=%d err=%v", id, cid, err)
		}
		return cid
	}
	return h, dsn, credit
}

func scalar(t *testing.T, h *db.Handles, q string, args ...any) int {
	t.Helper()
	var n int
	if err := h.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScreenBackfillCLI(t *testing.T) {
	h, dsn, credit := backfillTestDB(t)
	self := credit("in_self", "owner+x@gmail.com")
	credit("in_clean", "buyer@else.dev")

	out := captureStdout(t, func() {
		if err := runScreenBackfill(dsn, []string{"-dry-run"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "scanned 2, screened 2, would hold 1 (self_referral 1)") || !strings.Contains(out, "dry run") {
		t.Fatalf("dry-run report:\n%s", out)
	}
	if want := fmt.Sprintf("commission %d (affiliate 1): fraud review: self_referral", self); !strings.Contains(out, want) {
		t.Fatalf("dry run does not list %q:\n%s", want, out)
	}
	if n := scalar(t, h, `SELECT COUNT(*) FROM commission_holds`); n != 0 {
		t.Fatalf("dry run wrote %d holds", n)
	}
	if n := scalar(t, h, `SELECT COUNT(*) FROM commissions WHERE screened_at IS NOT NULL`); n != 0 {
		t.Fatalf("dry run marked %d screened", n)
	}

	out = captureStdout(t, func() {
		if err := runScreenBackfill(dsn, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "scanned 2, screened 2, held 1 (self_referral 1)") {
		t.Fatalf("report:\n%s", out)
	}
	if n := scalar(t, h, `SELECT COUNT(*) FROM commission_holds WHERE commission_id = ? AND resolved_at IS NULL`, self); n != 1 {
		t.Fatal("self-referral not held")
	}

	out = captureStdout(t, func() {
		if err := runScreenBackfill(dsn, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "scanned 0, screened 0, held 0") {
		t.Fatalf("rerun report:\n%s", out)
	}
	if n := scalar(t, h, `SELECT COUNT(*) FROM commission_holds`); n != 1 {
		t.Fatalf("rerun: holds = %d, want 1", n)
	}
}

func TestScreenBackfillCLI_BadFlags(t *testing.T) {
	for _, args := range [][]string{{"-since", "yesterday"}, {"-limit", "-1"}} {
		if err := runScreenBackfill("sqlite:"+t.TempDir()+"/x.db", args); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}
