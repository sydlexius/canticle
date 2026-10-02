// preview.js (#1243): a stream the browser cannot decode must say so in the
// player, log to the console, and leave typed offsets usable while turning
// Find by ear off with a visible reason.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { afterEach, describe, expect, it } from "vitest";

const JS = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "preview.js"), "utf8");

const KB = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "keyboard.js"), "utf8");

const openWindows = [];
afterEach(() => {
  while (openWindows.length > 0) {
    openWindows.pop().close();
  }
});

const PAGE = `<body>
  <audio id="mx-preview-audio" data-format="M4A" data-type="audio/mp4"></audio>
  <p id="mx-preview-audio-error" role="alert" hidden></p>
  <section id="mx-edit" data-save-url="/s" data-revert-url="/r" data-offset-ms="0"
    data-mtime="1" data-duration-ms="30000" data-orig-ms="1000" data-tolerance-ms="2000">
    <input type="hidden" name="csrf_token" value="tok">
    <span id="mx-edit-chip"></span><input id="mx-edit-offset"><input id="mx-edit-slider" type="range">
    <button class="mx-edit-nudge" data-delta="100">+0.1</button>
    <button id="mx-edit-save"></button><button id="mx-edit-discard"></button><button id="mx-edit-revert" hidden></button>
    <div class="mx-ear-row"><button id="mx-ear-toggle" data-ear-unit="line" aria-pressed="false"></button><span class="mx-edit-hint">Hear one line.</span></div>
    <p id="mx-edit-status"></p>
    <dialog id="mx-edit-confirm"><span id="mx-edit-confirm-offset"></span><p id="mx-edit-confirm-body"></p>
      <input id="mx-edit-skip" type="checkbox"><button id="mx-edit-confirm-cancel"></button><button id="mx-edit-confirm-ok"></button></dialog>
  </section>
  <div id="mx-ear-banner" hidden><span id="mx-ear-step"></span><span id="mx-ear-title"></span><span id="mx-ear-help"></span>
    <button id="mx-ear-replay" hidden></button><button id="mx-ear-cancel"></button></div>
  <p id="mx-preview-keys"></p>
  <ol id="mx-preview-lyrics"><li class="mx-preview-line" data-start-ms="1000">one</li></ol></body>`;

// load runs preview.js; preError, when set, is the audio.error already present
// when the script starts (the error fired before the deferred script ran).
function load({ preError = null, page = PAGE } = {}) {
  const dom = new JSDOM(page, { runScripts: "outside-only", pretendToBeVisual: true, url: "http://localhost/" });
  const win = dom.window;
  openWindows.push(win);
  Object.defineProperty(win.document, "readyState", { get: () => "complete" });
  const errors = [];
  win.console.error = (...a) => errors.push(a.join(" "));
  win.matchMedia = () => ({ matches: false });
  win.HTMLElement.prototype.scrollIntoView = () => {};
  win.HTMLDialogElement.prototype.showModal = function () {
    this.setAttribute("open", "");
  };
  win.HTMLDialogElement.prototype.close = function () {
    this.removeAttribute("open");
  };
  const audio = win.document.getElementById("mx-preview-audio");
  let err = preError;
  Object.defineProperty(audio, "error", { get: () => err });
  win.eval(KB);
  win.eval(JS);
  const fail = (code) => {
    err = { code };
    audio.dispatchEvent(new win.Event("error"));
  };
  const $ = (id) => win.document.getElementById(id);
  return { win, $, errors, fail };
}

