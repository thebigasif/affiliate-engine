"use strict";
// Admin console + partner portal (FR-27/28). Served as a same-origin file so
// the CSP needs no script 'unsafe-inline'. Loaded at the end of <body>: the
// DOM it binds to already exists. Every money number shown here comes from
// the server, which derives it from the double-entry ledger.
const $ = (id) => document.getElementById(id);
const state = { view: "", role: "", setup: null, affs: [], emailOf: {}, cur: "USD" }; // cur: the program currency
let brand = null; // program 1 branding, read by the Brand settings and Get started

// ---------------------------------------------------------------- format
function esc(s){ return String(s==null?"":s).replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c])); }
// Minor units per major unit by ISO 4217 exponent (internal/money): yen has
// none, dinar three, most two.
function curExp(c){
  if(/^(BIF|CLP|DJF|GNF|ISK|JPY|KMF|KRW|PYG|RWF|UGX|UYI|VND|VUV|XAF|XOF|XPF)$/.test(c)) return 0;
  if(/^(BHD|IQD|JOD|KWD|LYD|OMR|TND)$/.test(c)) return 3;
  if(/^(CLF|UYW)$/.test(c)) return 4;
  return 2;
}
function money(cents, cur){
  const c = String(cur||state.cur||"USD").toUpperCase(), d = curExp(c);
  const n = (Math.abs(cents||0)/Math.pow(10,d)).toLocaleString("en-US",{minimumFractionDigits:d,maximumFractionDigits:d});
  const sign = cents<0 ? "-" : "";
  if(c==="USD") return sign+"$"+n;
  if(c==="EUR") return sign+"€"+n;
  if(c==="GBP") return sign+"£"+n;
  return sign+n+" "+esc(c);
}
// Money in several currencies is listed, never added up:
// [{currency, amount_minor}] -> "$12.50 + 5,000 JPY". cur formats an
// empty list (default: the program currency).
function moneyList(list, cur){
  const xs = (list||[]).filter(x=>x.amount_minor);
  return xs.length ? xs.map(x=>money(x.amount_minor, x.currency)).join(" + ") : money(0, cur||state.cur);
}
// One commission status across a partner's currencies
// ([{currency, by_status_cents}]).
function moneyStatus(curs, status){
  return moneyList((curs||[]).map(c=>({currency:c.currency, amount_minor:(c.by_status_cents||{})[status]||0})));
}
// Major units typed by a person <-> minor units, by the currency's exponent.
function toMinor(v, cur){ return Math.round(parseFloat(v)*Math.pow(10, curExp(String(cur||state.cur||"USD").toUpperCase()))); }
function toMajor(minor, cur){ const d = curExp(String(cur||state.cur||"USD").toUpperCase()); return (minor/Math.pow(10,d)).toFixed(d); }
// when/day format a stored timestamp for people: "29 Sep 2026, 05:19" (UTC).
// Accepts RFC 3339, "YYYY-MM-DD HH:MM:SS" and Go's time.String() form; all UTC.
function parseTime(v){
  const m = /^(\d{4})-(\d\d)-(\d\d)(?:[ T](\d\d):(\d\d)(?::(\d\d))?)?/.exec(String(v));
  if(!m) return new Date(NaN);
  return new Date(Date.UTC(+m[1], +m[2]-1, +m[3], +(m[4]||0), +(m[5]||0), +(m[6]||0)));
}
const MONTHS = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
function when(v){
  if(!v) return "";
  const d = parseTime(v); if(isNaN(d)) return esc(v);
  const p2 = n=>String(n).padStart(2,"0");
  return `${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]} ${d.getUTCFullYear()}, ${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}`;
}
function day(v){
  if(!v) return "";
  const d = parseTime(v); if(isNaN(d)) return esc(v);
  return `${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]} ${d.getUTCFullYear()}`;
}
// ago: "5 minutes ago", "3 days ago".
function ago(v){
  if(!v) return "";
  const d = parseTime(v); if(isNaN(d)) return "";
  const s = Math.max(0, (Date.now()-d.getTime())/1000);
  if(s < 90) return "just now";
  const m = Math.round(s/60); if(m < 60) return `${m} minutes ago`;
  const h = Math.round(m/60); if(h < 36) return `${h} ${h===1?"hour":"hours"} ago`;
  const dd = Math.round(h/24); return `${dd} ${dd===1?"day":"days"} ago`;
}
function num(n){ return typeof n==="number" ? n.toLocaleString("en-US") : esc(n); }
function plural(n, one, many){ return n===1 ? one : many; }
function badge(cls, label){ return `<span class="badge ${esc(cls)}">${esc(label||cls)}</span>`; }
// Phone tables show each row as a card: every cell carries its column's
// header as a label (the CSS shows it only on narrow screens).
function labelCells(tbody){
  const heads = [...tbody.closest("table").querySelectorAll("thead th")].map(th=>th.textContent.trim());
  tbody.querySelectorAll("tr").forEach(tr=>{
    let i = 0;
    for(const td of tr.children){
      const h = heads[i]; i += td.colSpan || 1;
      if(h && td.colSpan===1) td.dataset.label = h; else delete td.dataset.label;
    }
  });
}
// Tables that scroll must be reachable by keyboard, and named for screen
// readers after their panel's heading.
// Code boxes scroll sideways on phones: keyboard users can reach them too.
document.querySelectorAll(".snip code").forEach(c=>{ c.tabIndex = 0; });
document.querySelectorAll(".tbl").forEach(t=>{
  t.tabIndex = 0;
  t.setAttribute("role", "region");
  const h = t.closest(".panel") && t.closest(".panel").querySelector("h2");
  t.setAttribute("aria-label", h ? h.textContent.trim() : "Table");
});
document.querySelectorAll(".tbl tbody").forEach(tb=>{
  labelCells(tb);
  new MutationObserver(()=>labelCells(tb)).observe(tb, {childList:true});
});
function none(cols, text){ return `<tr><td colspan="${cols}" class="none">${text}</td></tr>`; }
function card(label, value, cls){ if(typeof value==="number") value = value.toLocaleString("en-US"); return `<div class="card"><div class="label">${label}</div><div class="value ${cls||""}">${value}</div></div>`; }

// The 12 fraud rules by their names on affiliatetracking.co/fraud.
const RULES = {
  self_referral:"Self-referral", duplicate_account:"Duplicate accounts", payment_fingerprint:"Shared buyers",
  coupon_hijack:"Coupon hijack", click_velocity:"Click velocity", asn_reputation:"Datacenter traffic",
  bot_class_gating:"Bot traffic", ip_overlap:"IP overlap", conversion_velocity:"Conversion velocity",
  conversion_time_anomaly:"Odd conversion hours", geo_mismatch:"Geo mismatch", ip_fallback_anomaly:"IP fallback anomalies",
  inactive_affiliate:"Inactive partner"
};
function ruleName(code){ return RULES[code] || String(code||"").replace(/_/g," "); }
// Each rule's page on affiliatetracking.co/fraud explains what it checks.
const RULE_PAGE = {
  self_referral:"self-referral", duplicate_account:"duplicate-accounts", payment_fingerprint:"shared-buyers",
  coupon_hijack:"coupon-hijack", click_velocity:"click-velocity", asn_reputation:"datacenter-traffic",
  bot_class_gating:"bot-traffic", ip_overlap:"ip-overlap", conversion_velocity:"conversion-velocity",
  conversion_time_anomaly:"conversion-time-anomaly", geo_mismatch:"geo-mismatch", ip_fallback_anomaly:"ip-fallback-anomaly",
};
function ruleHelp(code){
  const page = RULE_PAGE[code];
  return page ? `<a class="sub rule-help" href="https://affiliatetracking.co/fraud/${page}" target="_blank" rel="noopener">How this rule works ↗</a>` : "";
}
// Verdict subjects are an affiliate id, "affiliate:<id>", "ip:<hash>" or a coupon code.
function subjectText(s){
  s = String(s||"");
  const m = /^(?:affiliate:)?(\d+)$/.exec(s);
  if(m) return state.emailOf[m[1]] ? esc(state.emailOf[m[1]]) : `Partner #${m[1]}`;
  if(s.indexOf("ip:")===0) return `A shared IP address<span class="sub mono">${esc(s.slice(3,15))}…</span>`;
  return `Coupon <b>${esc(s)}</b>`;
}
// Where a partner asked to be paid, or that they haven't said yet (and
// when we emailed them to).
function payToText(pay_to, asked){
  if(pay_to) return esc(pay_to);
  return badge("pending","Not given yet") + `<span class="sub">${asked ? `We emailed them to add it on ${day(asked)}.` : "They add it in their partner portal."}</span>`;
}
function partnerCell(id, email, name){
  const e = email || state.emailOf[id] || `Partner #${id}`;
  return `${esc(e)}${name?`<span class="sub">${esc(name)}</span>`:""}`;
}
const PARTNER = { active:["active","Active"], frozen:["frozen","Paused"], removed:["cancelled","Removed"] };
function statusBadge(s){ const x = PARTNER[s] || [s, s]; return badge(x[0], x[1]); }
const COMMISSION = { pending:["pending","In hold"], approved:["approved","Ready to pay"], paid:["paid","Paid"], cancelled:["cancelled","Cancelled"], clawback:["clawback","Taken back"], held:["hold","Held for review"] };
function commissionBadge(s){ const x = COMMISSION[s] || [s, s]; return badge(x[0], x[1]); }
const LEDGER = { maturation:"Commission ready to pay", payout:"Paid out to you", clawback:"Taken back (refund or fraud)", refund:"Refund", adjustment:"Adjustment", conversion:"Commission earned", usage:"Usage commission", mlm:"Team commission" };
const METHODS = { paypal:"PayPal", wise:"Wise", manual:"Bank transfer or other", stripe_connect:"Stripe" };

// ---------------------------------------------------------------- notices
function msg(text, cls){ const m = $("msg"); m.textContent = text; m.className = cls; clearTimeout(msg.t); msg.t = setTimeout(()=>{ m.className = ""; }, cls==="err" ? 9000 : 5000); }
function errText(e){ const t = (e && e.message) || String(e); return t.charAt(0).toUpperCase()+t.slice(1)+(/[.!?]$/.test(t)?"":"."); }
function copy(text, what){
  navigator.clipboard.writeText(text).then(()=>msg((what||"Copied")+".","ok"), ()=>msg("Couldn't copy. Select the text and copy it by hand.","err"));
}
document.addEventListener("click", e=>{
  const b = e.target.closest("[data-copy]"); if(!b) return;
  const src = $(b.dataset.copy); if(src) copy(src.textContent.trim(), b.dataset.what || "Copied");
});
// ask: a small confirm dialog (optionally with a note) instead of the
// browser's confirm() and prompt(). Resolves {ok, note}.
function ask({title, body, ok, danger, note}){
  const d = $("dlg");
  $("dlg-title").textContent = title; $("dlg-body").textContent = body||"";
  $("dlg-ok").textContent = ok||"OK"; $("dlg-ok").classList.toggle("danger", !!danger);
  const n = $("dlg-note"); n.hidden = !note; n.value = ""; n.placeholder = note||"";
  return new Promise(res=>{
    d.addEventListener("close", ()=>res({ok: d.returnValue==="ok", note: n.value.trim()}), {once:true});
    d.returnValue = ""; d.showModal();
  });
}

// ---------------------------------------------------------------- API
function key(){ return $("key").value.trim(); }
// Live demo: the demo key is an admin, which reads partner views for an
// explicit ?affiliate_id= (the partner the server picked for the demo).
function withDemoAff(path){
  const d = window.AE_DEMO;
  if(!d || !d.affiliate_id || path.indexOf("/api/v1/affiliate/")!==0 || /[?&]affiliate_id=/.test(path)) return path;
  return path + (path.indexOf("?")<0?"?":"&") + "affiliate_id=" + d.affiliate_id;
}
async function api(path, opts={}){
  // opts first: headers are merged after it, so Authorization is never
  // dropped by a call that sets Content-Type.
  const json = opts.body && !(opts.body instanceof FormData) ? {"Content-Type":"application/json"} : {};
  const r = await fetch(withDemoAff(path), {...opts, headers: Object.assign({"Authorization":"Bearer "+key()}, json, opts.headers||{})});
  let body = null;
  try { body = await r.json(); } catch(e) {}
  if(!r.ok) throw new Error((body&&body.error)||("HTTP "+r.status));
  return body;
}
function post(path, obj){ return api(path, {method:"POST", body: JSON.stringify(obj)}); }
// Authenticated download: the key rides in the Authorization header, so a
// plain link cannot fetch it. Fetch, then save the blob.
async function downloadAuthed(path, filename){
  try{
    const r = await fetch(path, {headers: {"Authorization":"Bearer "+key()}});
    if(!r.ok){ let m = "HTTP "+r.status; try{ m = (await r.json()).error||m; }catch(e){} throw new Error(m); }
    const url = URL.createObjectURL(await r.blob());
    const a = document.createElement("a");
    a.href = url; a.download = filename; document.body.appendChild(a); a.click(); a.remove();
    setTimeout(()=>URL.revokeObjectURL(url), 1000);
  }catch(e){ msg("Download failed: "+errText(e),"err"); }
}

