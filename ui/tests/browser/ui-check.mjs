#!/usr/bin/env node
// ui-check walks the console in a real browser and fails on layout faults a
// person would see: text covered by another element, two texts drawn on top
// of each other, a control stretched far past its content, a page or dialog
// that scrolls sideways, and console errors. It discovers the states to look
// at itself, so a new page, drawer, tab or header dialog is covered without
// registering it: every menu page (or the pages given), the first row's
// detail drawer and each of its tabs, and each dialog the page header opens.
// With --compare it also photographs the same states on a second build (the
// target branch) and reports the ones that look different, for a person to
// confirm; that part never fails the run.
//
//   node tests/browser/ui-check.mjs --base http://127.0.0.1:5210 \
//     --password-file /path/pw [--compare http://127.0.0.1:5216] \
//     [--routes /nodes,/gpus | --affected routes.json] [--widths 1440,1024] \
//     [--locales zh] [--out ui-check-report]
//
// The server must be a disposable one (`gpuworks experience`): the checker
// opens create dialogs and closes them, but never submits. Exit 1 on a hard
// finding; the report is <out>/index.html.
import { mkdirSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { join, resolve } from "node:path";
import { chromium } from "playwright-core";

const args = Object.fromEntries(process.argv.slice(2).reduce((pairs, arg, index, all) => {
  if (arg.startsWith("--")) pairs.push([arg.slice(2), all[index + 1]?.startsWith("--") || all[index + 1] == null ? "1" : all[index + 1]]);
  return pairs;
}, []));
const base = (args.base ?? "http://127.0.0.1:5210").replace(/\/$/, "");
const compare = args.compare?.replace(/\/$/, "");
const out = resolve(args.out ?? "ui-check-report");
const widths = (args.widths ?? "1440,1024").split(",").map(Number);
const locales = (args.locales ?? "zh").split(",");
const user = args.user ?? "admin";
const password = args["password-file"] ? readFileSync(args["password-file"], "utf8").trim() : process.env.GPUWORKS_UI_CHECK_PASSWORD;
if (!password) throw new Error("give --password-file or GPUWORKS_UI_CHECK_PASSWORD");

// Pages from the menu (ui/src/navigation.tsx keys) plus the ones reached from them.
const menuRoutes = [...readFileSync(new URL("../../src/navigation.tsx", import.meta.url), "utf8").matchAll(/key:\s*"(\/[^"]*)"/g)].map((m) => m[1]);
const extraRoutes = ["/deploy"];
let routes = args.routes ? args.routes.split(",") : [...menuRoutes, ...extraRoutes];
if (args.affected) {
  const affected = JSON.parse(readFileSync(args.affected, "utf8"));
  if (!affected.includes("*")) routes = routes.filter((route) => affected.some((page) => route === page || route.startsWith(`${page}/`)));
}

// Errors the environment produces on its own (no update source, no listener
// management in experience mode), not the UI.
const ignoredConsole = [/Failed to load resource: the server responded with a status of (401|403|404)/, /\[antd: .*\] .* is deprecated/i, /Static function can not consume context/];

const ruleScript = () => {
  const visible = (el) => { const r = el.getBoundingClientRect(); const s = getComputedStyle(el); return r.width > 0 && r.height > 0 && s.visibility !== "hidden" && Number(s.opacity) > 0.05; };
  const dialogs = [...document.querySelectorAll("[role=dialog], .ant-drawer-open .ant-drawer-content, .ant-modal-wrap:not([style*='display: none']) .ant-modal-content")].filter(visible);
  const root = dialogs.at(-1) ?? document.querySelector(".app-shell") ?? document.body;
  const transient = (el) => el.closest("[role=tooltip], [data-slot=popover-content], .ant-tooltip, .ant-popover, .ant-message, [data-slot=toast]");
  const name = (el) => {
    const text = (el.innerText || el.getAttribute("aria-label") || "").trim().replace(/\s+/g, " ").slice(0, 40);
    return `${el.tagName.toLowerCase()}${el.id ? `#${el.id}` : ""}${typeof el.className === "string" && el.className ? "." + el.className.trim().split(/\s+/).slice(0, 2).join(".") : ""} "${text}"`;
  };
  const findings = [];
  const add = (rule, el, detail) => { if (findings.length < 60) findings.push({ rule, element: name(el), detail, box: (({ x, y, width, height }) => ({ x: Math.round(x), y: Math.round(y), w: Math.round(width), h: Math.round(height) }))(el.getBoundingClientRect()) }); };

  // The part of a rect that is actually shown: cut by every overflow box it sits in,
  // its own included (text-overflow: ellipsis clips the text node's full width).
  const shown = (el, r) => {
    let { left, top, right, bottom } = r;
    for (let a = el; a && a !== document.documentElement; a = a.parentElement) {
      const s = getComputedStyle(a);
      if (s.clip !== "auto" && s.position === "absolute") return null;
      if (s.overflowX === "visible" && s.overflowY === "visible") continue;
      const b = a.getBoundingClientRect();
      left = Math.max(left, b.left); top = Math.max(top, b.top); right = Math.min(right, b.right); bottom = Math.min(bottom, b.bottom);
      if (right - left < 2 || bottom - top < 2) return null;
    }
    return { left, top, right, bottom, width: right - left, height: bottom - top };
  };
  const clipped = (el, x, y) => { const r = shown(el, { left: x, top: y, right: x + 3, bottom: y + 3 }); return !r; };
  // Text pieces: one rect per text node line.
  const pieces = [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (!node.textContent.trim()) continue;
    const el = node.parentElement;
    if (!el || !visible(el) || el.closest("[aria-hidden=true], script, style, svg, canvas, .sr-only") || transient(el)) continue;
    const range = document.createRange(); range.selectNodeContents(node);
    for (const raw of range.getClientRects()) {
      if (raw.width <= 2 || raw.height <= 4) continue;
      const r = shown(el, raw);
      if (r && r.bottom > 0 && r.top < innerHeight && r.right > 0 && r.left < innerWidth) pieces.push({ el, r });
    }
  }
  // A pinned (sticky) table column is meant to slide over the cells beside it. Other sticky
  // bars (tabs, headers) must not cover anything before the user scrolls.
  const sticky = (el) => { for (let a = el; a && a !== document.body; a = a.parentElement) if (getComputedStyle(a).position === "sticky" && a.closest("table")) return true; return false; };
  // Does the element draw anything at this point: a fill, an image or a control.
  const paints = (el) => {
    const s = getComputedStyle(el);
    if (el.closest("svg")) return false; // a chart's own shapes around its labels
    return (s.backgroundColor !== "rgba(0, 0, 0, 0)" && s.backgroundColor !== "transparent") || s.backgroundImage !== "none" || /^(IMG|CANVAS|VIDEO)$/.test(el.tagName);
  };
  const formOverlay = (el) => /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName) || el.closest(".ant-select-selector, .ant-picker");
  // 1. Covered text: what is drawn at the text's own points must be the text, not a filled element above it.
  const coveredSeen = new Set();
  for (const { el, r } of pieces) {
    if (coveredSeen.has(el)) continue;
    for (const [x, y] of [[r.left + r.width / 2, r.top + r.height / 2], [r.left + r.width / 2, r.top + Math.min(3, r.height / 3)]]) {
      const hit = document.elementFromPoint(x, y);
      if (!hit || hit === el || el.contains(hit) || hit.contains(el) || transient(hit) || formOverlay(hit) || formOverlay(el)) continue;
      // The covering element counts if it, or anything it sits in that does not also hold the text, draws a fill.
      let painter = null;
      for (let c = hit; c && !c.contains(el); c = c.parentElement) if (paints(c)) { painter = c; break; }
      if (clipped(el, x, y) || sticky(hit) || !painter) continue;
      add("covered-text", el, `covered by ${name(hit)}`); coveredSeen.add(el); break;
    }
  }
  // 1b. A control an invisible layer sits on: it looks fine but does not take the click.
  for (const el of root.querySelectorAll("button, a[href], [role=button], [role=tab], [role=switch], input:not([type=hidden])")) {
    if (!visible(el) || transient(el)) continue;
    const r = el.getBoundingClientRect();
    const [x, y] = [r.left + r.width / 2, r.top + r.height / 2];
    if (x < 0 || y < 0 || x > innerWidth || y > innerHeight || clipped(el, x, y)) continue;
    const hit = document.elementFromPoint(x, y);
    if (!hit || hit === el || el.contains(hit) || hit.contains(el) || transient(hit) || sticky(hit) || formOverlay(hit)) continue;
    if (getComputedStyle(hit).pointerEvents === "none") continue;
    add("blocked-control", el, `click lands on ${name(hit)}`);
  }
  // 2. Two texts drawn over each other.
  const overlapSeen = new Set();
  for (let i = 0; i < pieces.length; i++) for (let j = i + 1; j < pieces.length; j++) {
    const a = pieces[i], b = pieces[j];
    if (a.el === b.el || a.el.contains(b.el) || b.el.contains(a.el) || sticky(a.el) || sticky(b.el)) continue;
    const w = Math.min(a.r.right, b.r.right) - Math.max(a.r.left, b.r.left), h = Math.min(a.r.bottom, b.r.bottom) - Math.max(a.r.top, b.r.top);
    if (w <= 0 || h <= 0) continue;
    const area = Math.min(a.r.width * a.r.height, b.r.width * b.r.height);
    const key = [a.el, b.el].map(name).sort().join("|");
    if ((w * h) / area > 0.3 && !overlapSeen.has(key)) { overlapSeen.add(key); add("text-overlap", a.el, `over ${name(b.el)}`); }
  }
  // 3. A control stretched far past its content (a switch or a short button as wide as its row).
  for (const el of root.querySelectorAll("[role=switch], button, .ant-btn, [role=button]")) {
    if (!visible(el) || transient(el) || el.closest("[data-ui-check-allow=wide]") || /\bw-full\b|ant-btn-block/.test(el.className)) continue;
    const r = el.getBoundingClientRect();
    if (el.getAttribute("role") === "switch" || el.classList.contains("ant-switch")) { if (r.width > 120) add("stretched-control", el, `switch ${Math.round(r.width)}px wide`); continue; }
    const label = (el.innerText || "").trim();
    if (!label || label.length > 12) continue;
    const range = document.createRange(); range.selectNodeContents(el);
    const content = range.getBoundingClientRect().width;
    if (r.width > 260 && r.width > content * 4 + 48) add("stretched-control", el, `${Math.round(r.width)}px for ${Math.round(content)}px of content`);
  }
  // 3b. An empty list's placeholder off the middle of what is shown: a table wider than its
  // container centers it across columns scrolled out of view unless it holds to the visible part.
  for (const el of root.querySelectorAll("[data-slot=table-container] [data-slot=empty]")) {
    if (!visible(el)) continue;
    const box = el.closest("[data-slot=table-container]").getBoundingClientRect(), media = (el.querySelector("[data-slot=empty-media]") ?? el).getBoundingClientRect();
    const off = Math.round(media.left + media.width / 2 - (box.left + box.width / 2));
    if (Math.abs(off) > 8) add("empty-off-center", el, `${off}px from the middle of the visible table`);
  }
  // 4. Sideways scrolling.
  if (document.documentElement.scrollWidth > innerWidth + 1) add("page-overflow-x", document.documentElement, `${document.documentElement.scrollWidth}px > ${innerWidth}px`);
  for (const d of dialogs) if (d.scrollWidth > d.clientWidth + 1 && getComputedStyle(d).overflowX !== "visible") add("dialog-overflow-x", d, `${d.scrollWidth}px > ${d.clientWidth}px`);
  // Font sizes in use, reported for the page-look review (not a finding).
  const sizes = {};
  for (const { el } of pieces) { const s = getComputedStyle(el).fontSize; sizes[s] = (sizes[s] ?? 0) + 1; }
  return { findings, sizes, scope: dialogs.length ? "dialog" : "page" };
};

