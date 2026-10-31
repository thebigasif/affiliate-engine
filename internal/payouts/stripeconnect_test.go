package payouts_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// fakeTransfer is a transfer as Stripe lists it.
type fakeTransfer struct {
	ID          string `json:"id"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Destination string `json:"destination"`
	Created     int64  `json:"created"`
	group       string
}

// fakeStripe is a Stripe account shared by every StripeConnect pointed at
// it: transfers persist across installs, like one real Stripe key.
type fakeStripe struct {
	mu        sync.Mutex
	transfers []fakeTransfer
	posts     int32
	idem      []string
	forms     []string
}

func (f *fakeStripe) add(t fakeTransfer, group string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.group = group
	f.transfers = append(f.transfers, t)
}

func (f *fakeStripe) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet {
			group := r.URL.Query().Get("transfer_group")
			out := []fakeTransfer{}
			for _, tr := range f.transfers {
				if tr.group == group {
					out = append(out, tr)
				}
			}
			b, _ := json.Marshal(map[string]any{"data": out})
			w.Write(b)
			return
		}
		atomic.AddInt32(&f.posts, 1)
		r.ParseForm()
		f.idem = append(f.idem, r.Header.Get("Idempotency-Key"))
		f.forms = append(f.forms, r.PostForm.Encode())
		amount, _ := strconv.ParseInt(r.PostForm.Get("amount"), 10, 64)
		tr := fakeTransfer{
			ID: "tr_" + strconv.Itoa(len(f.transfers)+1), Amount: amount,
			Currency: r.PostForm.Get("currency"), Destination: r.PostForm.Get("destination"),
			Created: time.Now().Unix(), group: r.PostForm.Get("transfer_group"),
		}
		f.transfers = append(f.transfers, tr)
		w.Write([]byte(`{"id":"` + tr.ID + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// SC1: successful transfer — form fields + install-scoped idempotency key.
func TestStripeConnect_Execute(t *testing.T) {
	fs := &fakeStripe{}
	srv := fs.server(t)
	sc := &payouts.StripeConnect{SecretKey: "sk_test_x", BaseURL: srv.URL, InstallID: "inst1"}

	if err := sc.Execute(77, 5000, "acct_partner", "USD"); err != nil {
		t.Fatal(err)
	}
	if len(fs.idem) != 1 || fs.idem[0] != "ae_inst1_payout_77" {
		t.Fatalf("idem=%v", fs.idem)
	}
	for _, want := range []string{
		"amount=5000", "currency=usd", "destination=acct_partner",
		"metadata%5Bpayout_id%5D=77", "transfer_group=ae_inst1_payout_77",
	} {
		if !strings.Contains(fs.forms[0], want) {
			t.Fatalf("form missing %s: %s", want, fs.forms[0])
		}
	}
}

// SC2: Stripe errors surface with retryable classification.
func TestStripeConnect_Errors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		retry  bool
	}{
		{402, `{"error":{"code":"card_declined","message":"declined"}}`, false},
		{429, `{"error":{"code":"rate_limited","message":"slow down"}}`, true},
		{500, `{"error":{"message":"internal"}}`, true},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		sc := &payouts.StripeConnect{SecretKey: "sk_test_x", BaseURL: srv.URL, InstallID: "inst1"}
		err := sc.Execute(1, 100, "acct_x", "USD")
		if err == nil {
			t.Fatalf("status %d not errored", c.status)
		}
		se, ok := err.(*payouts.StripeError)
		if !ok {
			t.Fatalf("status %d: not a StripeError: %T", c.status, err)
		}
		if se.Retryable() != c.retry {
			t.Fatalf("status %d retryable=%v want %v", c.status, se.Retryable(), c.retry)
		}
		srv.Close()
	}
}

// SC3: a retry after the transfer landed never re-sends — even past
// Stripe's 24h idempotency window, the transfer_group lookup finds it.
func TestStripeConnect_NoResendAfterTransfer(t *testing.T) {
	fs := &fakeStripe{}
	srv := fs.server(t)
	sc := &payouts.StripeConnect{SecretKey: "sk_test_x", BaseURL: srv.URL, InstallID: "inst1"}
	if err := sc.Execute(9, 100, "acct_x", "USD"); err != nil {
		t.Fatal(err)
	}
	if err := sc.Execute(9, 100, "acct_x", "USD"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&fs.posts); n != 1 {
		t.Fatalf("transfer creates=%d want 1 (retry must adopt, not re-send)", n)
	}
}

