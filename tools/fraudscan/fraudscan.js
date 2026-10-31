// fraudscan.js - free in-browser fraud scan for affiliate program exports.
//
// Dependency-free ES module. It reads the TEXT of a CSV export (the page
// reads the file locally; nothing is uploaded, and this module never makes
// a network request) and replays the Go engine's fraud rules over it.
//
// Two export shapes are understood:
//
//   1. Affiliate lists. Column mapping mirrors internal/importer/importer.go
//      (indexHeader + Parse) exactly: same header candidates, same
//      candidate-priority order, same email / status / slug handling, same
//      in-file duplicate collapse.
//   2. Conversion lists (referrals / commissions / conversions). The Go
//      importer does not read these (it never imports money rows), so this
//      mapping is scan-only. Its header candidates are listed in
//      CONVERSION_COLUMNS below.
//
// Replay model: rows are sorted by conversion time (file order when there
// are no timestamps) and every Go rule is evaluated at each conversion with
// only the rows seen so far, exactly as the engine screens a live
// conversion. A finding's `count` is how many conversions the engine would
// have put on hold. tools/fraudscan/parity_test.go runs the real Go engine
// over the same fixtures and asserts the same counts.

// ---------------------------------------------------------------------------
// Constants mirrored from Go. Change them only together with the Go source.
// ---------------------------------------------------------------------------

export const GO = Object.freeze({
  // internal/fraud/engine.go ClickVelocity: n < 50 passes (1h window).
  CLICK_VELOCITY_PER_IP_HOUR: 50,
  // internal/fraud/engine.go ConversionVelocity: n >= 30 (1h window).
  CONVERSION_VELOCITY_PER_HOUR: 30,
  // internal/fraud/engine.go DefaultCouponHijackThreshold (7d window).
  COUPON_HIJACK_THRESHOLD: 3,
  // internal/fraud/rules2.go ConversionTimeAnomaly: minSample, z > 3, share > 0.5, hour < 6.
  TIME_ANOMALY_MIN_SAMPLE: 20,
  TIME_ANOMALY_Z: 3,
  TIME_ANOMALY_MIN_NIGHT_SHARE: 0.5,
  TIME_ANOMALY_NIGHT_END_HOUR: 6,
  EPSILON: 1e-9,
  // internal/fraud/rules3.go IPOverlap: minShared = 2, window 30d.
  IP_OVERLAP_MIN_SHARED: 2,
  // internal/fraud/engine.go minClickRuleConversions.
  MIN_CLICK_RULE_CONVERSIONS: 3,
  // internal/fraud/rules3.go PaymentFingerprint: minShared = 2.
  PAYMENT_FINGERPRINT_MIN_SHARED: 2,
  HOUR_MS: 3600 * 1000,
  DAY_MS: 24 * 3600 * 1000,
});

// Scan-only checks. There is NO Go rule behind these: the engine gets
// refunds as clawbacks and has no click-to-conversion timer. Thresholds are
// our defaults, not industry figures, and can be overridden via options.
export const SCAN_DEFAULTS = Object.freeze({
  refundRateMinConversions: 5,
  refundRateMinShare: 0.2,
  fastConversionSeconds: 10,
  fastConversionMinCount: 3,
});

// Go default ruleset order: internal/fraud/engine.go defaultRules().
export const GO_RULES = Object.freeze([
  'self_referral',
  'click_velocity',
  'conversion_velocity',
  'coupon_hijack',
  'conversion_time_anomaly',
  'asn_reputation',
  'ip_overlap',
  'duplicate_account',
  'payment_fingerprint',
  'bot_class_gating',
  'geo_mismatch',
  'ip_fallback_anomaly',
  'inactive_affiliate',
]);

export const SCAN_ONLY_RULES = Object.freeze(['refund_rate', 'click_to_conversion']);

export const TOOLS = Object.freeze(['rewardful', 'firstpromoter', 'tapfiliate', 'tolt']);

// ---------------------------------------------------------------------------
// CSV
// ---------------------------------------------------------------------------

// parseCSV: RFC 4180 records. Mirrors encoding/csv as used by the importer
// (FieldsPerRecord = -1, blank lines skipped). Deviations, both lenient:
// a UTF-8 BOM is stripped (Go keeps it and then fails to match the first
// header), and a stray quote inside an unquoted field is kept as text
// (Go returns a parse error).
export function parseCSV(text) {
  if (typeof text !== 'string') throw new TypeError('fraudscan: CSV text must be a string');
  if (text.charCodeAt(0) === 0xfeff) text = text.slice(1);
  const out = [];
  let rec = [];
  let field = '';
  let quoted = false; // inside quotes
  let wasQuoted = false; // current field started with a quote
  let i = 0;
  const n = text.length;
  const endRecord = () => {
    rec.push(field);
    // encoding/csv skips empty lines entirely.
    if (!(rec.length === 1 && rec[0] === '' && !wasQuoted)) out.push(rec);
    rec = [];
    field = '';
    wasQuoted = false;
  };
  while (i < n) {
    const c = text[i];
    if (quoted) {
      if (c === '"') {
        if (text[i + 1] === '"') {
          field += '"';
          i += 2;
          continue;
        }
        quoted = false;
        i++;
        continue;
      }
      field += c;
      i++;
      continue;
    }
    if (c === '"' && field.trim() === '') {
      quoted = true;
      wasQuoted = true;
      field = '';
      i++;
      continue;
    }
    if (c === ',') {
      rec.push(field);
      field = '';
      wasQuoted = false;
      i++;
      continue;
    }
    if (c === '\r' || c === '\n') {
      endRecord();
      if (c === '\r' && text[i + 1] === '\n') i++;
      i++;
      continue;
    }
    field += c;
    i++;
  }
  if (field !== '' || rec.length > 0 || wasQuoted) endRecord();
  return out;
}

