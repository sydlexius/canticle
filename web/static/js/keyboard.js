// keyboard.js (#481): a small platform-aware shortcut registry that renders its
// own legend, so the legend can never drift from what is bound.
//
//   window.mxKeyboard = { register(entry), list(), renderLegend(container), isMac }
//   entry = { keys: ["Mod", "s"], label: "save", handler: fn(e), allowInInput?: bool }
//
// "Mod" is Meta on macOS and Control elsewhere (detected once at load). A
// matched chord always calls preventDefault before the handler. Shortcuts are
// ignored while focus is in an input, textarea, select or contenteditable,
// unless the entry sets allowInInput. CSP-safe: external file, no inline
// handlers, the legend is built with createElement/textContent only.
(function () {
  "use strict";

  if (window.mxKeyboard) {
    return; // re-init guard: one registry and one document listener per page
  }

  function detectMac(nav) {
    var p = (nav.userAgentData && nav.userAgentData.platform) || nav.platform || nav.userAgent || "";
    return /mac|iphone|ipad|ipod/i.test(p);
  }

  var isMac = detectMac(window.navigator);
  var MODIFIERS = { Mod: 1, Shift: 1, Alt: 1, Control: 1, Meta: 1 };
  // Shifted punctuation reports a different e.key ("}" for "]"), so these
  // match by physical code as well.
  var CODES = { "[": "BracketLeft", "]": "BracketRight" };
  var entries = [];

  function parse(keys) {
    var want = { meta: false, ctrl: false, shift: false, alt: false };
    var key = "";
    for (var i = 0; i < keys.length; i++) {
      var k = keys[i];
      if (k === "Mod") {
        want[isMac ? "meta" : "ctrl"] = true;
      } else if (k === "Meta") {
        want.meta = true;
      } else if (k === "Control") {
        want.ctrl = true;
      } else if (k === "Shift") {
        want.shift = true;
      } else if (k === "Alt") {
        want.alt = true;
      } else {
        key = k;
      }
    }
    return { want: want, key: key };
  }

  function register(entry) {
    if (!entry || !entry.keys || !entry.keys.length || typeof entry.handler !== "function") {
      console.error("mxKeyboard.register: entry needs keys and a handler");
      return;
    }
    var p = parse(entry.keys);
    if (p.key === "") {
      console.error("mxKeyboard.register: entry has no non-modifier key");
      return;
    }
    entries.push({ entry: entry, want: p.want, key: p.key });
  }

  function matches(item, e) {
    var w = item.want;
    if (e.metaKey !== w.meta || e.ctrlKey !== w.ctrl || e.altKey !== w.alt) {
      return false;
    }
    var k = item.key;
    var isLetter = k.length === 1 && k.toLowerCase() !== k.toUpperCase();
    // Shift is exact for letters and named keys; punctuation is already
    // shift-dependent on some layouts, so it is only checked when asked for.
    if ((isLetter || k.length > 1 || w.shift) && e.shiftKey !== w.shift) {
      return false;
    }
    if (isLetter) {
      return e.key.toLowerCase() === k.toLowerCase();
    }
    return e.key === k || (w.shift && CODES[k] !== undefined && e.code === CODES[k]);
  }

  function isTyping(el) {
    if (!el || !el.tagName) {
      return false;
    }
    var tag = el.tagName.toLowerCase();
    if (tag === "input" || tag === "textarea" || tag === "select") {
      return true;
    }
    return !!(el.isContentEditable || (el.closest && el.closest('[contenteditable]:not([contenteditable="false"])')));
  }

  document.addEventListener("keydown", function (e) {
    var typing = isTyping(e.target);
    for (var i = 0; i < entries.length; i++) {
      var item = entries[i];
      if (typing && !item.entry.allowInInput) {
        continue;
      }
      if (matches(item, e)) {
        e.preventDefault();
        item.entry.handler(e);
        return;
      }
    }
  });

  function keyLabel(k) {
    if (k === "Mod") {
      return isMac ? "⌘" : "Ctrl";
    }
    if (k === "Escape") {
      return "Esc";
    }
    return k;
  }

  function renderLegend(container) {
    container.textContent = "";
    container.classList.add("mx-kbd-legend");
    entries.forEach(function (item) {
      var span = document.createElement("span");
      item.entry.keys.forEach(function (k) {
        var kbd = document.createElement("kbd");
        kbd.className = "mx-kbd";
        kbd.textContent = keyLabel(k);
        span.appendChild(kbd);
      });
      span.appendChild(document.createTextNode(" " + item.entry.label));
      container.appendChild(span);
    });
  }

  window.mxKeyboard = {
    register: register,
    list: function () {
      return entries.map(function (item) {
        return item.entry;
      });
    },
    renderLegend: renderLegend,
    isMac: isMac,
  };
})();
