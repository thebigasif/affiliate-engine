package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// runBench builds its own throwaway temp db, so tests exercise the real
// code path with small N. Every runBench call here passes -gate=false:
// with gates on, a slow dashboard query under parallel test load makes
// runBench os.Exit(1) — which kills the whole test binary with no
// --- FAIL line (observed in full-suite -race runs). Perf gates belong
// to the operator bench on a quiet machine, not to the test matrix.

// Tiny run, human output: must succeed end-to-end (pipeline + drift +
// dashboard) and print the verdict line.
func TestBench_RunSmall(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runBench([]string{"-clicks", "300", "-dashboard-iterations", "50", "-gate=false"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "click pipeline") || !strings.Contains(out, "dashboard p95") {
		t.Fatalf("output missing sections:\n%s", out)
	}
	if !strings.Contains(out, "drift check    : aggregates == raw: OK") {
		t.Fatalf("drift check not OK in output:\n%s", out)
	}
}

// -json: machine-readable contract — parses, carries the measured
// fields, and reports the click gate as advisory at small N. Perf-gate
// booleans (dashboard_gate_ok / all_ok) are machine-speed-dependent and
// deliberately NOT asserted here: under full-suite parallel load p95 can
// spike past 10ms on any dev machine. The gates are the operator bench's
// job on a quiet box.
func TestBench_JSON(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runBench([]string{"-clicks", "300", "-dashboard-iterations", "50", "-json", "-gate=false"}); err != nil {
			t.Fatal(err)
		}
	})
	var rep benchReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json parse: %v\n%s", err, out)
	}
	if rep.Clicks != 300 || rep.ClicksPerSec <= 0 {
		t.Fatalf("clicks=%d perSec=%d", rep.Clicks, rep.ClicksPerSec)
	}
	if !rep.DriftOK || rep.DashboardP95Ms <= 0 || rep.DashboardP50Ms <= 0 || rep.DashboardP50Ms > rep.DashboardP95Ms {
		t.Fatalf("measurement fields wrong: %+v", rep)
	}
	if rep.ClickGateEnforced || rep.ClickGateOK != nil {
		t.Fatalf("click gate must be advisory below %d clicks: %+v", clickGateMinN, rep)
	}
}

// Invalid args are rejected without side effects.
func TestBench_BadArgs(t *testing.T) {
	if err := runBench([]string{"-clicks", "0"}); err == nil {
		t.Fatal("clicks=0 must error")
	}
	if err := runBench([]string{"-dashboard-iterations", "0"}); err == nil {
		t.Fatal("iterations=0 must error")
	}
}

// captureStdout swaps os.Stdout for a pipe around fn (runBench writes
// its report to os.Stdout directly).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()

	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		return out
	case <-time.After(10 * time.Second):
		t.Fatal("stdout capture timed out")
		return ""
	}
}
