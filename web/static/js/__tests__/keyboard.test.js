// keyboard.js (#481): platform-aware shortcut registry that renders its own legend.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { afterEach, describe, expect, it, vi } from "vitest";

const JS = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "keyboard.js"), "utf8");

// Every window load() creates is closed afterwards, pass or fail, so a test's
// document listeners and timers never outlive it.
const openWindows = [];
afterEach(() => {
  while (openWindows.length > 0) {
    openWindows.pop().close();
  }
});

function load(platform, { userAgentData } = {}) {
  const dom = new JSDOM("<body></body>", { runScripts: "outside-only" });
  const win = dom.window;
  openWindows.push(win);
  Object.defineProperty(win.navigator, "platform", { value: platform, configurable: true });
  if (userAgentData) {
    Object.defineProperty(win.navigator, "userAgentData", { value: userAgentData, configurable: true });
  }
  win.eval(JS);
  return win;
}

function press(win, init, target) {
  const e = new win.KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init });
  (target || win.document).dispatchEvent(e);
  return e;
}

describe("keyboard.js", () => {
  it("binds Mod+S to Cmd on macOS and labels it", () => {
    const win = load("MacIntel");
    const kb = win.mxKeyboard;
    const save = vi.fn();
    kb.register({ keys: ["Mod", "s"], label: "save", handler: save, allowInInput: true });
    const e = press(win, { key: "s", metaKey: true });
    expect(save).toHaveBeenCalledOnce();
    expect(e.defaultPrevented).toBe(true);
    const div = win.document.createElement("div");
    kb.renderLegend(div);
    expect(div.textContent).toContain("⌘");
    expect(kb.isMac).toBe(true);
  });

  it("binds Mod+S to Ctrl elsewhere and ignores Cmd", () => {
    const win = load("Win32");
    const save = vi.fn();
    win.mxKeyboard.register({ keys: ["Mod", "s"], label: "save", handler: save, allowInInput: true });
    press(win, { key: "s", metaKey: true });
    expect(save).not.toHaveBeenCalled();
    press(win, { key: "s", ctrlKey: true });
    expect(save).toHaveBeenCalledOnce();
    const div = win.document.createElement("div");
    win.mxKeyboard.renderLegend(div);
    expect(div.textContent).toContain("Ctrl");
  });

  it("prefers userAgentData.platform over navigator.platform", () => {
    const win = load("Win32", { userAgentData: { platform: "macOS" } });
    expect(win.mxKeyboard.isMac).toBe(true);
  });

  it("ignores letter shortcuts while typing, but not allowInInput entries", () => {
    const win = load("Linux x86_64");
    const nudge = vi.fn();
    const esc = vi.fn();
    win.mxKeyboard.register({ keys: ["]"], label: "+0.1 s", handler: nudge });
    win.mxKeyboard.register({ keys: ["Escape"], label: "discard", handler: esc, allowInInput: true });
    for (const tag of ["input", "textarea", "select"]) {
      const el = win.document.createElement(tag);
      win.document.body.appendChild(el);
      const e = press(win, { key: "]" }, el);
      expect(e.defaultPrevented).toBe(false);
    }
    const ce = win.document.createElement("div");
    ce.setAttribute("contenteditable", "true");
    win.document.body.appendChild(ce);
    press(win, { key: "]" }, ce);
    expect(nudge).not.toHaveBeenCalled();
    press(win, { key: "Escape" }, ce);
    expect(esc).toHaveBeenCalledOnce();
    press(win, { key: "]" });
    expect(nudge).toHaveBeenCalledOnce();
  });

  it("requires exact modifiers and matches shifted bracket by code", () => {
    const win = load("Linux x86_64");
    const plain = vi.fn();
    const shifted = vi.fn();
    win.mxKeyboard.register({ keys: ["]"], label: "a", handler: plain });
    win.mxKeyboard.register({ keys: ["Shift", "]"], label: "b", handler: shifted });
    press(win, { key: "]", ctrlKey: true });
    expect(plain).not.toHaveBeenCalled();
    press(win, { key: "}", code: "BracketRight", shiftKey: true });
    expect(shifted).toHaveBeenCalledOnce();
    expect(plain).not.toHaveBeenCalled();
  });

  it("renders a legend matching the registry", () => {
    const win = load("Linux x86_64");
    const kb = win.mxKeyboard;
    kb.register({ keys: ["["], label: "-0.1 s", handler() {} });
    kb.register({ keys: ["Escape"], label: "discard", handler() {}, allowInInput: true });
    const div = win.document.createElement("div");
    kb.renderLegend(div);
    kb.renderLegend(div); // re-render replaces, never accumulates
    expect(div.querySelectorAll("kbd").length).toBe(2);
    expect(div.classList.contains("mx-kbd-legend")).toBe(true);
    expect(kb.list().map((e) => e.label)).toEqual(["-0.1 s", "discard"]);
  });

  it("re-init keeps the existing registry and a single listener", () => {
    const win = load("Linux x86_64");
    const first = win.mxKeyboard;
    const fn = vi.fn();
    first.register({ keys: ["x"], label: "x", handler: fn });
    win.eval(JS);
    expect(win.mxKeyboard).toBe(first);
    press(win, { key: "x" });
    expect(fn).toHaveBeenCalledOnce();
  });
});
