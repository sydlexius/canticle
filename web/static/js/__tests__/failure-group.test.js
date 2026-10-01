// failure-group.js (#478): a loaded failure group toggles without refetching.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { describe, expect, it } from "vitest";

const JS = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "failure-group.js"), "utf8");

function load() {
  const dom = new JSDOM(
    `<body><table><tbody>
      <tr><td><button type="button" class="mx-fg-toggle" aria-expanded="false" aria-controls="mx-fg-0">Show rows</button></td></tr>
      <tr class="mx-fg-detail"><td id="mx-fg-0"></td></tr>
    </tbody></table></body>`,
    { runScripts: "outside-only" },
  );
  dom.window.eval(JS);
  return dom.window;
}

// htmx shapes: beforeRequest detail.elt is the trigger; afterSwap detail.elt is
// the swap target and the trigger rides in requestConfig.elt.
const fire = (win, type, elt) => {
  const detail =
    type === "htmx:afterSwap"
      ? { elt: win.document.getElementById("mx-fg-0"), requestConfig: { elt } }
      : { elt };
  const ev = new win.CustomEvent(type, { bubbles: true, cancelable: true, detail });
  win.document.body.dispatchEvent(ev);
  return ev;
};

describe("failure-group.js toggle", () => {
  it("lets the first request through, then toggles without one", () => {
    const win = load();
    const btn = win.document.querySelector(".mx-fg-toggle");
    const row = win.document.querySelector(".mx-fg-detail");

    expect(fire(win, "htmx:beforeRequest", btn).defaultPrevented).toBe(false);
    fire(win, "htmx:afterSwap", btn);
    expect(btn.getAttribute("aria-expanded")).toBe("true");
    expect(btn.textContent).toBe("Hide rows");
    expect(row.hidden).toBe(false);

    expect(fire(win, "htmx:beforeRequest", btn).defaultPrevented).toBe(true);
    expect(btn.getAttribute("aria-expanded")).toBe("false");
    expect(btn.textContent).toBe("Show rows");
    expect(row.hidden).toBe(true);

    expect(fire(win, "htmx:beforeRequest", btn).defaultPrevented).toBe(true);
    expect(btn.getAttribute("aria-expanded")).toBe("true");
    expect(row.hidden).toBe(false);
  });

  it("ignores other elements", () => {
    const win = load();
    const ev = fire(win, "htmx:beforeRequest", win.document.body);
    expect(ev.defaultPrevented).toBe(false);
    fire(win, "htmx:afterSwap", win.document.body);
    expect(win.document.querySelector(".mx-fg-toggle").hasAttribute("data-loaded")).toBe(false);
  });
});
