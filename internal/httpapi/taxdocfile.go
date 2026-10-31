// FR-17 portal flow: an affiliate submits a W-9 / W-8BEN / W-8BEN-E PDF
// from the portal. The upload always sets the tax_documents row to
// 'pending', which blocks payouts until the operator reviews the file and
// records received / validated / not_required (or rejects it). The file is sealed with
// AES-256-GCM under TaxDocKey and stored in tax_document_files; it is never
// reachable through /static/ and only the admin download route decrypts it,
// as an attachment.
package httpapi

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
)

// MaxTaxDocBytes caps one uploaded tax form (a scanned W-9 is ~100 KB-2 MB).
const MaxTaxDocBytes = 5 << 20

// portalTaxDocTypes are the forms an affiliate may upload themselves.
var portalTaxDocTypes = map[string]bool{"w9": true, "w8ben": true, "w8bene": true}

// DeriveTaxDocKey derives the 32-byte tax-form sealing key from the
// deployment secret (domain-separated, like the click hash key). Rotating
// the deployment secret makes previously uploaded forms unreadable.
func DeriveTaxDocKey(secret []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("taxdoc-file-key-v1"))
	return m.Sum(nil)
}

func taxDocAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("tax document key not configured")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// taxDocAAD binds a sealed file to its row: a blob copied onto another
// affiliate's or another document's row fails to open.
func taxDocAAD(affiliateID, taxDocID int64) []byte {
	return []byte(fmt.Sprintf("taxdoc:%d:%d", affiliateID, taxDocID))
}

func sealTaxDoc(key []byte, affiliateID, taxDocID int64, plain []byte) ([]byte, error) {
	aead, err := taxDocAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, taxDocAAD(affiliateID, taxDocID)), nil
}

func openTaxDoc(key []byte, affiliateID, taxDocID int64, sealed []byte) ([]byte, error) {
	aead, err := taxDocAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("sealed file too short")
	}
	n := aead.NonceSize()
	return aead.Open(nil, sealed[:n], sealed[n:], taxDocAAD(affiliateID, taxDocID))
}

// cleanFileName keeps a display name only: no directories, no control
// characters, bounded length. It is never used as a path.
func cleanFileName(s string) string {
	s = path.Base(strings.ReplaceAll(s, "\\", "/"))
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, s)
	if s == "." || s == "/" {
		s = ""
	}
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

