package fraud_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

// Rule 7 pin: two affiliates sharing 2+ IP hashes inside 30d -> hold,
// once the affiliate has attributed conversions; the hold follows the
// overlapping clicks.
func TestIPOverlap_Farm(t *testing.T) {
	e_h := newDB(t)
	ctx := context.Background()
	day := time.Now().UTC()
	d := int64(day.Year())*10000 + int64(day.Month())*100 + int64(day.Day())

	// Affiliates 1 (from newEnv) and 2; shared IPs x2.
	e_h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (2, 'farm2@t.dev')`)
	for i := 0; i < 2; i++ {
		ip := []byte{1, 2, 3, byte(4 + i), 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
		for _, aff := range []int64{1, 2} {
			e_h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, ip_hash)
				VALUES (?, ?, ?, ?, ?, 1, ?)`,
				fmt.Sprintf("c-%d-%d", i, aff), day.Format(time.RFC3339Nano), d, d/100, aff, ip)
		}
	}

	// Shared egress alone (no conversions) must not enforce: clicks are
	// free to manufacture against a victim's links.
	vs0, err := fraud.IPOverlap{DB: e_h.DB}.Evaluate(ctx, fraud.Query{AffiliateID: 1, Now: day})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs0) != 0 {
		t.Fatalf("ip_overlap enforced without conversions: %+v", vs0)
	}
	seedCommissions(t, e_h, 1, 12, 3)

	vs, err := fraud.IPOverlap{DB: e_h.DB}.Evaluate(ctx, fraud.Query{AffiliateID: 1, Now: day})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Action != fraud.ActionHold {
		t.Fatalf("verdicts=%+v want 1 hold", vs)
	}
	if len(vs[0].ClickIDs) != 2 {
		t.Fatalf("flagged clicks=%v want affiliate 1's 2 overlapping clicks", vs[0].ClickIDs)
	}
	// Below threshold (1 shared IP) must stay silent.
	e_h.DB.Exec(`DELETE FROM clicks WHERE ip_hash = ?`, []byte{1, 2, 3, 5, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	vs2, _ := fraud.IPOverlap{DB: e_h.DB}.Evaluate(ctx, fraud.Query{AffiliateID: 1, Now: day})
	if len(vs2) != 0 {
		t.Fatalf("single shared IP must not flag: %+v", vs2)
	}
}

// Rule 8 pin: two active accounts on one alias-normalized email -> the
// rule reports freeze (the engine applies it as a hold only; see
// TestFraud_DuplicateAccountNeverFreezes).
func TestDuplicateAccount_AliasFamily(t *testing.T) {
	e_h := newDB(t)
	ctx := context.Background()
	// newEnv seeds affiliate 1 (a@t.dev per its fixture); add plus-alias twin.
	e_h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (2, 'owner+twin@gmail.com')`)

	vs, err := fraud.DuplicateAccount{DB: e_h.DB}.Evaluate(ctx, fraud.Query{AffiliateID: 2, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Action != fraud.ActionFreeze {
		t.Fatalf("verdicts=%+v want 1 freeze", vs)
	}
	// Distinct base address must stay silent.
	e_h.DB.Exec(`UPDATE affiliates SET email = 'other@t.dev' WHERE id = 2`)
	vs2, _ := fraud.DuplicateAccount{DB: e_h.DB}.Evaluate(ctx, fraud.Query{AffiliateID: 2, Now: time.Now()})
	if len(vs2) != 0 {
		t.Fatalf("distinct emails must not flag: %+v", vs2)
	}
}
