package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// autoCodeMax is the longest code approval mints, suffix included.
const autoCodeMax = 20

// autoCodeBase derives the stem of an approved affiliate's first referral
// code: the name lowercased to [a-z0-9] ("Jane Smith" -> "janesmith"),
// else the email's local part, else "partner". The stem always contains a
// letter, so it can never shadow the numeric-id fallback of /r/{id}.
func autoCodeBase(name, email string) string {
	for _, src := range []string{name, strings.SplitN(email, "@", 2)[0]} {
		var b strings.Builder
		letter := false
		for _, c := range strings.ToLower(src) {
			switch {
			case c >= 'a' && c <= 'z':
				letter = true
				b.WriteRune(c)
			case c >= '0' && c <= '9':
				b.WriteRune(c)
			}
		}
		if s := b.String(); len(s) >= 2 && letter {
			if len(s) > autoCodeMax {
				s = s[:autoCodeMax]
			}
			return s
		}
	}
	return "partner"
}

// autoCodeCandidate is the n-th try for base: base, base2, base3, ...,
// with the stem cut so the whole code stays within autoCodeMax.
func autoCodeCandidate(base string, n int) string {
	if n <= 1 {
		return base
	}
	suf := strconv.Itoa(n)
	if len(base)+len(suf) > autoCodeMax {
		base = base[:autoCodeMax-len(suf)]
	}
	return base + suf
}

// ensurePrimaryCode gives affiliateID a native primary referral code unless
// it already has any code, and returns the affiliate's primary (or first)
// code. It runs inside the approval transaction; ON CONFLICT DO NOTHING
// keeps the transaction usable on Postgres when a candidate is taken, and a
// concurrent approval claiming the same code simply moves us to the next.
func ensurePrimaryCode(ctx context.Context, tx *sql.Tx, affiliateID int64, name, email string) (string, error) {
	var existing string
	err := tx.QueryRowContext(ctx, `
		SELECT code FROM affiliate_codes WHERE affiliate_id = ?
		ORDER BY is_primary DESC, id LIMIT 1`, affiliateID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	base := autoCodeBase(name, email)
	for n := 1; n <= 60; n++ {
		code := autoCodeCandidate(base, n)
		if n == 60 { // pathological crowding: fall back to a random tail
			var r [3]byte
			if _, err := rand.Read(r[:]); err != nil {
				return "", err
			}
			stem := base
			if len(stem) > autoCodeMax-6 {
				stem = stem[:autoCodeMax-6]
			}
			code = stem + hex.EncodeToString(r[:])
		}
		var id int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source)
			VALUES (?, ?, TRUE, 'native')
			ON CONFLICT DO NOTHING
			RETURNING id`, affiliateID, code).Scan(&id)
		if err == nil {
			return code, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", errors.New("no free referral code")
}
