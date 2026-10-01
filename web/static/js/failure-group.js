// Reports failure groups (#478): the "Show rows" control fetches a group's rows
// once; after that it toggles the loaded rows without another request (the
// fragment query is a status scan). A file, not inline (CSP script-src 'self').
(function () {
  "use strict";

  function isToggle(elt) {
    return !!elt && !!elt.classList && elt.classList.contains("mx-fg-toggle");
  }

  function detailRow(btn) {
    var cell = document.getElementById(btn.getAttribute("aria-controls"));
    return cell && cell.closest("tr");
  }

  function setExpanded(btn, expanded) {
    var row = detailRow(btn);
    if (!row) {
      console.error("failure-group.js: no detail row for", btn.getAttribute("aria-controls"));
      return;
    }
    row.hidden = !expanded;
    btn.setAttribute("aria-expanded", expanded ? "true" : "false");
    btn.textContent = expanded ? "Hide rows" : "Show rows";
  }

  // Once loaded, a click only toggles: cancel htmx's request.
  document.addEventListener("htmx:beforeRequest", function (evt) {
    var btn = evt.detail && evt.detail.elt;
    if (!isToggle(btn) || btn.getAttribute("data-loaded") !== "true") return;
    evt.preventDefault();
    setExpanded(btn, btn.getAttribute("aria-expanded") !== "true");
  });

  // The first successful swap marks the group loaded and expanded. On
  // afterSwap, detail.elt is the swap TARGET; the trigger is requestConfig.elt.
  document.addEventListener("htmx:afterSwap", function (evt) {
    var cfg = evt.detail && evt.detail.requestConfig;
    var btn = cfg && cfg.elt;
    if (!isToggle(btn)) return;
    btn.setAttribute("data-loaded", "true");
    setExpanded(btn, true);
  });
})();
