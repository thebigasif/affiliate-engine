package httpapi

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/partnermail"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// testClock is a settable clock for cooling periods.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// changeFixture is the API fixture served with mail on and a test clock.
func changeFixture(t *testing.T) (*apiFixture, *testClock) {
	t.Helper()
	f := setupAPI(t)
	clock := &testClock{t: time.Now().UTC().Truncate(time.Second)}
	srv := httptest.NewServer(NewAPIHandler(APIDeps{
		DB:           f.db.DB,
		PayoutRunner: payouts.NewRunner(f.db.DB),
		TaxDocKey:    DeriveTaxDocKey([]byte("api-test-deployment-secret-0123456789")),
		MailOn:       true,
		Now:          clock.Now,
	}))
	t.Cleanup(srv.Close)
	g := *f
	g.srv = srv
	return &g, clock
}

func exportPayPal(t *testing.T, f *apiFixture) (rows [][]string, skipped string) {
	t.Helper()
	res, body := rawGet(t, f.srv.URL+"/api/v1/admin/payouts/export/paypal.csv", f.adminKey)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export: %d %s", res.StatusCode, body)
	}
	recs, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return recs[1:], res.Header.Get("X-Payouts-Skipped")
}

func payoutHeld(t *testing.T, f *apiFixture, payoutID int64) string {
	t.Helper()
	_, m := getJSON(t, f.srv.URL+"/api/v1/admin/payouts", f.adminKey)
	for _, x := range m["payouts"].([]any) {
		if p := x.(map[string]any); int64(p["id"].(float64)) == payoutID {
			held, _ := p["held"].(string)
			return held
		}
	}
	t.Fatalf("payout %d not listed", payoutID)
	return ""
}

func auditDetails(t *testing.T, f *apiFixture) []string {
	t.Helper()
	rows, err := f.db.DB.Query(`SELECT actor, detail FROM audit_log WHERE action = 'payout_details_change' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var actor, detail string
		if err := rows.Scan(&actor, &detail); err != nil {
			t.Fatal(err)
		}
		if actor != "affiliate:1" {
			t.Fatalf("audit actor = %q", actor)
		}
		out = append(out, detail)
	}
	return out
}

func noticePayloads(t *testing.T, f *apiFixture) []string {
	t.Helper()
	rows, err := f.db.DB.Query(`SELECT payload FROM outbox_events WHERE topic = ? ORDER BY id`, partnermail.TopicPayoutDetailsChanged)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// Payout details change only through a portal session. Neither the
// partner's own API key nor the owner's admin key can redirect payouts;
// both can still read them.
func TestPayoutDetailsChange_APIKeysRefused(t *testing.T) {
	f := setupAPI(t)
	body := `{"method":"paypal","paypal_email":"thief@evil.test"}`
	if code, m := putPayoutDetails(t, f, f.aff1Key, body); code != http.StatusForbidden {
		t.Fatalf("affiliate key PUT: %d %v", code, m)
	}
	req, _ := http.NewRequest("PUT", f.srv.URL+"/api/v1/affiliate/payout-details?affiliate_id=1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.adminKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("admin key PUT: %d", res.StatusCode)
	}
	var n int
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM affiliate_payout_details`).Scan(&n)
	if n != 0 {
		t.Fatalf("details stored through a key: %d rows", n)
	}
	if code, m := putPayoutDetails(t, f, portalSession(t, f, 1), body); code != http.StatusOK {
		t.Fatalf("portal session PUT: %d %v", code, m)
	}
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/affiliate/payout-details", f.aff1Key); code != http.StatusOK {
		t.Fatalf("affiliate key GET: %d", code)
	}
}

// A first-ever setup pays at once, and is still audited and announced.
func TestPayoutDetailsChange_FirstSetupNotHeld(t *testing.T) {
	f, _ := changeFixture(t)
	sess := portalSession(t, f, 1)
	code, m := putPayoutDetails(t, f, sess, `{"method":"paypal","paypal_email":"jo.pay@example.com"}`)
	if code != http.StatusOK {
		t.Fatalf("save: %d %v", code, m)
	}
	if d := m["payout_details"].(map[string]any); d["cooling_until"] != nil {
		t.Fatalf("first setup cools: %v", d)
	}
	audit := auditDetails(t, f)
	if len(audit) != 1 || !strings.Contains(audit[0], "PayPal: j***@example.com") || strings.Contains(audit[0], "jo.pay") {
		t.Fatalf("audit: %v", audit)
	}
	notices := noticePayloads(t, f)
	if len(notices) != 1 || !strings.Contains(notices[0], "j***@example.com") || strings.Contains(notices[0], "jo.pay") {
		t.Fatalf("notice: %v", notices)
	}
	pid := stageAff1Payout(t, f)
	if held := payoutHeld(t, f, pid); held != "" {
		t.Fatalf("first setup held: %q", held)
	}
	rows, _ := exportPayPal(t, f)
	if len(rows) != 1 || rows[0][0] != "jo.pay@example.com" {
		t.Fatalf("export: %v", rows)
	}
}

