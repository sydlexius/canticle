// Test harness for the served settings.js.
//
// It loads web/static/js/settings.js VERBATIM -- the same bytes the browser
// gets and web/static/embed.go embeds. Nothing is transpiled, bundled, or
// re-implemented here, so a test can only pass by agreeing with the shipped
// file. That is the whole point: the previous safety net for this file was Go
// tests asserting the rendered MARKUP plus manual UAT, neither of which can
// reach the script's own behavior.
//
// Each loadSettings() call builds a FRESH JSDOM window. settings.js is an IIFE
// that attaches delegated listeners to `document`, so reusing one document
// across tests would stack a second set of handlers and make one click save
// twice -- tests would then interfere through global state rather than through
// anything the code does.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";

const HERE = dirname(fileURLToPath(import.meta.url));
const SETTINGS_JS = join(HERE, "..", "settings.js");

// CSRF_TOKEN mirrors the shape the server mints: settings.js only reads the
// value, so any stable non-empty string exercises the same path.
export const CSRF_TOKEN = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

/**
 * fieldCard renders one settings card the way web/templates/settings.templ
 * does. The data-* attributes are the contract settings.js selects on, so they
 * are spelled out here rather than abbreviated.
 *
 * @param {object} opts
 * @param {string} opts.path   config path -> data-field-path and the control name
 * @param {string} opts.tier   "safe" | "caution" | "critical"
 * @param {string} opts.label  visible label text (the confirm dialog quotes it)
 * @param {string} opts.control inner HTML for the control itself
 */
export function fieldCard(opts) {
  const save =
    opts.tier === "safe"
      ? ""
      : `<button type="button" class="mx-settings-save"${
          opts.tier === "critical" ? ' data-confirm="true"' : ""
        }>Save</button>`;
  return `
    <div class="mx-settings-field" data-field-path="${opts.path}" data-field-tier="${opts.tier}">
      <div class="mx-settings-field-head">
        <label class="mx-settings-field-label" for="field-${opts.path}">${opts.label}</label>
      </div>
      ${opts.control}
      <div class="mx-settings-save-row">
        ${save}
        <span class="mx-settings-field-status" role="status" aria-live="polite"></span>
      </div>
    </div>`;
}

// TAB_SHELL is the CSS-only radio tab group from settings.templ. settings.js's
// syncTabs() console.errors on any missing id, so a fixture that omits these
// produces noise that looks like a failure but is not one.
const TAB_SHELL = `
  <input type="radio" name="mx-settings-tab" id="mx-tab-common" class="mx-tab-radio" checked>
  <input type="radio" name="mx-settings-tab" id="mx-tab-advanced" class="mx-tab-radio">
  <input type="radio" name="mx-settings-tab" id="mx-tab-raw" class="mx-tab-radio">
  <div class="mx-tablist" role="tablist" aria-label="Settings sections">
    <label class="mx-tab" id="mx-tabctl-common" for="mx-tab-common" role="tab"
           aria-controls="mx-panel-common" aria-selected="false" tabindex="0">Common</label>
    <label class="mx-tab" id="mx-tabctl-advanced" for="mx-tab-advanced" role="tab"
           aria-controls="mx-panel-advanced" aria-selected="false" tabindex="-1">Advanced</label>
    <label class="mx-tab" id="mx-tabctl-raw" for="mx-tab-raw" role="tab"
           aria-controls="mx-panel-raw" aria-selected="false" tabindex="-1">Config file</label>
  </div>`;

/**
 * loadSettings builds a window containing `cards`, evaluates the real
 * settings.js in it, and returns handles for driving and observing it.
 *
 * @param {string} cards  markup for the field cards under test
 * @param {object} [options]
 * @param {boolean} [options.csrf=true]  render the CSRF token input
 * @param {Array} [options.responses]    queued fetch responses, oldest first;
 *   each is {ok, status, text}. Exhausting the queue is itself a failure, so a
 *   test can never silently pass on a request it did not expect.
 */
export function loadSettings(cards, options = {}) {
  const { csrf = true, responses = [] } = options;

  const dom = new JSDOM(
    `<!doctype html><html><body>
      ${csrf ? `<input type="hidden" id="mx-csrf-token" value="${CSRF_TOKEN}">` : ""}
      ${TAB_SHELL}
      <div class="mx-tabpanel" id="mx-panel-common">${cards}</div>
    </body></html>`,
    { runScripts: "outside-only", url: "https://canticle.test/settings" },
  );

  const { window } = dom;

  // Every fetch settings.js makes, in order, with its parsed body. Assertions
  // read this rather than a spy's call list so the WIRE FORMAT is what gets
  // pinned -- that is the contract the Go handlers actually parse.
  const requests = [];
  const queued = responses.slice();

  window.fetch = (url, init) => {
    const body = new window.URLSearchParams(init.body);
    // getAll per key: `value` REPEATS for slices, provider sets and ordered
    // lists, and collapsing it to a single value would hide the ordering bug
    // class entirely.
    const fields = {};
    for (const key of new Set(body.keys())) {
      fields[key] = body.getAll(key);
    }
    requests.push({ url, method: init.method, headers: init.headers, fields });

    if (!queued.length) {
      return Promise.reject(new Error(`unexpected request to ${url}: no queued response`));
    }
    const next = queued.shift();
    return Promise.resolve({
      ok: next.ok,
      status: next.status,
      text: () => Promise.resolve(next.text ?? ""),
    });
  };

  window.eval(readFileSync(SETTINGS_JS, "utf8"));

  return {
    dom,
    window,
    document: window.document,
    requests,
    /** card returns the field card for a config path. */
    card: (path) => window.document.querySelector(`[data-field-path="${path}"]`),
    /** statusOf returns the text settings.js wrote to a card's status line. */
    statusOf: (path) =>
      window.document
        .querySelector(`[data-field-path="${path}"] .mx-settings-field-status`)
        .textContent,
    /**
     * settle flushes the promise chain inside saveField/saveSection. Those
     * handlers are `.then()` chains with no completion signal, so a test must
     * yield the microtask queue before asserting on the status they write.
     */
    settle: () => new Promise((resolve) => setTimeout(resolve, 0)),
  };
}

/** dispatch fires a bubbling event of `type` on `el`, as a real interaction would. */
export function dispatch(window, el, type) {
  el.dispatchEvent(new window.Event(type, { bubbles: true }));
}
