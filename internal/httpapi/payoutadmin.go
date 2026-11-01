// Admin payout console routes: list payouts (so the UI can offer an
// Approve button per row), download the staged batch as a PayPal or Wise
// CSV (manual rails), and program payout settings.
package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/export"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
	"github.com/thebigasif/affiliate-engine/internal/settings"
)

var payoutStatuses = map[string]bool{
	"requested": true, "approved": true, "processing": true, "paid": true, "failed": true,
}

// handlePayoutList: GET ?status=requested&limit=&after_id= (admin).
// status defaults to 'requested' (what awaits approval); "all" lists every
// status.
func (a *api) handlePayoutList(w http.ResponseWriter, r *http.Request) {
	limit, afterID, err := pageParams(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if status == "" {
		status = "requested"
	}
	q := `SELECT p.id, p.affiliate_id, COALESCE(a.email, ''), p.amount, p.currency,
	             p.method, p.status, p.created_at, p.processed_at,
	             p.exported_at, COALESCE(p.export_batch, ''), COALESCE(p.attention, '')
	      FROM payouts p JOIN affiliates a ON a.id = p.affiliate_id
	      WHERE p.id > ?`
	var rows *sql.Rows
	switch {
	case status == "all":
		rows, err = a.db.QueryContext(r.Context(), q+` ORDER BY p.id LIMIT ?`, afterID, limit)
	case payoutStatuses[status]:
		rows, err = a.db.QueryContext(r.Context(), q+` AND p.status = ? ORDER BY p.id LIMIT ?`, afterID, status, limit)
	default:
		apiError(w, http.StatusBadRequest, "status must be requested|approved|processing|paid|failed|all")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	type payout struct {
		ID          int64  `json:"id"`
		AffiliateID int64  `json:"affiliate_id"`
		Email       string `json:"email"`
		AmountCents int64  `json:"amount_cents"`
		Currency    string `json:"currency"`
		Method      string `json:"method"`
		Status      string `json:"status"`
		CreatedAt   string `json:"created_at"`
		ProcessedAt string `json:"processed_at"`
		ExportedAt  string `json:"exported_at"`
		ExportBatch string `json:"export_batch"`
		Attention   string `json:"attention"`
		PayTo       string `json:"pay_to"`                    // where the partner asked to be paid ("" = not given)
		PayToAsked  string `json:"pay_to_asked_at,omitempty"` // when we last emailed them to add it
		Held        string `json:"held,omitempty"`            // why it can't be paid yet (payout details changed or unreadable)
	}
	out := []payout{}
	for rows.Next() {
		var p payout
		var processed, exported sql.NullString
		if err := rows.Scan(&p.ID, &p.AffiliateID, &p.Email, &p.AmountCents, &p.Currency,
			&p.Method, &p.Status, &p.CreatedAt, &processed, &exported, &p.ExportBatch, &p.Attention); err != nil {
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		p.ProcessedAt = processed.String
		p.ExportedAt = exported.String
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		apiError(w, http.StatusInternalServerError, "query iteration failed")
		return
	}
	ids := make([]int64, 0, len(out))
	for _, p := range out {
		ids = append(ids, p.AffiliateID)
	}
	details := a.payToByAffiliate(r.Context(), ids)
	asked := partnermail.PayoutDetailsAsked(r.Context(), a.db, ids)
	held := map[int64]string{}
	for i := range out {
		out[i].PayTo = details[out[i].AffiliateID].PayTo()
		if out[i].PayTo == "" {
			out[i].PayToAsked = asked[out[i].AffiliateID]
		}
		if out[i].Status != "requested" || out[i].ExportedAt != "" {
			continue
		}
		reason, ok := held[out[i].AffiliateID]
		if !ok {
			if reason, err = a.payoutHold(r.Context(), out[i].AffiliateID, false); err != nil {
				apiError(w, http.StatusInternalServerError, "query failed")
				return
			}
			held[out[i].AffiliateID] = reason
		}
		out[i].Held = reason
	}
	var next int64
	if len(out) == limit {
		next = out[len(out)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"payouts": out, "next_after_id": next})
}

// handlePayoutExport: GET /api/v1/admin/payouts/export/{rail} (admin),
// rail = paypal.csv | wise.csv.
//
// The file lists only 'requested' payouts whose method is the rail and
// that pass the same payment gate Approve runs (payouts.Runner.
// ClaimForExport: no open fraud hold, no pending/rejected tax document,
// every member commission still 'approved', no clawback since staging).
// Each listed payout is claimed (exported_at / export_batch) under its
// money locks, so it never appears in a second file and is never
// restaged into a new run. Payouts the gate blocks now are left out and
// counted in X-Payouts-Skipped. Status stays 'requested': the operator
// pays the file, then approves each payout, which re-checks the gate and
// settles the ledger without running a rail. An exported payout the gate
// blocks at approval is flagged (payouts.attention), never restaged.
func (a *api) handlePayoutExport(w http.ResponseWriter, r *http.Request) {
	var method string
	var write func(io.Writer, []export.Row) error
	switch r.PathValue("rail") {
	case "paypal.csv":
		method, write = "paypal", export.PayPal
	case "wise.csv":
		method, write = "wise", export.Wise
	default:
		apiError(w, http.StatusNotFound, "rail must be paypal.csv or wise.csv")
		return
	}
	runner := a.deps.PayoutRunner
	if runner == nil {
		runner = payouts.NewRunner(a.db)
		a.guardPayouts(runner)
	}
	// ExportPayee (guardPayouts) picks each payee: the address the partner
	// gave for this rail, else their login email. A partner whose details
	// are cooling or cannot be read is left out (X-Payouts-Skipped).
	res, err := runner.ClaimForExport(r.Context(), method)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "export failed")
		return
	}
	rows := make([]export.Row, 0, len(res.Payouts))
	for _, p := range res.Payouts {
		rows = append(rows, export.Row{PayoutID: p.PayoutID, Email: p.Email, Amount: p.Amount, Currency: p.Currency})
	}
	var buf bytes.Buffer
	if err := write(&buf, rows); err != nil {
		apiError(w, http.StatusInternalServerError, "export failed")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="payouts-%s.csv"`, res.Batch))
	h.Set("X-Export-Batch", res.Batch)
	h.Set("X-Payouts-Exported", strconv.Itoa(len(rows)))
	h.Set("X-Payouts-Skipped", strconv.Itoa(res.Skipped))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// handleSettings: GET / PUT program payout settings (admin).
// PUT body: {"min_payout_cents":5000,"default_hold_days":30,"currency":"USD",
// "min_payout_by_currency":{"JPY":5000}}; a field left
// out keeps its current value.
func (a *api) handleSettings(w http.ResponseWriter, r *http.Request) {
	cur, err := settings.Load(r.Context(), a.db)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "settings unavailable")
		return
	}
	if r.Method == http.MethodGet {
		// setup: what the dashboard's getting-started list can't read
		// from the database (which billing webhooks are on, email).
		billing := a.deps.Billing
		if billing == nil {
			billing = []string{}
		}
		notify, err := partnermail.LoadOwnerNotify(r.Context(), a.db, a.deps.OwnerEmail)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "settings unavailable")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			settings.Program
			Setup  any                     `json:"setup"`
			Notify partnermail.OwnerNotify `json:"notify"`
		}{cur, map[string]any{"billing": billing, "mail": a.deps.MailOn, "webhooks": a.webhookHealth(r.Context()), "site_url": a.deps.SiteURL}, notify})
		return
	}
	var req struct {
		MinPayoutCents  *int64 `json:"min_payout_cents"`
		DefaultHoldDays *int   `json:"default_hold_days"`
		// The program currency, and the minimum payout of other
		// currencies (minor units of each; replaces the whole set).
		Currency            *string          `json:"currency"`
		MinPayoutByCurrency map[string]int64 `json:"min_payout_by_currency"`
		NotifyEmail         *string          `json:"notify_email"`
		NotifyOn            *bool            `json:"notify_on"`
		WeeklyReport        *bool            `json:"weekly_report"`
		PartnerTips         *bool            `json:"partner_tips"`
		PartnerRecap        *bool            `json:"partner_recap"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.NotifyEmail != nil || req.NotifyOn != nil || req.WeeklyReport != nil || req.PartnerTips != nil || req.PartnerRecap != nil {
		n, err := partnermail.LoadOwnerNotify(r.Context(), a.db, a.deps.OwnerEmail)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "settings unavailable")
			return
		}
		if req.NotifyEmail != nil {
			n.Email = strings.TrimSpace(*req.NotifyEmail)
			if n.Email != "" && !validPayoutEmail(strings.ToLower(n.Email)) {
				apiError(w, http.StatusBadRequest, "Enter one email address for your notifications.")
				return
			}
		}
		if req.NotifyOn != nil {
			n.Enabled = *req.NotifyOn
		}
		for _, sw := range []struct {
			in  *bool
			out *bool
		}{{req.WeeklyReport, &n.WeeklyReport}, {req.PartnerTips, &n.PartnerTips}, {req.PartnerRecap, &n.PartnerRecap}} {
			if sw.in != nil {
				*sw.out = *sw.in
			}
		}
		if (n.Enabled || n.WeeklyReport) && n.Email == "" {
			apiError(w, http.StatusBadRequest, "Enter the email address to send them to.")
			return
		}
		if err := partnermail.SaveOwnerNotify(r.Context(), a.db, n); err != nil {
			apiError(w, http.StatusInternalServerError, "save failed")
			return
		}
		if req.MinPayoutCents == nil && req.DefaultHoldDays == nil && req.Currency == nil && req.MinPayoutByCurrency == nil {
			writeJSON(w, http.StatusOK, map[string]any{"notify": n})
			return
		}
	}
	if req.MinPayoutCents != nil {
		cur.MinPayoutCents = *req.MinPayoutCents
	}
	if req.Currency != nil {
		cur.Currency = *req.Currency
	}
	if req.MinPayoutByCurrency != nil {
		cur.MinPayoutByCurrency = req.MinPayoutByCurrency
	}
	cur.Normalize()
	if req.DefaultHoldDays != nil {
		cur.DefaultHoldDays = *req.DefaultHoldDays
	}
	if err := cur.Validate(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := settings.Save(r.Context(), a.db, cur); err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	writeJSON(w, http.StatusOK, cur)
}

// handleCampaignUpdate: PATCH /api/v1/admin/campaigns/{id} (admin).
// Body: {"net_terms_days":14,"attribution_window_days":60,"commission_months":12};
// every field optional. commission_months 0 = lifetime; a change applies to
// every later payment, including customers bound before it. A new hold applies to commissions created after the change;
// existing ones keep the matures_at they were stamped with.
func (a *api) handleCampaignUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusBadRequest, "campaign id must be a positive integer")
		return
	}
	var req struct {
		NetTermsDays          *int `json:"net_terms_days"`
		AttributionWindowDays *int `json:"attribution_window_days"`
		CommissionMonths      *int `json:"commission_months"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.NetTermsDays == nil && req.AttributionWindowDays == nil && req.CommissionMonths == nil {
		apiError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	if req.NetTermsDays != nil && (*req.NetTermsDays < 0 || *req.NetTermsDays > settings.MaxHoldDays) {
		apiError(w, http.StatusBadRequest, "net_terms_days must be 0-365")
		return
	}
	if req.AttributionWindowDays != nil && (*req.AttributionWindowDays < 1 || *req.AttributionWindowDays > 365) {
		apiError(w, http.StatusBadRequest, "attribution_window_days must be 1-365")
		return
	}
	if req.CommissionMonths != nil && (*req.CommissionMonths < 0 || *req.CommissionMonths > settings.MaxCommissionMonths) {
		apiError(w, http.StatusBadRequest, "commission_months must be 0-120 (0 = lifetime)")
		return
	}
	res, err := a.db.ExecContext(r.Context(), `
		UPDATE campaigns SET
			net_terms_days = COALESCE(CAST(? AS INTEGER), net_terms_days),
			attribution_window_days = COALESCE(CAST(? AS INTEGER), attribution_window_days),
			commission_months = COALESCE(CAST(? AS INTEGER), commission_months)
		WHERE id = ?`, intPtrArg(req.NetTermsDays), intPtrArg(req.AttributionWindowDays), intPtrArg(req.CommissionMonths), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		apiError(w, http.StatusNotFound, "unknown campaign")
		return
	}
	var nt, aw, cm int
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT net_terms_days, attribution_window_days, commission_months FROM campaigns WHERE id = ?`, id).Scan(&nt, &aw, &cm); err != nil {
		apiError(w, http.StatusInternalServerError, "read back failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "net_terms_days": nt, "attribution_window_days": aw, "commission_months": cm})
}

func intPtrArg(p *int) any {
	if p == nil {
		return nil
	}
	return int64(*p)
}

// webhookHealth is each billing tool's last accepted and last rejected
// webhook (billing health on the dashboard). last_err_at and err_count are
// verified failures only; failed signature checks are reported apart
// (last_sigfail_at, sigfail_count) and never mean "failing". Empty on
// error: it is advice, never a reason to fail the settings read.
func (a *api) webhookHealth(ctx context.Context) []map[string]any {
	out := []map[string]any{}
	rows, err := a.db.QueryContext(ctx, `
		SELECT provider, COALESCE(last_ok_at, ''), COALESCE(last_err_at, ''), COALESCE(last_err, ''), ok_count, err_count,
		       COALESCE(last_sigfail_at, ''), sigfail_count
		FROM webhook_health ORDER BY provider`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p, okAt, errAt, lastErr, sigAt string
		var okN, errN, sigN int64
		if rows.Scan(&p, &okAt, &errAt, &lastErr, &okN, &errN, &sigAt, &sigN) != nil {
			return out
		}
		out = append(out, map[string]any{
			"provider": p, "last_ok_at": okAt, "last_err_at": errAt, "last_err": lastErr, "ok_count": okN, "err_count": errN,
			"last_sigfail_at": sigAt, "sigfail_count": sigN,
		})
	}
	return out
}
