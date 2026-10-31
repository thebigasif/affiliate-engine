package httpapi

import (
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

// handlePartnerMessage: POST /admin/partners/message {subject, body}. Queues
// one program-news email per active partner who hasn't unsubscribed; each
// carries their own unsubscribe link. Answers how many were queued.
func (a *api) handlePartnerMessage(w http.ResponseWriter, r *http.Request) {
	if !a.deps.MailOn || len(a.deps.NewsKey) == 0 {
		apiError(w, http.StatusServiceUnavailable, "partner emails are off; set AE_MAIL_PROVIDER to send messages")
		return
	}
	var req struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.Subject = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(req.Subject))
	req.Body = strings.TrimSpace(req.Body)
	if req.Subject == "" || utf8.RuneCountInString(req.Subject) > 150 {
		apiError(w, http.StatusBadRequest, "subject required, up to 150 characters")
		return
	}
	if req.Body == "" || utf8.RuneCountInString(req.Body) > 5000 {
		apiError(w, http.StatusBadRequest, "message required, up to 5,000 characters")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(r.Context(), `SELECT id FROM affiliates WHERE status = 'active' AND news_opt_out = 0 ORDER BY id`)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			apiError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	for _, id := range ids {
		if err := partnermail.EnqueueNews(r.Context(), tx, id, req.Subject, req.Body); err != nil {
			apiError(w, http.StatusInternalServerError, "queue failed")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": len(ids)})
}

// handleUnsubscribe: GET shows a confirm button, POST opts the partner out
// of program emails (news, tips, monthly recaps). Two steps on purpose: link scanners in mail clients
// open every link, and must not unsubscribe anyone.
func (a *api) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	tok := r.FormValue("t")
	id, ok := partnermail.ParseUnsubscribeToken(a.deps.NewsKey, tok)
	if !ok {
		unsubPage(w, http.StatusBadRequest, "This link doesn't work", "It may be incomplete. Copy the whole link from the email, or reply to the email and ask to stop.", "")
		return
	}
	if r.Method == http.MethodGet {
		unsubPage(w, http.StatusOK, "Stop program emails?", "No more program news, tips or monthly recaps. You'll still get sign-in links and payout notes about your own account.",
			`<form method="post" action="/api/v1/portal/unsubscribe"><input type="hidden" name="t" value="`+html.EscapeString(tok)+`"><button type="submit">Stop program emails</button></form>`)
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE affiliates SET news_opt_out = 1 WHERE id = ?`, id); err != nil {
		unsubPage(w, http.StatusInternalServerError, "Something went wrong", "Please try again in a minute.", "")
		return
	}
	unsubPage(w, http.StatusOK, "Done", "You won't get program news, tips or monthly recaps anymore. Sign-in links and payout notes still come.", "")
}

func unsubPage(w http.ResponseWriter, code int, title, text, form string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="robots" content="noindex"><title>` + html.EscapeString(title) + `</title>
<style>body{font:16px/1.55 system-ui,-apple-system,"Segoe UI",sans-serif;background:#FBFAF6;color:#111;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center;padding:16px}main{background:#fff;border:1.5px solid #111;box-shadow:5px 5px 0 #111;border-radius:16px;padding:32px;max-width:440px}h1{font-size:24px;margin:0 0 8px}p{color:#5C5C66;margin:0 0 20px}button{font:inherit;font-weight:600;background:#111;color:#fff;border:0;border-radius:8px;padding:10px 18px;cursor:pointer}</style></head>
<body><main><h1>` + html.EscapeString(title) + `</h1><p>` + html.EscapeString(text) + `</p>` + form + `</main></body></html>`))
}
