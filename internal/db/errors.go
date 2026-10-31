// Error classification across engines. This file is part of the dialect
// layer — the ONLY place engine-specific error shapes are named.
package db

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
)

// IsUniqueViolation reports whether err is a unique-constraint violation
// on either engine: PG SQLSTATE 23505, SQLite SQLITE_CONSTRAINT_UNIQUE
// (2067) or SQLITE_CONSTRAINT_PRIMARYKEY (1555, PK conflicts on tables
// where the key is the primary key).
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	var liteErr *sqlite.Error
	if errors.As(err, &liteErr) {
		return liteErr.Code() == 2067 || liteErr.Code() == 1555
	}
	// Fallback for wrapped/legacy shapes: match the message text.
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// IsTxConflict reports whether err is a Postgres transaction conflict the
// whole transaction may simply be retried after: serialization_failure
// (40001) or deadlock_detected (40P01). SQLite never reports these: its
// writers queue on the file lock under busy_timeout instead.
func IsTxConflict(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}
