package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// f3Env: fresh DB, affiliate 1 with a PENDING commission (money in HOLD)
// and an open fraud hold. The commission's matures_at is NOW — Mature()
// will drain HOLD into payable the moment it runs.
func f3Env(t *testing.T) (*httptest.Server, *db.Handles, *ledger.Poster, string, int64) {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)
	poster := ledger.NewPoster(h.Ledger)
	poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD": "liability", "EXPENSE:COMMISSION:USD": "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
	})
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'f3@t.dev')`)
	now := time.Now().UTC()
	// The commission EXACTLY as the engine posts it at credit time
	// (commissions.go:127): expense +amount, HOLD -amount. NOTE: no raw
	// journal_entries pre-insert — the Post below creates the journal; a
	// pre-inserted row with the same idem key made Post a silent no-op
	// (ErrDuplicate dedupe), so the seed journal never landed at all and
	// the fixture started from HOLD=0.
	jid, err := poster.Post(ctx, "f3j", "conversion", "f3", now, []ledger.Line{
		{Account: "EXPENSE:COMMISSION:USD", Amount: 1000},
		{Account: "HOLD:USD", Amount: -1000},
	})
	if err != nil {
		t.Fatalf("seed post: %v", err)
	}
	h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at, matures_at)
		VALUES (1, ?, 1000, '{}', 'pending', ?, ?)`, jid, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	var cid int64
	h.DB.QueryRow(`SELECT id FROM commissions WHERE journal_id = ?`, jid).Scan(&cid)
	h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason) VALUES (?, 'f3 test')`, cid)

	key := "ae_TestKeyF3Race00000000000000000000"
	sum := sha256.Sum256([]byte(key))
	exec := func(q string, args ...any) {
		if _, err := h.DB.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO api_keys (prefix, key_hash, scope, name) VALUES ('ae_TestKeyF3', ?, 'admin', 'f3')`,
		hex.EncodeToString(sum[:]))
	srv := httptest.NewServer(httpapi.NewAPIHandler(httpapi.APIDeps{
		DB:           h.DB,
		RateLimit:    6000,
		LedgerPoster: poster,
	}))
	t.Cleanup(srv.Close)
	return srv, h, poster, key, cid
}

// F3 pin: Mature() racing fraud-uphold. Whichever wins, the clawback must
// draw from the account that ACTUALLY holds the money — HOLD and payable
// must both return to 0 for this commission's 1000.
func TestFraudUphold_MaturationRace(t *testing.T) {
	srv, h, poster, key, cid := f3Env(t)
	_ = poster

	// Fire uphold while maturation runs concurrently.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/admin/fraud/resolve",
			jsonBody(fmt.Sprintf(`{"commission_id":%d,"decision":"uphold","note":"race"}`, cid)))
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
	}()
	go func() {
		defer wg.Done()
		// REAL engine maturation (production shape: moneylock + CAS discipline
		// — posts only when its status transition wins). The previous version
		// hand-rolled this and posted unconditionally, creating ledger states
		// production cannot make (a maturation journal for a cancelled
		// commission) whenever uphold won the race — CI's slower runners hit
		// that interleaving ~1-in-N; local never did.
		eng := commissions.NewEngine(h.DB, poster)
		if _, err := eng.Mature(context.Background()); err != nil {
			t.Logf("mature: %v (engine reverts on failure; race outcome still valid)", err)
		}
	}()
	wg.Wait()

	// After BOTH paths ran, the net effect on HOLD and payable must be
	// zero: credit +1000 HOLD, mature -1000 HOLD, claw -/+ somewhere —
	// the claw must offset whichever debit actually happened.
	// NOTE: ledger reads JOIN ledger_accounts on code; the naive
	// `WHERE account = ?` column does not exist and Scan would swallow
	// the error into 0 (vacuous pass).
	bal := func(acct string) int64 {
		var v int64
		if err := h.DB.QueryRow(`SELECT COALESCE(SUM(le.amount),0)
			FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
			WHERE la.code = ?`, acct).Scan(&v); err != nil {
			t.Fatalf("balance(%s): %v", acct, err)
		}
		return v
	}
	holdBal, payBal := bal("HOLD:USD"), bal("AFFILIATE_PAYABLE:1:USD")
	if holdBal != 0 {
		t.Fatalf("F3: HOLD residual %d (claw hit wrong account)", holdBal)
	}
	if payBal != 0 {
		t.Fatalf("F3: payable residual %d (phantom payable)", payBal)
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM commissions WHERE id = ?`, cid).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("commission not cancelled: %s", status)
	}
}

func jsonBody(s string) io.Reader { return strings.NewReader(s) }