// ---------------------------------------------------------------------------
// Normalizers mirrored from Go
// ---------------------------------------------------------------------------

// normalizeEmail mirrors internal/fraud/engine.go normalizeEmail: lowercase,
// strip +tag, drop dots on gmail, googlemail -> gmail.
export function normalizeEmail(e) {
  e = String(e ?? '').trim().toLowerCase();
  const at = e.lastIndexOf('@');
  if (at < 0) return e;
  let local = e.slice(0, at);
  let domain = e.slice(at + 1);
  const plus = local.indexOf('+');
  if (plus > 0) local = local.slice(0, plus);
  if (domain === 'gmail.com' || domain === 'googlemail.com') {
    local = local.split('.').join('');
    if (domain === 'googlemail.com') domain = 'gmail.com';
  }
  return local + '@' + domain;
}

// normalizeSlug mirrors internal/importer/importer.go normalizeSlug.
export function normalizeSlug(s) {
  s = String(s ?? '').trim().toLowerCase();
  let b = '';
  for (const c of s) {
    if ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c === '-' || c === '_') b += c;
  }
  if (b.length < 2 || b.length > 32) return '';
  return b;
}

// mapAffiliateStatus mirrors the status switch in importer.Parse.
export function mapAffiliateStatus(s) {
  switch (String(s ?? '').trim().toLowerCase()) {
    case '':
    case 'active':
    case 'approved':
    case 'accepted':
    case 'enabled':
      return 'active';
    default:
      return 'frozen';
  }
}

// ---------------------------------------------------------------------------
// Header mapping
// ---------------------------------------------------------------------------

// Mirrors internal/importer/history.go normHeader: lowercase, BOM and edge
// spaces trimmed, "_" and "-" read as spaces, runs of spaces collapsed.
// Built from a char code so the source carries no escape sequence.
const BOM_RE = new RegExp('^' + String.fromCharCode(0xfeff));

export function normHeader(h) {
  return String(h ?? '').replace(BOM_RE, '').trim().toLowerCase()
    .replace(/[_-]/g, ' ').split(/\s+/).filter(Boolean).join(' ');
}

function lowerHeader(header) {
  return header.map(normHeader);
}

// pick mirrors indexHeader's closure: candidate priority first, then the
// leftmost column with that exact (trimmed, lowercased) name.
// Mirrors history.go mapHeader: a column serves one seat only.
function makePicker(lower, col) {
  const used = new Set();
  return (seat, candidates) => {
    for (const c of candidates) {
      const want = normHeader(c);
      for (let i = 0; i < lower.length; i++) {
        if (lower[i] === want && !used.has(i)) {
          used.add(i);
          col[seat] = i;
          return true;
        }
      }
    }
    return false;
  };
}

// Mirrors internal/importer/importer.go affSeats. Keep in lockstep.
export const AFFILIATE_COLUMNS = Object.freeze({
  email: ['email', 'email address', 'e-mail', 'affiliate email', 'promoter email'],
  name: ['name', 'full name', 'first name'],
  code: ['code', 'referral code', 'referral_id', 'ref_id', 'token', 'username', 'handle', 'slug'],
  status: ['status', 'state'],
  coupon: ['coupon', 'coupon code', 'coupons'],
});

export function mapAffiliateHeader(header) {
  const lower = lowerHeader(header);
  const col = {};
  const pick = makePicker(lower, col);
  if (!pick('email', AFFILIATE_COLUMNS.email)) return null;
  pick('name', AFFILIATE_COLUMNS.name);
  pick('code', AFFILIATE_COLUMNS.code);
  pick('status', AFFILIATE_COLUMNS.status);
  pick('coupon', AFFILIATE_COLUMNS.coupon);
  return col;
}

// Conversion exports: scan-only mapping (no Go counterpart). Header names
// are our reading of the incumbents' referral / commission exports and
// common spreadsheet naming. A bare "email" is deliberately NOT mapped here:
// in a conversion file it is ambiguous (affiliate or customer).
export const CONVERSION_COLUMNS = Object.freeze({
  affiliate_email: [
    'affiliate email', 'affiliate_email', 'affiliate e-mail', 'promoter email', 'promoter_email',
    'partner email', 'partner_email', 'referrer email', 'referrer_email',
  ],
  affiliate_id: [
    'affiliate id', 'affiliate_id', 'affiliate', 'affiliate name', 'affiliate_name', 'affiliate code',
    'affiliate_code', 'promoter id', 'promoter_id', 'promoter', 'promoter name', 'partner id',
    'partner_id', 'partner', 'partner name',
  ],
  customer_email: [
    'customer email', 'customer_email', 'customer e-mail', 'buyer email', 'buyer_email', 'lead email',
    'lead_email', 'referral email', 'referral_email', 'referred email', 'referred_email',
  ],
  customer_id: [
    'customer id', 'customer_id', 'stripe customer id', 'stripe_customer_id', 'external customer id',
    'customer external id', 'external_customer_id', 'lead id', 'lead_id', 'uid', 'customer',
  ],
  coupon: [
    'coupon', 'coupon code', 'coupon_code', 'coupons', 'promo code', 'promo_code', 'discount code',
    'discount_code',
  ],
  commission: [
    'commission', 'commission amount', 'commission_amount', 'commission_cents', 'commission cents',
    'commission_amount_cents', 'reward', 'reward amount', 'reward_amount', 'amount earned', 'earned',
    'payout amount', 'payout_amount',
  ],
  sale: [
    'sale amount', 'sale_amount', 'sale_amount_cents', 'amount', 'amount_cents', 'revenue',
    'order amount', 'order_amount', 'order total', 'order_total', 'sale', 'total',
  ],
  converted_at: [
    'conversion date', 'conversion_date', 'converted at', 'converted_at', 'conversion time',
    'created at', 'created_at', 'purchase date', 'sale date', 'date', 'timestamp',
  ],
  clicked_at: [
    'click date', 'click_date', 'clicked at', 'clicked_at', 'click time', 'click_at', 'visit date',
    'visited at', 'visited_at', 'first click', 'first_click_at',
  ],
  ip: ['ip', 'ip address', 'ip_address', 'customer ip', 'customer_ip', 'visitor ip', 'visitor_ip'],
  status: ['status', 'state', 'conversion status', 'commission status', 'referral status'],
  refunded: [
    'refunded', 'is_refunded', 'refund', 'refunded at', 'refunded_at', 'chargeback', 'disputed',
  ],
  currency: ['currency'],
});

