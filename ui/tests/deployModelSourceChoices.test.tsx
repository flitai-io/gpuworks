// The weights row of the deploy form. A request can say only two things about
// the model's files: which of the template's public weight sources to run
// (model_source.provider) and which storage a node saves the model into when it
// has to fetch it (model_source.storage_id). Each question is asked only when
// the evaluation offers a real choice; where a node gets the files from is the
// server's call when the replica starts.
import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deploy"]);
const { cleanup, render, screen, waitFor, fireEvent, within } = await import("@testing-library/react");
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
type AcquisitionResult = import("../src/api.ts").AcquisitionResult;
type Candidate = import("../src/api.ts").AcquisitionCandidate;
const DeployWeightSourceFields = (await import("../src/pages/deploy/DeployWeightSourceFields.tsx")).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { weightStorageInvalid, askProvider, askSaveTo } = await import("../src/pages/deploy/deployWeightChoices.ts");

afterEach(() => cleanup());
after(() => { closeChannels(); dom.window.close(); });

const text = (key: string) => locale.deploy.workbench.acquisition[key] as string;
const typeName = (type: string) => locale.common.storageType[type] as string;
const GiB = 1024 ** 3;

const huggingface: Candidate = { key: "public:huggingface:org/model:main", label: "huggingface", reason: "configured model provider" };
const modelscope: Candidate = { key: "public:modelscope:org/model:main", label: "modelscope", reason: "configured model provider", recommended: true };
const localTarget: Candidate = {
  key: "storage:1", label: "default", reason: "writable storage", recommended: true,
  storage_id: 1, storage_type: "local", default: true,
};
const scratchTarget: Candidate = { key: "storage:3", label: "scratch", reason: "writable storage", storage_id: 3, storage_type: "local" };
const nfsTarget: Candidate = { key: "storage:7", label: "model-archive", reason: "writable storage", storage_id: 7, storage_type: "nfs" };
const nfsPresent: Candidate = {
  key: "present:7", label: "model-archive", reason: "model files are in this storage", recommended: true,
  storage_id: 7, storage_type: "nfs", size_bytes: 15 * GiB,
};

const acq = (parts: Partial<AcquisitionResult>): AcquisitionResult => ({ image: { image: "vllm:v1" }, ...parts });

let form!: FormInstance;
function Harness({ acquisition, modelSource }: { acquisition?: AcquisitionResult; modelSource?: string }) {
  const [instance] = Form.useForm();
  form = instance;
  return (
    <Form form={instance}>
      <DeployWeightSourceFields acquisition={acquisition} modelSource={modelSource} />
    </Form>
  );
}
const tree = (acquisition?: AcquisitionResult, modelSource?: string) => (
  <I18nextProvider i18n={i18n}><Harness acquisition={acquisition} modelSource={modelSource} /></I18nextProvider>
);
const mount = (acquisition?: AcquisitionResult, modelSource?: string) => render(tree(acquisition, modelSource));

// The model_source the evaluation request would carry for the form as it is.
const requested = () => buildPayload({ engine: "vllm", model_id: 1, ...form.getFieldsValue(true) }).payload.model_source;
const key = (select: HTMLElement, name: "ArrowDown" | "Enter") =>
  fireEvent.keyDown(select, { key: name, code: name, keyCode: name === "Enter" ? 13 : 40 });
const targetName = (name: string, type: string) =>
  text("storageTarget").replace("{{name}}", name).replace("{{type}}", typeName(type));
const optionRow = (label: string) => {
  const row = screen.getAllByText(label).map((node) => node.closest(".ant-select-item-option")).find(Boolean);
  assert.ok(row, `an option row for ${label}`);
  return row as HTMLElement;
};

// ---------------------------------------------------------------- which weights run