// ---------------------------------------------------------------- session + pages
// The key lives in the hidden #key input and in sessionStorage, so a reload
// keeps you signed in until the tab closes. Admin keys (ae_) open the admin
// pages; partner sessions (aps_) and partner-scoped keys open the portal.
const ADMIN_HOME = "overview", PORTAL_HOME = "home";
function store(k){ try{ k ? sessionStorage.setItem("ae_key", k) : sessionStorage.removeItem("ae_key"); }catch(e){} }
function stored(){ try{ return sessionStorage.getItem("ae_key")||""; }catch(e){ return ""; } }
function setRole(role){
  state.role = role;
  document.body.classList.toggle("role-admin", role==="admin");
  document.body.classList.toggle("role-partner", role==="partner");
  $("role-label").textContent = role==="admin" ? "Admin" : "Partner portal";
  $("who").textContent = role==="admin" ? "Signed in as admin" : "Signed in as partner";
}
async function signIn(k, role){
  $("key").value = k; store(k);
  if(!role){
    if(k.indexOf("aps_")===0) role = "partner";
    else { try{ await api("/api/v1/admin/dashboard"); role = "admin"; }catch(e){ role = "partner"; } }
  }
  setRole(role);
  const want = pageFromHash();
  // A link to a page opens it after sign-in when it's the role's own kind
  // of page; the demo shows both kinds, so any page opens there.
  const fits = want && (groupOf(want)===(role==="admin"?"admin":"portal") || (state.demo && groupOf(want)));
  go(fits ? want : (role==="admin"?ADMIN_HOME:PORTAL_HOME), true);
}
function signOut(){
  if(key().indexOf("aps_")===0) fetch("/api/v1/portal/logout",{method:"POST"}).catch(()=>{});
  $("key").value = ""; store("");
  state.role = ""; state.view = "";
  document.body.classList.remove("role-admin","role-partner");
  history.replaceState(null, "", location.pathname + location.search);
  document.title = brandName();
  refresh();
}
function brandName(){ const b = document.querySelector(".side .brand-name"); return b ? b.textContent.trim() : "AffiliateTracking"; }
function pageFromHash(){ const m = /^#\/([a-z]+)$/.exec(location.hash||""); return m ? m[1] : ""; }
function groupOf(page){ const el = document.querySelector('.page[data-page="'+page+'"]'); return el ? el.dataset.group : ""; }
// go shows one page, marks it in the sidebar and loads its data.
function go(page, load){
  const el = document.querySelector('.page[data-page="'+page+'"]');
  if(!el) return;
  document.querySelectorAll(".page").forEach(p=>p.classList.toggle("active", p===el));
  document.querySelectorAll(".nav a[data-page]").forEach(a=>{ if(a.dataset.page===page) a.setAttribute("aria-current","page"); else a.removeAttribute("aria-current"); });
  if(location.hash !== "#/"+page) history.replaceState(null, "", "#/"+page);
  document.body.classList.remove("nav-open"); $("menu").setAttribute("aria-expanded","false");
  const h = el.querySelector("h1"); document.title = (h ? h.textContent+" · " : "") + brandName();
  const was = state.view;
  state.view = el.dataset.group;
  window.scrollTo(0,0);
  if(load || was !== state.view) refresh();
}
window.addEventListener("hashchange", ()=>{ const p = pageFromHash(); if(p && key() && state.role) go(p); });
document.addEventListener("click", e=>{
  const a = e.target.closest('a[href^="#/"]'); if(!a || !state.role) return;
  e.preventDefault(); go(a.getAttribute("href").slice(2));
  const f = a.dataset.focus && $(a.dataset.focus);
  if(f) setTimeout(()=>{ f.scrollIntoView({block:"center"}); f.focus(); }, 50);
});
$("menu").addEventListener("click", ()=>{ const open = document.body.classList.toggle("nav-open"); $("menu").setAttribute("aria-expanded", String(open)); });
$("scrim").addEventListener("click", ()=>document.body.classList.remove("nav-open"));
document.querySelectorAll(".origin").forEach(e=>{ e.textContent = location.origin; });
$("snip-script").textContent = `<script src="${location.origin}/t.js" async></script>`;
$("snip-apply").textContent = location.origin + "/apply";
$("key").addEventListener("change", refresh);

// termsText: a program's public offer (portal/brand "terms") as sentences.
function termsText(t){
  if(!t) return null;
  const rate = t.rate_type==="percent" ? `${(t.rate_value/100).toString().replace(/\.0+$/,"")}% of every payment` : `${money(t.rate_value, t.currency)} for every payment`;
  return [
    `${rate} from customers you send${t.commission_months ? `, for their first ${t.commission_months} ${plural(t.commission_months,"month","months")}` : ", for as long as they stay"}`,
    `A ${t.cookie_days}-day cookie: you're credited if they buy within ${t.cookie_days} days of clicking`,
    `Paid once you've earned ${money(t.min_payout_cents, t.currency)}`
  ];
}

// ---------------------------------------------------------------- public: /apply, sign-in links, sign in
if(location.pathname.replace(/\/+$/,"")==="/apply"){
  document.body.classList.add("apply-mode");
  document.body.classList.remove("signed-out");
  // The program's offer, so applicants know what they'd earn.
  fetch("/api/v1/portal/brand").then(r=>r.json()).then(b=>{
    const items = termsText(b && b.terms); if(!items) return;
    $("ap-terms").innerHTML = items.map(x=>`<li>${esc(x)}</li>`).join("");
    $("ap-terms").hidden = false;
  }).catch(()=>{});
  $("apply-form").addEventListener("submit", async e=>{
    e.preventDefault();
    const body = {name:$("ap-name").value.trim(), email:$("ap-email").value.trim(), note:$("ap-note").value.trim()};
    try{
      const r = await fetch("/api/v1/portal/apply",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(body)});
      if(!r.ok){ let m="Something went wrong. Try again in a minute."; try{ const j=await r.json(); if(r.status===400&&j.error) m="Please enter a valid email."; }catch(_){} msg(m,"err"); return; }
      $("apply-form-wrap").style.display="none"; $("apply-done").style.display="block";
    }catch(_){ msg("Couldn't send. Check your connection and try again.","err"); }
  });
}

