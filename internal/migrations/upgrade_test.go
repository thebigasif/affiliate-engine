package migrations_test

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// An upgraded SQLite database gets every column, including one added
// after an earlier ADD COLUMN in the same file. The chain used to stop a
// file at its first "duplicate column": campaigns.commission_months never
// reached databases that already had attribution_window_days, and every
// commission (and the admin campaign list) failed on them.
func TestSQLiteUpgradeAddsEveryColumn(t *testing.T) {
	ctx := context.Background()
	h, err := db.Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "u.db"), db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.DB.Close()
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}

	// Every ADD COLUMN in the chain, as (table, column).
	re := regexp.MustCompile(`(?im)^[ \t]*ALTER[ \t]+TABLE[ \t]+(\w+)[ \t]+ADD[ \t]+COLUMN[ \t]+(\w+)`)
	type col struct{ table, name string }
	var all []col
	for _, name := range migrations.Names(migrations.SQLite) {
		b, err := migrations.Files().ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			all = append(all, col{m[1], m[2]})
		}
	}
	has := func(c col) bool {
		var n int
		if err := h.DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	// Simulate an older database: drop every added column SQLite lets us
	// drop (indexed or trigger-referenced ones refuse), then upgrade.
	var dropped []col
	for _, c := range all {
		if !has(c) {
			t.Fatalf("fresh chain lacks %s.%s", c.table, c.name)
		}
		if _, err := h.DB.Exec(`ALTER TABLE ` + c.table + ` DROP COLUMN ` + c.name); err == nil {
			dropped = append(dropped, c)
		}
	}
	found := false
	for _, c := range dropped {
		if c.table == "campaigns" && c.name == "commission_months" {
			found = true
		}
	}
	if !found {
		t.Fatalf("test could not drop campaigns.commission_months; dropped %v", dropped)
	}
	// Put the first column of campaigns back, as an older release left it,
	// so the file's first ALTER is a duplicate again.
	if _, err := h.DB.Exec(`ALTER TABLE campaigns ADD COLUMN attribution_window_days INTEGER NOT NULL DEFAULT 30`); err != nil && !strings.Contains(err.Error(), "duplicate") {
		t.Fatal(err)
	}

	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if !has(c) {
			t.Errorf("upgrade did not add %s.%s", c.table, c.name)
		}
	}
	// And the admin campaign list query runs.
	if _, err := h.DB.Exec(`SELECT id, commission_months, attribution_window_days, net_terms_days FROM campaigns`); err != nil {
		t.Fatal(err)
	}
}
