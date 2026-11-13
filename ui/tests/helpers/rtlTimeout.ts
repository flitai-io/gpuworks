// Preloaded into every test process (package.json passes it to tsx with
// --import). React Testing Library's waitFor/findBy give up after 1s by
// default, which a loaded machine outlasts while a component merely renders,
// so the suite failed at random under load (todo T23). A generous bound costs
// nothing when tests pass: waitFor returns as soon as its callback does.
// A CI runner starved by concurrent pipelines outlasted even 15 s (the images
// page's series list stayed behind its loading veil), hence 30 s.
//
// Only the config module is loaded here. The package entry would evaluate
// `screen` before a test file installs its JSDOM globals, binding it to "no
// document"; config.js is the same CommonJS module instance the entry uses.
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { configure } = require("@testing-library/dom/dist/config.js") as {
  configure: (config: { asyncUtilTimeout: number }) => void;
};

configure({ asyncUtilTimeout: 30_000 });

// Node 22 tries to parse an entire source-mapped TSX file when assert.ok has
// no message. On these large fixtures that blocks the event loop inside
// findColumn/parseCode, so waitFor cannot reach its timeout. Keep the exact
// assertion, with a fallback message instead of implicit source extraction.
import assert from "node:assert/strict";
const originalOk = assert.ok;
assert.ok = ((value: unknown, message?: string | Error) => {
  originalOk(value, message ?? "Expected a truthy value (assertion has no explicit message)");
}) as typeof assert.ok;
