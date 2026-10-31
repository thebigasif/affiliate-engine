package attribution_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

func newEnv(t *testing.T) (*db.Handles, *attribution.Resolver, *tokens.Signer) {
	t.Helper()
	h := dbtest.Open(t)
	secret, err := tokens.NewRandomSecret(32)
	if err != nil {
		t.Fatal(err)
	}
	signer, err2 := tokens.New(secret)
	if err2 != nil {
		t.Fatal(err2)
	}
	exec := func(q string, args ...any) {
		if _, err := h.DB.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v (%s)", err, q)
		}
	}
	exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev'), (2, 'b@t.dev')`)
	exec(`INSERT INTO coupons (code, affiliate_id, campaign_id, rate_type, rate_value)
	      VALUES ('SAVE10', 1, 1, 'inherit', 0)`)
	exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id)
	      VALUES (1, 'cus_9', 2)`)
	return h, attribution.NewResolver(h.DB, signer), signer
}

func validToken(t *testing.T, s *tokens.Signer, clickID string, aff int64) string {
	t.Helper()
	tok, err := s.Sign(clickID, aff, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A1: coupon wins over everything.
func TestLadder_CouponFirst(t *testing.T) {
	h, r, s := newEnv(t)
	ctx := context.Background()
	// Token points at affiliate 2; coupon points at 1. Coupon must win.
	exec(h, `INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id)
	         VALUES ('clk_1', CURRENT_TIMESTAMP, 20260915, 202609, 2, 1)`)
	exec(h, `INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
	         VALUES ('clk_1', 2, 1, 'token', CURRENT_TIMESTAMP, '`+dbtest.At(1*24*time.Hour)+`')`)
	res, err := r.Resolve(ctx, attribution.Query{
		CouponCode: "SAVE10", CustomerExtID: "cus_9",
		Token: validToken(t, s, "clk_1", 2), CampaignID: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "coupon" || res.AffiliateID != 1 {
		t.Fatalf("res=%+v want coupon/1", res)
	}
}

// A2: customer binding beats token.
func TestLadder_CustomerBeforeToken(t *testing.T) {
	h, r, s := newEnv(t)
	ctx := context.Background()
	exec(h, `INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id)
	         VALUES ('clk_2', CURRENT_TIMESTAMP, 20260915, 202609, 1, 1)`)
	exec(h, `INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
	         VALUES ('clk_2', 1, 1, 'token', CURRENT_TIMESTAMP, '`+dbtest.At(1*24*time.Hour)+`')`)
	res, err := r.Resolve(ctx, attribution.Query{
		CustomerExtID: "cus_9", // bound to affiliate 2
		Token:         validToken(t, s, "clk_2", 1),
		CampaignID:    1, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "customer" || res.AffiliateID != 2 {
		t.Fatalf("res=%+v want customer/2", res)
	}
}

// A3: valid token + live attribution row resolves.
func TestLadder_Token(t *testing.T) {
	h, r, s := newEnv(t)
	ctx := context.Background()
	exec(h, `INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id)
	         VALUES ('clk_3', CURRENT_TIMESTAMP, 20260915, 202609, 1, 1)`)
	exec(h, `INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
	         VALUES ('clk_3', 1, 1, 'token', CURRENT_TIMESTAMP, '`+dbtest.At(1*24*time.Hour)+`')`)
	res, err := r.Resolve(ctx, attribution.Query{
		Token: validToken(t, s, "clk_3", 1), CampaignID: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "token" || res.AffiliateID != 1 || res.ClickID != "clk_3" {
		t.Fatalf("res=%+v", res)
	}
}

// A4: expired window falls through to unattributed (not error).
func TestLadder_TokenExpired(t *testing.T) {
	h, r, s := newEnv(t)
	ctx := context.Background()
	exec(h, `INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id)
	         VALUES ('clk_4', '`+dbtest.At(-30*24*time.Hour)+`', 20260816, 202608, 1, 1)`)
	exec(h, `INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
	         VALUES ('clk_4', 1, 1, 'token', '`+dbtest.At(-30*24*time.Hour)+`', '`+dbtest.At(-1*24*time.Hour)+`')`)
	res, err := r.Resolve(ctx, attribution.Query{
		Token: validToken(t, s, "clk_4", 1), CampaignID: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "" {
		t.Fatalf("res=%+v want unattributed after expiry", res)
	}
}

// A5: forged token never attributes (HMAC fails, silent fallthrough).
func TestLadder_ForgedToken(t *testing.T) {
	_, r, s := newEnv(t)
	ctx := context.Background()
	forged := "FORGED." + mustSign(t, s, "x", 1)[len("FORGED."):]
	// Actually simplest: sign with different content than claimed.
	valid := validToken(t, s, "clk_real", 1)
	tampered := valid[:len(valid)-3] + "AAA"
	for _, tok := range []string{forged, tampered, "garbage"} {
		res, err := r.Resolve(ctx, attribution.Query{
			Token: tok, CampaignID: 1, Now: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Method != "" {
			t.Fatalf("forged token %q attributed: %+v", tok, res)
		}
	}
}

// A6: IP fallback attributes but is flagged.
func TestLadder_IPFallbackFlagged(t *testing.T) {
	h, r, _ := newEnv(t)
	ctx := context.Background()
	ip := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	exec(h, `INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, ip_hash)
	         VALUES ('c1', ?, 20260915, 202609, 1, 1, ?)`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), ip)
	res, err := r.Resolve(ctx, attribution.Query{
		IPHash: ip, CampaignID: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "ip_fallback" || !res.Flagged || res.AffiliateID != 1 {
		t.Fatalf("res=%+v want flagged ip_fallback/1", res)
	}
}

// A7: no signals => unattributed, no error.
func TestLadder_Unattributed(t *testing.T) {
	_, r, _ := newEnv(t)
	res, err := r.Resolve(context.Background(), attribution.Query{CampaignID: 1, Now: time.Now()})
	if err != nil || res.Method != "" {
		t.Fatalf("res=%+v err=%v want clean unattributed", res, err)
	}
}

func exec(h *db.Handles, q string, args ...any) {
	if _, err := h.DB.Exec(q, args...); err != nil {
		panic(fmt.Sprintf("seed failed: %v (q=%s)", err, q))
	}
}

func mustSign(t *testing.T, s *tokens.Signer, click string, aff int64) string {
	tok, err := s.Sign(click, aff, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A sale credits in the program of the link that was clicked, not program 1:
// the token and IP paths read the click's campaign, and a customer binding
// keeps the program it was made under, so renewals stay there too.
func TestLadder_CampaignFollowsClickAndBinding(t *testing.T) {
	h, r, s := newEnv(t)
	ctx := context.Background()
	exec(h, `INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, ip_hash)
	         VALUES ('clk_p2', CURRENT_TIMESTAMP, 20260915, 202609, 1, 2, ?)`, []byte{1, 2})
	exec(h, `INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
	         VALUES ('clk_p2', 1, 2, 'token', CURRENT_TIMESTAMP, '`+dbtest.At(1*24*time.Hour)+`')`)

	res, err := r.Resolve(ctx, attribution.Query{Token: validToken(t, s, "clk_p2", 1), CampaignID: 1, Now: time.Now()})
	if err != nil || res.Method != "token" || res.CampaignID != 2 {
		t.Fatalf("token: res=%+v err=%v, want campaign 2", res, err)
	}
	res, err = r.Resolve(ctx, attribution.Query{IPHash: []byte{1, 2}, CampaignID: 1, Now: time.Now()})
	if err != nil || res.Method != "ip_fallback" || res.CampaignID != 2 {
		t.Fatalf("ip: res=%+v err=%v, want campaign 2", res, err)
	}

	exec(h, `INSERT INTO customers (program_id, external_customer_id, affiliate_id, campaign_id)
	         VALUES (1, 'cus_p2', 1, 2)`)
	res, err = r.Resolve(ctx, attribution.Query{CustomerExtID: "cus_p2", CampaignID: 1, Now: time.Now()})
	if err != nil || res.Method != "customer" || res.CampaignID != 2 {
		t.Fatalf("customer: res=%+v err=%v, want campaign 2", res, err)
	}
	// Bindings made before the column existed read as program 1.
	res, err = r.Resolve(ctx, attribution.Query{CustomerExtID: "cus_9", CampaignID: 1, Now: time.Now()})
	if err != nil || res.CampaignID != 1 {
		t.Fatalf("legacy binding: res=%+v err=%v, want campaign 1", res, err)
	}
}
