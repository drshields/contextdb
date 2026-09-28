/*
 * contextdb identity helper
 *
 * A companion tag for gtag.js. It does not replace analytics.js, it does not
 * declare a second source of truth, and it never concludes that two visits
 * belong to the same person. It reports what it saw; the server decides what
 * that means.
 *
 * What it captures, and why:
 *
 *   1. The session spine. Everyone instruments the click. gclid, wbraid,
 *      fbclid and the utm family arrive in the URL and get copied into GA4.
 *      Nobody instruments the path -- referrer chain, landing page, page
 *      sequence, dwell, scroll, and the gaps between visits. No single
 *      pageview identifies anybody; a four-step path does. That is the signal
 *      this tag exists to collect.
 *
 *   2. Coarse, session-scoped device characteristics. Deliberately bucketed
 *      and salted per site. This is NOT a cross-site fingerprint. A stable
 *      per-browser identifier is the category of thing being legislated out of
 *      existence, and an identity product built on one is the thing that gets
 *      shut down. These fields are weak corroboration and they expire.
 *
 * What it deliberately does not do:
 *
 *   - Set or read third-party cookies. Not required, and increasingly blocked.
 *   - Fingerprint across sites. See above.
 *   - Transmit anything on page load without a first-party subject key.
 *   - Decide identity. The server weights evidence; the browser reports it.
 *
 * Install:
 *
 *   <script async src="https://your-host/identity-helper.js"
 *           data-ctxdb-endpoint="https://your-host/v1/identity/ingest"></script>
 *
 * It can be loaded before or after gtag.js. Order does not matter; they do not
 * interact. If you serve it from the same contextdb binary, the only moving
 * part is the endpoint URL.
 */