// SC4: a payout that already carries a provider_ref is never re-sent (no
// Stripe call at all), and a fresh transfer's id is persisted. The install
// id comes from the database.
func TestStripeConnect_ProviderRefGuard(t *testing.T) {
	h, _, _ := newEnv(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		w.Write([]byte(`{"id":"tr_new"}`))
	}))
	defer srv.Close()

	var paidID, freshID int64
	if err := h.DB.QueryRow(`INSERT INTO payouts (affiliate_id, amount, provider_ref)
		VALUES (1, 100, 'tr_old') RETURNING id`).Scan(&paidID); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.QueryRow(`INSERT INTO payouts (affiliate_id, amount)
		VALUES (1, 200) RETURNING id`).Scan(&freshID); err != nil {
		t.Fatal(err)
	}
	sc := &payouts.StripeConnect{SecretKey: "sk_test_x", BaseURL: srv.URL, DB: h.DB}

	if err := sc.Execute(paidID, 100, "acct_x", "USD"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("stripe hits=%d for a payout with provider_ref (must not re-send)", n)
	}

	if err := sc.Execute(freshID, 200, "acct_x", "USD"); err != nil {
		t.Fatal(err)
	}
	var ref string
	if err := h.DB.QueryRow(`SELECT provider_ref FROM payouts WHERE id = ?`, freshID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref != "tr_new" {
		t.Fatalf("provider_ref=%q want tr_new", ref)
	}
	var inst string
	if err := h.DB.QueryRow(`SELECT install_id FROM install_identity WHERE id = 1`).Scan(&inst); err != nil || inst == "" {
		t.Fatalf("install id=%q err=%v", inst, err)
	}
	if sc.InstallID != inst {
		t.Fatalf("install id not read from the database: %q vs %q", sc.InstallID, inst)
	}
}

// SC5: two installs (staging and production, or one database reset)
// share a Stripe key and both have payout 5. The second must send its own
// transfer, not adopt the first's and mark the payout paid with nothing
// sent.
func TestStripeConnect_InstallsDoNotShareTransfers(t *testing.T) {
	fs := &fakeStripe{}
	srv := fs.server(t)
	prod := &payouts.StripeConnect{SecretKey: "sk_test_x", BaseURL: srv.URL, InstallID: "prod"}
	staging := &payouts.StripeConnect{SecretKey: "sk_test_x", BaseURL: srv.URL, InstallID: "stag"}
	if err := prod.Execute(5, 1000, "acct_a", "USD"); err != nil {
		t.Fatal(err)
	}
	if err := staging.Execute(5, 1000, "acct_a", "USD"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&fs.posts); n != 2 {
		t.Fatalf("transfers created=%d want 2 (one per install)", n)
	}
}

// SC6: a transfer under the legacy group (ae_payout_<id>, before the
// install id) is adopted only when it pays this payout's destination,
// amount and currency; another install's legacy transfer is ignored.
func TestStripeConnect_LegacyGroupVerified(t *testing.T) {
	fs := &fakeStripe{}
	srv := fs.server(t)
	now := time.Now().Unix()
	fs.add(fakeTransfer{ID: "tr_other", Amount: 999, Currency: "usd", Destination: "acct_someone", Created: now}, payouts.LegacyTransferGroup(3))
	fs.add(fakeTransfer{ID: "tr_mine", Amount: 700, Currency: "eur", Destination: "acct_b", Created: now}, payouts.LegacyTransferGroup(4))
	sc := &payouts.StripeConnect{SecretKey: "sk_test_x", BaseURL: srv.URL, InstallID: "inst1"}

	if err := sc.Execute(3, 1000, "acct_a", "USD"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&fs.posts); n != 1 {
		t.Fatalf("creates=%d want 1 (a foreign legacy transfer must not count as paid)", n)
	}
	id, err := sc.Find(4, 700, "acct_b", "EUR")
	if err != nil || id != "tr_mine" {
		t.Fatalf("legacy adopt: id=%q err=%v want tr_mine", id, err)
	}
	if id, err := sc.Find(4, 700, "acct_c", "EUR"); err != nil || id != "" {
		t.Fatalf("legacy transfer to another destination adopted: id=%q err=%v", id, err)
	}
}

// SC7: a transfer under the payout's own group that pays something else
// is never adopted and nothing is sent.
func TestStripeConnect_OwnGroupMismatchRefuses(t *testing.T) {
	fs := &fakeStripe{}
	srv := fs.server(t)
	fs.add(fakeTransfer{ID: "tr_x", Amount: 50, Currency: "usd", Destination: "acct_a", Created: time.Now().Unix()},
		payouts.TransferGroup("inst1", 8))
	sc := &payouts.StripeConnect{SecretKey: "sk_test_x", BaseURL: srv.URL, InstallID: "inst1"}
	err := sc.Execute(8, 1000, "acct_a", "USD")
	if !errors.Is(err, payouts.ErrTransferMismatch) {
		t.Fatalf("err=%v want ErrTransferMismatch", err)
	}
	if n := atomic.LoadInt32(&fs.posts); n != 0 {
		t.Fatalf("creates=%d want 0", n)
	}
}

// SC8: a transfer older than the payout row (an old database's payout
// with the same id) is not this payout's.
func TestStripeConnect_TransferOlderThanPayoutIgnored(t *testing.T) {
	h, _, _ := newEnv(t)
	fs := &fakeStripe{}
	srv := fs.server(t)
	var pid int64
	if err := h.DB.QueryRow(`INSERT INTO payouts (affiliate_id, amount) VALUES (1, 300) RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	fs.add(fakeTransfer{
		ID: "tr_old", Amount: 300, Currency: "usd", Destination: "acct_a",
		Created: time.Now().Add(-48 * time.Hour).Unix(),
	}, payouts.LegacyTransferGroup(pid))
	sc := &payouts.StripeConnect{SecretKey: "sk_test_x", BaseURL: srv.URL, DB: h.DB}
	if err := sc.Execute(pid, 300, "acct_a", "USD"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&fs.posts); n != 1 {
		t.Fatalf("creates=%d want 1 (a transfer from before the payout existed is not its)", n)
	}
}
