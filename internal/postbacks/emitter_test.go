package postbacks_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/postbacks"
)

func newEnv(t *testing.T) *db.Handles {
	t.Helper()
	h := dbtest.Open(t)
	return h
}

func seedCommission(t *testing.T, h *db.Handles, id, aff, amount int64) {
	t.Helper()
	seedCommissionFull(t, h, id, aff, amount, "", "", 0, "USD")
}

// seedCommissionFull plants a commission with the postback linkage columns
// (click_id/order_id/gross/currency) for macro-expansion tests.
func seedCommissionFull(t *testing.T, h *db.Handles, id, aff, amount int64, clickID, orderID string, gross int64, currency string) {
	t.Helper()
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at)
		VALUES ('j` + fmtI(id) + `','conversion', CURRENT_TIMESTAMP)`)
	h.DB.Exec(`INSERT INTO commissions (id, affiliate_id, journal_id, amount, currency, gross_minor, click_id, order_id, rate_snapshot, status, created_at)
		VALUES (?, ?, (SELECT id FROM journal_entries WHERE idem_key = 'j`+fmtI(id)+`'), ?, ?, ?, ?, ?, '{}', 'approved', ?)`,
		id, aff, amount, currency, gross, nullStr(clickID), nullStr(orderID), time.Now().UTC().Format(time.RFC3339))
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func fmtI(n int64) string {
	if n == 0 {
		return "0"
	}
	var s []byte
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	return string(s)
}

// PB1: conversion fires the postback with macros expanded, 2xx = delivered.
func TestPostback_Delivery(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()

	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.String())
		w.WriteHeader(200)
	}))
	defer srv.Close()

	h.DB.Exec(`INSERT INTO affiliates (id, email, postback_url) VALUES (1, 'a@t.dev', ?)`,
		srv.URL+"/track?cid={click_id}&payout={commission_cents}&oid={order_id}")
	seedCommission(t, h, 100, 1, 2500)

	e := postbacks.NewEmitter(h.DB)
	e.AllowLocalhost = true
	e.Start(ctx)
	defer e.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var delivered int
		h.DB.QueryRow(`SELECT COUNT(*) FROM postback_state WHERE delivered_at IS NOT NULL`).Scan(&delivered)
		if delivered == 1 {
			p, _ := gotPath.Load().(string)
			if p != "/track?cid=&payout=2500&oid=" {
				t.Fatalf("macros wrong: %q", p)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("postback never delivered")
}

// PB2: non-2xx retries with backoff, then delivers on recovery.
func TestPostback_Retry(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()

	var fails int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&fails, 1) <= 2 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	h.DB.Exec(`INSERT INTO affiliates (id, email, postback_url) VALUES (1, 'a@t.dev', ?)`, srv.URL+"/cb")
	seedCommission(t, h, 101, 1, 100)

	e := postbacks.NewEmitter(h.DB)
	e.AllowLocalhost = true
	e.BackoffBase = 100 * time.Millisecond // fast retry ladder for the test
	e.TickInterval = 200 * time.Millisecond
	e.Start(ctx)
	defer e.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var attempts int
		var delivered int
		h.DB.QueryRow(`SELECT COALESCE(attempts,0), CASE WHEN delivered_at IS NOT NULL THEN 1 ELSE 0 END FROM postback_state`).Scan(&attempts, &delivered)
		if delivered == 1 && attempts >= 3 {
			return // 2 fails + 1 success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("retry never recovered")
}

// PB3: SSRF guard — internal targets never fire.
func TestPostback_SSRFBlock(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	for i, evil := range []string{
		"http://127.0.0.1:9/x", "http://localhost/x", "http://10.0.0.5/x",
		"http://192.168.1.1/x", "http://169.254.169.254/meta",
		"http://[::1]/x", "http://172.16.0.1/x",
	} {
		id := int64(200 + i)
		h.DB.Exec(`INSERT INTO affiliates (id, email, postback_url) VALUES (?, ?, ?)`,
			id, "ev"+fmtI(id)+"@t.dev", evil)
		seedCommission(t, h, id, id, 100)
	}
	e := postbacks.NewEmitter(h.DB)
	// Direct call (no goroutine loop): one pass.
	e.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	e.Stop()

	// All 7 marked unsafe (skipped, never fired); none reached a live
	// listener (there IS no listener — any HTTP attempt would error, not
	// deliver; the assertion is that every row carries the skip note).
	var blocked int
	h.DB.QueryRow(`SELECT COUNT(*) FROM postback_state WHERE note = 'unsafe target' AND delivered_at IS NOT NULL`).Scan(&blocked)
	var other int
	h.DB.QueryRow(`SELECT COUNT(*) FROM postback_state WHERE note != 'unsafe target'`).Scan(&other)
	if blocked != 7 {
		t.Fatalf("blocked=%d want 7", blocked)
	}
	if other != 0 {
		t.Fatalf("unexpected states: %d", other)
	}
}

// PB4: no postback configured = marked done (no infinite rescan).
func TestPostback_NoURL(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`)
	seedCommission(t, h, 300, 1, 100)

	e := postbacks.NewEmitter(h.DB)
	e.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	e.Stop()

	var done int
	h.DB.QueryRow(`SELECT COUNT(*) FROM postback_state WHERE delivered_at IS NOT NULL AND note = 'no postback url'`).Scan(&done)
	if done != 1 {
		t.Fatal("no-url commission keeps rescanning")
	}
}

