// Queue drill-down (#598): after "Show more" replaces its own row, focus would
// fall to <body>; move it to the first appended row. A file, not inline (CSP).
(function () {
  "use strict";

  // A filter select applies on change; without JS the form's button submits it.
  // The reload drops focus to <body>, so the select's id is remembered across it
  // (sessionStorage, optional: the page works without it) and refocused on load.
  // The entry names the page and the time: a submit that never lands on a queue
  // page (error page, Stop) leaves it behind, and it must not move focus on some
  // later, unrelated page.
  var FOCUS_KEY = "mx-queue-refocus";
  var FOCUS_MAX_AGE_MS = 5000;

  document.addEventListener("change", function (evt) {
    var el = evt.target;
    if (!el || !el.hasAttribute || !el.hasAttribute("data-queue-autosubmit")) return;
    if (!el.form) {
      console.error("queue.js: [data-queue-autosubmit] control has no form", el.id || el.name);
      return;
    }
    try {
      if (el.id) {
        window.sessionStorage.setItem(
          FOCUS_KEY,
          JSON.stringify({ id: el.id, path: window.location.pathname, at: Date.now() }),
        );
      }
    } catch (e) {
      // storage blocked: the filter still applies, focus just is not restored.
    }
    el.form.requestSubmit();
  });

  (function restoreFocus() {
    var raw = null;
    try {
      raw = window.sessionStorage.getItem(FOCUS_KEY);
      window.sessionStorage.removeItem(FOCUS_KEY);
    } catch (e) {
      return;
    }
    if (!raw) return;
    var entry = null;
    try {
      entry = JSON.parse(raw);
    } catch (e) {
      return; // not an entry this script wrote: nothing to refocus.
    }
    if (!entry || typeof entry.id !== "string" || entry.path !== window.location.pathname) return;
    var age = Date.now() - entry.at;
    if (!(age >= 0 && age <= FOCUS_MAX_AGE_MS)) return;
    var el = document.getElementById(entry.id);
    if (el && el.hasAttribute("data-queue-autosubmit")) el.focus();
  })();

  document.addEventListener("htmx:afterSettle", function (evt) {
    var cfg = evt.detail && evt.detail.requestConfig;
    var trigger = cfg && cfg.elt;
    if (!trigger || !trigger.classList || !trigger.classList.contains("mx-queue-more")) return;
    var batches = document.querySelectorAll("#mx-queue-body tr[data-queue-batch]");
    var row = batches[batches.length - 1];
    if (!row) {
      console.error("queue.js: no appended row to focus after Show more");
      return;
    }
    row.setAttribute("tabindex", "-1");
    row.addEventListener("blur", function () { row.removeAttribute("tabindex"); }, { once: true });
    row.focus();
  });
})();
