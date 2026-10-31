// Run: node tools/fraudscan/test.mjs
// Regenerate the checked-in parity files after an intended change:
//   node tools/fraudscan/test.mjs --update
// then run `go test ./tools/fraudscan/` so the Go engine re-confirms them.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  scan,
  parseCSV,
  normalizeEmail,
  normalizeSlug,
  parseTimestamp,
  parseMoneyCents,
  detectTool,
  normalizedForParity,
  GO_RULES,
  SCAN_ONLY_RULES,
} from './fraudscan.js';
import { estimateRisk, PRESETS, presetOptions, formatCents } from './calculator.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const td = (f) => path.join(here, 'testdata', f);
const read = (f) => fs.readFileSync(td(f), 'utf8');

const FIXTURES = {
  'rewardful-affiliates.csv': 'rewardful',
  'firstpromoter-affiliates.csv': 'firstpromoter',
  'tapfiliate-affiliates.csv': 'tapfiliate',
  'tolt-affiliates.csv': 'tolt',
  'rewardful-referrals.csv': 'rewardful',
  'firstpromoter-commissions.csv': 'firstpromoter',
  'tapfiliate-conversions.csv': 'tapfiliate',
  'tolt-referrals.csv': 'tolt',
};

// The shape both this test and parity_test.go assert.
function expectedShape(r) {
  const go = {};
  const scanOnly = {};
  for (const f of r.findings) {
    const bucket = f.scan_only ? scanOnly : go;
    bucket[f.rule] ??= {};
    bucket[f.rule][f.affiliate] = f.count;
  }
  return {
    tool: r.tool,
    export_kind: r.export_kind,
    parse: r.parse,
    rules_run: r.rules_run,
    rules_skipped: r.rules_skipped.map((s) => s.rule),
    go_findings: go,
    scan_only_findings: scanOnly,
    summary: r.summary,
  };
}

if (process.argv.includes('--update')) {
  const expected = {};
  const normalized = {};
  for (const f of Object.keys(FIXTURES)) {
    expected[f] = expectedShape(scan(read(f)));
    normalized[f] = normalizedForParity(read(f));
  }
  fs.writeFileSync(td('expected.json'), JSON.stringify(expected, null, 2) + '\n');
  fs.writeFileSync(td('normalized.json'), JSON.stringify(normalized, null, 2) + '\n');
  console.log('wrote testdata/expected.json and testdata/normalized.json');
}

const expected = JSON.parse(read('expected.json'));
const normalized = JSON.parse(read('normalized.json'));

test('every fixture matches the checked-in expected.json (also asserted by Go)', () => {
  for (const f of Object.keys(FIXTURES)) {
    assert.deepEqual(expectedShape(scan(read(f))), expected[f], f);
  }
});

test('normalized rows match what parity_test.go replays', () => {
  for (const f of Object.keys(FIXTURES)) {
    assert.deepEqual(normalizedForParity(read(f)), normalized[f], f);
  }
});

test('auto-detects the tool for every fixture', () => {
  for (const [f, tool] of Object.entries(FIXTURES)) {
    const r = scan(read(f));
    assert.equal(r.tool, tool, f);
    assert.equal(r.tool_detected, true);
  }
});

test('explicit tool wins over detection and never changes the mapping', () => {
  const a = scan(read('tolt-referrals.csv'));
  const b = scan(read('tolt-referrals.csv'), { tool: 'rewardful' });
  assert.equal(b.tool, 'rewardful');
  assert.equal(b.tool_detected, false);
  assert.deepEqual(b.findings, a.findings);
  assert.throws(() => scan(read('tolt-referrals.csv'), { tool: 'impact' }), /unknown tool/);
});

// Hand-computed expectations (so --update can't quietly bless a regression).
const count = (r, rule, aff) => r.findings.find((f) => f.rule === rule && f.affiliate === aff)?.count ?? 0;