// Letters of any script survive, so Chinese tab and dialog names do not
// collapse onto their page's file.
const safe = (value) => value.replace(/[^\p{L}\p{N}]+/gu, "-").replace(/^-|-$/g, "").slice(0, 80) || "root";
// States come in this order on both builds; a base state of a later kind waits for its turn.
const kindOrder = { page: 0, drawer: 1, "drawer-tab": 2, dialog: 3 };

async function login(page, origin) {
  await page.goto(`${origin}/login`);
  await page.locator("input").first().waitFor();
  await page.locator("input:not([type=password])").first().fill(user);
  await page.locator("input[type=password]").fill(password);
  await page.keyboard.press("Enter");
  await page.waitForURL((url) => !url.pathname.startsWith("/login"), { timeout: 20000 });
}

async function settle(page) {
  await page.waitForLoadState("networkidle", { timeout: 8000 }).catch(() => {});
  await page.waitForTimeout(500);
}

const openDialog = (page) => page.locator("[role=dialog]:visible, .ant-drawer-open .ant-drawer-content:visible, .ant-modal-wrap:visible .ant-modal-content");
async function closeAll(page) {
  for (let i = 0; i < 3 && await openDialog(page).count(); i++) { await page.keyboard.press("Escape"); await page.waitForTimeout(400); }
}

