package privacy_test

// Stored-payload erasure and buyer subject rights (dual-engine, like
// subject_test.go). The contract: personal details leave the JSON
// payloads of raw_events, webhook_events and outbox_events; the rows,
// their event ids and every money row stay.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/privacy"
)

// seedEvents adds stored billing events around seedWorld's subject
// (affiliate 1), its buyer (buyer1@shop.test) and the control.
func seedEvents(t *testing.T, db *sql.DB) {
	t.Helper()
	raw := func(id, payload string) {
		// The engine stores raw_events payloads as bytes.
		mustExec(t, db, `INSERT INTO raw_events (source, external_event_id, event_type, payload) VALUES ('stripe', ?, 'purchase', ?)`,
			id, []byte(payload))
	}
	// The affiliate bought through their own coupon (self-purchase).
	raw("evt_self", `{"Source":"stripe","ExternalID":"evt_self","AmountMinor":9007199254740993,"CustomerEmail":"subject@x.test","CustomerExtID":"cus_self","CouponCode":"savesubj","AffiliateID":1}`)
	// A real buyer of the subject: another person, kept by affiliate erase.
	raw("evt_b1", `{"Source":"stripe","ExternalID":"evt_b1","AmountMinor":20000,"CustomerEmail":"buyer1@shop.test","CustomerExtID":"cus_1","CouponCode":"","AffiliateID":1}`)
	// Same buyer, other customer id, mixed case: found through the email.
	raw("evt_b1b", `{"Source":"stripe","ExternalID":"evt_b1b","AmountMinor":1000,"CustomerEmail":"BUYER1@shop.test","CustomerExtID":"cus_1b","AffiliateID":1}`)
	// Look-alike addresses and the control's coupon must survive.
	raw("evt_ctrl", `{"Source":"stripe","ExternalID":"evt_ctrl","AmountMinor":3000,"CustomerEmail":"xsubject@x.test","CustomerExtID":"cus_2","CouponCode":"savectrl","Note":"cus_1 is not a customer key here","AffiliateID":2}`)
	// The affiliate's payout email used as a buyer email.
	raw("evt_pay", `{"Source":"stripe","ExternalID":"evt_pay","AmountMinor":500,"CustomerEmail":"pay@subject.test","AffiliateID":2}`)

	hook := func(id, payload string) {
		mustExec(t, db, `INSERT INTO webhook_events (source, external_event_id, signature_ok, payload) VALUES ('stripe', ?, ?, ?)`,
			id, true, payload)
	}
	hook("parked:cus_x", `{"kind":"parked","event":{"CustomerEmail":"Subject+promo@x.test","CustomerExtID":"cus_x","CustomerName":"Subject One"}}`)
	// A parked first invoice of the buyer: the late credit reads its
	// CustomerExtID and CouponCode again, so those must stay usable.
	hook("parked:in_b1", `{"kind":"parked","event":{"ExternalID":"in_b1","CustomerEmail":"buyer1@shop.test","CustomerExtID":"cus_1","CouponCode":"SAVE1","AmountMinor":2500},"basis":2500}`)
	hook("pi_1", `{"kind":"link","event_id":"evt_b1","commission_id":1,"keys":["pi_1"]}`)

	notice := func(payload string) {
		mustExec(t, db, `INSERT INTO outbox_events (topic, payload) VALUES ('mail.payout_details_changed', ?)`, payload)
	}
	notice(`{"affiliate_id":1,"method":"paypal","before":"PayPal: s***@x.test","after":"PayPal: p***@subject.test"}`)
	notice(`{"affiliate_id":2,"method":"wise","before":"","after":"Wise: c***@x.test"}`)
}

func payloadOf(t *testing.T, db *sql.DB, table, extID string) string {
	t.Helper()
	var b []byte
	if err := db.QueryRow(`SELECT payload FROM `+table+` WHERE external_event_id = ?`, extID).Scan(&b); err != nil {
		t.Fatalf("payload %s/%s: %v", table, extID, err)
	}
	return strings.ToLower(string(b))
}

func subjectOpener(affID int64, sealed []byte) (map[string]any, bool) {
	if affID != 1 {
		return nil, false
	}
	return map[string]any{"method": "paypal", "paypal_email": "Pay@Subject.test"}, true
}

