// preview.js Auto alignment (#1008 S8): start, poll, cancel, each failure
// code, the dropped-run 404, stale-mtime discard, discard and Esc.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { afterEach, describe, expect, it } from "vitest";

const DIR = join(dirname(fileURLToPath(import.meta.url)), "..");
const JS = readFileSync(join(DIR, "preview.js"), "utf8");
const KB = readFileSync(join(DIR, "keyboard.js"), "utf8");

const MTIME = "1700000000000000001";
const PAGE = `<body>
  <audio id="mx-preview-audio"></audio>
  <section id="mx-edit" data-save-url="/preview/7/offset" data-revert-url="/preview/7/revert" data-offset-ms="0"
    data-mtime="${MTIME}" data-duration-ms="30000" data-orig-ms="1000,3000" data-tolerance-ms="2000" AUTOURL>
    <input type="hidden" name="csrf_token" value="tok">
    <span id="mx-edit-chip"></span><input id="mx-edit-offset"><input id="mx-edit-slider" type="range">
    <button class="mx-edit-nudge" data-delta="100">+0.1</button>
    <button id="mx-auto-run">Auto</button><button id="mx-auto-stop" hidden>Cancel</button><span id="mx-auto-progress" hidden></span>
    <button id="mx-edit-save"></button><button id="mx-edit-discard"></button><button id="mx-edit-revert" hidden></button>
    <button id="mx-ear-toggle" data-ear-unit="line" aria-pressed="false"></button><p id="mx-edit-status"></p>
    <dialog id="mx-edit-confirm"><span id="mx-edit-confirm-offset"></span><p id="mx-edit-confirm-body"></p>
      <input id="mx-edit-skip" type="checkbox"><button id="mx-edit-confirm-cancel"></button><button id="mx-edit-confirm-ok"></button></dialog>
  </section>
  <div id="mx-ear-banner" hidden><span id="mx-ear-step"></span><span id="mx-ear-title"></span><span id="mx-ear-help"></span>
    <button id="mx-ear-replay" hidden></button><button id="mx-ear-cancel"></button></div>
  <p id="mx-preview-keys"></p>
  <ol id="mx-preview-lyrics"><li class="mx-preview-line" data-start-ms="1000">one</li><li class="mx-preview-line" data-start-ms="3000">two</li></ol></body>`;

const open = [];
afterEach(() => {
  while (open.length) {
    open.pop().close();
  }
});

