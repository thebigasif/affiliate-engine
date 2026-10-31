package httpapi

// Partner resources: ready-to-copy text, banners and logos, and useful
// links the owner shares with partners in their portal.
//
//	GET    /api/v1/admin/resources                (admin) list
//	POST   /api/v1/admin/resources                (admin) {"kind":"text"|"link","title","body"}
//	POST   /api/v1/admin/resources/image?title=   (admin) raw image bytes
//	DELETE /api/v1/admin/resources?id=           (admin)
//	GET    /api/v1/affiliate/resources            (partner) list
//	GET    /api/v1/portal/resources/{id}/image    (public) the image

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
)

// maxResourceImage bounds one banner or logo.
const maxResourceImage = 2 << 20

// maxResources bounds a program's resource list.
const maxResources = 100

// Resource is one item in the partner resources list.
type Resource struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"` // text | image | link
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`      // text: the copy; link: the URL
	ImageURL string `json:"image_url,omitempty"` // image: where it's served
}

func (a *api) listResources(r *http.Request) ([]Resource, error) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, kind, title, body, sha256 FROM partner_resources
		WHERE campaign_id = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Resource{}
	for rows.Next() {
		var x Resource
		var digest string
		if err := rows.Scan(&x.ID, &x.Kind, &x.Title, &x.Body, &digest); err != nil {
			return nil, err
		}
		if x.Kind == "image" {
			x.ImageURL = "/api/v1/portal/resources/" + strconv.FormatInt(x.ID, 10) + "/image?v=" + digest[:12]
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// handleResources: GET list, POST text/link, DELETE ?id= (admin).
func (a *api) handleResources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		out, err := a.listResources(r)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"resources": out})
	case http.MethodDelete:
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || id <= 0 {
			apiError(w, http.StatusBadRequest, "id required")
			return
		}
		res, err := a.db.ExecContext(r.Context(), `DELETE FROM partner_resources WHERE id = ?`, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "delete failed")
			return
		}
		n, _ := res.RowsAffected()
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
	case http.MethodPost:
		var req struct {
			Kind  string `json:"kind"`
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<15)).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid body")
			return
		}
		title, body := strings.TrimSpace(req.Title), strings.TrimSpace(req.Body)
		if title == "" || utf8.RuneCountInString(title) > 120 {
			apiError(w, http.StatusBadRequest, "Give it a title (up to 120 characters).")
			return
		}
		switch req.Kind {
		case "text":
			if body == "" || utf8.RuneCountInString(body) > 5000 {
				apiError(w, http.StatusBadRequest, "Write the text partners can copy (up to 5,000 characters).")
				return
			}
		case "link":
			u, err := url.Parse(body)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(body) > 2000 {
				apiError(w, http.StatusBadRequest, "Enter a full link, like https://yoursite.com/pricing")
				return
			}
		default:
			apiError(w, http.StatusBadRequest, "kind must be text or link (images upload to /resources/image)")
			return
		}
		id, err := a.insertResource(r, req.Kind, title, body, "", nil, "")
		if err != nil {
			a.resourceErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	default:
		apiError(w, http.StatusMethodNotAllowed, "GET, POST or DELETE")
	}
}

var errTooManyResources = errors.New("too many resources")

func (a *api) resourceErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errTooManyResources) {
		apiError(w, http.StatusConflict, "You have 100 resources. Remove one to add another.")
		return
	}
	apiError(w, http.StatusInternalServerError, "save failed")
}

func (a *api) insertResource(r *http.Request, kind, title, body, ct string, data []byte, digest string) (int64, error) {
	var n int
	if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM partner_resources WHERE campaign_id = 1`).Scan(&n); err != nil {
		return 0, err
	}
	if n >= maxResources {
		return 0, errTooManyResources
	}
	var id int64
	err := a.db.QueryRowContext(r.Context(), `
		INSERT INTO partner_resources (campaign_id, kind, title, body, content_type, data, sha256)
		VALUES (1, ?, ?, ?, ?, ?, ?) RETURNING id`, kind, title, body, ct, data, digest).Scan(&id)
	return id, err
}

// handleResourceImage: POST /api/v1/admin/resources/image?title= with the
// raw image as the body. The type is sniffed from the bytes (no SVG: it
// can carry script).
func (a *api) handleResourceImage(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	if title == "" || utf8.RuneCountInString(title) > 120 {
		apiError(w, http.StatusBadRequest, "Give it a title (up to 120 characters).")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResourceImage+1))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) || len(body) > maxResourceImage {
		apiError(w, http.StatusRequestEntityTooLarge, "Images must be 2 MB or smaller.")
		return
	}
	if err != nil || len(body) == 0 {
		apiError(w, http.StatusBadRequest, "send the image as the request body")
		return
	}
	ct := http.DetectContentType(body)
	if !logoTypes[ct] {
		apiError(w, http.StatusUnsupportedMediaType, "Images must be PNG, JPEG, WebP or GIF.")
		return
	}
	sum := sha256.Sum256(body)
	id, err := a.insertResource(r, "image", title, "", ct, body, hex.EncodeToString(sum[:]))
	if err != nil {
		a.resourceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// handleAffiliateResources: GET /api/v1/affiliate/resources (partner).
func (a *api) handleAffiliateResources(w http.ResponseWriter, r *http.Request, _ *apikeys.Identity) {
	out, err := a.listResources(r)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": out})
}

// handleResourceImageGet: GET /api/v1/portal/resources/{id}/image (public).
func (a *api) handleResourceImageGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	var ct, digest string
	var data []byte
	err = a.db.QueryRowContext(r.Context(), `
		SELECT content_type, data, sha256 FROM partner_resources WHERE id = ? AND kind = 'image'`, id).Scan(&ct, &data, &digest)
	if errors.Is(err, sql.ErrNoRows) || len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin") // partners show banners on their own sites
	etag := `"` + digest[:16] + `"`
	h.Set("ETag", etag)
	if r.URL.Query().Get("v") == digest[:12] {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=300")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}

// SeedDemoResources gives the demo program a few resources, once.
func SeedDemoResources(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM partner_resources`).Scan(&n); err != nil || n > 0 {
		return err
	}
	for _, x := range []struct{ kind, title, body string }{
		{"text", "Short email intro", "I've used Northwind Notes for a year: it keeps my research, drafts and meeting notes in one searchable place. If you write for a living, try it free: {link}"},
		{"text", "Social post", "My favourite writing tool this year: Northwind Notes. Fast, private, and the search actually finds things. {link}"},
		{"link", "Pricing page (converts best)", "https://northwind.example/pricing"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO partner_resources (campaign_id, kind, title, body) VALUES (1, ?, ?, ?)`, x.kind, x.title, x.body); err != nil {
			return err
		}
	}
	return nil
}
