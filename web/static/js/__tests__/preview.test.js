// preview.js (#481): current line/word highlighting, auto-scroll, click/keyboard seek.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { afterEach, describe, expect, it, vi } from "vitest";

const JS = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "preview.js"), "utf8");

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
    <p id="mx-edit-status"></p>
    <dialog id="mx-edit-confirm"><span id="mx-edit-confirm-offset"></span><p id="mx-edit-confirm-body"></p>
      <input id="mx-edit-skip" type="checkbox"><button id="mx-edit-confirm-cancel"></button><button id="mx-edit-confirm-ok"></button></dialog>
  </section>
  <ol id="mx-preview-lyrics">LINES</ol></body>`;

  // mountEditor builds the editor markup subset around the given line starts.
  // orig defaults to the shown starts (an unedited file); toleranceMs to timing.Tolerance.
  function mountEditor({ durationMs = 30000, starts = [1000], orig = null, toleranceMs = 2000, savedOffsetMs = 0, edited = false, fetchImpl = null, decorative = [] } = {}) {
    const lines = starts
      .map((ms, i) => `<li class="mx-preview-line${decorative.includes(i) ? " mx-preview-line-decorative" : ""}" data-start-ms="${ms}">l${ms}</li>`)
      .join("");
    const html = EDITOR.replace("OFFSET", String(savedOffsetMs))
      .replace("EDITED", edited ? "data-edited" : "")
      .replace("DURATION", String(durationMs))
      .replace("ORIG", (orig || starts).join(","))
      .replace("TOLERANCE", String(toleranceMs))
      .replace("LINES", lines);
    const p = load({
      html,
      setup: (win) => {
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
      },
    });
    const $ = (id) => p.doc.getElementById(id);
    const nudge = (label) =>
      Array.from(p.doc.querySelectorAll(".mx-edit-nudge")).find((b) => b.textContent === label).click();
    const times = () => Array.from(p.doc.querySelectorAll(".mx-preview-time")).map((e) => e.textContent);
    return { ...p, $, nudge, times };
  }
  const reply = (status, text) =>
    vi.fn(() => Promise.resolve({ ok: status === 200, status, text: () => Promise.resolve(text) }));

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
