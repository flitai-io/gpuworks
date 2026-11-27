// Shared harness for the image page tests: jsdom globals, the app's own i18n
// instance in Chinese (so API errors are localized through the same path the
// browser uses), and a fetch stub that answers with the server's wire JSON.
import fs from "node:fs";
import { dom } from "./dom.ts";

class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
for (const name of ["File", "Blob", "KeyboardEvent", "FileList", "DataTransfer"] as const) {
  if (name in dom.window) Object.defineProperty(globalThis, name, { value: (dom.window as never)[name], configurable: true });
}

const read = (lng: string, ns: string) =>
  JSON.parse(fs.readFileSync(new URL(`../../src/i18n/locales/${lng}/${ns}.json`, import.meta.url), "utf8"));
export const zhImages = read("zh", "images");
export const zhCommon = read("zh", "common");

const { default: i18n } = await import("../../src/i18n/index.ts");
i18n.addResourceBundle("zh", "common", zhCommon, true, true);
i18n.addResourceBundle("zh", "images", zhImages, true, true);
// The confirmation before the channel's catalog replaces an uploaded one is the settings page's.
i18n.addResourceBundle("zh", "settings", read("zh", "settings"), true, true);
await i18n.changeLanguage("zh");
export { i18n };

export type Call = { method: string; path: string; body?: string; contentType?: string };
export type Handler = (call: Call) => Response | Promise<Response> | undefined;

export function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

/**
 * installFetch routes every request through `handler`; a request the test did
 * not expect fails loudly as a 599 so a silent extra call cannot pass.
 */
export function installFetch(handler: Handler) {
  const original = globalThis.fetch;
  const calls: Call[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://localhost/");
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const call: Call = {
      method: init?.method ?? "GET",
      path: url.pathname + url.search,
      body: typeof init?.body === "string" ? init.body : undefined,
      contentType: headers["Content-Type"],
    };
    calls.push(call);
    const res = await handler(call);
    return res ?? json(599, { error: `unexpected ${call.method} ${call.path}` });
  }) as typeof fetch;
  return { calls, restore: () => { globalThis.fetch = original; } };
}

const minutesAgo = (minutes: number) => new Date(Date.now() - minutes * 60_000 - 1_000).toISOString();

// Wire fixtures, shaped like dto.ImageSeries / dto.Image / dto.ImageNode. The
// fleet has three nodes (gpu-01..03), so every total_nodes is 3.
export const systemSeries = {
  id: 1, name: "vLLM · CUDA", engine: "vllm", purpose: "inference", framework: "cuda", source: "official",
  enabled: true, version_count: 1, present_nodes: 1, total_nodes: 3,
};
export const legacySeries = {
  id: 2, name: "gpustack runner · CUDA", engine: "vllm", purpose: "inference", framework: "cuda", source: "official",
  enabled: true, version_count: 1, present_nodes: 0, total_nodes: 3,
};
export const communitySeries = {
  id: 3, name: "vllm/vllm-openai", engine: "vllm", purpose: "inference", framework: "cuda", source: "community",
  enabled: true, version_count: 1, present_nodes: 2, total_nodes: 3,
};

// A node without a row for a version is listed absent, with no updated_at.
const absent = (uuid: string, name: string) => ({ node_uuid: uuid, node_name: name, node_state: "online", state: "absent", updated_at: "" });

// GET /images/versions entries: counts only, never node rows.
export const systemVersion = {
  id: 11, series_id: 1, purpose: "inference", name: "cuda13.0-vllm0.30.0-r2", engine: "vllm", engine_version: "0.30.0",
  framework: "cuda", framework_version: "13.0", arch: "multi", image_ref: "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2",
  source: "official", deprecated: false, enabled: true, rolling_tag: false, upstream_gone: false, effective_enabled: true,
  reference_count: 0, present_nodes: 1, total_nodes: 3, size_bytes: 1024 * 1024 * 1024,
  local_runtime_size_bytes: 2 * 1024 * 1024 * 1024, local_size_known_nodes: 1,
  registry_sources: ["quay.io/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2"],
};
export const legacyVersion = {
  id: 21, series_id: 2, purpose: "inference", name: "cuda12.9-vllm0.25.1", engine: "vllm", engine_version: "0.25.1",
  framework: "cuda", framework_version: "12.9", arch: "multi", image_ref: "gpustack/runner:cuda12.9-vllm0.25.1",
  source: "official", deprecated: true, deprecated_since: "2026-10-08.1a2b3c4d", enabled: true, rolling_tag: false,
  upstream_gone: false, effective_enabled: true, reference_count: 1, present_nodes: 0, total_nodes: 3,
};
export const nightlyVersion = {
  id: 31, series_id: 3, purpose: "inference", name: "nightly", engine: "vllm", framework: "cuda", arch: "multi",
  image_ref: "vllm/vllm-openai:nightly", source: "community", deprecated: false, enabled: true, rolling_tag: true,
  upstream_gone: false, effective_enabled: true, reference_count: 0, present_nodes: 2, total_nodes: 3,
};