test('rewardful referrals: self-referral via gmail aliases, shared buyers, refunds', () => {
  const r = scan(read('rewardful-referrals.csv'));
  assert.equal(r.export_kind, 'conversions');
  assert.equal(r.parse.skipped, 1); // row with no affiliate
  assert.equal(count(r, 'self_referral', 'owner@gmail.com'), 2); // dots+tag and googlemail
  // payment_fingerprint fires once 2 buyers are shared: bob's 2nd sale, jane's 3rd.
  assert.equal(count(r, 'payment_fingerprint', 'bob@shop.io'), 1);
  assert.equal(count(r, 'payment_fingerprint', 'jane@shop.io'), 1);
  assert.equal(count(r, 'refund_rate', 'rick@refunds.io'), 2);
  const sr = r.findings.find((f) => f.rule === 'self_referral');
  assert.equal(sr.amount_cents, 2940);
  assert.equal(r.summary.commission_at_risk_cents, 2940 + 2970 * 2 + 570 * 2);
  assert.equal(r.summary.affiliates_flagged, 4);
  assert.ok(r.rules_skipped.some((s) => s.rule === 'coupon_hijack'));
  assert.ok(r.rules_skipped.some((s) => s.rule === 'ip_overlap' && /IP/.test(s.reason)));
});

test('firstpromoter commissions: coupon hijack at the 3rd stolen customer, alias twins', () => {
  const r = scan(read('firstpromoter-commissions.csv'));
  assert.equal(count(r, 'coupon_hijack', 'leaker@fp.dev'), 2);
  assert.equal(count(r, 'payment_fingerprint', 'leaker@fp.dev'), 3);
  assert.equal(count(r, 'duplicate_account', 'dup@gmail.com'), 1);
  assert.equal(count(r, 'duplicate_account', 'd.u.p+2@gmail.com'), 1);
  assert.equal(count(r, 'refund_rate', 'ann@fp.dev'), 0); // 4 sales: under the minimum of 5
  // "$1,200.00" style amounts parse; each flagged sale counted once.
  assert.equal(r.summary.commission_at_risk_cents, 1800 * 3 + 36000 * 2);
});

test('tapfiliate conversions: velocity, IP rules, fast click-to-sale', () => {
  const r = scan(read('tapfiliate-conversions.csv'));
  assert.equal(count(r, 'conversion_velocity', 'burst@tap.io'), 23); // sales 30..52 in the hour
  assert.equal(count(r, 'click_velocity', 'burst@tap.io'), 3); // sales 50..52 from one IP
  assert.equal(count(r, 'ip_overlap', 'farm1@tap.io'), 2);
  assert.equal(count(r, 'ip_overlap', 'farm2@tap.io'), 1);
  assert.equal(count(r, 'click_to_conversion', 'burst@tap.io'), 52);
  assert.equal(count(r, 'ip_overlap', 'solo@tap.io'), 0);
  assert.ok(r.rules_skipped.some((s) => s.rule === 'self_referral' && /customer email/.test(s.reason)));
});

test('tolt referrals: night-heavy affiliate flagged, sorted by time not file order', () => {
  const r = scan(read('tolt-referrals.csv'));
  assert.ok(count(r, 'conversion_time_anomaly', 'night@tolt.app') > 0);
  assert.equal(count(r, 'conversion_time_anomaly', 'day@tolt.app'), 0);
});

