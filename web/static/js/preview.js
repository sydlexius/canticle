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

  // Find by ear (#1222): play one unit's snippet at the CURRENT shifted timing,
  // then the next unit clicked is the one actually heard, and the offset moves
  // by (played - heard). The flow knows nothing about lines: it takes a list of
  // timed units ({start} in shifted ms, UNCLAMPED so a start the offset pushed
  // below 0 still measures its true distance; plus whatever the caller paints),
  // a noun for the copy, the audio, and a frame clock seam, so word-level
  // timing can reuse it with a different list. Modes: off, pick, playing, answer.
  var SNIPPET_CAP_MS = 10000;
  // A rAF frame is ~17 ms at 60 Hz, so pausing two frames early keeps a late
  // frame from letting the next unit's first sound through.
  var STOP_EARLY_MS = 40;

  function createEar(o) {
    var mode = "off";
    var played = -1;
    var msg = "";
    var stopAt = 0;
    var frame = 0;
    var attempt = 0;
    var started = false;
    var noun = o.noun;

    function halt() {
      if (frame) {
        o.cancelFrame(frame);
        frame = 0;
      }
      if (!o.audio.paused) {
        o.audio.pause();
      }
    }
    function set(m, p, text) {
      mode = m;
      played = p;
      msg = text || "";
      o.onChange();
    }
    // bounds: from the unit's shifted start to the EARLIEST start strictly
    // after it anywhere in the list (word stamps can be out of order, so the
    // next element is not necessarily the next sound); with none, it plays to
    // the track end, capped. Both ends clamp at 0 for playback only.
    function bounds(i) {
      var u = o.units();
      var start = u[i].start;
      var end = Infinity;
      for (var j = 0; j < u.length; j++) {
        if (u[j].start > start && u[j].start < end) {
          end = u[j].start;
        }
      }
      var from = Math.max(0, start);
      if (end !== Infinity) {
        return [from, Math.max(0, end)];
      }
      var cap = from + SNIPPET_CAP_MS;
      return [from, o.durationMs > from ? Math.min(cap, o.durationMs) : cap];
    }
    function watch() {
      frame = 0;
      if (mode !== "playing") {
        return;
      }
      // Until play() settles, a paused element means "not started yet", not
      // "finished": treating it as finished would reach the answer step for a
      // snippet that may never sound.
      if (!started) {
        frame = o.frame(watch);
        return;
      }
      if (o.audio.paused || o.audio.currentTime * 1000 >= stopAt - STOP_EARLY_MS) {
        halt();
        set("answer", played);
        return;
      }
      frame = o.frame(watch);
    }
    function play(i) {
      var b = bounds(i);
      halt();
      stopAt = b[1];
      set("playing", i);
      o.audio.currentTime = b[0] / 1000;
      var token = ++attempt;
      var p = o.audio.play();
      started = !(p && p.then);
      frame = o.frame(watch);
      if (!started) {
        // A refused play (autoplay policy, unsupported source) never sounded,
        // so there is nothing to answer: back to pick with a retryable note.
        // The token drops a stale settle from an earlier snippet.
        p.then(function () {
          if (token === attempt) {
            started = true;
          }
        }, function (e) {
          // One failure, one log line (#1243): a media error already logged by
          // audioFailed owns it; otherwise this logs and tells the page so a
          // media error landing right behind it does not log a second time.
          if (!o.audio.error) {
            console.error("preview.js: snippet playback failed", e && e.message);
            if (o.onRejectLogged) {
              o.onRejectLogged();
            }
          }
          if (token !== attempt || !pending()) {
            return;
          }
          halt();
          set("pick", -1, "That " + noun + "'s snippet could not play" + (e && e.message ? " (" + e.message + ")" : "") + ". Click a " + noun + " to try again.");
        });
      }
    }
    function answer(h) {
      var u = o.units();
      var delta = u[played].start - u[h].start;
      halt();
      if (delta === 0) {
        set("pick", -1, "That " + noun + " was already in time, so the offset is unchanged.");
        return;
      }
      o.setOffset(o.getOffset() + delta);
      set("pick", -1, "Offset moved " + fmtOffset(delta) + " s, now " + fmtOffset(o.getOffset()) + " s. Test another " + noun + " to confirm it.");
    }
    function pending() {
      return mode === "playing" || mode === "answer";
    }

    return {
      pending: pending,
      on: function () {
        return mode !== "off";
      },
      toggle: function () {
        if (mode !== "off" || o.enabled()) {
          halt();
          set(mode === "off" ? "pick" : "off", -1);
        }
      },
      // activate routes a unit click; true means the mode consumed it.
      activate: function (i) {
        if (mode === "off") {
          return false;
        }
        if (o.enabled()) {
          if (mode === "pick") {
            play(i);
          } else {
            answer(i);
          }
        }
        return true;
      },
      replay: function () {
        if (pending()) {
          play(played);
        }
      },
      // cancel drops a pending test; with none pending it leaves the mode.
      cancel: function () {
        if (mode !== "off") {
          halt();
          set(pending() ? "pick" : "off", -1);
        }
      },
      view: function () {
        var v = { on: mode !== "off", played: played, playing: mode === "playing", step: mode === "answer" ? "2" : "1", replay: mode === "answer", cancel: mode === "pick" ? "Done" : "Cancel test" };
        if (mode === "pick") {
          v.title = "Click a " + noun + " to hear it";
          v.help = msg || "Only that " + noun + "'s stretch of the track plays, at the current timing.";
        } else if (mode === "playing") {
          v.title = "Playing that " + noun + "'s snippet";
          v.help = "Listen for which " + noun + " is actually sung. You can answer as soon as you know.";
        } else {
          v.title = "Click the " + noun + " you actually heard";
          v.help = "Heard the same " + noun + "? Click it again: its timing is already right.";
        }
        return v;
      },
    };
  }

  window.mxPreviewEdit = { parseOffset: parseOffset, pastEnd: pastEnd, createEar: createEar };

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
  function initEditor(panel, audio, lines, lineStarts, update, route, playback) {
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
      byEar: false,
    };
    var earBtn = $("mx-ear-toggle");
    var banner = $("mx-ear-banner");
    var earCancel = $("mx-ear-cancel");
    var earReplay = $("mx-ear-replay");
    var legend = $("mx-preview-keys");
    var escEntry = null;
    var ear = createEar({
      audio: audio,
      noun: earBtn.getAttribute("data-ear-unit"),
      durationMs: duration,
      // start is the UNCLAMPED shifted time: lineStarts clamps at 0 for
      // display and seek, which would understate the delta for a line the
      // offset pushed below 0. createEar clamps only the playback position.
      units: function () {
        return lines.map(function (li, i) {
          return { start: base[i] + st.offset, el: li };
        });
      },
      getOffset: function () {
        return st.offset;
      },
      setOffset: function (ms) {
        st.byEar = true;
        setOffset(ms);
      },
      enabled: function () {
        return !locked() && !playback.failed;
      },
      frame: function (fn) {
        return window.requestAnimationFrame(fn);
      },
      cancelFrame: function (id) {
        window.cancelAnimationFrame(id);
      },
      onChange: function () {
        render();
      },
      onRejectLogged: function () {
        playback.rejectLogged = true;
        window.setTimeout(function () {
          playback.rejectLogged = false;
        }, 250); // covers the gap between the rejection and the error event
      },
    });
    route.activate = ear.activate;
    earBtn.addEventListener("click", ear.toggle);
    earCancel.addEventListener("click", ear.cancel);
    earReplay.addEventListener("click", ear.replay);

    // Find by ear needs playback. When the audio failed it is disabled and its
    // hint says why, in place, so the control is never a silent dead end (#1243).
    var earHint = earBtn.parentNode && earBtn.parentNode.querySelector(".mx-edit-hint");
    var earHintText = earHint ? earHint.textContent : "";
    function paintEar(isLocked) {
      if ((isLocked || playback.failed) && ear.on()) {
        ear.toggle(); // re-renders with the mode off; a failure must not strand the mode on
      }
      var v = ear.view();
      earBtn.disabled = isLocked || playback.failed;
      if (earHint) {
        earHint.textContent = playback.failed ? "Unavailable: " + FAILURE_WORDS[playback.kind].hint + " You can still type an offset or use the nudges." : earHintText;
      }
      earBtn.setAttribute("aria-pressed", String(v.on));
      banner.hidden = !v.on;
      $("mx-ear-step").textContent = v.step;
      $("mx-ear-title").textContent = v.title;
      $("mx-ear-help").textContent = v.help;
      earReplay.hidden = !v.replay;
      earCancel.textContent = v.cancel;
      lines.forEach(function (li, i) {
        li.classList.toggle("is-ear-playing", v.playing && i === v.played);
        li.classList.toggle("is-ear-played", !v.playing && i === v.played);
      });
      var esc = ear.pending() ? "cancel test" : "discard";
      if (escEntry && escEntry.label !== esc) {
        escEntry.label = esc;
        window.mxKeyboard.renderLegend(legend);
      }
    }

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
      } else if (dirty && st.byEar) {
        msg = ["Offset set by ear. Play to check it, fine-tune with the nudges, then save. Nothing is written until you save.", ""];
      } else if (dirty) {
        msg = ["Play the track and nudge until the lines land. Nothing is written until you save.", ""];
      } else if (st.edited) {
        msg = ["This file is " + fmtOffset(st.saved).replace(/^[+-]/, "") + " s " + (st.saved > 0 ? "later" : "earlier") + " than the original.", ""];
      } else {
        msg = ["Matches the original file. Nudge while it plays to line the lyrics up.", ""];
      }
      if (playback.failed) {
        // The panel is always on screen (sticky on a phone), so it carries the
        // audio failure too, whatever the message tone, not only the message
        // beside the player (#1243).
        msg = [msg[0] + " " + FAILURE_WORDS[playback.kind].status + " so Find by ear is off.", msg[1]];
      }
      statusEl.textContent = msg[0];
      statusEl.className = "mx-edit-status" + (msg[1] ? " is-" + msg[1] : "");
      paintEar(isLocked);
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
        st.byEar = false;
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
              st.byEar = false;
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
      escEntry = {
        keys: ["Escape"],
        label: "discard",
        allowInInput: true,
        handler: function () {
          if (dialog.open) {
            dialog.close();
          } else if (ear.pending()) {
            ear.cancel(); // a pending test is dropped first; the offset stays
          } else {
            discard.click();
          }
        },
      };
      kb.register(escEntry);
      kb.register({
        keys: ["E"],
        label: "find by ear",
        handler: function () {
          if (!dialog.open) {
            ear.toggle();
          }
        },
      });
      kb.renderLegend(legend);
    }
    playback.onFail = render;
    render();
  }

  // audioFailure words a media error for the page: what the browser reported,
  // the file's format, and what still works. Codes 3 and 4 (decode, or no
  // supported source) are the unplayable-format case; 4 is also what a missing
  // file reports, so the text allows for it.
  var FAILURE_WORDS = {
    interrupted: { hint: "audio playback was interrupted.", status: "Audio playback was interrupted," },
    network: { hint: "the audio could not be loaded (network error).", status: "The audio could not be loaded (network error)," },
    unplayable: { hint: "this browser cannot play the audio.", status: "The audio cannot be played in this browser," },
    missing: { hint: "the audio file could not be found on the server.", status: "The audio file could not be found on the server," },
    server: { hint: "the server could not deliver the audio.", status: "The server could not deliver the audio," },
    session: { hint: "your session has expired. Reload the page and sign in again.", status: "Your session has expired," },
    refused: { hint: "the server refused to deliver the audio.", status: "The server refused to deliver the audio," },
  };

  // failureKind maps a media error code to the kind that words every surface.
  function failureKind(audio) {
    var code = audio.error && audio.error.code;
    return code === 1 ? "interrupted" : code === 2 ? "network" : "unplayable";
  }

  function audioFailure(audio) {
    var fmt = audio.getAttribute("data-format") || "unknown";
    var type = audio.getAttribute("data-type");
    var what = fmt + " file" + (type ? ", served as " + type : "");
    var code = audio.error && audio.error.code;
    if (code === 1) {
      return "Playback was interrupted (" + what + "). Reload the page to try again.";
    }
    if (code === 2) {
      return "The audio could not be loaded (network error, " + what + "). Reload the page to try again.";
    }
    return "This browser cannot play this track's audio (" + what + "), or the file could not be read. Try another browser, or play the file in a desktop player. The lyrics are shown, and an offset can still be typed.";
  }

  function init() {
    var audio = document.getElementById("mx-preview-audio");
    var list = document.getElementById("mx-preview-lyrics");
    if (!audio || !list) {
      console.error("preview.js: missing #mx-preview-audio or #mx-preview-lyrics");
      return;
    }
    var lines = Array.prototype.slice.call(list.querySelectorAll(".mx-preview-line"));
    // A stream the browser cannot decode (ALAC outside Safari, WMA, APE, ...)
    // otherwise just sits silent, so say so in the player and in the console
    // (#1243). The error may already have fired before this script ran. This is
    // wired before the no-lines early return so a lyric-less page still says so.
    var playback = { failed: false, kind: "", onFail: null };
    var errorBox = document.getElementById("mx-preview-audio-error");
    // The server renders data-flac-src only when the opt-in FLAC fallback is on
    // (#1243); the page never probes for it. The browser's playback error is the
    // sole trigger, and the retry happens once.
    var flacSrc = audio.getAttribute("data-flac-src");
    var flacTried = false;
    function retryAsFlac() {
      flacTried = true;
      console.warn("preview.js: audio failed to play; retrying once as a server-made FLAC conversion");
      if (errorBox) {
        errorBox.textContent = "Converting this track to FLAC so this browser can play it...";
        errorBox.hidden = false;
        audio.addEventListener("loadedmetadata", function () {
          errorBox.hidden = true;
        }, { once: true });
      }
      audio.src = flacSrc;
      audio.load();
    }
    // probeAudio asks the audio route what it says about the file with one
    // Range GET for the first byte (#1261): the route is a ServeContent
    // handler, so a hit answers 206 and a refusal answers a bare 404. The
    // X-Requested-With header makes the session guard answer 401 instead of a
    // 303 to the login page, which fetch would follow and report as a 200.
    // It resolves to {status, redirected}; status is 0 when the request failed
    // (or was aborted by the timeout).
    function probeAudio(url, signal) {
      var opts = { headers: { Range: "bytes=0-0", "X-Requested-With": "XMLHttpRequest" }, credentials: "same-origin", cache: "no-store" };
      if (signal) {
        opts.signal = signal;
      }
      return window.fetch(url, opts).then(function (res) {
        return { status: res.status, redirected: res.redirected === true };
      }, function (e) {
        console.error("preview.js: audio check request failed", e && e.message);
        return { status: 0, redirected: false };
      });
    }
    // PROBE_TIMEOUT_MS bounds the probe so a stalled answer cannot leave the
    // player with no message. The route opens the file before it answers, and
    // a spun-down disk can take several seconds to wake, so 8 s outlasts a
    // normal spin-up while still ending the wait in a bounded time.
    var PROBE_TIMEOUT_MS = 8000;
    var probing = false;
    function settleFailure(quiet) {
      var msg = audioFailure(audio);
      if (flacTried) {
        msg += " The FLAC conversion did not play either.";
      }
      showFailure(msg, failureKind(audio), quiet);
    }
    function audioFailed() {
      if (playback.failed || probing) {
        return; // the event can follow an audio.error already handled below
      }
      var code = audio.error && audio.error.code;
      // A network (2) or unsupported-source (4) error is also what a refused
      // file reports, so ask the route before blaming the browser (#1261). The
      // FLAC retry's own failure is judged as before.
      if ((code === 2 || code === 4) && !flacTried) {
        var url = audio.getAttribute("src");
        if (typeof window.fetch !== "function") {
          console.error("preview.js: fetch is unavailable; cannot check why the audio did not play");
        } else if (!url) {
          console.error("preview.js: audio element has no src; cannot check why the audio did not play");
        } else {
          probing = true;
          // Captured now so a snippet rejection that logged before this media
          // error still suppresses the second log however slow the probe is.
          var quiet = playback.rejectLogged;
          var done = false;
          var timer = 0;
          var finish = function (r) {
            if (done) {
              return;
            }
            done = true;
            window.clearTimeout(timer);
            probing = false;
            // Stale: the source changed while the check was in flight.
            if (audio.getAttribute("src") !== url) {
              return;
            }
            var status = r.status;
            if (r.redirected || status === 401 || status === 403) {
              showFailure("Your session has expired. Reload the page and sign in again.", "session", quiet);
            } else if (status === 404) {
              showFailure("The audio file could not be found on the server (it may have moved since the last scan).", "missing", quiet);
            } else if (status >= 500) {
              showFailure("The server could not deliver the audio (error " + status + "). Reload the page to try again.", "server", quiet);
            } else if (status !== 0 && status !== 200 && status !== 206) {
              showFailure("The audio could not be loaded (error " + status + "). Reload the page to try again.", "refused", quiet);
            } else if (flacSrc && code === 4) {
              retryAsFlac();
            } else {
              settleFailure(quiet);
            }
          };
          var ctl = null;
          if (typeof window.AbortController === "function") {
            ctl = new window.AbortController();
          } else {
            console.error("preview.js: AbortController is unavailable; the audio check cannot be cancelled, only timed out");
          }
          timer = window.setTimeout(function () {
            console.error("preview.js: audio check timed out");
            if (ctl) {
              ctl.abort(); // the fetch rejects and finish() runs with status 0
            } else {
              finish({ status: 0, redirected: false });
            }
          }, PROBE_TIMEOUT_MS);
          probeAudio(url, ctl && ctl.signal).then(finish);
          return;
        }
      }
      // Only a decode or unsupported-source error (3, 4) earns the conversion
      // (4 reaches here only when the check above could not run): never a
      // network or interrupted error, and never a missing or unknown code,
      // which says nothing about the format.
      if (flacSrc && !flacTried && (code === 3 || code === 4)) {
        retryAsFlac();
        return;
      }
      settleFailure();
    }
    function showFailure(msg, kind, quiet) {
      if (quiet === undefined) {
        quiet = playback.rejectLogged;
      }
      if (!quiet) {
        console.error("preview.js: audio failed: " + msg + " (media error code " + (audio.error ? audio.error.code : "none") + ")");
      }
      playback.failed = true;
      playback.kind = kind;
      if (errorBox) {
        errorBox.textContent = msg;
        errorBox.hidden = false;
      } else {
        console.error("preview.js: missing #mx-preview-audio-error; failure not shown on the page");
      }
      if (playback.onFail) {
        playback.onFail();
      }
    }
    audio.addEventListener("error", audioFailed);
    // A <source> child reports its error on itself and does not bubble.
    Array.prototype.forEach.call(audio.querySelectorAll("source"), function (src) {
      src.addEventListener("error", audioFailed);
    });
    if (audio.error) {
      audioFailed();
    }

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

    // route lets the editor's find-by-ear mode claim line activations: while
    // it is on, a click or Enter/Space is an ear answer, never a seek.
    var route = { activate: null };
    function seekTo(li) {
      if (route.activate && route.activate(li)) {
        return;
      }
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
      initEditor(panel, audio, lines, lineStarts, update, route, playback);
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
