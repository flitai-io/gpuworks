import { dom, deployLocale, fireEvent, screen, waitFor, within, userEvent, errorText, nodeOne, nodeTwo, stubFleet, baseAnswer, openDeployPage, chooseTopology, pool, segment, selected, node, selectOption, lastOf, recordEvaluate, placementNone, assert, test } from "./helpers/deployAutoUnitsForm.tsx";
// The automatic-placement deploy form, part two: a multi-node draft opens on automatic, shows
// the server's rank preview and refusals, and switches to manual. The shared fixture is in
// helpers/deployAutoUnitsForm.tsx.

test("a multi-node draft opens on automatic: only the node count and TP are stated, the nodes are a preview", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  await chooseTopology("multi");
  await screen.findByText(node.multiAutoTitle);
  assert.ok(selected(node.auto));
  assert.ok(screen.getByText(node.autoUnitsDescription));
  assert.equal(screen.queryByPlaceholderText(node.multiPlaceholder), null, "no per-rank node picker");
  assert.ok(screen.getByText(deployLocale.workbench.parallel.tpRequiredAuto), "automatic placement asks for a TP");
  await userEvent.setup({ document: dom.window.document }).type(screen.getByLabelText(deployLocale.workbench.resource.tp), "4");
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "auto");
    assert.equal(body.placement_strategy, "binpack", "a multi-node group packs by default");
    assert.equal(body.nnodes, 2);
    assert.equal(body.node_uuids, undefined);
    assert.equal(body.node_uuid, undefined);
    assert.deepEqual(body.placement, { tp: 4 });
  });
  assert.equal(screen.queryByText(deployLocale.workbench.parallel.tpRequiredAuto), null);
});

test("the automatic multi-node preview lists the server's ranks as a read-only preview", async () => {
  recordEvaluate(() => ({
    ...baseAnswer,
    auto_placed: true,
    parallelism: { tp: 4, pp: 1, nodes: 2, gpus_per_node: 2 },
    preflight: { ok: true },
    rank_commands: [
      { node_uuid: "node-2", node_name: "GPU Node 2", node_rank: 0, role: "leader", headless: false, image: "runner:vllm", argv: [] },
      { node_uuid: "node-1", node_name: "GPU Node 1", node_rank: 1, role: "follower", headless: true, image: "runner:vllm", argv: [] },
    ],
  }));
  await openDeployPage();
  await chooseTopology("multi");
  const preview = within(await screen.findByTestId("auto-group-preview"));
  assert.ok(preview.getByText("GPU Node 2"));
  assert.ok(preview.getByText("GPU Node 1"));
  assert.ok(preview.getByText("Master"));
  assert.ok(preview.getByText("Worker"));
  assert.ok(screen.getByText(node.previewTitle));
  assert.equal(document.querySelector('[data-testid=auto-group-preview] [role="combobox"]'), null, "nothing to pick in a preview");
});

test("a multi-node draft that cannot be placed shows the server's reason, in the user's words", async () => {
  recordEvaluate(() => ({ ...baseAnswer, ok: false, auto_placed: true, errors: [placementNone] }));
  await openDeployPage();
  await chooseTopology("multi");
  await waitFor(() => assert.ok(pool().getByText(errorText("UNIT_PLACEMENT_NONE", placementNone.params))));
  await screen.findByText(node.autoGroupNoPreview);
});

test("a missing TP is the server's AUTO_PLACEMENT_TP_REQUIRED beside the switch", async () => {
  recordEvaluate(() => ({
    ...baseAnswer, ok: false, auto_placed: true,
    errors: [{ field: "placement_mode", code: "AUTO_PLACEMENT_TP_REQUIRED" }],
  }));
  await openDeployPage();
  await chooseTopology("multi");
  await waitFor(() => assert.ok(pool().getByText(errorText("AUTO_PLACEMENT_TP_REQUIRED"))));
});

test("switching a multi-node draft to manual shows the node picker and states the nodes", async () => {
  const payloads = recordEvaluate();
  // A rank must advertise an address the group can reach.
  stubFleet([{ ...(nodeOne as object), management_address: "10.0.0.1:9443" }, { ...(nodeTwo as object), management_address: "10.0.0.2:9443" }]);
  await openDeployPage();
  await chooseTopology("multi");
  await screen.findByText(node.multiAutoTitle);
  fireEvent.click(segment(node.manual));
  const picker = await screen.findByPlaceholderText(node.multiPlaceholder);
  assert.ok(screen.getByText(node.manualUnitsDescription));
  await selectOption(picker, "GPU Node 1 · 2 GPU");
  await selectOption(picker, "GPU Node 2 · 2 GPU");
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "manual");
    assert.deepEqual(body.node_uuids, ["node-1", "node-2"]);
    assert.equal(body.placement_strategy, undefined);
  });
});
