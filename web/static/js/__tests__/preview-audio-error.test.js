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
  <audio id="mx-preview-audio" src="/preview/7/audio" data-format="M4A" data-type="audio/mp4"></audio>
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
function load({ preError = null, page = PAGE, fetch = null, noAbort = false, fakeTimers = false } = {}) {
  const dom = new JSDOM(page, { runScripts: "outside-only", pretendToBeVisual: true, url: "http://localhost/" });
  const win = dom.window;
  openWindows.push(win);
  Object.defineProperty(win.document, "readyState", { get: () => "complete" });
  const errors = [];
  win.console.error = (...a) => errors.push(a.join(" "));
  win.matchMedia = () => ({ matches: false });
  win.ResizeObserver = class {
    observe() {}
  };
  if (noAbort) {
    delete win.AbortController;
  }
  // fakeTimers captures setTimeout so a test can fire the probe timeout at will.
  const timers = [];
  if (fakeTimers) {
    win.setTimeout = (fn, ms) => timers.push({ fn, ms, live: true });
    win.clearTimeout = (id) => {
      if (timers[id - 1]) timers[id - 1].live = false;
    };
  }
  const fetches = [];
  if (fetch) {
    win.fetch = (url, opts) => {
      fetches.push({ url, opts });
      return fetch(url, opts);
    };
  }
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
  return { win, $, errors, fail, fetches, timers };
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
    expect(p.errors.filter((e) => e.includes("audio failed")).length).toBe(1);
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

  it("marks the status as a playback failure and leads with it, for the collapsed phone bar", () => {
    const p = load();
    expect(p.$("mx-edit-status").classList.contains("is-playback-failed")).toBe(false);
    p.fail(4);
    expect(p.$("mx-edit-status").classList.contains("is-playback-failed")).toBe(true);
    expect(p.$("mx-edit-status").textContent.startsWith("The audio cannot be played in this browser, so Find by ear is off.")).toBe(true);
    p.$("mx-edit-offset").value = "40";
    p.$("mx-edit-offset").dispatchEvent(new p.win.Event("change", { bubbles: true }));
    const cls = p.$("mx-edit-status").className;
    expect(cls).toContain("is-warn");
    expect(cls).toContain("is-playback-failed");
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
      expect(p.errors.filter((e) => !e.includes("fetch is unavailable")).length).toBe(1);
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

  it("retries on a decode error (code 3) as on an unsupported source (code 4)", () => {
    const p = loadFlac();
    p.fail(3);
    expect(p.loads()).toBe(1);
    expect(p.audio.getAttribute("src")).toBe(FLAC);
  });

  it("does not retry an error event with no media error", () => {
    const p = loadFlac();
    p.audio.dispatchEvent(new p.win.Event("error"));
    expect(p.loads()).toBe(0);
    expect(p.audio.getAttribute("src")).toBe("/preview/7/audio");
    expect(p.$("mx-preview-audio-error").hidden).toBe(false);
  });

  it("does not retry an unknown media error code", () => {
    const p = loadFlac();
    p.fail(5);
    expect(p.loads()).toBe(0);
    expect(p.audio.getAttribute("src")).toBe("/preview/7/audio");
    expect(p.$("mx-preview-audio-error").textContent).not.toContain("Converting");
  });

  it("does not retry when the page names no fallback", () => {
    const p = load();
    const audio = p.$("mx-preview-audio");
    let loads = 0;
    audio.load = () => loads++;
    p.fail(4);
    expect(loads).toBe(0);
    expect(audio.getAttribute("src")).toBe("/preview/7/audio");
    expect(p.$("mx-preview-audio-error").textContent).not.toContain("FLAC");
  });
});

describe("preview.js audio failure check (#1261)", () => {
  const FLAC = "/preview/7/audio.flac";
  const withFlac = PAGE.replace('data-type="audio/mp4"', 'data-type="audio/mp4" data-flac-src="' + FLAC + '"');
  const flush = () => new Promise((r) => setTimeout(r, 0));
  const answer = (status) => () => Promise.resolve({ status });

  function loadWith(fetch, page = withFlac, opts = {}) {
    const p = load({ page, fetch, ...opts });
    const audio = p.$("mx-preview-audio");
    let loads = 0;
    audio.load = () => {
      loads++;
    };
    return { ...p, audio, loads: () => loads, text: () => p.$("mx-preview-audio-error").textContent };
  }

  it("says the file is missing on a 404 and does not retry as FLAC", async () => {
    const p = loadWith(answer(404));
    p.fail(4);
    await flush();
    expect(p.text()).toContain("could not be found on the server");
    expect(p.text()).not.toContain("cannot play");
    expect(p.loads()).toBe(0);
    expect(p.audio.getAttribute("src")).toBe("/preview/7/audio");
    expect(p.fetches.length).toBe(1);
    expect(p.fetches[0].opts.headers.Range).toBe("bytes=0-0");
    expect(p.$("mx-ear-toggle").disabled).toBe(true);
    expect(p.win.document.querySelector(".mx-ear-row .mx-edit-hint").textContent).toContain("could not be found");
  });

  it("checks a network error (code 2) too", async () => {
    const p = loadWith(answer(404));
    p.fail(2);
    await flush();
    expect(p.text()).toContain("could not be found on the server");
  });

  it("words a 5xx as a server error", async () => {
    const p = loadWith(answer(503));
    p.fail(4);
    await flush();
    expect(p.text()).toContain("server could not deliver");
    expect(p.text()).not.toContain("cannot play");
    expect(p.loads()).toBe(0);
  });

  it("keeps the decode message and the FLAC retry on a 206", async () => {
    const p = loadWith(answer(206));
    p.fail(4);
    await flush();
    expect(p.loads()).toBe(1);
    expect(p.audio.getAttribute("src")).toBe(FLAC);
    p.fail(4);
    await flush();
    expect(p.text()).toContain("cannot play");
    expect(p.text()).toContain("FLAC conversion did not play");
    expect(p.fetches.length).toBe(1);
  });

  it("keeps the decode message on a 200 when no fallback is named", async () => {
    const p = loadWith(answer(200), PAGE);
    p.fail(4);
    await flush();
    expect(p.text()).toContain("cannot play");
    expect(p.text()).toContain("M4A");
  });

  it("falls back to the decode message when the check itself fails", async () => {
    const p = loadWith(() => Promise.reject(new Error("offline")), PAGE);
    p.fail(4);
    await flush();
    expect(p.text()).toContain("cannot play");
    expect(p.errors.join("\n")).toContain("audio check request failed");
  });

  it("drops a late answer when the source changed meanwhile", async () => {
    let resolve;
    const p = loadWith(() => new Promise((r) => (resolve = r)));
    p.fail(4);
    p.audio.setAttribute("src", "/preview/8/audio");
    resolve({ status: 404 });
    await flush();
    expect(p.$("mx-preview-audio-error").hidden).toBe(true);
    expect(p.loads()).toBe(0);
  });

  it("checks once when a second error event lands mid-check", async () => {
    const p = loadWith(answer(404));
    p.fail(4);
    p.fail(4);
    await flush();
    expect(p.fetches.length).toBe(1);
    expect(p.text()).toContain("could not be found");
  });

  const reply = (status, extra = {}) => () => Promise.resolve({ status, redirected: false, ...extra });

  it("sends X-Requested-With so the session guard answers 401, not a redirect", async () => {
    const p = loadWith(answer(206));
    p.fail(4);
    await flush();
    expect(p.fetches[0].opts.headers["X-Requested-With"]).toBe("XMLHttpRequest");
  });

  it("reports an expired session on a 401, 403 or followed redirect, with no FLAC retry", async () => {
    for (const r of [reply(401), reply(403), reply(200, { redirected: true })]) {
      const p = loadWith(r);
      p.fail(4);
      await flush();
      expect(p.text()).toContain("session has expired");
      expect(p.text()).not.toContain("cannot play");
      expect(p.loads()).toBe(0);
      expect(p.audio.getAttribute("src")).toBe("/preview/7/audio");
      expect(p.win.document.querySelector(".mx-ear-row .mx-edit-hint").textContent).toContain("session has expired");
    }
  });

  it("shows a generic message with the status for another refusal, with no FLAC retry", async () => {
    for (const code of [416, 429, 400]) {
      const p = loadWith(reply(code));
      p.fail(4);
      await flush();
      expect(p.text()).toContain("error " + code);
      expect(p.text()).not.toContain("cannot play");
      expect(p.loads()).toBe(0);
    }
  });

  it("treats any 2xx answer as served: decode message and the FLAC retry", async () => {
    for (const code of [200, 204, 299]) {
      const p = loadWith(reply(code));
      p.fail(4);
      await flush();
      expect(p.loads()).toBe(1);
      expect(p.audio.getAttribute("src")).toBe(FLAC);
      expect(p.text()).not.toContain("error " + code);
    }
  });

  it("does not retry as FLAC when the check itself fails", async () => {
    const p = loadWith(() => Promise.reject(new Error("offline")));
    p.fail(4);
    await flush();
    expect(p.loads()).toBe(0);
    expect(p.audio.getAttribute("src")).toBe("/preview/7/audio");
    expect(p.text()).toContain("cannot play");
    expect(p.text()).not.toContain("Converting");
  });

  it("never retries a network error (code 2) as FLAC after a 2xx answer", async () => {
    const p = loadWith(answer(206));
    p.fail(2);
    await flush();
    expect(p.loads()).toBe(0);
    expect(p.audio.getAttribute("src")).toBe("/preview/7/audio");
    expect(p.text()).toContain("network error");
  });

  it("sets the find by ear hint to the server wording on a 5xx", async () => {
    const p = loadWith(answer(502));
    p.fail(4);
    await flush();
    expect(p.win.document.querySelector(".mx-ear-row .mx-edit-hint").textContent).toContain("server could not deliver");
  });

  it("logs and does not probe when fetch is missing", () => {
    const p = loadWith(null, PAGE);
    p.fail(4);
    expect(p.errors.join("\n")).toContain("fetch is unavailable");
    expect(p.fetches.length).toBe(0);
    expect(p.text()).toContain("cannot play");
  });

  it("logs and does not probe when the audio element has no src", () => {
    const p = loadWith(answer(404), PAGE.replace(' src="/preview/7/audio"', ""));
    p.fail(4);
    expect(p.errors.join("\n")).toContain("no src");
    expect(p.fetches.length).toBe(0);
    expect(p.text()).toContain("cannot play");
  });

  it("shows a message once the probe times out, and aborts the request", async () => {
    let signal;
    const never = (url, opts) => {
      signal = opts.signal;
      return new Promise((_, rj) => opts.signal.addEventListener("abort", () => rj(new Error("aborted"))));
    };
    const p = loadWith(never, PAGE, { fakeTimers: true });
    p.fail(4);
    await flush();
    expect(p.$("mx-preview-audio-error").hidden).toBe(true);
    const t = p.timers.find((x) => x.ms >= 1000);
    expect(t.ms).toBe(8000);
    t.fn();
    await flush();
    expect(signal.aborted).toBe(true);
    expect(p.text()).toContain("cannot play");
    expect(p.errors.join("\n")).toContain("timed out");
  });

  it("times out without AbortController and ignores a late answer", async () => {
    let resolve;
    const p = loadWith(() => new Promise((r) => (resolve = r)), PAGE, { fakeTimers: true, noAbort: true });
    p.fail(4);
    expect(p.errors.join("\n")).toContain("AbortController is unavailable");
    p.timers.find((x) => x.ms >= 1000).fn();
    await flush();
    expect(p.text()).toContain("cannot play");
    const shown = p.errors.filter((e) => e.includes("audio failed")).length;
    resolve({ status: 404, redirected: false });
    await flush();
    expect(p.text()).toContain("cannot play");
    expect(p.errors.filter((e) => e.includes("audio failed")).length).toBe(shown);
  });

  it("clears the timer when the probe settles", async () => {
    const p = loadWith(answer(404), PAGE, { fakeTimers: true });
    p.fail(4);
    await flush();
    expect(p.timers.find((x) => x.ms >= 1000).live).toBe(false);
  });

  it("logs one console error for a snippet rejection then a media error, however slow the probe", async () => {
    for (const delay of [0, 300]) {
      const slow = () => new Promise((r) => setTimeout(() => r({ status: 404, redirected: false }), delay));
      const p = loadWith(slow, PAGE);
      let reject;
      p.audio.play = () => new p.win.Promise((_, rj) => { reject = rj; });
      p.$("mx-ear-toggle").click();
      p.win.document.querySelector(".mx-preview-line").click();
      reject(new Error("NotSupportedError"));
      await flush();
      p.fail(4);
      await new Promise((r) => setTimeout(r, delay + 20));
      expect(p.text()).toContain("could not be found");
      expect(p.errors.length).toBe(1);
    }
  });
});
