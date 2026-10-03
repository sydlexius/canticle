// queue.js (#598): focus lands on the appended batch's first row, not <body>.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { describe, expect, it } from "vitest";

const QUEUE_JS = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "queue.js"), "utf8");

function load() {
  const dom = new JSDOM(
    `<body><table><tbody id="mx-queue-body">
      <tr data-queue-batch="first"><td>old</td></tr>
      <tr data-queue-batch="first" id="new"><td>new</td></tr>
      <tr class="mx-queue-more-row"><td><a class="mx-queue-more" href="#">Show more</a></td></tr>
    </tbody></table></body>`,
    { runScripts: "outside-only" },
  );
  dom.window.eval(QUEUE_JS);
  return dom.window;
}

const settle = (win, elt) =>
  win.document.body.dispatchEvent(
    new win.CustomEvent("htmx:afterSettle", { bubbles: true, detail: { requestConfig: { elt } } }),
  );

describe("queue.js focus after Show more", () => {
  it("focuses the first row of the newest batch and drops tabindex on blur", () => {
    const win = load();
    const link = win.document.querySelector(".mx-queue-more");
    settle(win, link);
    const row = win.document.getElementById("new");
    expect(win.document.activeElement).toBe(row);
    expect(row.getAttribute("tabindex")).toBe("-1");
    row.blur();
    expect(row.hasAttribute("tabindex")).toBe(false);
  });

  it("ignores settles that did not come from the Show more control", () => {
    const win = load();
    settle(win, win.document.body);
    expect(win.document.activeElement).toBe(win.document.body);
  });
});

// Filter selects (#1235): a change submits the form, the select is refocused
// after the reload, and a missing form or blocked storage never fails silently
// or breaks the filter.
function loadFilter(html, before, path = "/queue/settled") {
  const dom = new JSDOM(`<body>${html}</body>`, { runScripts: "outside-only", url: "http://localhost" + path });
  const win = dom.window;
  const calls = { submit: 0, errors: [] };
  win.console.error = (...a) => calls.errors.push(a.join(" "));
  const form = win.document.querySelector("form");
  if (form) form.requestSubmit = () => { calls.submit++; };
  if (before) before(win);
  win.eval(QUEUE_JS);
  return { win, calls };
}

const FORM = `<form><select id="mx-queue-library" name="library" data-queue-autosubmit>
  <option value="">All</option><option value="1">One</option></select>
  <select id="plain" name="x"><option value="">a</option><option value="1">b</option></select></form>`;

const change = (win, id) =>
  win.document.getElementById(id).dispatchEvent(new win.Event("change", { bubbles: true }));

const KEY = "mx-queue-refocus";
// remember seeds the entry a change on `path` would have left `ageMs` ago.
const remember = (id, path = "/queue/settled", ageMs = 0) => (win) =>
  win.sessionStorage.setItem(KEY, JSON.stringify({ id, path, at: Date.now() - ageMs }));

describe("queue.js filter selects", () => {
  it("submits the form when an auto-submit select changes, and only then", () => {
    const { win, calls } = loadFilter(FORM);
    change(win, "plain");
    expect(calls.submit).toBe(0);
    change(win, "mx-queue-library");
    expect(calls.submit).toBe(1);
  });

  it("logs an error instead of a silent no-op when the select has no form", () => {
    const { win, calls } = loadFilter(`<select id="orphan" data-queue-autosubmit><option>a</option></select>`);
    change(win, "orphan");
    expect(calls.submit).toBe(0);
    expect(calls.errors.length).toBe(1);
  });

  it("refocuses the changed select after the reload", () => {
    const first = loadFilter(FORM);
    change(first.win, "mx-queue-library");
    const stored = JSON.parse(first.win.sessionStorage.getItem(KEY));
    expect([stored.id, stored.path]).toEqual(["mx-queue-library", "/queue/settled"]);
    expect(Math.abs(Date.now() - stored.at)).toBeLessThan(2000);
    // The reload reads exactly what the change wrote.
    const reloaded = loadFilter(FORM, (win) => win.sessionStorage.setItem(KEY, JSON.stringify(stored)));
    expect(reloaded.win.document.activeElement.id).toBe("mx-queue-library");
    expect(reloaded.win.sessionStorage.getItem(KEY)).toBe(null);
  });

  it("ignores, and still removes, an entry left by another page or an old submit", () => {
    const stale = {
      "another bucket": [remember("mx-queue-library", "/queue/pending"), "/queue/settled"],
      "an old submit": [remember("mx-queue-library", "/queue/settled", 60000), "/queue/settled"],
      "a clock that moved back": [remember("mx-queue-library", "/queue/settled", -60000), "/queue/settled"],
      "no timestamp": [(w) => w.sessionStorage.setItem(KEY, JSON.stringify({ id: "mx-queue-library", path: "/queue/settled" })), "/queue/settled"],
      "a bare id": [(w) => w.sessionStorage.setItem(KEY, "mx-queue-library"), "/queue/settled"],
    };
    for (const [name, [seed, path]] of Object.entries(stale)) {
      const { win, calls } = loadFilter(FORM, seed, path);
      expect(win.document.activeElement, name).toBe(win.document.body);
      expect(win.sessionStorage.getItem(KEY), name).toBe(null);
      expect(calls.errors, name).toEqual([]);
    }
  });

  it("does not focus an element that is not an auto-submit select", () => {
    const { win } = loadFilter(FORM, remember("plain"));
    expect(win.document.activeElement).toBe(win.document.body);
  });

  it("still submits when storage is blocked", () => {
    const { win, calls } = loadFilter(FORM, (w) => {
      Object.defineProperty(w, "sessionStorage", { get() { throw new Error("blocked"); } });
    });
    change(win, "mx-queue-library");
    expect(calls.submit).toBe(1);
    expect(calls.errors.length).toBe(0);
  });
});
