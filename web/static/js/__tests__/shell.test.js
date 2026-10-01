// Behavioral tests for the phone sidebar toggle (shell.js, #1094).
//
// shell.js is loaded VERBATIM into a fresh JSDOM window per test (the same
// approach as settings.test.js), so a test can only pass by agreeing with the
// shipped file. The Go tests assert the rendered markup and the script text;
// these reach the behavior, notably the htmx history-restore case where a
// listener bound to the original button goes dead.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { describe, expect, it } from "vitest";

const SHELL_JS = join(dirname(fileURLToPath(import.meta.url)), "..", "shell.js");

// The shell body as the server renders it: toggle, drawer with a link and some
// empty space, and content with a focusable control.
const BODY = `
  <header><button type="button" class="mx-nav-toggle" aria-expanded="false" aria-controls="mx-sidebar">Menu</button></header>
  <aside id="mx-sidebar"><a id="navlink" href="/queue">Queue</a><div id="empty-space">spacer</div></aside>
  <main><button type="button" id="content-btn">Content</button><p id="content-text">text</p></main>`;

function load({ phoneMatches = true } = {}) {
  const dom = new JSDOM(`<!doctype html><html><body>${BODY}</body></html>`, {
    runScripts: "outside-only",
    url: "https://canticle.test/",
    pretendToBeVisual: true,
  });
  const { window } = dom;
  const listeners = [];
  window.matchMedia = () => ({
    matches: phoneMatches,
    addEventListener: (_t, fn) => listeners.push(fn),
  });
  window.eval(readFileSync(SHELL_JS, "utf8"));
  const doc = window.document;
  return {
    window,
    doc,
    listeners,
    toggle: () => doc.querySelector(".mx-nav-toggle"),
    nav: () => doc.getElementById("mx-sidebar"),
    el: (id) => doc.getElementById(id),
    fire: (type, target = doc) => target.dispatchEvent(new window.Event(type, { bubbles: true })),
    // Moves real focus, which makes JSDOM emit focusout with relatedTarget.
    focus: (el) => el.focus(),
    // Simulates htmx restoring history: the body is rebuilt from markup (fresh
    // nodes, the original ones detached), then htmx:historyRestore fires.
    restoreBody: () => {
      doc.body.innerHTML = BODY;
      doc.dispatchEvent(new window.Event("htmx:historyRestore", { bubbles: true }));
    },
  };
}

const isOpen = (h) =>
  h.toggle().getAttribute("aria-expanded") === "true" && h.nav().hasAttribute("data-open");

describe("phone sidebar toggle", () => {
  it("opens the drawer on tap and moves focus into the nav", () => {
    const h = load();
    h.toggle().click();
    expect(h.toggle().getAttribute("aria-expanded")).toBe("true");
    expect(h.nav().getAttribute("data-open")).toBe("true");
    expect(h.doc.activeElement).toBe(h.nav());
  });

  it("still opens after an htmx history restore replaced the nodes", () => {
    const h = load();
    const original = h.toggle();
    h.restoreBody();
    expect(h.toggle()).not.toBe(original);
    expect(original.isConnected).toBe(false);

    h.toggle().click();
    expect(isOpen(h)).toBe(true);
    expect(h.doc.activeElement).toBe(h.nav());
  });

  it("closes when focus moves to content, not when it moves to the toggle", () => {
    const h = load();
    h.toggle().click();
    expect(isOpen(h)).toBe(true);

    h.focus(h.toggle());
    expect(isOpen(h)).toBe(true);

    h.focus(h.el("content-btn"));
    expect(h.toggle().getAttribute("aria-expanded")).toBe("false");
    expect(h.nav().hasAttribute("data-open")).toBe(false);
  });

  it("closes on a content or drawer-link click but not on empty drawer space", () => {
    const h = load();

    h.toggle().click();
    h.el("empty-space").click();
    expect(isOpen(h)).toBe(true);

    h.el("navlink").addEventListener("click", (e) => e.preventDefault());
    h.el("navlink").click();
    expect(isOpen(h)).toBe(false);

    h.toggle().click();
    expect(isOpen(h)).toBe(true);
    h.el("content-text").click();
    expect(isOpen(h)).toBe(false);
  });

  it("closes on Escape and returns focus to the toggle", () => {
    const h = load();
    h.toggle().click();
    h.doc.dispatchEvent(new h.window.KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(isOpen(h)).toBe(false);
    expect(h.toggle().getAttribute("aria-expanded")).toBe("false");
    expect(h.doc.activeElement).toBe(h.toggle());
  });

  it("closes before htmx snapshots history", () => {
    const h = load();
    h.toggle().click();
    expect(isOpen(h)).toBe(true);
    h.fire("htmx:beforeHistorySave");
    expect(h.toggle().getAttribute("aria-expanded")).toBe("false");
    expect(h.nav().hasAttribute("data-open")).toBe(false);
  });

  it("normalizes an open drawer when the viewport leaves phone width", () => {
    const h = load();
    h.toggle().click();
    expect(h.listeners).toHaveLength(1);
    h.listeners[0]({ matches: false });
    expect(h.toggle().getAttribute("aria-expanded")).toBe("false");
  });

  it("falls back to addListener when MediaQueryList lacks addEventListener", () => {
    const dom = new JSDOM(`<!doctype html><html><body>${BODY}</body></html>`, {
      runScripts: "outside-only",
      url: "https://canticle.test/",
    });
    const { window } = dom;
    const legacy = [];
    window.matchMedia = () => ({ matches: true, addListener: (fn) => legacy.push(fn) });
    window.eval(readFileSync(SHELL_JS, "utf8"));
    expect(legacy).toHaveLength(1);

    window.document.querySelector(".mx-nav-toggle").click();
    legacy[0]({ matches: false });
    expect(window.document.querySelector(".mx-nav-toggle").getAttribute("aria-expanded")).toBe("false");
  });
});
