package screening_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/screening"
)

type fakeScreener struct {
	calls   int
	vs      []fraud.Verdict
	err     error
	lastQ   fraud.Query
	inner   *fakeInner // to prove ordering: inner must have run first
	orderOK bool
}

func (f *fakeScreener) Evaluate(ctx context.Context, q fraud.Query) ([]fraud.Verdict, error) {
	f.calls++
	f.lastQ = q
	if f.inner != nil {
		f.orderOK = f.inner.calls == 1
	}
	return f.vs, f.err
}

// holdingScreener adds the fraud-engine hold + lookup surfaces.
type holdingScreener struct {
	fakeScreener
	holds    map[int64]string
	cc       fraud.CommissionContext
	lookupEr error
}

func (h *holdingScreener) HoldCommission(ctx context.Context, id int64, reason string) error {
	if h.holds == nil {
		h.holds = map[int64]string{}
	}
	h.holds[id] = reason
	return nil
}

func (h *holdingScreener) LookupCommission(ctx context.Context, id int64) (fraud.CommissionContext, error) {
	return h.cc, h.lookupEr
}

type fakeInner struct {
	calls int
	err   error
}

func (f *fakeInner) ProcessPurchase(ctx context.Context, ev commissions.Event) (int64, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	return 42, nil
}

func (f *fakeInner) ProcessRefund(ctx context.Context, ev commissions.Event, ocid, refunded, original int64) error {
	return nil
}

// Screening runs AFTER the credit, with the new commission id in the
// query (so a verdict can hold the commission that triggered it).
func TestScreening_CreditsThenScreensWithCommissionID(t *testing.T) {
	fi := &fakeInner{}
	fs := &fakeScreener{inner: fi}
	e := screening.New(fi, fs, false)

	cid, err := e.ProcessPurchase(context.Background(), commissions.Event{
		Source: "stripe", ExternalID: "s1", EventType: "purchase",
		AffiliateID: 3, CampaignID: 5, CustomerEmail: "b@x.dev", CouponCode: "C1",
		OccurredAt: time.Now(),
	})
	if err != nil || cid != 42 {
		t.Fatalf("cid=%d err=%v", cid, err)
	}
	if fs.calls != 1 || !fs.orderOK {
		t.Fatalf("screener calls=%d ranAfterCredit=%v", fs.calls, fs.orderOK)
	}
	q := fs.lastQ
	if q.CommissionID != 42 || q.AffiliateID != 3 || q.ProgramID != 5 ||
		q.Email != "b@x.dev" || q.CouponCode != "C1" {
		t.Fatalf("query=%+v", q)
	}
}

// The persisted commission is the source of truth for affiliate, program
// and the click ip hash (click_velocity's input).
func TestScreening_UsesCommissionLookup(t *testing.T) {
	ip := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	hs := &holdingScreener{cc: fraud.CommissionContext{AffiliateID: 9, CampaignID: 4, ClickID: "clk", IPHash: ip}}
	e := screening.New(&fakeInner{}, hs, false)
	if _, err := e.ProcessPurchase(context.Background(), commissions.Event{OccurredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	q := hs.lastQ
	if q.AffiliateID != 9 || q.ProgramID != 4 || string(q.IPHash) != string(ip) {
		t.Fatalf("lookup not applied: %+v", q)
	}
	if len(hs.holds) != 0 {
		t.Fatalf("clean screen held: %v", hs.holds)
	}
}

// Screening errors FAIL CLOSED: the credit stands (the id is returned for
// refund linkage) but the commission is held as screening_error.
func TestScreening_ErrorHoldsCommission(t *testing.T) {
	hs := &holdingScreener{fakeScreener: fakeScreener{err: context.DeadlineExceeded}}
	fi := &fakeInner{}
	e := screening.New(fi, hs, false)
	cid, err := e.ProcessPurchase(context.Background(), commissions.Event{OccurredAt: time.Now()})
	if err != nil || cid != 42 {
		t.Fatalf("cid=%d err=%v", cid, err)
	}
	if fi.calls != 1 {
		t.Fatalf("inner calls=%d want 1", fi.calls)
	}
	if hs.holds[42] != screening.ScreeningErrorReason {
		t.Fatalf("screening error did not hold the commission: %v", hs.holds)
	}

	// A lookup failure is a screening failure too.
	hs2 := &holdingScreener{lookupEr: errors.New("db down")}
	if _, err := screening.New(&fakeInner{}, hs2, false).ProcessPurchase(context.Background(),
		commissions.Event{OccurredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if hs2.holds[42] != screening.ScreeningErrorReason {
		t.Fatalf("lookup failure did not hold: %v", hs2.holds)
	}
}

// Dry-run: the flag propagates, and even a screening error never holds.
func TestScreening_DryRunNeverHolds(t *testing.T) {
	hs := &holdingScreener{fakeScreener: fakeScreener{err: errors.New("boom")}}
	e := screening.New(&fakeInner{}, hs, true)
	if _, err := e.ProcessPurchase(context.Background(), commissions.Event{OccurredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if !hs.lastQ.DryRun {
		t.Fatal("dry-run flag not propagated")
	}
	if len(hs.holds) != 0 {
		t.Fatalf("dry-run held: %v", hs.holds)
	}
}

// Failed or zero credits are not screened (nothing to hold) and the inner
// error is returned unchanged.
func TestScreening_InnerErrorSkipsScreen(t *testing.T) {
	fs := &fakeScreener{}
	e := screening.New(&fakeInner{err: commissions.ErrNoAttribution}, fs, false)
	if _, err := e.ProcessPurchase(context.Background(), commissions.Event{}); !errors.Is(err, commissions.ErrNoAttribution) {
		t.Fatalf("err=%v", err)
	}
	if fs.calls != 0 {
		t.Fatalf("screened a failed credit: calls=%d", fs.calls)
	}
}

// nil screener = no-op pass-through (wiring safety).
func TestScreening_NilScreener(t *testing.T) {
	e := screening.New(&fakeInner{}, nil, false)
	if _, err := e.ProcessPurchase(context.Background(), commissions.Event{}); err != nil {
		t.Fatal(err)
	}
}
