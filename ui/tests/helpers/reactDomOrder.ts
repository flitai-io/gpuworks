// Preloaded into every test process (package.json passes it to tsx with
// --import). react-dom decides once, when it loads, whether the DOM has
// native input events. A test file that installs its JSDOM globals after
// react-dom already loaded - a static import of antd or react-router-dom
// loads it before the file body runs - gets React's IE fallback instead: it
// stays bound to the first text input any test in the file focused, so a
// later test's typing never reaches onChange. This fails such a file at
// exit, naming the import that loaded react-dom first; import that module
// dynamically after the globals are installed.
import { createRequire } from "node:module";

// Zeron's popups (Base UI, framer-motion) call the bare requestAnimationFrame;
// jsdom has none. Files that build their own JSDOM never pass through
// helpers/dom.ts (which installs a per-test frame queue for ECharts), so the
// process gets a plain fallback here; dom.ts replaces it where it is used.
if (typeof globalThis.requestAnimationFrame !== "function") {
  Object.defineProperties(globalThis, {
    requestAnimationFrame: { configurable: true, writable: true, value: (callback: (time: number) => void) => setTimeout(() => callback(performance.now()), 16) as unknown as number },
    cancelAnimationFrame: { configurable: true, writable: true, value: (id: number) => clearTimeout(id) },
  });
}

const require = createRequire(import.meta.url);
const Module = require("node:module") as {
  _load: (request: string, parent: { filename?: string } | null, isMain: boolean) => unknown;
};

let loadedWithoutDom: string | undefined;
let compatibilityLoaded = false;
const load = Module._load;
Module._load = function (request, parent, isMain) {
  if (
    loadedWithoutDom === undefined &&
    typeof globalThis.document === "undefined" &&
    /^react-dom(\/|$)/.test(request)
  ) {
    loadedWithoutDom = parent?.filename ?? request;
  }
  const result = load.call(this, request, parent, isMain);
  // Tests mount components without main.tsx. Apply the same official patch
  // after antd and DOM globals exist, including files with their own JSDOM.
  // Mark first because the patch itself imports antd.
  if ((request === "antd" || /[/\\]antd[/\\]lib[/\\]index\.js$/.test(request)) && typeof globalThis.document !== "undefined" && !compatibilityLoaded) {
    compatibilityLoaded = true;
    require("@ant-design/v5-patch-for-react-19");
  }
  return result;
};

process.on("exit", () => {
  if (loadedWithoutDom === undefined || typeof globalThis.document === "undefined") return;
  console.error(
    `react-dom was loaded (by ${loadedWithoutDom}) before this test file installed its JSDOM globals; ` +
      "import it dynamically after the globals are installed (see tests/helpers/reactDomOrder.ts)",
  );
  process.exitCode = 1;
});
