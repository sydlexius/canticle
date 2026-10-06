// preview.js (#481): current line/word highlighting, auto-scroll, click/keyboard seek.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { afterEach, describe, expect, it, vi } from "vitest";

const JS = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "preview.js"), "utf8");
const KB = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "keyboard.js"), "utf8");

// Every window load() creates. preview.js runs a rAF loop while the audio
// reports playing, and jsdom keeps a window (and its timers) alive until it is
// closed, so each test's window is closed afterwards, pass or fail.
const openWindows = [];
afterEach(() => {
  while (openWindows.length > 0) {
    openWindows.pop().close();
  }
});

const PAGE = `<body>
  <audio id="mx-preview-audio"></audio>
  <ol id="mx-preview-lyrics">
    <li class="mx-preview-line" data-start-ms="1000"><span class="mx-preview-word" data-start-ms="1000">a</span> <span class="mx-preview-word" data-start-ms="1500">b</span></li>
    <li class="mx-preview-line" data-start-ms="3000">second</li>
    <li class="mx-preview-line mx-preview-line-decorative" data-start-ms="5000">***</li>
  </ol></body>`;

function load({ html = PAGE, reduce = false, setup = null } = {}) {
  const dom = new JSDOM(html, { runScripts: "outside-only", pretendToBeVisual: true, url: "http://localhost/" });
  const win = dom.window;
  openWindows.push(win);
  // jsdom reports readyState "loading" until its own async DOMContentLoaded, so
  // evaluate the script as a deferred script would: after the document is parsed.
  Object.defineProperty(win.document, "readyState", { get: () => "complete" });
  const errors = [];
  win.console.error = (...a) => errors.push(a.join(" "));
  win.matchMedia = () => ({ matches: reduce });
  const scrolls = [];
  win.HTMLElement.prototype.scrollIntoView = function (opts) {
    scrolls.push({ text: this.textContent, opts });
  };
  const audio = win.document.getElementById("mx-preview-audio");
  let t = 0;
  if (audio) {
    Object.defineProperty(audio, "currentTime", { get: () => t, set: (v) => (t = v) });
  }
  if (setup) {
    setup(win);
  }
  win.eval(JS);
  const at = (sec) => {
    t = sec;
    audio.dispatchEvent(new win.Event("timeupdate"));
  };
  const current = (sel) =>
    Array.from(win.document.querySelectorAll(`${sel}.is-current`)).map((e) => e.textContent);
  return { win, doc: win.document, audio, at, current, errors, scrolls, time: () => t };
}

describe("preview.js highlighting", () => {
  it("marks nothing before the first line", () => {
    const p = load();
    p.at(0.5);
    expect(p.current(".mx-preview-line")).toEqual([]);
    expect(p.current(".mx-preview-word")).toEqual([]);
  });

  it("marks the last line and word whose start has passed, one at a time", () => {
    const p = load();
    p.at(1.2);
    expect(p.current(".mx-preview-line")).toEqual(["a b"]);
    expect(p.current(".mx-preview-word")).toEqual(["a"]);
    p.at(1.6);
    expect(p.current(".mx-preview-word")).toEqual(["b"]);
    p.at(3.1);
    expect(p.current(".mx-preview-line")).toEqual(["second"]);
    expect(p.current(".mx-preview-word")).toEqual([]);
  });

  it("picks the greatest word start when word timestamps are out of order", () => {
    const html = `<body><audio id="mx-preview-audio"></audio><ol id="mx-preview-lyrics">
      <li class="mx-preview-line" data-start-ms="1000"><span class="mx-preview-word" data-start-ms="1500">late</span> <span class="mx-preview-word" data-start-ms="1000">early</span></li></ol></body>`;
    const p = load({ html });
    p.at(1.6);
    expect(p.current(".mx-preview-word")).toEqual(["late"]);
    p.at(1.2);
    expect(p.current(".mx-preview-word")).toEqual(["early"]);
  });

  it("clears the highlight when seeking back before the first line", () => {
    const p = load();
    p.at(4);
    p.at(0);
    expect(p.current(".is-current")).toEqual([]);
  });

  it("handles line-only pages and decorative lines", () => {
    const p = load();
    p.at(6);
    expect(p.current(".mx-preview-line")).toEqual(["***"]);
  });

  it("follows currentTime from rAF while playing, with no timeupdate", async () => {
    const p = load();
    Object.defineProperty(p.audio, "paused", { get: () => false });
    p.audio.currentTime = 3.1;
    p.audio.dispatchEvent(new p.win.Event("play"));
    await new Promise((r) => p.win.requestAnimationFrame(() => r()));
    expect(p.current(".mx-preview-line")).toEqual(["second"]);
  });
});

describe("preview.js scrolling", () => {
  it("scrolls the new current line to centre, smooth by default", () => {
    const p = load();
    p.at(3.1);
    expect(p.scrolls).toEqual([{ text: "second", opts: { block: "center", behavior: "smooth" } }]);
  });

  it("uses instant scroll under prefers-reduced-motion", () => {
    const p = load({ reduce: true });
    p.at(3.1);
    expect(p.scrolls[0].opts.behavior).toBe("auto");
  });

  it("stops following after a manual scroll until the next seek", () => {
    const p = load();
    p.at(1.1);
    p.win.dispatchEvent(new p.win.Event("wheel"));
    p.at(3.1);
    expect(p.scrolls).toHaveLength(1);
    p.audio.dispatchEvent(new p.win.Event("seeked"));
    p.at(5.1);
    expect(p.scrolls).toHaveLength(3); // initial, resume on seeked, then the line change
  });

  it("scrolls the current line back into view when following resumes within it", () => {
    const p = load();
    p.at(1.1);
    p.win.dispatchEvent(new p.win.Event("wheel"));
    p.at(1.3); // same line, still paused: no scroll
    expect(p.scrolls).toHaveLength(1);
    p.at(1.4);
    p.audio.dispatchEvent(new p.win.Event("seeked")); // seek within the same line
    expect(p.scrolls).toHaveLength(2);
    expect(p.scrolls[1].text).toBe("a b");
    p.at(1.5); // once, not every frame
    expect(p.scrolls).toHaveLength(2);
  });

  it("pauses following on a scroll the script did not cause, not on its own", async () => {
    const p = load();
    p.at(1.1); // auto-scroll opens the programmatic window
    p.win.dispatchEvent(new p.win.Event("scroll")); // its own smooth scroll
    p.at(3.1);
    expect(p.scrolls).toHaveLength(2);
    await new Promise((r) => setTimeout(r, 300));
    p.win.dispatchEvent(new p.win.Event("scroll")); // scrollbar drag
    p.at(5.1);
    expect(p.scrolls).toHaveLength(2);
  });

  it("does not pause following for Space on a focused line", () => {
    const p = load();
    p.at(1.1);
    const line = p.doc.querySelectorAll(".mx-preview-line")[1];
    line.dispatchEvent(new p.win.KeyboardEvent("keydown", { key: " ", bubbles: true }));
    expect(p.time()).toBe(3);
    p.doc.body.dispatchEvent(new p.win.KeyboardEvent("keydown", { key: "PageDown", bubbles: true }));
    p.at(5.1);
    expect(p.scrolls).toHaveLength(2); // initial + the seek; PageDown paused the rest
  });
});

