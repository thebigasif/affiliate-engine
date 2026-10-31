// Referral-code management endpoints: slugs for /r/{code} (the substrate for
// imported incumbent links, FR-32 slug preservation).
package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// handleCodes: GET ?affiliate_id=&limit=&after_id= list / POST create /
// DELETE ?code=.
//
// POST body: {"affiliate_id":1,"code":"asif","primary":true}
// Codes are normalized to lowercase; [a-z0-9-_] only, 2..32 chars.
func (a *api) handleCodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, afterID, err := pageParams(r)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		where := " WHERE c.id > ?"
		args := []any{afterID}
		if q := r.URL.Query().Get("affiliate_id"); q != "" {
			affID, convErr := strconv.ParseInt(q, 10, 64)
			if convErr != nil {
				apiError(w, http.StatusBadRequest, "affiliate_id must be an integer")
				return
			}
			where += " AND c.affiliate_id = ?"
			args = append(args, affID)
		}
		args = append(args, limit)
		rows, err := a.db.QueryContext(ctx(r), `
			SELECT c.id, c.affiliate_id, c.code, c.is_primary, c.source, c.created_at
			FROM affiliate_codes c`+where+` ORDER BY c.id LIMIT ?`, args...)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		type code struct {
			ID          int64  `json:"id"`
			AffiliateID int64  `json:"affiliate_id"`
			Code        string `json:"code"`
			Primary     bool   `json:"primary"`
			Source      string `json:"source"`
			CreatedAt   string `json:"created_at"`
		}
		out := []code{}
		for rows.Next() {
			var x code
			if err := rows.Scan(&x.ID, &x.AffiliateID, &x.Code, &x.Primary, &x.Source, &x.CreatedAt); err != nil {
				apiError(w, http.StatusInternalServerError, "scan failed")
				return
			}
			out = append(out, x)
		}
		if err := rows.Err(); err != nil {
			apiError(w, http.StatusInternalServerError, "query iteration failed")
			return
		}
		var next int64
		if len(out) == limit {
			next = out[len(out)-1].ID
		}
		writeJSON(w, http.StatusOK, map[string]any{"codes": out, "next_after_id": next})
	case http.MethodPost:
		var req struct {
			AffiliateID int64  `json:"affiliate_id"`
			Code        string `json:"code"`
			Primary     bool   `json:"primary"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid body")
			return
		}
		code := normalizeCode(req.Code)
		if code == "" {
			apiError(w, http.StatusBadRequest, "code required: 2-32 chars, [a-z0-9_-]")
			return
		}
		if req.AffiliateID <= 0 {
			apiError(w, http.StatusBadRequest, "affiliate_id required")
			return
		}
		var exists int
		if err := a.db.QueryRowContext(ctx(r),
			`SELECT COUNT(*) FROM affiliates WHERE id = ?`, req.AffiliateID).Scan(&exists); err != nil || exists == 0 {
			apiError(w, http.StatusBadRequest, "unknown affiliate")
			return
		}
		// One primary code per affiliate: demote existing on promote.
		if req.Primary {
			if _, err := a.db.ExecContext(ctx(r),
				`UPDATE affiliate_codes SET is_primary = FALSE WHERE affiliate_id = ? AND is_primary = TRUE`,
				req.AffiliateID); err != nil {
				apiError(w, http.StatusInternalServerError, "demote failed")
				return
			}
		}
		var id int64
		err := a.db.QueryRowContext(ctx(r), `
			INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source)
			VALUES (?, ?, ?, 'native')
			ON CONFLICT(affiliate_id, code) DO UPDATE SET is_primary = excluded.is_primary
			RETURNING id`,
			req.AffiliateID, code, req.Primary).Scan(&id)
		if err != nil {
			// Global UNIQUE(code) clash (different affiliate, same code):
			// SQLite says "UNIQUE constraint failed", PG says "duplicate key
			// value violates unique constraint" — match both, case-blind.
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				apiError(w, http.StatusConflict, "code already taken")
				return
			}
			apiError(w, http.StatusInternalServerError, "create failed")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "code": code})
	case http.MethodDelete:
		code := normalizeCode(r.URL.Query().Get("code"))
		if code == "" {
			apiError(w, http.StatusBadRequest, "code required")
			return
		}
		res, err := a.db.ExecContext(ctx(r), `DELETE FROM affiliate_codes WHERE code = ?`, code)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "delete failed")
			return
		}
		n, _ := res.RowsAffected()
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
	default:
		apiError(w, http.StatusMethodNotAllowed, "GET, POST or DELETE")
	}
}

// normalizeCode lowercases and validates the slug shape.
func normalizeCode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) < 2 || len(s) > 32 {
		return ""
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return ""
		}
	}
	return s
}
