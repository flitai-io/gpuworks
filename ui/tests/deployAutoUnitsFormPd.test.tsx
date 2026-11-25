import { dom, deployLocale, commonLocale, fireEvent, screen, waitFor, within, userEvent, errorText, baseAnswer, openDeployPage, chooseTopology, pool, segment, selected, node, selectOption, lastOf, recordEvaluate, placementNone, assert, test } from "./helpers/deployAutoUnitsForm.tsx";
// The automatic-placement deploy form, part one: a PD draft opens on automatic, shows the
// server's preview and explanation, and switches to manual. The shared fixture is in
// helpers/deployAutoUnitsForm.tsx.

test("a PD draft opens on automatic: no node or GPU pickers, units counted, payload says auto without a node", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  await chooseTopology("pd");
  await screen.findByText(deployLocale.pd.mode);
  assert.ok(selected(node.auto), "the same control a single node has, on automatic");
  // One line says how automatic placement works; that the list is only a
  // preview sits behind the help mark on that line.
  const unitsLine = screen.getByText(node.autoUnitsDescription);
  assert.equal(screen.queryByText(node.autoUnitsHelp), null);
  await userEvent.setup({ document: dom.window.document }).hover(within(unitsLine).getByRole("button", { name: commonLocale.helpTip.label }));
  assert.equal((await screen.findByRole("tooltip")).textContent, node.autoUnitsHelp);
  assert.equal(screen.queryAllByRole("combobox", { name: /instance 1 node$/ }).length, 0, "no unit node picker");
  assert.equal(screen.queryAllByRole("combobox", { name: /instance 1 GPU/ }).length, 0, "no unit GPU picker");
  assert.equal(screen.queryAllByText(deployLocale.pd.addNode.replace("{{role}}", deployLocale.pd.role.decode).replace("{{index}}", "1")).length, 0, "an automatic unit fits one node");
  // The units are still counted, per role, with the TP the deployer sets.
  fireEvent.click((await screen.findByText(deployLocale.pd.addUnit.replace("{{role}}", deployLocale.pd.role.decode))).closest("button") as HTMLElement);
  await userEvent.setup({ document: dom.window.document }).type(
    screen.getByRole("spinbutton", { name: deployLocale.pd.tpAria.replace("{{role}}", deployLocale.pd.role.decode) }),
    "{backspace}2",
  );
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "auto");
    assert.equal(body.placement_strategy, "binpack", "PD packs by default");
    assert.equal(body.node_uuid, undefined);
    assert.deepEqual(body.placement?.pd?.roles.decode, { tp: 2, units: [{}, {}] });
    assert.deepEqual(body.placement?.pd?.roles.prefill, { tp: 1, units: [{}] });
  });
  assert.ok(screen.getByText(deployLocale.pd.autoUnit.replace("{{role}}", deployLocale.pd.role.decode).replace("{{index}}", "2")));
});

test("the automatic PD preview is the server's pick, titled as a preview; the form keeps its own counts", async () => {
  recordEvaluate(() => ({
    ...baseAnswer,
    auto_placed: true,
    pd: {
      mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail",
      roles: [
        { role: "decode", tp: 1, units: [{ node_uuid: "node-2", node_name: "GPU Node 2", address: "10.0.0.2", fits: true, gpus: [0], argv: [], command: ["vllm"] }] },
        { role: "prefill", tp: 1, units: [{ node_uuid: "node-1", node_name: "GPU Node 1", address: "10.0.0.1", fits: true, gpus: [1], argv: [], command: ["vllm"] }] },
      ],
    },
  }));
  await openDeployPage();
  await chooseTopology("pd");
  const preview = within(await screen.findByTestId("pd-preview"));
  await preview.findByText(deployLocale.pd.previewAutoTitle);
  await preview.findByText("GPU Node 2");
  assert.ok(preview.getByText("GPU Node 1"));
  assert.equal(preview.queryByText(deployLocale.pd.previewTitle), null);
});

test("nothing to place a PD draft on is the server's explanation beside the switch, and the draft stays editable", async () => {
  recordEvaluate(() => ({ ...baseAnswer, ok: false, auto_placed: true, errors: [placementNone] }));
  await openDeployPage();
  await chooseTopology("pd");
  const text = errorText("UNIT_PLACEMENT_NONE", placementNone.params);
  await waitFor(() => assert.ok(pool().getByText(text)));
  assert.ok(pool().getByText(text).closest(".ant-form-item-explain-error"), "rendered as the field's error beside the control");
  // A request automatic placement cannot take reads the same way.
  assert.ok(screen.getByText(deployLocale.pd.addUnit.replace("{{role}}", deployLocale.pd.role.decode)));
});

test("switching a PD draft to manual brings the node pickers back and the payload names the nodes", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  await chooseTopology("pd");
  await screen.findByText(deployLocale.pd.mode);
  fireEvent.click(segment(node.manual));
  await screen.findByText(node.manualUnitsDescription);
  assert.ok(selected(node.manual));
  await selectOption(screen.getByRole("combobox", { name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node$`) }), "GPU Node 1 · 2 GPU");
  await selectOption(screen.getByRole("combobox", { name: new RegExp(`^${deployLocale.pd.role.prefill} instance 1 node$`) }), "GPU Node 2 · 2 GPU");
  assert.ok(screen.getAllByRole("combobox", { name: /instance 1 GPU/ }).length >= 2, "cards can be pinned by hand");
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, undefined, "a PD request naming its nodes does not say auto");
    assert.equal(body.placement_strategy, undefined);
    assert.deepEqual(body.placement?.pd?.roles.decode.units, [{ node_uuid: "node-1" }]);
    assert.deepEqual(body.placement?.pd?.roles.prefill.units, [{ node_uuid: "node-2" }]);
  });
  // And back to automatic: the nodes picked by hand are no longer stated.
  fireEvent.click(segment(node.auto));
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "auto");
    assert.deepEqual(body.placement?.pd?.roles.decode.units, [{}]);
  });
});