test("two public weight sources ask which to run, default to the recommended one and send the choice", async () => {
  mount(acq({ providers: [huggingface, modelscope] }));
  const select = screen.getByRole("combobox", { name: text("modelProvider") });
  await waitFor(() => assert.deepEqual(requested(), { provider: "modelscope" }));
  assert.equal(document.querySelector(".ant-select-selection-item")?.textContent, "modelscope", "the recommended source is what the control shows");

  // Open the list (the highlight starts on its first entry) and pick with the keyboard alone.
  key(select, "ArrowDown");
  key(select, "Enter");
  await waitFor(() => assert.deepEqual(requested(), { provider: "huggingface" }));
  assert.equal(document.querySelector(".ant-select-selection-item")?.textContent, "huggingface");
});

test("a model registered from a given source locks the provider: no select, the locked label is sent", async () => {
  mount(acq({ providers: [huggingface, modelscope] }), "HuggingFace");
  assert.equal(screen.queryAllByRole("combobox").length, 0, "the already-declared source is not asked again");
  assert.ok(screen.getByText("huggingface"), "but the source that runs stays visible");
  await waitFor(() => assert.deepEqual(requested(), { provider: "huggingface" }));
});

test("a single public weight source is not a question and sends nothing", async () => {
  const view = mount(acq({ providers: [modelscope], targets: [localTarget] }));
  assert.equal(view.container.textContent, "", "no row at all");
  assert.equal(requested(), undefined, "the server follows its own default");
  assert.equal(askProvider(acq({ providers: [modelscope] })), false);
  view.unmount();
  // A locked source is still the one that runs, even when it is the only one.
  mount(acq({ providers: [modelscope] }), "modelscope");
  assert.equal(screen.queryAllByRole("combobox").length, 0);
  await waitFor(() => assert.deepEqual(requested(), { provider: "modelscope" }));
});

test("a provider the next evaluation no longer offers gives way to the recommended one", async () => {
  const view = mount(acq({ providers: [huggingface, modelscope] }));
  await waitFor(() => assert.deepEqual(requested(), { provider: "modelscope" }));
  const select = screen.getByRole("combobox", { name: text("modelProvider") });
  key(select, "ArrowDown");
  key(select, "Enter");
  await waitFor(() => assert.deepEqual(requested(), { provider: "huggingface" }));

  view.rerender(tree(acq({ providers: [modelscope, { ...huggingface, label: "gitee" }] })));
  await waitFor(() => assert.deepEqual(requested(), { provider: "modelscope" }));
  view.rerender(tree(acq({ providers: [modelscope] })));
  await waitFor(() => assert.equal(requested(), undefined, "with one source left nothing is asked or sent"));
});

test("a local model with no template sources clears the provider and preserves the selected storage", async () => {
  const targets = [localTarget, nfsTarget];
  const view = mount(acq({ providers: [huggingface, modelscope], targets }), "huggingface");
  await waitFor(() => assert.deepEqual(requested(), { provider: "huggingface", storage_id: 1 }));
  fireEvent.mouseDown(screen.getByRole("combobox", { name: text("saveTo") }));
  fireEvent.click(optionRow(targetName("model-archive", "nfs")));
  await waitFor(() => assert.deepEqual(requested(), { provider: "huggingface", storage_id: 7 }));

  // The API omits providers when there are no candidates.
  view.rerender(tree(acq({ targets }), "local"));
  await waitFor(() => assert.deepEqual(requested(), { storage_id: 7 }));
  assert.equal(screen.queryByRole("combobox", { name: text("modelProvider") }), null);
  assert.equal(buildPayload({ engine: "vllm", model_id: 1, ...form.getFieldsValue(true) }).payload.model_id, 1);

  // Losing all candidates must not silently discard an explicit destination.
  view.rerender(tree(acq({ providers: [] }), "local"));
  await waitFor(() => assert.deepEqual(requested(), { storage_id: 7 }));
  assert.equal(form.getFieldValue(["model_source", "provider"]), undefined);
  assert.equal(form.getFieldValue(["model_source", "storage_picked"]), true);
  assert.equal((await screen.findByRole("alert")).textContent, text("storageGone"));
});

// ---------------------------------------------------------------- where a fetched model is saved