func TestPrivacy_EraseScrubsStoredEvents(t *testing.T) {
	db := seedWorld(t)
	seedEvents(t, db)
	mustExec(t, db, `INSERT INTO affiliate_payout_details (affiliate_id, method, sealed) VALUES (1, 'paypal', ?)`, []byte("sealed"))
	ctx := context.Background()
	s := privacy.NewStore(db).WithPayoutOpener(subjectOpener)

	rawCount := oneInt(t, db, `SELECT COUNT(*) FROM raw_events`)
	hookCount := oneInt(t, db, `SELECT COUNT(*) FROM webhook_events`)

	rep, err := s.Erase(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	// The subject's email, coupon and payout email are gone.
	self := payloadOf(t, db, "raw_events", "evt_self")
	for _, gone := range []string{"subject@x.test", "savesubj"} {
		if strings.Contains(self, gone) {
			t.Errorf("evt_self still has %q: %s", gone, self)
		}
	}
	// Money and ids inside the payload keep their exact values.
	for _, kept := range []string{"9007199254740993", "evt_self", "cus_self"} {
		if !strings.Contains(self, kept) {
			t.Errorf("evt_self lost %q: %s", kept, self)
		}
	}
	if p := payloadOf(t, db, "raw_events", "evt_pay"); strings.Contains(p, "pay@subject.test") {
		t.Errorf("payout email survived: %s", p)
	}
	parked := payloadOf(t, db, "webhook_events", "parked:cus_x")
	if strings.Contains(parked, "subject+promo@x.test") || strings.Contains(parked, "subject one") {
		t.Errorf("alias email or name survived: %s", parked)
	}
	if !strings.Contains(parked, `"cus_x"`) {
		t.Errorf("parked customer id changed: %s", parked)
	}

	// Other people's data is untouched.
	if p := payloadOf(t, db, "raw_events", "evt_b1"); !strings.Contains(p, "buyer1@shop.test") {
		t.Errorf("buyer email removed by affiliate erase: %s", p)
	}
	ctrl := payloadOf(t, db, "raw_events", "evt_ctrl")
	for _, kept := range []string{"xsubject@x.test", "savectrl"} {
		if !strings.Contains(ctrl, kept) {
			t.Errorf("control event lost %q: %s", kept, ctrl)
		}
	}
	if p := payloadOf(t, db, "webhook_events", "pi_1"); !strings.Contains(p, "pi_1") {
		t.Errorf("link row damaged: %s", p)
	}

	// Payout change notices: the subject's masked details are blanked.
	if n := oneInt(t, db, `SELECT COUNT(*) FROM outbox_events WHERE CAST(payload AS TEXT) LIKE '%subject.test%' OR CAST(payload AS TEXT) LIKE '%s***@x.test%'`); n != 0 {
		t.Errorf("payout notice details survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM outbox_events WHERE CAST(payload AS TEXT) LIKE '%c***@x.test%'`); n != 1 {
		t.Errorf("control notice changed: %d", n)
	}

	// Rows and their event ids stay: duplicate detection still works.
	if got := oneInt(t, db, `SELECT COUNT(*) FROM raw_events`); got != rawCount {
		t.Errorf("raw_events rows %d -> %d", rawCount, got)
	}
	if got := oneInt(t, db, `SELECT COUNT(*) FROM webhook_events`); got != hookCount {
		t.Errorf("webhook_events rows %d -> %d", hookCount, got)
	}
	if rep.Scrubbed["raw_events"] != 2 || rep.Scrubbed["webhook_events"] != 1 || rep.Scrubbed["outbox_events"] != 1 {
		t.Errorf("report = %v", rep.Scrubbed)
	}
	if !containsAny(strings.Join(rep.RedactedInPayloads, "|"), "payout identifiers") || len(rep.NotRedactedInPayloads) != 0 {
		t.Errorf("redaction lists = %v / %v", rep.RedactedInPayloads, rep.NotRedactedInPayloads)
	}

	// A second erase changes nothing.
	rep, err = s.Erase(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"raw_events", "webhook_events", "outbox_events"} {
		if rep.Scrubbed[table] != 0 {
			t.Errorf("second erase scrubbed %s: %d", table, rep.Scrubbed[table])
		}
	}
}

func TestPrivacy_EraseBuyer(t *testing.T) {
	db := seedWorld(t)
	seedEvents(t, db)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_at, bound_method)
		VALUES (1, 'cus_1', 1, '2026-09-01T00:00:00Z', 'token'), (1, 'cus_1b', 1, '2026-09-02T00:00:00Z', 'token')`)
	mustExec(t, db, `INSERT INTO fraud_verdicts (rule, subject, reason, action) VALUES
		('self_referral', '1', 'The buyer''s email "buyer1@shop.test" is the partner''s own email', 1)`)
	s := privacy.NewStore(db)

	if _, err := s.EraseBuyer(ctx, privacy.BuyerRef{}); err != privacy.ErrNoBuyerRef {
		t.Fatalf("empty ref err = %v", err)
	}
	ledgerRows := oneInt(t, db, `SELECT COUNT(*) FROM ledger_entries`)

	rep, err := s.EraseBuyer(ctx, privacy.BuyerRef{Email: " Buyer1@Shop.test "})
	if err != nil {
		t.Fatal(err)
	}

	// Commission: email gone; money, order reference and the provider
	// customer id kept.
	if n := oneInt(t, db, `SELECT COUNT(*) FROM commissions WHERE id = 1 AND buyer_email = '' AND customer_ext_id = 'cus_1'
		AND amount = 5000 AND status = 'pending' AND gross_minor = 20000 AND order_id = 'ord_1'`); n != 1 {
		t.Errorf("commission 1 not anonymised with money and binding key intact")
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM commissions WHERE id = 2 AND buyer_email = 'buyer2@shop.test' AND customer_ext_id = 'cus_2'`); n != 1 {
		t.Errorf("other buyer's commission changed")
	}
	if got := oneInt(t, db, `SELECT COUNT(*) FROM ledger_entries`); got != ledgerRows {
		t.Errorf("ledger rows %d -> %d", ledgerRows, got)
	}
	// Lifetime bindings stay: the affiliate keeps earning on renewals.
	if n := oneInt(t, db, `SELECT COUNT(*) FROM customers`); n != 4 {
		t.Errorf("customer bindings = %d want 4 (all kept)", n)
	}
	// The click behind the purchase loses the visitor data.
	if n := oneInt(t, db, `SELECT COUNT(*) FROM clicks WHERE id = 'clk1' AND ip_hash IS NULL AND ua_hash IS NULL AND referer = '' AND extra_json = '{}'`); n != 1 {
		t.Errorf("buyer click not scrubbed")
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM clicks WHERE id = 'clk2' AND ip_hash IS NOT NULL`); n != 1 {
		t.Errorf("other click scrubbed")
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM uniq_daily`); n != 1 {
		t.Errorf("uniq_daily rows = %d want 1 (the control's)", n)
	}
	// Fraud text quoting the buyer.
	if n := oneInt(t, db, `SELECT COUNT(*) FROM fraud_verdicts WHERE LOWER(reason) LIKE '%buyer1@shop.test%'`); n != 0 {
		t.Errorf("verdict text survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM commission_holds WHERE LOWER(reason) LIKE '%buyer1@shop.test%'`); n != 0 {
		t.Errorf("hold text survived: %d", n)
	}
	// Stored events: email removed; customer id, amount and ids kept.
	for id, cus := range map[string]string{"evt_b1": `"cus_1"`, "evt_b1b": `"cus_1b"`} {
		p := payloadOf(t, db, "raw_events", id)
		if strings.Contains(p, "buyer1@shop.test") {
			t.Errorf("%s still has the buyer email: %s", id, p)
		}
		if !strings.Contains(p, `"amountminor"`) || !strings.Contains(p, cus) {
			t.Errorf("%s lost its amount or customer id: %s", id, p)
		}
	}
	// The parked invoice stays creditable: customer id and coupon intact,
	// the email field empty (the engine's "unknown").
	parked := payloadOf(t, db, "webhook_events", "parked:in_b1")
	if strings.Contains(parked, "buyer1@shop.test") || !strings.Contains(parked, `"cus_1"`) || !strings.Contains(parked, `"save1"`) {
		t.Errorf("parked invoice = %s", parked)
	}
	if p := payloadOf(t, db, "raw_events", "evt_ctrl"); !strings.Contains(p, "cus_1 is not a customer key here") {
		t.Errorf("unrelated text changed: %s", p)
	}
	if p := payloadOf(t, db, "raw_events", "evt_self"); !strings.Contains(p, "subject@x.test") {
		t.Errorf("another subject's event changed: %s", p)
	}
	// Audit: one record, no identifier in it.
	if n := oneInt(t, db, `SELECT COUNT(*) FROM audit_log WHERE table_name = 'buyers' AND action = 'erasure'`); n != 1 {
		t.Errorf("buyer erasure audit rows = %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM audit_log WHERE LOWER(detail) LIKE '%buyer1%' OR detail LIKE '%cus_1%'`); n != 0 {
		t.Errorf("audit detail carries an identifier")
	}
	if rep.Scrubbed["commissions"] != 1 || rep.Scrubbed["raw_events"] != 2 || rep.Scrubbed["webhook_events"] != 1 || len(rep.Deleted) != 1 {
		t.Errorf("report = scrubbed %v deleted %v", rep.Scrubbed, rep.Deleted)
	}

	// Second run: nothing left to match, nothing changes, no new audit row.
	rep, err = s.EraseBuyer(ctx, privacy.BuyerRef{Email: "buyer1@shop.test"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total() != 0 {
		t.Errorf("second buyer erase changed rows: %v %v", rep.Scrubbed, rep.Deleted)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM audit_log WHERE table_name = 'buyers'`); n != 1 {
		t.Errorf("audit rows after re-run = %d", n)
	}

	// By customer id: the linked email goes too.
	if _, err := s.EraseBuyer(ctx, privacy.BuyerRef{CustomerID: "cus_2"}); err != nil {
		t.Fatal(err)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM commissions WHERE id = 2 AND buyer_email = '' AND customer_ext_id = 'cus_2' AND amount = 5000`); n != 1 {
		t.Errorf("erase by customer id did not anonymise commission 2")
	}
}

func TestPrivacy_ExportBuyer(t *testing.T) {
	db := seedWorld(t)
	seedEvents(t, db)
	mustExec(t, db, `INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_at, bound_method)
		VALUES (1, 'cus_1', 1, '2026-09-01T00:00:00Z', 'token')`)
	ctx := context.Background()
	s := privacy.NewStore(db)

	b, err := s.ExportBuyer(ctx, privacy.BuyerRef{Email: "BUYER1@shop.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Purchases) != 1 || b.Purchases[0]["order_id"] != "ord_1" {
		t.Fatalf("purchases = %v", b.Purchases)
	}
	if _, ok := b.Purchases[0]["amount"]; ok {
		t.Errorf("purchase exposes the affiliate's commission amount")
	}
	if len(b.Customers) != 1 || len(b.Clicks) != 1 || len(b.Events) != 2 {
		t.Errorf("customers=%d clicks=%d events=%d want 1/1/2", len(b.Customers), len(b.Clicks), len(b.Events))
	}
	ids, _ := b.Subject["customer_ids"].([]string)
	if strings.Join(ids, ",") != "cus_1,cus_1b" {
		t.Errorf("customer ids = %v", b.Subject["customer_ids"])
	}
	if js, err := b.JSON(); err != nil || strings.Contains(string(js), "buyer2@shop.test") {
		t.Errorf("bundle render: %v", err)
	}

	if _, err := s.ExportBuyer(ctx, privacy.BuyerRef{Email: "nobody@shop.test"}); err != privacy.ErrUnknownBuyer {
		t.Fatalf("unknown buyer err = %v", err)
	}
	if _, err := s.ExportBuyer(ctx, privacy.BuyerRef{}); err != privacy.ErrNoBuyerRef {
		t.Fatalf("empty ref err = %v", err)
	}
}

// Payout details that do not open stop the erase before anything
// changes; the explicit override erases and reports exactly what stayed.
func TestPrivacy_EraseSealedPayoutDetails(t *testing.T) {
	db := seedWorld(t)
	seedEvents(t, db)
	mustExec(t, db, `INSERT INTO affiliate_payout_details (affiliate_id, method, sealed) VALUES (1, 'paypal', ?)`, []byte("sealed"))
	ctx := context.Background()

	for name, s := range map[string]*privacy.Store{
		"no opener": privacy.NewStore(db),
		"wrong key": privacy.NewStore(db).WithPayoutOpener(func(int64, []byte) (map[string]any, bool) { return nil, false }),
	} {
		if _, err := s.Erase(ctx, 1); !errors.Is(err, privacy.ErrPayoutDetailsSealed) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if got := oneStr(t, db, `SELECT email FROM affiliates WHERE id = 1`); got != "subject@x.test" {
			t.Fatalf("%s: erase changed data before refusing: %q", name, got)
		}
	}

	rep, err := privacy.NewStore(db).AllowSealedPayout().Erase(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.NotRedactedInPayloads) != 1 || !containsAny(rep.NotRedactedInPayloads[0], "payout identifiers") {
		t.Errorf("not redacted = %v", rep.NotRedactedInPayloads)
	}
	if containsAny(strings.Join(rep.RedactedInPayloads, "|"), "payout") {
		t.Errorf("report claims payout identifiers were redacted: %v", rep.RedactedInPayloads)
	}
	if p := payloadOf(t, db, "raw_events", "evt_pay"); !strings.Contains(p, "pay@subject.test") {
		t.Errorf("payout email changed without the key: %s", p)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM affiliate_payout_details WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("sealed payout row survived")
	}
}

// After either erase, replaying a real stored purchase through the
// commission engine is still rejected as a duplicate, and no money moves.
func TestPrivacy_EraseKeepsReplayDetection(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	p := ledger.NewPoster(h.Ledger)
	if err := p.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
	}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, h.DB, `INSERT INTO affiliates (id, email, name) VALUES (1, 'aff@t.dev', 'Affiliate One')`)
	mustExec(t, h.DB, `INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		VALUES (1, NULL, 'purchase', 'percent', 1000)`)
	eng := commissions.NewEngine(h.DB, p)
	ev := func(id, email string) commissions.Event {
		return commissions.Event{
			Source: "stripe", ExternalID: id, EventType: "purchase", AmountMinor: 10000, Currency: "USD",
			CustomerEmail: email, CustomerExtID: "cus_r" + id, AffiliateID: 1, CampaignID: 1,
			OccurredAt: time.Now().UTC(),
		}
	}
	for _, e := range []commissions.Event{ev("evt_buyer", "buyer@r.test"), ev("evt_aff", "aff@t.dev")} {
		if _, err := eng.ProcessPurchase(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	s := privacy.NewStore(h.DB)
	if rep, err := s.EraseBuyer(ctx, privacy.BuyerRef{Email: "buyer@r.test"}); err != nil || rep.Scrubbed["raw_events"] != 1 {
		t.Fatalf("buyer erase: %v %v", err, rep)
	}
	if rep, err := s.Erase(ctx, 1); err != nil || rep.Scrubbed["raw_events"] != 1 {
		t.Fatalf("affiliate erase: %v %v", err, rep)
	}
	commissionsBefore := oneInt(t, h.DB, `SELECT COUNT(*) FROM commissions`)
	ledgerBefore := oneInt(t, h.DB, `SELECT COUNT(*) FROM ledger_entries`)
	for _, e := range []commissions.Event{ev("evt_buyer", "buyer@r.test"), ev("evt_aff", "aff@t.dev")} {
		if _, err := eng.ProcessPurchase(ctx, e); !errors.Is(err, commissions.ErrAlreadyProcessed) {
			t.Errorf("replay of %s after erase: err = %v, want ErrAlreadyProcessed", e.ExternalID, err)
		}
	}
	if got := oneInt(t, h.DB, `SELECT COUNT(*) FROM commissions`); got != commissionsBefore {
		t.Errorf("replay created commissions: %d -> %d", commissionsBefore, got)
	}
	if got := oneInt(t, h.DB, `SELECT COUNT(*) FROM ledger_entries`); got != ledgerBefore {
		t.Errorf("replay posted money: %d -> %d", ledgerBefore, got)
	}
}