export function mapConversionHeader(header) {
  const lower = lowerHeader(header);
  const col = {};
  const pick = makePicker(lower, col);
  for (const [seat, cands] of Object.entries(CONVERSION_COLUMNS)) pick(seat, cands);
  const hasAffiliate = 'affiliate_email' in col || 'affiliate_id' in col;
  const hasConversion =
    'customer_email' in col || 'customer_id' in col || 'commission' in col || 'converted_at' in col;
  if (!hasAffiliate || !hasConversion) return null;
  return col;
}

// Tool detection. The Go importer does NOT auto-detect (the operator passes
// --source) and its mapping is the same for all four tools, so detection
// only labels the report; it never changes how columns are read.
const TOOL_HINTS = {
  rewardful: ['referral_id', 'via', 'stripe_customer_id', 'rewardful'],
  firstpromoter: ['email address', 'referral id', 'promoter', 'promoter email', 'promoter id', 'fpr', 'firstpromoter'],
  tapfiliate: ['ref_id', 'conversion id', 'conversion_id', 'tapfiliate', 'affiliate id'],
  tolt: ['handle', 'coupons', 'partner', 'partner email', 'partner id', 'tolt'],
};

export function detectTool(header) {
  // Raw names here (not normHeader): "referral_id" vs "Referral ID" is
  // exactly what tells Rewardful from FirstPromoter.
  const lower = new Set(header.map((h) => String(h ?? '').replace(BOM_RE, '').trim().toLowerCase()));
  let best = 'unknown';
  let bestScore = 0;
  let tie = false;
  for (const tool of TOOLS) {
    const score = TOOL_HINTS[tool].filter((h) => lower.has(h)).length;
    if (score > bestScore) {
      best = tool;
      bestScore = score;
      tie = false;
    } else if (score === bestScore && score > 0) {
      tie = true;
    }
  }
  return tie ? 'unknown' : best;
}

// ---------------------------------------------------------------------------
// Value parsing
// ---------------------------------------------------------------------------

const TS_RE =
  /^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.\d+)?)?\s*(Z|UTC|[+-]\d{2}:?\d{2})?)?$/i;

// parseTimestamp accepts ISO 8601 style values only. Values without a zone
// are read as UTC. Returns {ms, hasTime} truncated to whole seconds (the Go
// engine stores RFC 3339 seconds), or null.
export function parseTimestamp(s) {
  const m = TS_RE.exec(String(s ?? '').trim());
  if (!m) return null;
  const [, y, mo, d, hh, mi, ss, zone] = m;
  const hasTime = hh !== undefined;
  let ms = Date.UTC(+y, +mo - 1, +d, hasTime ? +hh : 0, hasTime ? +mi : 0, ss ? +ss : 0);
  if (Number.isNaN(ms)) return null;
  if (zone && !/^(z|utc)$/i.test(zone)) {
    const zm = /^([+-])(\d{2}):?(\d{2})$/.exec(zone);
    const off = (+zm[2] * 60 + +zm[3]) * 60000;
    ms -= zm[1] === '+' ? off : -off;
  }
  return { ms, hasTime };
}

// parseMoneyCents: "$1,234.50" -> 123450. A header containing "cents" or
// "minor" means the value is already in cents. Assumes "." as the decimal
// separator. Parentheses mean negative. Returns null when unreadable.
export function parseMoneyCents(s, headerName = '') {
  let t = String(s ?? '').trim();
  if (t === '') return null;
  let neg = false;
  if (/^\(.*\)$/.test(t)) {
    neg = true;
    t = t.slice(1, -1);
  }
  t = t.replace(/[^0-9.\-]/g, '');
  if (t === '' || t === '-' || t === '.') return null;
  let v = Number(t);
  if (!Number.isFinite(v)) return null;
  if (neg) v = -Math.abs(v);
  const inCents = /cents|minor/i.test(headerName);
  return Math.round(inCents ? v : v * 100);
}

const REFUND_STATUSES = new Set([
  'refunded', 'refund', 'partially_refunded', 'partially refunded', 'chargeback', 'charged back',
  'chargedback', 'disputed', 'dispute', 'reversed',
]);

