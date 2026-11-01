package httpapi

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// trimTo never cuts through a multi-byte character.
func TestTrimTo_CharacterBoundary(t *testing.T) {
	s := strings.Repeat("a", 119) + "é" // é is two bytes: bytes 119 and 120
	got := trimTo(s, 120)
	if !utf8.ValidString(got) || got != strings.Repeat("a", 119) {
		t.Fatalf("trimTo cut through a character: %q", got)
	}
	if got := trimTo("short", 120); got != "short" {
		t.Fatalf("short string changed: %q", got)
	}
	if got := trimTo("日本語", 4); got != "日" {
		t.Fatalf("trimTo(日本語, 4) = %q, want 日", got)
	}
}

// parseInt64 refuses values past int64 instead of wrapping them.
func TestParseInt64_Bounds(t *testing.T) {
	if n, err := parseInt64("42"); err != nil || n != 42 {
		t.Fatalf("42: %d %v", n, err)
	}
	for _, s := range []string{"", "-1", "1e3", "12a", "99999999999999999999"} {
		if n, err := parseInt64(s); err == nil {
			t.Errorf("%q accepted as %d", s, n)
		}
	}
}
