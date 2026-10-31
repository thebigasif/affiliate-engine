package httpapi

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// portalSession signs a partner in to the portal and returns the session
// token (sent as a bearer token, like the portal's API calls).
func portalSession(t *testing.T, f *apiFixture, affID int64) string {
	t.Helper()
	tok := "aps_" + strings.Repeat(fmt.Sprintf("%02x", affID), 32)
	if _, err := f.db.DB.Exec(`INSERT INTO affiliate_sessions (token_hash, affiliate_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(tok), affID, time.Now().UTC().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	return tok
}

func putPayoutDetails(t *testing.T, f *apiFixture, key, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("PUT", f.srv.URL+"/api/v1/affiliate/payout-details", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// A partner saves where to be paid; it's stored sealed (no plaintext in
// the database), read back only by them and the owner, and the owner's
// payout list and PayPal file use it.
func TestPayoutDetails_SaveShowAndExport(t *testing.T) {
	f := setupAPI(t)
	// Only a portal session changes payout details (an API key reads them).
	sess1 := portalSession(t, f, 1)

	// Nothing saved yet.
	if code, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/payout-details", f.aff1Key); code != 200 || m["payout_details"] != nil {
		t.Fatalf("empty get: %d %v", code, m)
	}
	// Validation.
	for _, bad := range []string{
		`{"method":"paypal","paypal_email":"not-an-email"}`,
		`{"method":"wise"}`,
		`{"method":"bank","bank":"   "}`,
		`{"method":"crypto","paypal_email":"a@b.co"}`,
	} {
		if code, m := putPayoutDetails(t, f, sess1, bad); code != 400 {
			t.Fatalf("%s accepted: %d %v", bad, code, m)
		}
	}
	code, m := putPayoutDetails(t, f, sess1, `{"method":"paypal","paypal_email":" Jo.Pay@Example.com ","bank":"ignored"}`)
	if code != 200 {
		t.Fatalf("save: %d %v", code, m)
	}
	var raw []byte
	var method string
	f.db.DB.QueryRow(`SELECT method, sealed FROM affiliate_payout_details WHERE affiliate_id = 1`).Scan(&method, &raw)
	if method != "paypal" || bytes.Contains(raw, []byte("jo.pay")) || bytes.Contains(raw, []byte("ignored")) {
		t.Fatalf("stored in the clear or wrong method: %q %q", method, raw)
	}
	_, m = getJSON(t, f.srv.URL+"/api/v1/affiliate/payout-details", f.aff1Key)
	d, _ := m["payout_details"].(map[string]any)
	if d["paypal_email"] != "jo.pay@example.com" || d["bank"] != nil {
		t.Fatalf("read back: %v", m)
	}
	// Another partner sees only their own (nothing).
	if _, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/payout-details", f.aff2Key); m["payout_details"] != nil {
		t.Fatalf("partner 2 sees partner 1's details: %v", m)
	}
	// A sealed blob copied to another partner's row doesn't open there.
	f.db.DB.Exec(`INSERT INTO affiliate_payout_details (affiliate_id, method, sealed) VALUES (2, 'paypal', ?)`, raw)
	if _, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/payout-details", f.aff2Key); m["payout_details"] != nil {
		t.Fatalf("copied blob opened for partner 2: %v", m)
	}

	// Owner: the partners list and the payout list say where to pay.
	_, m = getJSON(t, f.srv.URL+"/api/v1/admin/affiliates", f.adminKey)
	var payTo string
	for _, x := range m["affiliates"].([]any) {
		if a := x.(map[string]any); a["id"].(float64) == 1 {
			payTo, _ = a["pay_to"].(string)
		}
	}
	if payTo != "PayPal: jo.pay@example.com" {
		t.Fatalf("partners list pay_to = %q", payTo)
	}
	stageAff1Payout(t, f)
	_, m = getJSON(t, f.srv.URL+"/api/v1/admin/payouts", f.adminKey)
	if p := m["payouts"].([]any)[0].(map[string]any); p["pay_to"] != "PayPal: jo.pay@example.com" {
		t.Fatalf("payout pay_to: %v", p)
	}
	// The PayPal file pays their PayPal address, not their login email.
	_, body := rawGet(t, f.srv.URL+"/api/v1/admin/payouts/export/paypal.csv", f.adminKey)
	recs, _ := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if len(recs) != 2 || recs[1][0] != "jo.pay@example.com" {
		t.Fatalf("paypal csv: %v", recs)
	}

	// Switching to bank keeps only the bank details.
	if code, m := putPayoutDetails(t, f, sess1, `{"method":"bank","bank":"Jo Bloggs\nIBAN GB00 0000 1234\nMonzo","paypal_email":"x@y.co"}`); code != 200 {
		t.Fatalf("bank: %d %v", code, m)
	}
	_, m = getJSON(t, f.srv.URL+"/api/v1/admin/affiliates", f.adminKey)
	for _, x := range m["affiliates"].([]any) {
		if a := x.(map[string]any); a["id"].(float64) == 1 && a["pay_to"] != "Bank: Jo Bloggs IBAN GB00 0000 1234 Monzo" {
			t.Fatalf("bank pay_to = %v", a["pay_to"])
		}
	}
}

// A partner who hasn't said where to be paid shows when we asked them.
func TestPayoutDetails_AskedShown(t *testing.T) {
	f := setupAPI(t)
	stageAff1Payout(t, f)
	if _, err := f.db.DB.Exec(`INSERT INTO payout_details_asks (affiliate_id, asked_at) VALUES (1, '2026-09-30T09:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_, m := getJSON(t, f.srv.URL+"/api/v1/admin/payouts", f.adminKey)
	if p := m["payouts"].([]any)[0].(map[string]any); p["pay_to"] != "" || p["pay_to_asked_at"] != "2026-09-30T09:00:00Z" {
		t.Fatalf("payout: %v", p)
	}
	_, m = getJSON(t, f.srv.URL+"/api/v1/admin/affiliates", f.adminKey)
	for _, x := range m["affiliates"].([]any) {
		a := x.(map[string]any)
		if want := map[float64]any{1: "2026-09-30T09:00:00Z", 2: nil}[a["id"].(float64)]; a["pay_to_asked_at"] != want {
			t.Fatalf("partner %v asked_at = %v", a["id"], a["pay_to_asked_at"])
		}
	}
}
