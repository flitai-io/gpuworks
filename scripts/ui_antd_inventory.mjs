#!/usr/bin/env node
// ui_antd_inventory: where the console UI still uses antd, and which menu page
// owns each source file. Node built-ins only.
//
//   node scripts/ui_antd_inventory.mjs                 human summary
//   node scripts/ui_antd_inventory.mjs --json          full inventory + page attribution
//   node scripts/ui_antd_inventory.mjs --write [path]  regenerate the snapshot (default ui/antd-inventory.json)
//   node scripts/ui_antd_inventory.mjs --check [path]  fail when a file's antd usage grew against the snapshot
//   node scripts/ui_antd_inventory.mjs --affected <file...>   menu pages to re-check, JSON array
//   node scripts/ui_antd_inventory.mjs --base <ref>           same, for `git diff --name-only <ref>` + untracked
//
// Scope, counting rules and the attribution model: scripts/arch-constraints.md
// (ui_antd_inventory).

import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const uiRoot = join(repoRoot, "ui");
const srcRoot = join(uiRoot, "src");
const defaultSnapshot = "ui/antd-inventory.json";
const sourceExt = /\.(tsx?|jsx?|css)$/;

// Repo-relative POSIX path, the one spelling used in every output.
const rel = (abs) => relative(repoRoot, abs).split(sep).join("/");

function walk(dir, out = []) {
  for (const name of readdirSync(dir).sort()) {
    const abs = join(dir, name);
    if (statSync(abs).isDirectory()) walk(abs, out);
    else out.push(abs);
  }
  return out;
}

const isTest = (p) => /\.test\.[^/]+$/.test(p);

// Inventory scope: managed Zeron code (components/ui, lib, hooks/use-*),
// tests and locale text are not migration targets.
function inInventoryScope(p) {
  const r = rel(p);
  if (!sourceExt.test(r) || isTest(r)) return false;
  return !/^ui\/src\/(components\/ui\/|lib\/|hooks\/use-|i18n\/)/.test(r);
}

// --- source scanning -------------------------------------------------------

function stripComments(text) {
  // Only comments that start a line (block, line or JSX {/* */}): anywhere
  // else "/*" and "//" may sit inside a string ("/playground/*") or a URL, and
  // this is not a tokenizer. A trailing comment after code is left alone.
  const blank = (m) => m.replace(/[^\n]/g, " ");
  return text
    .replace(/^[ \t]*\{?\/\*[\s\S]*?\*\/\}?/gm, blank)
    .replace(/^[ \t]*\/\/.*$/gm, "");
}

// importClauses yields {spec, clause, typeOnly} for static imports and re-exports.
function* importClauses(code) {
  const re = /(?:^|[\n;])\s*(import|export)\s+(type\s+)?([^;()=]*?)\s*from\s*["']([^"']+)["']/g;
  let m;
  while ((m = re.exec(code))) {
    yield { kind: m[1], typeOnly: Boolean(m[2]), clause: m[3], spec: m[4] };
  }
}

// parseBindings turns an import clause into [{local, imported, type}].
function parseBindings(clause, typeOnly, subpath) {
  const out = [];
  const braces = clause.match(/\{([\s\S]*)\}/);
  const head = clause.replace(/\{[\s\S]*\}/, "").replace(/,/g, " ").trim();
  if (head && !head.startsWith("*")) {
    // Default import: named after the subpath it came from (antd/locale/zh_CN).
    out.push({ local: head.split(/\s+/)[0], imported: subpath || "default", type: typeOnly });
  }
  const star = clause.match(/\*\s+as\s+(\w+)/);
  if (star) out.push({ local: star[1], imported: "*", type: typeOnly });
  if (braces) {
    for (const part of braces[1].split(",")) {
      const t = part.trim();
      if (!t) continue;
      const pm = t.match(/^(type\s+)?(\w+)(?:\s+as\s+(\w+))?$/);
      if (!pm) continue;
      out.push({ local: pm[3] || pm[2], imported: pm[2], type: typeOnly || Boolean(pm[1]) });
    }
  }
  return out;
}

const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const countMatches = (code, re) => (code.match(re) || []).length;

