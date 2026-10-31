// FR-17 tax-doc gating: admin records/reviews each affiliate's tax-document
// status. The form is either uploaded by the affiliate from the portal
// (taxdocfile.go: status becomes 'pending' until reviewed) or collected out-of-band.
// A 'pending' or 'rejected' doc blocks payout batching + approval for that
// affiliate (see payouts.Runner); every transition lands in the audit log.
package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var taxDocTypes = map[string]bool{
	"w9": true, "w8ben": true, "w8bene": true, "other": true,
}

var taxDocStatuses = map[string]bool{
	"pending": true, "received": true, "validated": true,
	"rejected": true, "not_required": true,
}

// handleTaxDocs: GET ?affiliate_id=&limit=&after_id= list / POST record|update.
//
// POST body: {"affiliate_id":1,"doc_type":"w9","status":"received",
//
//	"reference":"collector-123","note":""}
//
// Upsert per (affiliate_id, doc_type). reviewed_at is stamped on every
// status change away from 'pending' (the operator looked at it).
func (a *api) handleTaxDocs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, afterID, err := pageParams(r)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		aff := r.URL.Query().Get("affiliate_id")
		// NOTE: reviewed_at is scanned as NullString, never
		// COALESCE(reviewed_at,'') — on PG the column is TIMESTAMPTZ and
		// COALESCE with a text literal is a server-side type error
		// (SQLSTATE 22007). Direct scan yields RFC3339 on both engines.
		q := `SELECT td.id, td.affiliate_id, td.doc_type, td.status, td.reference, td.note, td.reviewed_at,
		             COALESCE(f.file_name, ''), COALESCE(f.size_bytes, 0), f.uploaded_at
		      FROM tax_documents td
		      LEFT JOIN tax_document_files f ON f.tax_document_id = td.id
		      WHERE td.id > ?`
		var rows *sql.Rows
		if aff != "" {
			affID, convErr := strconv.ParseInt(aff, 10, 64)
			if convErr != nil || affID <= 0 {
				apiError(w, http.StatusBadRequest, "affiliate_id must be a positive integer")
				return
			}
			rows, err = a.db.QueryContext(ctx(r), q+` AND td.affiliate_id = ? ORDER BY td.id LIMIT ?`, afterID, affID, limit)
		} else {
			rows, err = a.db.QueryContext(ctx(r), q+` ORDER BY td.id LIMIT ?`, afterID, limit)
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		type doc struct {
			ID          int64  `json:"id"`
			AffiliateID int64  `json:"affiliate_id"`
			DocType     string `json:"doc_type"`
			Status      string `json:"status"`
			Reference   string `json:"reference"`
			Note        string `json:"note"`
			ReviewedAt  string `json:"reviewed_at"`
			HasFile     bool   `json:"has_file"`
			FileName    string `json:"file_name"`
			SizeBytes   int64  `json:"size_bytes"`
			UploadedAt  string `json:"uploaded_at"`
		}
		out := []doc{}
		for rows.Next() {
			var d doc
			var reviewed, uploaded sql.NullString
			if err := rows.Scan(&d.ID, &d.AffiliateID, &d.DocType, &d.Status,
				&d.Reference, &d.Note, &reviewed, &d.FileName, &d.SizeBytes, &uploaded); err != nil {
				apiError(w, http.StatusInternalServerError, "scan failed")
				return
			}
			d.ReviewedAt = reviewed.String
			d.UploadedAt = uploaded.String
			d.HasFile = uploaded.Valid
			out = append(out, d)
		}
		if err := rows.Err(); err != nil {
			apiError(w, http.StatusInternalServerError, "query iteration failed")
			return
		}
		var next int64
		if len(out) == limit {
			next = out[len(out)-1].ID
		}
		writeJSON(w, http.StatusOK, map[string]any{"tax_documents": out, "next_after_id": next})
	case http.MethodPost:
		var req struct {
			AffiliateID int64  `json:"affiliate_id"`
			DocType     string `json:"doc_type"`
			Status      string `json:"status"`
			Reference   string `json:"reference"`
			Note        string `json:"note"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid body")
			return
		}
		if req.AffiliateID <= 0 {
			apiError(w, http.StatusBadRequest, "affiliate_id required")
			return
		}
		req.DocType = strings.ToLower(strings.TrimSpace(req.DocType))
		if !taxDocTypes[req.DocType] {
			apiError(w, http.StatusBadRequest, "doc_type must be w9|w8ben|w8bene|other")
			return
		}
		req.Status = strings.ToLower(strings.TrimSpace(req.Status))
		if req.Status == "" {
			req.Status = "pending"
		}
		if !taxDocStatuses[req.Status] {
			apiError(w, http.StatusBadRequest,
				"status must be pending|received|validated|rejected|not_required")
			return
		}
		if len(req.Reference) > 128 || len(req.Note) > 512 {
			apiError(w, http.StatusBadRequest, "reference/note too long (128/512)")
			return
		}
		var exists int
		if err := a.db.QueryRowContext(ctx(r),
			`SELECT COUNT(*) FROM affiliates WHERE id = ?`, req.AffiliateID).
			Scan(&exists); err != nil || exists == 0 {
			apiError(w, http.StatusBadRequest, "unknown affiliate")
			return
		}
		// reviewed_at stamps the operator's look at the document; setting
		// 'pending' (re-)opens the requirement and clears it.
		var reviewedAt any
		if req.Status != "pending" {
			reviewedAt = time.Now().UTC().Format(time.RFC3339)
		}
		var id int64
		err := a.db.QueryRowContext(ctx(r), `
			INSERT INTO tax_documents (affiliate_id, doc_type, status, reference, note, reviewed_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (affiliate_id, doc_type) DO UPDATE SET
				status = excluded.status,
				reference = excluded.reference,
				note = excluded.note,
				reviewed_at = excluded.reviewed_at
			RETURNING id`,
			req.AffiliateID, req.DocType, req.Status, req.Reference, req.Note, reviewedAt).
			Scan(&id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "save failed")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "status": req.Status})
	default:
		apiError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}