// Every state of one route: the page, its first detail drawer and tabs, header dialogs.
async function* states(page, origin, route) {
  await page.goto(origin + route); await settle(page);
  yield { id: route, kind: "page" };
  const back = async () => { if (new URL(page.url()).pathname !== route) { await page.goto(origin + route); await settle(page); } };
  const link = page.locator("tbody tr:not(.ant-table-measure-row) td a, tbody tr:not(.ant-table-measure-row) td button.ant-btn-link").first();
  if (await link.count()) {
    await link.click().catch(() => {}); await settle(page);
    if (await openDialog(page).count()) {
      yield { id: `${route} · detail`, kind: "drawer" };
      const tabs = openDialog(page).last().locator("[role=tab]");
      for (let i = 1; i < Math.min(await tabs.count(), 8); i++) {
        // Without its count badge ("GPUs 8"), which differs between builds and data.
        const label = (await tabs.nth(i).innerText()).trim().split("\n")[0].replace(/\s*\d+$/, "");
        await tabs.nth(i).click().catch(() => {}); await settle(page);
        yield { id: `${route} · detail · ${label}`, kind: "drawer-tab" };
      }
      await closeAll(page);
    }
    await back();
  }
  const header = page.locator(".page-header [data-slot=page-actions] button, .page-header-extra button");
  for (let i = 0; i < Math.min(await header.count(), 4); i++) {
    const button = header.nth(i);
    const label = ((await button.innerText().catch(() => "")) || (await button.getAttribute("aria-label")) || "").trim();
    if (!label || await button.isDisabled() || /刷新|refresh|删除|delete|停止|stop|退出|log ?out/i.test(label)) continue;
    await button.click().catch(() => {}); await settle(page);
    if (await openDialog(page).count()) { yield { id: `${route} · ${label}`, kind: "dialog" }; await closeAll(page); }
    await back();
  }
}

