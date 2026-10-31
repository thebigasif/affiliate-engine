package db

import (
	"testing"
)

func TestRewritePlaceholders(t *testing.T) {
	cases := []struct{ in, want string }{
		// basics
		{"SELECT 1", "SELECT 1"},
		{"INSERT INTO t (a) VALUES (?)", "INSERT INTO t (a) VALUES ($1)"},
		{"INSERT INTO t VALUES (?, ?, ?)", "INSERT INTO t VALUES ($1, $2, $3)"},
		{"WHERE id = ? AND name = ?", "WHERE id = $1 AND name = $2"},
		// literals must not be touched
		{"SELECT 'what?'", "SELECT 'what?'"},
		{"SELECT 'it''s ? here'", "SELECT 'it''s ? here'"},
		{"WHERE x = ? AND y = 'a?b'", "WHERE x = $1 AND y = 'a?b'"},
		// identifiers
		{`WHERE "col?" = ?`, `WHERE "col?" = $1`},
		// comments
		{"SELECT 1 -- trailing ? comment\nFROM t WHERE x = ?", "SELECT 1 -- trailing ? comment\nFROM t WHERE x = $1"},
		{"SELECT /* ? */ 1 FROM t WHERE x = ?", "SELECT /* ? */ 1 FROM t WHERE x = $1"},
		// dollar-quoted bodies (trigger functions)
		{"CREATE FUNCTION f() AS $$ BEGIN IF x ? y THEN END IF; END $$; SELECT ?", "CREATE FUNCTION f() AS $$ BEGIN IF x ? y THEN END IF; END $$; SELECT $1"},
		// pre-existing $N placeholders pass through untouched. NOTE: mixing
		// $N and ? in one query is unsupported (never occurs in this repo).
		{"SELECT $1, ?", "SELECT $1, $1"},
		// unterminated edge cases must not panic
		{"SELECT 'unterminated", "SELECT 'unterminated"},
		{"SELECT /* unterminated", "SELECT /* unterminated"},
		{"SELECT $$unterminated", "SELECT $$unterminated"},
	}
	for _, c := range cases {
		if got := rewritePlaceholders(c.in); got != c.want {
			t.Errorf("rewrite(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}
