import { defineConfig } from "vitest/config";

// `environment: "node"`, deliberately, even though every test here drives a DOM.
//
// settings.js is an IIFE that attaches DELEGATED listeners to `document` when it
// loads. Under vitest's shared `jsdom` environment the same document persists
// across tests in a file, so loading the script a second time would stack a
// second set of handlers on it and a single click would then save twice. Tests
// would interfere through global state rather than through anything the code
// does.
//
// So each test builds its OWN JSDOM window (see __tests__/harness.js) and the
// runner itself stays in plain node. That is what makes it safe to load the real
// file repeatedly and still observe it honestly.
export default defineConfig({
  test: {
    environment: "node",
    include: ["web/static/js/__tests__/**/*.test.js"],
  },
});