// PB5: FR-7 macro contract — {click_id}/{order_id} resolve from the
// commission's linkage columns; {value} = order gross, {payout} =
// commission cents; unknown/empty macros collapse to empty strings.
func TestPostback_MacroContract(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()

	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.String())
		w.WriteHeader(200)
	}))
	defer srv.Close()

	h.DB.Exec(`INSERT INTO affiliates (id, email, postback_url) VALUES (1, 'a@t.dev', ?)`,
		srv.URL+`/pb?cid={click_id}&oid={order_id}&v={value}&p={payout}&x={unknown_macro}`)
	seedCommissionFull(t, h, 400, 1, 2500, "clk_9", "ord_77", 10000, "USD")

	e := postbacks.NewEmitter(h.DB)
	e.AllowLocalhost = true
	e.Start(ctx)
	defer e.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var delivered int
		h.DB.QueryRow(`SELECT COUNT(*) FROM postback_state WHERE delivered_at IS NOT NULL`).Scan(&delivered)
		if delivered == 1 {
			p, _ := gotPath.Load().(string)
			// {value}=10000 (gross), {payout}=2500 (commission), unknown macro
			// left verbatim (receiver-side contract), linkage macros filled.
			want := "/pb?cid=clk_9&oid=ord_77&v=10000&p=2500&x={unknown_macro}"
			if p != want {
				t.Fatalf("macros wrong:\n got %q\nwant %q", p, want)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("postback never delivered")
}

// PB6: {ts:Source:param} traffic-source macros resolve from the
// originating click's captured params (dedicated columns + extra_json
// catch-all); unknown params and click-less commissions resolve empty.
func TestPostback_TrafficSourceMacros(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()

	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.String())
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// The originating click: dedicated columns + two catch-all params.
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`)
	h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id,
		utm_source, subid, placement, extra_json)
		VALUES ('clk_ts', ?, 20260920, 202609, 1, 1, 'newsletter', 's7', 'hero',
		'{"all_params":{"fbclid":"fb1","gclid":"gc2"}}')`,
		time.Now().UTC().Format(time.RFC3339))
	h.DB.Exec(`UPDATE affiliates SET postback_url = ? WHERE id = 1`,
		srv.URL+`/pb?utm={ts:Newsletter:utm_source}&sub={ts:Any:subid}&fb={ts:Meta:fbclid}&g={ts:Google:gclid}&miss={ts:Meta:nope}`)
	seedCommissionFull(t, h, 401, 1, 100, "clk_ts", "ord_1", 0, "USD")

	e := postbacks.NewEmitter(h.DB)
	e.AllowLocalhost = true
	e.Start(ctx)
	defer e.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var delivered int
		h.DB.QueryRow(`SELECT COUNT(*) FROM postback_state WHERE delivered_at IS NOT NULL`).Scan(&delivered)
		if delivered == 1 {
			p, _ := gotPath.Load().(string)
			// Source segment is documentation (the value namespace is the
			// click's params); unknown params -> empty.
			want := "/pb?utm=newsletter&sub=s7&fb=fb1&g=gc2&miss="
			if p != want {
				t.Fatalf("ts macros wrong:\n got %q\nwant %q", p, want)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("postback never delivered")
}

// PB7: signed delivery — when postback_secret is set, the request carries
// X-AE-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, "<unix>.<uri>")).
func TestPostback_SignedDelivery(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	const secret = "pbsec_test_48_chars_0123456789abcdef01234567"

	var sig, uri atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig.Store(r.Header.Get("X-AE-Signature"))
		uri.Store(r.URL.RequestURI())
		w.WriteHeader(200)
	}))
	defer srv.Close()

	h.DB.Exec(`INSERT INTO affiliates (id, email, postback_url, postback_secret)
		VALUES (1, 'a@t.dev', ?, ?)`, srv.URL+"/pb?cid={click_id}", secret)
	seedCommissionFull(t, h, 402, 1, 500, "clk_sig", "ord_2", 0, "USD")

	e := postbacks.NewEmitter(h.DB)
	e.AllowLocalhost = true
	e.Start(ctx)
	defer e.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var delivered int
		h.DB.QueryRow(`SELECT COUNT(*) FROM postback_state WHERE delivered_at IS NOT NULL`).Scan(&delivered)
		if delivered == 1 {
			s, _ := sig.Load().(string)
			u, _ := uri.Load().(string)
			if s == "" {
				t.Fatal("no X-AE-Signature on signed delivery")
			}
			var ts string
			var v1 string
			for _, part := range strings.Split(s, ",") {
				if kv := strings.SplitN(part, "=", 2); len(kv) == 2 {
					if kv[0] == "t" {
						ts = kv[1]
					}
					if kv[0] == "v1" {
						v1 = kv[1]
					}
				}
			}
			if ts == "" || v1 == "" {
				t.Fatalf("malformed signature %q", s)
			}
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(ts + "." + u))
			want := hex.EncodeToString(mac.Sum(nil))
			if !hmac.Equal([]byte(v1), []byte(want)) {
				t.Fatalf("signature mismatch:\n got  %s\n want %s (uri=%s)", v1, want, u)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("postback never delivered")
}

// PB8: unsigned when no secret — no header ever (backward compatible).
func TestPostback_UnsignedByDefault(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()

	var sawHeader atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-AE-Signature") != "" {
			sawHeader.Store(true)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	h.DB.Exec(`INSERT INTO affiliates (id, email, postback_url) VALUES (1, 'a@t.dev', ?)`, srv.URL+"/pb")
	seedCommission(t, h, 403, 1, 100)

	e := postbacks.NewEmitter(h.DB)
	e.AllowLocalhost = true
	e.Start(ctx)
	defer e.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var delivered int
		h.DB.QueryRow(`SELECT COUNT(*) FROM postback_state WHERE delivered_at IS NOT NULL`).Scan(&delivered)
		if delivered == 1 {
			if sawHeader.Load() {
				t.Fatal("signature header present without a secret")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("postback never delivered")
}
