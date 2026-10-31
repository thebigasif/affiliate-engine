// Package dbutil holds small cross-engine helpers shared by domain code.
package dbutil

import (
	"fmt"
	"strings"
	"time"
)

// IsBusy reports whether err is a SQLite/PG transient lock contention
// error (SQLITE_BUSY / SQLITE_LOCKED, or PG serialization).
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "database is locked") ||
		strings.Contains(s, "SQLITE_BUSY") ||
		strings.Contains(s, "SQLITE_LOCKED") ||
		strings.Contains(s, "40P01") // PG serialization_failure
}

// RetryBusy retries fn up to attempts times with linear backoff while it
// returns busy-class errors. SQLite read->write lock upgrades return
// SQLITE_BUSY immediately regardless of busy_timeout; domain writes on the
// hot path (admin endpoints, period runs) must tolerate the blip.
func RetryBusy(attempts int, sleep time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); !IsBusy(err) {
			return err
		}
		time.Sleep(sleep)
	}
	if err == nil {
		return nil
	}
	return fmt.Errorf("still busy after %d attempts: %w", attempts, err)
}

// AsTime reads a scanned timestamp column on either engine: PG returns
// time.Time; SQLite returns RFC3339 text (what the engine writes) or
// datetime('now') text (column defaults). ok=false for NULL or garbage.
func AsTime(v any) (t time.Time, ok bool) {
	switch x := v.(type) {
	case time.Time:
		return x.UTC(), true
	case []byte:
		v = string(x)
	}
	s, isStr := v.(string)
	if !isStr {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02 15:04:05.999999999-07:00"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
