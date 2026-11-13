// The delivery row of the deploy form: what the server forecasts for each node
// the draft names, and the one-line note when placement is automatic. It is
// information only; nothing in it is a choice or reaches a request.
import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deploy"]);
const { cleanup, render, screen, within } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
type AcquisitionResult = import("../src/api.ts").AcquisitionResult;
type AcquisitionNodeFact = import("../src/api.ts").AcquisitionNodeFact;
const DeployNodeAcquisition = (await import("../src/pages/deploy/DeployNodeAcquisition.tsx")).default;

afterEach(() => cleanup());
after(() => { closeChannels(); dom.window.close(); });

const na = locale.deploy.workbench.nodeAcquisition as Record<string, string>;
const IMG = "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2";

function view(acquisition: AcquisitionResult | undefined, options: { automatic?: boolean; loading?: boolean; declaredProviders?: string[] } = {}) {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter>
        <DeployNodeAcquisition acquisition={acquisition} automatic={options.automatic ?? false} loading={options.loading} declaredProviders={options.declaredProviders} />
      </MemoryRouter>
    </I18nextProvider>,
  );
}

const forecast = (...nodes: AcquisitionNodeFact[]): AcquisitionResult => ({ image: { image: IMG }, nodes });
const table = () => screen.getByRole("table", { name: na.forecastNote });
const row = (name: string) => within(table()).getByText(name).closest("tr") as HTMLElement;
const cells = (name: string) => [...row(name).querySelectorAll("td")].map((cell) => cell.textContent ?? "");

const present: AcquisitionNodeFact = {
  node_uuid: "u1", node_name: "gpu-a-01", image_cached: true, image_plan: "present", weights_present: true, weights_plan: "present",
};

test("named nodes get a captioned forecast table with node, image and weights columns", () => {
  view(forecast(present));
  const headers = within(table()).getAllByRole("columnheader").map((h) => h.textContent);
  assert.deepEqual(headers, [na.node, na.image, na.weights]);
  assert.ok(screen.getByText(na.forecastNote), "the caption says it is a forecast, not a choice");
  assert.deepEqual(cells("gpu-a-01"), ["gpu-a-01", na.present, na.present]);
});

test("every image plan reads as what will happen", () => {
  view(forecast(
    { ...present, node_uuid: "c", node_name: "copy-node", image_cached: false, image_plan: "copy", image_copy_from: "gpu-a-01" },
    { ...present, node_uuid: "p", node_name: "pull-node", image_cached: false, image_plan: "pull", image_pull_refs: [`quay.io/${IMG}`, `swr.example/${IMG}`, IMG], image_pull_ranked: true },
    { ...present, node_uuid: "r", node_name: "rule-node", image_cached: false, image_plan: "pull", image_pull_refs: [`harbor.corp.example/${IMG}`, `quay.io/${IMG}`, IMG], image_pull_ranked: true, image_pull_fixed: 1 },
    { ...present, node_uuid: "o", node_name: "rule-only-node", image_cached: false, image_plan: "pull", image_pull_refs: [`harbor-a.example/${IMG}`, `harbor-b.example/${IMG}`], image_pull_fixed: 2 },
    { ...present, node_uuid: "t", node_name: "rule-then-one", image_cached: false, image_plan: "pull", image_pull_refs: [`harbor.corp.example/${IMG}`, IMG], image_pull_ranked: true, image_pull_fixed: 1 },
    { ...present, node_uuid: "h", node_name: "hub-node", image_cached: false, image_plan: "pull", image_pull_refs: [IMG] },
    { ...present, node_uuid: "a", node_name: "archive-node", image_cached: false, image_plan: "pull", image_pull_refs: ["https://containerimages.example/runner.tar"] },
  ));
  const fill = (key: string, values: Record<string, string>) => na[key].replace(/\{\{(\w+)\}\}/g, (_, k) => values[k]);
  assert.equal(cells("copy-node")[1], `Copy from gpu-a-01 ${na.sameGroup}`);
  assert.equal(cells("pull-node")[1], fill("pullRanked", { sources: "quay.io / swr.example / docker.io" }), "every source the probe chooses between is named");
  assert.equal(within(row("pull-node")).getByText(/quay\.io/).closest("span")?.getAttribute("title"), `quay.io/${IMG}\nswr.example/${IMG}\n${IMG}`, "every candidate stays readable");
  assert.equal(cells("rule-node")[1], fill("pullFixedThenRanked", { fixed: "harbor.corp.example", sources: "quay.io / docker.io" }), "a rule's endpoint goes first, the image's own registries after it");
  assert.equal(cells("rule-only-node")[1], fill("pullFixed", { sources: "harbor-a.example → harbor-b.example" }), "endpoints only, in their order");
  assert.equal(cells("rule-then-one")[1], fill("pullFixedThen", { fixed: "harbor.corp.example", source: "docker.io" }), "one registry after the endpoint: nothing to probe");
  assert.equal(cells("hub-node")[1].trim(), fill("pullFrom", { source: "docker.io" }), "a reference without a registry is Docker Hub's, and nothing says it was probed");
  assert.equal(cells("archive-node")[1].trim(), fill("pullFrom", { source: "containerimages.example" }));
});

