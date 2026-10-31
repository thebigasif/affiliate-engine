package httpapi

// "Test your setup": a guided, live check that tracking works end to end.
//
//  1. Site: fetch the owner's page (SSRF-guarded client) and look for the
//     tracking script.
//  2. Click: a test partner ("Test partner (you)", code setup-test) whose
//     link the owner opens; the dashboard polls until the click lands.
//  3. Sale: the owner buys through that link in their billing tool's test
//     mode; the dashboard polls until the commission is credited.
//
// The test partner is a normal partner (so the check exercises the real
// path), opted out of program emails, that the owner pauses when done.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbutil"
	"github.com/thebigasif/affiliate-engine/internal/postbacks"
)

const (
	setupTestEmail = "setup-test@example.com"
	setupTestCode  = "setup-test"
	setupTestName  = "Test partner (you)"
)

// scriptSrcRE finds the tracking script's src on a page.
var scriptSrcRE = regexp.MustCompile(`(?i)<script[^>]+src\s*=\s*["']([^"']*/t\.js(?:\?[^"']*)?)["']`)

func (a *api) siteClient() *http.Client {
	if a.deps.SiteFetch != nil {
		return a.deps.SiteFetch
	}
	return postbacks.NewEmitter(a.db).Client() // resolves once, public IPs only
}

// handleSetupCheckSite: POST /api/v1/admin/setup-check/site {"url": "..."}.
func (a *api) handleSetupCheckSite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	raw := strings.TrimSpace(req.URL)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		apiError(w, http.StatusBadRequest, "Enter your website's address, like https://yoursite.com")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	hreq.Header.Set("User-Agent", "AffiliateTracking setup check")
	hreq.Header.Set("Accept", "text/html")
	resp, err := a.siteClient().Do(hreq)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"url": u.String(), "reached": false,
			"problem": "We couldn't open that page. Check the address, and that the site is online.",
		})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		writeJSON(w, http.StatusOK, map[string]any{
			"url": u.String(), "reached": false,
			"problem": "That page answered with an error (HTTP " + http.StatusText(resp.StatusCode) + ").",
		})
		return
	}
	m := scriptSrcRE.FindSubmatch(body)
	out := map[string]any{"url": u.String(), "reached": true, "found": m != nil}
	if m != nil {
		out["src"] = string(m[1])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSetupCheckStart: POST /api/v1/admin/setup-check/start makes (or
// resumes) the test partner and returns their link.
func (a *api) handleSetupCheckStart(w http.ResponseWriter, r *http.Request) {
	id, err := a.ensureTestPartner(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "couldn't create the test partner")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"affiliate_id": id, "code": setupTestCode,
		"started_at": time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *api) ensureTestPartner(ctx context.Context) (int64, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM affiliates WHERE email = ?`, setupTestEmail).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `INSERT INTO affiliates (email, name) VALUES (?, ?) RETURNING id`, setupTestEmail, setupTestName).Scan(&id)
	}
	if err != nil {
		return 0, err
	}
	// Active again if paused after an earlier check; no program emails.
	if _, err := tx.ExecContext(ctx, `UPDATE affiliates SET status = 'active', news_opt_out = 1 WHERE id = ?`, id); err != nil {
		return 0, err
	}
	var owner int64
	err = tx.QueryRowContext(ctx, `SELECT affiliate_id FROM affiliate_codes WHERE code = ?`, setupTestCode).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO affiliate_codes (affiliate_id, code, is_primary) VALUES (?, ?, TRUE)`, id, setupTestCode)
	} else if err == nil && owner != id {
		err = errors.New("the setup-test code belongs to another partner")
	}
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// handleSetupCheck: GET /api/v1/admin/setup-check?since=RFC3339 — what the
// test partner has had since the check started.
func (a *api) handleSetupCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	since := r.URL.Query().Get("since")
	t, err := time.Parse(time.RFC3339, since)
	if err != nil {
		t = time.Now().UTC().Add(-time.Hour)
	}
	var id int64
	err = a.db.QueryRowContext(ctx, `SELECT id FROM affiliates WHERE email = ?`, setupTestEmail).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"started": false})
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	// Clicks: the raw click log (ts is RFC3339 text on both engines).
	var clicks int64
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM clicks WHERE affiliate_id = ? AND ts >= ?`,
		id, t.UTC().Format(time.RFC3339)).Scan(&clicks); err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	type sale struct {
		AmountCents int64  `json:"amount_cents"`
		Currency    string `json:"currency"`
		Status      string `json:"status"`
	}
	sales := []sale{}
	rows, err := a.db.QueryContext(ctx, `
		SELECT amount, currency, status, created_at FROM commissions
		WHERE affiliate_id = ? AND amount > 0 ORDER BY id DESC LIMIT 20`, id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var s sale
		var created any
		if err := rows.Scan(&s.AmountCents, &s.Currency, &s.Status, &created); err != nil {
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		if at, ok := dbutil.AsTime(created); ok && !at.Before(t.Add(-time.Minute)) {
			sales = append(sales, s)
		}
	}
	var status string
	_ = a.db.QueryRowContext(ctx, `SELECT status FROM affiliates WHERE id = ?`, id).Scan(&status)
	writeJSON(w, http.StatusOK, map[string]any{
		"started": true, "affiliate_id": id, "status": status,
		"code": setupTestCode, "clicks": clicks, "sales": sales,
	})
}
