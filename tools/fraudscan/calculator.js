// calculator.js - the no-upload version of the fraud scan.
//
// Pure functions, no dependencies, no I/O. The visitor types three numbers;
// nothing leaves the page.
//
// The fraud rate is ALWAYS the visitor's own guess. The presets below are
// round numbers to start from. They are not industry statistics and must not
// be presented as such.

export const PRESETS = Object.freeze({
  low: Object.freeze({ pct: 2, label: 'Low (your guess: 2%)' }),
  typical: Object.freeze({ pct: 5, label: 'Middle (your guess: 5%)' }),
  high: Object.freeze({ pct: 10, label: 'High (your guess: 10%)' }),
});

export function presetOptions() {
  return Object.entries(PRESETS).map(([key, p]) => ({ key, pct: p.pct, label: p.label }));
}

function num(v, name, max) {
  const n = typeof v === 'string' ? Number(v.replace(/[^0-9.\-]/g, '')) : Number(v);
  if (!Number.isFinite(n) || n < 0 || (max !== undefined && n > max)) {
    throw new RangeError(`calculator: ${name} must be a number from 0${max !== undefined ? ' to ' + max : ''}`);
  }
  return n;
}

// formatCents(123456) -> "1,234.56". No currency symbol: the page adds it.
export function formatCents(cents) {
  const neg = cents < 0;
  const abs = Math.abs(Math.round(cents));
  const whole = Math.floor(abs / 100).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  const frac = String(abs % 100).padStart(2, '0');
  return `${neg ? '-' : ''}${whole}.${frac}`;
}

// estimateRisk
//   monthlyPayouts: affiliate commissions paid per month (currency units)
//   couponSharePct: share of those payouts that came from coupon-code sales (0-100)
//   fraudRatePct:   the visitor's guess (0-100), or
//   preset:         'low' | 'typical' | 'high' (used when fraudRatePct is absent)
//   currencySymbol: optional, only used in the text lines (default "$")
export function estimateRisk({ monthlyPayouts, couponSharePct, fraudRatePct, preset, currencySymbol = '$' } = {}) {
  const monthly = num(monthlyPayouts, 'monthly payouts');
  const couponShare = num(couponSharePct ?? 0, 'coupon share', 100);
  let rate;
  if (fraudRatePct !== undefined && fraudRatePct !== null && fraudRatePct !== '') {
    rate = num(fraudRatePct, 'fraud rate', 100);
  } else if (preset !== undefined) {
    if (!Object.hasOwn(PRESETS, preset)) throw new RangeError(`calculator: unknown preset "${preset}"`);
    rate = PRESETS[preset].pct;
  } else {
    throw new RangeError('calculator: fraud rate is required (a number from 0 to 100, or a preset)');
  }

  const yearly = Math.round(monthly * 100) * 12;
  const atRisk = Math.round((yearly * rate) / 100);
  const couponPayouts = Math.round((yearly * couponShare) / 100);
  const couponAtRisk = Math.round((atRisk * couponShare) / 100);
  const linkAtRisk = atRisk - couponAtRisk;
  const monthlyAtRisk = Math.round(atRisk / 12);
  const m = (c) => currencySymbol + formatCents(c);

  return {
    inputs: { monthly_payouts: monthly, coupon_share_pct: couponShare, fraud_rate_pct: rate },
    fraud_rate_pct: rate,
    fraud_rate_source: 'your guess',
    yearly_payouts_cents: yearly,
    yearly_at_risk_cents: atRisk,
    monthly_at_risk_cents: monthlyAtRisk,
    breakdown: {
      coupon_payouts_cents: couponPayouts,
      link_payouts_cents: yearly - couponPayouts,
      coupon_at_risk_cents: couponAtRisk,
      link_at_risk_cents: linkAtRisk,
    },
    lines: [
      `You pay about ${m(yearly)} a year in affiliate commissions.`,
      `If ${rate}% of it is fraud (your guess), that is ${m(atRisk)} a year.`,
      `About ${m(couponAtRisk)} of that is on coupon code sales. ${m(linkAtRisk)} is on link sales.`,
      `That is roughly ${m(monthlyAtRisk)} a month.`,
    ],
  };
}
