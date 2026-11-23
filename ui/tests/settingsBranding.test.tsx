import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["settings"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const branding = await import("../src/branding.tsx");
const { FULL_CROP, isFullCrop, LogoError, LOGO_MAX_EDGE, prepareLogo, squareCrop } = await import("../src/brandingLogo.ts");
const { default: BrandingCard } = await import("../src/pages/settings/BrandingCard.tsx");
const settingsPage = await import("../src/pages/SettingsPage.tsx");
const b = locale.settings.branding as Record<string, any>;

type Branding = import("../src/api.ts").Branding;
const EMPTY: Branding = { product_name: "", console_title: "", login_description: "", logo: "" };
const original = { branding: apiModule.api.branding, updateBranding: apiModule.api.updateBranding, me: apiModule.api.me };

function stub({ role = "admin", load = async () => EMPTY }: { role?: string; load?: () => Promise<Branding> } = {}) {
  const updates: Partial<Branding>[] = [];
  let saveError: Error | null = null;
  apiModule.api.me = (async () => ({ id: 1, username: role, role })) as typeof apiModule.api.me;
  apiModule.api.branding = load;
  apiModule.api.updateBranding = async (body) => {
    updates.push(body);
    if (saveError) throw saveError;
    return { ...EMPTY, ...body, product_name: (body.product_name ?? "").trim() };
  };
  return { updates, failSaveWith: (error: Error) => { saveError = error; } };
}

// ShellBrand reads the shared branding the way the sidebar does.
function ShellBrand() {
  const { productName, consoleTitle } = branding.useBranding();
  return <output aria-label="shell">{productName}|{consoleTitle}</output>;
}

function renderCard() {
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><BrandingCard /><ShellBrand /></I18nextProvider>);
  return user;
}
const saveButton = () => screen.getByRole("button", { name: locale.settings.common.save }) as HTMLButtonElement;
const productInput = () => screen.getByPlaceholderText("GPUWorks") as HTMLInputElement;

afterEach(() => {
  cleanup();
  Object.assign(apiModule.api, original);
  branding.resetBrandingForTest();
});
after(() => closeChannels());

test("an admin sets a product name and header title; the shell follows the save", async () => {
  const api = stub();
  const user = renderCard();
  await waitFor(() => assert.equal(saveButton().disabled, false));
  // A fresh deployment has no top bar title: the logo stands alone.
  assert.equal(screen.getByRole("status", { name: "shell" }).textContent, "GPUWorks|");
  await user.type(productInput(), "Acme AI");
  const title = screen.getByPlaceholderText(b.consoleTitlePlaceholder);
  await user.type(title, "Acme 算力平台");
  await user.type(screen.getByPlaceholderText(locale.common.branding.defaultLoginDescription), "first{Enter}second");
  assert.equal(api.updates.length, 0, "typing alone does not save");
  await user.click(saveButton());
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.deepEqual(api.updates, [{ product_name: "Acme AI", console_title: "Acme 算力平台", login_description: "first\nsecond", logo: "" }]));
  assert.ok(await screen.findByText(b.saved));
  assert.equal(screen.getByRole("status", { name: "shell" }).textContent, "Acme AI|Acme 算力平台");
});

test("a stored logo shows in the preview and restoring the default clears it", async () => {
  const logo = "data:image/png;base64,AA==";
  const api = stub({ load: async () => ({ ...EMPTY, product_name: "Acme", logo }) });
  const { container } = render(<I18nextProvider i18n={i18n}><BrandingCard /></I18nextProvider>);
  const user = userEvent.setup();
  await waitFor(() => assert.equal(container.querySelector(".branding-logo-preview img")?.getAttribute("src"), logo));
  assert.equal(productInput().value, "Acme");
  await waitFor(() => assert.equal(saveButton().disabled, false));
  await user.click(screen.getByRole("button", { name: b.restoreDefault }));
  const popup = await screen.findByRole("dialog");
  assert.ok(within(popup).getByText(b.confirmRestoreLogo));
  assert.equal(container.querySelector(".branding-logo-preview img")?.getAttribute("src"), logo);
  await user.click(within(popup).getByRole("button", { name: "Confirm" }));
  assert.equal(container.querySelector(".branding-logo-preview img")?.getAttribute("src"), branding.DEFAULT_LOGO);
  assert.equal(screen.queryByRole("button", { name: b.restoreDefault }), null);
  await user.click(saveButton());
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(api.updates[0]?.logo, ""));
  assert.equal(productInput().value, "Acme");
});

