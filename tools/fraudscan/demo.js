// demo.js - wiring for demo.html. Everything cell-derived is written with
// textContent only: CSV contents are untrusted and must never be parsed as HTML.
import { scan } from './fraudscan.js';
import { estimateRisk, presetOptions, formatCents } from './calculator.js';

const $ = (id) => document.getElementById(id);
const MAX_BYTES = 50 * 1024 * 1024;

const RULE_LABELS = {
  self_referral: 'Self-referral',
  click_velocity: 'Too many sales from one IP in an hour',
  conversion_velocity: 'Too many sales in an hour',
  coupon_hijack: 'Coupon taking other affiliates’ customers',
  conversion_time_anomaly: 'Sales at odd hours',
  ip_overlap: 'IPs shared with other affiliates',
  duplicate_account: 'Duplicate affiliate accounts',
  payment_fingerprint: 'Same buyers across affiliates',
  refund_rate: 'High refund rate',
  click_to_conversion: 'Sale seconds after the click',
};

const money = (c) => (c === null || c === undefined ? 'n/a' : formatCents(c));

function cell(tr, text) {
  const td = document.createElement('td');
  td.textContent = text;
  tr.appendChild(td);
}

function render(result) {
  const s = result.summary;
  const summary = $('summary');
  summary.textContent = '';
  const p = document.createElement('p');
  p.textContent =
    `${result.tool} ${result.export_kind}, ${result.rows} rows. ` +
    `${s.affiliates_flagged} affiliates flagged. ` +
    (s.commission_at_risk_cents === null
      ? ''
      : `Commission at risk: ${money(s.commission_at_risk_cents)}${result.currency ? ' ' + result.currency : ''}.`);
  summary.appendChild(p);
  for (const w of result.warnings) {
    const wp = document.createElement('p');
    wp.textContent = w;
    summary.appendChild(wp);
  }

  const tbody = $('findings').querySelector('tbody');
  tbody.textContent = '';
  for (const f of result.findings) {
    const tr = document.createElement('tr');
    cell(tr, (RULE_LABELS[f.rule] || f.rule) + (f.scan_only ? ' (scan only)' : ''));
    cell(tr, f.affiliate);
    cell(tr, f.unit === 'accounts' ? 'account' : String(f.count));
    cell(tr, money(f.amount_cents));
    cell(tr, f.examples.join(' | '));
    tbody.appendChild(tr);
  }
  $('findings').hidden = result.findings.length === 0;

  const ul = $('skipped');
  ul.textContent = '';
  for (const k of result.rules_skipped) {
    const li = document.createElement('li');
    li.textContent = `${RULE_LABELS[k.rule] || k.rule}: ${k.reason}`;
    ul.appendChild(li);
  }
}

$('file').addEventListener('change', async (e) => {
  const file = e.target.files[0];
  if (!file) return;
  if (file.size > MAX_BYTES) {
    $('status').textContent = 'That file is over 50 MB. Export a shorter date range.';
    return;
  }
  $('status').textContent = 'Reading on your device...';
  try {
    const text = await file.text(); // local read; no request is made
    const result = scan(text, { tool: $('tool').value || undefined });
    $('status').textContent = '';
    render(result);
  } catch (err) {
    $('status').textContent = err.message.replace(/^fraudscan: /, '');
  }
});

function recalc() {
  const ul = $('estimate');
  ul.textContent = '';
  try {
    const r = estimateRisk({
      monthlyPayouts: $('monthly').value,
      couponSharePct: $('coupon').value,
      fraudRatePct: $('rate').value,
    });
    for (const line of r.lines) {
      const li = document.createElement('li');
      li.textContent = line;
      ul.appendChild(li);
    }
  } catch (err) {
    const li = document.createElement('li');
    li.textContent = 'Enter numbers from 0 (percentages up to 100).';
    ul.appendChild(li);
  }
}

for (const p of presetOptions()) {
  const b = document.createElement('button');
  b.type = 'button';
  b.textContent = p.label;
  b.addEventListener('click', () => {
    $('rate').value = String(p.pct);
    recalc();
  });
  $('presets').appendChild(b);
}
for (const id of ['monthly', 'coupon', 'rate']) $(id).addEventListener('input', recalc);
recalc();
