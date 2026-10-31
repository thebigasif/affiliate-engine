package fingerprint

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Table test: IP canonicalization — all spellings of one address must hash
// identically; different addresses must differ.

func TestUniqueHashIPCanonicalization(t *testing.T) {
	sameIP := [][]string{
		{"1.2.3.4", "::ffff:1.2.3.4"}, // v4 vs v4-mapped-v6
		{"2001:db8::1", "2001:DB8::1", "2001:0db8:0000:0000:0000:0000:0000:0001"}, // v6 case/zero compression
		{"::1", "0:0:0:0:0:0:0:1"}, // loopback expansions
	}
	for _, group := range sameIP {
		want := UniqueHash(group[0], "", "", ByIP)
		for _, spelling := range group[1:] {
			if got := UniqueHash(spelling, "", "", ByIP); got != want {
				t.Errorf("ByIP(%q) = %s, want %s (same address as %q)", spelling, got.Hex(), want.Hex(), group[0])
			}
		}
	}

	diffs := [][2]string{
		{"1.2.3.4", "1.2.3.5"},
		{"1.2.3.4", "1.2.3.40"},
		{"1.2.3.4", "5.6.7.8"},
		{"10.0.0.1", "10.0.0.1."}, // trailing dot: unparseable → raw string, distinct hash
	}
	for _, d := range diffs {
		if UniqueHash(d[0], "", "", ByIP) == UniqueHash(d[1], "", "", ByIP) {
			t.Errorf("ByIP(%q) == ByIP(%q); want different hashes", d[0], d[1])
		}
	}
}

// Table test: UA canonicalization — case and whitespace must not split
// identity; different UAs must differ.

func TestUniqueHashUACanonicalization(t *testing.T) {
	base := "Mozilla/5.0 (Macintosh) Chrome/126.0"
	variants := []string{
		"mozilla/5.0 (macintosh) chrome/126.0",      // lowercased
		"Mozilla/5.0    (Macintosh)   Chrome/126.0", // collapsed space runs
		"  Mozilla/5.0 (Macintosh) Chrome/126.0  ",  // trimmed
		"Mozilla/5.0\t(Macintosh)\nChrome/126.0",    // tabs/newlines are whitespace
	}
	want := UniqueHash("1.2.3.4", base, "", ByIPUA)
	for _, v := range variants {
		if got := UniqueHash("1.2.3.4", v, "", ByIPUA); got != want {
			t.Errorf("ByIPUA ua=%q = %s, want %s (canonical variant of %q)", v, got.Hex(), want.Hex(), base)
		}
	}
	if UniqueHash("1.2.3.4", base, "", ByIPUA) == UniqueHash("1.2.3.4", base+" X", "", ByIPUA) {
		t.Error("different UAs produced identical hash")
	}
}

// Length-prefixing must make ("ab","c") and ("a","bc") collide-proof.

func TestNewIdentityLengthPrefixNoCollision(t *testing.T) {
	a := NewIdentity(ByCookie, "ab", "c")
	b := NewIdentity(ByCookie, "a", "bc")
	if a == b {
		t.Fatalf("length-prefixed parts collided: %s == %s", a.Hex(), b.Hex())
	}
	// The canonical forms differ in their length prefixes: "2:ab1:c" vs "1:a2:bc".
	if "2:ab1:c" == "1:a2:bc" {
		t.Fatal("test bug: canonical strings unexpectedly equal")
	}
}

// The Method is NOT mixed into the digest: identical parts hash
// identically regardless of the method label (cookie-less degradation
// depends on this — see TestUniqueHashEmptyCookieDegrades).

func TestNewIdentityMethodNotHashed(t *testing.T) {
	if NewIdentity(ByIP, "x") != NewIdentity(ByIPUA, "x") {
		t.Error("identical parts under different methods must hash identically")
	}
	if NewIdentity(ByCookie, "a", "b") != NewIdentity(ByCookieIPUA, "a", "b") {
		t.Error("identical parts under different methods must hash identically")
	}
}

// Empty cookie contributes NO part: ByCookieIP/ByCookieIPUA must degrade
// exactly to ByIP/ByIPUA for cookie-less (first-visit) clicks.

func TestUniqueHashEmptyCookieDegrades(t *testing.T) {
	if UniqueHash("1.2.3.4", "UA", "", ByCookieIP) != UniqueHash("1.2.3.4", "UA", "", ByIP) {
		t.Error("ByCookieIP with empty cookie must equal ByIP")
	}
	if UniqueHash("1.2.3.4", "UA", "", ByCookieIPUA) != UniqueHash("1.2.3.4", "UA", "", ByIPUA) {
		t.Error("ByCookieIPUA with empty cookie must equal ByIPUA")
	}
	// And with a cookie present, it must NOT equal the degraded form.
	if UniqueHash("1.2.3.4", "UA", "ck1", ByCookieIP) == UniqueHash("1.2.3.4", "UA", "", ByCookieIP) {
		t.Error("cookie present must change the ByCookieIP hash")
	}
}