describe("preview.js audio failure", () => {
  it("shows a specific message naming the format and logs to the console", () => {
    const p = load();
    expect(p.$("mx-preview-audio-error").hidden).toBe(true);
    p.fail(4);
    const box = p.$("mx-preview-audio-error");
    expect(box.hidden).toBe(false);
    expect(box.textContent).toContain("cannot play");
    expect(box.textContent).toContain("M4A");
    expect(box.textContent).toContain("audio/mp4");
    expect(p.errors.join("\n")).toContain("audio failed");
  });

  it("words a network error differently from an unplayable format", () => {
    const p = load();
    p.fail(2);
    expect(p.$("mx-preview-audio-error").textContent).toContain("network error");
    expect(p.$("mx-preview-audio-error").textContent).not.toContain("cannot play");
  });

  it("words an aborted load neutrally, not as an unplayable format", () => {
    const p = load();
    p.fail(1);
    const text = p.$("mx-preview-audio-error").textContent;
    expect(text).toContain("interrupted");
    expect(text).not.toContain("cannot play");
  });

  it("reports an error that fired before the script ran", () => {
    const p = load({ preError: { code: 4 } });
    expect(p.$("mx-preview-audio-error").hidden).toBe(false);
    expect(p.$("mx-ear-toggle").disabled).toBe(true);
    // a later error event must not log or report a second time
    p.win.document.getElementById("mx-preview-audio").dispatchEvent(new p.win.Event("error"));
    expect(p.errors.length).toBe(1);
  });

  it("turns find by ear off when playback fails while it is on", () => {
    const p = load();
    p.$("mx-ear-toggle").click();
    expect(p.$("mx-ear-banner").hidden).toBe(false);
    p.fail(2);
    expect(p.$("mx-ear-banner").hidden).toBe(true);
    expect(p.$("mx-ear-toggle").getAttribute("aria-pressed")).toBe("false");
    // a lyric click seeks again instead of being swallowed by the dead mode
    const audio = p.$("mx-preview-audio");
    audio.currentTime = 0;
    p.win.document.querySelector(".mx-preview-line").click();
    expect(audio.currentTime).toBeGreaterThan(0);
    expect(p.$("mx-ear-banner").hidden).toBe(true);
  });

  it("keeps the e shortcut from starting find by ear after a failure", () => {
    const p = load();
    p.fail(4);
    p.win.document.dispatchEvent(new p.win.KeyboardEvent("keydown", { key: "e", bubbles: true }));
    expect(p.$("mx-ear-banner").hidden).toBe(true);
    expect(p.$("mx-ear-toggle").getAttribute("aria-pressed")).toBe("false");
  });

  it("keeps the failure note beside a toned status message", () => {
    const p = load();
    p.fail(4);
    p.$("mx-edit-offset").value = "40";
    p.$("mx-edit-offset").dispatchEvent(new p.win.Event("change", { bubbles: true }));
    expect(p.$("mx-edit-status").className).toContain("is-warn");
    expect(p.$("mx-edit-status").textContent).toContain("cannot be played");
  });

  it("disables find by ear with a visible reason and keeps typed offsets usable", () => {
    const p = load();
    expect(p.$("mx-ear-toggle").disabled).toBe(false);
    p.fail(4);
    const ear = p.$("mx-ear-toggle");
    expect(ear.disabled).toBe(true);
    const hint = p.win.document.querySelector(".mx-ear-row .mx-edit-hint").textContent;
    expect(hint).toContain("cannot play the audio");
    expect(p.$("mx-edit-status").textContent).toContain("cannot be played");
    expect(p.$("mx-edit-offset").disabled).toBe(false);
    expect(p.$("mx-edit-slider").disabled).toBe(false);
    expect(p.win.document.querySelector(".mx-edit-nudge").disabled).toBe(false);
    ear.click();
    expect(p.$("mx-ear-banner").hidden).toBe(true);
  });

  it("words the hint and status line to match the failure kind", () => {
    const want = {
      1: ["interrupted", "interrupted"],
      2: ["network error", "network error"],
      4: ["cannot play the audio", "cannot be played"],
    };
    for (const code of [1, 2, 4]) {
      const p = load();
      p.fail(code);
      const hint = p.win.document.querySelector(".mx-ear-row .mx-edit-hint").textContent;
      const status = p.$("mx-edit-status").textContent;
      expect(hint).toContain(want[code][0]);
      expect(status).toContain(want[code][1]);
      if (code !== 4) {
        expect(hint).not.toContain("cannot play");
        expect(status).not.toContain("cannot be played");
      }
      expect(p.$("mx-ear-toggle").disabled).toBe(true);
    }
  });

  it("shows the error on a page whose sidecar has no lyric lines", () => {
    const page = PAGE.replace(/<li class="mx-preview-line"[^>]*>one<\/li>/, "");
    const p = load({ page });
    expect(p.win.document.querySelectorAll(".mx-preview-line").length).toBe(0);
    p.fail(4);
    expect(p.$("mx-preview-audio-error").hidden).toBe(false);
    expect(p.errors.filter((e) => e.includes("audio failed")).length).toBe(1);
  });

  it("logs one console error when a media error lands during a pending snippet", async () => {
    for (const rejectFirst of [false, true]) {
      const p = load();
      const audio = p.$("mx-preview-audio");
      let reject;
      audio.play = () => new p.win.Promise((_, rj) => { reject = rj; });
      p.$("mx-ear-toggle").click();
      p.win.document.querySelector(".mx-preview-line").click();
      const settle = async () => {
        reject(new Error("NotSupportedError"));
        await new Promise((r) => setTimeout(r, 0));
      };
      if (rejectFirst) {
        p.fail(4);
        await settle();
      } else {
        await settle();
        p.fail(4);
      }
      await new Promise((r) => setTimeout(r, 0));
      expect(p.errors.length).toBe(1);
    }
  });

  it("leaves a healthy page alone", () => {
    const p = load();
    expect(p.errors).toEqual([]);
    expect(p.$("mx-preview-audio-error").hidden).toBe(true);
    expect(p.$("mx-ear-toggle").disabled).toBe(false);
  });
});