test("a pull that cannot be planned shows the server's error, in the reader's language, instead of a source", () => {
  const forbidden = { code: "IMAGE_SOURCE_FORBIDDEN", params: { node: "stuck", route: "/api/v1/pull/source-rules" }, message: "no pull source is allowed" };
  const untyped = { code: "SOMETHING_NEW", message: "no pull source reaches the node" };
  view(forecast(
    { ...present, node_name: "stuck", image_cached: false, image_plan: "pull", image_pull_error: forbidden, image_pull_refs: undefined },
    { ...present, node_uuid: "n", node_name: "new", image_cached: false, image_plan: "pull", image_pull_error: untyped, image_pull_refs: undefined },
  ));
  assert.equal(cells("stuck")[1], `Cannot pull: ${(locale.common.apiError.IMAGE_SOURCE_FORBIDDEN as string).replace("{{node}}", "stuck")}`);
  assert.equal(cells("new")[1], "Cannot pull: no pull source reaches the node", "a code the client does not know falls back to the server's text");
  assert.equal(screen.queryAllByText(/Pull from/).length, 0);
});

test("an image whose cache cannot be read is unknown, not a guess", () => {
  view(forecast(
    { ...present, node_uuid: "o", node_name: "offline", image_cached: undefined, image_cache_reason: "node_offline", image_plan: "pull", image_pull_refs: [IMG] },
    { ...present, node_uuid: "u", node_name: "unselected", image_cached: undefined, image_cache_reason: "node_unselected", image_plan: "pull" },
    { ...present, node_uuid: "f", node_name: "failed", image_cached: undefined, image_cache_reason: "query_failed", image_plan: "pull", image_pull_refs: [IMG] },
    { ...present, node_uuid: "x", node_name: "future", image_cached: false, image_plan: "teleport" },
  ));
  assert.equal(cells("offline")[1], na.unknownOffline);
  assert.equal(cells("unselected")[1], na.unknownUnselected);
  // A failed cache query still has a plan; only an unreadable node has none.
  assert.equal(cells("failed")[1].trim(), "Pull from docker.io");
  assert.equal(cells("future")[1], na.unknown, "a plan this client does not know is shown as unknown");
});

test("every weights plan reads as what will happen", () => {
  const base = { ...present, image_cached: true, image_plan: "present", weights_present: false };
  view(forecast(
    { ...base, node_uuid: "1", node_name: "w-copy", weights_plan: "copy", weights_copy_from: "gpu-a-03" },
    { ...base, node_uuid: "2", node_name: "w-storage", weights_plan: "storage", weights_storage: "model-archive" },
    { ...base, node_uuid: "3", node_name: "w-download", weights_plan: "download", weights_provider: "huggingface" },
    { ...base, node_uuid: "4", node_name: "w-new", weights_plan: "levitate" },
  ));
  assert.equal(cells("w-copy")[2], `Copy from gpu-a-03 ${na.sameGroup}`);
  assert.equal(cells("w-storage")[2], "Read from storage model-archive");
  assert.equal(cells("w-download")[2], "Download from huggingface");
  assert.equal(cells("w-new")[2], na.unknown);
});

test("a download names the hosts the node will try, in order", () => {
  const base = { ...present, image_cached: true, image_plan: "present", weights_present: false, weights_plan: "download", weights_provider: "huggingface" };
  view(forecast(
    { ...base, node_uuid: "1", node_name: "one-host", weights_fetch_hosts: ["modelscope.cn"] },
    { ...base, node_uuid: "2", node_name: "two-hosts", weights_fetch_hosts: ["hf-mirror.com", "huggingface.co"] },
  ));
  assert.equal(cells("one-host")[2], "Download from modelscope.cn");
  assert.equal(cells("two-hosts")[2], "Download from hf-mirror.com → huggingface.co, in this order");
});