function truthyFlag(v) {
  const t = String(v ?? '').trim().toLowerCase();
  return !(t === '' || t === '0' || t === 'false' || t === 'no' || t === 'n' || t === 'none');
}

// dayInt mirrors internal/fraud/rules3.go dayInt (YYYYMMDD, UTC).
export function dayInt(ms) {
  const d = new Date(ms);
  return d.getUTCFullYear() * 10000 + (d.getUTCMonth() + 1) * 100 + d.getUTCDate();
}

// ---------------------------------------------------------------------------
// Parsing an export
// ---------------------------------------------------------------------------

// parseExport returns the normalized records the rules run on.
// opts.tool: one of TOOLS to force the label; otherwise detected.
export function parseExport(text, opts = {}) {
  const records = parseCSV(text);
  if (records.length === 0) throw new Error('fraudscan: empty file');
  const header = records[0];
  const body = records.slice(1);
  let tool = opts.tool ? String(opts.tool).toLowerCase() : '';
  if (tool && !TOOLS.includes(tool)) throw new Error(`fraudscan: unknown tool "${opts.tool}"`);
  const toolDetected = !tool;
  if (!tool) tool = detectTool(header);

  const ccol = mapConversionHeader(header);
  if (ccol) return parseConversions(header, body, ccol, tool, toolDetected);
  const acol = mapAffiliateHeader(header);
  if (acol) return parseAffiliates(header, body, acol, tool, toolDetected);
  throw new Error(
    `fraudscan: unrecognized export header: ${header.map((h) => String(h).trim()).join(', ')}`,
  );
}

function columnsReport(header, col) {
  const out = {};
  for (const [seat, i] of Object.entries(col)) out[seat] = String(header[i]).trim();
  return out;
}

// Mirrors importer.Parse row handling.
function parseAffiliates(header, body, col, tool, toolDetected) {
  const rep = { rows: 0, affiliates: 0, skipped: 0, dup_emails: 0 };
  const affiliates = [];
  const seen = new Set();
  for (const rec of body) {
    rep.rows++;
    const get = (name) => {
      const i = col[name];
      if (i === undefined || i >= rec.length) return '';
      return String(rec[i]).trim();
    };
    const email = get('email').toLowerCase();
    if (email === '' || !email.includes('@')) {
      rep.skipped++;
      continue;
    }
    if (seen.has(email)) {
      rep.dup_emails++;
      continue;
    }
    seen.add(email);
    affiliates.push({
      row: rep.rows + 1, // 1-based file line incl. header
      email,
      name: get('name'),
      code: normalizeSlug(get('code')),
      status: mapAffiliateStatus(get('status')),
      coupon: get('coupon'),
    });
  }
  rep.affiliates = affiliates.length;
  return {
    kind: 'affiliates',
    tool,
    tool_detected: toolDetected,
    columns: columnsReport(header, col),
    parse: rep,
    affiliates,
    conversions: [],
    warnings: [],
  };
}

function parseConversions(header, body, col, tool, toolDetected) {
  const rep = { rows: 0, conversions: 0, skipped: 0, dup_emails: 0 };
  const warnings = [];
  const conversions = [];
  const currencies = new Set();
  const commissionHeader = col.commission !== undefined ? String(header[col.commission]) : '';
  let lineNo = 1;
  for (const rec of body) {
    lineNo++;
    rep.rows++;
    const get = (name) => {
      const i = col[name];
      if (i === undefined || i >= rec.length) return '';
      return String(rec[i]).trim();
    };
    const affEmail = get('affiliate_email').toLowerCase();
    const affId = get('affiliate_id');
    const affiliate = affEmail.includes('@') ? affEmail : affId;
    if (!affiliate) {
      rep.skipped++;
      continue;
    }
    const custEmailRaw = get('customer_email').toLowerCase();
    const customerEmail = custEmailRaw.includes('@') ? custEmailRaw : '';
    const customerId = get('customer_id');
    const status = get('status').toLowerCase();
    const conv = parseTimestamp(get('converted_at'));
    const click = parseTimestamp(get('clicked_at'));
    if (col.currency !== undefined && get('currency')) currencies.add(get('currency').toUpperCase());
    conversions.push({
      row: lineNo,
      affiliate,
      affiliate_email: affEmail.includes('@') ? affEmail : '',
      // commissions.buyer_email is stored lower(trim()) (commissions.go).
      customer_email: customerEmail,
      // customer_ext_id stand-in: the export's customer id, else the email.
      customer_key: customerId || customerEmail,
      coupon: get('coupon'),
      commission_cents:
        col.commission !== undefined ? parseMoneyCents(get('commission'), commissionHeader) : null,
      ts: conv ? conv.ms : null,
      ts_has_time: conv ? conv.hasTime : false,
      click_ts: click ? click.ms : null,
      click_has_time: click ? click.hasTime : false,
      ip: get('ip').toLowerCase(),
      refunded:
        REFUND_STATUSES.has(status) || (col.refunded !== undefined && truthyFlag(get('refunded'))),
    });
  }
  rep.conversions = conversions.length;
  if (currencies.size > 1) {
    warnings.push(`File mixes currencies (${[...currencies].sort().join(', ')}). Totals add them as-is.`);
  }
  // Affiliates are whatever the conversion rows name. Their status is not in
  // a conversion export: they count as active (as importer.Parse maps an
  // empty status).
  const affiliates = [];
  const seenAff = new Set();
  for (const c of conversions) {
    if (seenAff.has(c.affiliate)) continue;
    seenAff.add(c.affiliate);
    affiliates.push({ email: c.affiliate_email, key: c.affiliate, status: 'active' });
  }
  return {
    kind: 'conversions',
    tool,
    tool_detected: toolDetected,
    columns: columnsReport(header, col),
    parse: rep,
    affiliates,
    conversions,
    warnings,
    currency: currencies.size === 1 ? [...currencies][0] : null,
  };
}

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