test('affiliate lists: mirror importer.Parse counts and flag alias twins', () => {
  const rw = scan(read('rewardful-affiliates.csv'));
  assert.deepEqual(rw.parse, { rows: 4, affiliates: 4, skipped: 0, dup_emails: 0 });
  assert.deepEqual(rw.rules_run, ['duplicate_account']);
  assert.deepEqual(
    rw.findings.map((f) => f.affiliate).sort(),
    ['jane+promo@shop.io', 'jane@shop.io'],
  );
  const fp = scan(read('firstpromoter-affiliates.csv'));
  assert.equal(fp.parse.skipped, 1); // not-an-email, as in the Go test
  const tolt = scan(read('tolt-affiliates.csv'));
  assert.equal(tolt.parse.dup_emails, 1); // in-file dup, as in the Go test
  // banned@shop.io is "blocked" -> frozen: it would not count as an active twin.
  const n = normalizedForParity(read('rewardful-affiliates.csv'));
  assert.equal(n.affiliates.find((a) => a.email === 'banned@shop.io').status, 'frozen');
  assert.equal(n.affiliates.find((a) => a.email === 'jane@shop.io').code, 'janeslink');
});

test('every Go rule and scan-only check is either run or skipped with a reason', () => {
  for (const f of Object.keys(FIXTURES)) {
    const r = scan(read(f));
    const all = [...r.rules_run, ...r.rules_skipped.map((s) => s.rule)].sort();
    assert.deepEqual(all, [...GO_RULES, ...SCAN_ONLY_RULES].sort(), f);
    for (const s of r.rules_skipped) assert.ok(s.reason.length > 10, `${f}: ${s.rule}`);
  }
});

test('unrecognized header errors like the Go importer', () => {
  assert.throws(() => scan('a,b,c\n1,2,3\n'), /unrecognized export header/);
  assert.throws(() => scan(''), /empty file/);
});

test('BOM, CRLF, quotes and blank lines', () => {
  const csv = '﻿email,name\r\n"jane@shop.io","Doe, Jane"\r\n\r\n"bob ""b"" @x.io",Bob\r\n';
  assert.deepEqual(parseCSV(csv), [
    ['email', 'name'],
    ['jane@shop.io', 'Doe, Jane'],
    ['bob "b" @x.io', 'Bob'],
  ]);
  const r = scan(csv);
  assert.equal(r.export_kind, 'affiliates');
  assert.equal(r.parse.affiliates, 2);
});

test('date-only exports skip time-of-day rules instead of guessing', () => {
  const csv =
    'affiliate_email,customer_email,commission,created_at\n' +
    'a@x.io,c1@y.io,10.00,2026-03-01\n' +
    'b@x.io,c2@y.io,10.00,2026-03-02\n';
  const r = scan(csv);
  const skipped = Object.fromEntries(r.rules_skipped.map((s) => [s.rule, s.reason]));
  assert.match(skipped.conversion_velocity, /time of day/);
  assert.match(skipped.conversion_time_anomaly, /time of day/);
  assert.ok(r.rules_run.includes('self_referral'));
});

test('no commission column: findings still reported, money left null with a warning', () => {
  const csv = 'affiliate_email,customer_email,created_at\nowner@gmail.com,owner+x@gmail.com,2026-03-01T10:00:00Z\n';
  const r = scan(csv);
  assert.equal(r.findings[0].rule, 'self_referral');
  assert.equal(r.findings[0].amount_cents, null);
  assert.equal(r.summary.commission_at_risk_cents, null);
  assert.ok(r.warnings.some((w) => /commission/.test(w)));
});

test('normalizers mirror Go', () => {
  // internal/fraud engine_test.go TestFraud_SelfReferralAliases cases.
  for (const a of ['owner@gmail.com', 'o.w.n.e.r@gmail.com', 'owner+blog@gmail.com', 'owner@googlemail.com']) {
    assert.equal(normalizeEmail(a), normalizeEmail('Owner@Gmail.com'));
  }
  assert.notEqual(normalizeEmail('o.wner@corp.io'), normalizeEmail('owner@corp.io'));
  assert.equal(normalizeSlug(' JanesLink! '), 'janeslink');
  assert.equal(normalizeSlug('a'), '');
  assert.equal(parseTimestamp('2026-03-01T10:00:00+02:00').ms, Date.UTC(2026, 2, 1, 8));
  assert.equal(parseTimestamp('2026-03-01 10:00').hasTime, true);
  assert.equal(parseTimestamp('03/01/2026'), null);
  assert.equal(parseMoneyCents('$1,234.50'), 123450);
  assert.equal(parseMoneyCents('(5.00)'), -500);
  assert.equal(parseMoneyCents('1250', 'commission_cents'), 1250);
  assert.equal(parseMoneyCents(''), null);
  assert.equal(detectTool(['a', 'b']), 'unknown');
});