test("a server forecast that carries weights_fetch_hosts is rendered from them", async () => {
  const fixture = JSON.parse((await import("node:fs")).readFileSync(new URL("./fixtures/evaluate/template_vllm.json", import.meta.url), "utf8"));
  const node = fixture.acquisition.nodes[0] as AcquisitionNodeFact;
  assert.ok(node.weights_fetch_hosts?.length, "the fixture is generated by the Go server and names the hosts");
  view({ nodes: [node] });
  assert.equal(cells(node.node_name)[2], `Download from ${node.weights_fetch_hosts!.join(" → ")}`);
});

test("a download the rules forbid says so and, when the template declares another source, points at it", () => {
  const forbidden = { code: "WEIGHTS_FETCH_FORBIDDEN", params: { node: "locked", model: "org/model" }, message: "no weights address is allowed" };
  const base = { ...present, image_cached: true, image_plan: "present", weights_present: false, weights_plan: "download", weights_provider: "huggingface", weights_fetch_error: forbidden };
  view(forecast(
    { ...base, node_uuid: "1", node_name: "locked" },
    { ...base, node_uuid: "2", node_name: "locked-other", weights_fetch_error: { ...forbidden, params: { node: "locked-other", model: "org/model" } } },
  ), { declaredProviders: ["huggingface"] });
  const text = (locale.common.apiError.WEIGHTS_FETCH_FORBIDDEN as string).replace("{{node}}", "locked").replace("{{model}}", "org/model");
  assert.equal(cells("locked")[2], `Cannot download: ${text}`, "no other declared source: no hint");
  cleanup();
  view(forecast({ ...base, node_uuid: "1", node_name: "locked" }), { declaredProviders: ["huggingface", "modelscope"] });
  assert.equal(cells("locked")[2], `Cannot download: ${text} The ModelScope source can be used instead`);
  assert.ok(text.includes("register the model's path") || text.includes("shared storage"), "the error text itself says where else the model can come from");
});

test("an unrelated download error shows no source hint", () => {
  const other = { code: "SOMETHING_NEW", message: "cannot plan" };
  view(forecast({ ...present, image_cached: true, image_plan: "present", weights_present: false, weights_plan: "download", weights_provider: "huggingface", weights_fetch_error: other }), { declaredProviders: ["huggingface", "modelscope"] });
  assert.equal(cells("gpu-a-01")[2], "Cannot download: cannot plan");
});

test("an unavailable model path shows its reason rather than a download forecast", () => {
  view(forecast({ ...present, weights_present: false, weights_plan: "unavailable", weights_fetch_error: {
    code: "MODEL_SOURCE_UNAVAILABLE", params: { reason: "facts_unavailable" }, message: "cannot read model facts",
  } }));
  const detail = locale.deploy.diagnostics.reason.facts_unavailable;
  assert.equal(cells("gpu-a-01")[2], na.unavailable.replace("{{detail}}", detail));
  assert.equal(cells("gpu-a-01")[2].includes("Download from"), false);
});

test("automatic placement says the platform fetches after scheduling and links to the pull settings", () => {
  view({ image: { image: IMG } }, { automatic: true });
  assert.ok(screen.getByText(na.automatic, { exact: false }));
  assert.equal(screen.queryAllByRole("table").length, 0, "no node is named, so there is nothing to forecast");
  const link = screen.getByRole("link", { name: na.viewSettings });
  assert.equal(link.getAttribute("href"), "/settings");
});

test("nothing is said before an answer, after a failed one, or for a manual draft that names no node", () => {
  const empty = view(undefined, { automatic: true });
  assert.equal(empty.container.textContent, "", "no evaluation yet");
  empty.unmount();
  const manual = view({ image: { image: IMG } }, { automatic: false });
  assert.equal(manual.container.textContent, "", "a manual draft without a node has no forecast and is not automatic either");
});

test("a re-evaluation in flight marks the forecast as busy instead of presenting it as current", () => {
  const busy = view(forecast(present), { loading: true });
  assert.ok(busy.container.querySelector(".ant-spin-spinning"), "the table shows its loading state");
  busy.unmount();
  const settled = view(forecast(present), { loading: false });
  assert.equal(settled.container.querySelector(".ant-spin-spinning"), null);
});

test("the forecast table is visible without expanding anything", () => {
  const shown = view(forecast(present));
  assert.equal(shown.container.querySelector(".ant-collapse"), null);
  assert.equal(shown.container.querySelector(".deploy-workbench-label")?.textContent, na.title);
});
