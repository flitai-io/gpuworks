import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["images"]);
const imagesLocale = locale.images;

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const ImageBuiltInConfig = (await import("../src/pages/images/ImageBuiltInConfig.tsx")).default;

afterEach(() => cleanup());

const version = {
  id: 9, series_id: 1, name: "runner", image_ref: "example/runner:v1", source: "community",
  enabled: true, reference_count: 0, framework: "cuda", nodes: [{ node_uuid: "n1", state: "present", updated_at: "2026-10-08T00:00:00Z" }],
} as never;

function renderDrawer(inspection: unknown) {
  render(React.createElement(I18nextProvider, { i18n }, React.createElement(ImageBuiltInConfig, {
    version,
    inspection,
    nodeName: (uuid: string) => (uuid === "n1" ? "gpu-node-1" : uuid),
  } as never)));
}

// The catalog carries no launch configuration (ADR 0031): before a node is
// inspected the card only says how to read the image's own configuration, and
// nothing about it is editable.
test("an uninspected version explains how to read the image's own configuration", async () => {
  renderDrawer(null);
  assert.ok(screen.getByText(imagesLocale.detail.builtInTitle));
  assert.ok(screen.getByText(imagesLocale.detail.builtInHint));
  // That deployments inherit none of it sits behind the help mark.
  assert.equal(screen.queryByText(imagesLocale.detail.builtInHelp), null);
  await userEvent.setup().hover(screen.getByRole("button", { name: locale.common.helpTip.label }));
  assert.equal((await screen.findByRole("tooltip")).textContent, imagesLocale.detail.builtInHelp);
  assert.ok(screen.queryByText(/^ENTRYPOINT:/) === null, "nothing is shown before an inspection");
  assert.equal(screen.queryAllByRole("textbox").length, 0, "no launch default can be edited");
});

test("an inspection is shown as read, naming the node it came from", () => {
  renderDrawer({
    nodeUUID: "n1",
    result: { image_ref: "example/runner:v1", entrypoint: ["/opt/entry.sh"], cmd: [], env: { PATH: "/usr/bin", LANG: "C" }, volumes: ["/data"] },
  });
  assert.ok(screen.getByText(imagesLocale.detail.builtInFrom.replace("{{node}}", "gpu-node-1")));
  assert.ok(screen.getByText('["/opt/entry.sh"]'));
  assert.ok(screen.getByText(imagesLocale.detail.builtInNone), "an empty CMD reads as none rather than disappearing");
  assert.ok(screen.getByText("PATH=/usr/bin"));
  assert.ok(screen.getByText("LANG=C"));
  assert.ok(screen.getByText("/data"));
  assert.ok(screen.queryByText(imagesLocale.detail.builtInHint) === null, "the hint gives way to the inspection");
});