// Table test: each Method consumes exactly the expected attributes.

func TestUniqueHashMethods(t *testing.T) {
	ip, ua, ck := "9.9.9.9", "Mozilla/5.0", "cookie-42"
	allMethods := []Method{ByIP, ByIPUA, ByCookie, ByCookieIP, ByCookieIPUA}

	cases := []struct {
		desc     string
		vary     func(m Method) Hash // same method, one attribute changed
		affected map[Method]bool     // methods whose hash MUST change
	}{
		{
			desc:     "cookie change",
			vary:     func(m Method) Hash { return UniqueHash(ip, ua, ck+"x", m) },
			affected: map[Method]bool{ByCookie: true, ByCookieIP: true, ByCookieIPUA: true},
		},
		{
			desc:     "ua change",
			vary:     func(m Method) Hash { return UniqueHash(ip, ua+"x", ck, m) },
			affected: map[Method]bool{ByIPUA: true, ByCookieIPUA: true},
		},
		{
			desc:     "ip change",
			vary:     func(m Method) Hash { return UniqueHash("8.8.8.8", ua, ck, m) },
			affected: map[Method]bool{ByIP: true, ByIPUA: true, ByCookieIP: true, ByCookieIPUA: true},
		},
	}
	for _, c := range cases {
		for _, m := range allMethods {
			base := UniqueHash(ip, ua, ck, m)
			got := c.vary(m)
			if c.affected[m] && base == got {
				t.Errorf("%s: %v hash unexpectedly identical after change", c.desc, m)
			}
			if !c.affected[m] && base != got {
				t.Errorf("%s: %v hash changed but method must ignore the attribute", c.desc, m)
			}
		}
	}

	// Determinism.
	u1 := UniqueHash(ip, ua, ck, ByCookieIPUA)
	u2 := UniqueHash(ip, ua, ck, ByCookieIPUA)
	if !bytes.Equal(u1[:], u2[:]) {
		t.Error("UniqueHash not deterministic")
	}
}

// Hash helpers: 32-char lowercase hex, round-trips to the same bytes.

func TestHashHex(t *testing.T) {
	h := NewIdentity(ByIP, "1.2.3.4")
	s := h.Hex()
	if len(s) != 32 {
		t.Fatalf("Hex length = %d, want 32", len(s))
	}
	if s != strings.ToLower(s) {
		t.Errorf("Hex not lowercase: %q", s)
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("Hex not valid hex: %v", err)
	}
	if string(raw) != string(h[:]) {
		t.Errorf("Hex round-trip mismatch")
	}
}

// Table test: bot classification covers all three classes and UA variations.

func TestClassify(t *testing.T) {
	tests := []struct {
		ua            string
		acc, accLang  bool
		wantClass     BotClass
		wantReasonSub string
	}{
		// Known: empty/blank UA.
		{"", true, true, Known, "empty user-agent"},
		{"   ", true, true, Known, "empty user-agent"},

		// Known: bot tokens, case-insensitive substrings.
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://google.com/bot.html)", true, true, Known, "googlebot"},
		{"Mozilla/5.0 (compatible; bingbot/2.0)", true, true, Known, "bingbot"},
		{"Mozilla/5.0 (compatible; Yahoo! Slurp;)", true, true, Known, "slurp"},
		{"DuckDuckBot/1.0; (+http://duckduckgo.com/duckduckbot.html)", true, true, Known, "duckduckbot"},
		{"Mozilla/5.0 (compatible; Baiduspider/2.0)", true, true, Known, "baiduspider"},
		{"Mozilla/5.0 (compatible; YandexBot/3.0)", true, true, Known, "yandexbot"},
		{"facebookexternalhit/1.1", true, true, Known, "facebookexternalhit"},
		{"Twitterbot/1.0", true, true, Known, "twitterbot"},
		{"LinkedInBot/1.0", true, true, Known, "linkedinbot"},
		{"curl/8.4.0", true, true, Known, "curl"},
		{"Wget/1.21", true, true, Known, "wget"},
		{"python-requests/2.31.0", true, true, Known, "python-requests"},
		{"Python-urllib/3.11", true, true, Known, "python-urllib"},
		{"Go-http-client/2.0", true, true, Known, "go-http-client"},
		{"Java/1.8.0_321", true, true, Known, "java/"},
		{"okhttp/4.12.0", true, true, Known, "okhttp"},
		{"libwww-perl/6.36", true, true, Known, "libwww"},
		{"Apache-HttpClient/4.5.13", true, true, Known, "httpclient"},
		{"node-fetch", true, true, Known, "node-fetch"},
		{"Scrapy/2.11", true, true, Known, "scrapy"},
		{"Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/120.0", true, true, Known, "headlesschrome"},
		{"Mozilla/5.0 (Windows NT 6.1) PhantomJS/2.1.1", true, true, Known, "phantomjs"},

		// Suspected: BOTH Accept and Accept-Language missing.
		{"Mozilla/5.0 (Macintosh) Chrome/126.0", false, false, Suspected, "missing accept and accept-language"},
		// Single missing header stays Human (fail-open).
		{"Mozilla/5.0 (Macintosh) Chrome/126.0", false, true, Human, "ok"},
		{"Mozilla/5.0 (Macintosh) Chrome/126.0", true, false, Human, "ok"},

		// Human.
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/126.0.0.0 Safari/537.36", true, true, Human, "ok"},
	}
	for _, tt := range tests {
		class, reason := Classify(tt.ua, tt.acc, tt.accLang)
		if class != tt.wantClass {
			t.Errorf("Classify(%q, acc=%v, accLang=%v) class = %v, want %v (reason %q)",
				tt.ua, tt.acc, tt.accLang, class, tt.wantClass, reason)
		}
		if !strings.Contains(reason, tt.wantReasonSub) {
			t.Errorf("Classify(%q) reason = %q, want substring %q", tt.ua, reason, tt.wantReasonSub)
		}
	}
}

