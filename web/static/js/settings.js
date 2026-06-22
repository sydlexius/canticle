// settings.js powers the client-side affordances on the Settings page (Phase 1,
// no persistence): the webhook "Generate key" button, the reactive enable/grey
// of dependent fields (provider credentials, verification/detector children),
// and the add/remove address-list controls. Persisting any of it is Phase 2.
// Capability gaps fail loudly (console.error), never silently.
(function () {
  "use strict";

  // --- Webhook key generation ---------------------------------------------
  function randomSuffix() {
    if (!(window.crypto && typeof window.crypto.getRandomValues === "function")) {
      console.error("settings.js: window.crypto.getRandomValues unavailable; cannot generate a key");
      return null;
    }
    var bytes = new Uint8Array(24);
    window.crypto.getRandomValues(bytes);
    var hex = "";
    for (var i = 0; i < bytes.length; i++) {
      hex += ("0" + bytes[i].toString(16)).slice(-2);
    }
    return hex;
  }

  function handleGenerateKey(btn) {
    var targetId = btn.getAttribute("data-target");
    var input = targetId ? document.getElementById(targetId) : null;
    if (!input) {
      console.error("settings.js: generate-key target not found:", targetId);
      return;
    }
    var suffix = randomSuffix();
    if (suffix === null) {
      return;
    }
    input.value = (btn.getAttribute("data-prefix") || "") + suffix;
  }

  // --- Reactive enable / grey-out -----------------------------------------
  // A field carrying data-enable-when-checked names the id of the checkbox or
  // radio whose checked state enables it. When that control is unchecked, every
  // form control inside the field is disabled and the field is greyed. A field
  // that is server-locked (an env/CLI override is in effect) stays disabled
  // regardless, so gating never re-enables a locked control.
  function syncGating() {
    var fields = document.querySelectorAll("[data-enable-when-checked]");
    for (var i = 0; i < fields.length; i++) {
      var field = fields[i];
      if (field.classList.contains("mx-field-locked")) {
        continue;
      }
      var controllerId = field.getAttribute("data-enable-when-checked");
      var controller = controllerId ? document.getElementById(controllerId) : null;
      if (!controller) {
        console.error("settings.js: gating controller not found:", controllerId);
        continue;
      }
      setFieldEnabled(field, controller.checked);
    }
  }

  function setFieldEnabled(field, enabled) {
    field.classList.toggle("mx-field-disabled", !enabled);
    var controls = field.querySelectorAll("input, select, textarea, button");
    for (var i = 0; i < controls.length; i++) {
      controls[i].disabled = !enabled;
    }
  }

  function fieldLocked(el) {
    var field = el.closest(".mx-settings-field");
    return !!(field && field.classList.contains("mx-field-locked"));
  }

  // syncProviders makes the provider-selection controls follow the enablement
  // checkboxes: a disabled provider is removed from "Main lyrics source"
  // (providers.primary) and from the source order (providers.fallback_order),
  // and "How to use multiple sources" (providers.mode) is greyed when fewer than
  // two providers are enabled - there is nothing to order with one source.
  // (#288 E2/E3.) Locked fields (env/CLI override) are left untouched.
  function syncProviders() {
    var boxes = document.querySelectorAll('input[name="providers.disabled"]');
    if (!boxes.length) {
      return;
    }
    var enabled = {};
    var count = 0;
    for (var i = 0; i < boxes.length; i++) {
      if (boxes[i].checked) {
        enabled[boxes[i].value] = true;
        count++;
      }
    }

    var primary = document.querySelector('select[name="providers.primary"]');
    if (primary && !fieldLocked(primary)) {
      var selected = primary.options[primary.selectedIndex];
      for (var j = 0; j < primary.options.length; j++) {
        primary.options[j].disabled = !enabled[primary.options[j].value];
      }
      if (selected && selected.disabled) {
        for (var k = 0; k < primary.options.length; k++) {
          if (!primary.options[k].disabled) {
            primary.selectedIndex = k;
            break;
          }
        }
      }
    }

    var fb = document.querySelectorAll('input[name="providers.fallback_order"]');
    for (var m = 0; m < fb.length; m++) {
      if (fieldLocked(fb[m])) {
        continue;
      }
      var on = !!enabled[fb[m].value];
      fb[m].disabled = !on;
      if (!on) {
        fb[m].checked = false;
      }
    }

    var mode = document.querySelector('select[name="providers.mode"]');
    if (mode) {
      var modeField = mode.closest(".mx-settings-field");
      if (modeField && !modeField.classList.contains("mx-field-locked")) {
        setFieldEnabled(modeField, count >= 2);
      }
      // G3: the "Set the order to try sources" jump only matters in ordered mode.
      if (modeField) {
        var jumpLink = modeField.querySelector(".mx-settings-jump");
        if (jumpLink) {
          jumpLink.style.display = mode.value === "ordered" ? "" : "none";
        }
      }
    }
  }

  // syncTLS is the reverse of the cert/key gating (which settings.js's generic
  // data-enable-when-checked handles via the self_signed "off" radio): when a
  // cert_file or key_file path is set, self_signed can't be combined with it, so
  // disable the self_signed control. Re-enabled once both are blank. The server
  // checkTLSInvariant is the hard net; this just avoids an always-400 action.
  function syncTLS() {
    var cert = document.querySelector('input[name="server.tls.cert_file"]');
    var key = document.querySelector('input[name="server.tls.key_file"]');
    var ss = document.querySelectorAll('input[name="server.tls.self_signed"]');
    if (!ss.length) {
      return;
    }
    var hasCertKey = (cert && cert.value.trim() !== "") || (key && key.value.trim() !== "");
    var ssField = ss[0].closest(".mx-settings-field");
    if (ssField && !ssField.classList.contains("mx-field-locked")) {
      setFieldEnabled(ssField, !hasCertKey);
    }
  }

  // --- ARIA tab semantics -------------------------------------------------
  // Maps each radio id to the corresponding tabctl label id.
  var TAB_RADIOS = ["mx-tab-common", "mx-tab-advanced", "mx-tab-raw"];
  var TAB_CTLS   = ["mx-tabctl-common", "mx-tabctl-advanced", "mx-tabctl-raw"];

  // Sync aria-selected + tabindex on the three tab labels to match which radio
  // is :checked. Called on init (via syncAll) and after any radio state change.
  function syncTabs() {
    for (var i = 0; i < TAB_RADIOS.length; i++) {
      var radio = document.getElementById(TAB_RADIOS[i]);
      var ctl   = document.getElementById(TAB_CTLS[i]);
      if (!radio || !ctl) {
        console.error("settings.js syncTabs: missing element", TAB_RADIOS[i], TAB_CTLS[i]);
        continue;
      }
      var active = radio.checked;
      ctl.setAttribute("aria-selected", active ? "true" : "false");
      ctl.setAttribute("tabindex", active ? "0" : "-1");
    }
  }

  // Keyboard navigation for the tablist: Left/Right cycle through tabs,
  // Home/End jump to the first/last tab. Checks the radio and focuses the label.
  function initTabKeyboard() {
    var tablist = document.querySelector(".mx-tablist");
    if (!tablist) {
      console.error("settings.js initTabKeyboard: .mx-tablist not found");
      return;
    }
    tablist.addEventListener("keydown", function (event) {
      var key = event.key;
      if (key !== "ArrowLeft" && key !== "ArrowRight" && key !== "Home" && key !== "End") {
        return;
      }
      event.preventDefault();
      var currentIndex = -1;
      for (var i = 0; i < TAB_RADIOS.length; i++) {
        var radio = document.getElementById(TAB_RADIOS[i]);
        if (radio && radio.checked) {
          currentIndex = i;
          break;
        }
      }
      if (currentIndex === -1) {
        return;
      }
      var nextIndex;
      if (key === "ArrowLeft") {
        nextIndex = (currentIndex - 1 + TAB_RADIOS.length) % TAB_RADIOS.length;
      } else if (key === "ArrowRight") {
        nextIndex = (currentIndex + 1) % TAB_RADIOS.length;
      } else if (key === "Home") {
        nextIndex = 0;
      } else {
        nextIndex = TAB_RADIOS.length - 1;
      }
      var nextRadio = document.getElementById(TAB_RADIOS[nextIndex]);
      var nextCtl   = document.getElementById(TAB_CTLS[nextIndex]);
      if (nextRadio && nextCtl) {
        nextRadio.checked = true;
        syncTabs();
        nextCtl.focus();
      }
    });
  }
  // -------------------------------------------------------------------------

  function handleJump(link) {
    var tabId = link.getAttribute("data-jump-tab");
    var targetId = link.getAttribute("data-jump-target");
    var tab = tabId ? document.getElementById(tabId) : null;
    if (tab) {
      tab.checked = true; // switch the CSS-only tab to reveal the target panel
      syncTabs();
    } else {
      console.error("settings.js: jump tab not found:", tabId);
    }
    var target = targetId ? document.getElementById(targetId) : null;
    if (!target) {
      console.error("settings.js: jump target not found:", targetId);
      return;
    }
    // Scroll to the whole field CARD, not the bare control: the target id sits on
    // the field's first input (e.g. the first fallback-order checkbox), so
    // scrolling to it lands mid-card with the field's label scrolled off above.
    // The enclosing .mx-settings-field puts the label/heading at the top (#288).
    var dest = target.closest(".mx-settings-field") || target;
    // Flipping the tab radio un-hides the previously display:none panel; defer
    // the scroll until after layout reflows (two animation frames) so the
    // destination's offset is current, not the stale pre-reflow value (#288 G2).
    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        if (typeof dest.scrollIntoView === "function") {
          dest.scrollIntoView({ behavior: "smooth", block: "start" });
        }
      });
    });
  }

  // --- Add / remove address list ------------------------------------------
  function addTag(container) {
    var input = container.querySelector(".mx-taglist-input");
    var items = container.querySelector(".mx-taglist-items");
    if (!input || !items) {
      console.error("settings.js: taglist input or list missing");
      return;
    }
    var value = input.value.trim();
    if (value === "") {
      return;
    }

    var li = document.createElement("li");
    li.className = "mx-taglist-item";
    var span = document.createElement("span");
    span.className = "mx-taglist-text";
    span.textContent = value;
    var remove = document.createElement("button");
    remove.type = "button";
    remove.className = "mx-taglist-remove";
    remove.setAttribute("aria-label", "Remove entry");
    remove.textContent = "×";
    li.appendChild(span);
    li.appendChild(remove);
    items.appendChild(li);

    input.value = "";
    input.focus();
  }

  // --- Save (POST /settings/field) ----------------------------------------
  function csrfToken() {
    var el = document.getElementById("mx-csrf-token");
    return el ? el.value : "";
  }

  function setStatus(field, msg, isError) {
    var el = field.querySelector(".mx-settings-field-status");
    if (!el) {
      return;
    }
    el.textContent = msg;
    el.classList.toggle("mx-status-error", !!isError);
    el.classList.toggle("mx-status-ok", !isError);
  }

  // collectValuePairs reads the field's current value as [name, value] pairs to
  // POST, dispatching on the control kind: duration (number + unit), taglist
  // (each item), checkboxes (each checked), radios (the checked one), or a
  // single select/input. A list with nothing selected sends no value pair, which
  // the server reads as an empty list.
  function collectValuePairs(field) {
    var pairs = [];
    var durNum = field.querySelector(".mx-settings-duration-num");
    if (durNum) {
      pairs.push(["value", durNum.value]);
      var unitSel = field.querySelector(".mx-settings-duration-unit");
      if (unitSel) {
        pairs.push(["unit", unitSel.value]);
      }
      return pairs;
    }
    var taglist = field.querySelector("[data-taglist]");
    if (taglist) {
      var items = taglist.querySelectorAll(".mx-taglist-text");
      for (var i = 0; i < items.length; i++) {
        pairs.push(["value", items[i].textContent]);
      }
      return pairs;
    }
    var checks = field.querySelectorAll('input[type="checkbox"]');
    if (checks.length) {
      for (var j = 0; j < checks.length; j++) {
        if (checks[j].checked) {
          pairs.push(["value", checks[j].value]);
        }
      }
      return pairs;
    }
    var radios = field.querySelectorAll('input[type="radio"]');
    if (radios.length) {
      for (var k = 0; k < radios.length; k++) {
        if (radios[k].checked) {
          pairs.push(["value", radios[k].value]);
          break;
        }
      }
      return pairs;
    }
    var sel = field.querySelector("select");
    if (sel) {
      pairs.push(["value", sel.value]);
      return pairs;
    }
    var input = field.querySelector("input, textarea");
    if (input) {
      pairs.push(["value", input.value]);
    }
    return pairs;
  }

  function saveField(field) {
    var path = field.getAttribute("data-field-path");
    if (!path) {
      return;
    }
    var token = csrfToken();
    if (!token) {
      setStatus(field, "Cannot save: missing CSRF token (reload the page)", true);
      return;
    }
    var body = new URLSearchParams();
    body.append("csrf_token", token);
    body.append("path", path);
    var pairs = collectValuePairs(field);
    for (var i = 0; i < pairs.length; i++) {
      body.append(pairs[i][0], pairs[i][1]);
    }
    setStatus(field, "Saving...", false);
    fetch("/settings/field", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: body.toString(),
      credentials: "same-origin"
    }).then(function (res) {
      if (res.ok) {
        setStatus(field, "Saved - restart to apply", false);
        return null;
      }
      return res.text().then(function (text) {
        setStatus(field, "Error: " + (text || ("HTTP " + res.status)), true);
      });
    }).catch(function (err) {
      setStatus(field, "Error: " + err, true);
    });
  }

  // saveSection POSTs every field in the triggering card's save group together
  // to /settings/section as one atomic change (#298): the [server.tls] cert+key
  // pair must be written together to satisfy the "set together" invariant, which
  // a single-field save cannot do from an empty state (each blank-partner POST
  // 400s). Each group member is a card carrying the same data-save-group; its
  // config path is data-field-path and its value is the card's single input.
  function saveSection(field) {
    var group = field.getAttribute("data-save-group");
    var cards = document.querySelectorAll('[data-save-group="' + group + '"]');
    if (!cards.length) {
      return;
    }
    var token = csrfToken();
    if (!token) {
      setStatus(field, "Cannot save: missing CSRF token (reload the page)", true);
      return;
    }
    var body = new URLSearchParams();
    body.append("csrf_token", token);
    var i;
    for (i = 0; i < cards.length; i++) {
      var path = cards[i].getAttribute("data-field-path");
      if (!path) {
        continue;
      }
      var input = cards[i].querySelector("input, select, textarea");
      body.append("path", path);
      body.append(path, input ? input.value.trim() : "");
    }
    for (i = 0; i < cards.length; i++) {
      setStatus(cards[i], "Saving...", false);
    }
    fetch("/settings/section", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: body.toString(),
      credentials: "same-origin"
    }).then(function (res) {
      if (res.ok) {
        for (var j = 0; j < cards.length; j++) {
          setStatus(cards[j], "Saved - restart to apply", false);
        }
        return null;
      }
      return res.text().then(function (text) {
        for (var k = 0; k < cards.length; k++) {
          setStatus(cards[k], "Error: " + (text || ("HTTP " + res.status)), true);
        }
      });
    }).catch(function (err) {
      for (var m = 0; m < cards.length; m++) {
        setStatus(cards[m], "Error: " + err, true);
      }
    });
  }

  // saveCard dispatches a card's save to the section endpoint when it is part of
  // a save group, otherwise to the single-field endpoint.
  function saveCard(card) {
    if (card.getAttribute("data-save-group")) {
      saveSection(card);
    } else {
      saveField(card);
    }
  }

  // --- Wiring --------------------------------------------------------------
  document.addEventListener("click", function (event) {
    var jumpLink = event.target.closest(".mx-settings-jump");
    if (jumpLink) {
      event.preventDefault();
      handleJump(jumpLink);
      return;
    }

    var genBtn = event.target.closest("[data-generate-key]");
    if (genBtn) {
      event.preventDefault();
      handleGenerateKey(genBtn);
      return;
    }

    var addBtn = event.target.closest(".mx-taglist-addbtn");
    if (addBtn) {
      event.preventDefault();
      var container = addBtn.closest("[data-taglist]");
      if (container) {
        addTag(container);
      }
      return;
    }

    var removeBtn = event.target.closest(".mx-taglist-remove");
    if (removeBtn) {
      event.preventDefault();
      var li = removeBtn.closest(".mx-taglist-item");
      if (li) {
        li.remove();
      }
      return;
    }

    var saveBtn = event.target.closest(".mx-settings-save");
    if (saveBtn) {
      event.preventDefault();
      var saveField2 = saveBtn.closest("[data-field-path]");
      if (!saveField2) {
        return;
      }
      if (saveBtn.getAttribute("data-confirm") === "true") {
        var lbl = saveField2.querySelector(".mx-settings-field-label");
        var name = lbl ? lbl.textContent : "this setting";
        if (!window.confirm('Save "' + name + '"? This is a critical setting and may affect access or startup.')) {
          return;
        }
      }
      saveCard(saveField2);
    }
  });

  // Enter inside a taglist input adds the entry rather than submitting.
  document.addEventListener("keydown", function (event) {
    if (event.key !== "Enter") {
      return;
    }
    var input = event.target.closest(".mx-taglist-input");
    if (!input) {
      return;
    }
    event.preventDefault();
    var container = input.closest("[data-taglist]");
    if (container) {
      addTag(container);
    }
  });

  // Re-evaluate gating and provider-selection state on any control change, and
  // once on load. Safe-tier fields also hot-save on change; caution/critical
  // fields wait for their Save button.
  function syncAll() {
    syncGating();
    syncProviders();
    syncTLS();
    syncTabs();
  }
  document.addEventListener("change", function (event) {
    syncAll();
    var field = event.target.closest ? event.target.closest("[data-field-path]") : null;
    if (field && field.getAttribute("data-field-tier") === "safe") {
      saveCard(field);
    }
  });
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () {
      syncAll();
      initTabKeyboard();
    });
  } else {
    syncAll();
    initTabKeyboard();
  }
})();
