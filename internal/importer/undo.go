package importer

// undo.go — one-command reversal of an import batch.
//
// Undo removes exactly what the batch created: customer bindings, coupons,
// referral codes, the affiliates it created and their (still empty)
// payable ledger accounts. It runs in one transaction and changes nothing
// unless every step can run.
//
// Money invariant: an import posts no journal, commission or payout, so
// there is never anything of its own to reverse. What CAN happen is money
// moving AFTER the import against something it created: an imported
// affiliate earns a commission or is paid, a customer binding or coupon
// credits a sale. Those rows are append-only (guard triggers) and real,
// so Undo refuses and names them; the operator freezes the affiliate
// instead. No reversing entries are ever posted by Undo. Undo also refuses
// when a referral code the batch created (on a new or a pre-existing
// affiliate) has clicks or attributions since the import.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrUndoRefused wraps every refusal (money moved, or other data now
// hangs off what the batch created).
var ErrUndoRefused = errors.New("importer: undo refused")

// UndoReport says what an Undo removed.
type UndoReport struct {
	BatchID        int64  `json:"batch_id"`
	Source         string `json:"source"`
	AlreadyUndone  bool   `json:"already_undone,omitempty"`
	Affiliates     int    `json:"affiliates"`
	Codes          int    `json:"codes"`
	Coupons        int    `json:"coupons"`
	Bindings       int    `json:"customer_bindings"`
	LedgerAccounts int    `json:"ledger_accounts"` // empty payable accounts removed
}