(function () {
  "use strict";

  var script = document.currentScript ||
    (function () {
      var all = document.getElementsByTagName("script");
      for (var i = all.length - 1; i >= 0; i--) {
        if ((all[i].src || "").indexOf("identity-helper") !== -1) return all[i];
      }
      return null;
    })();

  var ENDPOINT =
    (script && script.getAttribute("data-ctxdb-endpoint")) || "/v1/identity/ingest";
  var SITE = (script && script.getAttribute("data-ctxdb-site")) || location.hostname;
  var SALT = SITE;

  var SUBJECT_KEY = "ctxdb_subject";
  var SESSION_KEY = "ctxdb_session";
  var SESSION_TTL_MS = 30 * 60 * 1000;

  /* ---------------------------------------------------------------- utils */

  function readCookie(name) {
    var match = document.cookie.match(new RegExp("(^|;\\s*)" + name + "=([^;]*)"));
    return match ? decodeURIComponent(match[2]) : null;
  }

  function writeCookie(name, value, ttlMs) {
    var expires = new Date(Date.now() + ttlMs).toUTCString();
    var secure = location.protocol === "https:" ? "; Secure" : "";
    // SameSite=Lax keeps this first-party. Lax rather than Strict so a click
    // from an external referrer still carries the subject key -- which is
    // exactly the return visit that matters.
    document.cookie =
      name + "=" + encodeURIComponent(value) +
      "; Path=/; Max-Age=" + Math.floor(ttlMs / 1000) +
      "; SameSite=Lax" + secure;
  }

  function uuid() {
    if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
    var bytes = new Uint8Array(16);
    (window.crypto || {}).getRandomValues
      ? crypto.getRandomValues(bytes)
      : bytes.forEach(function (_, i) { bytes[i] = Math.floor(Math.random() * 256); });
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    var hex = [];
    for (var i = 0; i < 16; i++) hex.push(("0" + bytes[i].toString(16)).slice(-2));
    return (
      hex.slice(0, 4).join("") + "-" + hex.slice(4, 6).join("") + "-" +
      hex.slice(6, 8).join("") + "-" + hex.slice(8, 10).join("") + "-" +
      hex.slice(10, 16).join("")
    );
  }

  // Cheap non-cryptographic hash, used only to bucket coarse characteristics
  // before they leave the browser. It exists to reduce the entropy of what we
  // transmit, not to provide integrity.
  function bucket(value, salt) {
    var str = String(value == null ? "" : value) + "|" + salt;
    var h = 5381;
    for (var i = 0; i < str.length; i++) {
      h = ((h << 5) + h + str.charCodeAt(i)) >>> 0;
    }
    return h.toString(36).slice(0, 6);
  }

  function pathOnly(url) {
    try {
      var u = new URL(url, location.href);
      return (u.pathname + u.search).slice(0, 512);
    } catch (e) {
      return String(url || "").slice(0, 512);
    }
  }

  function hostOnly(url) {
    if (!url) return "";
    try {
      return new URL(url, location.href).hostname.toLowerCase();
    } catch (e) {
      return "";
    }
  }

  /* ------------------------------------------------------------- lifecycle */

  var session = null;

  function loadSession() {
    var raw = null;
    try { raw = sessionStorage.getItem(SESSION_KEY); } catch (e) { /* private mode */ }
    if (raw) {
      try {
        var parsed = JSON.parse(raw);
        if (parsed && Date.now() - parsed.last < SESSION_TTL_MS) {
          parsed.last = Date.now();
          return parsed;
        }
      } catch (e) { /* fall through to a new session */ }
    }
    return {
      id: uuid(),
      started: Date.now(),
      last: Date.now(),
      entryPath: pathOnly(location.href),
      entryReferrer: document.referrer || "",
      referrerChain: [],
      pageviews: []
    };
  }

  function saveSession() {
    try { sessionStorage.setItem(SESSION_KEY, JSON.stringify(session)); } catch (e) { /* no-op */ }
  }

  function subject() {
    var key = readCookie(SUBJECT_KEY);
    if (!key) {
      key = uuid();
      writeCookie(SUBJECT_KEY, key, 365 * 24 * 60 * 60 * 1000);
    }
    return key;
  }

  /* ------------------------------------------------------------- extraction */

  var CLICK_PARAMS = [
    "gclid", "gbraid", "wbraid", "dclid", "fbclid", "ttclid", "msclkid",
    "li_fat_id", "epik", "irclickid", "rdt_cid", "sccid", "yclid"
  ];
  var UTM_PARAMS = ["utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content"];

  function query() {
    var out = {};
    try {
      var params = new URLSearchParams(location.search);
      CLICK_PARAMS.concat(UTM_PARAMS).forEach(function (k) {
        var v = params.get(k);
        if (v) out[k] = v;
      });
    } catch (e) { /* no-op */ }
    return out;
  }

  function coarseSignal() {
    var nav = navigator || {};
    var conn = nav.connection || {};
    return {
      // Bucketed, not raw. A raw screen resolution across sites is a
      // fingerprint primitive; a bucket salted per site is not.
      screen_bucket: bucket(screen.width + "x" + screen.height + "x" + (screen.colorDepth || 0), SALT),
      lang_bucket: bucket((nav.languages && nav.languages[0]) || nav.language || "", SALT),
      timezone_offset: -new Date().getTimezoneOffset(),
      conn_type: conn.effectiveType || conn.type || "",
      hardware_cores: nav.hardwareConcurrency || 0,
      // Platform class, not a fingerprinting surface: no canvas, no audio,
      // no font enumeration, no plugin list. Those are the techniques that
      // make cross-site tracking work and the ones that are being banned.
      platform: /android/i.test(nav.userAgent || "") ? "android"
              : /iphone|ipad|ipod/i.test(nav.userAgent || "") ? "ios"
              : /mac/i.test(nav.userAgent || "") ? "macos"
              : /win/i.test(nav.userAgent || "") ? "windows"
              : /linux/i.test(nav.userAgent || "") ? "linux" : "other"
    };
  }

  function consent() {
    // Mirrors the GA4 consent default. Respects an explicit denial by
    // degrading rather than blocking: the session spine is still first-party
    // behaviour, and pretending we have no linkage when we do would be worse
    // than recording that consent was withheld.
    var granted = true;
    try {
      var w = window;
      if (w.dataLayer) {
        for (var i = 0; i < w.dataLayer.length; i++) {
          var entry = w.dataLayer[i];
          if (entry && entry.gtag && entry.gtag[0] === "consent" && entry.gtag[1] === "update") {
            granted = !!(entry.gtag[2] && entry.gtag[2].analytics_storage);
          }
        }
      }
    } catch (e) { /* default to granted, matching GA4 */ }
    return { analytics: granted };
  }

  /* ------------------------------------------------------------ observation */

  var current = null;

  function onPageview() {
    var params = query();
    var pv = {
      url: pathOnly(location.href),
      path: location.pathname,
      referrer: document.referrer || "",
      referrer_host: current ? current.refHost : "",
      title: document.title || "",
      at: new Date().toISOString(),
      dwell_ms: current ? Date.now() - current.startedAt : 0,
      scroll_pct: current ? current.maxScroll : 0,
      viewport_w: window.innerWidth,
      viewport_h: window.innerHeight
    };
    session.pageviews.push(pv);
    current = { startedAt: Date.now(), maxScroll: 0, refHost: hostOnly(document.referrer) };
    session.last = Date.now();
    saveSession();
    return params;
  }

  function trackScroll() {
    if (!current) return;
    var doc = document.documentElement;
    var height = (doc.scrollHeight || 0) - window.innerHeight;
    var pct = height > 0 ? Math.round((window.scrollY / height) * 100) : 100;
    if (pct > current.maxScroll) current.maxScroll = Math.min(100, pct);
  }

  /* ----------------------------------------------------------------- send */

  var queued = false;

  function flush() {
    if (queued) return;
    queued = true;

    var params = onPageview();
    if (!session.pageviews.length) session.pageviews = [{ path: location.pathname, at: new Date().toISOString() }];

    var clickIds = {};
    var utm = {};
    CLICK_PARAMS.forEach(function (k) { if (params[k]) clickIds[k] = params[k]; });
    UTM_PARAMS.forEach(function (k) { if (params[k]) utm[k] = params[k]; });

    var payload = {
      site: SITE,
      subject: subject(),
      session: {
        id: session.id,
        started_at: new Date(session.started).toISOString(),
        ended_at: new Date().toISOString(),
        entry_path: session.entryPath,
        entry_referrer: session.entryReferrer,
        pageviews: session.pageviews,
        referrer_chain: session.referrerChain
      },
      click_ids: Object.keys(clickIds).length ? clickIds : undefined,
      utm: Object.keys(utm).length ? utm : undefined,
      signal: coarseSignal(),
      consent: consent(),
      agent: navigator.userAgent ? navigator.userAgent.slice(0, 200) : ""
    };

    // sendBeacon survives page unload, which is exactly when the most
    // interesting pageviews (the ones that end a session) are lost.
    var body = JSON.stringify(payload);
    try {
      if (navigator.sendBeacon) {
        navigator.sendBeacon(ENDPOINT, new Blob([body], { type: "application/json" }));
        resetSession();
        return;
      }
    } catch (e) { /* fall through to fetch */ }

    fetch(ENDPOINT, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: body,
      keepalive: true,
      mode: "cors",
      credentials: "omit"
    }).catch(function () { /* never break the host page */ })
      .then(resetSession);
  }

  function resetSession() {
    queued = false;
    session = loadSession();
    saveSession();
  }

  /* ------------------------------------------------------------------ boot */

  function boot() {
    if (window.__ctxdbIdentity) return;
    window.__ctxdbIdentity = true;

    session = loadSession();
    var ref = hostOnly(document.referrer);
    if (ref && ref !== hostOnly(location.origin)) {
      session.referrerChain.push(ref);
    }
    current = { startedAt: Date.now(), maxScroll: 0, refHost: ref };
    saveSession();

    if (window.scroll) {
      window.addEventListener("scroll", trackScroll, { passive: true });
    }
    // visibilitychange is a better session boundary than unload: it fires on
    // tab switches and backgrounding, which is where the long gaps open up.
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "hidden") {
        trackScroll();
        saveSession();
      }
    });
    window.addEventListener("pagehide", function () {
      trackScroll();
      saveSession();
      flush();
    });

    flush();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }

  window.ctxdbIdentity = {
    subject: subject,
    flush: flush,
    // Exposed so a site can send a deliberate high-value signal (an identified
    // CRM match, a consent change, a support chat) through the same pipeline
    // instead of bolting on another vendor.
    //
    // The mark travels from an untrusted channel, so the server weighs it at
    // WeightMark (0.5) and only promotes it to WeightMarkCorroborated (0.85)
    // on a repeat. Repeating the same mark does not keep raising it. A mark
    // should therefore carry a claim you can afford to be wrong about.
    mark: function (kind, detail) {
      var params = query();
      var utm = pick(params, UTM_PARAMS);
      var payload = {
        site: SITE,
        subject: subject(),
        session: {
          id: session.id,
          started_at: new Date(session.started).toISOString(),
          ended_at: new Date().toISOString(),
          entry_path: session.entryPath,
          entry_referrer: session.entryReferrer,
          pageviews: session.pageviews,
          referrer_chain: session.referrerChain
        },
        utm: Object.keys(utm).length ? utm : undefined,
        consent: consent(),
        agent: navigator.userAgent ? navigator.userAgent.slice(0, 200) : "",
        mark: { kind: String(kind), detail: detail || null }
      };
      return fetch(ENDPOINT, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
        mode: "cors",
        credentials: "omit"
      }).catch(function () {});
    }
  };

  function pick(src, keys) {
    var out = {};
    keys.forEach(function (k) { if (src[k]) out[k] = src[k]; });
    return out;
  }
})();
