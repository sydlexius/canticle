// preview.js drives the preview player page (#481): it highlights the current
// lyric line and word as the audio plays, keeps the current line in view, and
// seeks the audio when a line is clicked or activated from the keyboard.
//
// The page is complete without it (server-rendered lines, native audio
// controls); this only adds the sync. CSP-safe: external file, class toggles
// only (no inline styles, no eval). A missing element fails loudly.
//
// Current line = the LAST line whose data-start-ms <= currentTime (lines are
// time-sorted by the server), none before the first. Current word = the word
// with the GREATEST start <= currentTime inside the current line; word starts
// are in source order and may be non-monotonic, so they are scanned, not
// bisected.
//
// Auto-scroll rule: the current line is scrolled to the centre whenever it
// changes, EXCEPT after the user scrolled by hand (wheel, touch drag, or a
// scroll key). That pauses following so the page never fights the reader; it
// resumes on the next seek (clicking a line, or the audio's own seeked event)
// or on play, and resuming scrolls the current line back into view once. A
// scroll event the script did not cause (e.g. dragging the scrollbar) also
// pauses following; scrolls caused by scrollIntoView are told apart by a short
// "programmatic" window that lasts until scroll events go quiet. Motion is instant under prefers-reduced-motion, smooth otherwise.
//
// Offset editor (#1211): when the page carries #mx-edit, nudging shifts the
// in-memory line starts (highlight, seek and the shown times) only; nothing is
// written until Save, which POSTs the total offset and the mtime the page
// loaded with. Pure helpers are exported on window.mxPreviewEdit for tests.
(function () {
  "use strict";

  var MAX_OFFSET_MS = 600000;
  var SKIP_KEY = "mx-offset-confirm-skip";

  // parseOffset reads a typed offset in seconds ("-7.25", "+0.6", "1,5") and
  // returns ms rounded to 10, or null for anything else or |ms| > 600000.
  function parseOffset(text) {
    var t = String(text).trim().replace(",", ".");
    if (!/^[+-]?(\d+\.?\d*|\.\d+)$/.test(t)) {
      return null;
    }
    var ms = Math.round(parseFloat(t) * 100) * 10;
    return Math.abs(ms) > MAX_OFFSET_MS ? null : ms;
  }

  // pastEnd counts the line starts that the offset would put more than
  // toleranceMs after the audio end; 0 when the duration is unknown. Callers
  // pass only non-decorative lines: the server marks those with
  // timing.IsDecorative, the same rule its timing guard skips. This is a
  // preview only; the server's timing guard stays authoritative on save.
  function pastEnd(startsMs, offsetMs, durationMs, toleranceMs) {
    if (!(durationMs > 0)) {
      return 0;
    }
    return startsMs.filter(function (s) {
      return Math.max(0, s + offsetMs) > durationMs + toleranceMs;
    }).length;
  }

  window.mxPreviewEdit = { parseOffset: parseOffset, pastEnd: pastEnd };

  function fmtTime(ms) {
    var m = Math.floor(ms / 60000);
    var r = (ms % 60000) / 1000;
    return m + ":" + (r < 10 ? "0" : "") + r.toFixed(2);
  }

  function fmtOffset(ms) {
    return (ms > 0 ? "+" : ms < 0 ? "-" : "") + (Math.abs(ms) / 1000).toFixed(2);
  }

  var SCROLL_KEYS = ["PageUp", "PageDown", "Home", "End", "ArrowUp", "ArrowDown", " "];

  function startOf(el) {
    return Number(el.getAttribute("data-start-ms"));
  }

  // maxAtOrBefore returns the index of the item with the greatest start <= ms
  // (the later one on a tie), or -1. Order-independent.
  function maxAtOrBefore(starts, ms) {
    var found = -1;
    for (var i = 0; i < starts.length; i++) {
      if (starts[i] <= ms && (found < 0 || starts[i] >= starts[found])) {
        found = i;
      }
    }
    return found;
  }

  // lastAtOrBefore returns the index of the last item with start <= ms, or -1.
  // Only valid for sorted input (the line list).
  function lastAtOrBefore(starts, ms) {
    var lo = 0;
    var hi = starts.length - 1;
    var found = -1;
    while (lo <= hi) {
      var mid = (lo + hi) >> 1;
      if (starts[mid] <= ms) {
        found = mid;
        lo = mid + 1;
      } else {
        hi = mid - 1;
      }
    }
    return found;
  }

  // initEditor wires the offset editor. lineStarts is mutated in place so the
  // highlight and click-to-seek in init() follow the shifted times.
  function initEditor(panel, audio, lines, lineStarts, update) {
    var $ = function (id) {
      return document.getElementById(id);
    };
    // The page shows the file as saved, which is the ORIGINAL shifted by the
    // saved offset (with negative starts clamped to 0, so it cannot be undone
    // here). The server renders the original starts in line order; the offset
    // is always applied to those.
    var savedMS = Number(panel.getAttribute("data-offset-ms")) || 0;
    var origParts = String(panel.getAttribute("data-orig-ms") || "").split(",");
    var base = origParts.map(Number);
    // timing.Tolerance in ms, rendered by the server so this preview cannot
    // drift from the guard that judges the save.
    var tolerance = Number(panel.getAttribute("data-tolerance-ms"));
    var bad =
      origParts.length !== lineStarts.length ||
      origParts.some(function (p) {
        return !/^\d+$/.test(p);
      })
        ? "data-orig-ms does not match the lyric lines"
        : !(tolerance > 0)
          ? "data-tolerance-ms is missing"
          : "";
    if (bad) {
      console.error("preview.js: " + bad + "; editor off");
      panel.hidden = true;
      return;
    }
    var live = lines.map(function (li) {
      return !li.classList.contains("mx-preview-line-decorative");
    });
    var times = lines.map(function (li) {
      var t = document.createElement("span");
      t.className = "mx-preview-time";
      li.insertBefore(t, li.firstChild);
      return t;
    });
    var field = $("mx-edit-offset");
    var slider = $("mx-edit-slider");
    var save = $("mx-edit-save");
    var discard = $("mx-edit-discard");
    var revert = $("mx-edit-revert");
    var chip = $("mx-edit-chip");
    var statusEl = $("mx-edit-status");
    var dialog = $("mx-edit-confirm");
    var nudgers = Array.prototype.slice.call(panel.querySelectorAll(".mx-edit-nudge"));
    var duration = Number(panel.getAttribute("data-duration-ms")) || 0;
    var token = panel.querySelector('input[name="csrf_token"]');
    var st = {
      offset: savedMS,
      saved: savedMS,
      edited: panel.hasAttribute("data-edited"),
      mtime: panel.getAttribute("data-mtime"),
      phase: "idle",
    };

    function skipAsking() {
      try {
        return window.localStorage.getItem(SKIP_KEY) === "1";
      } catch (e) {
        return false; // storage blocked: ask every time
      }
    }

    var MESSAGES = {
      saving: ["Saving. The original file is backed up first.", ""],
      "refused-timing": ["Not saved: the timing check failed for this offset. Nothing was written.", "error"],
      "refused-changed": ["Not saved: the lyrics file changed on disk since this page loaded. Reload to edit the current file.", "error"],
      busy: ["Not saved: the track is being processed. Try again shortly.", "error"],
      error: ["Not saved: the server could not complete the request. Reload to see the file's current state.", "error"],
      saved: ["Saved. The original is kept as a backup, so Revert can restore it.", "ok"],
      reverted: ["Reverted. The file is back to its original timing.", "ok"],
    };

    function render() {
      var starts = [];
      lines.forEach(function (li, i) {
        lineStarts[i] = Math.max(0, base[i] + st.offset);
        times[i].textContent = fmtTime(lineStarts[i]);
        if (live[i]) {
          starts.push(base[i]);
        }
        li.classList.toggle("is-past-end", live[i] && duration > 0 && lineStarts[i] > duration + tolerance);
      });
      var past = pastEnd(starts, st.offset, duration, tolerance);
      var dirty = st.offset !== st.saved;
      var busy = st.phase === "saving";
      var isLocked = locked();
      field.value = fmtOffset(st.offset);
      slider.value = String(Math.max(-5000, Math.min(5000, st.offset)));
      nudgers.concat([field, slider]).forEach(function (c) {
        c.disabled = isLocked;
      });
      save.disabled = !dirty || past > 0 || isLocked;
      save.textContent = busy ? "Saving" : "Save";
      discard.disabled = !dirty || isLocked;
      revert.hidden = !(st.edited && !dirty) || isLocked;
      var tone = busy ? "blue" : dirty ? "amber" : st.edited ? "green" : "grey";
      chip.textContent = busy ? "Saving" : dirty ? "Unsaved" : st.edited ? "Edited" : "Original";
      chip.className = "mx-edit-chip is-" + tone;
      var msg;
      if (past > 0) {
        msg = [past + (past === 1 ? " line would" : " lines would") + " start more than " + tolerance / 1000 + " s after the track ends (" + fmtTime(duration).replace(/\.\d+$/, "") + "). Save is off until they fit.", "warn"];
      } else if (MESSAGES[st.phase]) {
        msg = MESSAGES[st.phase];
      } else if (dirty) {
        msg = ["Play the track and nudge until the lines land. Nothing is written until you save.", ""];
      } else if (st.edited) {
        msg = ["This file is " + fmtOffset(st.saved).replace(/^[+-]/, "") + " s " + (st.saved > 0 ? "later" : "earlier") + " than the original.", ""];
      } else {
        msg = ["Matches the original file. Nudge while it plays to line the lyrics up.", ""];
      }
      statusEl.textContent = msg[0];
      statusEl.className = "mx-edit-status" + (msg[1] ? " is-" + msg[1] : "");
      update();
    }

    function setOffset(ms) {
      if (locked()) {
        return;
      }
      st.offset = Math.max(-MAX_OFFSET_MS, Math.min(MAX_OFFSET_MS, ms));
      st.phase = "idle";
      render();
    }

    nudgers.forEach(function (b) {
      b.addEventListener("click", function () {
        setOffset(st.offset + Number(b.getAttribute("data-delta")));
      });
    });
    slider.addEventListener("input", function () {
      setOffset(parseInt(slider.value, 10) || 0);
    });
    field.addEventListener("change", function () {
      var ms = parseOffset(field.value);
      if (ms === null) {
        render(); // invalid input is ignored: put the current value back
      } else {
        setOffset(ms);
      }
    });
    field.addEventListener("keydown", function (ev) {
      if (ev.key === "Enter") {
        ev.preventDefault();
        field.dispatchEvent(new window.Event("change"));
      }
    });
    // A "changed" refusal locks the editor for good: the page's mtime and
    // original are stale, so only a reload may edit again.
    function locked() {
      return st.phase === "saving" || st.phase === "refused-changed";
    }

    discard.addEventListener("click", function () {
      if (!locked()) {
        st.offset = st.saved;
        st.phase = "idle";
        render();
      }
    });

    // post sends one edit. The response mtime is unix nanoseconds, beyond 2^53,
    // so it is read from the text and kept as a string, never via JSON.parse.
    function post(url, offsetMs, isRevert) {
      // One request at a time: a second call (a double click, Save racing
      // Revert) would send the mtime the first one is about to replace.
      if (locked()) {
        return;
      }
      st.phase = "saving";
      render();
      var body = new window.URLSearchParams();
      body.append("mtime", st.mtime);
      body.append("csrf_token", token ? token.value : "");
      if (!isRevert) {
        body.append("offset_ms", String(offsetMs));
      }
      return window
        .fetch(url, { method: "POST", body: body, credentials: "same-origin" })
        .then(function (res) {
          return res.text().then(function (text) {
            var data = {};
            try {
              data = JSON.parse(text);
            } catch (e) {
              data = {};
            }
            if (res.ok) {
              var m = /"mtime"\s*:\s*(\d+)/.exec(text);
              if (m) {
                st.mtime = m[1];
              }
              st.saved = isRevert ? 0 : offsetMs;
              st.offset = st.saved;
              st.edited = !isRevert;
              st.phase = isRevert ? "reverted" : "saved";
            } else {
              st.phase = { timing: "refused-timing", changed: "refused-changed", busy: "busy" }[data.error] || "error";
            }
            render();
          });
        })
        .catch(function (e) {
          console.error("preview.js: edit request failed", e && e.message);
          st.phase = "error";
          render();
        });
    }

    function requestSave() {
      if (save.disabled || locked()) {
        return;
      }
      if (skipAsking()) {
        post(panel.getAttribute("data-save-url"), st.offset, false);
        return;
      }
      $("mx-edit-confirm-offset").textContent = fmtOffset(st.offset) + " s";
      $("mx-edit-confirm-body").textContent = st.edited
        ? "This rewrites the lyrics file with the new timing. Your backup of the original already exists and is kept unchanged."
        : "This rewrites the lyrics file with the new timing. The original is first copied to a .lrc.orig file beside it, so Revert can always restore it.";
      dialog.showModal();
    }
    save.addEventListener("click", requestSave);
    revert.addEventListener("click", function () {
      post(panel.getAttribute("data-revert-url"), 0, true);
    });
    $("mx-edit-confirm-cancel").addEventListener("click", function () {
      dialog.close();
    });
    $("mx-edit-confirm-ok").addEventListener("click", function () {
      if ($("mx-edit-skip").checked) {
        try {
          window.localStorage.setItem(SKIP_KEY, "1");
        } catch (e) {
          // blocked storage only means the dialog appears again next time
        }
      }
      dialog.close();
      post(panel.getAttribute("data-save-url"), st.offset, false);
    });

    var kb = window.mxKeyboard;
    if (!kb) {
      console.error("preview.js: keyboard.js did not load; editor shortcuts are off");
    } else {
      var step = function (d) {
        return function () {
          if (!dialog.open) {
            setOffset(st.offset + d);
          }
        };
      };
      kb.register({
        keys: [" "],
        label: "play / pause",
        // keyboard.js leaves Space on a focused control (a button, or a lyric
        // line, which is role=button and seeks itself) to that control.
        handler: function () {
          if (audio.paused) {
            audio.play();
          } else {
            audio.pause();
          }
        },
      });
      kb.register({ keys: ["["], label: "-0.1 s", handler: step(-100) });
      kb.register({ keys: ["]"], label: "+0.1 s", handler: step(100) });
      kb.register({ keys: ["Shift", "["], label: "-1 s", handler: step(-1000) });
      kb.register({ keys: ["Shift", "]"], label: "+1 s", handler: step(1000) });
      kb.register({ keys: ["Alt", "["], label: "-0.01 s", handler: step(-10) });
      kb.register({ keys: ["Alt", "]"], label: "+0.01 s", handler: step(10) });
      kb.register({
        keys: ["Mod", "S"],
        label: "save",
        allowInInput: true,
        handler: function () {
          if (!dialog.open) {
            requestSave();
          }
        },
      });
      kb.register({
        keys: ["Escape"],
        label: "discard",
        allowInInput: true,
        handler: function () {
          if (dialog.open) {
            dialog.close();
          } else {
            discard.click();
          }
        },
      });
      kb.renderLegend($("mx-preview-keys"));
    }
    render();
  }

  function init() {
    var audio = document.getElementById("mx-preview-audio");
    var list = document.getElementById("mx-preview-lyrics");
    if (!audio || !list) {
      console.error("preview.js: missing #mx-preview-audio or #mx-preview-lyrics");
      return;
    }
    var lines = Array.prototype.slice.call(list.querySelectorAll(".mx-preview-line"));
    if (lines.length === 0) {
      console.error("preview.js: no .mx-preview-line elements to sync");
      return;
    }
    var lineStarts = lines.map(startOf);
    var lineWords = lines.map(function (li) {
      var words = Array.prototype.slice.call(li.querySelectorAll(".mx-preview-word"));
      return { words: words, starts: words.map(startOf) };
    });

    var curLine = -1;
    var curWord = -1;
    var following = true;
    var needScroll = false;
    var programmatic = false;
    var settleTimer = 0;
    var raf = 0;

    // markProgrammatic opens the window in which scroll events are the
    // script's own; it closes once they have been quiet for SETTLE_MS.
    var SETTLE_MS = 150;
    function markProgrammatic() {
      programmatic = true;
      window.clearTimeout(settleTimer);
      settleTimer = window.setTimeout(function () {
        programmatic = false;
      }, SETTLE_MS);
    }

    function resumeFollowing() {
      following = true;
      needScroll = true;
    }

    function setCurrent(els, idx, prev) {
      if (prev === idx) {
        return;
      }
      if (prev >= 0 && els[prev]) {
        els[prev].classList.remove("is-current");
      }
      if (idx >= 0) {
        els[idx].classList.add("is-current");
      }
    }

    function reducedMotion() {
      return !!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
    }

    function update() {
      var ms = audio.currentTime * 1000;
      var li = lastAtOrBefore(lineStarts, ms);
      if (li !== curLine) {
        // Words belong to the line, so leaving a line clears its word.
        if (curLine >= 0) {
          setCurrent(lineWords[curLine].words, -1, curWord);
        }
        curWord = -1;
        setCurrent(lines, li, curLine);
        curLine = li;
        needScroll = true;
      }
      if (needScroll && following && curLine >= 0) {
        needScroll = false;
        markProgrammatic();
        lines[curLine].scrollIntoView({ block: "center", behavior: reducedMotion() ? "auto" : "smooth" });
      }
      if (curLine >= 0) {
        var w = lineWords[curLine];
        var wi = maxAtOrBefore(w.starts, ms);
        setCurrent(w.words, wi, curWord);
        curWord = wi;
      }
    }

    function tick() {
      update();
      raf = audio.paused || audio.ended ? 0 : window.requestAnimationFrame(tick);
    }

    function startLoop() {
      if (!raf) {
        raf = window.requestAnimationFrame(tick);
      }
    }

    function seekTo(li) {
      audio.currentTime = lineStarts[li] / 1000;
      resumeFollowing();
      update();
    }

    lines.forEach(function (li, i) {
      li.setAttribute("tabindex", "0");
      li.setAttribute("role", "button");
      li.setAttribute("title", "Play from here");
      li.addEventListener("click", function () {
        seekTo(i);
      });
      li.addEventListener("keydown", function (ev) {
        if (ev.target === li && (ev.key === "Enter" || ev.key === " ")) {
          ev.preventDefault();
          seekTo(i);
        }
      });
    });

    function pauseFollowing() {
      following = false;
    }
    window.addEventListener("scroll", function () {
      if (!programmatic) {
        pauseFollowing();
        return;
      }
      markProgrammatic(); // still scrolling on our behalf: extend the window
    });
    window.addEventListener("wheel", pauseFollowing, { passive: true });
    window.addEventListener("touchmove", pauseFollowing, { passive: true });
    window.addEventListener("keydown", function (ev) {
      // A focused line handles Enter/Space itself; arrows and paging scroll.
      if (SCROLL_KEYS.indexOf(ev.key) >= 0 && !(ev.key === " " && ev.target.classList.contains("mx-preview-line"))) {
        pauseFollowing();
      }
    });

    audio.addEventListener("timeupdate", update);
    audio.addEventListener("seeked", function () {
      resumeFollowing();
      update();
    });
    audio.addEventListener("play", function () {
      resumeFollowing();
      startLoop();
    });
    audio.addEventListener("pause", update);
    audio.addEventListener("ended", update);
    update();

    var panel = document.getElementById("mx-edit");
    if (panel) {
      initEditor(panel, audio, lines, lineStarts, update);
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
