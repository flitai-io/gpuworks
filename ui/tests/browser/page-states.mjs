export async function settle(page) {
  await page.waitForLoadState("networkidle", { timeout: 8000 }).catch(() => {});
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