// Undo reverses one import batch. Running it again on an undone batch is
// a no-op that reports AlreadyUndone.
func Undo(ctx context.Context, db *sql.DB, batchID int64) (*UndoReport, error) {
	importMu.Lock()
	defer importMu.Unlock()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var payload string
	err = tx.QueryRowContext(ctx, `SELECT undo_json FROM import_undos WHERE batch_id = ?`, batchID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("importer: no import batch %d", batchID)
	}
	if err != nil {
		return nil, err
	}
	var doc undoDoc
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return nil, fmt.Errorf("importer: batch %d undo file unreadable: %w", batchID, err)
	}
	rep := &UndoReport{BatchID: batchID, Source: doc.Source}

	// Batches from before import_batch_state get a state row on first undo.
	var undone sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT CAST(undone_at AS TEXT) FROM import_batch_state WHERE batch_id = ?`, batchID).Scan(&undone)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_batch_state (batch_id) VALUES (?)`, batchID); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case undone.Valid:
		rep.AlreadyUndone = true
		return rep, nil
	}

	var createdIDs []int64
	for _, a := range doc.Affiliates {
		if a.Created {
			createdIDs = append(createdIDs, a.AffiliateID)
		}
	}

	var why []string
	count := func(q string, args ...any) (int, error) {
		var n int
		err := tx.QueryRowContext(ctx, q, args...).Scan(&n)
		return n, err
	}
	// 1. Money: anything that moved against what this batch created.
	for _, id := range createdIDs {
		label := affLabel(doc, id)
		checks := []struct {
			q, what string
			arg     any
		}{
			{`SELECT COUNT(*) FROM commissions WHERE affiliate_id = ?`, "commissions", id},
			{`SELECT COUNT(*) FROM payouts WHERE affiliate_id = ?`, "payouts", id},
			{`SELECT COUNT(*) FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
			  WHERE la.code LIKE ? ESCAPE '!'`, "ledger entries", payableCodes(id)},
		}
		for _, c := range checks {
			n, err := count(c.q, c.arg)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				why = append(why, fmt.Sprintf("affiliate %s has %d %s since the import (money moved)", label, n, c.what))
			}
		}
	}
	for _, c := range doc.Customers {
		n, err := count(`
			SELECT COUNT(*) FROM commissions c JOIN customers cu
			  ON cu.external_customer_id = c.customer_ext_id AND cu.affiliate_id = c.affiliate_id
			WHERE cu.id = ?`, c.CustomerID)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			why = append(why, fmt.Sprintf("imported customer binding %d credited %d commissions (money moved)", c.CustomerID, n))
		}
	}
	for _, a := range doc.Affiliates {
		if a.Coupon == "" {
			continue
		}
		n, err := count(`SELECT COUNT(*) FROM commissions WHERE coupon_code = ? AND affiliate_id = ?`, a.Coupon, a.AffiliateID)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			why = append(why, fmt.Sprintf("imported coupon %s credited %d commissions (money moved)", a.Coupon, n))
		}
	}

	// 1b. Traffic through a referral code this batch created, on ANY
	// affiliate (a pre-existing affiliate the batch only attached a code to
	// included): deleting the code would break links already in use and
	// lose which code those clicks and attributions came through.
	since := undoCutoff(doc.ImportedAt)
	for _, a := range doc.Affiliates {
		if a.Code == "" {
			continue
		}
		for _, c := range []struct{ q, what string }{
			{`SELECT COUNT(*) FROM clicks WHERE ref_code = ? AND ts >= ?`, "clicks"},
			{`SELECT COUNT(*) FROM attributions at JOIN clicks c ON c.id = at.click_id
			  WHERE c.ref_code = ? AND at.created_at >= ?`, "attributions"},
		} {
			n, err := count(c.q, a.Code, since)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				why = append(why, fmt.Sprintf("imported referral code %s (affiliate %s) has %d %s since the import", a.Code, affLabel(doc, a.AffiliateID), n, c.what))
			}
		}
	}

	// 2. Other data now hanging off affiliates this batch created: deleting
	// them would orphan it (or fail on the foreign key).
	ours := batchRows(doc)
	for _, id := range createdIDs {
		label := affLabel(doc, id)
		for _, c := range []struct{ q, what string }{
			{`SELECT COUNT(*) FROM clicks WHERE affiliate_id = ?`, "clicks"},
			{`SELECT COUNT(*) FROM attributions WHERE affiliate_id = ?`, "attributions"},
			{`SELECT COUNT(*) FROM affiliate_sessions WHERE affiliate_id = ?`, "portal sessions"},
			{`SELECT COUNT(*) FROM affiliates WHERE sponsor_id = ? OR placement_parent_id = ?`, "recruited affiliates"},
		} {
			args := []any{id}
			if strings.Count(c.q, "?") == 2 {
				args = append(args, id)
			}
			n, err := count(c.q, args...)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				why = append(why, fmt.Sprintf("affiliate %s has %d %s since the import", label, n, c.what))
			}
		}
		for _, t := range []struct{ tbl, key, what string }{
			{"affiliate_codes", "code", "referral codes"},
			{"coupons", "code", "coupons"},
			{"customers", "id", "customer bindings"},
		} {
			n, err := foreignRows(ctx, tx, t.tbl, t.key, id, ours[t.tbl])
			if err != nil {
				return nil, err
			}
			if n > 0 {
				why = append(why, fmt.Sprintf("affiliate %s has %d %s this batch did not create (added later: undo later batches or remove them first)", label, n, t.what))
			}
		}
	}
	if len(why) > 0 {
		return nil, fmt.Errorf("%w: batch %d: %s", ErrUndoRefused, batchID, strings.Join(why, "; "))
	}

	// 3. Remove, newest dependents first.
	for _, c := range doc.Customers {
		res, err := tx.ExecContext(ctx, `DELETE FROM customers WHERE id = ? AND affiliate_id = ? AND bound_method = ?`,
			c.CustomerID, c.AffiliateID, "import:"+doc.Source)
		if err != nil {
			return nil, fmt.Errorf("importer: undo binding %d: %w", c.CustomerID, err)
		}
		rep.Bindings += affected(res)
	}
	for _, a := range doc.Affiliates {
		if a.Coupon != "" {
			res, err := tx.ExecContext(ctx, `DELETE FROM coupons WHERE affiliate_id = ? AND code = ?`, a.AffiliateID, a.Coupon)
			if err != nil {
				return nil, fmt.Errorf("importer: undo coupon %s: %w", a.Coupon, err)
			}
			rep.Coupons += affected(res)
		}
		if a.Code != "" {
			res, err := tx.ExecContext(ctx, `DELETE FROM affiliate_codes WHERE affiliate_id = ? AND code = ?`, a.AffiliateID, a.Code)
			if err != nil {
				return nil, fmt.Errorf("importer: undo code %s: %w", a.Code, err)
			}
			rep.Codes += affected(res)
		}
	}
	for _, id := range createdIDs {
		res, err := tx.ExecContext(ctx, `DELETE FROM affiliates WHERE id = ?`, id)
		if err != nil {
			return nil, fmt.Errorf("%w: batch %d: affiliate %s still has data attached (%v)", ErrUndoRefused, batchID, affLabel(doc, id), err)
		}
		rep.Affiliates += affected(res)
		// Its payable accounts, one per currency: empty (checked above),
		// and not ledger rows, so they can go.
		res, err = tx.ExecContext(ctx, `
			DELETE FROM ledger_accounts WHERE code LIKE ? ESCAPE '!'
			  AND NOT EXISTS (SELECT 1 FROM ledger_entries le WHERE le.account_id = ledger_accounts.id)`, payableCodes(id))
		if err != nil {
			return nil, fmt.Errorf("importer: undo payable account %d: %w", id, err)
		}
		rep.LedgerAccounts += affected(res)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE import_batch_state SET undone_at = ? WHERE batch_id = ?`,
		time.Now().UTC().Format(time.RFC3339), batchID); err != nil {
		return nil, err
	}
	detail := fmt.Sprintf("undo %s: affiliates=%d codes=%d coupons=%d bindings=%d",
		doc.Source, rep.Affiliates, rep.Codes, rep.Coupons, rep.Bindings)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (actor, table_name, row_id, action, detail) VALUES ('system', 'import_batches', ?, 'undo', ?)`,
		batchID, detail); err != nil {
		return nil, err
	}
	return rep, tx.Commit()
}