// The same versions as GET /images/{series_id} serves them: one node row per
// fleet node. Rows are built when a response is served, not when this module
// loads: "updated 5 minutes ago" must still read that way however late a test
// runs (a coverage run can reach a test minutes after import).
const systemVersionNodes = () => [
  { node_uuid: "n1", node_name: "gpu-01", node_state: "online", state: "present", source: "quay", image_digest_id: "sha256:aaaaaaaaaaaabbbbbbbbbbbb", updated_at: minutesAgo(90) },
  absent("n2", "gpu-02"),
  absent("n3", "gpu-03"),
];
const nightlyVersionNodes = () => [
  { node_uuid: "n1", node_name: "gpu-01", node_state: "online", state: "present", image_digest_id: "sha256:0123456789abcdef0123456789abcdef", updated_at: minutesAgo(5) },
  { node_uuid: "n2", node_name: "gpu-02", node_state: "online", state: "present", image_digest_id: "sha256:fedcba9876543210fedcba9876543210", updated_at: minutesAgo(180) },
  { node_uuid: "n3", node_name: "gpu-03", node_state: "online", state: "absent", updated_at: minutesAgo(600) },
];

export const seriesList = [systemSeries, legacySeries, communitySeries];
export const versionList = [systemVersion, legacyVersion, nightlyVersion];
export const seriesDetails: Record<string, unknown> = {
  get "/api/v1/images/1"() { return { ...systemSeries, versions: [{ ...systemVersion, nodes: systemVersionNodes() }] }; },
  "/api/v1/images/2": { ...legacySeries, versions: [{ ...legacyVersion, nodes: [absent("n1", "gpu-01"), absent("n2", "gpu-02"), absent("n3", "gpu-03")] }] },
  get "/api/v1/images/3"() { return { ...communitySeries, versions: [{ ...nightlyVersion, nodes: nightlyVersionNodes() }] }; },
};

// GET /api/v1/images/engines: the image form's engine dictionary, in order.
export const imageEngines = [
  { name: "vllm", label: "vLLM" },
  { name: "vllm-omni", label: "vLLM Omni" },
  { name: "sglang", label: "SGLang" },
  { name: "callable", label: "Callable" },
];

/** The catalog endpoints every render of the page reads. */
export const catalogRoutes: Handler = (call) => {
  if (call.method !== "GET") return undefined;
  if (call.path === "/api/v1/images") return json(200, { items: seriesList, total: seriesList.length });
  if (call.path === "/api/v1/images/versions") return json(200, { items: versionList, total: versionList.length });
  if (call.path === "/api/v1/settings") return json(200, {});
  if (call.path === "/api/v1/images/engines") return json(200, { items: imageEngines, total: imageEngines.length });
  if (seriesDetails[call.path]) return json(200, seriesDetails[call.path]);
  return undefined;
};

// GET /api/v1/images/community/repos: the six built-in cards.
export const communityRepos = [
  { repo: "vllm/vllm-openai", registry: "docker-hub", engine: "vllm", framework: "cuda" },
  { repo: "vllm/vllm-omni", registry: "docker-hub", engine: "vllm-omni", framework: "cuda" },
  { repo: "lmsysorg/sglang", registry: "docker-hub", engine: "sglang", framework: "cuda" },
  { repo: "quay.io/ascend/vllm-ascend", registry: "quay", engine: "vllm", framework: "cann" },
  { repo: "quay.io/ascend/sglang", registry: "quay", engine: "sglang", framework: "cann" },
  {
    repo: "harbor.sourcefind.cn:5443/dcu/admin/base/vllm", registry: "oci", engine: "vllm", framework: "dtk",
    siblings: ["harbor.sourcefind.cn:5443/dcu/admin/base/custom", "harbor.sourcefind.cn:5443/dcu/admin/base/sglang"],
  },
];

// GET /api/v1/images/community/browse?repo=vllm/vllm-openai: every upstream
// tag, unfiltered, with Docker Hub's last_updated passed through.
export const vllmOpenAIBrowse = {
  repo: "vllm/vllm-openai", registry: "docker-hub", engine: "vllm", framework: "cuda",
  tags: [
    { tag: "v0.11.0", engine_version: "0.11.0", last_updated: "2026-10-01T08:00:00Z" },
    { tag: "qwen38", last_updated: "2026-09-28T12:30:00Z" },
    { tag: "nightly", rolling_tag: true, last_updated: "2026-10-08T02:00:00Z" },
    { tag: "latest", rolling_tag: true, last_updated: "2026-10-01T08:00:00Z" },
    { tag: "v0.10.2", engine_version: "0.10.2", upstream_gone: true },
  ],
};
