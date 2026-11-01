package privacy

// Stored payload scrubbing: raw_events, webhook_events and outbox_events
// keep JSON copies of billing events (the buyer's email, the coupon)
// next to the money rows. Erasure rewrites those JSON values in place.
// The rows themselves stay: their (source, external_event_id) is the
// duplicate-delivery guard, and the money rows they produced are kept
// under pseudonymous ids. Provider customer ids are never touched: they
// are pseudonymous and the engine still reads them (lifetime bindings,
// a parked invoice's late credit).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

// redacted replaces an erased value in stored JSON. It is the empty
// string, the engine's own "unknown" for every field it scrubs (email,
// coupon), so a payload the engine reads again (a parked invoice, a held
// refund) decodes as if the value had never been sent. A placeholder
// such as "[erased]" would be read as a real coupon or email, and two
// erased buyers would share it.
const redacted = ""

// scrubber knows one data subject's identifiers and redacts them from
// decoded JSON.
type scrubber struct {
	// emails are lowercased addresses. A string value that contains one,
	// or that is an address with the same alias-normalized form, is
	// replaced whatever its key.
	emails []string
	norms  map[string]bool
	// exact are whole values (trimmed, case-insensitive) replaced
	// wherever they appear: a full name or bank text. Only long values
	// go here, so a short common word never matches by accident.
	exact map[string]bool
	// keyed are values replaced only under a key whose lowercased name
	// contains the map key, for identifiers that can collide with
	// ordinary values: coupon codes under "coupon".
	keyed map[string]map[string]bool
	// origs are the identifiers as given (trimmed, original case), for
	// the SQL prefilter on SQLite, whose LOWER folds ASCII only.
	origs []string
}

func newScrubber() *scrubber {
	return &scrubber{norms: map[string]bool{}, exact: map[string]bool{}, keyed: map[string]map[string]bool{}}
}

// minExactLen is the shortest whole value matched without a key.
const minExactLen = 5

func (sc *scrubber) addEmail(e string) {
	orig := strings.TrimSpace(e)
	e = strings.ToLower(orig)
	if e == "" || !strings.Contains(e, "@") || strings.HasSuffix(e, "@erased.invalid") {
		return
	}
	for _, have := range sc.emails {
		if have == e {
			return
		}
	}
	sc.emails = append(sc.emails, e)
	sc.origs = append(sc.origs, orig)
	sc.norms[fraud.NormalizeEmail(e)] = true
}

func (sc *scrubber) addExact(v string) {
	orig := strings.TrimSpace(v)
	v = strings.ToLower(orig)
	if len(v) >= minExactLen {
		sc.exact[v] = true
		sc.origs = append(sc.origs, orig)
	}
}

func (sc *scrubber) addKeyed(keyPart, v string) {
	orig := strings.TrimSpace(v)
	v = strings.ToLower(orig)
	if v == "" {
		return
	}
	sc.origs = append(sc.origs, orig)
	if sc.keyed[keyPart] == nil {
		sc.keyed[keyPart] = map[string]bool{}
	}
	sc.keyed[keyPart][v] = true
}

func (sc *scrubber) empty() bool {
	return len(sc.emails) == 0 && len(sc.exact) == 0 && len(sc.keyed) == 0
}

// prefilter returns LIKE patterns (escaped, %-wrapped, lowercased) that
// narrow the payloads read into Go to those holding a full identifier:
// each address with its alias-normalized and +tag forms, and each exact
// or keyed value. A Gmail dot variant ("j.ohn@") is not found by these
// patterns; the same limit applies to the fraud-text scrub.
func (sc *scrubber) prefilter() []string {
	vals := append([]string(nil), sc.emails...)
	for v := range sc.exact {
		vals = append(vals, v)
	}
	for _, vs := range sc.keyed {
		for v := range vs {
			vals = append(vals, v)
		}
	}
	set := map[string]bool{}
	for _, e := range sc.emails {
		for _, p := range emailResiduePatterns(e) {
			set[p] = true
		}
	}
	for _, v := range vals {
		set["%"+likeEscape(v)+"%"] = true
	}
	// SQLite's LOWER folds ASCII only; PostgreSQL folds all letters. The
	// value as given, ASCII-folded, finds a non-ASCII value stored in
	// that same case on SQLite.
	for _, o := range sc.origs {
		set["%"+likeEscape(asciiLower(o))+"%"] = true
	}
	pats := make([]string, 0, len(set))
	for p := range set {
		pats = append(pats, p)
	}
	sort.Strings(pats)
	return pats
}