// One-time sign-in link for a partner: /#grant=aps_... becomes a portal
// session. The grant is wiped from the address bar first.
(function(){
  const m = /(?:^#|&)grant=(aps_[A-Za-z0-9_-]{8,200})(?:&|$)/.exec(location.hash||"");
  if(!m) return;
  if(/(?:^#|&)to=payto(?:&|$)/.test(location.hash)) state.toPayTo = true; // from the "where should we send your money?" email
  history.replaceState(null, "", location.pathname + location.search);
  fetch("/api/v1/portal/login",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({grant:m[1]})})
    .then(r=>r.json().then(j=>({ok:r.ok,j})))
    .then(({ok,j})=>{
      if(!ok||!j.session){ msg("This sign-in link has expired or was already used. Ask for a new one below.","err"); return; }
      signIn(j.session, "partner");
    })
    .catch(()=>msg("Couldn't sign you in. Try the link again.","err"));
})();

// "Email me a link": the same answer whether or not the address is a
// partner's (the server never says which).
$("linkmail-form").addEventListener("submit", async e=>{
  e.preventDefault();
  const email = $("linkmail-email").value.trim();
  try{
    const r = await fetch("/api/v1/portal/login-link",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({email})});
    let body = {}; try{ body = await r.json(); }catch(err){}
    if(!r.ok) throw new Error(body.error||("HTTP "+r.status));
    $("linkmail-email").value = "";
    msg("If "+email+" belongs to a partner, a sign-in link is on its way. Check your inbox.","ok");
  }catch(err){ msg(errText(err),"err"); }
});

function authUI(){ document.body.classList.toggle("signed-out", !key()); }
$("signin-form").addEventListener("submit", e=>{
  e.preventDefault();
  const k = $("signin-key").value.trim();
  if(!/^(ae|aps)_[A-Za-z0-9_-]{8,200}$/.test(k)){ msg("That doesn't look like a key. Admin keys start with ae_.","err"); return; }
  $("signin-key").value = "";
  signIn(k);
});
$("signout").addEventListener("click", signOut);

// Cloud hand-off: the control plane's "Open dashboard" lands here with the
// admin key in the URL fragment (never sent to any server). Take it, wipe
// it from the address bar and history, and open the admin pages. Otherwise
// pick up a key this tab already signed in with.
(function(){
  if(document.body.classList.contains("apply-mode")) return;
  const m = /(?:^#|&)key=(ae_[A-Za-z0-9_-]{8,200})(?:&|$)/.exec(location.hash||"");
  if(m){ history.replaceState(null, "", location.pathname + location.search); signIn(m[1], "admin"); return; }
  if(/grant=/.test(location.hash||"")) return;
  const k = stored();
  if(k && !window.AE_DEMO) signIn(k);
})();

// ---------------------------------------------------------------- attention
// Counts from each loader, shown as sidebar badges and the Overview list.
const attention = {};
function setCount(k, n){ attention[k] = n; renderAttention(); }
function renderAttention(){
  const a = attention, dot = (id, n)=>{ const el = $(id); if(el) el.textContent = n>0 ? String(n) : ""; };
  dot("n-apps", a.apps); dot("n-fraud", a.holds); dot("n-payouts", a.payouts); dot("n-tax", a.tax); dot("n-settings", (a.norate?1:0)+(a.billingFailing||[]).length);
  const items = [];
  (a.billingFailing||[]).forEach(h=>items.push(["red", `${h.name} sales events are being rejected (last ${ago(h.w.last_err_at)}). Sales may not be credited until you fix it.`, "settings", "Fix"]));
  if(a.apps>0) items.push(["", `${a.apps} ${plural(a.apps,"person wants","people want")} to become a partner.`, "applications", "Review"]);
  if(a.holds>0) items.push(["red", `${a.holds} ${plural(a.holds,"commission is","commissions are")} held by fraud checks and ${plural(a.holds,"needs","need")} your decision.`, "fraud", "Decide"]);
  if(a.payouts>0) items.push(["", `${a.payouts} ${plural(a.payouts,"payout is","payouts are")} waiting for you to pay and mark as paid.`, "payouts", "Pay"]);
  if(a.tax>0) items.push(["", `${a.tax} tax ${plural(a.tax,"form needs","forms need")} your review.`, "taxforms", "Review"]);
  $("todo").innerHTML = items.map(([cls, text, page, cta])=>`<li class="${cls}"><i></i><span>${esc(text)}</span><a href="#/${page}">${cta} →</a></li>`).join("");
  $("allclear").hidden = items.length>0;
}
function books(ok){
  $("books").className = "books " + (ok ? "ok" : "bad");
  $("f-integrity").textContent = ok ? "Books balance (Σ = 0)" : "Books don't balance. Contact support.";
}

// ---------------------------------------------------------------- admin: load
async function loadAdmin(){
  const safe = (p)=>p.catch(()=>null);
  const [d, affs, fraud, apps, payouts, tax, settings, camps, rules, imports, tiers] = await Promise.all([
    safe(api("/api/v1/admin/dashboard")), safe(api("/api/v1/admin/affiliates")), safe(api("/api/v1/admin/fraud/queue")),
    safe(api("/api/v1/admin/portal/applications")), safe(api("/api/v1/admin/payouts?status=requested")), safe(api("/api/v1/admin/taxdocs")),
    safe(api("/api/v1/admin/settings")), safe(api("/api/v1/admin/campaigns")), safe(api("/api/v1/admin/rules")), safe(api("/api/v1/admin/import/batches")),
    safe(api("/api/v1/admin/tiers?campaign_id=1"))
  ]);
  if(!d){ msg("Couldn't load your program. Check your key, or try again.","err"); return; }
  state.affs = (affs&&affs.affiliates)||[];
  state.affs.forEach(x=>{ state.emailOf[x.id] = x.email; });
  state.setup = settings && settings.setup;
  state.cur = (settings && settings.currency) || d.currency || "USD";
  state.dash = d;
  if(camps && rules) renderCampaigns(camps.campaigns||[], rules.rules||[]);
  renderOverview(d);
  renderPartners();
  if(fraud) renderFraud(fraud);
  if(apps) renderApplications(apps);
  if(payouts) renderPayouts(payouts);
  if(tax) renderTaxDocs(tax);
  if(settings) renderSettings(settings);
  if(imports) renderImports(imports);
  if(tiers) renderTiers(tiers.tiers||[]);
  loadResources();
  loadBrand();
  attention.billingFailing = billingHealth().filter(h=>h.failing);
  renderAttention();
  renderSetup();
}

// ---------------------------------------------------------------- admin: overview
function renderOverview(d){
  setCount("holds", d.commission_holds||0);
  books(d.ledger_integrity_sum === 0);
  $("a-cards").innerHTML =
    card("Clicks (all time)", d.clicks_total) +
    card("Sales", d.conversions_total) +
    card("Paid to partners", d.payouts_paid_by_currency ? moneyList(d.payouts_paid_by_currency) : money(d.payouts_paid_cents)) +
    card("Payouts to approve", d.payouts_open_count) +
    card("Open fraud verdicts", d.fraud_open_verdicts, d.fraud_open_verdicts>0?"bad":"") +
    card("Held commissions", d.commission_holds, d.commission_holds>0?"bad":"");
  renderTrend();
  const top = d.top_affiliates||[];
  $("a-leaders").innerHTML = top.length ? top.map(l=>
    `<tr class="click" tabindex="0" data-aff="${l.affiliate_id}"><td>${partnerCell(l.affiliate_id, l.email)}</td><td class="num">${l.earned ? moneyList(l.earned) : money(l.earned_cents)}</td><td>${statusBadge(l.status)}</td></tr>`
  ).join("") : none(3, "No earnings yet. They show here after your partners' first sales.");
}

// Last 30 days: one bar per UTC day for the chosen measure, on the admin
// Overview (whole program) and the partner's Earnings page (their links).
// Pointing at, tapping or tabbing to a bar reads that day out in the header.
// Commission is one currency at a time (the program's by default); when the
// days hold more than one, a labelled picker chooses which. Nothing is
// converted or added across currencies.
const TREND_HINT = matchMedia("(hover: none)").matches ? "Tap a day to see its numbers." : "Point at a day to see its numbers.";
function trendChart(root, texts){
  const chart = root.querySelector(".chart"), read = root.querySelector(".trend-read");
  const curRow = root.querySelector(".trend-cur-row"), curPick = root.querySelector(".trend-cur");
  let metric = "clicks", days = [], cur = "";
  const m = {
    clicks: {fmt: v=>num(v), one:"click", many:"clicks", empty: texts.clicks},
    conversions: {fmt: v=>num(v), one:"sale", many:"sales", empty: texts.conversions},
    commission_cents: {fmt: v=>money(v, cur||undefined), word: texts.money, empty: texts.commission_cents},
  };
  // The day's value; commission in the picked currency when one is picked.
  const val = (d, k)=>{
    if(k !== "commission_cents" || !cur) return d[k]||0;
    const c = (d.commissions||[]).find(x=>x.currency===cur);
    return c ? c.amount_minor : 0;
  };
  const text = d=>{
    const k = m[metric], v = val(d, metric);
    return `${day(d.day)}: ` + (k.one ? `${k.fmt(v)} ${plural(v, k.one, k.many)}` : `${k.fmt(v)} ${k.word}`);
  };
  const clear = ()=>{ chart.querySelectorAll(".bar.on").forEach(b=>b.classList.remove("on")); };
  function render(){
    for(const k in m){ root.querySelector(`[data-total="${k}"]`).textContent = m[k].fmt(days.reduce((s,d)=>s+val(d, k), 0)); }
    root.querySelectorAll(".seg button").forEach(b=>b.setAttribute("aria-selected", String(b.dataset.metric===metric)));
    const vals = days.map(d=>val(d, metric)), max = Math.max(0, ...vals);
    chart.innerHTML = days.map((d,i)=>
      `<span class="bar${vals[i]?"":" zero"}" role="img" tabindex="0" data-i="${i}" aria-label="${esc(text(d))}"><i style="height:${max ? Math.max(2, Math.round(vals[i]/max*100)) : 0}%"></i></span>`
    ).join("") + (max ? `<span class="max">${m[metric].fmt(max)}</span>` : `<div class="chart-empty">${m[metric].empty}</div>`);
    root.querySelector(".chart-x").innerHTML = days.length ? `<span>${day(days[0].day)}</span><span>Today</span>` : "";
    read.textContent = max ? TREND_HINT : texts.idle;
  }
  function show(e){
    const bar = e.target.closest && e.target.closest(".bar"); if(!bar) return;
    const d = days[+bar.dataset.i]; if(!d) return;
    clear(); bar.classList.add("on");
    read.textContent = text(d);
  }
  chart.addEventListener("mouseover", show);
  chart.addEventListener("focusin", show);
  chart.addEventListener("click", show);
  chart.addEventListener("mouseleave", ()=>{ if(!chart.contains(document.activeElement)){ clear(); if(days.some(d=>val(d, metric))) read.textContent = TREND_HINT; } });
  root.querySelectorAll(".seg button").forEach(b=>b.addEventListener("click", ()=>{ metric = b.dataset.metric; render(); }));
  curPick.addEventListener("change", ()=>{ cur = curPick.value; render(); });
  return {set(d, base){
    days = d || [];
    // Every currency the days have commission in, the default one first.
    const seen = new Set(base ? [base] : []);
    days.forEach(x=>(x.commissions||[]).forEach(c=>seen.add(c.currency)));
    const list = [...seen];
    curRow.hidden = list.length < 2;
    if(list.length < 2){ cur = ""; }
    else {
      if(!list.includes(cur)) cur = base || list[0];
      curPick.innerHTML = list.map(c=>`<option value="${esc(c)}">${esc(c)}</option>`).join("");
      curPick.value = cur;
    }
    render();
  }};
}
const adminTrend = trendChart($("trend"), {
  clicks: "No clicks in the last 30 days yet. They show here once partners share their links.",
  conversions: "No sales in the last 30 days yet.",
  commission_cents: "No commission earned in the last 30 days yet.",
  money: "commission", idle: "Clicks, sales and commission, day by day.",
});
function renderTrend(){ adminTrend.set(state.dash && state.dash.daily, state.dash && state.dash.currency); }
const portalTrend = trendChart($("p-trend"), {
  clicks: "No clicks in the last 30 days yet. Share your link and they show up here.",
  conversions: "No sales in the last 30 days yet.",
  commission_cents: "Nothing earned in the last 30 days yet.",
  money: "earned", idle: "Your clicks, sales and earnings, day by day.",
});

// Getting started: five steps, each ticked off by what the engine can see.
const BILLING = {stripe:"Stripe", paddle:"Paddle", chargebee:"Chargebee", lemonsqueezy:"Lemon Squeezy", recurly:"Recurly", polar:"Polar"};
// billingHealth: per configured billing tool, whether its webhook events
// arrive, and whether the latest signed one was rejected (in the last week).
function billingHealth(){
  const s = state.setup||{}, week = Date.now()-7*864e5;
  return (s.billing||[]).map(p=>{
    const w = (s.webhooks||[]).find(x=>x.provider===p) || {};
    const failing = !!w.last_err_at && (!w.last_ok_at || w.last_err_at > w.last_ok_at) && parseTime(w.last_err_at).getTime() > week;
    return {provider:p, name:BILLING[p]||p, received:(w.ok_count||0)>0, failing, w};
  });
}
function renderSetup(){
  const d = state.dash||{}, s = state.setup||{}, billing = s.billing||[];
  const billingNames = billing.map(b=>BILLING[b]||b).join(", "), health = billingHealth();
  const received = health.some(h=>h.received && !h.failing);
  const guides = `<a href="https://affiliatetracking.co/integrations" target="_blank" rel="noopener">Setup guides</a>`;
  const named = !!(brand && brand.brand_name);
  const steps = [
    [named, "Name your program", "Partners see it in their portal and in every email.", `<a class="mini go" href="#/settings" data-focus="br-name">Name it</a>`],
    [!attention.norate, "Set your commission", "How much partners earn from each payment.", `<a class="mini go" href="#/settings" data-focus="cs-rate">Set commission</a>`],
    [received, "Connect your billing", !billing.length ? `Sales reach us through a webhook from Stripe, Paddle, Chargebee, Recurly, Lemon Squeezy or Polar. ${guides}.`
      : received ? `Connected: ${billingNames}. Events are arriving.`
      : health.some(h=>h.failing) ? `${billingNames} is set up, but its events are being rejected. <a href="#/settings">See why</a>.`
      : `${billingNames} is set up, but no event has arrived yet. This ticks off with your first sale, or send a test event from your ${billingNames} dashboard.`, ""],
    [(d.clicks_total||0)>0, "Add the tracking script to your website", "Put it on every page, before &lt;/body&gt;. This ticks off after the first partner click.", `<div class="snip"><code id="st-script">${esc('<script src="'+location.origin+'/t.js" async></script>')}</code><button class="mini" data-copy="st-script" data-what="Script copied">Copy</button></div>`],
    [state.affs.length>0, "Invite your first partner", "Share your signup page, or add someone yourself.", `<div class="snip"><code id="st-apply">${esc(location.origin+"/apply")}</code><button class="mini" data-copy="st-apply" data-what="Signup link copied">Copy</button><button class="mini go" type="button" data-add-partner>Add partner</button></div>`],
    [(d.conversions_total||0)>0, "Get your first partner sale", "Happens by itself when someone a partner sent buys.", ""]
  ];
  const done = steps.filter(x=>x[0]).length;
  $("setup").hidden = done === steps.length || !!state.demo; // the demo is a finished program
  $("setup-progress").textContent = `${done} of ${steps.length} done`;
  $("setup").querySelector("p").textContent = `${steps.length} steps to your first partner sale. Each one ticks itself off.`;
  $("setup-steps").innerHTML = steps.map(([ok, title, text, extra], i)=>
    `<li class="${ok?"done":""}"><b>${ok?"✓":i+1}</b><div><h3>${title}</h3><p>${text}</p>${ok||!extra?"":`<div style="margin-top:10px">${extra}</div>`}</div></li>`
  ).join("");
  const row = (on, text)=>`<div class="status ${on===true?"on":on==="bad"?"bad":"off"}"><i></i><span>${text}</span></div>`;
  $("setup-status").innerHTML =
    (billing.length ? health.map(h=>
      h.failing ? row("bad", `${esc(h.name)}: the last event was rejected ${ago(h.w.last_err_at)}. Sales may not be credited. Check the webhook signing secret you set for ${esc(h.name)}.<span class="sub">${esc(h.w.last_err)}${h.w.last_ok_at?` · Last accepted event: ${ago(h.w.last_ok_at)}`:""}</span>`)
      : h.received ? row(true, `${esc(h.name)}: working. Last event ${ago(h.w.last_ok_at)}, ${num(h.w.ok_count)} received.`)
      : row(false, `${esc(h.name)}: set up, but no event has arrived yet.`)).join("") : row(false, `Billing: not connected yet. ${guides}`)) +
    row(!!s.mail, s.mail ? "Partner emails: on (sign-in links and payout notes)" : "Partner emails: off. You copy sign-in links and send them yourself.") +
    row((d.clicks_total||0)>0, (d.clicks_total||0)>0 ? "Tracking script: working, clicks are arriving" : "Tracking script: no clicks seen yet");
}

// ---------------------------------------------------------------- test your setup
// Live checks: the script on the owner's site, a click through the test
// partner's link, then a test-mode sale credited to it.
const sc = {since:"", timer:0, id:0};
function scSet(el, cls, text){ el.className = "sc-out" + (cls?" "+cls:""); el.textContent = text; }
function scStep(n, ok){ $("sc-"+n).classList.toggle("ok", !!ok); $("sc-"+n).querySelector("b").textContent = ok ? "✓" : String(n); }
document.addEventListener("click", async e=>{
  if(!e.target.closest("[data-setup-check]")) return;
  const d = $("sc-dlg");
  $("sc-site").value = $("sc-site").value || (state.setup && state.setup.site_url) || "";
  scSet($("sc-site-out"), "", ""); scSet($("sc-click-out"), "", "Getting your test link…"); scSet($("sc-sale-out"), "", "Waiting for a test sale…");
  [1,2,3].forEach(n=>scStep(n,false)); $("sc-done").hidden = true;
  d.showModal();
  try{
    const r = await post("/api/v1/admin/setup-check/start", {});
    sc.since = r.started_at; sc.id = r.affiliate_id;
    const link = location.origin + "/r/" + r.code;
    $("sc-link").textContent = link; $("sc-open").href = link;
    scSet($("sc-click-out"), "", "Waiting for the click…");
    clearInterval(sc.timer); sc.timer = setInterval(scPoll, 3000);
  }catch(err){ scSet($("sc-click-out"), "bad", errText(err)); }
});
async function scPoll(){
  if(!$("sc-dlg").open){ clearInterval(sc.timer); return; }
  let r; try{ r = await api("/api/v1/admin/setup-check?since="+encodeURIComponent(sc.since)); }catch(e){ return; }
  if(r.clicks>0){ scStep(2,true); scSet($("sc-click-out"), "ok", `✓ Click recorded. Your website got the visitor with their partner's token.`); }
  const sale = (r.sales||[])[0];
  if(sale){
    scStep(3,true);
    scSet($("sc-sale-out"), "ok", `✓ Sale credited: ${money(sale.amount_cents, sale.currency)} commission to the test partner.`);
    $("sc-done").hidden = false; clearInterval(sc.timer);
  }else{
    const bad = billingHealth().find(h=>h.failing);
    if(bad) scSet($("sc-sale-out"), "bad", `${bad.name} sent an event we had to reject: ${bad.w.last_err}. Check the webhook signing secret.`);
  }
}
$("sc-site-form").addEventListener("submit", async e=>{
  e.preventDefault();
  const out = $("sc-site-out"); scSet(out, "", "Checking…");
  try{
    const r = await post("/api/v1/admin/setup-check/site", {url: $("sc-site").value.trim()});
    if(!r.reached){ scStep(1,false); scSet(out, "bad", r.problem); return; }
    if(!r.found){ scStep(1,false); scSet(out, "bad", `We opened ${r.url} but didn't find the tracking script. Add it before </body> on every page (Settings, Your setup), then check again.`); return; }
    const ours = r.src.indexOf(location.host) >= 0 || r.src.charAt(0)==="/";
    scStep(1, ours);
    scSet(out, ours?"ok":"bad", ours ? `✓ Found the tracking script on ${r.url}.` : `Found a tracking script, but it loads from ${r.src}, not ${location.host}. Copy the script from Settings, Your setup.`);
  }catch(err){ scSet(out, "bad", errText(err)); }
});
$("sc-close").addEventListener("click", ()=>{ $("sc-dlg").close(); clearInterval(sc.timer); });
$("sc-pause").addEventListener("click", async ()=>{
  try{
    await api("/api/v1/admin/affiliates/"+sc.id, {method:"PATCH", body: JSON.stringify({status:"frozen"})});
    $("sc-dlg").close(); msg("Test partner paused. Nothing will be paid to it.","ok"); loadAdmin();
  }catch(err){ msg(errText(err),"err"); }
});

// ---------------------------------------------------------------- admin: partners
// A partner's money in one status, in every currency they earn in (the
// *_cents fields are the program currency only).
function affMoney(x, status){ return x.currencies ? moneyStatus(x.currencies, status) : money(x[status+"_cents"]); }
function owesAny(x){ return x.currencies ? x.currencies.some(c=>((c.by_status_cents||{}).approved||0)>0) : x.approved_cents>0; }
function renderPartners(){
  const q = ($("aff-search").value||"").trim().toLowerCase();
  const list = state.affs.filter(x=>!q || (x.email||"").toLowerCase().includes(q) || (x.name||"").toLowerCase().includes(q));
  $("a-affs").innerHTML = !state.affs.length
    ? none(6, `No partners yet. Share <b>${esc(location.origin)}/apply</b> or press <b>Add partner</b>.`)
    : !list.length ? none(6, "No partner matches that search.")
    : list.map(x=>
      `<tr class="click" tabindex="0" data-aff="${x.id}"><td>${partnerCell(x.id, x.email, x.name)}</td><td>${statusBadge(x.status)}${x.status==="active" && !x.pay_to && owesAny(x) ? " "+badge("pending","No payout details") : ""}</td><td class="num">${affMoney(x,"pending")}</td><td class="num">${affMoney(x,"approved")}</td><td class="num">${affMoney(x,"paid")}</td><td class="num">${num(x.clicks_30d)}</td></tr>`
    ).join("");
  const opts = `<option value="">Choose a partner</option>` + state.affs.map(x=>`<option value="${x.id}">${esc(x.name ? `${x.name} (${x.email})` : x.email)}</option>`).join("");
  document.querySelectorAll("select.aff-pick").forEach(s=>{ const keep = s.value; s.innerHTML = opts; s.value = keep; });
}
$("aff-search").addEventListener("input", renderPartners);
document.addEventListener("click", e=>{
  const tr = e.target.closest("tr[data-aff]"); if(tr && !e.target.closest("button,a")) openPartner(Number(tr.dataset.aff));
});
document.addEventListener("keydown", e=>{
  const tr = e.target.closest && e.target.closest("tr[data-aff]");
  if(tr && (e.key==="Enter"||e.key===" ")){ e.preventDefault(); openPartner(Number(tr.dataset.aff)); }
});

// Partner panel: referral links, money, sign-in link, pause.
async function openPartner(id){
  const x = state.affs.find(a=>a.id===id) || {id, email: state.emailOf[id]||("Partner #"+id), status:""};
  const pp = $("pp"), mail = state.setup && state.setup.mail, paused = x.status==="frozen";
  $("pp-in").innerHTML = `<div class="top"><div><h2 id="pp-name">${esc(x.email)}</h2>${x.name?`<span class="sub">${esc(x.name)}</span>`:""}</div><button class="x" type="button" id="pp-close" aria-label="Close">×</button></div>
    <div>${x.status?statusBadge(x.status):""}${paused?`<p class="note">Paused: they can't sign in, new sales aren't credited to them, and nothing is paid to them until you resume. What they already earned is kept.</p>`:""}</div>
    <div><h3>Referral links</h3><div id="pp-links"><p class="muted">Loading…</p></div>
      <form class="row" id="pp-addcode" style="margin-top:8px"><input type="text" id="pp-code" placeholder="Add a code, like spring-promo" maxlength="32" style="flex:1 1 180px"><button class="mini" type="submit">Add</button></form></div>
    <div><h3>Money</h3><div class="mini-cards"><div><span>In hold</span><b>${affMoney(x,"pending")}</b></div><div><span>Ready to pay</span><b>${affMoney(x,"approved")}</b></div><div><span>Paid</span><b>${affMoney(x,"paid")}</b></div></div>
      <p class="note">${num(x.clicks_30d||0)} ${plural(x.clicks_30d||0,"click","clicks")} in the last 30 days.</p>
      <p class="note"><b>Commission:</b> ${x.own_rate ? `${esc(rateLabel(x.own_rate))}, their own rate.` : `the program rate${csDefault(1) ? `, ${esc(rateLabel(csDefault(1)))}` : ""}.`}</p>
      <form class="row" id="pp-rate" style="margin-top:8px">
        <label class="sr-only" for="pp-rate-v">Their own commission</label><input type="number" id="pp-rate-v" min="0" step="0.01" placeholder="${x.own_rate ? "" : "e.g. 40"}" value="${x.own_rate ? (x.own_rate.rate_type==="percent" ? x.own_rate.rate_value/100 : toMajor(x.own_rate.rate_value, x.own_rate.currency)) : ""}" style="width:110px">
        <label class="sr-only" for="pp-rate-t">Commission type</label><select id="pp-rate-t"><option value="percent"${x.own_rate && x.own_rate.rate_type==="fixed" ? "" : " selected"}>% of each payment</option><option value="fixed"${x.own_rate && x.own_rate.rate_type==="fixed" ? " selected" : ""}>$ per payment</option></select>
        <button class="mini" type="submit">${x.own_rate ? "Change rate" : "Give them their own rate"}</button>${x.own_rate ? `<button class="mini" type="button" id="pp-rate-clear">Back to program rate</button>` : ""}
      </form>
      <p class="note" style="margin-top:0">Applies to their new sales. What they already earned keeps its rate.</p>
      <p class="note"><b>Pay to:</b> ${x.pay_to ? esc(x.pay_to) : `not given yet. They add it in their partner portal, under How you get paid.${x.pay_to_asked_at ? ` We emailed them to add it on ${day(x.pay_to_asked_at)}.` : ""}`}</p></div>
    <div><h3>Access</h3>${paused ? "" : mail
      ? `<p class="note" style="margin:0 0 10px">Email them a one-time sign-in link to their partner portal. It works for 1 hour.</p><button class="primary" id="pp-mail" type="button">Email a sign-in link</button>`
      : `<p class="note" style="margin:0 0 10px">Partner emails are off, so sign-in links only come from approving an application.</p>`}
      <div style="margin-top:12px">${paused
        ? `<button class="primary" id="pp-resume" type="button">Resume partner</button>`
        : `<button class="ghost" id="pp-pause" type="button">Pause partner</button>`}</div></div>`;
  $("pp-close").addEventListener("click", ()=>pp.close());
  const mb = $("pp-mail");
  if(mb) mb.addEventListener("click", async ()=>{
    mb.disabled = true;
    try{ await post("/api/v1/portal/login-link", {email: x.email}); msg(`Sign-in link sent to ${x.email}.`,"ok"); }
    catch(e){ msg(errText(e),"err"); mb.disabled = false; }
  });
  const setStatus = async (st)=>{
    try{
      await api("/api/v1/admin/affiliates/"+id, {method:"PATCH", body: JSON.stringify({status: st})});
      x.status = st; msg(st==="frozen" ? `${x.email} is paused.` : `${x.email} is active again.`,"ok");
      await loadAdmin(); openPartner(id);
    }catch(e){ msg(errText(e),"err"); }
  };
  if($("pp-pause")) $("pp-pause").addEventListener("click", async ()=>{
    pp.close();
    const a = await ask({title:`Pause ${x.email}?`, body:"They can't sign in, new sales aren't credited to them, and nothing is paid to them until you resume. What they already earned is kept.", ok:"Pause partner", danger:true});
    if(a.ok) setStatus("frozen"); else openPartner(id);
  });
  if($("pp-resume")) $("pp-resume").addEventListener("click", ()=>setStatus("active"));
  const saveRate = async body=>{
    try{
      const r = await api(`/api/v1/admin/affiliates/${id}/rate`, {method:"PUT", body: JSON.stringify(body)});
      msg(r.own_rate ? `${x.email} now earns ${rateLabel(r.own_rate)}.` : `${x.email} is back on the program rate.`, "ok");
      await loadAdmin(); openPartner(id);
    }catch(e){ msg(errText(e),"err"); }
  };
  $("pp-rate").addEventListener("submit", e=>{
    e.preventDefault();
    const raw = parseFloat($("pp-rate-v").value), type = $("pp-rate-t").value, value = type==="percent" ? Math.round(raw*100) : toMinor(raw);
    if(!(raw>0)){ $("pp-rate-v").focus(); msg("Enter a commission above 0.","err"); return; }
    if(type==="percent" && value>10000){ msg("A percentage commission can't be over 100%.","err"); return; }
    saveRate({rate_type:type, rate_value:value});
  });
  if($("pp-rate-clear")) $("pp-rate-clear").addEventListener("click", ()=>saveRate({clear:true}));
  $("pp-addcode").addEventListener("submit", async e=>{
    e.preventDefault();
    const code = $("pp-code").value.trim().toLowerCase();
    if(!/^[a-z0-9_-]{2,32}$/.test(code)){ msg("Codes are 2 to 32 letters, numbers, - or _.","err"); return; }
    try{ await post("/api/v1/admin/codes", {affiliate_id:id, code}); msg(`Added ${location.origin}/r/${code}.`,"ok"); loadCodes(); $("pp-code").value = ""; }
    catch(err){ msg(errText(err),"err"); }
  });
  if(!pp.open) pp.showModal();
  async function loadCodes(){
    try{
      const c = await api("/api/v1/admin/codes?affiliate_id="+id);
      const codes = (c.codes||[]).sort((a,b)=>(b.primary?1:0)-(a.primary?1:0));
      $("pp-links").innerHTML = codes.length ? codes.map((k,i)=>`<div class="snip" style="margin-bottom:6px"><code id="pp-l${i}">${esc(location.origin+"/r/"+k.code)}</code><button class="mini" data-copy="pp-l${i}" data-what="Link copied">Copy</button>${k.primary?`<span class="sub" style="margin:0 6px">Main</span>`:`<button class="mini" data-delcode="${esc(k.code)}">Remove</button>`}</div>`).join("")
        : `<p class="muted">No referral code yet. Add one below.</p>`;
      $("pp-links").querySelectorAll("[data-delcode]").forEach(b=>b.addEventListener("click", async ()=>{
        const code = b.dataset.delcode;
        pp.close();
        const a = await ask({title:`Remove ${code}?`, body:`Links using /r/${code} stop crediting ${x.email}. Their other links keep working.`, ok:"Remove link", danger:true});
        pp.showModal();
        if(!a.ok) return;
        try{ await api("/api/v1/admin/codes?code="+encodeURIComponent(code), {method:"DELETE"}); msg(`Removed ${code}.`,"ok"); loadCodes(); }
        catch(err){ msg(errText(err),"err"); }
      }));
    }catch(e){ $("pp-links").innerHTML = `<p class="muted">${esc(errText(e))}</p>`; }
  }
  loadCodes();
}
$("pp").addEventListener("click", e=>{ if(e.target===$("pp")) $("pp").close(); });

// Email partners: program news to every active partner who hasn't
// unsubscribed (the server picks them and adds each unsubscribe link).
$("news-open").addEventListener("click", ()=>{
  const mail = state.setup && state.setup.mail, n = state.affs.filter(x=>x.status==="active").length;
  $("news-form").reset(); $("news-out").innerHTML = "";
  $("news-go").hidden = false; $("news-go").disabled = !mail || !n; $("news-cancel").textContent = "Cancel";
  $("news-go").textContent = n ? `Send to ${n} ${plural(n,"partner","partners")}` : "Send";
  $("news-help").textContent = !mail ? "Partner emails are off, so messages can't be sent yet. Turn on partner emails first (see Settings, Your setup)."
    : !n ? "You don't have any active partners to email yet."
    : "Program news, like a higher commission this month or a new page to link to.";
  $("news-dlg").showModal(); if(mail && n) $("news-subject").focus();
});
$("news-cancel").addEventListener("click", ()=>$("news-dlg").close());
$("news-form").addEventListener("submit", async e=>{
  e.preventDefault();
  const go = $("news-go");
  go.disabled = true; go.textContent = "Sending…";
  try{
    const r = await post("/api/v1/admin/partners/message", {subject: $("news-subject").value, body: $("news-body").value});
    $("news-out").innerHTML = `<div class="grantbox"><b>✓ Sent to ${r.queued} ${plural(r.queued,"partner","partners")}.</b> The emails go out within a minute.</div>`;
    go.hidden = true; $("news-cancel").textContent = "Done";
  }catch(err){
    $("news-out").innerHTML = `<p class="fail">${esc(errText(err))}</p>`;
    go.disabled = false; go.textContent = "Try again";
  }
});

// Add partner: the same path as the signup page, then approve it.
document.addEventListener("click", e=>{ if(e.target.closest("#add-partner,[data-add-partner]")) openAdd(); });
function openAdd(){
  $("add-form").reset(); $("add-out").innerHTML = ""; $("add-go").disabled = false; $("add-go").hidden = false;
  $("add-cancel").textContent = "Cancel";
  $("add-help").textContent = state.setup&&state.setup.mail ? "They get a referral link right away, and an email with a sign-in link." : "They get a referral link right away. You'll get a sign-in link to send them.";
  $("add-dlg").showModal(); $("add-name").focus();
}
$("add-cancel").addEventListener("click", ()=>$("add-dlg").close());
$("add-form").addEventListener("submit", async e=>{
  e.preventDefault();
  const email = $("add-email").value.trim().toLowerCase(), name = $("add-name").value.trim(), note = $("add-note").value.trim();
  if(state.affs.some(a=>(a.email||"").toLowerCase()===email)){ $("add-out").innerHTML = `<div class="empty">${esc(email)} is already a partner.</div>`; return; }
  $("add-go").disabled = true;
  try{
    await post("/api/v1/portal/apply", {email, name, note});
    const apps = await api("/api/v1/admin/portal/applications");
    const app = (apps.applications||[]).find(a=>a.status==="pending" && a.email.toLowerCase()===email);
    if(!app){ $("add-out").innerHTML = `<div class="empty">${esc(email)} already applied before. Find them under Applications.</div>`; $("add-go").disabled = false; return; }
    const out = await post("/api/v1/admin/portal/review", {application_id: app.id, decision: "approve"});
    $("add-out").innerHTML = approvedBox(out, email);
    $("add-go").hidden = true; $("add-cancel").textContent = "Done";
    loadAdmin();
  }catch(err){ $("add-out").innerHTML = `<div class="empty">${esc(errText(err))}</div>`; $("add-go").disabled = false; }
});
function approvedBox(out, email){
  const link = location.origin + "/#grant=" + out.login_grant;
  const ref = out.referral_code ? `<div class="snip" style="margin:8px 0 0"><code id="ok-ref">${esc(location.origin+"/r/"+out.referral_code)}</code><button class="mini" data-copy="ok-ref" data-what="Referral link copied">Copy</button></div>` : "";
  return out.emailed
    ? `<div class="grantbox"><b>Done.</b> We emailed ${esc(email)} a sign-in link. Their referral link:${ref}</div>`
    : `<div class="grantbox"><b>Done.</b> Send ${esc(email)} this sign-in link. It works once, within 24 hours, and is shown only now.<div class="snip" style="margin:8px 0"><code id="ok-grant">${esc(link)}</code><button class="mini go" data-copy="ok-grant" data-what="Sign-in link copied">Copy</button></div>Their referral link:${ref}</div>`;
}

// ---------------------------------------------------------------- admin: applications
function renderApplications(apps){
  const list = (apps.applications||[]).filter(a=>a.status==="pending");
  setCount("apps", list.length);
  $("a-apps-table").style.display = list.length?"table":"none";
  $("a-apps-empty").style.display = list.length?"none":"block";
  $("a-apps").innerHTML = list.map(a=>
    `<tr><td>${esc(a.email)}${a.name?`<span class="sub">${esc(a.name)}</span>`:""}</td><td class="muted">${esc(a.note||"")}</td><td class="muted">${day(a.created_at)}</td>`+
    `<td class="nowrap"><button class="mini go" data-approve="${a.id}" data-email="${esc(a.email)}">Approve</button><button class="mini" data-reject="${a.id}" data-email="${esc(a.email)}">Reject</button></td></tr>`
  ).join("");
}
document.addEventListener("click", async e=>{
  const ap = e.target.closest("[data-approve]"), rj = e.target.closest("[data-reject]");
  if(!ap && !rj) return;
  const b = ap||rj, id = Number(b.dataset.approve||b.dataset.reject), email = b.dataset.email;
  const mail = state.setup && state.setup.mail;
  let reason = "";
  if(rj){
    const a = await ask({title:`Reject ${email}?`, ok:"Reject", danger:true,
      body: mail ? "We'll email them a short, polite note saying no. Add a reason if you like. They can't apply again with this address."
                 : "They aren't emailed, because partner emails are off. They can't apply again with this address.",
      note: mail ? "Reason (optional), like: We only work with newsletters right now." : ""});
    if(!a.ok) return;
    reason = a.note || "";
  }
  b.disabled = true;
  try{
    const out = await post("/api/v1/admin/portal/review", {application_id:id, decision: ap?"approve":"reject", reason});
    $("a-apps-out").innerHTML = ap ? approvedBox(out, email) : "";
    if(rj) msg(out.emailed ? `${email} rejected. We emailed them.` : `${email} rejected.`,"ok");
    loadAdmin();
  }catch(err){ b.disabled = false; msg(errText(err),"err"); }
});

// ---------------------------------------------------------------- admin: fraud
function renderFraud(f){
  const vs = f.verdicts||[], hs = f.holds||[];
  $("a-holds-table").style.display = hs.length?"table":"none";
  $("a-holds-empty").style.display = hs.length?"none":"block";
  $("a-holds").innerHTML = hs.map(h=>{
    const r = /fraud review:\s*([a-z_]+)/.exec(h.reason||"");
    const why = r ? `${esc(ruleName(r[1]))}${ruleHelp(r[1])}` : esc(h.reason);
    return `<tr><td>${partnerCell(h.affiliate_id, h.email)}</td><td class="num">${money(h.amount_cents)}</td><td class="muted">${why}</td>`+
      `<td class="nowrap"><button class="mini go" data-release="${h.commission_id}">Release</button><button class="mini" data-uphold="${h.commission_id}" data-amt="${h.amount_cents}" data-email="${esc(h.email||"")}">Uphold</button></td></tr>`;
  }).join("");
  $("a-fraud-table").style.display = vs.length?"table":"none";
  $("a-fraud-empty").style.display = vs.length?"none":"block";
  $("a-verdicts").innerHTML = vs.map(v=>
    `<tr><td>${esc(ruleName(v.rule))}${ruleHelp(v.rule)}</td><td>${subjectText(v.subject)}</td><td class="muted">${esc(v.reason)}</td>`+
    `<td class="nowrap">${v.action===2?badge("frozen","Partner paused"):v.action===1?badge("hold","Commission held"):badge("", "Noted")}${v.dry_run?`<span class="sub">Dry run: not enforced</span>`:""}</td></tr>`
  ).join("");
}
document.addEventListener("click", async e=>{
  const rel = e.target.closest("[data-release]"), up = e.target.closest("[data-uphold]");
  if(!rel && !up) return;
  const id = Number((rel||up).dataset.release||(rel||up).dataset.uphold);
  let note = "", cents = up ? Number(up.dataset.amt) : 0;
  if(up){
    const a = await ask({title:`Uphold this ${money(cents)} commission?`, body:`It's cancelled and the money is taken back from ${up.dataset.email||"the partner"}. This can't be undone.`, ok:"Uphold and take it back", danger:true, note:"Optional note for your records: why it's fraud"});
    if(!a.ok) return;
    note = a.note;
  }
  try{
    const r = await post("/api/v1/admin/fraud/resolve", {commission_id:id, decision: rel?"release":"uphold", note});
    msg(rel ? "Released. It's paid in the next payout." : `Upheld. ${money(r.clawback_cents||cents)} taken back.`, "ok");
    loadAdmin();
  }catch(err){ msg(errText(err),"err"); }
});

// ---------------------------------------------------------------- admin: payouts
function renderPayouts(out){
  const list = out.payouts||[];
  setCount("payouts", list.length);
  $("po-table").style.display = list.length?"table":"none";
  $("po-empty").style.display = list.length?"none":"block";
  $("po-body").innerHTML = list.map(p=>
    `<tr><td>${partnerCell(p.affiliate_id, p.email)}</td><td class="num">${money(p.amount_cents, p.currency)}</td>`+
    `<td>${esc(METHODS[p.method]||p.method)}${p.exported_at?'<span class="sub">In a downloaded CSV</span>':""}${p.attention?`<span class="sub" style="color:var(--red)">${esc(p.attention)}</span>`:""}${p.held?`<span class="sub" style="color:var(--red)">${esc(p.held)}</span>`:""}</td>`+
    `<td class="muted">${payToText(p.pay_to, p.pay_to_asked_at)}</td>`+
    `<td class="muted">${day(p.created_at)}</td><td class="nowrap"><button class="mini go" data-pay="${p.id}" data-amt="${p.amount_cents}" data-cur="${esc(p.currency)}" data-email="${esc(p.email)}" data-payto="${esc(p.pay_to||"")}">Mark as paid</button></td></tr>`
  ).join("");
}
document.addEventListener("click", async e=>{
  const b = e.target.closest("[data-pay]"); if(!b || b.disabled) return;
  const a = await ask({title:`Mark ${money(Number(b.dataset.amt), b.dataset.cur)} to ${b.dataset.email} as paid?`, body:(b.dataset.payto ? `They asked to be paid by ${b.dataset.payto}. ` : "They haven't said where to be paid yet. ")+"Do this after you've sent the money. It settles the books and emails the partner.", ok:"Yes, it's paid"});
  if(!a.ok) return;
  b.disabled = true; b.textContent = "Saving…"; // double-click guard (the server is idempotent too)
  try{
    const r = await post("/api/v1/admin/payouts/approve", {payout_id:Number(b.dataset.pay)});
    msg(r.already_paid ? "That payout was already marked as paid." : `Marked as paid.${r.emailed?" We emailed the partner.":""}`,"ok");
    loadAdmin();
  }catch(err){ b.disabled = false; b.textContent = "Mark as paid"; msg(errText(err),"err"); }
});
async function runPayouts(dry){
  const body = {method: $("pr-method").value, dry_run: dry};
  const v = $("pr-min").value.trim();
  if(v !== ""){
    const c = toMinor(v);
    if(!(c>=0)){ msg("Enter the minimum as an amount, like 50.","err"); return; }
    body.min_threshold_cents = c;
  }
  if(!dry && !(await ask({title:"Create payouts now?", body:"Each partner's ready commissions become one payout, waiting for you to pay and mark as paid. No money is sent by us.", ok:"Create payouts"})).ok) return;
  try{
    const r = await post("/api/v1/admin/payouts/run", body);
    const box = $("pr-out"); box.hidden = false;
    if(r.dry_run){
      // One payout per partner and currency; the total is per currency.
      const rows = r.batches||[], total = {};
      rows.forEach(x=>{ total[x.currency] = (total[x.currency]||0) + x.cents; });
      box.innerHTML = rows.length
        ? `<p class="note" style="margin:0 0 8px"><b>Preview.</b> Nothing has changed yet. ${rows.length} ${plural(rows.length,"payout","payouts")} would be made.</p><div class="tbl"><table><thead><tr><th>Partner</th><th class="num">Commissions</th><th class="num">Amount</th></tr></thead><tbody>`+
          rows.map(x=>`<tr><td>${partnerCell(x.affiliate_id)}</td><td class="num">${x.commissions}</td><td class="num">${money(x.cents, x.currency)}</td></tr>`).join("")+
          `<tr class="total"><td>Total</td><td class="num">${rows.reduce((s,x)=>s+x.commissions,0)}</td><td class="num">${moneyList(Object.keys(total).sort().map(c=>({currency:c, amount_minor:total[c]})))}</td></tr></tbody></table></div>`
        : `<div class="empty">Nobody is owed ${money(r.min_threshold_cents)} or more yet, so there's nothing to pay.</div>`;
    } else {
      box.innerHTML = `<div class="grantbox"><b>${r.payouts_created} ${plural(r.payouts_created,"payout","payouts")} created.</b> Pay ${plural(r.payouts_created,"it","them")}, then mark each one as paid below.${r.asked_for_details ? ` We emailed ${r.asked_for_details} ${plural(r.asked_for_details,"partner","partners")} to add where to be paid.` : ""}</div>`;
      loadAdmin();
    }
  }catch(err){ msg(errText(err),"err"); }
}
$("pr-preview").addEventListener("click", ()=>runPayouts(true));
$("pr-go").addEventListener("click", ()=>runPayouts(false));
$("po-paypal").addEventListener("click", ()=>downloadAuthed("/api/v1/admin/payouts/export/paypal.csv", "payouts-paypal.csv"));
$("po-wise").addEventListener("click", ()=>downloadAuthed("/api/v1/admin/payouts/export/wise.csv", "payouts-wise.csv"));

// ---------------------------------------------------------------- admin: tax forms
const FORM = { w9:"W-9", w8ben:"W-8BEN", w8bene:"W-8BEN-E", other:"Other" };
function renderTaxDocs(docs){
  const list = docs.tax_documents||[];
  setCount("tax", list.filter(d=>d.status==="pending"||d.status==="received").length);
  $("a-tax-table").style.display = list.length?"table":"none";
  $("a-tax-empty").style.display = list.length?"none":"block";
  const btn = (st, k, label, go)=>`<button class="mini${go?" go":""}" data-tx="${st}:${k}">${label}</button>`;
  const actions = d=>{
    const k = `${d.affiliate_id}:${d.doc_type}`;
    if(d.status==="pending"||d.status==="received") return btn("validated",k,"Approve",true)+btn("rejected",k,"Reject");
    if(d.status==="rejected") return btn("validated",k,"Approve")+btn("pending",k,"Re-open");
    return btn("rejected",k,"Reject")+btn("pending",k,"Re-open");
  };
  const label = {pending:"Waiting for the form", received:"Needs review", validated:"Approved", rejected:"Rejected"};
  $("a-taxdocs").innerHTML = list.map(d=>
    `<tr><td>${partnerCell(d.affiliate_id)}</td><td>${esc(FORM[d.doc_type]||d.doc_type)}</td>`+
    `<td>${badge(d.status, label[d.status]||d.status)}</td><td class="muted">${day(d.reviewed_at)}</td>`+
    `<td>${d.has_file?`<button class="mini" data-dl="${d.id}">Download</button>`:`<span class="muted">No file</span>`}</td><td class="nowrap">${actions(d)}</td></tr>`
  ).join("");
}
async function setTaxDoc(affiliateId, docType, status){
  try{ await post("/api/v1/admin/taxdocs", {affiliate_id:affiliateId, doc_type:docType, status}); msg("Tax form updated.","ok"); loadAdmin(); }
  catch(e){ msg("Couldn't update the tax form: "+errText(e),"err"); }
}
document.addEventListener("click", e=>{
  const t = e.target.closest("[data-tx]"), dl = e.target.closest("[data-dl]");
  if(t){ const [status, aff, type] = t.dataset.tx.split(":"); t.disabled = true; setTaxDoc(Number(aff), type, status); }
  if(dl) downloadAuthed(`/api/v1/admin/taxdocs/file?id=${Number(dl.dataset.dl)}`, `taxdoc-${Number(dl.dataset.dl)}.pdf`);
});
$("tx-record").addEventListener("click", ()=>{
  const aff = Number($("tx-aff").value);
  if(!(aff>0)){ msg("Choose a partner first.","err"); return; }
  setTaxDoc(aff, $("tx-type").value, "pending");
});

// ---------------------------------------------------------------- admin: settings
function renderSettings(s){
  $("pr-min").placeholder = toMajor(s.min_payout_cents);
  const n = s.notify || {}, mailOn = !!(s.setup && s.setup.mail);
  $("nt-off").hidden = mailOn;
  if(!dirty($("nt-out"))){
    $("nt-on").checked = !!n.enabled;
    $("nt-weekly").checked = !!n.weekly_report;
    $("nt-tips").checked = !!n.partner_tips;
    $("nt-recap").checked = !!n.partner_recap;
    if(document.activeElement!==$("nt-email")) $("nt-email").value = n.email || "";
    hint($("nt-out"), mailOn && (n.enabled || n.weekly_report) && !n.email ? "Add the address to send them to." : "");
  }
  if(dirty($("ps-out"))) return;
  if(document.activeElement!==$("ps-min")) $("ps-min").value = toMajor(s.min_payout_cents);
  if(document.activeElement!==$("ps-hold")) $("ps-hold").value = s.default_hold_days;
  $("pr-min").placeholder = toMajor(s.min_payout_cents);
}
// saveWith is the one pattern for every Save button: the button reads
// "Saving…" while the request runs, then the status right next to it says
// what happened, where the person is looking (a toast at the top of a tall
// page is easy to miss). fn returns the success text, or throws to show why
// it failed. Editing the form again marks it "Unsaved changes".
async function saveWith(btn, out, fn){
  const label = btn.dataset.label || (btn.dataset.label = btn.textContent);
  btn.disabled = true; btn.textContent = "Saving…";
  out.className = "muted"; out.textContent = "";
  try{
    const text = await fn();
    out.className = "saved"; out.textContent = "✓ " + (text || "Saved");
  }catch(e){
    out.className = "fail"; out.textContent = errText(e);
  }finally{ btn.disabled = false; btn.textContent = btn.dataset.label || label; }
}
function watchUnsaved(panel, out){
  const mark = e=>{ if(e.target.closest("[data-nosave]")) return; out.className = "unsaved"; out.textContent = "Unsaved changes"; };
  panel.addEventListener("input", mark);
  panel.addEventListener("change", mark);
}
// A status the person should see (saved, failed, unsaved) is not replaced
// by a background refresh; only a plain hint is.
// dirty: the panel has edits that aren't saved yet; a refresh must not
// overwrite them.
function dirty(out){ return out.className==="unsaved" || out.className==="fail"; }
function hint(out, text){ if(out.className==="muted" || !out.className) out.textContent = text; }

$("ps-save").addEventListener("click", ()=>saveWith($("ps-save"), $("ps-out"), async ()=>{
  const min = toMinor($("ps-min").value), hold = parseInt($("ps-hold").value,10);
  if(!(min>=0)) throw new Error("Enter a minimum payout of 0 or more");
  if(!(hold>=0 && hold<=365)) throw new Error("The hold must be 0 to 365 days");
  await api("/api/v1/admin/settings",{method:"PUT", body: JSON.stringify({min_payout_cents:min, default_hold_days:hold})});
  loadAdmin();
  return `Saved. Partners are paid once they're owed ${money(min)}`;
}));
watchUnsaved($("ps-save").closest(".panel"), $("ps-out"));

$("nt-save").addEventListener("click", ()=>saveWith($("nt-save"), $("nt-out"), async ()=>{
  const on = $("nt-on").checked, weekly = $("nt-weekly").checked, email = $("nt-email").value.trim();
  if((on || weekly) && !email){ $("nt-email").focus(); throw new Error("Enter the email address to send them to"); }
  await api("/api/v1/admin/settings",{method:"PUT", body: JSON.stringify({notify_on:on, weekly_report:weekly, partner_tips:$("nt-tips").checked, partner_recap:$("nt-recap").checked, notify_email:email})});
  return on || weekly ? `Saved. Your emails go to ${email}` : "Saved. We won't email you";
}));
watchUnsaved($("nt-panel"), $("nt-out"));

// Brand: program name and colour (program 1's branding). PUT replaces the
// whole record, so logo and portal domain are carried over from the read.
async function loadBrand(){
  try{ brand = await api("/api/v1/admin/branding?campaign_id=1"); }catch(e){ brand = null; return; }
  showLogo(brand.uploaded_logo || "");
  if(dirty($("br-out"))) return;
  if(document.activeElement!==$("br-name")) $("br-name").value = brand.brand_name||"";
  $("br-color").value = /^#[0-9a-fA-F]{6}$/.test(brand.accent_color||"") && brand.accent_color.toLowerCase()!=="#2563eb" ? brand.accent_color : "#111111";
  hint($("br-out"), brand.brand_name ? "" : "No name yet: partners see \"AffiliateTracking\" and emails say \"Main program\".");
  renderSetup();
}
$("br-save").addEventListener("click", ()=>saveWith($("br-save"), $("br-out"), async ()=>{
  const name = $("br-name").value.trim();
  if(!name){ $("br-name").focus(); throw new Error("Enter a program name"); }
  if(!csCamps.length) throw new Error("Save your commission first. That creates the program");
  const color = $("br-color").value;
  await api("/api/v1/admin/branding?campaign_id=1",{method:"PUT", body: JSON.stringify(Object.assign({}, brand||{}, {brand_name:name, accent_color: color.toLowerCase()==="#111111" ? "" : color}))});
  try{ sessionStorage.setItem("ae_brand_v", String(Date.now())); }catch(e){}
  document.querySelectorAll(".brand-name").forEach(el=>{ el.textContent = name; });
  document.documentElement.style.setProperty("--accent", color);
  document.title = document.title.replace(/[^·]*$/, " "+name);
  loadBrand();
  return `Saved. Partners now see "${name}"`;
}));
watchUnsaved($("br-save").closest(".panel"), $("br-out"));

// Logo: saved the moment a file is picked (its own status, not the Brand
// Save button). Big images and SVGs are redrawn as a 256px PNG in the
// browser first, so any logo file someone has just works.
function showLogo(url){
  const prev = $("br-logo-prev");
  prev.innerHTML = "";
  if(url){ const img = document.createElement("img"); img.alt = "Your logo"; img.src = url; prev.appendChild(img); }
  else { const mark = document.querySelector(".brand svg.mark"); if(mark) prev.appendChild(mark.cloneNode(true)).removeAttribute("hidden"); }
  const pick = $("br-logo-pick");
  pick.dataset.label = url ? "Replace logo" : "Upload logo";
  if(!pick.disabled) pick.textContent = pick.dataset.label;
  $("br-logo-rm").hidden = !url;
  if(window.AE_applyLogo) window.AE_applyLogo(url||"");
}
const LOGO_TYPES = ["image/png","image/jpeg","image/webp","image/gif"];
async function logoBlob(file){
  const url = URL.createObjectURL(file);
  try{
    const img = new Image();
    img.src = url;
    try{ await img.decode(); }catch(e){ throw new Error("That file isn't an image. Pick a PNG, JPG, WebP, GIF or SVG"); }
    const w = img.naturalWidth || 256, h = img.naturalHeight || 256;
    if(LOGO_TYPES.includes(file.type) && file.size <= 256*1024 && Math.max(w,h) <= 512) return file;
    const scale = 256 / Math.max(w,h);
    const c = document.createElement("canvas");
    c.width = Math.max(1, Math.round(w*scale)); c.height = Math.max(1, Math.round(h*scale));
    c.getContext("2d").drawImage(img, 0, 0, c.width, c.height);
    const out = await new Promise(r=>c.toBlob(r, "image/png"));
    if(!out) throw new Error("Couldn't read that image. Try a PNG");
    return out;
  }finally{ URL.revokeObjectURL(url); }
}
$("br-logo-pick").addEventListener("click", ()=>$("br-logo-file").click());
$("br-logo-file").addEventListener("change", ()=>{
  const file = $("br-logo-file").files[0];
  $("br-logo-file").value = "";
  if(!file) return;
  saveWith($("br-logo-pick"), $("br-logo-out"), async ()=>{
    if(!csCamps.length) throw new Error("Save your commission first. That creates the program");
    const blob = await logoBlob(file);
    const r = await api("/api/v1/admin/branding/logo?campaign_id=1", {method:"PUT", body: blob, headers: {"Content-Type": blob.type||"application/octet-stream"}});
    if(brand) brand.uploaded_logo = r.logo_url;
    showLogo(r.logo_url);
    return "Logo saved. Partners see it now";
  });
});
$("br-logo-rm").addEventListener("click", ()=>saveWith($("br-logo-rm"), $("br-logo-out"), async ()=>{
  await api("/api/v1/admin/branding/logo?campaign_id=1", {method:"DELETE"});
  if(brand) brand.uploaded_logo = "";
  showLogo("");
  return "Logo removed";
}));

// Commission: the campaign-default rule the engine applies is the active
// one with no partner and no product, at the highest priority.
let csCamps = [], csRules = [];
// rateLabel: "25% of each payment" or "$5.00 per payment".
function rateLabel(r){
  if(!r) return "";
  return r.rate_type==="percent" ? `${+(r.rate_value/100).toFixed(2)}% of each payment` : `${money(r.rate_value, r.currency)} per payment`;
}
function csDefault(campID){
  return csRules.filter(r=>r.campaign_id===campID && !r.affiliate_id && !r.product_key && r.active)
    .sort((a,b)=>b.priority-a.priority)[0] || null;
}
function renderCampaigns(camps, rules){
  csCamps = camps; csRules = rules;
  setCount("norate", !csCamps.length || csCamps.some(c=>!csDefault(c.id)));
  const sel = $("cs-camp"), keep = sel.value;
  sel.innerHTML = csCamps.length
    ? csCamps.map(c=>`<option value="${c.id}">${esc(c.name||("Program "+c.id))}</option>`).join("")
    : `<option value="new">Main program (new)</option>`;
  if(keep && csCamps.some(c=>String(c.id)===keep)) sel.value = keep;
  csShow();
}
function csShow(){
  const editing = document.activeElement && ["cs-rate","cs-months","cs-window","cs-hold","cs-type"].includes(document.activeElement.id);
  if(editing || dirty($("cs-out"))) return;
  const id = parseInt($("cs-camp").value,10);
  const c = csCamps.find(x=>x.id===id);
  if(!c){
    $("cs-months").value = 0; $("cs-window").value = 30; $("cs-hold").value = 30;
    $("cs-rate").value = ""; $("cs-type").value = "percent";
    hint($("cs-out"), "No program yet. Set a commission and press Save to create it.");
    return;
  }
  $("cs-months").value = c.commission_months;
  $("cs-window").value = c.attribution_window_days;
  $("cs-hold").value = c.net_terms_days;
  const r = csDefault(id);
  if(r){
    $("cs-type").value = r.rate_type;
    $("cs-rate").value = r.rate_type==="percent" ? (r.rate_value/100).toFixed(2).replace(/\.00$/,"") : toMajor(r.rate_value, r.currency).replace(/\.0+$/,"");
    hint($("cs-out"), "");
  }else{
    $("cs-rate").value = ""; $("cs-type").value = "percent";
    hint($("cs-out"), "No commission yet: sales in this program earn nothing until you set one.");
  }
}
$("cs-camp").addEventListener("change", csShow);
$("cs-save").addEventListener("click", ()=>saveWith($("cs-save"), $("cs-out"), async ()=>{
  let id = parseInt($("cs-camp").value,10);
  const months = parseInt($("cs-months").value,10), win = parseInt($("cs-window").value,10), hold = parseInt($("cs-hold").value,10);
  const type = $("cs-type").value, raw = parseFloat($("cs-rate").value);
  // percent is stored in basis points (30% = 3000), fixed in minor units of
  // the program currency ($5 = 500, ¥500 = 500)
  const value = type==="percent" ? Math.round(raw*100) : toMinor(raw);
  if(!(raw>0)){ $("cs-rate").focus(); throw new Error("Enter a commission above 0"); }
  if(type==="percent" && value>10000) throw new Error("A percentage commission can't be over 100%");
  if(!(months>=0 && months<=120)) throw new Error("Months must be 0 to 120 (0 = lifetime)");
  if(!(win>=1 && win<=365)) throw new Error("The cookie must be 1 to 365 days");
  if(!(hold>=0 && hold<=365)) throw new Error("The hold must be 0 to 365 days");
  if(!(id>0)){
    // Fresh install: create the first program. On an empty table it gets
    // id 1, the program billing webhooks credit by default.
    const made = await post("/api/v1/admin/campaigns", {name:"Main program", commission_months:months, attribution_window_days:win, net_terms_days:hold});
    id = made.id; $("cs-camp").innerHTML = `<option value="${id}">Main program</option>`;
  }else{
    await api(`/api/v1/admin/campaigns/${id}`,{method:"PATCH", body: JSON.stringify({commission_months:months, attribution_window_days:win, net_terms_days:hold})});
  }
  const cur = csDefault(id);
  if(!cur || cur.rate_type!==type || cur.rate_value!==value){
    // Rules are append-only: a new default at a higher priority wins.
    const prio = csRules.filter(r=>r.campaign_id===id && !r.affiliate_id && !r.product_key).reduce((m,r)=>Math.max(m,r.priority),0)+1;
    await post("/api/v1/admin/rules", {campaign_id:id, rate_type:type, rate_value:value, priority:prio});
  }
  if(document.activeElement) document.activeElement.blur();
  loadAdmin();
  return `Saved. Partners earn ${type==="percent"?raw+"% of each payment":money(value)+" per payment"}${months?` for ${months} ${plural(months,"month","months")} per customer`:" for the customer's lifetime"}`;
}));
watchUnsaved($("cs-save").closest(".panel"), $("cs-out"));

// Bonus tiers: the program's ladder (product-agnostic, program 1).
function renderTiers(list){
  const ts = list.filter(t=>t.active && !t.product_key).sort((a,b)=>a.threshold_minor-b.threshold_minor);
  $("tr-list").innerHTML = ts.length
    ? `<ul class="tiers">${ts.map(t=>`<li><span>${t.threshold_minor ? `At ${money(t.threshold_minor, t.currency)} in total ${esc(t.currency||state.cur)} sales` : "From their first sale"}: <b>${esc(rateLabel(t))}</b></span><button class="mini" type="button" data-deltier="${t.id}">Remove</button></li>`).join("")}</ul>`
    : `<div class="empty">No tiers yet. Everyone earns the program rate.</div>`;
}
$("tr-add").addEventListener("submit", e=>{
  e.preventDefault();
  saveWith($("tr-save"), $("tr-out"), async ()=>{
    // Thresholds and fixed amounts are in the program currency.
    const at = parseFloat($("tr-at").value), raw = parseFloat($("tr-rate").value), type = $("tr-type").value,
      value = type==="percent" ? Math.round(raw*100) : toMinor(raw);
    if(!(at>=0)){ $("tr-at").focus(); throw new Error("Enter the total sales amount, like 5000"); }
    if(!(raw>0)){ $("tr-rate").focus(); throw new Error("Enter what they earn, above 0"); }
    if(type==="percent" && value>10000) throw new Error("A percentage commission can't be over 100%");
    await post("/api/v1/admin/tiers", {campaign_id:1, threshold_minor:toMinor(at), rate_type:type, rate_value:value});
    $("tr-add").reset();
    await loadAdmin();
    return `Added. At ${money(toMinor(at))} in sales, partners earn ${rateLabel({rate_type:type, rate_value:value})}`;
  });
});
document.addEventListener("click", async e=>{
  const b = e.target.closest("[data-deltier]"); if(!b) return;
  if(!(await ask({title:"Remove this tier?", body:"New sales stop earning its rate. Commissions already earned keep theirs.", ok:"Remove tier", danger:true})).ok) return;
  try{ await api("/api/v1/admin/tiers?id="+b.dataset.deltier, {method:"DELETE"}); msg("Tier removed.","ok"); loadAdmin(); }
  catch(err){ msg(errText(err),"err"); }
});
watchUnsaved($("tr-panel"), $("tr-out"));

// ---------------------------------------------------------------- resources
// Admin: the list and the add form. Portal: each resource with the
// partner's own referral link filled in.
function showRsFields(){ const k = $("rs-kind").value; document.querySelectorAll("#rs-form .rs-for").forEach(el=>{ el.hidden = el.dataset.kind !== k; }); }
$("rs-kind").addEventListener("change", showRsFields);
async function loadResources(){
  let r; try{ r = await api("/api/v1/admin/resources"); }catch(e){ return; }
  const list = r.resources||[];
  $("rs-list").innerHTML = list.length ? `<ul class="rs-list">${list.map(x=>`<li>${x.kind==="image"?`<img src="${esc(x.image_url)}" alt="">`:""}<div><b>${esc(x.title)}</b>${
      x.kind==="text" ? `<span class="rs-body">${esc(x.body)}</span>` : x.kind==="link" ? `<span class="sub">${esc(x.body)}</span>` : `<span class="sub">Banner or logo</span>`
    }</div><button class="mini" type="button" data-delres="${x.id}">Remove</button></li>`).join("")}</ul>`
    : `<div class="empty">Nothing yet. Partners sell more with a banner and a few lines they can paste.</div>`;
}
$("rs-form").addEventListener("submit", e=>{
  e.preventDefault();
  saveWith($("rs-save"), $("rs-out"), async ()=>{
    const kind = $("rs-kind").value, title = $("rs-title").value.trim();
    if(!title){ $("rs-title").focus(); throw new Error("Give it a title"); }
    if(kind==="image"){
      const f = $("rs-file").files[0];
      if(!f) throw new Error("Choose an image");
      if(f.size > 2*1024*1024) throw new Error("Images must be 2 MB or smaller");
      await api("/api/v1/admin/resources/image?title="+encodeURIComponent(title), {method:"POST", body:f, headers:{"Content-Type": f.type||"application/octet-stream"}});
    }else{
      const body = kind==="text" ? $("rs-text").value.trim() : $("rs-url").value.trim();
      if(!body) throw new Error(kind==="text" ? "Write the text partners can copy" : "Enter the page address");
      await post("/api/v1/admin/resources", {kind, title, body});
    }
    $("rs-form").reset(); showRsFields(); loadResources();
    return "Added. Partners see it under Resources";
  });
});
document.addEventListener("click", async e=>{
  const b = e.target.closest("[data-delres]"); if(!b) return;
  if(!(await ask({title:"Remove this resource?", body:"Partners won't see it anymore. Banners they already put on their sites stop showing.", ok:"Remove", danger:true})).ok) return;
  try{ await api("/api/v1/admin/resources?id="+b.dataset.delres, {method:"DELETE"}); msg("Removed.","ok"); loadResources(); }
  catch(err){ msg(errText(err),"err"); }
});
watchUnsaved($("rs-panel"), $("rs-out"));

function refLink(dest){
  const base = `${location.origin}/r/${state.referral_code || state.affiliate_id}`;
  return dest ? `${base}?${new URLSearchParams({dest})}` : base;
}
async function loadPortalResources(){
  let r; try{ r = await api("/api/v1/affiliate/resources"); }catch(e){ return; }
  const list = r.resources||[], link = refLink("");
  $("rs-portal").innerHTML = !list.length ? `<div class="empty">The program hasn't added any resources yet. Your referral link works anywhere: <b>${esc(link)}</b></div>`
    : `<div class="rs-grid">${list.map((x,i)=>{
      if(x.kind==="text"){
        const text = x.body.split("{link}").join(link);
        return `<div class="rs-card"><h2>${esc(x.title)}</h2><div class="rs-body" id="rs-t${i}">${esc(text)}</div><div class="row"><button class="mini go" data-copy="rs-t${i}" data-what="Text copied">Copy text</button></div></div>`;
      }
      if(x.kind==="link"){
        const l = refLink(x.body);
        return `<div class="rs-card"><h2>${esc(x.title)}</h2><p class="muted">Sends people straight to ${esc(x.body)}.</p><div class="snip"><code id="rs-l${i}">${esc(l)}</code><button class="mini go" data-copy="rs-l${i}" data-what="Link copied">Copy link</button></div></div>`;
      }
      const img = location.origin + x.image_url, html = `<a href="${link}"><img src="${img}" alt="${x.title.replace(/"/g,"&quot;")}"></a>`;
      return `<div class="rs-card"><h2>${esc(x.title)}</h2><img src="${esc(x.image_url)}" alt="${esc(x.title)}"><p class="muted">Put it on your site with this code. It links to your referral link.</p><div class="snip"><code id="rs-h${i}">${esc(html)}</code><button class="mini go" data-copy="rs-h${i}" data-what="Code copied">Copy code</button></div><div class="row"><a class="mini" href="${esc(x.image_url)}" download>Download image</a></div></div>`;
    }).join("")}</div>`;
}

// ---------------------------------------------------------------- admin: import
function importForm(dry){
  const fd = new FormData();
  fd.append("source", $("im-src").value);
  if(dry) fd.append("dry_run","1");
  const add = (id, name)=>{ const f = $(id).files[0]; if(f) fd.append(name, f); };
  add("im-aff","affiliates"); add("im-ref","referrals"); add("im-com","commissions");
  return fd;
}
function renderImport(out){
  const r = out.report||{}, box = $("im-out");
  const n = (v)=>Number(v||0).toLocaleString();
  let h = `<b>${r.dry_run?"Review: nothing has been written yet":"Imported"}${r.batch_id?` as import #${r.batch_id}`:""}.</b> `;
  if(r.nothing_new) h += `Everything in these files is already here${r.previous_batch?` (import #${r.previous_batch})`:""}.`;
  h += `<table style="margin-top:8px"><tbody>`+
    `<tr><td>Rows read</td><td class="num">${n(r.rows)}</td></tr>`+
    `<tr><td>Partners ${r.dry_run?"to create":"created"}</td><td class="num">${n(r.affiliates_created)}</td></tr>`+
    `<tr><td>Links they answer to</td><td class="num">${n(r.links)}</td></tr>`+
    `<tr><td>Coupons</td><td class="num">${n(r.coupons)}</td></tr>`+
    `<tr><td>Customers linked (renewals keep earning)</td><td class="num">${n(r.customer_bindings)}</td></tr>`+
    `<tr><td>Rows skipped</td><td class="num">${n(r.skipped)}</td></tr></tbody></table>`;
  const conf = Object.entries(r.conflicts||{});
  if(conf.length){
    h += `<p style="margin-top:10px"><b>Check these first</b></p><ul style="margin-left:18px">`+conf.map(([k,v])=>
      `<li>${esc((out.conflict_text||{})[k]||k)}: ${n(v.count)}${(v.examples||[]).length?` <span class="muted">(${esc(v.examples.join(", "))})</span>`:""}</li>`).join("")+`</ul>`;
  }
  const fr = (r.fraud&&r.fraud.findings)||[];
  if(fr.length){
    h += `<p style="margin-top:10px"><b>Fraud found in this history</b></p><ul style="margin-left:18px">`+fr.map(f=>
      `<li>${esc(ruleName(f.rule))}: ${esc(f.subject)}, ${esc(f.reason)}</li>`).join("")+`</ul>`;
  }
  (r.warnings||[]).forEach(w=>{ h += `<p class="muted">${esc(w)}</p>`; });
  if(!r.dry_run && r.batch_id && out.link_param){
    h += `<p style="margin-top:10px"><b>Keep old links working:</b> use this tracking script on every page of your site instead of the plain one.</p>`+
      `<div class="snip"><code id="im-snip">${esc(`<script src="${location.origin}/t.js" data-param="${out.link_param}" async></script>`)}</code><button class="mini" data-copy="im-snip" data-what="Script copied">Copy</button></div>`;
  }
  box.innerHTML = h; box.style.display = "block";
}
function renderImports(out){
  const list = out.batches||[];
  $("im-table").style.display = list.length?"table":"none";
  $("im-empty").style.display = list.length?"none":"block";
  $("im-body").innerHTML = list.map(b=>
    `<tr><td class="mono">#${b.id}</td><td>${esc(b.source)}</td><td class="num">${b.affiliates}</td><td class="muted">${when(b.imported_at)}</td>`+
    `<td>${b.undone_at?`<span class="muted">Undone ${day(b.undone_at)}</span>`:`<button class="mini" data-undo="${b.id}">Undo</button>`}</td></tr>`
  ).join("");
}
document.addEventListener("click", async e=>{
  const btn = e.target.closest("[data-undo]"); if(!btn) return;
  if(!(await ask({title:`Undo import #${btn.dataset.undo}?`, body:"It removes the partners, links and coupons that import created.", ok:"Undo import", danger:true})).ok) return;
  btn.disabled = true;
  try{ await post("/api/v1/admin/import/undo", {batch_id:Number(btn.dataset.undo)}); msg(`Import #${btn.dataset.undo} undone.`,"ok"); loadAdmin(); }
  catch(err){ btn.disabled = false; msg("Couldn't undo: "+errText(err),"err"); }
});
(function(){
  const rv = $("im-review"), go = $("im-go");
  ["im-src","im-aff","im-ref","im-com"].forEach(id=>$(id).addEventListener("change", ()=>{ go.disabled = true; }));
  rv.addEventListener("click", async ()=>{
    if(!$("im-aff").files[0]){ msg("Choose the affiliates CSV first.","err"); return; }
    rv.disabled = true;
    try{ const out = await api("/api/v1/admin/import", {method:"POST", body: importForm(true)}); renderImport(out); go.disabled = !!(out.report&&out.report.nothing_new); }
    catch(e){ msg("Review failed: "+errText(e),"err"); }
    finally{ rv.disabled = false; }
  });
  go.addEventListener("click", async ()=>{
    if(go.disabled || !(await ask({title:"Import these files now?", body:"You can undo it afterwards from Past imports.", ok:"Import"})).ok) return;
    go.disabled = true;
    try{ const out = await api("/api/v1/admin/import", {method:"POST", body: importForm(false)}); renderImport(out); msg("Import done.","ok"); loadAdmin(); }
    catch(e){ go.disabled = false; msg("Import failed: "+errText(e),"err"); }
  });
})();

