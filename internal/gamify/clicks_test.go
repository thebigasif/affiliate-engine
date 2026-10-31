package gamify_test

import (
	"context"
	"testing"
)

// G5: streak + traffic badges fire from click stats.
func TestGamify_ClickBadges(t *testing.T) {
	_, e := newEnv(t)
	ctx := context.Background()
	if err := e.OnClickStats(ctx, 1, 30, 12000); err != nil {
		t.Fatal(err)
	}
	// Replay: no double XP.
	if err := e.OnClickStats(ctx, 1, 30, 12000); err != nil {
		t.Fatal(err)
	}
	p, _ := e.Profile(ctx, 1)
	// 500 streak + 1000 traffic = 1500 XP exactly once.
	if p["xp"].(int64) != 1500 {
		t.Fatalf("xp=%v want 1500", p["xp"])
	}
	codes := map[string]bool{}
	for _, b := range p["badges"].([]map[string]any) {
		codes[b["code"].(string)] = true
	}
	if !codes["streak_30"] || !codes["traffic_master"] {
		t.Fatalf("badges=%v", codes)
	}
	// Below thresholds: nothing.
	if err := e.OnClickStats(ctx, 2, 5, 100); err != nil {
		t.Fatal(err)
	}
	p2, _ := e.Profile(ctx, 2)
	if p2["xp"].(int64) != 0 {
		t.Fatalf("affiliate 2 xp=%v want 0", p2["xp"])
	}
}