// asciiLower lowers ASCII letters only, like SQLite's LOWER.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// likeAny renders "(LOWER(CAST(col AS TEXT)) LIKE ? ESCAPE '\' OR ...)".
func likeAny(col string, pats []string) (string, []any) {
	if len(pats) == 0 {
		return "1 = 0", nil
	}
	parts := make([]string, len(pats))
	args := make([]any, len(pats))
	for i, p := range pats {
		parts[i] = `LOWER(CAST(` + col + ` AS TEXT)) LIKE ? ESCAPE '\'`
		args[i] = p
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// matchString reports whether a string value identifies the subject,
// regardless of its key.
func (sc *scrubber) matchString(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	if l == "" {
		return false
	}
	if sc.exact[l] {
		return true
	}
	for _, e := range sc.emails {
		if containsEmail(l, e) {
			return true
		}
	}
	if strings.Contains(l, "@") && !strings.ContainsAny(l, " \t\n") && sc.norms[fraud.NormalizeEmail(l)] {
		return true
	}
	return false
}

// containsEmail reports whether s holds the address e as a whole address:
// "a@b.co" is not inside "aa@b.com".
func containsEmail(s, e string) bool {
	for from := 0; ; {
		i := strings.Index(s[from:], e)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(e)
		if (start == 0 || !isEmailByte(s[start-1])) && (end == len(s) || !isEmailByte(s[end])) {
			return true
		}
		from = start + 1
	}
}

func isEmailByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("._%+-", c) >= 0 || c >= 0x80
}

// matchKeyed reports whether s, stored under key, is a keyed identifier.
func (sc *scrubber) matchKeyed(key, s string) bool {
	k := strings.ToLower(key)
	l := strings.ToLower(strings.TrimSpace(s))
	for part, vals := range sc.keyed {
		if strings.Contains(k, part) && vals[l] {
			return true
		}
	}
	return false
}

// walk redacts matching values in decoded JSON and reports a change.
func (sc *scrubber) walk(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		for k, child := range t {
			if s, ok := child.(string); ok && sc.matchKeyed(k, s) {
				t[k] = redacted
				changed = true
				continue
			}
			if nv, c := sc.walk(child); c {
				t[k] = nv
				changed = true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, child := range t {
			if nv, c := sc.walk(child); c {
				t[i] = nv
				changed = true
			}
		}
		return t, changed
	case string:
		if sc.matchString(t) {
			return redacted, true
		}
	}
	return v, false
}

// scrubJSON returns the rewritten payload, or ok=false when nothing in it
// identifies the subject (or it is not JSON: left as it is).
func (sc *scrubber) scrubJSON(payload []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber() // large ids and amounts keep their exact digits
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	doc, changed := sc.walk(doc)
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}

// payloadTable is one stored-payload table and its key column.
type payloadTable struct {
	name, key string
}

var payloadTables = []payloadTable{
	{"raw_events", "id"},
	{"webhook_events", "id"},
	{"outbox_events", "id"},
}

// scrubPayloads rewrites every stored payload that identifies the subject
// and returns the rows changed per table. Event ids, sources, types and
// timestamps are untouched.
func (sc *scrubber) scrubPayloads(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	out := map[string]int64{}
	if sc.empty() {
		return out, nil
	}
	where, args := likeAny("payload", sc.prefilter())
	for _, pt := range payloadTables {
		q := fmt.Sprintf(`SELECT %s, payload FROM %s WHERE %s`, pt.key, pt.name, where)
		n, err := rewritePayloads(ctx, tx, pt, q, args, sc.scrubJSON)
		if err != nil {
			return nil, err
		}
		out[pt.name] = n
	}
	return out, nil
}

// scrubPayoutChangeNotices blanks the masked before and after details in
// the affiliate's queued payout-details-changed notices.
func scrubPayoutChangeNotices(ctx context.Context, tx *sql.Tx, affiliateID int64) (int64, error) {
	return rewritePayloads(ctx, tx, payloadTable{"outbox_events", "id"},
		`SELECT id, payload FROM outbox_events WHERE topic = ?`, []any{"mail.payout_details_changed"},
		func(payload []byte) ([]byte, bool) {
			dec := json.NewDecoder(bytes.NewReader(payload))
			dec.UseNumber()
			var doc map[string]any
			if dec.Decode(&doc) != nil {
				return nil, false
			}
			if n, ok := doc["affiliate_id"].(json.Number); !ok || n.String() != fmt.Sprint(affiliateID) {
				return nil, false
			}
			changed := false
			for _, k := range []string{"before", "after"} {
				if v, ok := doc[k].(string); ok && v != "" {
					doc[k] = ""
					changed = true
				}
			}
			if !changed {
				return nil, false
			}
			out, err := json.Marshal(doc)
			return out, err == nil
		})
}

// rewritePayloads selects (id, payload) rows with q, applies fn, and
// writes back the rows fn changed.
func rewritePayloads(ctx context.Context, tx *sql.Tx, pt payloadTable, q string, args []any,
	fn func([]byte) ([]byte, bool),
) (int64, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("privacy: erase %s: %w", pt.name, err)
	}
	type pending struct {
		id      int64
		payload string
	}
	var updates []pending
	for rows.Next() {
		var id int64
		var payload []byte
		if err := rows.Scan(&id, &payload); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("privacy: erase %s scan: %w", pt.name, err)
		}
		if out, ok := fn(payload); ok {
			updates = append(updates, pending{id, string(out)})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("privacy: erase %s rows: %w", pt.name, err)
	}
	_ = rows.Close()
	for _, u := range updates {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET payload = ? WHERE %s = ?`, pt.name, pt.key),
			u.payload, u.id); err != nil {
			return 0, fmt.Errorf("privacy: erase %s update: %w", pt.name, err)
		}
	}
	return int64(len(updates)), nil
}