// ---------------------------------------------------------------- admin: advanced
$("ui-go").addEventListener("click", async ()=>{
  const cust = $("ui-cust").value.trim(), units = Number($("ui-units").value), type = $("ui-type").value.trim()||"tokens", rate = Number($("ui-rate").value);
  if(!cust||!(units>0)){ msg("Enter a customer ID and a number of units.","err"); return; }
  const now = new Date(), d = now.getUTCFullYear()*10000+(now.getUTCMonth()+1)*100+now.getUTCDate();
  try{
    const out = await post("/api/v1/admin/usage", {events:[{external_id:"ui-"+Date.now(), customer_id:cust, units, unit_type:type}], settle_day:d, settle_rate_bp:rate});
    const o = $("ui-out"); o.style.display="block";
    o.textContent = `Accepted ${out.accepted}, duplicates ${out.duplicates}, commissions settled ${out.settlements!=null?out.settlements:0}.`;
  }catch(e){ msg("Usage failed: "+errText(e),"err"); }
});
$("ml-go").addEventListener("click", async ()=>{
  const a = Number($("ml-a").value), s = Number($("ml-sponsor").value);
  if(!(a>0)||!(s>0)){ msg("Choose both partners.","err"); return; }
  if(a===s){ msg("A partner can't recruit themselves.","err"); return; }
  const who = id=>{ const x = state.affs.find(p=>p.id===id); return x ? (x.name||x.email) : `Partner ${id}`; };
  try{ await post("/api/v1/admin/mlm/place", {affiliate_id:a, sponsor_id:s}); const o = $("ml-out"); o.style.display="block"; o.textContent = `${who(a)} is now under ${who(s)}.`; }
  catch(e){ msg("Placement failed: "+errText(e),"err"); }
});