// The classifier is fail-open by construction: every input maps to a
// valid class and non-empty reason; no code path panics or rejects.

func TestClassifyFailOpen(t *testing.T) {
	inputs := []string{
		"", " ", "\x00\xff", repeat("a", 10_000),
		"正常なUA", repeat("curl", 3), strings.ToUpper("googlebot"),
	}
	for _, ua := range inputs {
		class, reason := Classify(ua, true, true)
		if class != Human && class != Suspected && class != Known {
			t.Errorf("Classify(%q) returned invalid class %d", ua, class)
		}
		if reason == "" {
			t.Errorf("Classify(%q) returned empty reason", ua)
		}
	}
	// GOOGLEBOT uppercase must still classify Known (case-insensitive).
	if class, _ := Classify("GOOGLEBOT", true, true); class != Known {
		t.Errorf("uppercase bot token not detected: class = %v", class)
	}
}

// BotClass / Method string helpers.

func TestStringers(t *testing.T) {
	if Human.String() != "human" || Suspected.String() != "suspected" || Known.String() != "known" {
		t.Error("BotClass.String names wrong")
	}
	if BotClass(9).String() == "human" {
		t.Error("invalid BotClass should not alias human")
	}
	for m, want := range map[Method]string{
		ByIP: "ip", ByIPUA: "ip+ua", ByCookie: "cookie",
		ByCookieIP: "cookie+ip", ByCookieIPUA: "cookie+ip+ua",
	} {
		if m.String() != want {
			t.Errorf("Method(%d).String() = %q, want %q", int(m), m.String(), want)
		}
	}
	if Method(9).String() == "ip" {
		t.Error("invalid Method should not alias ByIP")
	}
}

// Bench-style: 100k UniqueHash calls must complete under 2s.

func TestUniqueHashThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping throughput test in -short mode")
	}
	if raceEnabled {
		t.Skip("throughput budget not meaningful under -race")
	}
	ips := []string{"1.2.3.4", "::ffff:1.2.3.4", "2001:db8::1", "not-an-ip", "9.9.9.9"}
	uas := []string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/126.0.0.0 Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) Safari/604.1",
		"curl/8.4.0",
	}
	methods := []Method{ByIP, ByIPUA, ByCookie, ByCookieIP, ByCookieIPUA}

	const n = 100_000
	var sink Hash
	pass := func() time.Duration {
		start := time.Now()
		for i := 0; i < n; i++ {
			sink = UniqueHash(
				ips[i%len(ips)],
				uas[i%len(uas)],
				fmt.Sprintf("cookie-%d", i%997),
				methods[i%len(methods)],
			)
		}
		return time.Since(start)
	}
	// Best of 3: load on a shared machine only ever adds time.
	elapsed := pass()
	for range 2 {
		elapsed = min(elapsed, pass())
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("100k UniqueHash calls took %v, budget 2s (sink=%s)", elapsed, sink.Hex())
	}
	t.Logf("100k UniqueHash calls in %v (%.2f µs/call)", elapsed, float64(elapsed.Microseconds())/float64(n))
}

func repeat(s string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(s)
	}
	return b.String()
}