test("restoring all branding is a draft until the administrator confirms the save", async () => {
  const stored = { product_name: "Acme", console_title: "Console", login_description: "Welcome", logo: "data:image/png;base64,AA==" };
  const api = stub({ load: async () => stored });
  const user = renderCard();
  const restore = screen.getByRole("button", { name: b.restoreAll }) as HTMLButtonElement;
  await waitFor(() => assert.equal(restore.disabled, false));
  await user.click(restore);
  const popup = await screen.findByRole("dialog");
  assert.ok(within(popup).getByText(b.confirmRestoreAll));
  await user.click(within(popup).getByRole("button", { name: "Cancel" }));
  assert.equal(productInput().value, "Acme");
  assert.deepEqual(api.updates, []);
  await user.click(restore);
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Confirm" }));
  assert.equal(productInput().value, "");
  assert.equal((screen.getByPlaceholderText(b.consoleTitlePlaceholder) as HTMLInputElement).value, "");
  assert.equal((screen.getByPlaceholderText(locale.common.branding.defaultLoginDescription) as HTMLTextAreaElement).value, "");
  assert.equal(screen.queryByRole("button", { name: b.restoreDefault }), null);
  assert.equal(restore.disabled, true);
  await user.click(saveButton());
  await user.click(await screen.findByRole("button", { name: "Cancel", exact: true }));
  assert.deepEqual(api.updates, []);
  assert.equal(screen.getByRole("status", { name: "shell" }).textContent, "Acme|Console");
  await user.click(saveButton());
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.deepEqual(api.updates, [EMPTY]));
  await waitFor(() => assert.equal(screen.getByRole("status", { name: "shell" }).textContent, "GPUWorks|"));
});

test("a non-admin sees the branding read-only", async () => {
  const api = stub({ role: "member", load: async () => ({ ...EMPTY, product_name: "Acme" }) });
  renderCard();
  assert.ok(await screen.findByText(b.adminOnly));
  await waitFor(() => assert.equal(productInput().value, "Acme"));
  assert.equal(saveButton().disabled, true);
  assert.equal(productInput().disabled, true);
  assert.equal((screen.getByRole("button", { name: b.restoreAll }) as HTMLButtonElement).disabled, true);
  assert.equal(api.updates.length, 0);
});

test("a load failure offers a retry and a refused save keeps the draft", async () => {
  let fail = true;
  const api = stub({ load: async () => { if (fail) throw new Error("offline"); return EMPTY; } });
  const user = renderCard();
  const alert = (await screen.findByText("offline")).closest(".ant-alert") as HTMLElement;
  assert.ok(alert, "the failure shows as an alert");
  assert.equal(saveButton().disabled, true, "nothing to save before the branding is known");
  fail = false;
  await user.click(within(alert).getByRole("button", { name: locale.common.actions.retry }));
  await waitFor(() => assert.equal(saveButton().disabled, false));
  api.failSaveWith(new APIError("FIELD_INVALID", { field: "logo", detail: "logo: image is larger than 128 KiB" }, "logo: image is larger than 128 KiB", 400));
  await user.type(productInput(), "Acme");
  await user.click(saveButton());
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  assert.ok(await screen.findByText(/image is larger than 128 KiB/));
  assert.equal(productInput().value, "Acme");
  assert.equal(screen.getByRole("status", { name: "shell" }).textContent?.startsWith("GPUWorks|"), true, "a refused save does not change the shell");
});