// ---------------------------------------------------------------- portal
// How you get paid: the partner's PayPal or Wise email, or bank details.
function showPayToFields(){
  const m = $("pt-method").value;
  document.querySelectorAll("#payto [data-for]").forEach(el=>{ el.hidden = el.dataset.for !== m; });
}
async function loadPayTo(){
  if(dirty($("pt-out"))) return;
  let d = null;
  try { d = (await api("/api/v1/affiliate/payout-details")).payout_details; } catch(e) { hint($("pt-out"), ""); return; }
  $("pt-method").value = d ? d.method : "paypal";
  $("pt-paypal").value = d && d.paypal_email || "";
  $("pt-wise").value = d && d.wise_email || "";
  $("pt-bank").value = d && d.bank || "";
  showPayToFields();
  hint($("pt-out"), d ? "" : "Not set yet. The program owner needs this to pay you.");
  // Not set: put it first on the page, where it can't be missed.
  const panel = $("payto"), first = panel.parentNode.querySelector(".panel");
  if(!d && first && first !== panel) first.before(panel);
  if(state.toPayTo){
    state.toPayTo = false;
    panel.scrollIntoView({block:"start"});
    $("pt-method").focus({preventScroll:true});
  }
}
$("pt-method").addEventListener("change", showPayToFields);
$("pt-save").addEventListener("click", ()=>saveWith($("pt-save"), $("pt-out"), async ()=>{
  const method = $("pt-method").value;
  const body = {method, paypal_email:$("pt-paypal").value.trim(), wise_email:$("pt-wise").value.trim(), bank:$("pt-bank").value.trim()};
  const d = (await api("/api/v1/affiliate/payout-details", {method:"PUT", body: JSON.stringify(body)})).payout_details || {};
  if(d.paypal_email) $("pt-paypal").value = d.paypal_email;
  if(d.wise_email) $("pt-wise").value = d.wise_email;
  const wait = d.cooling_until ? `. For your safety, payouts to the new details wait until ${day(d.cooling_until)}, and we emailed you about the change` : "";
  return (method==="bank" ? "Saved. Your bank details are stored encrypted" : `Saved. You'll be paid to ${d.paypal_email||d.wise_email}`) + wait;
}));
watchUnsaved($("payto"), $("pt-out"));