const NO_CONVERSIONS =
  'This is an affiliate list. It has no sales, so there is nothing to check here. Export referrals or commissions to run it.';

const STATIC_SKIPS = {
  asn_reputation:
    'Needs each click IP classified as datacenter or residential. Exports have no click stream, and this scan never looks IPs up online.',
  bot_class_gating: 'Needs bot classification of every click (user agents). Exports have no click stream.',
  geo_mismatch: 'Needs the country of each click and each sale. Exports do not carry click country.',
  ip_fallback_anomaly:
    'Needs our own attribution method per sale. Other tools do not record IP-fallback attribution.',
  inactive_affiliate:
    'Needs live, unpaid commissions to hold. An export is history, so there is nothing to hold.',
};

// rulePlan decides which rules can run on this export and why others can't.
function rulePlan(p) {
  const run = [];
  const skipped = [];
  const skip = (rule, reason) => skipped.push({ rule, reason });
  if (p.kind === 'affiliates') {
    for (const r of GO_RULES) {
      if (r === 'duplicate_account') run.push(r);
      else if (STATIC_SKIPS[r]) skip(r, STATIC_SKIPS[r]);
      else skip(r, NO_CONVERSIONS);
    }
    for (const r of SCAN_ONLY_RULES) skip(r, NO_CONVERSIONS);
    return { run, skipped, timed: false };
  }
  const c = p.columns;
  const convs = p.conversions;
  const allTs = convs.length > 0 && convs.every((x) => x.ts !== null);
  const allTime = allTs && convs.every((x) => x.ts_has_time);
  const hasIp = 'ip' in c && convs.some((x) => x.ip);
  const hasCustomerEmail = 'customer_email' in c && convs.some((x) => x.customer_email);
  const hasCustomerKey = convs.some((x) => x.customer_key);
  const hasAffEmail = 'affiliate_email' in c && convs.some((x) => x.affiliate_email);
  const hasCoupon = 'coupon' in c && convs.some((x) => x.coupon);
  const noDates = !('converted_at' in c)
    ? 'No sale date column.'
    : !allTs
      ? 'Some rows have no readable sale date (we read YYYY-MM-DD with optional time).'
      : null;
  const needTime = noDates || (allTime ? null : 'Sale dates have no time of day.');

  for (const r of GO_RULES) {
    let why = null;
    switch (r) {
      case 'self_referral':
        if (!hasAffEmail) why = 'No affiliate email column.';
        else if (!hasCustomerEmail) why = 'No customer email column.';
        break;
      case 'click_velocity':
        why = !hasIp ? 'No IP column.' : needTime;
        break;
      case 'conversion_velocity':
      case 'conversion_time_anomaly':
        why = needTime;
        break;
      case 'coupon_hijack':
        if (!hasCoupon) why = 'No coupon column (or no coupons used).';
        else if (!hasCustomerKey) why = 'No customer email or customer id column.';
        else why = noDates;
        break;
      case 'ip_overlap':
        why = !hasIp ? 'No IP column.' : noDates;
        break;
      case 'duplicate_account':
        if (!hasAffEmail) why = 'No affiliate email column.';
        break;
      case 'payment_fingerprint':
        if (!hasCustomerEmail) why = 'No customer email column.';
        break;
      default:
        why = STATIC_SKIPS[r];
    }
    if (why) skip(r, why);
    else run.push(r);
  }
  const hasRefund = 'status' in c || 'refunded' in c;
  if (hasRefund) run.push('refund_rate');
  else skip('refund_rate', 'No status or refunded column.');
  const hasClick = 'clicked_at' in c && convs.every((x) => x.click_ts !== null && x.click_has_time);
  if (!('clicked_at' in c)) skip('click_to_conversion', 'No click date column.');
  else if (!hasClick) skip('click_to_conversion', 'Click dates are missing or have no time of day.');
  else if (needTime) skip('click_to_conversion', needTime);
  else run.push('click_to_conversion');
  return { run, skipped, timed: allTs };
}

// A sliding window over the replay order: rows enter on add(), and leave
// (oldest first) once `inWindow(row, now)` turns false.
class Window {
  constructor(inWindow, onLeave) {
    this.rows = [];
    this.head = 0;
    this.inWindow = inWindow;
    this.onLeave = onLeave;
  }
  add(row) {
    this.rows.push(row);
  }
  evict(now) {
    while (this.head < this.rows.length && !this.inWindow(this.rows[this.head], now)) {
      this.onLeave(this.rows[this.head]);
      this.head++;
    }
  }
}

const inc = (m, k, d = 1) => {
  const v = (m.get(k) || 0) + d;
  if (v === 0) m.delete(k);
  else m.set(k, v);
  return v;
};

function fmtTs(ms) {
  if (ms === null || ms === undefined) return '';
  return new Date(ms).toISOString().replace('T', ' ').replace(/:\d{2}\.\d{3}Z$/, ' UTC');
}

