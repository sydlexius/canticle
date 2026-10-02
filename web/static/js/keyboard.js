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
  // A modified bracket reports a different e.key: Shift gives "}" for "]", and
  // on macOS Option gives a typographic quote. These match by physical code
  // whenever Shift or Alt is part of the chord.
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
    var isPunct = k.length === 1 && !isLetter && k !== " ";
    var shiftExact = e.shiftKey === w.shift;
    // Shift is exact for letters, Space and named keys. A punctuation key typed
    // literally tolerates an extra Shift, since some layouts need Shift to
    // produce it; the physical-code fallback below always checks Shift exactly,
    // so a Shift+bracket chord never lands on the unshifted binding.
    if (!isPunct && !shiftExact) {
      return false;
    }
    if (isLetter) {
      return e.key.toLowerCase() === k.toLowerCase();
    }
    if (e.key === k) {
      return w.shift ? shiftExact : true;
    }
    return shiftExact && (w.shift || w.alt) && CODES[k] !== undefined && e.code === CODES[k];
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

  // A focused button, link, checkbox or role=button element activates on a bare
  // Space or Enter; a shortcut bound to either key must not steal that press,
  // or the control (a dialog's Save, a nudge button) becomes unusable from the
  // keyboard.
  function activatesOnKey(el, e) {
    if (!el || !el.tagName || e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) {
      return false;
    }
    if (e.key !== " " && e.key !== "Enter") {
      return false;
    }
    var tag = el.tagName.toLowerCase();
    if (tag === "button" || tag === "a" || tag === "summary") {
      return true;
    }
    if (tag === "input" && (el.type === "checkbox" || el.type === "radio" || el.type === "button" || el.type === "submit")) {
      return true;
    }
    return el.getAttribute && el.getAttribute("role") === "button";
  }

  document.addEventListener("keydown", function (e) {
    // A keydown during IME composition (an Escape that cancels it, for one)
    // belongs to the input method, never to an application shortcut.
    if (e.isComposing || e.keyCode === 229) {
      return;
    }
    // composedPath()[0] is the real target inside an open shadow root, where
    // e.target is only the host.
    var path = typeof e.composedPath === "function" ? e.composedPath() : [];
    var origin = path.length ? path[0] : e.target;
    if (activatesOnKey(origin, e)) {
      return;
    }
    var typing = isTyping(origin);
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
    if (k === " ") {
      return "Space"; // a bare space would render an empty keycap
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
