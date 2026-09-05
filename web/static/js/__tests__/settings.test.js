// Characterization tests for the settings page's CURRENT save behavior.
//
// These describe what settings.js does today, not what it should do. #836
// changes most of it -- per-tab Save replaces the per-field triggers, the
// safe-tier fields keep their zero-click path but leave the per-field POST for
// the tab batch, and the hardcoded restart notice becomes per-key data from the
// save response. Pinning the present behavior first makes
// each of those a VISIBLE, deliberate diff rather than an unnoticed side
// effect, which is the entire reason this harness lands before the rewrite.
//
// Where a test pins something #836 is expected to change, it says so.

import { describe, expect, it } from "vitest";
import { CSRF_TOKEN, dispatch, fieldCard, loadSettings } from "./harness.js";

const textControl = (path) =>
  `<input class="mx-settings-input" type="text" id="field-${path}" name="${path}" value="initial">`;

describe("saveField wire format", () => {
  it("posts the path, the value and the CSRF token as a form body", async () => {
    const h = loadSettings(
      fieldCard({ path: "server.addr", tier: "caution", label: "Listen address", control: textControl("server.addr") }),
      { responses: [{ ok: true, status: 200, text: "saved" }] },
    );

    h.card("server.addr").querySelector("input").value = "127.0.0.1:9999";
    h.card("server.addr").querySelector(".mx-settings-save").click();
    await h.settle();

    expect(h.requests).toHaveLength(1);
    const req = h.requests[0];
    expect(req.url).toBe("/settings/field");
    expect(req.method).toBe("POST");
    // Form-encoded, not JSON. The Go handler parses r.PostForm, and the CSRF
    // check reads the token from the BODY rather than a header -- so this
    // content type is load-bearing, not incidental.
    expect(req.headers["Content-Type"]).toBe("application/x-www-form-urlencoded");
    expect(req.fields.csrf_token).toEqual([CSRF_TOKEN]);
    expect(req.fields.path).toEqual(["server.addr"]);
    expect(req.fields.value).toEqual(["127.0.0.1:9999"]);
  });

  it("repeats the value key for an ordered list, preserving row order", async () => {
    // The ORDER is the value here. Reading these rows as an unordered set is a
    // real defect class this project has already shipped and fixed once, so the
    // ordering is pinned explicitly.
    const control = `
      <ol class="mx-orderlist" data-orderlist data-order-path="providers.fallback_order">
        <li class="mx-orderlist-item" data-value="petitlyrics"></li>
        <li class="mx-orderlist-item" data-value="musixmatch"></li>
        <li class="mx-orderlist-item mx-orderlist-item-off" data-value="lrclib"></li>
      </ol>`;
    const h = loadSettings(
      fieldCard({ path: "providers.fallback_order", tier: "caution", label: "Fallback order", control }),
      { responses: [{ ok: true, status: 200, text: "saved" }] },
    );

    h.card("providers.fallback_order").querySelector(".mx-settings-save").click();
    await h.settle();

    // Deactivated rows are excluded; the active two keep document order.
    expect(h.requests[0].fields.value).toEqual(["petitlyrics", "musixmatch"]);
  });

  it("sends a duration as a value plus its companion unit", async () => {
    const control = `
      <div class="mx-settings-duration">
        <input class="mx-settings-duration-num" type="number" id="field-api.cooldown" name="api.cooldown" value="5">
        <select class="mx-settings-duration-unit" name="api.cooldown.unit">
          <option value="seconds">seconds</option>
          <option value="minutes" selected>minutes</option>
        </select>
      </div>`;
    const h = loadSettings(
      fieldCard({ path: "api.cooldown", tier: "caution", label: "Cooldown", control }),
      { responses: [{ ok: true, status: 200, text: "saved" }] },
    );

    h.card("api.cooldown").querySelector(".mx-settings-save").click();
    await h.settle();

    expect(h.requests[0].fields.value).toEqual(["5"]);
    expect(h.requests[0].fields.unit).toEqual(["minutes"]);
  });
});

