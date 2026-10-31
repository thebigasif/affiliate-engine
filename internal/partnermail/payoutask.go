package partnermail

import (
	"context"
	"database/sql"
	"time"
)

// askGap: a partner who still hasn't said where to be paid is asked again
// after a week, not every time a payout run happens.
const askGap = 7 * 24 * time.Hour

// AskPayoutDetails emails every active partner with a payout waiting who
// hasn't said where to be paid, at most once a week each. It returns how
// many it asked. Safe to run often: the payout run calls it right away
// and a ticker catches the rest.
func AskPayoutDetails(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	cutoff := now.UTC().Add(-askGap).Format(time.RFC3339)
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT p.affiliate_id FROM payouts p
		JOIN affiliates a ON a.id = p.affiliate_id AND a.status = 'active'
		WHERE p.status = 'requested'
		  AND NOT EXISTS (SELECT 1 FROM affiliate_payout_details d WHERE d.affiliate_id = p.affiliate_id)
		  AND NOT EXISTS (SELECT 1 FROM payout_details_asks k WHERE k.affiliate_id = p.affiliate_id AND k.asked_at > ?)
		ORDER BY p.affiliate_id`, cutoff)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	asked := 0
	stamp := now.UTC().Format(time.RFC3339)
	for _, id := range ids {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return asked, err
		}
		// The ask and its email land together, so a crash never asks twice
		// or records an ask that wasn't queued.
		res, err := tx.ExecContext(ctx, `
			INSERT INTO payout_details_asks (affiliate_id, asked_at) VALUES (?, ?)
			ON CONFLICT (affiliate_id) DO UPDATE SET asked_at = excluded.asked_at
			WHERE payout_details_asks.asked_at <= ?`, id, stamp, cutoff)
		if err == nil {
			var n int64
			if n, err = res.RowsAffected(); err == nil && n == 1 {
				err = EnqueueLoginLink(ctx, tx, id, ReasonPayoutDetails)
			}
			if err == nil && n != 1 {
				_ = tx.Rollback()
				continue // another run asked in the meantime
			}
		}
		if err != nil {
			_ = tx.Rollback()
			return asked, err
		}
		if err := tx.Commit(); err != nil {
			return asked, err
		}
		asked++
	}
	return asked, nil
}

// PayoutDetailsAsked is when each partner was last asked, for the admin.
func PayoutDetailsAsked(ctx context.Context, db *sql.DB, ids []int64) map[int64]string {
	out := map[int64]string{}
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var at string
		if err := db.QueryRowContext(ctx, `SELECT asked_at FROM payout_details_asks WHERE affiliate_id = ?`, id).Scan(&at); err == nil {
			out[id] = at
		}
	}
	return out
}