describe("preview.js seeking", () => {
  it("seeks to the line start on click and highlights it", () => {
    const p = load();
    p.doc.querySelectorAll(".mx-preview-line")[1].click();
    expect(p.time()).toBe(3);
    expect(p.current(".mx-preview-line")).toEqual(["second"]);
  });

  it("marks lines as buttons for assistive tech", () => {
    const p = load();
    expect(p.doc.querySelectorAll('.mx-preview-line[role="button"]')).toHaveLength(3);
  });

  it("makes lines focusable and seeks on Enter, ignoring other keys", () => {
    const p = load();
    const line = p.doc.querySelectorAll(".mx-preview-line")[2];
    expect(line.getAttribute("tabindex")).toBe("0");
    line.dispatchEvent(new p.win.KeyboardEvent("keydown", { key: "a", bubbles: true }));
    expect(p.time()).toBe(0);
    line.dispatchEvent(new p.win.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    expect(p.time()).toBe(5);
  });
});

describe("preview.js missing elements", () => {
  it("fails loudly without audio or lyrics", () => {
    const p = load({ html: "<body></body>" });
    expect(p.errors.join()).toContain("missing #mx-preview-audio");
  });

  it("fails loudly with no lines", () => {
    const p = load({ html: '<body><audio id="mx-preview-audio"></audio><ol id="mx-preview-lyrics"></ol></body>' });
    expect(p.errors.join()).toContain("no .mx-preview-line");
  });
});

describe("offset editor helpers", () => {
  const EDITOR = `<body>
  <audio id="mx-preview-audio"></audio>
  <section id="mx-edit" data-save-url="/preview/7/offset" data-revert-url="/preview/7/revert"
    data-offset-ms="OFFSET" EDITED data-mtime="1700000000000000001" data-duration-ms="DURATION"
    data-orig-ms="ORIG" data-tolerance-ms="TOLERANCE">
    <input type="hidden" name="csrf_token" value="tok">
    <span id="mx-edit-chip"></span><input id="mx-edit-offset"><input id="mx-edit-slider" type="range">
    <button class="mx-edit-nudge" data-delta="-1000">-1s</button><button class="mx-edit-nudge" data-delta="-100">-0.1</button>
    <button class="mx-edit-nudge" data-delta="100">+0.1</button><button class="mx-edit-nudge" data-delta="1000">+1s</button>
    <button id="mx-edit-save"></button><button id="mx-edit-discard"></button><button id="mx-edit-revert" hidden></button>
    <button id="mx-ear-toggle" class="mx-ear-toggle" data-ear-unit="line" aria-pressed="false"></button>
    MORE<p id="mx-edit-status"></p>
    <dialog id="mx-edit-confirm"><span id="mx-edit-confirm-offset"></span><p id="mx-edit-confirm-body"></p>
      <input id="mx-edit-skip" type="checkbox"><button id="mx-edit-confirm-cancel"></button><button id="mx-edit-confirm-ok"></button></dialog>
  </section>
  <div id="mx-ear-banner" hidden><span id="mx-ear-step"></span><span id="mx-ear-title"></span><span id="mx-ear-help"></span>
    <button id="mx-ear-replay" hidden></button><button id="mx-ear-cancel"></button></div>
  <p id="mx-preview-keys"></p>
  <ol id="mx-preview-lyrics">LINES</ol></body>`;

  // mountEditor builds the editor markup subset around the given line starts.
  // orig defaults to the shown starts (an unedited file); toleranceMs to timing.Tolerance.
  function mountEditor({ durationMs = 30000, starts = [1000], orig = null, toleranceMs = 2000, savedOffsetMs = 0, edited = false, fetchImpl = null, decorative = [], keyboard = false, more = false, observer = true } = {}) {
    const lines = starts
      .map((ms, i) => `<li class="mx-preview-line${decorative.includes(i) ? " mx-preview-line-decorative" : ""}" data-start-ms="${ms}">l${ms}</li>`)
      .join("");
    const html = EDITOR.replace("OFFSET", String(savedOffsetMs))
      .replace("EDITED", edited ? "data-edited" : "")
      .replace("MORE", more ? '<button id="mx-edit-more" aria-expanded="false">More</button>' : "")
      .replace("DURATION", String(durationMs))
      .replace("ORIG", (orig || starts).join(","))
      .replace("TOLERANCE", String(toleranceMs))
      .replace("LINES", lines);
    const observed = [];
    const p = load({
      html,
      setup: (win) => {
        // jsdom has no ResizeObserver: record the callbacks so a test can fire one.
        if (observer) {
          win.ResizeObserver = class {
            constructor(cb) {
              observed.push(cb);
            }
            observe() {}
          };
        }
        // jsdom has no <dialog> behavior: model the two calls the editor makes.
        win.HTMLDialogElement.prototype.showModal = function () {
          this.setAttribute("open", "");
        };
        win.HTMLDialogElement.prototype.close = function () {
          this.removeAttribute("open");
        };
        if (fetchImpl) {
          win.fetch = fetchImpl;
        }
        // jsdom has no media playback: model play/pause as a paused flag.
        let paused = true;
        Object.defineProperty(win.HTMLMediaElement.prototype, "paused", { get: () => paused });
        win.HTMLMediaElement.prototype.play = () => {
          paused = false;
          return Promise.resolve();
        };
        win.HTMLMediaElement.prototype.pause = () => {
          paused = true;
        };
        if (keyboard) {
          win.eval(KB);
        }
      },
    });
    const $ = (id) => p.doc.getElementById(id);
    const nudge = (label) =>
      Array.from(p.doc.querySelectorAll(".mx-edit-nudge")).find((b) => b.textContent === label).click();
    const times = () => Array.from(p.doc.querySelectorAll(".mx-preview-time")).map((e) => e.textContent);
    // layout stubs jsdom's missing geometry: the player's bottom edge (document
    // coordinates), the bar's content height and the viewport height.
    const layout = ({ playerBottom, barHeight, viewport, bannerBottom = 0 }) => {
      p.win.innerHeight = viewport;
      $("mx-ear-banner").getBoundingClientRect = () => ({ bottom: bannerBottom });
      $("mx-preview-audio").getBoundingClientRect = () => ({ bottom: playerBottom });
      Object.defineProperty($("mx-edit"), "scrollHeight", { configurable: true, get: () => barHeight });
    };
    return { ...p, $, nudge, times, observed, layout };
  }
  const reply = (status, text) =>
    vi.fn(() => Promise.resolve({ ok: status === 200, status, text: () => Promise.resolve(text) }));

  describe("phone expand control (#1247)", () => {
    it("toggles the panel class and aria-expanded, and relabels itself", () => {
      const e = mountEditor({ more: true, keyboard: true });
      const btn = e.$("mx-edit-more");
      btn.click();
      expect(e.$("mx-edit").classList.contains("is-open")).toBe(true);
      expect(btn.getAttribute("aria-expanded")).toBe("true");
      expect(btn.textContent).toBe("Less");
      btn.click();
      expect(e.$("mx-edit").classList.contains("is-open")).toBe(false);
      expect(btn.getAttribute("aria-expanded")).toBe("false");
      expect(btn.textContent).toBe("More");
      expect(e.errors).toEqual([]);
    });

    it("leaves a panel without the control alone", () => {
      const e = mountEditor({ keyboard: true });
      e.$("mx-edit").click();
      expect(e.$("mx-edit").classList.contains("is-open")).toBe(false);
      expect(e.errors).toEqual([]);
    });

    it("does not toggle on a click elsewhere in a panel that has the control", () => {
      const e = mountEditor({ more: true, keyboard: true });
      e.nudge("+0.1");
      e.$("mx-edit").click();
      e.$("mx-edit-save").click();
      expect(e.$("mx-edit").classList.contains("is-open")).toBe(false);
      expect(e.$("mx-edit-more").getAttribute("aria-expanded")).toBe("false");
    });

    it("swaps the accessible label with the state", () => {
      const e = mountEditor({ more: true, keyboard: true });
      const btn = e.$("mx-edit-more");
      btn.click();
      expect(btn.getAttribute("aria-label")).toBe("Fewer timing controls");
      btn.click();
      expect(btn.getAttribute("aria-label")).toBe("More timing controls");
    });

    it("collapses when By ear turns on, by button and by the E shortcut", () => {
      for (const start of [(e) => e.$("mx-ear-toggle").click(), (e) => e.doc.body.dispatchEvent(new e.win.KeyboardEvent("keydown", { key: "e", bubbles: true }))]) {
        const e = mountEditor({ more: true, keyboard: true, starts: [1000, 3000] });
        e.$("mx-edit-more").click();
        expect(e.$("mx-edit").classList.contains("is-open")).toBe(true);
        start(e);
        expect(e.$("mx-ear-toggle").getAttribute("aria-pressed")).toBe("true");
        expect(e.$("mx-edit").classList.contains("is-open")).toBe(false);
        expect(e.$("mx-edit-more").getAttribute("aria-expanded")).toBe("false");
        expect(e.$("mx-edit-more").textContent).toBe("More");
      }
    });

    it("keeps focus on the By ear toggle when the panel collapses, since the collapsed bar shows it", () => {
      const e = mountEditor({ more: true, keyboard: true });
      e.$("mx-edit-more").click();
      const toggle = e.$("mx-ear-toggle");
      toggle.focus();
      e.$("mx-edit-more").click(); // collapse while the toggle holds focus
      expect(e.$("mx-edit").classList.contains("is-open")).toBe(false);
      expect(e.doc.activeElement).toBe(toggle);
    });

    it("keeps focus on the By ear toggle when it is turned off while collapsed", () => {
      const e = mountEditor({ more: true, keyboard: true, starts: [1000, 3000] });
      const toggle = e.$("mx-ear-toggle");
      e.$("mx-edit-more").click();
      toggle.focus();
      toggle.click(); // turning it on collapses the open panel under the focused toggle
      expect(toggle.getAttribute("aria-pressed")).toBe("true");
      expect(e.doc.activeElement).toBe(toggle);
      toggle.click();
      expect(toggle.getAttribute("aria-pressed")).toBe("false");
      expect(e.$("mx-edit").classList.contains("is-open")).toBe(false);
      expect(e.doc.activeElement).toBe(toggle);
    });

    it("moves focus to More when collapsing from a control the collapsed bar hides", () => {
      const e = mountEditor({ more: true, keyboard: true });
      const btn = e.$("mx-edit-more");
      btn.click();
      const far = Array.from(e.doc.querySelectorAll(".mx-edit-nudge")).find((b) => b.dataset.delta === "1000");
      far.focus();
      expect(e.doc.activeElement).toBe(far);
      btn.click();
      expect(e.doc.activeElement).toBe(btn);
    });

    it("leaves focus alone when it is on a control the collapsed bar keeps", () => {
      const e = mountEditor({ more: true, keyboard: true });
      const btn = e.$("mx-edit-more");
      btn.click();
      const near = Array.from(e.doc.querySelectorAll(".mx-edit-nudge")).find((b) => b.dataset.delta === "100");
      near.focus();
      btn.click();
      expect(e.doc.activeElement).toBe(near);
    });

    it("tones the status for a save result so the collapsed bar can keep it", async () => {
      const f = reply(200, '{"offset_ms":100,"mtime":1700000000000000002,"created_orig":true}');
      const e = mountEditor({ starts: [1000], fetchImpl: f });
      e.win.localStorage.setItem("mx-offset-confirm-skip", "1");
      e.nudge("+0.1");
      e.$("mx-edit-save").click();
      await vi.waitFor(() => expect(e.$("mx-edit-status").textContent).toContain("Saved."));
      expect(e.$("mx-edit-status").classList.contains("is-ok")).toBe(true);
      e.nudge("+0.1");
      expect(e.$("mx-edit-status").classList.contains("is-ok")).toBe(false);
    });
  });

  describe("phone bar clears the player (#1247)", () => {
    const unpinned = (e) => e.$("mx-edit").classList.contains("is-unpinned");

    it("pinFits: the bar fits only when it ends below the player plus the gap", () => {
      const { pinFits } = mountEditor().win.mxPreviewEdit;
      expect(pinFits(300, 150, 568)).toBe(true); // 308 <= 418
      expect(pinFits(300, 260, 568)).toBe(true); // 308 <= 308: exactly fits
      expect(pinFits(300, 261, 568)).toBe(false);
      expect(pinFits(300, 900, 568)).toBe(false); // capped at 70% of the viewport: 568 - 397.6 = 170
      expect(pinFits(150, 900, 568)).toBe(true); // a short header leaves room even for the capped bar
    });

    it("pinFits: a showing banner must also clear the bar (#1273)", () => {
      const { pinFits } = mountEditor().win.mxPreviewEdit;
      expect(pinFits(300, 150, 568, 0)).toBe(true);
      expect(pinFits(300, 150, 568, 420)).toBe(false); // 428 > 418
      expect(pinFits(300, 150, 568, 410)).toBe(true);
    });

    it("unpins while By ear is on and its banner would sit under the bar, and re-pins when it ends", () => {
      const e = mountEditor({ more: true, keyboard: true, starts: [1000] });
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568, bannerBottom: 450 });
      e.nudge("+0.1");
      expect(unpinned(e)).toBe(false); // banner hidden: not counted
      e.$("mx-ear-toggle").click();
      expect(e.$("mx-ear-banner").hidden).toBe(false);
      expect(unpinned(e)).toBe(true);
      e.$("mx-ear-toggle").click();
      expect(e.$("mx-ear-banner").hidden).toBe(true);
      expect(unpinned(e)).toBe(false);
      expect(e.errors).toEqual([]);
    });

    it("moves focus from the By ear toggle to Cancel when turning it on unpins the bar (#1273)", () => {
      const e = mountEditor({ more: true, keyboard: true, starts: [1000] });
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568, bannerBottom: 450 });
      const toggle = e.$("mx-ear-toggle");
      toggle.focus();
      toggle.click();
      expect(unpinned(e)).toBe(true);
      expect(e.doc.activeElement).toBe(e.$("mx-ear-cancel"));
    });

    it("leaves focus on the By ear toggle when the bar stays pinned (#1273)", () => {
      const e = mountEditor({ more: true, keyboard: true, starts: [1000] });
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568, bannerBottom: 380 });
      const toggle = e.$("mx-ear-toggle");
      toggle.focus();
      toggle.click();
      expect(unpinned(e)).toBe(false);
      expect(e.doc.activeElement).toBe(toggle);
    });

    it("does not steal focus on the off transition or when focus is elsewhere (#1273)", () => {
      const e = mountEditor({ more: true, keyboard: true, starts: [1000] });
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568, bannerBottom: 450 });
      const toggle = e.$("mx-ear-toggle");
      toggle.click(); // focus never on the toggle
      expect(unpinned(e)).toBe(true);
      expect(e.doc.activeElement).not.toBe(e.$("mx-ear-cancel"));
      toggle.focus();
      toggle.click(); // off: bar re-pins, focus stays on the toggle
      expect(unpinned(e)).toBe(false);
      expect(e.doc.activeElement).toBe(toggle);
    });

    it("only moves focus on the on transition, not on later renders while unpinned (#1273)", () => {
      const e = mountEditor({ more: true, keyboard: true, starts: [1000] });
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568, bannerBottom: 450 });
      e.$("mx-ear-toggle").click();
      expect(unpinned(e)).toBe(true);
      const toggle = e.$("mx-ear-toggle");
      toggle.focus();
      e.nudge("+0.1"); // re-renders with By ear still on
      expect(e.doc.activeElement).toBe(toggle);
    });

    it("stays pinned while By ear is on when the banner clears the bar", () => {
      const e = mountEditor({ more: true, keyboard: true, starts: [1000] });
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568, bannerBottom: 380 });
      e.$("mx-ear-toggle").click();
      expect(e.$("mx-ear-banner").hidden).toBe(false);
      expect(unpinned(e)).toBe(false);
    });

    it("stays pinned while the bar fits, unpins when it would intersect, and re-pins when it shrinks", () => {
      const e = mountEditor({ more: true, keyboard: true, starts: [1000] });
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568 });
      e.nudge("+0.1");
      expect(unpinned(e)).toBe(false);
      e.layout({ playerBottom: 300, barHeight: 270, viewport: 568 }); // a long status grew the bar
      e.nudge("+0.1");
      expect(unpinned(e)).toBe(true);
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568 });
      e.nudge("+0.1");
      expect(unpinned(e)).toBe(false);
      expect(e.errors).toEqual([]);
    });

    it("re-evaluates when the observed layout changes and on resize, with no render", () => {
      const e = mountEditor({ more: true, keyboard: true });
      e.layout({ playerBottom: 400, barHeight: 150, viewport: 568 }); // 408 <= 418: fits
      e.observed.forEach((cb) => cb());
      expect(unpinned(e)).toBe(false);
      e.layout({ playerBottom: 430, barHeight: 150, viewport: 568 }); // an error notice pushed the player down
      e.observed.forEach((cb) => cb());
      expect(unpinned(e)).toBe(true);
      e.layout({ playerBottom: 100, barHeight: 150, viewport: 568 });
      e.win.dispatchEvent(new e.win.Event("resize"));
      expect(unpinned(e)).toBe(false);
    });

    it("unpins an expanded panel that does not fit and brings it into view", () => {
      const e = mountEditor({ more: true, keyboard: true });
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568 });
      e.nudge("+0.1");
      e.layout({ playerBottom: 300, barHeight: 400, viewport: 568 }); // the open panel is taller
      e.$("mx-edit-more").click();
      expect(unpinned(e)).toBe(true);
      expect(e.scrolls.map((s) => s.opts)).toEqual([{ block: "nearest" }]);
      e.layout({ playerBottom: 300, barHeight: 150, viewport: 568 });
      e.$("mx-edit-more").click(); // collapsing measures the short bar again
      expect(unpinned(e)).toBe(false);
    });

    it("fails loudly when ResizeObserver is missing", () => {
      const e = mountEditor({ more: true, keyboard: true, observer: false });
      expect(e.errors.some((m) => m.includes("ResizeObserver is missing"))).toBe(true);
    });
  });

  describe("More while By ear is on (#1247)", () => {
    it("disables More and refuses an expand, so the open panel never covers the banner", () => {
      const e = mountEditor({ more: true, keyboard: true, starts: [1000, 3000] });
      const btn = e.$("mx-edit-more");
      e.$("mx-ear-toggle").click();
      expect(btn.disabled).toBe(true);
      btn.click();
      btn.dispatchEvent(new e.win.MouseEvent("click", { bubbles: true })); // the guard holds even if a click gets through
      expect(e.$("mx-edit").classList.contains("is-open")).toBe(false);
      expect(btn.getAttribute("aria-expanded")).toBe("false");
      expect(e.$("mx-ear-banner").hidden).toBe(false);
      e.$("mx-ear-toggle").click(); // By ear off: More works again
      expect(btn.disabled).toBe(false);
      btn.click();
      expect(e.$("mx-edit").classList.contains("is-open")).toBe(true);
    });
  });

  describe("find by ear (unit-agnostic core)", () => {
    // fakeEar drives createEar with a synthetic unit list (not lines: plain
    // objects with no element), a fake audio and a hand-cranked frame clock.
    // playImpl, when given, replaces play() (e.g. a rejecting promise).
    function fakeEar(starts, { durationMs = 0, offset = 0, enabled = true, playImpl = null } = {}) {
      const loaded = load();
      const { createEar } = loaded.win.mxPreviewEdit;
      const audio = { currentTime: 0, paused: true, plays: [], play() { this.paused = false; this.plays.push(this.currentTime); }, pause() { this.paused = true; } };
      if (playImpl) {
        audio.play = function () {
          this.plays.push(this.currentTime);
          return playImpl.call(this);
        };
      }
      const frames = [];
      const cancelled = [];
      const st = { offset, enabled };
      const ear = createEar({
        audio,
        noun: "word",
        durationMs,
        units: () => starts.map((s) => ({ start: s + st.offset })),
        getOffset: () => st.offset,
        setOffset: (ms) => (st.offset = ms),
        enabled: () => st.enabled,
        frame: (fn) => frames.push(fn),
        cancelFrame: (id) => cancelled.push(id),
        onChange: () => {},
      });
      // run advances the audio clock to ms and fires the queued frames,
      // returning the furthest time the audio reached while still playing.
      const run = (stepMs) => {
        let reached = audio.currentTime * 1000;
        for (let n = 0; n < 1000 && frames.length > 0; n++) {
          frames.shift()();
          if (audio.paused) break;
          reached = audio.currentTime * 1000 + stepMs;
          audio.currentTime = reached / 1000;
        }
        return reached;
      };
      return { ear, audio, frames, cancelled, st, run, errors: loaded.errors };
    }

    it("stops at the earliest later start, not the next element, when units are out of order", () => {
      // word stamps can be non-monotonic: after 1000 the next SOUND is 2000,
      // even though the next element in the list is 5000.
      const f = fakeEar([1000, 5000, 2000]);
      f.ear.toggle();
      f.ear.activate(0);
      const reached = f.run(17);
      expect(reached).toBeLessThan(2000);
      expect(reached).toBeGreaterThan(2000 - 100);
      // a unit with nothing later still falls back to the track end, capped
      const last = fakeEar([1000, 5000, 2000], { durationMs: 7000 });
      last.ear.toggle();
      last.ear.activate(1);
      const r = last.run(17);
      expect(r).toBeLessThan(7000);
      expect(r).toBeGreaterThan(6900);
    });

    it("a refused play returns to pick with a retryable note, never the answer step", async () => {
      const f = fakeEar([1000, 3000], { playImpl: () => Promise.reject(new Error("not allowed")) });
      f.ear.toggle();
      f.ear.activate(0);
      // frames that fire before play() settles see a paused element; that is
      // "not started", not "finished", so the test must not reach step 2.
      for (let n = 0; n < 2 && f.frames.length > 0; n++) f.frames.shift()();
      expect(f.ear.view().step).toBe("1");
      expect(f.ear.view().playing).toBe(true);
      await new Promise((r) => setTimeout(r, 0));
      expect(f.ear.pending()).toBe(false);
      expect(f.ear.view().step).toBe("1");
      expect(f.ear.view().playing).toBe(false);
      expect(f.ear.view().help).toBe("That word's snippet could not play (not allowed). Click a word to try again.");
      expect(f.cancelled.length).toBeGreaterThan(0); // the queued frame was cancelled
      expect(f.errors.join()).toContain("snippet playback failed");
      // whatever frames remain do nothing; a retry plays again
      while (f.frames.length > 0) f.frames.shift()();
      expect(f.ear.view().step).toBe("1");
      f.ear.activate(1);
      expect(f.audio.plays).toEqual([1, 3]);
      expect(f.st.offset).toBe(0);
    });

    it("plays from the unit's shifted start and stops before the next unit's shifted start", () => {
      const f = fakeEar([1000, 3000, 6000], { offset: 500 });
      f.ear.toggle();
      f.ear.activate(1);
      expect(f.audio.plays).toEqual([3.5]);
      const reached = f.run(17); // ~60 Hz frames
      expect(f.audio.paused).toBe(true);
      expect(reached).toBeLessThan(6500);
      expect(reached).toBeGreaterThan(6500 - 100);
      expect(f.ear.view().step).toBe("2");
    });

    it("plays the last unit to the track end, capped at 10 s", () => {
      const end = fakeEar([1000, 3000], { durationMs: 5000 });
      end.ear.toggle();
      end.ear.activate(1);
      const r1 = end.run(17);
      expect(r1).toBeLessThan(5000);
      expect(r1).toBeGreaterThan(4900);
      const cap = fakeEar([1000, 3000], { durationMs: 60000 });
      cap.ear.toggle();
      cap.ear.activate(1);
      const r = cap.run(17);
      expect(r).toBeLessThan(13000);
      expect(r).toBeGreaterThan(12900);
    });

    it("an answer adds played minus heard to the current offset, and a second test refines it", () => {
      const f = fakeEar([1000, 3000, 6000], { offset: 200 });
      f.ear.toggle();
      f.ear.activate(2); // played the unit at 6.2 s
      f.ear.activate(1); // heard the one at 3.2 s
      expect(f.st.offset).toBe(200 + 3000);
      expect(f.ear.view().help).toContain("Test another word");
      f.ear.activate(1); // now at 6.2 s
      f.ear.activate(0); // heard the one at 4.2 s
      expect(f.st.offset).toBe(3200 + 2000);
    });

    it("the same unit answered is a no-op that says so", () => {
      const f = fakeEar([1000, 3000], { offset: 300 });
      f.ear.toggle();
      f.ear.activate(1);
      f.ear.activate(1);
      expect(f.st.offset).toBe(300);
      expect(f.ear.view().help).toBe("That word was already in time, so the offset is unchanged.");
    });

    it("cancel drops a pending test without moving the offset; replay plays the same unit again", () => {
      const f = fakeEar([1000, 3000, 6000]);
      f.ear.toggle();
      f.ear.activate(2);
      f.ear.replay();
      expect(f.audio.plays).toEqual([6, 6]);
      f.ear.cancel();
      expect(f.audio.paused).toBe(true);
      expect(f.ear.pending()).toBe(false);
      expect(f.ear.on()).toBe(true);
      f.ear.activate(0); // a fresh test, not an answer
      expect(f.st.offset).toBe(0);
      expect(f.ear.view().played).toBe(0);
      f.ear.cancel();
      f.ear.cancel(); // nothing pending: Done leaves the mode
      expect(f.ear.on()).toBe(false);
    });

    it("refuses to start or act while disabled, but still claims the click", () => {
      const f = fakeEar([1000, 3000], { enabled: false });
      f.ear.toggle();
      expect(f.ear.on()).toBe(false);
      expect(f.ear.activate(0)).toBe(false);
      f.st.enabled = true;
      f.ear.toggle();
      f.st.enabled = false;
      expect(f.ear.activate(0)).toBe(true);
      expect(f.audio.plays).toEqual([]);
    });
  });

  describe("find by ear (editor wiring)", () => {
    it("routes a line click to the snippet, not a seek, and an answer moves the offset", () => {
      // saved at +200: the snippet must start at the SHIFTED stamp (6.2 s).
      const e = mountEditor({ starts: [1200, 3200, 6200], orig: [1000, 3000, 6000], savedOffsetMs: 200, edited: true });
      e.$("mx-ear-toggle").click();
      expect(e.$("mx-ear-toggle").getAttribute("aria-pressed")).toBe("true");
      expect(e.$("mx-ear-banner").hidden).toBe(false);
      const lines = e.doc.querySelectorAll(".mx-preview-line");
      lines[2].click();
      expect(e.time()).toBe(6.2);
      expect(lines[2].classList.contains("is-ear-playing")).toBe(true);
      expect(e.$("mx-ear-title").textContent).toBe("Playing that line's snippet");
      lines[0].click();
      expect(e.$("mx-edit-offset").value).toBe("+5.20");
      expect(e.$("mx-edit-status").textContent).toContain("Offset set by ear");
      expect(e.$("mx-edit-save").disabled).toBe(false);
    });

    it("measures the answer from unclamped starts when the offset pushes a line below 0", () => {
      // at -2 s the first line's shifted start is -1 s: shown and played from
      // 0, but the delta must use -1 s or it understates the move by 1 s.
      const e = mountEditor({ starts: [1000, 3000] });
      e.nudge("-1s");
      e.nudge("-1s");
      e.$("mx-ear-toggle").click();
      const lines = e.doc.querySelectorAll(".mx-preview-line");
      lines[0].click();
      expect(e.time()).toBe(0); // playback still clamps at 0
      lines[1].click(); // heard the line shifted to 1 s
      expect(e.$("mx-edit-offset").value).toBe("-4.00");
    });

    it("Esc cancels a pending test and keeps the offset; a second Esc discards", () => {
      const e = mountEditor({ starts: [1000, 3000], keyboard: true });
      e.nudge("+0.1");
      e.$("mx-ear-toggle").click();
      e.doc.querySelectorAll(".mx-preview-line")[1].click();
      expect(e.$("mx-preview-keys").textContent).toContain("cancel test");
      const esc = () => e.doc.body.dispatchEvent(new e.win.KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
      esc();
      expect(e.$("mx-edit-offset").value).toBe("+0.10");
      expect(e.$("mx-ear-title").textContent).toBe("Click a line to hear it");
      expect(e.$("mx-preview-keys").textContent).toContain("discard");
      esc();
      expect(e.$("mx-edit-offset").value).toBe("0.00");
    });

    it("E toggles the mode, and a locked editor turns it off and disables the toggle", async () => {
      const e = mountEditor({ starts: [1000, 3000], keyboard: true, fetchImpl: reply(409, '{"error":"changed"}') });
      e.doc.body.dispatchEvent(new e.win.KeyboardEvent("keydown", { key: "e", bubbles: true }));
      expect(e.$("mx-ear-banner").hidden).toBe(false);
      e.win.localStorage.setItem("mx-offset-confirm-skip", "1");
      e.nudge("+0.1");
      e.$("mx-edit-save").click();
      await vi.waitFor(() => expect(e.$("mx-edit-status").textContent).toContain("changed on disk"));
      expect(e.$("mx-ear-toggle").disabled).toBe(true);
      expect(e.$("mx-ear-banner").hidden).toBe(true);
      e.doc.querySelectorAll(".mx-preview-line")[1].click(); // back to a plain seek
      expect(e.time()).toBe(3.1);
    });
  });

  it("parses typed offsets", () => {
    const { parseOffset } = load().win.mxPreviewEdit;
    expect(parseOffset("-7.25")).toBe(-7250);
    expect(parseOffset("+0.6")).toBe(600);
    expect(parseOffset("1,5")).toBe(1500);
    expect(parseOffset("abc")).toBeNull();
    expect(parseOffset("900")).toBeNull();
  });

  it("counts lines past the end", () => {
    const { pastEnd } = load().win.mxPreviewEdit;
    expect(pastEnd([1000, 9000], 0, 10000, 2000)).toBe(0);
    expect(pastEnd([1000, 9000], 4000, 10000, 2000)).toBe(1);
    expect(pastEnd([1000, 9000], 4000, 0, 2000)).toBe(0);
    expect(pastEnd([1000, 9000], 4000, 10000, 5000)).toBe(0);
  });

  it("a nudge shifts the shown times, the highlight and the seek target", () => {
    const e = mountEditor({ starts: [1000, 3000] });
    e.nudge("+1s");
    expect(e.times()).toEqual(["0:02.00", "0:04.00"]);
    e.at(2.5); // past the shifted first line (2 s), before the second (4 s)
    expect(e.current(".mx-preview-line")).toEqual(["0:02.00l1000"]);
    e.doc.querySelectorAll(".mx-preview-line")[1].click();
    expect(e.time()).toBe(4);
  });

  it("clamps shifted starts at 0", () => {
    const e = mountEditor({ starts: [500] });
    e.nudge("-1s");
    expect(e.times()).toEqual(["0:00.00"]);
    expect(e.$("mx-edit-offset").value).toBe("-1.00");
  });

  it("disables Save while a line is past the end", () => {
    const e = mountEditor({ durationMs: 10000, starts: [1000, 9000] });
    for (let i = 0; i < 4; i++) {
      e.nudge("+1s");
    }
    expect(e.$("mx-edit-save").disabled).toBe(true);
    expect(e.doc.querySelectorAll(".mx-preview-line.is-past-end").length).toBe(1);
    expect(e.$("mx-edit-status").textContent).toContain("Save is off");
  });

  it("applies a typed offset on change and ignores invalid input", () => {
    const e = mountEditor({ starts: [1000] });
    const f = e.$("mx-edit-offset");
    f.value = "1,5";
    f.dispatchEvent(new e.win.Event("change"));
    expect(f.value).toBe("+1.50");
    f.value = "abc";
    f.dispatchEvent(new e.win.Event("change"));
    expect(f.value).toBe("+1.50");
    expect(e.times()).toEqual(["0:02.50"]);
  });

  it("Discard returns to the last saved offset", () => {
    const e = mountEditor({ starts: [1000], savedOffsetMs: 300 });
    e.nudge("+1s");
    e.$("mx-edit-discard").click();
    expect(e.$("mx-edit-offset").value).toBe("+0.30");
    expect(e.$("mx-edit-save").disabled).toBe(true);
  });

  it("measures the offset from the original, not from the already-shifted saved file", () => {
    // the file shows 1300 ms because the original 1000 ms was saved with +300
    const e = mountEditor({ starts: [1300], orig: [1000], savedOffsetMs: 300, edited: true });
    expect(e.times()).toEqual(["0:01.30"]);
    e.nudge("+0.1");
    expect(e.times()).toEqual(["0:01.40"]);
  });

  it("takes the base from data-orig-ms, so a line a negative save clamped to 0 keeps its true original", () => {
    // original 100 ms saved at -300: the file shows 0, and subtracting the
    // offset would wrongly give 300. The server says the original is 100.
    const e = mountEditor({ starts: [0, 4700], orig: [100, 5000], savedOffsetMs: -300, edited: true });
    expect(e.times()).toEqual(["0:00.00", "0:04.70"]);
    e.nudge("+1s"); // offset -300 + 1000 = +700 from the original
    expect(e.times()).toEqual(["0:00.80", "0:05.70"]);
  });

  it("turns the editor off when data-orig-ms does not match the lines", () => {
    const e = mountEditor({ starts: [1000, 2000], orig: [1000] });
    expect(e.$("mx-edit").hidden).toBe(true);
    expect(e.errors.join()).toContain("data-orig-ms");
    expect(e.times()).toEqual([]);
  });

  it("uses the server's tolerance for the past-end check", () => {
    // line at 9 s +4 s = 13 s on a 10 s track: 3 s over. Past the end with a
    // 2 s tolerance, inside it with 5 s.
    const tight = mountEditor({ durationMs: 10000, starts: [1000, 9000], toleranceMs: 2000 });
    const loose = mountEditor({ durationMs: 10000, starts: [1000, 9000], toleranceMs: 5000 });
    for (let i = 0; i < 4; i++) {
      tight.nudge("+1s");
      loose.nudge("+1s");
    }
    expect(tight.$("mx-edit-save").disabled).toBe(true);
    expect(loose.$("mx-edit-save").disabled).toBe(false);
    expect(loose.doc.querySelectorAll(".is-past-end").length).toBe(0);
  });

  it("a decorative line (the server's timing.IsDecorative) never blocks Save", () => {
    const e = mountEditor({ durationMs: 10000, starts: [1000, 9000], decorative: [1] });
    for (let i = 0; i < 4; i++) {
      e.nudge("+1s");
    }
    expect(e.$("mx-edit-save").disabled).toBe(false);
    expect(e.doc.querySelectorAll(".is-past-end").length).toBe(0);
  });

  it("an edited row shows Edited and Revert, which posts and returns to Original", async () => {
    const f = reply(200, '{"offset_ms":0,"mtime":1700000000000000009,"created_orig":false}');
    const e = mountEditor({ starts: [1000], savedOffsetMs: 300, edited: true, fetchImpl: f });
    expect(e.$("mx-edit-chip").textContent).toBe("Edited");
    expect(e.$("mx-edit-revert").hidden).toBe(false);
    e.$("mx-edit-revert").click();
    await vi.waitFor(() => expect(e.$("mx-edit-status").textContent).toContain("Reverted."));
    expect(f.mock.calls[0][0]).toBe("/preview/7/revert");
    expect(e.$("mx-edit-chip").textContent).toBe("Original");
    expect(e.$("mx-edit-offset").value).toBe("0.00");
    expect(e.$("mx-edit-revert").hidden).toBe(true);
  });

  it("falls back to asking when localStorage throws", () => {
    const e = mountEditor({ starts: [1000] });
    vi.spyOn(e.win.Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    e.nudge("+0.1");
    e.$("mx-edit-save").click();
    expect(e.$("mx-edit-confirm").open).toBe(true);
  });

  it("remembers Don't ask again, and a later save posts without the dialog", async () => {
    const f = reply(200, '{"offset_ms":100,"mtime":1700000000000000002,"created_orig":true}');
    const e = mountEditor({ starts: [1000], fetchImpl: f });
    e.nudge("+0.1");
    e.$("mx-edit-save").click();
    e.$("mx-edit-skip").checked = true;
    e.$("mx-edit-confirm-ok").click();
    await vi.waitFor(() => expect(e.$("mx-edit-status").textContent).toContain("Saved."));
    expect(e.win.localStorage.getItem("mx-offset-confirm-skip")).toBe("1");
    e.nudge("+0.1");
    e.$("mx-edit-save").click();
    expect(e.$("mx-edit-confirm").open).toBe(false);
    expect(f).toHaveBeenCalledTimes(2);
  });

  it("a second save uses the mtime the first one returned, kept as exact text", async () => {
    const f = reply(200, '{"offset_ms":100,"mtime":1700000000000000002,"created_orig":true}');
    const e = mountEditor({ starts: [1000], fetchImpl: f });
    e.win.localStorage.setItem("mx-offset-confirm-skip", "1");
    e.nudge("+0.1");
    e.$("mx-edit-save").click();
    await vi.waitFor(() => expect(e.$("mx-edit-status").textContent).toContain("Saved."));
    e.nudge("+0.1");
    e.$("mx-edit-save").click();
    expect(f.mock.calls[0][1].body.get("mtime")).toBe("1700000000000000001");
    expect(f.mock.calls[0][1].body.get("offset_ms")).toBe("100");
    expect(f.mock.calls[1][1].body.get("mtime")).toBe("1700000000000000002");
    expect(f.mock.calls[1][1].body.get("csrf_token")).toBe("tok");
  });

  it("maps refusals to their states", async () => {
    const e = mountEditor({ starts: [1000], fetchImpl: reply(409, '{"error":"busy"}') });
    e.win.localStorage.setItem("mx-offset-confirm-skip", "1");
    e.nudge("+0.1");
    e.$("mx-edit-save").click();
    await vi.waitFor(() => expect(e.$("mx-edit-status").textContent).toContain("being processed"));
    e.win.fetch = reply(409, '{"error":"changed"}');
    e.$("mx-edit-save").click();
    await vi.waitFor(() => expect(e.$("mx-edit-status").textContent).toContain("Reload"));
    expect(e.doc.querySelector(".mx-edit-nudge").disabled).toBe(true);
  });

  it("a changed refusal locks the editor until reload: Discard, nudges, Save and keys do nothing", async () => {
    const f = reply(409, '{"error":"changed"}');
    const e = mountEditor({ starts: [1000], fetchImpl: f });
    e.win.localStorage.setItem("mx-offset-confirm-skip", "1");
    e.nudge("+0.1");
    e.$("mx-edit-save").click();
    await vi.waitFor(() => expect(e.$("mx-edit-status").textContent).toContain("changed on disk"));
    expect(e.$("mx-edit-discard").disabled).toBe(true);
    expect(e.$("mx-edit-save").disabled).toBe(true);
    expect(e.doc.querySelector(".mx-edit-nudge").disabled).toBe(true);
    // A click on a disabled button still reaches its listener when dispatched
    // programmatically (and via the Escape shortcut), so the handlers must refuse too.
    e.$("mx-edit-discard").dispatchEvent(new e.win.Event("click"));
    e.doc.querySelector(".mx-edit-nudge").dispatchEvent(new e.win.Event("click"));
    e.$("mx-edit-save").dispatchEvent(new e.win.Event("click"));
    e.$("mx-edit-revert").dispatchEvent(new e.win.Event("click"));
    expect(e.$("mx-edit-status").textContent).toContain("changed on disk");
    expect(e.$("mx-edit-offset").value).toBe("+0.10");
    expect(e.$("mx-edit-discard").disabled).toBe(true);
    expect(f).toHaveBeenCalledTimes(1);
  });

  it("a second post while saving sends nothing (Save or Revert)", async () => {
    let resolve;
    const f = vi.fn(() => new Promise((r) => (resolve = r)));
    const e = mountEditor({ starts: [1000], savedOffsetMs: 300, orig: [700], edited: true, fetchImpl: f });
    e.win.localStorage.setItem("mx-offset-confirm-skip", "1");
    e.$("mx-edit-revert").click();
    expect(e.$("mx-edit-chip").textContent).toBe("Saving");
    // The Revert link is hidden while saving, but a queued click still fires.
    e.$("mx-edit-revert").dispatchEvent(new e.win.Event("click"));
    e.$("mx-edit-confirm-ok").click();
    expect(f).toHaveBeenCalledTimes(1);
    resolve({ ok: true, status: 200, text: () => Promise.resolve('{"offset_ms":0,"mtime":5}') });
    await vi.waitFor(() => expect(e.$("mx-edit-status").textContent).toContain("Reverted."));
  });
});
