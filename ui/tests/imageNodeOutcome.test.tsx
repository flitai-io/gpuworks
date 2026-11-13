import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import type { ImageCatalogEntry } from "../src/api.ts";

const { cleanup, fireEvent, render, screen, within } = await import("@testing-library/react");
const ImageNodeTable = (await import("../src/pages/images/ImageNodeTable.tsx")).default;

afterEach(cleanup);

// A present row whose forced re-fetch failed keeps the image it had: the
// table shows it as present, with no source for the failed attempt, and the
// details say the re-fetch failed and why, as the server's code and message
// describe it.
for (const language of ["en", "zh"] as const) {
  test(`a failed forced re-fetch over a held image reads as a failure with its reason (${language})`, async () => {
    const { i18n, locale } = await createI18n(["images"], language);
    const nodeText = locale.images.nodeTable as Record<string, string>;
    const version = {
      id: 9, series_id: 3, name: "runner", image_ref: "canonical.example/team/runner:v1",
      source: "custom", enabled: true, reference_count: 0, registry_sources: [],
      nodes: [
        { node_uuid: "n-refetch", node_name: "host-refetch", node_state: "online", state: "present",
          transfer_error_code: "IMAGE_REFETCH_FAILED", message: "load image archive: unexpected EOF", image_digest_id: "sha256:old" },
        { node_uuid: "n-ok", node_name: "host-ok", node_state: "online", state: "present", source: "url", message: "image archive loaded" },
      ],
    } as ImageCatalogEntry;
    render(React.createElement(I18nextProvider, { i18n }, React.createElement(ImageNodeTable, {
      selectedVersion: version, inspecting: null, availablePeer: () => false,
      modeFor: () => "direct", setMode: () => {}, pullVersion: () => {},
      probeImage: async () => {}, syncFromPeer: async () => {}, pullFromURL: async () => {},
      inspectVersion: () => {}, cancelTransfer: () => {}, removeFromNode: () => {},
      repullVersion: () => {},
    })));

    const failed = screen.getByRole("row", { name: /(^|\s)host-refetch / });
    assert.equal(within(failed).queryByText("URL", { exact: true }), null, "the failed attempt is not where the held image came from");
    fireEvent.click(within(failed).getByRole("button", { name: nodeText.expand }));
    const want = language === "en"
      ? "The re-fetch failed; this node keeps the image it already had. Reason: load image archive: unexpected EOF"
      : "重新获取失败，该节点保留原有镜像。原因：load image archive: unexpected EOF";
    screen.getByText(want, { exact: true });

    const ok = screen.getByRole("row", { name: /(^|\s)host-ok / });
    fireEvent.click(within(ok).getByRole("button", { name: nodeText.expand }));
    screen.getByText("image archive loaded", { exact: true });
  });
}
