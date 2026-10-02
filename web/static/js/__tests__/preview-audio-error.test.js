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
function load({ preError = null } = {}) {
  const dom = new JSDOM(PAGE, { runScripts: "outside-only", pretendToBeVisual: true, url: "http://localhost/" });
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

  it("reports an error that fired before the script ran", () => {
    const p = load({ preError: { code: 4 } });
    expect(p.$("mx-preview-audio-error").hidden).toBe(false);
    expect(p.$("mx-ear-toggle").disabled).toBe(true);
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

  it("leaves a healthy page alone", () => {
    const p = load();
    expect(p.errors).toEqual([]);
    expect(p.$("mx-preview-audio-error").hidden).toBe(true);
    expect(p.$("mx-ear-toggle").disabled).toBe(false);
  });
});
