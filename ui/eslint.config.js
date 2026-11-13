import parser from "@typescript-eslint/parser";
import noCJKLiteral from "./eslint-local-rules/no-cjk-literal.mjs";

// M3 is complete: every TypeScript source file is now guarded. Locale
// resources are JSON and tests live outside src, so neither needs an ignore.
export default [
  // Zeron-managed sources (installed by the Zeron CLI, hash-tracked in
  // .zeron/install-state.json) follow Zeron's own lint, not this project's.
  { ignores: ["coverage/**", "src/components/ui/**", "src/lib/**", "src/hooks/use-*"] },
  {
    files: ["src/**/*.{ts,tsx}"],
    languageOptions: {
      parser,
      parserOptions: {
        ecmaFeatures: { jsx: true },
        ecmaVersion: "latest",
        sourceType: "module",
      },
    },
    plugins: {
      "gpuworks-i18n": { rules: { "no-cjk-literal": noCJKLiteral } },
    },
    rules: {
      "gpuworks-i18n/no-cjk-literal": "error",
      // navigator.clipboard is undefined on plain-HTTP deployments; copies go
      // through src/clipboard.ts (or components/CopyButton) for the fallback.
      "no-restricted-properties": ["error", {
        object: "navigator",
        property: "clipboard",
        message: "Use copyText from src/clipboard.ts or components/CopyButton; navigator.clipboard is unavailable on plain HTTP.",
      }],
    },
  },
  {
    files: ["src/clipboard.ts"],
    rules: { "no-restricted-properties": "off" },
  },
];
