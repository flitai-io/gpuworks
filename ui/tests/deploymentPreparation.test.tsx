import { createI18n, dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { ModelNodePresence } from "../src/api";
import { MemoryRouter } from "react-router-dom";
const { render, screen, cleanup, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { DeploymentPreparation } = await import("../src/pages/DeploymentPreparation.tsx");
const { api } = await import("../src/api.ts");
const { i18n, locale } = await createI18n(["deployments"]);
const labels = locale.deployments.preparation;
const original = api.retryModelCache;
afterEach(() => { cleanup(); api.retryModelCache = original; });
after(() => dom.window.close());
function row(overrides: Partial<ModelNodePresence> = {}) { return { path: "/cache/weights", transferred_bytes: 40, acquisition: "copy", node_uuid: "node", node_name: "GPU", model_id: 1, model_name: "weights", storage_id: 2, storage_name: "NFS", cache: true, state: "downloading", downloaded: 40, size_bytes: 100, progress: 0.4, rate_bps: 10, preparation: { phase: "copying", total_known: true, idle_seconds: 0, eta_seconds: 6 }, ...overrides } as ModelNodePresence; }
function show(rows: ReturnType<typeof row>[], changed = () => { }) { return render(<I18nextProvider i18n={i18n}><MemoryRouter><DeploymentPreparation rows={rows} onChanged={changed}/></MemoryRouter></I18nextProvider>); }
test("preparation shows real bytes, phase and storage link", async () => {
    show([row()]);
    assert.ok(screen.getByText(labels.phase.copying));
    assert.ok(screen.getByText(/40 B \/ 100 B/));
    assert.equal(screen.getByRole("link", { name: "NFS" }).getAttribute("href"), "/storage?storage=2");
    const user = userEvent.setup({ document: dom.window.document });
    await user.tab();
    assert.equal(dom.window.document.activeElement, screen.getByRole("link"));
});
test("old agent, unknown total and checking have no invented percentage", () => {
    show([row({ rate_bps: 0, preparation: { phase: "checking", total_known: false, idle_seconds: 0 } })]);
    assert.ok(screen.getByText(labels.phase.checking));
    assert.ok(screen.getByText(new RegExp(labels.unknownTotal)));
    assert.ok(screen.getByText(/40 B/));
    assert.equal(screen.queryByRole("progressbar"), null);
    cleanup();
    show([row({ rate_bps: 0, preparation: undefined })]);
    assert.ok(screen.getByText(labels.phase.waiting));
    assert.equal(screen.queryByRole("progressbar"), null);
});
test("stall and disconnection show no speed or ETA once the server zeroes them", () => {
    show([row({ rate_bps: 0, preparation: { phase: "copying", reason: "MODEL_COPY_STALLED", total_known: true, idle_seconds: 120 } })]);
    assert.ok(screen.getByText(labels.reason.MODEL_COPY_STALLED));
    assert.ok(screen.getByText(labels.waitForIO));
    assert.equal(screen.queryByRole("button"), null);
    cleanup();
    show([row({ rate_bps: 0, preparation: { phase: "waiting", reason: "MODEL_NODE_OFFLINE", total_known: false, idle_seconds: 0 } })]);
    assert.ok(screen.getByText(labels.reason.MODEL_NODE_OFFLINE));
    assert.equal(screen.queryByText(/\/s/), null);
    assert.equal(screen.queryByRole("progressbar"), null);
});
test("a download shows the bytes and rate the server marks measurable", () => {
    show([row({ acquisition: "download", cache: false, rate_bps: 2048, preparation: { phase: "waiting", reason: "MODEL_PREPARATION_WAITING", total_known: true, idle_seconds: 0 } })]);
    assert.ok(screen.getByRole("progressbar"));
    assert.ok(screen.getByText(/40 B \/ 100 B/));
    assert.ok(screen.getByText(/\/s/));
});
test("retry is offered only where the server says the copy is retryable", () => {
    show([row({ state: "failed", transfer_id: "intent", preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0 } })]);
    assert.equal(screen.queryByRole("button", { name: labels.retry }), null);
});
test("ready and empty preparation are hidden; verification does not say ready", () => {
    show([]);
    assert.equal(screen.queryByRole("region"), null);
    cleanup();
    show([row({ state: "present" })]);
    assert.equal(screen.queryByRole("region"), null);
    cleanup();
    show([row({ downloaded: 100, progress: 1, preparation: { phase: "verifying", total_known: true, idle_seconds: 0 } })]);
    assert.ok(screen.getByText(labels.phase.verifying));
    assert.equal(screen.queryByText(labels.phase.ready), null);
});
test("retry sends exact model/node/storage and waits for acknowledgement", async () => {
    let release!: () => void;
    const waiting = new Promise<void>(resolve => { release = resolve; });
    let changed = 0;
    api.retryModelCache = async (id, node, storage) => { assert.deepEqual([id, node, storage], [1, "node", 2]); await waiting; return { status: "pending" }; };
    show([row({ state: "failed", transfer_id: "intent", preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0, retryable: true } })], () => { changed++; });
    const user = userEvent.setup({ document: dom.window.document });
    await user.click(screen.getByRole("button", { name: labels.retry }));
    assert.equal(changed, 0);
    assert.equal((screen.getByRole("button") as HTMLButtonElement).disabled, true);
    release();
    await waitFor(() => assert.equal(changed, 1));
});
test("failed retry preserves the failed row and permits a new attempt", async () => {
    let changed = 0;
    api.retryModelCache = async () => { throw new Error("previous copy still running"); };
    show([row({ state: "failed", transfer_id: "intent", message: "copy failed", preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0, retryable: true } })], () => { changed++; });
    const user = userEvent.setup({ document: dom.window.document });
    await user.click(screen.getByRole("button", { name: labels.retry }));
    await waitFor(() => assert.equal((screen.getByRole("button", { name: labels.retry }) as HTMLButtonElement).disabled, false));
    assert.equal(changed, 0);
    assert.ok(screen.getByText("copy failed"));
    assert.ok(await screen.findByText("previous copy still running"));
});