test("choosing an image opens the crop dialog; a non-image is refused", async () => {
  Object.assign(URL, { createObjectURL: () => "blob:test/logo", revokeObjectURL: () => {} });
  stub();
  const user = userEvent.setup({ applyAccept: false });
  const { container } = render(<I18nextProvider i18n={i18n}><BrandingCard /></I18nextProvider>);
  await waitFor(() => assert.equal(saveButton().disabled, false));
  // The upload control replaces its input after every pick; query it afresh.
  const input = () => container.querySelector("input[type=file]") as HTMLInputElement;
  await user.upload(input(), new File(["<html>"], "page.html", { type: "text/html" }));
  assert.ok(await screen.findByText(b.logoError.notImage));
  assert.equal(screen.queryByText(b.crop.title), null);
  await user.upload(input(), new File([new Uint8Array(4)], "logo.png", { type: "image/png" }));
  assert.ok(await screen.findByText(b.crop.title));
  assert.equal(screen.getByAltText(b.crop.image).getAttribute("src"), "blob:test/logo");
  await user.click(screen.getByRole("button", { name: locale.settings.common.cancel }));
  assert.equal(container.querySelector(".branding-logo-preview img")?.getAttribute("src"), branding.DEFAULT_LOGO, "cancelling keeps the current logo");
});

// prepareLogo, with a codec that records what it was asked to produce.
function codec(sizes: Record<string, number>, failRaster = false) {
  const calls: string[] = [];
  const crops: import("../src/brandingLogo.ts").LogoCrop[] = [];
  return {
    calls,
    crops,
    codec: {
      dataURL: async (blob: Blob) => `data:${blob.type};base64,${blob.size}`,
      rasterize: async (_file: Blob, crop: import("../src/brandingLogo.ts").LogoCrop, edge: number, type: string, quality?: number) => {
        const key = `${edge} ${type}${quality ? ` ${quality}` : ""}`;
        calls.push(key);
        crops.push(crop);
        if (failRaster) throw new Error("cannot decode");
        return new Blob([new Uint8Array(sizes[key] ?? sizes.default ?? 10)], { type });
      },
    },
  };
}
const file = (type: string, size: number) => new File([new Uint8Array(size)], "logo", { type });
const LIMIT = branding.BRANDING_LOGO_MAX_BYTES;

test("a small SVG logo is kept as is, without rasterizing", async () => {
  const c = codec({});
  assert.equal(await prepareLogo(file("image/svg+xml", 500), FULL_CROP, c.codec), "data:image/svg+xml;base64,500");
  assert.deepEqual(c.calls, []);
});

test("a raster logo is scaled to the edge limit as PNG when that fits", async () => {
  const c = codec({ default: 2000 });
  assert.equal(await prepareLogo(file("image/jpeg", 5 << 20), FULL_CROP, c.codec), "data:image/png;base64,2000");
  assert.deepEqual(c.calls, [`${LOGO_MAX_EDGE} image/png`]);
});

test("a logo too large as PNG falls back to WebP, then to a smaller edge", async () => {
  const c = codec({ default: LIMIT + 1, "128 image/webp 0.9": 900 });
  assert.equal(await prepareLogo(file("image/png", 1 << 20), FULL_CROP, c.codec), "data:image/webp;base64,900");
  assert.deepEqual(c.calls, ["256 image/png", "256 image/webp 0.9", "256 image/webp 0.75", "128 image/png", "128 image/webp 0.9"]);
  // A large SVG is rasterized like any other image.
  const svg = codec({ default: 700 });
  assert.equal(await prepareLogo(file("image/svg+xml", LIMIT + 1), FULL_CROP, svg.codec), "data:image/png;base64,700");
});

test("a crop reaches the rasterizer, and a cropped SVG is rasterized", async () => {
  const crop = { x: 0.25, y: 0, width: 0.5, height: 1 };
  const c = codec({ default: 500 });
  assert.equal(await prepareLogo(file("image/svg+xml", 500), crop, c.codec), "data:image/png;base64,500");
  assert.deepEqual(c.crops, [crop]);
  const full = codec({});
  await prepareLogo(file("image/svg+xml", 500), { x: 0.0001, y: 0, width: 0.9999, height: 1 }, full.codec);
  assert.deepEqual(full.calls, [], "a selection a rounding error short of the whole image counts as uncropped");
});