async function loadPortal(){
  let e;
  try { e = await api("/api/v1/affiliate/earnings"); }
  catch(err){ msg(errText(err),"err"); return; }
  state.affiliate_id = e.affiliate_id;
  state.referral_code = e.referral_code || "";
  $("p-link").textContent = location.origin + "/r/" + (state.referral_code || state.affiliate_id);
  const deal = e.deal || {};
  fetch("/api/v1/portal/brand").then(r=>r.json()).then(b=>{
    const t = termsText(b && b.terms);
    let text = deal.own_rate ? `Your deal: ${rateLabel(deal.own_rate)}, your own rate.`
      : t ? "Standard deal: " + t[0].replace(/^./, c=>c.toLowerCase()) + "." : "";
    // Bonus tiers: the next step up, and how far they are.
    const next = !deal.own_rate && (deal.tiers||[]).find(x=>x.threshold_minor > (deal.volume_minor||0));
    if(next) text += ` Earn more: at ${money(next.threshold_minor, deal.currency)} in total sales you'll earn ${rateLabel(next)}. You're at ${money(deal.volume_minor||0, deal.currency)}.`;
    if(!text) return;
    $("p-deal").textContent = text.trim();
    $("p-deal").hidden = false;
  }).catch(()=>{});

  const t = e.totals||{}, p = e.ledger_proof||{};
  // Every currency the partner earns in, side by side (never added up).
  state.cur = e.currency || state.cur;
  const curs = e.currencies||[], per = f=>curs.map(c=>({currency:c.currency, amount_minor:f(c)}));
  const has = curs.length > 0;
  $("p-cards").innerHTML =
    card("In hold", has ? moneyList(per(c=>(c.totals.pending||{}).cents||0)) : money(t.pending? t.pending.cents:0)) +
    card("Ready for your next payout", has ? moneyList(per(c=>c.owed_cents)) : money(p.owed_cents||0)) +
    card("Paid to you", has ? moneyList(per(c=>(c.totals.paid||{}).cents||0)) : money(t.paid? t.paid.cents:0)) +
    card("Earned per click, 30 days", money(e.epc_cents_30d));
  portalTrend.set(e.daily, e.currency);

  const ok = p.holds === true, pr = $("p-proof");
  pr.style.display = "block";
  pr.className = "proof"+(ok?"":" bad");
  pr.innerHTML = ok
    ? `✓ <b>Your balance checks out.</b> ${has ? moneyList(per(c=>c.owed_cents)) : money(p.owed_cents)} ready to pay equals what the program's books owe you. Commissions in hold join it when their hold ends.`
    : `⚠ <b>Your balance doesn't match the books.</b> Please tell the program owner.`;

  const tb = $("p-taxblock"), doc = e.payout_blocked_by_taxdoc;
  tb.style.display = doc ? "block" : "none";
  if(doc) tb.innerHTML = `⚠ <b>Your payouts are paused until your tax form is approved.</b> <a href="#/tax">Upload your ${esc(FORM[doc]||String(doc).toUpperCase())}</a>. You keep earning meanwhile.`;

  const rc = e.recent_commissions||[];
  $("p-comms").innerHTML = rc.length ? rc.map(c=>
    `<tr><td>${day(c.created_at)}</td><td class="num">${money(c.amount_cents, c.currency)}</td><td>${commissionBadge(c.status)}</td><td class="muted">${day(c.matures_at)}</td></tr>`
  ).join("") : none(4, "No commissions yet. Share your link above to earn your first.");

  loadPayTo();
  try {
    const ps = (await api("/api/v1/affiliate/payouts")).payouts || [];
    $("p-payouts").innerHTML = ps.length ? ps.map(x=>
      `<tr><td>${day(x.paid_at || x.created_at)}</td><td class="num">${money(x.amount_cents, x.currency)}</td><td>${esc(METHODS[x.method]||x.method)}</td><td>${x.status==="paid" ? badge("paid","Paid") : badge("pending","On its way")}</td></tr>`
    ).join("") : none(4, "No payouts yet. They show here once the program pays you.");
  } catch(err) { $("p-payouts").innerHTML = none(4, "Couldn't load your payouts. Refresh to try again."); }

  if(!(await loadLedger())) return;
  loadProgress(); loadTaxForms(); loadPortalResources();
}