async function run() {
  mkdirSync(join(out, "shots"), { recursive: true });
  const browser = await chromium.launch(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : { channel: process.env.CI ? undefined : "chrome" });
  const results = [];
  for (const locale of locales) for (const width of widths) {
    const make = async (origin) => {
      const context = await browser.newContext({ viewport: { width, height: width >= 1280 ? 900 : 768 }, locale: locale === "en" ? "en-US" : "zh-CN" });
      await context.addInitScript((lang) => { try { localStorage.setItem("i18nextLng", lang); localStorage.setItem("gpuworks.navCollapsed", "0"); } catch {} }, locale);
      const page = await context.newPage();
      const errors = [];
      page.on("console", (m) => { if (m.type() === "error" && !ignoredConsole.some((re) => re.test(m.text()))) errors.push(m.text().slice(0, 200)); });
      page.on("pageerror", (e) => errors.push(`pageerror: ${String(e).slice(0, 200)}`));
      await login(page, origin);
      return { context, page, errors };
    };
    const head = await make(base);
    const ref = compare ? await make(compare) : null;
    for (const route of routes) {
      const refStates = ref ? states(ref.page, compare, route) : null;
      let pending = null;
      for await (const state of states(head.page, base, route)) {
        head.errors.length = 0;
        await head.page.mouse.move(0, 0);
        const check = await head.page.evaluate(ruleScript);
        let file = `${locale}-${width}-${safe(state.id)}`;
        for (let n = 2; results.some((entry) => entry.file === file); n++) file = `${locale}-${width}-${safe(state.id)}-${n}`;
        await head.page.screenshot({ path: join(out, "shots", `${file}.png`) });
        const entry = { ...state, locale, width, file, findings: check.findings, sizes: check.sizes, errors: [...head.errors] };
        for (const error of entry.errors) entry.findings.push({ rule: "console-error", element: "", detail: error });
        if (refStates) {
          // Walk the base build to the same state; one missing there must not
          // use up the states after it.
          let match;
          for (let candidate = pending ?? (await refStates.next()).value; candidate; candidate = (await refStates.next()).value) {
            pending = null;
            if (candidate.id === state.id) { match = candidate; break; }
            if (kindOrder[candidate.kind] > kindOrder[state.kind]) { pending = candidate; break; }
          }
          if (match) { await ref.page.mouse.move(0, 0); await ref.page.screenshot({ path: join(out, "shots", `${file}.base.png`) }); entry.base = `${file}.base.png`; }
        }
        results.push(entry);
        process.stdout.write(`${entry.findings.length ? "✗" : "✓"} ${locale} ${width} ${state.id}${entry.findings.length ? `  ${entry.findings.map((f) => f.rule).join(", ")}` : ""}\n`);
      }
    }
    await head.context.close(); if (ref) await ref.context.close();
  }
  // Visual difference against the base build, measured in the browser.
  if (compare) {
    const page = await browser.newPage();
    for (const entry of results.filter((e) => e.base)) {
      entry.diff = await page.evaluate(async ([a, b]) => {
        const load = (src) => new Promise((ok, fail) => { const img = new Image(); img.onload = () => ok(img); img.onerror = fail; img.src = src; });
        const [x, y] = await Promise.all([load(a), load(b)]);
        const w = Math.min(x.width, y.width), h = Math.min(x.height, y.height);
        const draw = (img) => { const c = new OffscreenCanvas(w, h); const g = c.getContext("2d"); g.drawImage(img, 0, 0); return g.getImageData(0, 0, w, h).data; };
        const p = draw(x), q = draw(y); let changed = 0;
        for (let i = 0; i < p.length; i += 4) if (Math.abs(p[i] - q[i]) + Math.abs(p[i + 1] - q[i + 1]) + Math.abs(p[i + 2] - q[i + 2]) > 48) changed++;
        return changed / (w * h);
      }, [`data:image/png;base64,${readFileSync(join(out, "shots", `${entry.file}.png`)).toString("base64")}`, `data:image/png;base64,${readFileSync(join(out, "shots", entry.base)).toString("base64")}`]);
    }
    await page.close();
  }
  await browser.close();
  report(results);
  const failing = results.filter((e) => e.findings.length);
  console.log(`\n${results.length} states, ${failing.length} with findings, report: ${join(out, "index.html")}`);
  process.exit(failing.length ? 1 : 0);
}

