import { deployLocale, i18n, MemoryRouter, fireEvent, render, screen, waitFor, nodeOne, nodeTwo, stub, stubFleet, baseAnswer, openDeployPage, chooseTopology, segment, selected, node, lastOf, recordEvaluate, assert, test, I18nextProvider, React } from "./helpers/deployAutoUnitsForm.tsx";
// The automatic-placement deploy form, part three: the placement strategy default, the
// single-node switch, the mixed-GPU-model warning, and clones of stored multi-node and PD
// deployments. The shared fixture is in helpers/deployAutoUnitsForm.tsx.

test("the strategy follows the topology's default unless the deployer picked one", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  const strategy = () => (lastOf(payloads).placement_strategy);
  await waitFor(() => assert.equal(strategy(), "spread"));
  await chooseTopology("multi");
  await waitFor(() => assert.equal(strategy(), "binpack"));
  await chooseTopology("single");
  await waitFor(() => assert.equal(strategy(), "spread"));
  // An explicit pick survives the switch.
  fireEvent.click(await screen.findByText(node.strategyBinpack));
  await chooseTopology("multi");
  await chooseTopology("single");
  await waitFor(() => assert.equal(strategy(), "binpack"));
});

test("the single-node switch is unchanged: automatic spreads, manual pins one node", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  await waitFor(() => assert.ok(payloads.length > 0));
  assert.ok(selected(node.auto));
  assert.ok(screen.getByText(node.autoPoolDescription));
  assert.equal(lastOf(payloads).placement_mode, "auto");
  fireEvent.click(segment(node.manual));
  await screen.findByText(node.manualPoolDescription);
  await waitFor(() => assert.equal(lastOf(payloads).placement_mode, "manual"));
});

test("a manual PD draft with units on different GPU models shows the server's warning", async () => {
  recordEvaluate(() => ({
    ...baseAnswer,
    warnings: [{ code: "PD_GPU_MODELS_DIFFER", params: { models: "H100, L20" } }],
  }));
  await openDeployPage();
  await chooseTopology("pd");
  fireEvent.click(segment(node.manual));
  await screen.findByText(node.manualUnitsDescription);
  await screen.findByText(deployLocale.warning.PD_GPU_MODELS_DIFFER.replace("{{models}}", "H100, L20"));
});

// ---- a clone of an existing multi-node or PD deployment ---------------------

async function openFrom(draft: Record<string, unknown>, deployment: Record<string, unknown>) {
  stub("deploymentDraft", async () => ({ deployment: draft, revision: "r1", locked: [] }));
  stub("deployment", async () => deployment);
  const DeployPage = (await import("../src/pages/DeployPage.tsx")).default;
  render(
    <MemoryRouter initialEntries={["/deploy?from=5"]}>
      <I18nextProvider i18n={i18n}>
        <DeployPage />
      </I18nextProvider>
    </MemoryRouter>,
  );
  await waitFor(() => assert.ok(document.querySelector(".deploy-node-pool")));
}

const storedMulti = {
  model_id: 3, name: "big", served_name: "big", engine: "vllm", target_vendor: "nvidia", nnodes: 2, replicas: 1,
  node_uuid: "node-1", node_uuids: ["node-1", "node-2"], placement: { tp: 4 },
};
const storedDeployment = { id: 5, name: "big", model_id: 3, model_name: "m", node_uuid: "node-1", desired_state: "active", runtime_state: "running", replicas: 1, nnodes: 2, template_deviated: false, health: {}, created_at: "2026-10-01T00:00:00Z" };

test("a clone of a multi-node deployment the platform places opens on automatic with no picker", async () => {
  recordEvaluate();
  await openFrom(storedMulti, { ...storedDeployment, placement_mode: "auto" });
  await waitFor(() => assert.ok(selected(node.auto)));
  assert.ok(screen.getByText(node.autoUnitsDescription));
  assert.equal(screen.queryByPlaceholderText(node.multiPlaceholder), null);
});

test("a stored draft that does not say who places it reads as manual, with its nodes", async () => {
  const payloads = recordEvaluate();
  stubFleet([{ ...(nodeOne as object), management_address: "10.0.0.1:9443" }, { ...(nodeTwo as object), management_address: "10.0.0.2:9443" }]);
  await openFrom(storedMulti, { ...storedDeployment, placement_mode: "manual" });
  await waitFor(() => assert.ok(selected(node.manual)));
  await screen.findByText("GPU Node 2");
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "manual");
    assert.deepEqual(body.node_uuids, ["node-1", "node-2"]);
  });
});

test("a stored PD draft with no placement_mode reads as manual: its units keep their nodes", async () => {
  const payloads = recordEvaluate();
  const pd = { mode: "vllm-nixl", roles: { decode: { tp: 1, units: [{ node_uuid: "node-1" }] }, prefill: { tp: 1, units: [{ node_uuid: "node-2" }] } } };
  await openFrom({ ...storedMulti, nnodes: 1, node_uuid: "node-1", node_uuids: undefined, placement: { pd } }, { ...storedDeployment, nnodes: 1 });
  await waitFor(() => assert.ok(selected(node.manual)));
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, undefined);
    assert.deepEqual(body.placement?.pd?.roles.decode.units, [{ node_uuid: "node-1" }]);
  });
});
