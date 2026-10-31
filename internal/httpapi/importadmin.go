// Import from another tracker, from the admin screen: the same review,
// import, batch list and undo as the import CLI, so cloud workspaces (no
// shell) can switch too.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/importer"
)

// MaxImportBytes bounds one import upload (all files together).
const MaxImportBytes = 50 << 20

// importDeadline is how long one import request may take to upload and run.
const importDeadline = 5 * time.Minute

// handleImport: POST /api/v1/admin/import (multipart/form-data).
// Fields: source (rewardful|firstpromoter|tapfiliate|tolt), dry_run
// ("1" = review only), files affiliates (required), referrals and
// commissions (optional). Returns the import report; a real run's
// batch_id is what undo takes.
func (a *api) handleImport(w http.ResponseWriter, r *http.Request) {
	// An export can be tens of MB: this route alone gets longer than the
	// server-wide 30s read / 60s write deadlines (best effort).
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(importDeadline))
	_ = rc.SetWriteDeadline(time.Now().Add(importDeadline))
	r.Body = http.MaxBytesReader(w, r.Body, MaxImportBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		apiError(w, http.StatusBadRequest, "multipart/form-data body required")
		return
	}
	var srcName string
	dryRun := false
	files := map[string][]byte{}
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			importReadError(w, perr)
			return
		}
		switch name := part.FormName(); name {
		case "source", "dry_run":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			v := strings.ToLower(strings.TrimSpace(string(b)))
			if name == "source" {
				srcName = v
			} else {
				dryRun = v == "1" || v == "true"
			}
		case "affiliates", "referrals", "commissions":
			if _, dup := files[name]; dup {
				apiError(w, http.StatusBadRequest, "one "+name+" file per import")
				return
			}
			b, rerr := io.ReadAll(part)
			if rerr != nil {
				importReadError(w, rerr)
				return
			}
			if len(b) > 0 {
				files[name] = b
			}
		}
	}
	src, ok := importer.ParseSource(srcName)
	if !ok {
		apiError(w, http.StatusBadRequest, "source must be rewardful, firstpromoter, tapfiliate or tolt")
		return
	}
	if files["affiliates"] == nil {
		apiError(w, http.StatusBadRequest, "the affiliates export is required")
		return
	}
	opt := func(name string) io.Reader {
		if b := files[name]; b != nil {
			return bytes.NewReader(b)
		}
		return nil
	}
	ds, preport, err := importer.ReadDataset(src, bytes.NewReader(files["affiliates"]), opt("referrals"), opt("commissions"))
	if err != nil {
		apiError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "importer: "))
		return
	}
	report, err := importer.Run(r.Context(), a.db, src, ds, preport, dryRun)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "import failed: "+err.Error())
		return
	}
	// Plain-words text for each conflict kind in this report (the CLI's).
	texts := map[string]string{}
	for k := range report.Conflicts {
		texts[k] = importer.IssueText[k]
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": report, "link_param": importer.LinkParam(src), "conflict_text": texts})
}

func importReadError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("files too large (max %d MB together)", MaxImportBytes>>20))
		return
	}
	apiError(w, http.StatusBadRequest, "bad multipart body")
}

// handleImportBatches: GET /api/v1/admin/import/batches, newest first.
func (a *api) handleImportBatches(w http.ResponseWriter, r *http.Request) {
	bs, err := importer.Batches(r.Context(), a.db)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if bs == nil {
		bs = []importer.Batch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": bs})
}

// handleImportUndo: POST /api/v1/admin/import/undo {"batch_id": N}.
// 409 with the reason when money has moved against the batch.
func (a *api) handleImportUndo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BatchID int64 `json:"batch_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil || req.BatchID <= 0 {
		apiError(w, http.StatusBadRequest, "batch_id required")
		return
	}
	rep, err := importer.Undo(r.Context(), a.db, req.BatchID)
	switch {
	case errors.Is(err, importer.ErrUndoRefused):
		apiError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), "importer: "))
		return
	case err != nil && strings.Contains(err.Error(), "no import batch"):
		apiError(w, http.StatusNotFound, "no such import batch")
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "undo failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