describe("per-field save triggers", () => {
  it("hot-saves a safe-tier field on change, with no button and no confirmation", async () => {
    // CHANGES IN #836: safe-tier fields are slated to keep the zero-click path,
    // but they leave the per-field POST behind for the tab batch. If this test
    // starts failing during that work, confirm the new behavior is intended
    // rather than restoring the old call.
    const h = loadSettings(
      fieldCard({ path: "logging.level", tier: "safe", label: "Log level", control: textControl("logging.level") }),
      { responses: [{ ok: true, status: 200, text: "saved" }] },
    );

    expect(h.card("logging.level").querySelector(".mx-settings-save")).toBeNull();

    dispatch(h.window, h.card("logging.level").querySelector("input"), "change");
    await h.settle();

    expect(h.requests).toHaveLength(1);
    expect(h.requests[0].fields.path).toEqual(["logging.level"]);
  });

  it("does not save a caution-tier field on change; the button is required", async () => {
    const h = loadSettings(
      fieldCard({ path: "server.addr", tier: "caution", label: "Listen address", control: textControl("server.addr") }),
    );

    dispatch(h.window, h.card("server.addr").querySelector("input"), "change");
    await h.settle();

    // No queued responses: an unexpected request would reject rather than pass.
    expect(h.requests).toEqual([]);
  });

  it("asks for confirmation on a critical field and abandons the save when declined", async () => {
    const h = loadSettings(
      fieldCard({ path: "server.tls.self_signed", tier: "critical", label: "Self-signed TLS", control: textControl("server.tls.self_signed") }),
    );

    const asked = [];
    h.window.confirm = (message) => {
      asked.push(message);
      return false;
    };

    h.card("server.tls.self_signed").querySelector(".mx-settings-save").click();
    await h.settle();

    expect(asked).toHaveLength(1);
    expect(asked[0]).toContain("Self-signed TLS");
    expect(h.requests).toEqual([]);
  });
});

describe("save status reporting", () => {
  it("reports the hardcoded restart notice on success", async () => {
    // CHANGES IN #836: this string is written by settings.js for EVERY field
    // regardless of the key. It becomes per-key data carried on the save
    // response, which is what #494 needs in order to say "applied" for the
    // three knobs it makes live.
    const h = loadSettings(
      fieldCard({ path: "server.addr", tier: "caution", label: "Listen address", control: textControl("server.addr") }),
      { responses: [{ ok: true, status: 200, text: "saved" }] },
    );

    h.card("server.addr").querySelector(".mx-settings-save").click();
    await h.settle();

    expect(h.statusOf("server.addr")).toBe("Saved - restart to apply");
  });

  it("surfaces the response body as the error text on a rejected save", async () => {
    // The server sends plain text, not JSON, and settings.js shows it verbatim.
    // This is how a cross-field invariant rejection reaches the operator.
    const h = loadSettings(
      fieldCard({ path: "providers.mode", tier: "caution", label: "Provider mode", control: textControl("providers.mode") }),
      {
        responses: [
          { ok: false, status: 400, text: "config: instrumental_detector.ordering=front requires providers.mode=ordered" },
        ],
      },
    );

    h.card("providers.mode").querySelector(".mx-settings-save").click();
    await h.settle();

    expect(h.statusOf("providers.mode")).toBe(
      "Error: config: instrumental_detector.ordering=front requires providers.mode=ordered",
    );
    const status = h.card("providers.mode").querySelector(".mx-settings-field-status");
    expect(status.classList.contains("mx-status-error")).toBe(true);
  });

  it("refuses to save at all when the CSRF token is absent", async () => {
    const h = loadSettings(
      fieldCard({ path: "server.addr", tier: "caution", label: "Listen address", control: textControl("server.addr") }),
      { csrf: false },
    );

    h.card("server.addr").querySelector(".mx-settings-save").click();
    await h.settle();

    expect(h.requests).toEqual([]);
    expect(h.statusOf("server.addr")).toBe("Cannot save: missing CSRF token (reload the page)");
  });
});

describe("unsaved work", () => {
  it("has no navigation guard: an edited, unsaved field is discarded silently", async () => {
    // This is the DEFECT #836 exists to fix, pinned as present behavior so the
    // fix is demonstrably a change. There is no beforeunload handler anywhere
    // in settings.js, so leaving the page loses a typed value with no warning.
    const h = loadSettings(
      fieldCard({ path: "server.addr", tier: "caution", label: "Listen address", control: textControl("server.addr") }),
    );

    // Edit the field the way a person does: set the value, then emit the events
    // a real keystroke emits. Without them this test could not tell a MISSING
    // guard from a correct EVENT-DRIVEN one -- it would keep passing after #836
    // adds the guard, which is the opposite of what it is for (CodeRabbit,
    // PR #847). Both events are sent because which one a guard listens on is an
    // implementation choice `input` (per keystroke) or `change` (on commit)
    // that this test must not pre-judge.
    const input = h.card("server.addr").querySelector("input");
    input.value = "127.0.0.1:9999";
    dispatch(h.window, input, "input");
    dispatch(h.window, input, "change");

    const event = new h.window.Event("beforeunload", { bubbles: false, cancelable: true });
    h.window.dispatchEvent(event);

    // Nothing armed the guard, so nothing cancels the navigation.
    //
    // returnValue is checked against its UNTOUCHED default of true, not against
    // a falsy value: on a cancelable event it is the legacy alias for "not
    // cancelled", so true means no handler intervened. A guard arms it by
    // assigning a STRING (the browser's prompt-and-ignore contract), which is
    // what this assertion will catch when #836 adds one.
    expect(event.defaultPrevented).toBe(false);
    expect(event.returnValue).toBe(true);
  });
});