// replayGoRules evaluates every runnable Go rule at each conversion.
// Returns Map rule -> Map affiliate -> [{row, conv, reason}].
function replayGoRules(p, plan) {
  const runs = new Set(plan.run);
  const hits = new Map();
  const hit = (rule, conv, reason) => {
    if (!hits.has(rule)) hits.set(rule, new Map());
    const m = hits.get(rule);
    if (!m.has(conv.affiliate)) m.set(conv.affiliate, []);
    m.get(conv.affiliate).push({ conv, reason });
  };

  // Replay order: by time (stable), else file order.
  const convs = p.conversions.slice();
  if (plan.timed) convs.sort((a, b) => a.ts - b.ts || a.row - b.row);

  // duplicate_account (rules3.go): other ACTIVE accounts on the same
  // alias-normalized email. The affiliate set is static in an export.
  const affEmail = new Map();
  const activeByNorm = new Map();
  for (const a of p.affiliates) {
    affEmail.set(a.key, a.email);
    if (a.email && a.status === 'active') inc(activeByNorm, normalizeEmail(a.email));
  }
  const dupes = (key) => {
    const e = affEmail.get(key);
    if (!e) return 0;
    const norm = normalizeEmail(e);
    if (!norm) return 0;
    return (activeByNorm.get(norm) || 0) - 1; // every conversion affiliate is active
  };

  // payment_fingerprint (rules3.go): per affiliate, how many of its distinct
  // buyer emails were also bought through another affiliate. Maintained
  // incrementally; no window in Go.
  const buyerAffs = new Map(); // email -> Set(aff)
  const affBuyers = new Map(); // aff -> Set(email)
  const pfShared = new Map(); // aff -> count
  const addBuyer = (aff, email) => {
    if (!email) return;
    if (!affBuyers.has(aff)) affBuyers.set(aff, new Set());
    const mine = affBuyers.get(aff);
    if (mine.has(email)) return;
    mine.add(email);
    if (!buyerAffs.has(email)) buyerAffs.set(email, new Set());
    const owners = buyerAffs.get(email);
    if (owners.size === 1) {
      for (const o of owners) inc(pfShared, o);
      inc(pfShared, aff);
    } else if (owners.size >= 2) {
      inc(pfShared, aff);
    }
    owners.add(aff);
  };

  // coupon_hijack (engine.go): customer binding is first touch (the first
  // conversion seen for that customer). A coupon sale whose customer is
  // bound to a different affiliate overrode that binding.
  const binding = new Map();

  // 1h window: conversion_velocity (per affiliate), click_velocity (per IP).
  const aff1h = new Map();
  const ip1h = new Map();
  const w1h = new Window(
    (r, now) => r.ts > now - GO.HOUR_MS,
    (r) => {
      inc(aff1h, r.affiliate, -1);
      if (r.ip) inc(ip1h, r.ip, -1);
    },
  );
  // 7d window: conversion_time_anomaly, coupon_hijack.
  const affNight = new Map();
  const affTotal7 = new Map();
  let progNight = 0;
  let progTotal = 0;
  const couponHijacked = new Map();
  const isNight = (r) => new Date(r.ts).getUTCHours() < GO.TIME_ANOMALY_NIGHT_END_HOUR;
  const w7d = new Window(
    (r, now) => r.ts > now - 7 * GO.DAY_MS,
    (r) => {
      inc(affTotal7, r.affiliate, -1);
      progTotal--;
      if (isNight(r)) {
        inc(affNight, r.affiliate, -1);
        progNight--;
      }
      if (r._hijack) inc(couponHijacked, r.coupon, -1);
    },
  );
  // 30d windows for ip_overlap: IPs by DAY (clicks.day >= dayInt(now-30d)),
  // conversions by timestamp (enoughConversions: amount > 0, created_at > since).
  const ipAffs = new Map(); // ip -> Map(aff -> n)
  const affIps = new Map(); // aff -> Map(ip -> n)
  const ipShared = new Map(); // aff -> distinct IPs it shares with another affiliate
  const addIp = (aff, ip) => {
    if (!ipAffs.has(ip)) ipAffs.set(ip, new Map());
    const owners = ipAffs.get(ip);
    if (!owners.has(aff)) {
      if (owners.size === 1) {
        for (const o of owners.keys()) inc(ipShared, o);
        inc(ipShared, aff);
      } else if (owners.size >= 2) {
        inc(ipShared, aff);
      }
    }
    inc(owners, aff);
    if (!affIps.has(aff)) affIps.set(aff, new Map());
    inc(affIps.get(aff), ip);
  };
  const removeIp = (aff, ip) => {
    const owners = ipAffs.get(ip);
    if (inc(owners, aff, -1) === 0) {
      if (owners.size === 1) {
        for (const o of owners.keys()) inc(ipShared, o, -1);
        inc(ipShared, aff, -1);
      } else if (owners.size >= 2) {
        inc(ipShared, aff, -1);
      }
      if (owners.size === 0) ipAffs.delete(ip);
    }
    inc(affIps.get(aff), ip, -1);
  };
  const wDay = new Window(
    (r, now) => dayInt(r.ts) >= dayInt(now - 30 * GO.DAY_MS),
    (r) => {
      if (r.ip) removeIp(r.affiliate, r.ip);
    },
  );
  const aff30 = new Map();
  const positive = (r) => r.commission_cents === null || r.commission_cents >= 0;
  const w30 = new Window(
    (r, now) => r.ts > now - 30 * GO.DAY_MS,
    (r) => {
      if (positive(r)) inc(aff30, r.affiliate, -1);
    },
  );

  for (const src of convs) {
    const r = { ...src };
    const now = r.ts;

    // Insert the conversion (the commission row + its click, in Go terms).
    addBuyer(r.affiliate, r.customer_email);
    if (r.customer_key && !binding.has(r.customer_key)) binding.set(r.customer_key, r.affiliate);
    r._hijack =
      r.coupon !== '' && r.customer_key !== '' && binding.get(r.customer_key) !== r.affiliate;
    if (plan.timed) {
      inc(aff1h, r.affiliate);
      if (r.ip) inc(ip1h, r.ip);
      w1h.add(r);
      inc(affTotal7, r.affiliate);
      progTotal++;
      if (isNight(r)) {
        inc(affNight, r.affiliate);
        progNight++;
      }
      if (r._hijack) inc(couponHijacked, r.coupon);
      w7d.add(r);
      if (r.ip) addIp(r.affiliate, r.ip);
      wDay.add(r);
      if (positive(r)) inc(aff30, r.affiliate);
      w30.add(r);
      w1h.evict(now);
      w7d.evict(now);
      wDay.evict(now);
      w30.evict(now);
    }

    // Evaluate (engine.go Engine.Evaluate runs every rule).
    if (runs.has('self_referral')) {
      // engine.go SelfReferral
      const ae = affEmail.get(r.affiliate);
      if (ae && r.customer_email && normalizeEmail(ae) === normalizeEmail(r.customer_email)) {
        hit('self_referral', r, `buyer ${r.customer_email} matches affiliate email`);
      }
    }
    if (runs.has('click_velocity') && r.ip) {
      // engine.go ClickVelocity (sale IPs stand in for click IPs)
      const n = ip1h.get(r.ip) || 0;
      if (n >= GO.CLICK_VELOCITY_PER_IP_HOUR) hit('click_velocity', r, `${n} sales from IP ${r.ip} in the last hour`);
    }
    if (runs.has('conversion_velocity')) {
      // engine.go ConversionVelocity
      const n = aff1h.get(r.affiliate) || 0;
      if (n >= GO.CONVERSION_VELOCITY_PER_HOUR) hit('conversion_velocity', r, `${n} sales in one hour`);
    }
    if (runs.has('coupon_hijack') && r.coupon) {
      // engine.go CouponHijackPattern
      const n = couponHijacked.get(r.coupon) || 0;
      if (n >= GO.COUPON_HIJACK_THRESHOLD) {
        hit('coupon_hijack', r, `coupon ${r.coupon} took ${n} customers already tied to another affiliate in 7 days`);
      }
    }
    if (runs.has('conversion_time_anomaly')) {
      // rules2.go ConversionTimeAnomaly
      const total = affTotal7.get(r.affiliate) || 0;
      const night = affNight.get(r.affiliate) || 0;
      if (total >= GO.TIME_ANOMALY_MIN_SAMPLE && progTotal >= GO.TIME_ANOMALY_MIN_SAMPLE) {
        const nightShare = night / total;
        const pShare = progNight / progTotal;
        const se = Math.sqrt((pShare * (1 - pShare)) / total + GO.EPSILON);
        const z = (nightShare - pShare) / se;
        if (z > GO.TIME_ANOMALY_Z && nightShare > GO.TIME_ANOMALY_MIN_NIGHT_SHARE) {
          hit(
            'conversion_time_anomaly',
            r,
            `${Math.round(nightShare * 100)}% of ${total} sales at 00:00-05:59 UTC vs ${Math.round(pShare * 100)}% for the program (z=${z.toFixed(1)})`,
          );
        }
      }
    }
    if (runs.has('ip_overlap')) {
      // rules3.go IPOverlap
      const shared = ipShared.get(r.affiliate) || 0;
      if (shared >= GO.IP_OVERLAP_MIN_SHARED && (aff30.get(r.affiliate) || 0) >= GO.MIN_CLICK_RULE_CONVERSIONS) {
        hit('ip_overlap', r, `shares ${shared} IPs with other affiliates in 30 days`);
      }
    }
    if (runs.has('duplicate_account')) {
      // rules3.go DuplicateAccount
      const n = dupes(r.affiliate);
      if (n > 0) hit('duplicate_account', r, `same email (alias-normalized) as ${n} other affiliate account(s)`);
    }
    if (runs.has('payment_fingerprint')) {
      // rules3.go PaymentFingerprint
      const n = pfShared.get(r.affiliate) || 0;
      if (n >= GO.PAYMENT_FINGERPRINT_MIN_SHARED) {
        hit('payment_fingerprint', r, `${n} of its buyers also bought through other affiliates`);
      }
    }
  }
  return hits;
}

