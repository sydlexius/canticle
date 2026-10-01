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
(function () {
  "use strict";

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
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
