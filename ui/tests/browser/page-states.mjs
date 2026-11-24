// Playwright's "networkidle" never arrives on a page that listens for live
// changes: the request for the SharedWorker script that carries the stream is
// reported as started and never as finished, so each such state waited out the
// whole timeout. Count the requests a page still waits for here instead, and
// leave out the ones that do not come back.
const tracked = new WeakMap();
const quietFor = 500, domQuietFor = 300, settleWithin = 8000, workerScriptAfter = 1000;

// track starts counting a page's requests. Call it before the first
// navigation; settle calls it too, for a page that was not registered.
export function track(page) {
  let net = tracked.get(page);
  if (net) return net;
  net = { open: new Map(), changed: Date.now() };
  const touch = () => { net.changed = Date.now(); };
  page.on("request", (request) => {
    if (["eventsource", "websocket"].includes(request.resourceType())) return;
    net.open.set(request, { at: Date.now(), answered: false }); touch();
  });
  page.on("response", (response) => {
    const entry = net.open.get(response.request());
    if (!entry) return;
    // A stream read through fetch stays open for as long as the page does.
    if ((response.headers()["content-type"] ?? "").includes("text/event-stream")) net.open.delete(response.request());
    else entry.answered = true;
    touch();
  });
  const end = (request) => { if (net.open.delete(request)) touch(); };
  page.on("requestfinished", end); page.on("requestfailed", end);
  tracked.set(page, net);
  return net;
}

// A script that has had no answer for a second is a worker's: the document's
// own scripts are answered at once by the local server the check runs against.
const pendingRequests = (net, now) => [...net.open].filter(([request, entry]) => !(request.resourceType() === "script" && !entry.answered && now - entry.at >= workerScriptAfter)).length;

// domQuiet resolves in the page once nothing in the document has changed for
// quietMs, or after capMs. A response is followed by a render, and a render can
// mount a part that asks for more, so the network alone does not say the page
// has stopped.
const domQuiet = ([quietMs, capMs]) => new Promise((resolve) => {
  let timer;
  const done = () => { observer.disconnect(); clearTimeout(cap); resolve(); };
  const observer = new MutationObserver(() => { clearTimeout(timer); timer = setTimeout(done, quietMs); });
  observer.observe(document.documentElement, { subtree: true, childList: true, attributes: true, characterData: true });
  timer = setTimeout(done, quietMs);
  const cap = setTimeout(done, capMs);
});

// settle returns once the page waits for no request, has asked for nothing
// new for quietFor (counted from the call, so that a click's own requests are
// seen) and has then stopped changing, or after settleWithin at the latest.
export async function settle(page) {
  const net = track(page);
  const start = Date.now();
  const left = () => settleWithin - (Date.now() - start);
  while (left() > 0) {
    const now = Date.now();
    if (pendingRequests(net, now) > 0 || now - Math.max(net.changed, start) < quietFor) { await page.waitForTimeout(50); continue; }
    const seen = net.changed;
    // A navigation under way replaces the document the observer runs in.
    await page.evaluate(domQuiet, [domQuietFor, Math.max(left(), 0)]).catch(() => {});
    if (net.changed === seen && pendingRequests(net, Date.now()) === 0) break;
  }
  await page.waitForTimeout(500);
}

const openDialog = (page) => page.locator("[role=dialog]:visible, .ant-drawer-open .ant-drawer-content:visible, .ant-modal-wrap:visible .ant-modal-content");
async function closeAll(page) {
  for (let i = 0; i < 3 && await openDialog(page).count(); i++) { await page.keyboard.press("Escape"); await page.waitForTimeout(400); }
}

// Visit both full-page and drawer details before returning for header actions.
export async function* states(page, origin, route) {
  await page.goto(origin + route); await settle(page);
  yield { id: route, kind: "page" };
  const back = async () => { if (new URL(page.url()).pathname !== route) { await page.goto(origin + route); await settle(page); } };
  const link = page.locator("tbody tr:not(.ant-table-measure-row) td a, tbody tr:not(.ant-table-measure-row) td button.ant-btn-link, td.resource-table-name button[data-slot=button]").first();
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
    } else if (new URL(page.url()).pathname !== new URL(origin + route).pathname) {
      yield { id: `${route} · detail`, kind: "detail-page" };
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