// mount loads the real preview.js and keyboard.js. fetch answers from
// `answers` (oldest first, each [status, body]); running out is a failure.
// Timers are held, and poll() fires the 2 s ones, so a test steps the loop.
function mount({ autoURL = "/preview/7/auto", strip = false } = {}) {
  let html = PAGE.replace("AUTOURL", autoURL ? `data-auto-url="${autoURL}"` : "");
  if (strip) {
    html = html.replace('<span id="mx-auto-progress" hidden></span>', "");
  }
  const dom = new JSDOM(html, { runScripts: "outside-only", pretendToBeVisual: true, url: "http://localhost/" });
  const win = dom.window;
  open.push(win);
  Object.defineProperty(win.document, "readyState", { get: () => "complete" });
  const errors = [];
  win.console.error = (...a) => errors.push(a.join(" "));
  win.matchMedia = () => ({ matches: false });
  win.HTMLElement.prototype.scrollIntoView = () => {};
  win.ResizeObserver = class {
    observe() {}
  };
  const timers = new Map();
  let next = 0;
  win.setTimeout = (fn, ms) => {
    timers.set(++next, { fn, ms });
    return next;
  };
  win.clearTimeout = (id) => timers.delete(id);
  const answers = [];
  const calls = [];
  win.fetch = (url, init) => {
    calls.push({ url, method: init.method, body: init.body ? Object.fromEntries(init.body) : null });
    if (!answers.length) {
      return Promise.reject(new Error("unexpected request to " + url));
    }
    const [status, body] = answers.shift();
    return Promise.resolve({ status, ok: status < 300, text: () => Promise.resolve(body) });
  };
  win.eval(KB);
  win.eval(JS);
  const $ = (id) => win.document.getElementById(id);
  const settle = () => new Promise((r) => setTimeout(r, 0));
  const poll = async () => {
    for (const [id, t] of [...timers]) {
      if (t.ms === 2000) {
        timers.delete(id);
        t.fn();
      }
    }
    await settle();
  };
  const run = async (...replies) => {
    answers.push(...replies);
    $("mx-auto-run").click();
    await settle();
  };
  const times = () => Array.from(win.document.querySelectorAll(".mx-preview-time")).map((e) => e.textContent);
  const status = () => $("mx-edit-status").textContent;
  const esc = () => win.document.body.dispatchEvent(new win.KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
  const pending = () => [...timers.values()].filter((t) => t.ms === 2000).length;
  return { win, $, answers, calls, errors, poll, run, times, status, esc, pending };
}

const START = [202, '{"state":"running"}'];
const RUNNING = [200, '{"state":"running"}'];
const done = (mtime = MTIME, lines = "[1500,3200]") => [200, `{"state":"done","mtime":${mtime},"aligned_words":2,"lines":${lines},"words":[null,null],"quality":{},"warnings":[]}`];

describe("Auto alignment", () => {
  it("is off without data-auto-url, and loud when the controls are missing", () => {
    const off = mount({ autoURL: "" });
    off.$("mx-auto-run").click();
    expect(off.calls).toEqual([]);
    expect(off.errors).toEqual([]);
    const bare = mount({ strip: true });
    expect(bare.errors.join()).toContain("Auto controls are missing");
  });

  it("runs: starts with the page mtime and token, polls every 2 s, then previews the suggestion", async () => {
    const p = mount();
    expect(p.times()).toEqual(["0:01.00", "0:03.00"]);
    await p.run(START);
    expect(p.calls[0]).toEqual({ url: "/preview/7/auto", method: "POST", body: { mtime: MTIME, csrf_token: "tok" } });
    expect(p.$("mx-edit-chip").textContent).toBe("Aligning");
    expect(p.$("mx-auto-run").hidden).toBe(true);
    expect(p.$("mx-auto-stop").textContent).toBe("Cancel");
    expect(p.$("mx-auto-progress").hidden).toBe(false);
    expect(p.status()).toContain("Aligning the lyrics");
    expect(p.$("mx-edit-save").disabled).toBe(true); // the offset controls wait
    p.answers.push(RUNNING);
    await p.poll();
    expect(p.calls[1]).toMatchObject({ url: "/preview/7/auto", method: "GET" });
    expect(p.pending()).toBe(1);
    p.answers.push(done());
    await p.poll();
    expect(p.times()).toEqual(["0:01.50", "0:03.20"]);
    expect(p.$("mx-edit-chip").textContent).toBe("Suggested");
    expect(p.$("mx-auto-stop").textContent).toBe("Discard suggestion");
    expect(p.$("mx-auto-progress").hidden).toBe(true);
    expect(p.status()).toContain("Nothing has been written");
    expect(p.pending()).toBe(0);
    // a line click seeks to the suggested start
    p.win.document.querySelectorAll(".mx-preview-line")[1].click();
    expect(p.$("mx-preview-audio").currentTime).toBe(3.2);
  });

  it("discard restores the starts shown before Auto, unsaved offset included", async () => {
    const p = mount();
    p.win.document.querySelector(".mx-edit-nudge").click();
    expect(p.times()).toEqual(["0:01.10", "0:03.10"]);
    await p.run(START);
    p.answers.push(done());
    await p.poll();
    expect(p.times()).toEqual(["0:01.50", "0:03.20"]);
    p.$("mx-auto-stop").click();
    expect(p.times()).toEqual(["0:01.10", "0:03.10"]);
    expect(p.$("mx-edit-chip").textContent).toBe("Unsaved");
    expect(p.status()).toContain("Suggestion discarded");
    expect(p.$("mx-auto-run").hidden).toBe(false);
    expect(p.calls).toHaveLength(2); // a discard writes and sends nothing
  });

  it("cancel posts to the cancel route, stops polling and drops a late answer", async () => {
    const p = mount();
    await p.run(START);
    p.answers.push([200, '{"state":"canceled"}']);
    p.$("mx-auto-stop").click();
    await Promise.resolve();
    expect(p.calls[1]).toEqual({ url: "/preview/7/auto/cancel", method: "POST", body: { mtime: MTIME, csrf_token: "tok" } });
    expect(p.status()).toBe("Alignment canceled. Nothing changed.");
    expect(p.pending()).toBe(0);
    expect(p.$("mx-auto-run").hidden).toBe(false);
    expect(p.times()).toEqual(["0:01.00", "0:03.00"]);
  });

  it("Esc cancels a run, and discards a shown suggestion", async () => {
    const p = mount();
    await p.run(START);
    expect(p.$("mx-preview-keys").textContent).toContain("cancel alignment");
    p.answers.push([200, '{"state":"canceled"}']);
    p.esc();
    await Promise.resolve();
    expect(p.calls.map((c) => c.url)).toEqual(["/preview/7/auto", "/preview/7/auto/cancel"]);
    expect(p.status()).toContain("Alignment canceled");
    await p.run(START);
    p.answers.push(done());
    await p.poll();
    expect(p.$("mx-preview-keys").textContent).toContain("discard suggestion");
    p.esc();
    expect(p.times()).toEqual(["0:01.00", "0:03.00"]);
    expect(p.status()).toContain("Suggestion discarded");
  });

  it("a changed file mtime discards the suggestion and locks the editor", async () => {
    const p = mount();
    await p.run(START);
    p.answers.push(done("1700000000000000002"));
    await p.poll();
    expect(p.times()).toEqual(["0:01.00", "0:03.00"]);
    expect(p.status()).toContain("changed on disk, so the suggestion was discarded");
    expect(p.$("mx-auto-run").disabled).toBe(true);
    // the server's own 409 on a poll says the same
    const q = mount();
    await q.run(START);
    q.answers.push([409, '{"error":"changed"}']);
    await q.poll();
    expect(q.status()).toContain("changed on disk");
  });

  it("a 404 while running restarts the dropped run, then says it was dropped, never an aligner failure", async () => {
    const p = mount();
    await p.run(START);
    p.answers.push([404, "404 page not found\n"], START);
    await p.poll();
    expect(p.calls.map((c) => c.method)).toEqual(["POST", "GET", "POST"]);
    expect(p.$("mx-edit-chip").textContent).toBe("Aligning");
    p.answers.push([404, ""], START);
    await p.poll();
    p.answers.push([404, ""]);
    await p.poll();
    expect(p.calls.map((c) => c.method)).toEqual(["POST", "GET", "POST", "GET", "POST", "GET"]);
    expect(p.status()).toContain("The server dropped the alignment");
    expect(p.status()).not.toContain("aligner failed");
    expect(p.pending()).toBe(0);
  });

  it("retries a start refused as busy, then says so", async () => {
    const p = mount();
    await p.run([429, '{"error":"busy"}']);
    for (let i = 0; i < 3; i++) {
      p.answers.push([429, '{"error":"busy"}']);
      await p.poll();
    }
    expect(p.calls).toHaveLength(4);
    expect(p.status()).toBe("The aligner is busy. Try Auto again shortly.");
    expect(p.$("mx-auto-run").hidden).toBe(false);
  });

  const startCodes = [
    [401, "", "session has expired"],
    [403, "Forbidden", "session has expired"],
    [404, "", "no longer offered"],
    [413, '{"error":"too_large"}', "too large to align"],
    [422, '{"error":"no_lines"}', "No lyric line has words"],
    [503, '{"error":"unavailable"}', "not available right now"],
    [500, '{"error":"read"}', "could not run the alignment"],
    [0, null, "could not run the alignment"], // no answer queued: the fetch rejects
  ];
  it.each(startCodes)("a start answered %i says so", async (status, body, want) => {
    const p = mount();
    await (status ? p.run([status, body]) : p.run());
    expect(p.status()).toContain(want);
    expect(p.$("mx-edit-chip").textContent).toBe("Original");
  });

  const runCodes = [
    ["busy", "The aligner is busy. Try Auto again in 7 s."],
    ["unavailable", "not available right now"],
    ["timeout", "took too long"],
    ["canceled", "The server dropped the alignment"],
    ["rejected", "could not use this track"],
    ["too_large", "too large to align"],
    ["aligner", "The aligner failed on this track"],
  ];
  it.each(runCodes)("a run failed with %s says so", async (code, want) => {
    const p = mount();
    await p.run(START);
    p.answers.push([200, `{"state":"failed","error":"${code}"${code === "busy" ? ',"retry_after":7' : ""}}`]);
    await p.poll();
    expect(p.status()).toContain(want);
    expect(p.$("mx-auto-run").hidden).toBe(false);
    expect(p.times()).toEqual(["0:01.00", "0:03.00"]);
  });

  it("no_suggestion is not a failure, and a malformed suggestion is never shown", async () => {
    const p = mount();
    await p.run(START);
    p.answers.push([200, '{"state":"no_suggestion","aligned_words":0}']);
    await p.poll();
    expect(p.status()).toContain("found no usable timing");
    await p.run(START);
    p.answers.push(done(MTIME, "[1500]"));
    await p.poll();
    expect(p.times()).toEqual(["0:01.00", "0:03.00"]);
    expect(p.errors.join()).toContain("does not match the lyric lines");
  });
});