// Changing existing details holds the partner's payouts for the cooling
// period: the payout list says why, Mark as paid is refused, and the
// PayPal file leaves them out. After the period the file pays the new
// address. Every change is audited and announced with masked values.
func TestPayoutDetailsChange_CoolingHoldsPayouts(t *testing.T) {
	f, clock := changeFixture(t)
	sess := portalSession(t, f, 1)
	if code, m := putPayoutDetails(t, f, sess, `{"method":"paypal","paypal_email":"jo.pay@example.com"}`); code != http.StatusOK {
		t.Fatalf("first save: %d %v", code, m)
	}
	clock.Add(time.Hour)
	code, m := putPayoutDetails(t, f, sess, `{"method":"paypal","paypal_email":"new.place@other.test"}`)
	if code != http.StatusOK {
		t.Fatalf("change: %d %v", code, m)
	}
	want := clock.Now().Add(PayoutDetailsCooling).Format(time.RFC3339)
	if d := m["payout_details"].(map[string]any); d["cooling_until"] != want {
		t.Fatalf("cooling_until = %v, want %s", d["cooling_until"], want)
	}

	audit := auditDetails(t, f)
	if len(audit) != 2 {
		t.Fatalf("audit rows: %v", audit)
	}
	for _, s := range []string{"PayPal: j***@example.com", "PayPal: n***@other.test", want} {
		if !strings.Contains(audit[1], s) {
			t.Fatalf("audit lacks %q: %s", s, audit[1])
		}
	}
	notices := noticePayloads(t, f)
	if len(notices) != 2 || !strings.Contains(notices[1], "n***@other.test") {
		t.Fatalf("notices: %v", notices)
	}
	for _, s := range append(audit, notices...) {
		if strings.Contains(s, "jo.pay") || strings.Contains(s, "new.place") {
			t.Fatalf("full identifier stored: %s", s)
		}
	}

	pid := stageAff1Payout(t, f)
	if held := payoutHeld(t, f, pid); !strings.Contains(held, "changed their payout details") {
		t.Fatalf("payout list held = %q", held)
	}
	if code, out := paPost(t, f.srv.URL+"/api/v1/admin/payouts/approve", `{"payout_id":`+strconv.FormatInt(pid, 10)+`}`, f.adminKey); code != http.StatusUnprocessableEntity {
		t.Fatalf("approve while cooling: %d %v", code, out)
	}
	rows, skipped := exportPayPal(t, f)
	if len(rows) != 0 || skipped != "1" {
		t.Fatalf("export while cooling: rows=%v skipped=%s", rows, skipped)
	}

	// Just before the end: still held. After it: paid to the new address.
	clock.Add(PayoutDetailsCooling - time.Minute)
	if rows, _ := exportPayPal(t, f); len(rows) != 0 {
		t.Fatalf("export a minute early: %v", rows)
	}
	clock.Add(2 * time.Minute)
	if held := payoutHeld(t, f, pid); held != "" {
		t.Fatalf("held after the period: %q", held)
	}
	rows, _ = exportPayPal(t, f)
	if len(rows) != 1 || rows[0][0] != "new.place@other.test" {
		t.Fatalf("export after the period: %v", rows)
	}
	if code, out := paPost(t, f.srv.URL+"/api/v1/admin/payouts/approve", `{"payout_id":`+strconv.FormatInt(pid, 10)+`}`, f.adminKey); code != http.StatusOK {
		t.Fatalf("approve after the period: %d %v", code, out)
	}
}

// Saving the same details again is not a change: no new cooling period,
// no audit row, no notice.
func TestPayoutDetailsChange_SameDetailsNoCooling(t *testing.T) {
	f, clock := changeFixture(t)
	sess := portalSession(t, f, 1)
	body := `{"method":"paypal","paypal_email":"jo.pay@example.com"}`
	putPayoutDetails(t, f, sess, body)
	clock.Add(time.Hour)
	code, m := putPayoutDetails(t, f, sess, body)
	if code != http.StatusOK {
		t.Fatalf("save again: %d %v", code, m)
	}
	if d := m["payout_details"].(map[string]any); d["cooling_until"] != nil {
		t.Fatalf("same details cool: %v", d)
	}
	if a, n := auditDetails(t, f), noticePayloads(t, f); len(a) != 1 || len(n) != 1 {
		t.Fatalf("audit=%d notices=%d, want 1 and 1", len(a), len(n))
	}
}

// A stored details row that cannot be decrypted blocks the partner's
// payout from the file instead of paying their login email.
func TestPayoutDetailsChange_UnreadableBlocksExport(t *testing.T) {
	f, _ := changeFixture(t)
	if _, err := f.db.DB.Exec(`INSERT INTO affiliate_payout_details (affiliate_id, method, sealed) VALUES (1, 'paypal', ?)`,
		[]byte("not a sealed blob, far longer than one nonce")); err != nil {
		t.Fatal(err)
	}
	pid := stageAff1Payout(t, f)
	if held := payoutHeld(t, f, pid); !strings.Contains(held, "can't be read") {
		t.Fatalf("payout list held = %q", held)
	}
	rows, skipped := exportPayPal(t, f)
	if len(rows) != 0 || skipped != "1" {
		t.Fatalf("export: rows=%v skipped=%s (paid the login email?)", rows, skipped)
	}
	// The partner sees "not set" and can enter them again.
	if _, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/payout-details", f.aff1Key); m["payout_details"] != nil {
		t.Fatalf("unreadable details shown: %v", m)
	}
}
