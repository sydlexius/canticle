// preview.js (#481): current line/word highlighting, auto-scroll, click/keyboard seek.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { describe, expect, it } from "vitest";

const JS = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "preview.js"), "utf8");

const PAGE = `<body>
  <audio id="mx-preview-audio"></audio>
  <ol id="mx-preview-lyrics">
    <li class="mx-preview-line" data-start-ms="1000"><span class="mx-preview-word" data-start-ms="1000">a</span> <span class="mx-preview-word" data-start-ms="1500">b</span></li>
    <li class="mx-preview-line" data-start-ms="3000">second</li>
    <li class="mx-preview-line mx-preview-line-decorative" data-start-ms="5000">***</li>
  </ol></body>`;

function load({ html = PAGE, reduce = false } = {}) {
  const dom = new JSDOM(html, { runScripts: "outside-only", pretendToBeVisual: true });
  const win = dom.window;
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
