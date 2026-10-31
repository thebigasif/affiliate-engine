package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
)

// Admin list pagination: ?limit= (default 200, max 1000) and ?after_id=
// (keyset on the table's id; 0 = from the start). Responses that page by
// id return next_after_id (0 = no more rows).
const (
	defaultPageLimit = 200
	maxPageLimit     = 1000
)

func pageParams(r *http.Request) (limit int, afterID int64, err error) {
	limit = defaultPageLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		v, convErr := strconv.Atoi(s)
		if convErr != nil || v <= 0 {
			return 0, 0, fmt.Errorf("limit must be a positive integer")
		}
		if v > maxPageLimit {
			v = maxPageLimit
		}
		limit = v
	}
	if s := r.URL.Query().Get("after_id"); s != "" {
		v, convErr := strconv.ParseInt(s, 10, 64)
		if convErr != nil || v < 0 {
			return 0, 0, fmt.Errorf("after_id must be a non-negative integer")
		}
		afterID = v
	}
	return limit, afterID, nil
}