// The partner's ledger, one currency at a time: each currency is its own
// account and balance (nothing is converted). With more than one, a
// labelled picker chooses which; cur "" is the API's default currency.
async function loadLedger(cur){
  let l;
  try { l = await api("/api/v1/affiliate/ledger" + (cur ? "?currency=" + encodeURIComponent(cur) : "")); }
  catch(err){ msg(errText(err),"err"); return false; }
  const curs = l.currencies||[], pick = $("p-ledger-cur");
  $("p-ledger-cur-wrap").hidden = curs.length < 2;
  if(curs.length > 1){
    pick.innerHTML = curs.map(c=>`<option value="${esc(c)}">${esc(c)}</option>`).join("");
    pick.value = l.currency;
  }
  // Ledger amounts are on the partner's payable account, where money owed
  // to them is a credit (negative). Shown flipped: owed is positive.
  let running = 0;
  const entries = l.entries||[];
  $("p-ledger").innerHTML = entries.length ? entries.map(x=>{
    running += x.amount_cents;
    return `<tr><td>${day(x.effective_at)}</td><td>${esc(LEDGER[x.source]||x.source)}${x.description?`<span class="sub">${esc(x.description)}</span>`:""}</td>`+
      `<td class="num">${money(-x.amount_cents, l.currency)}</td><td class="num">${money(-running, l.currency)}</td><td>${x.journal_sum_cents===0?`<span style="color:var(--green)">✓</span>`:`<span style="color:var(--red)">✗</span>`}</td></tr>`;
  }).join("") : none(5, "Nothing yet. Entries appear when a commission is ready to pay.");
  books((l.proof||{}).all_journals_sum_zero);
  return true;
}
$("p-ledger-cur").addEventListener("change", e=>loadLedger(e.target.value));

