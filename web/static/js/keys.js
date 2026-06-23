// keys.js powers the one client-side affordance on the webhook keys page (#300):
// the "Copy" button next to a freshly revealed raw key. Everything else (create,
// revoke) is plain htmx form posts, so the page works with this script absent.
// CSP-safe: external file, event-delegated listener, no inline handlers. Capability
// gaps fail loudly (console.error), never silently.
(function () {
  "use strict";

  // copyFromTarget copies the value of the input named by the button's
  // data-copy-target into the clipboard. It prefers the async Clipboard API and
  // falls back to selecting the field + execCommand so an http (non-secure)
  // context, where navigator.clipboard is unavailable, still copies.
  function copyFromTarget(btn) {
    var targetId = btn.getAttribute("data-copy-target");
    var input = targetId ? document.getElementById(targetId) : null;
    if (!input) {
      console.error("keys.js: copy target not found:", targetId);
      return;
    }
    var value = input.value;
    var done = function () {
      var original = btn.textContent;
      btn.textContent = "Copied";
      window.setTimeout(function () {
        btn.textContent = original;
      }, 1500);
    };

    if (window.navigator && window.navigator.clipboard && typeof window.navigator.clipboard.writeText === "function") {
      window.navigator.clipboard.writeText(value).then(done).catch(function (err) {
        console.error("keys.js: clipboard write failed; falling back to select", err);
        selectFallback(input, done);
      });
      return;
    }
    selectFallback(input, done);
  }

  // selectFallback selects the field's text and tries the legacy copy command,
  // leaving the value selected for a manual copy if that is unavailable too.
  function selectFallback(input, done) {
    input.focus();
    input.select();
    var copied = false;
    try {
      copied = document.execCommand && document.execCommand("copy");
    } catch (err) {
      console.error("keys.js: execCommand copy failed", err);
    }
    if (copied) {
      done();
    } else {
      console.error("keys.js: no clipboard mechanism available; key left selected for manual copy");
    }
  }

  document.addEventListener("click", function (event) {
    var btn = event.target.closest("[data-copy-target]");
    if (btn) {
      event.preventDefault();
      copyFromTarget(btn);
    }
  });

  // --- Viewer-local timestamps --------------------------------------------
  // Reformat <time data-tz="pending"> cells (Created / Revoked) into the
  // browser's local timezone with a 3-letter abbreviation (e.g. "PDT"), matching
  // the dashboard. The dashboard does this with an inline script; this page is
  // CSP script-src 'self', so the same logic lives here in the external file.
  // Elements marked data-tz-applied (server already applied a TZ-env zone) are
  // left untouched. Re-run after every htmx swap so the re-rendered key list (on
  // create/revoke) gets reformatted too.
  function reformatTimes(root) {
    var scope = root && root.querySelectorAll ? root : document;
    scope.querySelectorAll('time[data-tz="pending"]').forEach(function (el) {
      var iso = el.getAttribute("datetime");
      if (!iso) {
        return;
      }
      try {
        var d = new Date(iso);
        var tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
        el.textContent = d.toLocaleString("en-US", {
          year: "numeric", month: "2-digit", day: "2-digit",
          hour: "2-digit", minute: "2-digit",
          timeZone: tz,
          timeZoneName: "short"
        });
        // Mark done so a later swap that re-scopes to document does not
        // re-process an already-localized cell.
        el.setAttribute("data-tz", "applied");
      } catch (err) {
        console.error("keys.js: timestamp reformat failed", err);
      }
    });
  }

  // The script is deferred, so the DOM is parsed when this runs.
  reformatTimes(document);
  // htmx swaps the key-list panel on create/revoke; reformat the swapped-in nodes.
  if (document.body) {
    document.body.addEventListener("htmx:afterSwap", function (event) {
      reformatTimes(event.target || document);
    });
  }
})();