// duplicate_account over an affiliate list: one finding per account the
// rule would flag (there are no conversions to hold).
function duplicateAccountsInList(p) {
  const activeByNorm = new Map();
  for (const a of p.affiliates) if (a.status === 'active') inc(activeByNorm, normalizeEmail(a.email));
  const out = [];
  for (const a of p.affiliates) {
    const norm = normalizeEmail(a.email);
    const others = (activeByNorm.get(norm) || 0) - (a.status === 'active' ? 1 : 0);
    if (norm && others > 0) {
      out.push({
        rule: 'duplicate_account',
        affiliate: a.email,
        unit: 'accounts',
        count: 1,
        amount_cents: null,
        examples: [`row ${a.row}: ${a.email} normalizes to ${norm}, used by ${others} other active account(s)`],
        scan_only: false,
      });
    }
  }
  return out;
}

function scanOnlyChecks(p, plan, opts) {
  const out = [];
  const runs = new Set(plan.run);
  if (runs.has('refund_rate')) {
    const per = new Map();
    for (const c of p.conversions) {
      if (!per.has(c.affiliate)) per.set(c.affiliate, { total: 0, refunded: [] });
      const s = per.get(c.affiliate);
      s.total++;
      if (c.refunded) s.refunded.push(c);
    }
    for (const [aff, s] of per) {
      const share = s.refunded.length / s.total;
      if (s.total >= opts.refundRateMinConversions && share >= opts.refundRateMinShare) {
        out.push({
          rule: 'refund_rate',
          affiliate: aff,
          rows: s.refunded,
          reason: `${s.refunded.length} of ${s.total} sales refunded or charged back (${Math.round(share * 100)}%)`,
        });
      }
    }
  }
  if (runs.has('click_to_conversion')) {
    const per = new Map();
    for (const c of p.conversions) {
      const gap = (c.ts - c.click_ts) / 1000;
      if (gap >= 0 && gap < opts.fastConversionSeconds) {
        if (!per.has(c.affiliate)) per.set(c.affiliate, []);
        per.get(c.affiliate).push(c);
      }
    }
    for (const [aff, rows] of per) {
      if (rows.length >= opts.fastConversionMinCount) {
        out.push({
          rule: 'click_to_conversion',
          affiliate: aff,
          rows,
          reason: `${rows.length} sales under ${opts.fastConversionSeconds}s after the click`,
        });
      }
    }
  }
  return out;
}

