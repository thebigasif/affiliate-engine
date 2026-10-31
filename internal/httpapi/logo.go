package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/thebigasif/affiliate-engine/internal/plan"
)

// maxLogoBytes bounds an uploaded logo. A sidebar logo is shown at 30px;
// 256 KB is room for a crisp PNG without letting the table grow fat.
const maxLogoBytes = 256 << 10

// logoTypes are the image types a logo may be. SVG is left out on
// purpose: it can carry script, and these bytes are served same-origin.
var logoTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// handleBrandLogo: PUT/DELETE /api/v1/admin/branding/logo?campaign_id=N.
// PUT takes the raw image bytes as the body; the type is sniffed from the
// bytes, never trusted from the request. DELETE removes the logo.
func (a *api) handleBrandLogo(w http.ResponseWriter, r *http.Request) {
	campID, err := strconv.ParseInt(r.URL.Query().Get("campaign_id"), 10, 64)
	if err != nil || campID <= 0 {
		apiError(w, http.StatusBadRequest, "campaign_id required")
		return
	}
	if r.Method == http.MethodDelete {
		if _, err := a.db.ExecContext(r.Context(), `DELETE FROM program_logos WHERE campaign_id = ?`, campID); err != nil {
			apiError(w, http.StatusInternalServerError, "remove failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"campaign_id": campID, "logo_url": ""})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogoBytes+1))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) || len(body) > maxLogoBytes {
		apiError(w, http.StatusRequestEntityTooLarge, "logo must be 256 KB or smaller")
		return
	}
	if err != nil || len(body) == 0 {
		apiError(w, http.StatusBadRequest, "send the image as the request body")
		return
	}
	ct := http.DetectContentType(body)
	if !logoTypes[ct] {
		apiError(w, http.StatusUnsupportedMediaType, "logo must be a PNG, JPEG, WebP or GIF image")
		return
	}
	var exists int
	if err := a.db.QueryRowContext(r.Context(), `SELECT 1 FROM campaigns WHERE id = ?`, campID).Scan(&exists); err != nil {
		apiError(w, http.StatusNotFound, "campaign not found")
		return
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if _, err := a.db.ExecContext(r.Context(), `
		INSERT INTO program_logos (campaign_id, content_type, data, sha256, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (campaign_id) DO UPDATE SET
			content_type = excluded.content_type,
			data = excluded.data,
			sha256 = excluded.sha256,
			updated_at = excluded.updated_at`,
		campID, ct, body, digest); err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaign_id": campID, "logo_url": logoPath(campID, digest)})
}

// handlePortalLogo: GET /api/v1/portal/logo?campaign_id=N (public). The
// brand read links here with ?v=<digest>, so a versioned URL is cached
// for good; an unversioned one is revalidated.
func (a *api) handlePortalLogo(w http.ResponseWriter, r *http.Request) {
	if !a.deps.Plan.Allows(plan.FeatBranding) {
		http.NotFound(w, r)
		return
	}
	campID, err := strconv.ParseInt(r.URL.Query().Get("campaign_id"), 10, 64)
	if err != nil || campID <= 0 {
		campID = 1
		if b := BrandForHost(r.Context(), a.db, r.Host); b != nil {
			campID = b.CampaignID
		}
	}
	var ct, digest string
	var data []byte
	if err := a.db.QueryRowContext(r.Context(), `
		SELECT content_type, data, sha256 FROM program_logos WHERE campaign_id = ?`, campID).
		Scan(&ct, &data, &digest); err != nil {
		http.NotFound(w, r)
		return
	}
	etag := `"` + digest[:16] + `"`
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("ETag", etag)
	if r.URL.Query().Get("v") == digest[:12] {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}

// uploadedLogo returns the versioned path of a program's uploaded logo,
// or "" when it has none.
func uploadedLogo(ctx context.Context, db *sql.DB, campID int64) string {
	var digest string
	if err := db.QueryRowContext(ctx, `SELECT sha256 FROM program_logos WHERE campaign_id = ?`, campID).Scan(&digest); err != nil {
		return ""
	}
	return logoPath(campID, digest)
}

func logoPath(campID int64, digest string) string {
	return "/api/v1/portal/logo?campaign_id=" + strconv.FormatInt(campID, 10) + "&v=" + digest[:12]
}