// undoCutoff is the lower bound for "since the import" traffic checks: the
// batch's import time less one second (RFC3339 text compares correctly
// against SQLite's RFC3339Nano click timestamps and PG timestamptz alike).
// An unreadable time counts everything: refusing is the safe side.
func undoCutoff(importedAt string) string {
	t, err := time.Parse(time.RFC3339, importedAt)
	if err != nil {
		return "1970-01-01T00:00:00Z"
	}
	return t.UTC().Add(-time.Second).Format(time.RFC3339)
}

// payableCodes is the LIKE pattern (escape '!') of an affiliate's payable
// accounts in every currency: AFFILIATE_PAYABLE:<id>:<CCY>.
func payableCodes(affID int64) string { return fmt.Sprintf("AFFILIATE!_PAYABLE:%d:%%", affID) }

func affected(res sql.Result) int {
	n, _ := res.RowsAffected()
	return int(n)
}

func affLabel(doc undoDoc, id int64) string {
	for _, a := range doc.Affiliates {
		if a.AffiliateID == id && a.Email != "" {
			return fmt.Sprintf("%d (%s)", id, a.Email)
		}
	}
	return fmt.Sprint(id)
}

// batchRows lists, per table, the keys this batch created (per affiliate).
func batchRows(doc undoDoc) map[string]map[int64]map[string]bool {
	out := map[string]map[int64]map[string]bool{"affiliate_codes": {}, "coupons": {}, "customers": {}}
	put := func(tbl string, aff int64, key string) {
		if out[tbl][aff] == nil {
			out[tbl][aff] = map[string]bool{}
		}
		out[tbl][aff][key] = true
	}
	for _, a := range doc.Affiliates {
		if a.Code != "" {
			put("affiliate_codes", a.AffiliateID, a.Code)
		}
		if a.Coupon != "" {
			put("coupons", a.AffiliateID, a.Coupon)
		}
	}
	for _, c := range doc.Customers {
		put("customers", c.AffiliateID, fmt.Sprint(c.CustomerID))
	}
	return out
}

// foreignRows counts an affiliate's rows in tbl that the batch did not
// create. tbl and key come from a fixed list in Undo, never from input.
func foreignRows(ctx context.Context, tx *sql.Tx, tbl, key string, affID int64, ours map[int64]map[string]bool) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT CAST(`+key+` AS TEXT) FROM `+tbl+` WHERE affiliate_id = ?`, affID) // #nosec G202 -- tbl/key are constants chosen by Undo
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return 0, err
		}
		if !ours[affID][k] {
			n++
		}
	}
	return n, rows.Err()
}

// Batch is one row of the import history.
type Batch struct {
	ID         int64  `json:"id"`
	Source     string `json:"source"`
	Affiliates int    `json:"affiliates"`
	ImportedAt string `json:"imported_at"`
	UndoneAt   string `json:"undone_at,omitempty"`
}

// Batches lists every recorded import, newest first.
func Batches(ctx context.Context, db *sql.DB) ([]Batch, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT b.id, b.source, b.affiliates, CAST(b.imported_at AS TEXT), COALESCE(CAST(s.undone_at AS TEXT), '')
		FROM import_batches b LEFT JOIN import_batch_state s ON s.batch_id = b.id
		ORDER BY b.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Batch
	for rows.Next() {
		var b Batch
		if err := rows.Scan(&b.ID, &b.Source, &b.Affiliates, &b.ImportedAt, &b.UndoneAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
