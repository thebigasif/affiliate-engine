package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dispatchCLI must never fall into serve mode on an unknown or mistyped
// word (the old raw os.Args scan silently started a server — `help`
// hung the terminal), and flag VALUES must not be able to trigger
// subcommand dispatch.

func TestDispatch_UnknownSubcommandIsErrorNotServe(t *testing.T) {
	for _, bad := range []string{"help-me", "dctor", "serve", "run", "--help", "-h"} {
		serve, code := dispatchCLI([]string{bad}, "")
		if serve {
			t.Fatalf("%q fell through to serve mode", bad)
		}
		if code == 0 {
			t.Fatalf("%q must exit non-zero", bad)
		}
	}
}

func TestDispatch_NoArgsServes(t *testing.T) {
	serve, code := dispatchCLI(nil, "")
	if !serve || code != 0 {
		t.Fatalf("no args must serve, got serve=%v code=%d", serve, code)
	}
}

func TestDispatch_HelpAndVersion(t *testing.T) {
	// help: exit 0, usage lists every wired subcommand, does not serve.
	serve, code := dispatchCLI([]string{"help"}, "")
	if serve || code != 0 {
		t.Fatalf("help: serve=%v code=%d", serve, code)
	}

	// Every key in cliCommands must appear in the usage output.
	var sb strings.Builder
	printUsage(&sb)
	out := sb.String()
	for cmd := range cliCommands {
		if !strings.Contains(out, cmd) {
			t.Fatalf("usage output missing %q:\n%s", cmd, out)
		}
	}
	// Usage must also advertise the serve default.
	if !strings.Contains(out, "serve") {
		t.Fatal("usage must document the no-subcommand serve default")
	}

	// version: exit 0, does not serve.
	serve, code = dispatchCLI([]string{"version"}, "")
	if serve || code != 0 {
		t.Fatalf("version: serve=%v code=%d", serve, code)
	}
}

// Every declared command must be wired in the switch: a cliCommands
// entry without a case would hit the declared-but-unwired guard (exit
// 3). We assert the guard fires by temporarily probing with a fake
// declared entry semantics — i.e. every REAL entry must route somewhere
// other than the default-with-declaration branch. Cheapest honest
// approximation: unknown words exit 2, and the known set is exactly
// cliCommands (anything else errors), which the table above pins.
func TestDispatch_ExitCodes(t *testing.T) {
	if _, code := dispatchCLI([]string{"nope"}, ""); code != 2 {
		t.Fatalf("unknown subcommand exit=%d want 2", code)
	}
	if _, code := dispatchCLI([]string{"migrate"}, ""); code != 2 {
		t.Fatalf("migrate without -db exit=%d want 2", code)
	}
}

// SQLite-only commands must refuse a Postgres DSN outright — never derive
// a file path from it (they used to create a SQLite file named after the
// connection string, password included).
func TestDispatch_SQLiteOnlyCommandsRefusePostgres(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	dsn := "postgres://u:pw@127.0.0.1:1/ae?sslmode=disable"
	for _, args := range [][]string{
		{"backup"}, {"doctor"}, {"restore", "-from", "x.db"}, {"test-conversion", "-affiliate", "1"},
	} {
		serve, code := dispatchCLI(args, dsn)
		if serve || code != 1 {
			t.Fatalf("%v on postgres: serve=%v code=%d want refusal (1)", args, serve, code)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("refused commands created files: %v", names)
	}
	if err := requireSQLite("doctor", "sqlite:"+filepath.Join(dir, "a.db")); err != nil {
		t.Fatalf("sqlite dsn refused: %v", err)
	}
}