function scanScript(code) {
  const antdBindings = [];
  let icons = 0;
  for (const { clause, spec, typeOnly } of importClauses(code)) {
    const am = spec.match(/^antd(?:\/(.+))?$/);
    if (am) antdBindings.push(...parseBindings(clause, typeOnly, am[1] ? `antd/${am[1]}` : ""));
    else if (/^@ant-design\/icons(\/|$)/.test(spec)) icons += parseBindings(clause, typeOnly, spec).length;
  }
  // Dynamic import("antd") counts as one import with no attributable sites.
  const dynamicAntd = countMatches(code, /import\(\s*["']antd(?:\/[^"']*)?["']\s*\)/g);
  if (!antdBindings.length && !icons && !dynamicAntd) return null;

  const components = {};
  const imports = new Set();
  const types = new Set();
  if (dynamicAntd) imports.add("import()");
  const runtime = [];
  for (const b of antdBindings) {
    if (b.type) {
      types.add(b.imported);
      continue;
    }
    imports.add(b.imported);
    components[b.imported] ??= 0;
    runtime.push(b);
  }
  // const { Text } = Typography;  =>  <Text counts for Typography.
  for (const b of [...runtime]) {
    const dm = new RegExp(`(?:const|let|var)\\s*\\{([^}]*)\\}\\s*=\\s*${escapeRe(b.local)}\\s*[;\\n]`, "g");
    let m;
    while ((m = dm.exec(code))) {
      for (const part of m[1].split(",")) {
        const pm = part.trim().match(/^(\w+)(?:\s*:\s*(\w+))?$/);
        if (pm) runtime.push({ local: pm[2] || pm[1], imported: b.imported });
      }
    }
  }
  for (const b of runtime) {
    const name = escapeRe(b.local);
    const jsx = countMatches(code, new RegExp(`<${name}(?=[\\s>/.])`, "g"));
    const calls = countMatches(code, new RegExp(`(?<![\\w.$])${name}\\.\\w+\\s*\\(`, "g"));
    components[b.imported] += jsx + calls;
  }
  const sites = Object.values(components).reduce((a, b) => a + b, 0);
  return { components: sortObject(components), icons, imports: [...imports].sort(), sites, types: [...types].sort() };
}