test("shared storage downloads expose their phase and bytes before a total is known", () => {
    show([row({ cache: false, acquisition: "download", downloaded: 4096, size_bytes: 0, rate_bps: 2048, preparation: { phase: "downloading", total_known: false, idle_seconds: 0 } })]);
    assert.ok(screen.getByText("Downloading model"));
    assert.ok(screen.getByText(/4.0 KiB/));
    assert.ok(screen.getByText(new RegExp(labels.unknownTotal)));
    assert.ok(screen.getByText(/\/s/));
    assert.equal(screen.queryByRole("progressbar"), null);
});

test("shared model sync progress keeps the server phase", () => {
    show([row({ cache: false, acquisition: "peer", preparation: { phase: "syncing", total_known: true, idle_seconds: 0 } })]);
    assert.ok(screen.getByText("Syncing model"));
    assert.ok(screen.getByText(/40 B \/ 100 B/));
});

// The Jira's "no retry entry, no automatic retry, and nobody says so": the
// button must come with the statement that nothing retries on its own.
test("an offered retry says the copy is not retried automatically", () => {
    show([row({ state: "failed", transfer_id: "intent", preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0, retryable: true } })]);
    assert.ok(screen.getByRole("button", { name: labels.retry }));
    assert.ok(screen.getByText(labels.noAutoRetry));
});

// A failure the server does not offer to retry (a shared or download failure
// has no cache copy intent to replace) says why rather than rendering nothing.
test("a failed copy that cannot be retried says so instead of staying silent", () => {
    show([row({ state: "failed", acquisition: "download", cache: false, message: "permission denied", preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0 } })]);
    assert.equal(screen.queryByRole("button", { name: labels.retry }), null);
    assert.ok(screen.getByText(labels.retryUnavailable));
    assert.ok(screen.getByText("permission denied"));
});

// The reported failure text is the agent's own words, and a peer/download
// origin line must not hide it.
test("the failure message survives a transfer origin line", () => {
    const message = "copy .msc: open /cache/weights/.msc: permission denied";
    show([row({ state: "failed", acquisition: "peer", source_node_name: "gpu-9", source_node_uuid: "u-9", message, preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0, retryable: true } })]);
    assert.ok(screen.getByText(message));
    assert.ok(screen.getByText(labels.reason.MODEL_PREPARATION_FAILED));
});

// Disk exhaustion is reported with its own real state/reason, including when a
// peer's origin line is present. The origin must not hide the actionable error.
test("a peer disk-space failure preserves its path and capacity detail", () => {
    const detail = "insufficient disk space at /cache/models: available=1048576 required=2147483648";
    show([row({ state: "insufficient_disk_space", acquisition: "peer", source_node_name: "gpu-9", source_node_uuid: "u-9", message: detail, preparation: { phase: "waiting", reason: "MODEL_CACHE_DISK_FULL", total_known: false, idle_seconds: 0 } })]);
    assert.equal(screen.queryByText(detail)?.textContent, detail, "disk details must not be hidden by the peer source");
    assert.ok(screen.getByText("Insufficient model disk space"));
    assert.equal(screen.queryByRole("button", { name: labels.retry }), null);
    assert.equal(screen.queryByText(labels.retryUnavailable), null);
    assert.equal(screen.queryByText(labels.noAutoRetry), null);
});


test("a disk-full cache copy keeps only the retry action offered by the server", async () => {
    const detail = "insufficient disk space at /cache/models: available=1048576 required=2147483648";
    let changed = 0;
    const calls: unknown[] = [];
    api.retryModelCache = async (id, node, storage) => { calls.push([id, node, storage]); return { status: "pending" }; };
    show([row({ state: "insufficient_disk_space", transfer_id: "intent", message: detail, preparation: { phase: "waiting", reason: "MODEL_CACHE_DISK_FULL", total_known: false, idle_seconds: 0, retryable: true } })], () => { changed++; });
    assert.ok(screen.getByText(detail));
    assert.ok(screen.getByText(labels.noAutoRetry));
    const user = userEvent.setup({ document: dom.window.document });
    await user.click(screen.getByRole("button", { name: labels.retry }));
    await waitFor(() => assert.equal(changed, 1));
    assert.deepEqual(calls, [[1, "node", 2]]);
});