describe("preview.js FLAC fallback (#1243)", () => {
  const FLAC = "/preview/7/audio.flac";
  const withFlac = PAGE.replace('data-type="audio/mp4"', 'data-type="audio/mp4" data-flac-src="' + FLAC + '"');

  function loadFlac(opts = {}) {
    const p = load({ page: withFlac, ...opts });
    const audio = p.$("mx-preview-audio");
    let loads = 0;
    audio.load = () => {
      loads++;
    };
    return { ...p, audio, loads: () => loads };
  }

  it("retries once against the fallback URL on a decode error, without showing the error", () => {
    const p = loadFlac();
    p.fail(4);
    expect(p.audio.getAttribute("src")).toBe(FLAC);
    expect(p.loads()).toBe(1);
    expect(p.$("mx-ear-toggle").disabled).toBe(false);
    expect(p.$("mx-preview-audio-error").textContent).toContain("Converting");
    p.audio.dispatchEvent(new p.win.Event("loadedmetadata"));
    expect(p.$("mx-preview-audio-error").hidden).toBe(true);
  });

  it("shows the ordinary error, naming the conversion, when the retry fails too", () => {
    const p = loadFlac();
    p.fail(4);
    p.fail(4);
    expect(p.loads()).toBe(1);
    const box = p.$("mx-preview-audio-error");
    expect(box.hidden).toBe(false);
    expect(box.textContent).toContain("cannot play");
    expect(box.textContent).toContain("FLAC conversion did not play");
    expect(p.$("mx-ear-toggle").disabled).toBe(true);
  });

  it("does not retry a network error", () => {
    const p = loadFlac();
    p.fail(2);
    expect(p.loads()).toBe(0);
    expect(p.$("mx-preview-audio-error").textContent).toContain("network error");
  });

  it("does not retry when the page names no fallback", () => {
    const p = load();
    const audio = p.$("mx-preview-audio");
    let loads = 0;
    audio.load = () => loads++;
    p.fail(4);
    expect(loads).toBe(0);
    expect(audio.hasAttribute("src")).toBe(false);
    expect(p.$("mx-preview-audio-error").textContent).not.toContain("FLAC");
  });
});
