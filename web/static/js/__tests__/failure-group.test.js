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

const HTMX = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "htmx.min.js"), "utf8");

const tick = (win) => new Promise((r) => win.setTimeout(r, 20));

// Real htmx against a controllable XHR, so the queued-click behavior under test
// is htmx's own (vendored 2.0.x queues the last click during a request).
async function loadWithHtmx(sync) {
  const dom = new JSDOM(
    `<body><table><tbody>
      <tr><td><button type="button" class="mx-fg-toggle" aria-expanded="false" aria-controls="mx-fg-0"
        hx-get="/frag" hx-target="#mx-fg-0" hx-swap="innerHTML" ${sync ? 'hx-sync="this:drop"' : ""}>Show rows</button></td></tr>
      <tr class="mx-fg-detail" hidden><td id="mx-fg-0"></td></tr>
    </tbody></table></body>`,
    { runScripts: "outside-only", url: "http://localhost/" },
  );
  const win = dom.window;
  const pending = [];
  win.XMLHttpRequest = class {
    constructor() {
      this.headers = {};
      this.upload = { addEventListener() {} };
      this.listeners = {};
    }
    open(method, url) { this.method = method; this.url = url; }
    setRequestHeader() {}
    addEventListener(t, f) { this.listeners[t] = f; }
    overrideMimeType() {}
    getAllResponseHeaders() { return ""; }
    getResponseHeader() { return null; }
    abort() { this.aborted = true; }
    send() { pending.push(this); }
    respond(body) {
      this.status = 200;
      this.readyState = 4;
      this.response = this.responseText = body;
      this.onload && this.onload();
    }
  };
  // jsdom's XPath needs a result type htmx omits; it only scans for hx-on attrs.
  const none = { iterateNext: () => null };
  win.XPathEvaluator = class {
    createExpression() { return { evaluate: () => none }; }
  };
  win.eval(HTMX);
  win.eval(JS);
  win.htmx.process(win.document.body);
  await tick(win); // let htmx's own DOM-ready init finish before any click
  return { win, pending };
}


describe("failure-group.js with real htmx", () => {
  // Drives one click, a second click while the request is pending, then the response.
  async function doubleClick(sync) {
    const { win, pending } = await loadWithHtmx(sync);
    const btn = win.document.querySelector(".mx-fg-toggle");
    const row = win.document.querySelector(".mx-fg-detail");
    expect(row.hidden).toBe(true);
    btn.click();
    await tick(win);
    btn.click(); // lands while the first request is in flight
    await tick(win);
    pending[0].respond("<p>rows</p>");
    await tick(win);
    expect(win.document.getElementById("mx-fg-0").innerHTML).toContain("rows");
    return { btn, row, pending };
  }

  it("a second click while loading leaves the rows expanded (hx-sync=this:drop)", async () => {
    const { btn, row, pending } = await doubleClick(true);
    expect(pending.length).toBe(1);
    expect(btn.getAttribute("aria-expanded")).toBe("true");
    expect(row.hidden).toBe(false);
  });

  it("control: without hx-sync htmx queues the click and collapses the rows", async () => {
    const { btn, row } = await doubleClick(false);
    expect(btn.getAttribute("aria-expanded")).toBe("false");
    expect(row.hidden).toBe(true);
  });

  it("swaps into the cell while the detail row is hidden", async () => {
    const { win, pending } = await loadWithHtmx(true);
    win.document.querySelector(".mx-fg-toggle").click();
    await tick(win);
    expect(win.document.querySelector(".mx-fg-detail").hidden).toBe(true);
    pending[0].respond("<p>rows</p>");
    await tick(win);
    expect(win.document.getElementById("mx-fg-0").innerHTML).toContain("rows");
  });
});

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