function report(results) {
  writeFileSync(join(out, "results.json"), JSON.stringify(results, null, 1));
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;" }[c]));
  const changed = results.filter((e) => e.diff != null && e.diff > 0.01).sort((a, b) => b.diff - a.diff);
  const row = (e) => `<section><h3>${e.findings.length ? "✗" : "✓"} ${esc(e.locale)} · ${e.width} · ${esc(e.id)}${e.diff != null ? ` · 与目标分支差异 ${(e.diff * 100).toFixed(1)}%` : ""}</h3>
    ${e.findings.length ? `<ul>${e.findings.map((f) => `<li><b>${esc(f.rule)}</b> ${esc(f.element)} — ${esc(f.detail)}</li>`).join("")}</ul>` : ""}
    <div class="shots">${e.base ? `<figure><img loading="lazy" src="shots/${esc(e.base)}"><figcaption>目标分支</figcaption></figure>` : ""}<figure><img loading="lazy" src="shots/${esc(e.file)}.png"><figcaption>本分支</figcaption></figure></div></section>`;
  writeFileSync(join(out, "index.html"), `<!doctype html><meta charset="utf-8"><title>UI check</title>
<style>body{font:14px -apple-system,"PingFang SC",sans-serif;margin:24px;color:#00030a}h2{margin-top:32px}section{border-top:1px solid #e5e7eb;padding:12px 0}h3{font-size:14px;margin:0 0 6px}ul{margin:4px 0 8px;color:#a74a00}.shots{display:flex;gap:12px}figure{margin:0;flex:1}img{width:100%;border:1px solid #e5e7eb}figcaption{font-size:12px;color:#555}</style>
<h1>UI check</h1><p>${results.length} 个状态；${results.filter((e) => e.findings.length).length} 个有问题；${changed.length} 个与目标分支有明显差异（需人工确认）。</p>
<h2>有问题的状态</h2>${results.filter((e) => e.findings.length).map(row).join("") || "<p>无</p>"}
<h2>与目标分支有差异的状态</h2>${changed.map(row).join("") || "<p>无</p>"}
<h2>全部状态</h2>${results.filter((e) => !e.findings.length && !changed.includes(e)).map(row).join("")}`);
}

if (!existsSync(join(out))) mkdirSync(out, { recursive: true });
run().catch((error) => { console.error(error); process.exit(2); });
