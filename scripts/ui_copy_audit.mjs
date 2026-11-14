#!/usr/bin/env node
// Counts Chinese UI strings that break docs/dev/ui.md "Copy on the page".
//   node scripts/ui_copy_audit.mjs long     -> strings over LONG characters
//   node scripts/ui_copy_audit.mjs caveat   -> strings carrying a caveat phrase
//   node scripts/ui_copy_audit.mjs list     -> both, one "kind ns:key len text" per line
// The counts feed ui_copy_long_strings / ui_copy_caveat_strings in
// scripts/arch-constraints.txt. Node built-ins only.
import { readdirSync, readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

export const LONG = 120;
// HelpTip content (keys ending in "Help"/"help") may carry caveats and run to
// HELP_LONG characters; past that it belongs in the docs.
export const HELP_LONG = 200;
const isHelp = (key) => /help$/i.test(key);
// Caveats the rules move into a HelpTip; facts such as "不包含模型文件" are not caveats.
export const CAVEAT = /不保证|仅供参考|不代表|未经(节点)?实测|并不意味|不改变|仅用于界面演示/;

const dir = join(dirname(fileURLToPath(import.meta.url)), "..", "ui", "src", "i18n", "locales", "zh");

function* strings(node, prefix) {
  for (const [key, value] of Object.entries(node)) {
    const path = prefix ? `${prefix}.${key}` : key;
    if (value && typeof value === "object") yield* strings(value, path);
    else if (typeof value === "string") yield [path, value];
  }
}

const found = [];
for (const file of readdirSync(dir).filter((f) => f.endsWith(".json")).sort()) {
  const ns = file.slice(0, -5);
  for (const [key, text] of strings(JSON.parse(readFileSync(join(dir, file), "utf8")), "")) {
    const length = [...text].length;
    if (length > (isHelp(key) ? HELP_LONG : LONG)) found.push({ kind: "long", id: `${ns}:${key}`, length, text });
    if (!isHelp(key) && CAVEAT.test(text)) found.push({ kind: "caveat", id: `${ns}:${key}`, length, text });
  }
}

const mode = process.argv[2] ?? "list";
if (mode === "long" || mode === "caveat") console.log(found.filter((f) => f.kind === mode).length);
else if (mode === "list") for (const f of found) console.log(`${f.kind}\t${f.id}\t${f.length}\t${f.text}`);
else { console.error(`unknown mode ${mode}: want long, caveat or list`); process.exit(2); }
