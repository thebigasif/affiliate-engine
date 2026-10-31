// Test harness for t.js (driven by tracker_test.go): runs the script
// against a minimal fake browser described by the SCENARIO env var (JSON)
// and prints what it did as JSON. Only the DOM surface t.js touches is
// faked.
"use strict";
const fs = require("fs");
const vm = require("vm");

const sc = JSON.parse(process.env.SCENARIO);
const out = { replaced: null, replaceState: null, state: null, cookieWrites: [], errors: [] };

const loc = new URL(sc.href);
const location = {
  get protocol() { return loc.protocol; },
  get host() { return loc.host; },
  get hostname() { return loc.hostname; },
  get pathname() { return loc.pathname; },
  get search() { return loc.search; },
  get hash() { return loc.hash; },
  get href() { return loc.href; },
  replace(u) { out.replaced = String(u); },
};

// Cookie jar: honours max-age<=0 as a delete; attributes are recorded.
const jar = new Map();
for (const part of (sc.cookie || "").split(";")) {
  const p = part.trim();
  if (!p) continue;
  const eq = p.indexOf("=");
  jar.set(p.slice(0, eq), p.slice(eq + 1));
}

const session = new Map(Object.entries(sc.session || {}));
const sessionStorage = {
  getItem(k) { if (sc.sessionThrows) throw new Error("SecurityError"); return session.has(k) ? session.get(k) : null; },
  setItem(k, v) { if (sc.sessionThrows) throw new Error("SecurityError"); session.set(k, String(v)); },
};

function anchor(href) {
  const u = new URL(href);
  return {
    get hostname() { return u.hostname; },
    get pathname() { return u.pathname; },
    get search() { return u.search; },
    set search(v) { u.search = v; },
    get href() { return u.href; },
  };
}

function element(tag, attrs) {
  const a = Object.assign({}, attrs || {});
  return {
    tag,
    attrs: a,
    getAttribute(n) { return Object.prototype.hasOwnProperty.call(a, n) ? a[n] : null; },
    setAttribute(n, v) { a[n] = String(v); },
  };
}

const script = {
  src: sc.scriptSrc,
  getAttribute(n) { return n === "data-param" && sc.dataParam != null ? sc.dataParam : null; },
};
const links = (sc.links || []).map(anchor);
const tags = (sc.tags || []).map((t) => element(t.tag, t.attrs));
const timers = [];
const observers = [];
const listeners = [];

const document = {
  readyState: sc.readyState || "complete",
  currentScript: sc.noCurrentScript ? null : script,
  documentElement: {},
  get cookie() {
    if (sc.cookieThrows) throw new Error("SecurityError");
    return Array.from(jar, ([k, v]) => k + "=" + v).join("; ");
  },
  set cookie(v) {
    if (sc.cookieThrows) throw new Error("SecurityError");
    out.cookieWrites.push(v);
    const [pair, ...attrs] = v.split(";");
    const eq = pair.indexOf("=");
    const name = pair.slice(0, eq).trim();
    const ma = attrs.map((a) => a.trim()).find((a) => a.toLowerCase().startsWith("max-age="));
    if (ma && Number(ma.slice(8)) <= 0) jar.delete(name);
    else jar.set(name, pair.slice(eq + 1));
  },
  getElementsByTagName(n) {
    if (n === "a") return links;
    return [];
  },
  querySelectorAll(sel) {
    const want = sel.split(",").map((s) => s.trim());
    return tags.filter((t) => want.includes(t.tag));
  },
  addEventListener(type, fn) { listeners.push(fn); },
};

const window = {
  location,
  document,
  history: {
    state: { spa: 1 },
    replaceState(s, _t, u) { out.replaceState = String(u); out.state = s; },
  },
  get sessionStorage() { return sessionStorage; },
  atob: (s) => Buffer.from(s, "base64").toString("binary"),
  setTimeout(fn) { timers.push(fn); return timers.length; },
  addEventListener(type, fn) { listeners.push(fn); },
  MutationObserver: function (cb) { observers.push(cb); this.observe = function () {}; },
};
window.top = sc.framed ? {} : window;

const code = fs.readFileSync(sc.script, "utf8");
const ctx = vm.createContext({ window, document });
function run() {
  try { vm.runInContext(code, ctx); } catch (e) { out.errors.push(String(e && e.stack || e)); }
}
run();
if (sc.runTwice) run();
for (const fn of listeners) fn();

if (sc.addLater) {
  for (const h of sc.addLater.links || []) links.push(anchor(h));
  for (const t of sc.addLater.tags || []) tags.push(element(t.tag, t.attrs));
  for (const cb of observers) cb([]);
}
while (timers.length) timers.shift()();

const api = window.AffiliateTracking;
try {
  out.api = api ? { token: api.token(), clientReferenceId: api.clientReferenceId() } : null;
} catch (e) { out.errors.push("api: " + String(e)); }
out.jar = Object.fromEntries(jar);
out.session = Object.fromEntries(session);
out.links = links.map((a) => a.href);
out.tags = tags.map((t) => t.attrs);
out.observers = observers.length;
process.stdout.write(JSON.stringify(out));