test("several writable storages ask where to save, default to the recommended one and send the choice", async () => {
  mount(acq({ targets: [localTarget, scratchTarget, nfsTarget] }));
  const saveTo = screen.getByRole("combobox", { name: text("saveTo") });
  await waitFor(() => assert.deepEqual(requested(), { storage_id: 1 }));
  assert.ok(screen.getByText(targetName("default", "local")), "the default storage is preselected");

  key(saveTo, "ArrowDown");
  key(saveTo, "ArrowDown");
  key(saveTo, "Enter");
  await waitFor(() => assert.deepEqual(requested(), { storage_id: 3 }));
  assert.equal(form.getFieldValue(["model_source", "storage_picked"]), true);

  // The shared storage is listed too: a node may fetch straight into it.
  fireEvent.mouseDown(saveTo);
  fireEvent.click(optionRow(targetName("model-archive", "nfs")));
  await waitFor(() => assert.deepEqual(requested(), { storage_id: 7 }));
});

test("a single writable storage is not a question and sends nothing", async () => {
  const view = mount(acq({ targets: [localTarget] }));
  assert.equal(view.container.textContent, "");
  assert.equal(screen.queryAllByText(text("saveTo")).length, 0);
  assert.equal(requested(), undefined);
  assert.equal(askSaveTo(acq({ targets: [localTarget] })), false);
});

test("provider and save-to travel together and nothing else about the source does", async () => {
  mount(acq({ providers: [huggingface, modelscope], targets: [localTarget, scratchTarget] }));
  await waitFor(() => assert.deepEqual(requested(), { provider: "modelscope", storage_id: 1 }));
  const payload = buildPayload({ engine: "vllm", model_id: 1, ...form.getFieldsValue(true) }).payload;
  assert.equal(payload.image_pull, undefined, "how the image is fetched is not part of a request");
  const source = payload.model_source as Record<string, unknown>;
  for (const gone of ["kind", "source_node", "network", "local_present", "peer_sync", "public", "storage_picked"])
    assert.ok(!(gone in source), `model_source has no ${gone}`);
});

test("models already stored are listed for information; a storage that cannot serve is left out", () => {
  const unusable: Candidate = { ...nfsPresent, key: "present:9", label: "cold", storage_id: 9, unavailable: "storage_unavailable" };
  const localHolder: Candidate = { key: "present:1", label: "default", reason: "model files are in this storage", storage_id: 1, storage_type: "local", on_node: true };
  mount(acq({ present: [localHolder, nfsPresent, unusable], targets: [localTarget] }));
  const line = screen.getByText(text("storagePresent").replace("{{list}}", `${targetName("default", "local")}, ${targetName("model-archive", "nfs")}`));
  assert.ok(line, "the storages that hold the model are named");
  assert.equal(screen.queryAllByText(/cold/).length, 0);
  assert.equal(screen.queryAllByRole("combobox").length, 0, "information, not a control");
  assert.equal(requested(), undefined);
});

// ---------------------------------------------------------------- the choice follows the evaluation

test("an explicit save target is retained and reported when zero or one alternative remains", async () => {
  const view = mount(acq({ targets: [localTarget, nfsTarget] }));
  const saveTo = await screen.findByRole("combobox", { name: text("saveTo") });
  fireEvent.mouseDown(saveTo);
  fireEvent.click(optionRow(targetName("model-archive", "nfs")));
  await waitFor(() => assert.equal(form.getFieldValue(["model_source", "storage_id"]), 7));
  for (const targets of [[localTarget], []]) {
    view.rerender(tree(acq({ targets })));
    assert.ok(await screen.findByText(text("storageGone")));
    assert.equal(form.getFieldValue(["model_source", "storage_id"]), 7);
    assert.equal(form.getFieldValue(["model_source", "storage_picked"]), true);
    assert.deepEqual(requested(), { storage_id: 7 });
    assert.ok(screen.getByRole("combobox", { name: text("saveTo") }), "the person must still see the invalid choice");
  }
});

