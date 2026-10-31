// White-label boot (FR-33): resolve this program's branding before the
// shell paints. Cosmetic only — every money number stays server-derived.
// Loaded from <head> as a same-origin file (CSP script-src 'self').
(function () {
  // applyLogo swaps the AffiliateTracking mark for the program's logo (or
  // back, for ""). A logo that fails to load keeps the mark.
  function applyLogo(url) {
    var ok = typeof url === "string" && (/^\/api\/v1\/portal\/logo\?/.test(url) || /^https:\/\//.test(url));
    var imgs = document.querySelectorAll("img.logo-img");
    for (var i = 0; i < imgs.length; i++) {
      (function (img) {
        var mark = img.previousElementSibling;
        var show = function (on) {
          img.hidden = !on;
          if (mark) { if (on) mark.setAttribute("hidden", ""); else mark.removeAttribute("hidden"); }
        };
        if (!ok) { img.removeAttribute("src"); show(false); return; }
        img.onload = function () { show(true); };
        img.onerror = function () { show(false); };
        if (img.getAttribute("src") !== url) img.src = url; else if (img.complete && img.naturalWidth) show(true);
      })(imgs[i]);
    }
  }
  window.AE_applyLogo = applyLogo;
  function whenReady(fn) {
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", fn);
    else fn();
  }
  try {
    var params = new URLSearchParams(location.search);
    var camp = params.get("campaign") || "1";
    var req = new XMLHttpRequest();
    // A brand saved in this tab skips the 60-second cache (app.js sets it).
    var v = "";
    try { v = sessionStorage.getItem("ae_brand_v") || ""; } catch (e) {}
    req.open("GET", "/api/v1/portal/brand?campaign_id=" + encodeURIComponent(camp) + (v ? "&v=" + encodeURIComponent(v) : ""), true);
    req.onreadystatechange = function () {
      if (req.readyState !== 4 || req.status !== 200) return;
      try {
        var b = JSON.parse(req.responseText);
        // Live demo install: hand app.js the public read-only key.
        if (b.demo && b.demo.key) {
          window.AE_DEMO = b.demo;
          document.dispatchEvent(new Event("ae-demo"));
        }
        // #2563eb is the engine's default when a program set no colour:
        // keep the dashboard's own ink then.
        if (b.accent_color && /^#[0-9a-fA-F]{6}$/.test(b.accent_color) && b.accent_color.toLowerCase() !== "#2563eb") {
          document.documentElement.style.setProperty("--accent", b.accent_color);
        }
        if (b.brand_name) { document.title = b.brand_name; }
        whenReady(function () {
          if (b.brand_name) {
            var names = document.querySelectorAll(".brand-name");
            for (var i = 0; i < names.length; i++) { names[i].textContent = b.brand_name; }
          }
          if (b.logo_url) applyLogo(b.logo_url);
        });
      } catch (e) { /* cosmetic — never block the app */ }
    };
    req.send(null);
  } catch (e) { /* cosmetic */ }
})();