test("squareCrop centers the largest square; isFullCrop tolerates rounding", () => {
  assert.deepEqual(squareCrop(400, 100), { x: 0.375, y: 0, width: 0.25, height: 1 });
  assert.deepEqual(squareCrop(100, 200), { x: 0, y: 0.25, width: 1, height: 0.5 });
  assert.deepEqual(squareCrop(50, 50), { x: 0, y: 0, width: 1, height: 1 });
  assert.deepEqual(squareCrop(0, 10), FULL_CROP);
  assert.equal(isFullCrop(FULL_CROP), true);
  assert.equal(isFullCrop({ x: 0, y: 0, width: 0.9, height: 1 }), false);
});

test("prepareLogo rejects non-images, undecodable images and images that never fit", async () => {
  await assert.rejects(prepareLogo(file("text/html", 10), FULL_CROP, codec({}).codec), (e) => e instanceof LogoError && e.reason === "notImage");
  await assert.rejects(prepareLogo(file("image/png", 10), FULL_CROP, codec({}, true).codec), (e) => e instanceof LogoError && e.reason === "unreadable");
  const huge = codec({ default: LIMIT + 1 });
  await assert.rejects(prepareLogo(file("image/png", 10), FULL_CROP, huge.codec), (e) => e instanceof LogoError && e.reason === "tooLarge");
  assert.equal(huge.calls.length, 9, "every edge and encoding was tried");
});

test("a fragment selects the settings tab: its own key or a card it holds", () => {
  const { settingsTabFor } = settingsPage;
  assert.equal(settingsTabFor(""), "general");
  assert.equal(settingsTabFor("#nonsense"), "general");
  assert.equal(settingsTabFor("#branding"), "general");
  assert.equal(settingsTabFor("#updates"), "general");
  assert.equal(settingsTabFor("#pull-settings"), "pull");
  assert.equal(settingsTabFor("#nodes"), "nodes");
  assert.equal(settingsTabFor("#security"), "access");
  assert.equal(settingsTabFor("#outbound-policy"), "access");
  assert.equal(settingsTabFor("#telemetry"), "integrations");
});

test("the settings page opens on General and switches tabs by click and keyboard", async () => {
  stub();
  const restoreSettings = apiModule.api.settings;
  apiModule.api.settings = async () => ({});
  try {
    const user = userEvent.setup();
    render(
      <I18nextProvider i18n={i18n}>
        <MemoryRouter initialEntries={["/settings"]}>
          <Routes><Route path="*" element={<settingsPage.default />} /></Routes>
        </MemoryRouter>
      </I18nextProvider>,
    );
    const tabs = locale.settings.page.tabs as Record<string, string>;
    // Role queries with a name are slow on the card-heavy page; the tab list
    // is small, so find tabs by their text (a focused tab gains a hidden
    // "Tab n of m" prefix for screen readers).
    const tab = (label: string) => screen.getAllByRole("tab").find((element) => element.textContent?.endsWith(label))!;
    assert.deepEqual(screen.getAllByRole("tab").map((element) => element.textContent), [tabs.general, tabs.pull, tabs.nodes, tabs.access, tabs.integrations, tabs.data, tabs.diagnostics]);
    assert.equal(tab(tabs.general).getAttribute("aria-selected"), "true");
    assert.ok(await screen.findByText(b.title));
    await user.click(tab(tabs.nodes));
    assert.equal(tab(tabs.nodes).getAttribute("aria-selected"), "true");
    assert.ok(await screen.findByText(locale.settings.orphanCleanup.title));
    tab(tabs.integrations).focus();
    await user.keyboard("{Enter}");
    await waitFor(() => assert.equal(tab(tabs.integrations).getAttribute("aria-selected"), "true"));
    assert.ok(await screen.findByText(locale.settings.ai.service));
  } finally {
    apiModule.api.settings = restoreSettings;
  }
});