test('the scan never touches the network', () => {
  const trap = () => {
    throw new Error('network call');
  };
  const saved = globalThis.fetch;
  globalThis.fetch = trap;
  try {
    for (const f of Object.keys(FIXTURES)) scan(read(f));
  } finally {
    globalThis.fetch = saved;
  }
  for (const f of ['fraudscan.js', 'calculator.js', 'demo.js']) {
    const src = fs.readFileSync(path.join(here, f), 'utf8');
    assert.doesNotMatch(src, /\bfetch\(|XMLHttpRequest|WebSocket|sendBeacon|EventSource|import\(/, f);
    assert.doesNotMatch(src, /innerHTML|outerHTML|insertAdjacentHTML|document\.write/, f);
  }
  const html = fs.readFileSync(path.join(here, 'demo.html'), 'utf8');
  assert.match(html, /Content-Security-Policy[^>]*connect-src 'none'/);
  assert.match(html, /Your file is read in your browser\. It is never uploaded\./);
});

// ---------------------------------------------------------------------------
// calculator.js
// ---------------------------------------------------------------------------

test('calculator: yearly money at risk and breakdown', () => {
  const r = estimateRisk({ monthlyPayouts: 10000, couponSharePct: 40, fraudRatePct: 5 });
  assert.equal(r.yearly_payouts_cents, 12_000_000);
  assert.equal(r.yearly_at_risk_cents, 600_000);
  assert.equal(r.monthly_at_risk_cents, 50_000);
  assert.equal(r.breakdown.coupon_payouts_cents, 4_800_000);
  assert.equal(r.breakdown.link_payouts_cents, 7_200_000);
  assert.equal(r.breakdown.coupon_at_risk_cents + r.breakdown.link_at_risk_cents, r.yearly_at_risk_cents);
  assert.equal(r.fraud_rate_source, 'your guess');
  assert.ok(r.lines.length >= 3);
  for (const l of r.lines) assert.ok(!l.includes(String.fromCharCode(0x2014))); // no em dashes
});

test('calculator: presets are labelled as guesses, not facts', () => {
  assert.deepEqual(
    Object.fromEntries(Object.entries(PRESETS).map(([k, v]) => [k, v.pct])),
    { low: 2, typical: 5, high: 10 },
  );
  for (const p of presetOptions()) assert.match(p.label, /your guess/i);
  const r = estimateRisk({ monthlyPayouts: 1000, couponSharePct: 0, preset: 'high' });
  assert.equal(r.fraud_rate_pct, 10);
  assert.equal(r.yearly_at_risk_cents, 120_000);
});

test('calculator: validates input', () => {
  assert.throws(() => estimateRisk({ monthlyPayouts: -1, couponSharePct: 0, fraudRatePct: 5 }), /payouts/);
  assert.throws(() => estimateRisk({ monthlyPayouts: 10, couponSharePct: 120, fraudRatePct: 5 }), /coupon/);
  assert.throws(() => estimateRisk({ monthlyPayouts: 10, couponSharePct: 10, fraudRatePct: 101 }), /fraud/);
  assert.throws(() => estimateRisk({ monthlyPayouts: 10, couponSharePct: 10 }), /fraud/);
  assert.throws(() => estimateRisk({ monthlyPayouts: 10, couponSharePct: 10, preset: 'extreme' }), /preset/);
  assert.equal(formatCents(123456), '1,234.56');
  assert.equal(formatCents(-5), '-0.05');
});