test("a selected save target becoming a complete readable copy stays valid", async () => {
  const view = mount(acq({ targets: [localTarget, nfsTarget] }));
  fireEvent.mouseDown(await screen.findByRole("combobox", { name: text("saveTo") }));
  fireEvent.click(optionRow(targetName("model-archive", "nfs")));
  await waitFor(() => assert.equal(form.getFieldValue(["model_source", "storage_id"]), 7));
  const ready = acq({ targets: [localTarget], present: [nfsPresent] });
  view.rerender(tree(ready));
  await waitFor(() => assert.equal(weightStorageInvalid(ready, form.getFieldValue("model_source")), false));
  assert.deepEqual(requested(), { storage_id: 7 });
  assert.equal(screen.queryByRole("alert"), null);
  assert.ok(screen.getByText(targetName("model-archive", "nfs"), { selector: ".ant-select-selection-item" }));
});

test("a storage the form chose by itself follows the evaluation; one the person picked is kept and flagged", async () => {
  const first = acq({ targets: [localTarget, scratchTarget] });
  const view = mount(first);
  await waitFor(() => assert.equal(form.getFieldValue(["model_source", "storage_id"]), 1));
  // Nothing picked yet: the recommendation is the form's own and follows the evaluation.
  const moved = acq({ targets: [{ ...localTarget, recommended: false, default: false }, { ...scratchTarget, recommended: true }, nfsTarget] });
  view.rerender(tree(moved));
  await waitFor(() => assert.equal(form.getFieldValue(["model_source", "storage_id"]), 1), "a still-offered storage is kept");

  const saveTo = screen.getByRole("combobox", { name: text("saveTo") });
  fireEvent.mouseDown(saveTo);
  fireEvent.click(optionRow(targetName("model-archive", "nfs")));
  await waitFor(() => assert.equal(form.getFieldValue(["model_source", "storage_id"]), 7));
  assert.equal(screen.queryAllByRole("alert").length, 0);

  // The evaluation stops offering the picked storage: it is not swapped for another.
  const second = acq({ targets: [localTarget, scratchTarget] });
  view.rerender(tree(second));
  assert.ok(await screen.findByText(text("storageGone")), "the person is told to choose again");
  assert.equal(screen.getByRole("alert").textContent, text("storageGone"));
  assert.equal(form.getFieldValue(["model_source", "storage_id"]), 7, "the choice is kept, not replaced");
  assert.equal(weightStorageInvalid(second, form.getFieldValue("model_source")), true);
  assert.deepEqual(requested(), { storage_id: 7 }, "a request would still name it; the create refuses it (below)");

  // Choosing again clears it.
  fireEvent.mouseDown(screen.getByRole("combobox", { name: text("saveTo") }));
  fireEvent.click(optionRow(targetName("scratch", "local")));
  await waitFor(() => assert.equal(screen.queryAllByText(text("storageGone")).length, 0));
  assert.deepEqual(requested(), { storage_id: 3 });
  assert.equal(weightStorageInvalid(second, form.getFieldValue("model_source")), false);

  // Down to one writable storage: the explicit choice is still kept and flagged.
  view.rerender(tree(acq({ targets: [localTarget] })));
  assert.ok(await screen.findByText(text("storageGone")));
  assert.deepEqual(requested(), { storage_id: 3 });
  assert.equal(form.getFieldValue(["model_source", "storage_picked"]), true);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: text("saveTo") }));
  fireEvent.click(optionRow(targetName("default", "local")));
  await waitFor(() => assert.equal(screen.queryAllByText(text("storageGone")).length, 0));
  assert.deepEqual(requested(), { storage_id: 1 });
});

test("weightStorageInvalid only flags a storage the person picked", () => {
  const offered = acq({ targets: [localTarget, scratchTarget] });
  assert.equal(weightStorageInvalid(offered, { storage_id: 99, storage_picked: true }), true);
  assert.equal(weightStorageInvalid(offered, { storage_id: 99 }), false, "a stale id nobody picked is not flagged");
  assert.equal(weightStorageInvalid(offered, { storage_id: 3, storage_picked: true }), false);
  assert.equal(weightStorageInvalid(acq({ targets: [localTarget] }), { storage_id: 99, storage_picked: true }), true, "fewer targets must not erase an invalid explicit choice");
  assert.equal(weightStorageInvalid(offered, undefined), false);
});

// ---------------------------------------------------------------- states

test("before an answer and after a failed one the row asks nothing", () => {
  const view = mount(undefined);
  assert.equal(view.container.textContent, "");
  assert.equal(requested(), undefined);
});

