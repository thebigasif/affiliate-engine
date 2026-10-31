/* AffiliateTracking first-party script, served at /t.js.
   One line on every page of the merchant's site:
     <script src="https://acme.affiliatetracking.co/t.js" async></script>
   It keeps the attribution token /r/ hands over (?aff_token=) in a
   first-party cookie, sends rival-tracker links (?via=code) through /r/
   so they record a real click, and hands the token to Stripe's no-code
   checkouts as client_reference_id and to Lemon Squeezy checkout links as
   checkout[custom][token] and to Polar checkout links as reference_id. Plain ES5, no dependencies. Every
   step is guarded: this script must never break the page it runs on. */
(function (w, d) {
  "use strict";
  var COOKIE = "at_token";
  var URL_PARAM = "aff_token";
  var REF_PREFIX = "at1_";
  var DEFAULT_TTL = 30 * 24 * 3600; // seconds, when the token's exp is unreadable
  var TOKEN_RE = /^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$/;
  var CODE_RE = /^[A-Za-z0-9_-]{1,64}$/;
  var NAME_RE = /^[A-Za-z0-9_-]{1,32}$/;
  var STRIPE_HOST = "buy.stripe.com";
  var STRIPE_TAGS = "stripe-pricing-table,stripe-buy-button";
  var LS_HOST_RE = /(^|\.)lemonsqueezy\.com$/;
  var LS_PATH = "/checkout/buy/";
  var LS_PARAM = "checkout[custom][token]";
  var POLAR_PARAM = "reference_id";

  function now() { return Math.floor(new Date().getTime() / 1000); }

  function dec(s) {
    try { return decodeURIComponent(s.replace(/\+/g, " ")); } catch (e) { return null; }
  }

  // parseQuery splits "?a=1&b=2" into raw pairs, keeping the raw text so a
  // rebuilt query changes nothing but the removed keys.
  function parseQuery(search) {
    var s = search && search.charAt(0) === "?" ? search.slice(1) : (search || "");
    var out = [];
    if (!s) return out;
    var parts = s.split("&");
    for (var i = 0; i < parts.length; i++) {
      if (!parts[i]) continue;
      var eq = parts[i].indexOf("=");
      var k = eq < 0 ? parts[i] : parts[i].slice(0, eq);
      var v = eq < 0 ? "" : parts[i].slice(eq + 1);
      out.push({ raw: parts[i], key: dec(k), val: dec(v) });
    }
    return out;
  }

  function getParam(pairs, name) {
    for (var i = 0; i < pairs.length; i++) {
      if (pairs[i].key === name) return pairs[i].val || "";
    }
    return "";
  }

  function hasParam(pairs, name) {
    for (var i = 0; i < pairs.length; i++) {
      if (pairs[i].key === name) return true;
    }
    return false;
  }

  // withoutParam returns search minus every pair named name ("" or "?...").
  function withoutParam(search, name) {
    var pairs = parseQuery(search), kept = [];
    for (var i = 0; i < pairs.length; i++) {
      if (pairs[i].key !== name) kept.push(pairs[i].raw);
    }
    return kept.length ? "?" + kept.join("&") : "";
  }

  // tokenExp reads exp (unix seconds) from the token's base64url JSON
  // payload. The signature is the engine's business; this is only the
  // cookie lifetime. 0 = unreadable.
  function tokenExp(tok) {
    try {
      var p = tok.split(".")[0].replace(/-/g, "+").replace(/_/g, "/");
      while (p.length % 4) p += "=";
      var o = JSON.parse(w.atob(p));
      return o && typeof o.exp === "number" && o.exp > 0 ? Math.floor(o.exp) : 0;
    } catch (e) { return 0; }
  }

  function storeToken(tok) {
    var exp = tokenExp(tok), maxAge = DEFAULT_TTL;
    if (exp) {
      maxAge = exp - now();
      if (maxAge <= 0) return;
    }
    var c = COOKIE + "=" + tok + "; path=/; max-age=" + maxAge + "; SameSite=Lax";
    if (w.location.protocol === "https:") c += "; Secure";
    try { d.cookie = c; } catch (e) {}
  }

  var memToken = ""; // this page's token when cookies are blocked

  function token() {
    try {
      var parts = (d.cookie || "").split(";");
      for (var i = 0; i < parts.length; i++) {
        var c = parts[i].replace(/^\s+/, "");
        if (c.indexOf(COOKIE + "=") !== 0) continue;
        var v = c.slice(COOKIE.length + 1);
        if (!TOKEN_RE.test(v)) continue;
        var exp = tokenExp(v);
        if (exp && exp <= now()) continue;
        return v;
      }
    } catch (e) {}
    return memToken && !(tokenExp(memToken) && tokenExp(memToken) <= now()) ? memToken : "";
  }

  // clientReferenceId mirrors tokens.ClientReferenceID in the engine:
  // Stripe allows only [A-Za-z0-9_-], so "payload.sig" becomes
  // "at1_payload_sig" (the signature is always the last 43 chars).
  function clientReferenceId() {
    var t = token();
    if (!t) return "";
    var i = t.lastIndexOf(".");
    var ref = REF_PREFIX + t.slice(0, i) + "_" + t.slice(i + 1);
    return ref.length <= 200 ? ref : "";
  }

  // thisScript is the running tag. No guessing by src when the browser
  // cannot say (another site's /t.js could match): no tag, no bounce.
  function thisScript() {
    var s = d.currentScript;
    return s && s.src ? s : null;
  }

  // originOf is the engine's own origin: navigation only ever goes there.
  function originOf(src) {
    var m = /^(https?:)\/\/(?:[^\/?#@]*@)?([^\/?#@]+)/i.exec(src || "");
    return m ? m[1].toLowerCase() + "//" + m[2].toLowerCase() : "";
  }

  function paramNames(script) {
    var raw = (script && script.getAttribute("data-param")) || "via";
    var parts = raw.split(","), out = [];
    for (var i = 0; i < parts.length; i++) {
      var n = parts[i].replace(/^\s+|\s+$/g, "");
      if (NAME_RE.test(n) && n !== URL_PARAM) out.push(n);
    }
    return out;
  }

  function isTop() {
    try { return w.top === w; } catch (e) { return false; }
  }

  // bounce sends a rival link (?via=code) through the engine's /r/ so it
  // records a real click; /r/ comes back to this page with aff_token. The
  // param is dropped from the way back, and sessionStorage blocks a
  // second bounce for the same code (no storage, no bounce).
  function bounce(origin, name, code) {
    var loc = w.location;
    var key = "at_bounced:" + name + "=" + code.toLowerCase();
    try {
      if (w.sessionStorage.getItem(key)) return false;
      w.sessionStorage.setItem(key, "1");
    } catch (e) { return false; }
    var dest = loc.protocol + "//" + loc.host + loc.pathname + withoutParam(loc.search, name) + loc.hash;
    loc.replace(origin + "/r/" + encodeURIComponent(code) + "?dest=" + encodeURIComponent(dest));
    return true;
  }

  function addParam(a, name, value) {
    if (hasParam(parseQuery(a.search), name)) return; // the merchant's own value wins
    var s = a.search && a.search !== "?" ? a.search + "&" : "?";
    a.search = s + encodeURIComponent(name) + "=" + encodeURIComponent(value);
  }

  // Polar checkout links: buy.polar.sh/polar_cl_..., or the API's
  // /v1/checkout-links/.../redirect form (sandbox included).
  function isPolarCheckout(host, path) {
    if (host === "buy.polar.sh") return path.indexOf("/polar_cl_") === 0;
    return (host === "api.polar.sh" || host === "sandbox-api.polar.sh") && path.indexOf("/v1/checkout-links/") === 0;
  }

  function decorateLink(a, ref, tok) {
    try {
      var host = String(a.hostname).toLowerCase();
      if (host === STRIPE_HOST) addParam(a, "client_reference_id", ref);
      else if (LS_HOST_RE.test(host) && String(a.pathname).indexOf(LS_PATH) === 0) addParam(a, LS_PARAM, tok);
      else if (isPolarCheckout(host, String(a.pathname))) addParam(a, POLAR_PARAM, tok);
    } catch (e) {}
  }

  function decorate() {
    try {
      var tok = token(), ref = clientReferenceId();
      if (!tok || !ref) return;
      var links = d.getElementsByTagName("a");
      for (var i = 0; i < links.length; i++) decorateLink(links[i], ref, tok);
      var els = d.querySelectorAll ? d.querySelectorAll(STRIPE_TAGS) : [];
      for (var j = 0; j < els.length; j++) {
        if (!els[j].getAttribute("client-reference-id")) els[j].setAttribute("client-reference-id", ref);
      }
    } catch (e) {}
  }

  var pending = false;
  function decorateSoon() {
    if (pending) return;
    pending = true;
    w.setTimeout(function () { pending = false; decorate(); }, 50);
  }

  try {
    if (w.__atTracker) return; // included twice: the first copy runs
    w.__atTracker = true;
    w.AffiliateTracking = { token: token, clientReferenceId: clientReferenceId };

    var loc = w.location;
    var pairs = parseQuery(loc.search);
    var incoming = getParam(pairs, URL_PARAM);
    if (hasParam(pairs, URL_PARAM)) {
      if (TOKEN_RE.test(incoming)) {
        memToken = incoming;
        storeToken(incoming);
      }
      try {
        if (w.history && w.history.replaceState) {
          w.history.replaceState(w.history.state, "", loc.pathname + withoutParam(loc.search, URL_PARAM) + loc.hash);
        }
      } catch (e) {}
    } else if (isTop()) {
      var script = thisScript();
      var origin = originOf(script && script.src);
      var names = paramNames(script);
      for (var i = 0; origin && i < names.length; i++) {
        var code = getParam(pairs, names[i]);
        if (code && CODE_RE.test(code)) {
          if (bounce(origin, names[i], code)) return;
          break;
        }
      }
    }

    if (!token()) return; // nothing to hand to a checkout
    decorate();
    if (d.readyState === "loading" && d.addEventListener) d.addEventListener("DOMContentLoaded", decorate);
    if (w.addEventListener) w.addEventListener("load", decorate);
    if (w.MutationObserver && d.documentElement) {
      new w.MutationObserver(decorateSoon).observe(d.documentElement, { childList: true, subtree: true });
    }
  } catch (e) {}
})(window, document);