function sumCents(rows, known) {
  if (!known) return null;
  let s = 0;
  for (const r of rows) s += r.commission_cents || 0;
  return s;
}

// scan: the one call the page needs.
//   text: CSV file contents
//   opts.tool: 'rewardful' | 'firstpromoter' | 'tapfiliate' | 'tolt' (optional)
//   opts.maxExamples (default 3), plus SCAN_DEFAULTS overrides.
export function scan(text, opts = {}) {
  const o = { ...SCAN_DEFAULTS, maxExamples: 3, ...opts };
  const p = parseExport(text, o);
  const plan = rulePlan(p);
  const commissionKnown = p.kind === 'conversions' && 'commission' in p.columns;
  const findings = [];

  if (p.kind === 'affiliates') {
    findings.push(...duplicateAccountsInList(p));
  } else {
    const hits = replayGoRules(p, plan);
    for (const rule of GO_RULES) {
      const m = hits.get(rule);
      if (!m) continue;
      for (const [aff, list] of m) {
        const rows = list.map((h) => h.conv);
        findings.push({
          rule,
          affiliate: aff,
          unit: 'conversions',
          count: list.length,
          amount_cents: sumCents(rows, commissionKnown),
          examples: list
            .slice(0, o.maxExamples)
            .map((h) => `row ${h.conv.row}${h.conv.ts !== null ? ', ' + fmtTs(h.conv.ts) : ''}: ${h.reason}`),
          scan_only: false,
          _rows: rows,
        });
      }
    }
    for (const f of scanOnlyChecks(p, plan, o)) {
      findings.push({
        rule: f.rule,
        affiliate: f.affiliate,
        unit: 'conversions',
        count: f.rows.length,
        amount_cents: sumCents(f.rows, commissionKnown),
        examples: [f.reason, ...f.rows.slice(0, o.maxExamples - 1).map((r) => `row ${r.row}`)],
        scan_only: true,
        _rows: f.rows,
      });
    }
  }

  // Summary: each flagged conversion counts once even if several rules hit it.
  const flaggedRows = new Map();
  const flaggedAffs = new Set();
  for (const f of findings) {
    flaggedAffs.add(f.affiliate);
    for (const r of f._rows || []) flaggedRows.set(r.row, r);
    delete f._rows;
  }
  const warnings = [...p.warnings];
  if (p.kind === 'conversions' && !commissionKnown) {
    warnings.push('No commission column, so money at risk could not be added up.');
  }
  return {
    tool: p.tool,
    tool_detected: p.tool_detected,
    export_kind: p.kind,
    rows: p.parse.rows,
    parse: p.parse,
    columns: p.columns,
    currency: p.currency || null,
    rules_run: plan.run,
    rules_skipped: plan.skipped,
    findings,
    summary: {
      commission_at_risk_cents: commissionKnown ? sumCents([...flaggedRows.values()], true) : null,
      conversions_flagged: flaggedRows.size,
      affiliates_flagged: flaggedAffs.size,
    },
    warnings,
  };
}

// normalizedForParity exposes exactly what parity_test.go replays in Go.
export function normalizedForParity(text, opts = {}) {
  const p = parseExport(text, opts);
  const plan = rulePlan(p);
  const conversions = p.conversions.slice();
  if (plan.timed) conversions.sort((a, b) => a.ts - b.ts || a.row - b.row);
  return {
    kind: p.kind,
    parse: p.parse,
    affiliates: p.affiliates,
    conversions: conversions.map((c) => ({
      row: c.row,
      affiliate: c.affiliate,
      affiliate_email: c.affiliate_email,
      customer_email: c.customer_email,
      customer_key: c.customer_key,
      coupon: c.coupon,
      commission_cents: c.commission_cents,
      ts: c.ts === null ? null : new Date(c.ts).toISOString().replace('.000Z', 'Z'),
      ip: c.ip,
    })),
  };
}