test("the captured evaluate fixtures ask what they offer: two weight sources, one target", async () => {
  const read = (name: string) =>
    JSON.parse(fs.readFileSync(new URL(`./fixtures/evaluate/${name}`, import.meta.url), "utf8")).acquisition as AcquisitionResult;
  const vllm = read("template_vllm.json");
  assert.equal(vllm.targets?.length, 1);
  assert.equal(vllm.providers?.length, 2);
  const view = mount(vllm);
  assert.ok(screen.getByRole("combobox", { name: text("modelProvider") }));
  assert.equal(screen.queryAllByText(text("saveTo")).length, 0);
  await waitFor(() => assert.deepEqual(requested(), { provider: "modelscope" }));
  view.unmount();
  const auto = read("auto_placement.json");
  const single = mount(auto);
  assert.equal(single.container.textContent, "", "one source and one target: nothing to ask");
  assert.equal(requested(), undefined);
});

// ---------------------------------------------------------------- the create waits for a new choice

test("a create is not sent while the storage the person picked is no longer offered", async () => {
  const { renderHook, act } = await import("@testing-library/react");
  const { default: useDeploySubmit } = await import("../src/pages/deploy/useDeploySubmit.ts");
  const { api } = await import("../src/api.ts");
  const created: unknown[] = [];
  const original = api.createDeployment;
  api.createDeployment = (async (body: unknown) => { created.push(body); return { id: 1 }; }) as never;
  try {
    const acquisition = acq({ providers: [huggingface, modelscope], targets: [localTarget, nfsTarget] });
    const hook = renderHook(() => {
      const [instance] = Form.useForm();
      const submit = useDeploySubmit({
        form: instance, nodes: [], evalErr: null, evaluating: false, onCreated: () => {},
        evalResult: { ok: true, acquisition } as never,
      });
      return { instance, ...submit };
    }, { wrapper: ({ children }) => <I18nextProvider i18n={i18n}>{children}</I18nextProvider> });
    const base = { model_id: 3, name: "demo", node_uuid: "n1", node_selection_mode: "manual" };
    // The picked storage 9 is gone: nothing is created, nobody is substituted.
    hook.result.current.instance.setFieldsValue({ ...base, model_source: { provider: "huggingface", storage_id: 9, storage_picked: true } });
    await act(async () => { await hook.result.current.submit(); });
    assert.equal(created.length, 0);
    assert.ok(await screen.findByText(text("storageGone")), "the toast tells the person to choose again");
    // A choice the evaluation offers goes through, carrying exactly the two answers.
    hook.result.current.instance.setFieldsValue({ model_source: { provider: "huggingface", storage_id: 7, storage_picked: true } });
    await act(async () => { await hook.result.current.submit(); });
    assert.equal(created.length, 1);
    const body = created[0] as Record<string, unknown>;
    assert.deepEqual(body.model_source, { provider: "huggingface", storage_id: 7 });
    assert.equal(body.image_pull, undefined);
  } finally {
    api.createDeployment = original;
  }
});

test("the evaluation request carries the chosen storage and provider so the server plans against them", () => {
  const base = { engine: "vllm", model_id: 4 };
  assert.equal(buildPayload(base).payload.model_source, undefined, "nothing chosen: nothing is sent");
  assert.deepEqual(buildPayload({ ...base, model_source: { storage_id: 2 } }).payload.model_source, { storage_id: 2 });
  assert.deepEqual(
    buildPayload({ ...base, model_source: { provider: "modelscope", storage_id: "3" } }).payload.model_source,
    { provider: "modelscope", storage_id: 3 },
  );
  assert.equal(buildPayload({ ...base, model_source: { provider: " ", storage_id: "" } }).payload.model_source, undefined);
  // Legacy form state from before the pull settings is not forwarded.
  assert.deepEqual(
    buildPayload({ ...base, model_source: { kind: "public", provider: "huggingface", network: "direct||", source_node: "n" }, image_pull: { kind: "public" } }).payload,
    { engine: "vllm", model_id: 4, model_source: { provider: "huggingface" } },
  );
});
