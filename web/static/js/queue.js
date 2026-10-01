// Queue drill-down (#598): after "Show more" replaces its own row, focus would
// fall to <body>; move it to the first appended row. A file, not inline (CSP).
(function () {
  "use strict";

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