// Rules whose selector names an antd class (.ant-*), counted per rule block.
function scanCss(text) {
  const code = text.replace(/\/\*[\s\S]*?\*\//g, "");
  let antRules = 0;
  const re = /([^{}]+)\{/g;
  let m;
  while ((m = re.exec(code))) {
    if (/^\s*@/.test(m[1])) continue;
    if (/\.ant-[\w-]/.test(m[1])) antRules++;
  }
  return antRules ? { antRules } : null;
}

function sortObject(o) {
  return Object.fromEntries(Object.keys(o).sort().map((k) => [k, o[k]]));
}

function buildInventory(allFiles) {
  const files = {};
  for (const abs of allFiles) {
    if (!inInventoryScope(abs)) continue;
    const text = readFileSync(abs, "utf8");
    const entry = abs.endsWith(".css") ? scanCss(text) : scanScript(stripComments(text));
    if (entry) files[rel(abs)] = entry;
  }
  const components = {};
  let sites = 0;
  let antdFiles = 0;
  let iconFiles = 0;
  let icons = 0;
  let antRules = 0;
  let cssFiles = 0;
  for (const e of Object.values(files)) {
    if (e.imports?.length || e.types?.length) antdFiles++;
    if (e.icons) {
      iconFiles++;
      icons += e.icons;
    }
    if (e.antRules) {
      cssFiles++;
      antRules += e.antRules;
    }
    sites += e.sites || 0;
    for (const [k, v] of Object.entries(e.components || {})) components[k] = (components[k] || 0) + v;
  }
  return {
    components: sortObject(components),
    files: sortObject(files),
    summary: { antRules, antdFiles, cssFilesWithAntRules: cssFiles, iconFiles, iconImports: icons, sites },
  };
}

// --- page attribution ------------------------------------------------------

const resolveExts = ["", ".tsx", ".ts", ".jsx", ".js", ".css", ".json", "/index.tsx", "/index.ts", "/index.js"];

function resolveSpec(fromAbs, spec) {
  const clean = spec.replace(/\?.*$/, "");
  let base;
  if (clean.startsWith("@/")) base = join(srcRoot, clean.slice(2));
  else if (clean.startsWith(".")) base = resolve(dirname(fromAbs), clean);
  else return null; // a package
  for (const ext of resolveExts) {
    const p = base + ext;
    if (existsSync(p) && statSync(p).isFile()) return p;
  }
  return null;
}

function importSpecs(abs, text) {
  const specs = [];
  if (abs.endsWith(".css")) {
    for (const m of text.matchAll(/@import\s+(?:url\()?["']([^"']+)["']/g)) specs.push(m[1]);
    return specs;
  }
  const code = stripComments(text);
  for (const { spec } of importClauses(code)) specs.push(spec);
  for (const m of code.matchAll(/(?:^|[\n;])\s*import\s*["']([^"']+)["']/g)) specs.push(m[1]);
  for (const m of code.matchAll(/import\(\s*["']([^"']+)["']\s*\)/g)) specs.push(m[1]);
  return specs;
}

function buildGraph(allFiles) {
  const graph = new Map();
  for (const abs of allFiles) {
    if (!/\.(tsx?|jsx?|css)$/.test(abs) || isTest(abs)) continue;
    const deps = new Set();
    for (const spec of importSpecs(abs, readFileSync(abs, "utf8"))) {
      const target = resolveSpec(abs, spec);
      if (target) deps.add(target);
    }
    graph.set(abs, deps);
  }
  return graph;
}

// navKeys reads the side menu's entries (key + also) from navigation.tsx.
function navEntries() {
  const text = readFileSync(join(srcRoot, "navigation.tsx"), "utf8");
  const out = [];
  for (const m of text.matchAll(/\{\s*key:\s*"([^"]+)"([^}]*)\}/g)) {
    const also = (m[2].match(/also:\s*\[([^\]]*)\]/)?.[1] || "").match(/"[^"]+"/g)?.map((s) => s.slice(1, -1)) || [];
    out.push({ key: m[1], also });
  }
  return out;
}

// pageForPath mirrors navItemForPath: the menu entry owning a route's first segment.
function pageForPath(path, entries) {
  const section = path.split("/")[1] || "";
  for (const e of entries) {
    if (e.key.split("/")[1] === section || e.also.includes(section)) return e.key;
  }
  if (path === "*") return "/not-found";
  return null;
}

function routeElements(appAbs, graph) {
  const text = stripComments(readFileSync(appAbs, "utf8"));
  const localToModule = new Map();
  for (const { clause, spec } of importClauses(text)) {
    const target = resolveSpec(appAbs, spec);
    if (!target) continue;
    for (const b of parseBindings(clause, false, "")) localToModule.set(b.local, target);
  }
  const routes = [];
  for (const m of text.matchAll(/<Route\s+path="([^"]+)"\s+element=\{<(\w+)/g)) {
    const mod = localToModule.get(m[2]);
    if (mod && graph.has(mod)) routes.push({ path: m[1], module: mod });
  }
  return routes;
}

function reach(graph, starts, skipEdge) {
  const seen = new Set();
  const stack = [...starts];
  while (stack.length) {
    const cur = stack.pop();
    if (seen.has(cur)) continue;
    seen.add(cur);
    for (const dep of graph.get(cur) || []) {
      if (!skipEdge(cur, dep)) stack.push(dep);
    }
  }
  return seen;
}

function buildAttribution(allFiles) {
  const graph = buildGraph(allFiles);
  const appAbs = join(srcRoot, "App.tsx");
  const entries = navEntries();
  const routes = routeElements(appAbs, graph);
  const pageModules = new Set(routes.map((r) => r.module));

  // The shell: everything main.tsx reaches without following App.tsx's edge
  // to a route element. A file the shell reaches renders on every page.
  const shell = reach(graph, [join(srcRoot, "main.tsx"), appAbs], (from, to) => from === appAbs && pageModules.has(to));

  const pages = {};
  const owners = new Map();
  for (const r of routes) {
    const page = pageForPath(r.path, entries);
    if (!page) continue;
    pages[page] ??= { modules: new Set(), routes: new Set() };
    pages[page].routes.add(r.path);
    pages[page].modules.add(rel(r.module));
    for (const f of reach(graph, [r.module], () => false)) {
      if (!owners.has(f)) owners.set(f, new Set());
      owners.get(f).add(page);
    }
  }
  for (const f of shell) owners.set(f, new Set(["shared"]));

  const files = {};
  for (const abs of graph.keys()) {
    const o = owners.get(abs);
    files[rel(abs)] = o ? [...o].sort() : [];
  }
  const pagesOut = {};
  for (const k of Object.keys(pages).sort()) {
    pagesOut[k] = { modules: [...pages[k].modules].sort(), routes: [...pages[k].routes].sort() };
  }
  return { files: sortObject(files), pages: pagesOut };
}

// Changing any of these restyles or rebuilds every page.
const globalUiFiles = [
  /^ui\/src\/(styles|app)\.css$/,
  /^ui\/src\/theme\//,
  /^ui\/src\/(main|App|navigation)\.tsx$/,
  /^ui\/(package(-lock)?\.json|index\.html|vite\.config\.[jt]s|tsconfig[^/]*\.json|postcss\.config\.[cm]?js)$/,
  /^ui\/public\//,
];

function affectedPages(changed, attribution) {
  const out = new Set();
  for (const raw of changed) {
    const abs = resolve(process.cwd(), raw);
    const r = existsSync(abs) ? rel(abs) : raw.replace(/^\.\//, "");
    if (!r.startsWith("ui/")) continue;
    if (globalUiFiles.some((re) => re.test(r))) return ["*"];
    if (!r.startsWith("ui/src/") || isTest(r)) continue; // tests, docs, tooling
    const owners = attribution.files[r];
    if (!owners) {
      // Not in the graph: a deleted file (its importers changed too), or a
      // file type the graph does not follow (an asset): be conservative.
      if (existsSync(join(repoRoot, r)) && !/\.(md|txt)$/.test(r)) return ["*"];
      continue;
    }
    if (owners.includes("shared")) return ["*"];
    for (const o of owners) out.add(o);
  }
  return [...out].sort();
}

// --- snapshot check --------------------------------------------------------

function checkSnapshot(current, snapshot) {
  const problems = [];
  const before = snapshot.files || {};
  for (const [file, now] of Object.entries(current.files)) {
    const was = before[file];
    if (!was) {
      const what = [];
      if (now.imports?.length || now.types?.length) what.push("antd");
      if (now.icons) what.push("@ant-design/icons");
      if (now.antRules) what.push(`${now.antRules} .ant-* rule(s)`);
      problems.push(`${file}: new antd usage (${what.join(", ")})`);
      continue;
    }
    if ((now.sites || 0) > (was.sites || 0)) problems.push(`${file}: antd usage sites ${was.sites || 0} -> ${now.sites}`);
    const known = new Set([...(was.imports || []), ...(was.types || [])]);
    const added = [...(now.imports || []), ...(now.types || [])].filter((n) => !known.has(n));
    if (added.length) problems.push(`${file}: newly imports ${added.join(", ")} from antd`);
    if ((now.icons || 0) > (was.icons || 0)) problems.push(`${file}: @ant-design/icons imports ${was.icons || 0} -> ${now.icons}`);
    if ((now.antRules || 0) > (was.antRules || 0)) problems.push(`${file}: .ant-* rules ${was.antRules || 0} -> ${now.antRules}`);
  }
  return problems;
}

// --- output ----------------------------------------------------------------

function snapshotOf(inv) {
  return { components: inv.components, files: inv.files, summary: inv.summary };
}

const stable = (o) => `${JSON.stringify(o, null, 2)}\n`;

// The snapshot keeps one line per file, so a migration MR's diff shows
// exactly the files it touched.
function snapshotText(snap) {
  const block = (obj) =>
    Object.entries(obj)
      .map(([k, v]) => `    ${JSON.stringify(k)}: ${JSON.stringify(v)}`)
      .join(",\n");
  return `{\n  "components": {\n${block(snap.components)}\n  },\n  "files": {\n${block(snap.files)}\n  },\n  "summary": ${JSON.stringify(snap.summary)}\n}\n`;
}

function printSummary(inv, attribution) {
  const s = inv.summary;
  console.log("antd inventory (ui/src; Zeron-managed code, tests and i18n excluded)");
  console.log(`  files importing antd:              ${s.antdFiles}`);
  console.log(`  antd usage sites (JSX + calls):    ${s.sites}`);
  console.log(`  files importing @ant-design/icons: ${s.iconFiles} (${s.iconImports} icons)`);
  console.log(`  CSS .ant-* rules:                  ${s.antRules} in ${s.cssFilesWithAntRules} file(s)`);
  console.log("\ntop components by sites:");
  const comps = Object.entries(inv.components).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  for (const [k, v] of comps.slice(0, 15)) console.log(`  ${String(v).padStart(5)}  ${k}`);
  console.log("\nantd sites per page (a file counts for every page that reaches it):");
  const perPage = {};
  for (const [file, e] of Object.entries(inv.files)) {
    const owners = attribution.files[file] || [];
    const keys = owners.length ? owners : ["(unreached)"];
    for (const o of keys) {
      perPage[o] ??= { files: 0, sites: 0 };
      perPage[o].files++;
      perPage[o].sites += e.sites || 0;
    }
  }
  for (const [k, v] of Object.entries(perPage).sort((a, b) => b[1].sites - a[1].sites || a[0].localeCompare(b[0]))) {
    console.log(`  ${String(v.sites).padStart(5)} sites  ${String(v.files).padStart(3)} files  ${k}`);
  }
  console.log("\ntop files by sites:");
  const files = Object.entries(inv.files).sort((a, b) => (b[1].sites || 0) - (a[1].sites || 0) || a[0].localeCompare(b[0]));
  for (const [k, v] of files.slice(0, 15)) console.log(`  ${String(v.sites || 0).padStart(5)}  ${k}`);
}

function gitChanged(base) {
  const run = (args) => execFileSync("git", args, { cwd: repoRoot, encoding: "utf8" }).split("\n").filter(Boolean);
  return [...run(["diff", "--name-only", base, "--", "ui"]), ...run(["ls-files", "--others", "--exclude-standard", "--", "ui"])];
}

function main(argv) {
  const all = walk(srcRoot);
  const flag = (name) => argv.indexOf(name);
  const optionalPath = (i) => (argv[i + 1] && !argv[i + 1].startsWith("--") ? argv[i + 1] : defaultSnapshot);

  if (flag("--affected") >= 0 || flag("--base") >= 0) {
    const attribution = buildAttribution(all);
    let changed = [];
    if (flag("--base") >= 0) {
      const ref = argv[flag("--base") + 1];
      if (!ref) throw new Error("--base needs a git ref");
      changed = gitChanged(ref);
    }
    if (flag("--affected") >= 0) changed.push(...argv.slice(flag("--affected") + 1).filter((a) => !a.startsWith("--")));
    console.log(JSON.stringify(affectedPages(changed, attribution)));
    return 0;
  }

  const inv = buildInventory(all);
  if (flag("--write") >= 0) {
    const out = resolve(repoRoot, optionalPath(flag("--write")));
    writeFileSync(out, snapshotText(snapshotOf(inv)));
    console.log(`wrote ${rel(out)} (${inv.summary.antdFiles} antd files, ${inv.summary.sites} sites)`);
    return 0;
  }
  if (flag("--check") >= 0) {
    const path = resolve(repoRoot, optionalPath(flag("--check")));
    if (!existsSync(path)) {
      console.error(`missing ${rel(path)}; create it with: node scripts/ui_antd_inventory.mjs --write`);
      return 1;
    }
    const problems = checkSnapshot(inv, JSON.parse(readFileSync(path, "utf8")));
    if (problems.length) {
      console.error(`antd usage grew against ${rel(path)}:`);
      for (const p of problems) console.error(`  ${p}`);
      console.error("Migrate to Zeron / ui-kit instead (icons: ui/src/ui-kit/consoleIcons.ts).");
      console.error("If the growth is intended, record it: node scripts/ui_antd_inventory.mjs --write");
      return 1;
    }
    const stale = snapshotText(snapshotOf(inv)) !== readFileSync(path, "utf8");
    console.log(`antd inventory: no file grew its antd usage (${inv.summary.antdFiles} files, ${inv.summary.sites} sites)`);
    if (stale) console.log(`  NOTE: usage shrank; refresh the snapshot with: node scripts/ui_antd_inventory.mjs --write`);
    return 0;
  }
  const attribution = buildAttribution(all);
  if (flag("--json") >= 0) {
    process.stdout.write(stable({ ...snapshotOf(inv), attribution }));
    return 0;
  }
  printSummary(inv, attribution);
  return 0;
}

process.exitCode = main(process.argv.slice(2));