// handleAffiliateTaxDocUpload: POST multipart/form-data with fields
// doc_type (w9|w8ben|w8bene) and file (a PDF, <= MaxTaxDocBytes).
// Affiliate scope (portal session or affiliate key); admin keys may upload
// on an affiliate's behalf with ?affiliate_id=.
func (a *api) handleAffiliateTaxDocUpload(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity) {
	affID, err := targetAffiliateID(r, idn)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(a.deps.TaxDocKey) != 32 {
		apiError(w, http.StatusServiceUnavailable, "tax form uploads are not configured on this install")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxTaxDocBytes+(64<<10))
	mr, err := r.MultipartReader()
	if err != nil {
		apiError(w, http.StatusBadRequest, "multipart/form-data body required")
		return
	}
	var docType, fileName string
	var data []byte
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			var mbe *http.MaxBytesError
			if errors.As(perr, &mbe) {
				apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file too large (max %d MB)", MaxTaxDocBytes>>20))
				return
			}
			apiError(w, http.StatusBadRequest, "bad multipart body")
			return
		}
		switch part.FormName() {
		case "doc_type":
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			docType = strings.ToLower(strings.TrimSpace(string(b)))
		case "file":
			if data != nil {
				apiError(w, http.StatusBadRequest, "one file per upload")
				return
			}
			fileName = cleanFileName(part.FileName())
			b, rerr := io.ReadAll(io.LimitReader(part, MaxTaxDocBytes+1))
			if rerr != nil {
				var mbe *http.MaxBytesError
				if errors.As(rerr, &mbe) {
					apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file too large (max %d MB)", MaxTaxDocBytes>>20))
					return
				}
				apiError(w, http.StatusBadRequest, "could not read file")
				return
			}
			if len(b) > MaxTaxDocBytes {
				apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file too large (max %d MB)", MaxTaxDocBytes>>20))
				return
			}
			data = b
		}
		_ = part.Close()
	}
	if !portalTaxDocTypes[docType] {
		apiError(w, http.StatusBadRequest, "doc_type must be w9|w8ben|w8bene")
		return
	}
	if len(data) == 0 {
		apiError(w, http.StatusBadRequest, "file required")
		return
	}
	// Content sniff, never the client's Content-Type: a PDF starts with
	// %PDF- (leading whitespace/BOM is not accepted).
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		apiError(w, http.StatusUnsupportedMediaType, "file must be a PDF")
		return
	}
	sum := sha256.Sum256(data)

	c := r.Context()
	tx, err := a.db.BeginTx(c, nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(c, `SELECT COUNT(*) FROM affiliates WHERE id = ?`, affID).
		Scan(&exists); err != nil || exists == 0 {
		apiError(w, http.StatusBadRequest, "unknown affiliate")
		return
	}
	// A self-upload is never reviewed: it always lands 'pending', which
	// blocks payouts (FR-17), whatever the row was before (received,
	// validated, rejected, not_required). Only the operator moves a row to
	// received / validated / not_required, so no affiliate can clear the
	// payout block by uploading any file that starts with %PDF-.
	var docID int64
	var status string
	if err := tx.QueryRowContext(c, `
		INSERT INTO tax_documents (affiliate_id, doc_type, status, reviewed_at)
		VALUES (?, ?, 'pending', NULL)
		ON CONFLICT (affiliate_id, doc_type) DO UPDATE SET
			status = 'pending',
			reviewed_at = NULL
		RETURNING id, status`, affID, docType).Scan(&docID, &status); err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	sealed, err := sealTaxDoc(a.deps.TaxDocKey, affID, docID, data)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	if _, err := tx.ExecContext(c, `
		INSERT INTO tax_document_files (tax_document_id, affiliate_id, file_name, size_bytes, sha256, sealed)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (tax_document_id) DO UPDATE SET
			file_name = excluded.file_name,
			size_bytes = excluded.size_bytes,
			sha256 = excluded.sha256,
			sealed = excluded.sealed,
			uploaded_at = CURRENT_TIMESTAMP`,
		docID, affID, fileName, len(data), hex.EncodeToString(sum[:]), sealed); err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	if err := tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": docID, "doc_type": docType, "status": status, "size_bytes": len(data),
	})
}

// handleAffiliateTaxDocs: GET the caller's own tax documents (status and
// file metadata; never the file).
func (a *api) handleAffiliateTaxDocs(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity) {
	affID, err := targetAffiliateID(r, idn)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT td.id, td.doc_type, td.status, td.note, td.reviewed_at,
		       COALESCE(f.file_name, ''), COALESCE(f.size_bytes, 0), f.uploaded_at
		FROM tax_documents td
		LEFT JOIN tax_document_files f ON f.tax_document_id = td.id
		WHERE td.affiliate_id = ? ORDER BY td.id`, affID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	type doc struct {
		ID         int64  `json:"id"`
		DocType    string `json:"doc_type"`
		Status     string `json:"status"`
		Note       string `json:"note"`
		ReviewedAt string `json:"reviewed_at"`
		FileName   string `json:"file_name"`
		SizeBytes  int64  `json:"size_bytes"`
		UploadedAt string `json:"uploaded_at"`
	}
	out := []doc{}
	for rows.Next() {
		var d doc
		var reviewed, uploaded sql.NullString
		if err := rows.Scan(&d.ID, &d.DocType, &d.Status, &d.Note, &reviewed,
			&d.FileName, &d.SizeBytes, &uploaded); err != nil {
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		d.ReviewedAt, d.UploadedAt = reviewed.String, uploaded.String
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		apiError(w, http.StatusInternalServerError, "query iteration failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tax_documents": out, "max_bytes": MaxTaxDocBytes})
}

// handleTaxDocFile: GET ?id=<tax_document_id> (admin). Decrypts and returns
// the uploaded PDF as an attachment, with headers that stop a browser from
// rendering it inline in the app's origin.
func (a *api) handleTaxDocFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}
	var affID int64
	var sealed []byte
	err = a.db.QueryRowContext(r.Context(), `
		SELECT affiliate_id, sealed FROM tax_document_files WHERE tax_document_id = ?`, id).
		Scan(&affID, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no file uploaded for this tax document")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	plain, err := openTaxDoc(a.deps.TaxDocKey, affID, id, sealed)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "file cannot be decrypted (deployment secret changed?)")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/pdf")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="taxdoc-%d.pdf"`, id))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(plain)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(plain)
}