// link to a specific page
$("lg-go").addEventListener("click",()=>{
  const dest = $("lg-dest").value.trim();
  if(!/^https?:\/\//i.test(dest)){ msg("Paste a full page address, starting with https://","err"); return; }
  const params = new URLSearchParams({dest});
  if($("lg-sub").value.trim()) params.set("subid", $("lg-sub").value.trim());
  const url = `${location.origin}/r/${state.referral_code || state.affiliate_id}?${params}`;
  const out = $("lg-out"); out.textContent = url; out.style.display = "block";
  out.onclick = ()=>copy(url, "Link copied");
});

let lastLevel = null;
function toast(html){ const t = $("toast"); t.innerHTML = html; t.classList.add("show"); setTimeout(()=>t.classList.remove("show"), 4000); }
async function loadProgress(){
  try{
    const lb = await api("/api/v1/leaderboard");
    const rows = lb.leaderboard||[];
    $("g-lb").innerHTML = rows.length ? rows.map(r=>`<tr><td class="num">${r.rank}</td><td>${esc(r.name||("#"+r.affiliate_id))}</td><td class="num">${num(r.xp)}</td><td class="num">L${r.level}</td></tr>`).join("")
      : none(4, "No earners yet. Be the first.");
  }catch(e){ /* leaderboard is optional */ }
  try{
    const p = await api("/api/v1/affiliate/profile");
    // Level-up: the single hero moment. Fires once per gained level.
    if(lastLevel !== null && p.level > lastLevel){
      toast(`Level up: <span class="lvl">L${p.level}</span> reached`);
      $("g-cards").classList.add("levelup"); setTimeout(()=>$("g-cards").classList.remove("levelup"), 2200);
    }
    lastLevel = p.level;
    $("g-cards").innerHTML = card("Level", p.level) + card("XP", num(p.xp)) + card("Next level at", num(p.next_level_xp)+" XP") + card("Progress", (p.progress_percent||0)+"%");
    $("g-badges").innerHTML = (p.badges||[]).map(b=>`<span class="badge approved" title="${esc(b.name)}">${esc(b.icon)||"🏅"} ${esc(b.name)}</span>`).join(" ");
  }catch(e){ /* not available for this key */ }
}

// portal: tax form upload
async function loadTaxForms(){
  let out;
  try{ out = await api("/api/v1/affiliate/taxdocs"); }catch(e){ return; }
  const list = out.tax_documents||[];
  const label = {pending:"Waiting for your form", received:"Being reviewed", validated:"Approved", rejected:"Rejected: please upload again"};
  $("tf-table").style.display = list.length?"table":"none";
  $("tf-body").innerHTML = list.map(d=>
    `<tr><td>${esc(FORM[d.doc_type]||d.doc_type)}</td><td>${badge(d.status, label[d.status]||d.status)}</td>`+
    `<td class="muted">${d.file_name?esc(d.file_name):""}</td><td class="muted">${day(d.uploaded_at)}</td><td class="muted">${d.note?esc(d.note):""}</td></tr>`).join("");
}
$("tf-go").addEventListener("click", async ()=>{
  const go = $("tf-go"), f = $("tf-file").files[0];
  if(!f){ msg("Choose your PDF first.","err"); return; }
  if(f.size > 5*1024*1024){ msg("That file is over 5 MB. Please upload a smaller PDF.","err"); return; }
  const fd = new FormData();
  fd.append("doc_type", $("tf-type").value);
  fd.append("file", f);
  go.disabled = true;
  try{
    await api("/api/v1/affiliate/taxdocs", {method:"POST", body: fd});
    msg("Tax form uploaded. It's reviewed before your next payout.","ok");
    $("tf-file").value = "";
    loadTaxForms();
  }catch(e){ msg("Upload failed: "+errText(e),"err"); }
  finally{ go.disabled = false; }
});

// ---------------------------------------------------------------- refresh
function refresh(){
  if(document.body.classList.contains("apply-mode")) return;
  authUI();
  if(!key() || !state.role) return;
  if(state.view==="portal") loadPortal();
  else loadAdmin();
}
refresh();

// Live demo (AE_DEMO=1): boot.js learned the public read-only key from the
// brand payload. Use it, show the banner and open the admin pages.
function applyDemo(){
  const d = window.AE_DEMO;
  if(!d || state.demo) return;
  state.demo = true;
  document.body.classList.add("demo");
  $("signout").hidden = true;
  if(d.trial_url) $("demo-trial").href = d.trial_url;
  $("demo-banner").hidden = false;
  $("portal-label").textContent = "Partner view (demo)";
  signIn(d.key, "admin");
}
if(window.AE_DEMO) applyDemo(); else document.addEventListener("ae-demo", applyDemo);
