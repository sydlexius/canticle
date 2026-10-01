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
