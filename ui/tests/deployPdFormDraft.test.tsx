import { assert, test, fireEvent, screen, waitFor, within, userEvent, dom, deployLocale, errorText, stub, baseAnswer, pdRank, pdEvaluate, openDeployPage, choosePdTopology, selectOption, pickPdNode } from "./helpers/deployPdForm.tsx";
// The PD deploy form, part one: choosing "PD disaggregation" builds placement.pd from the role
// sections, role instance rows are added and removed (by keyboard too), and a field error the
// server attributes to "placement.pd" renders beside the section. The shared fixture is in
// helpers/deployPdForm.tsx.

test("choosing PD disaggregation builds placement.pd from the role sections, and leaves it out otherwise", async () => {
  const payloads: Array<Record<string, unknown>> = [];
  stub("evaluate", async (payload: Record<string, unknown>) => {
    payloads.push(payload);
    return { ...baseAnswer };
  });

  await openDeployPage();
  await waitFor(() => assert.ok(payloads.length > 0));
  assert.ok(
    payloads.every((payload) => !(payload.placement as Record<string, unknown> | undefined)?.pd),
    "an ordinary draft never sends placement.pd",
  );

  await choosePdTopology();

  const decodeNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node$`),
  });
  await selectOption(decodeNode, "GPU Node 1 · 2 GPU");
  const prefillNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.prefill} instance 1 node$`),
  });
  await selectOption(prefillNode, "GPU Node 2 · 2 GPU");

  await waitFor(() => {
    const last = payloads.at(-1) as { placement?: { pd?: Record<string, unknown> } };
    const pd = last.placement?.pd as
      | { mode: string; fallback: string; kv_load_failure: string; roles: Record<string, { units: Array<Record<string, unknown>> }> }
      | undefined;
    assert.ok(pd, "the draft has a placement.pd once nodes are picked");
    assert.equal(pd!.mode, "vllm-nixl");
    assert.equal(pd!.fallback, "decode");
    assert.equal(pd!.kv_load_failure, "fail");
    assert.deepEqual(pd!.roles.decode.units, [{ node_uuid: "node-1" }]);
    assert.deepEqual(pd!.roles.prefill.units, [{ node_uuid: "node-2" }]);
  });

  // Switching the replica shape away drops placement.pd from the next draft:
  // the ordinary single/multi-node fields take over again.
  const singleCard = screen.getByText(deployLocale.workbench.node.single);
  fireEvent.click(singleCard);
  await waitFor(() => {
    const last = payloads.at(-1) as { placement?: { pd?: unknown } };
    assert.equal(last.placement?.pd, undefined);
  });
});

test("add and remove PD role instance rows, including by keyboard, with a floor of one row", async () => {
  stub("evaluate", async (payload: Record<string, unknown>) => ({ ...baseAnswer, __payload: payload } as never));
  await openDeployPage();
  await choosePdTopology();

  const addDecodeText = deployLocale.pd.addUnit.replace("{{role}}", deployLocale.pd.role.decode);
  const addDecode = (await screen.findByText(addDecodeText)).closest("button") as HTMLElement;
  const rowCount = () => document.querySelectorAll(".deploy-pd-unit-row").length;
  const decodeRows = () =>
    [...document.querySelectorAll(".deploy-pd-unit-row")].filter((row) =>
      row.querySelector(`[aria-label^="${deployLocale.pd.role.decode} instance"]`),
    );
  assert.equal(decodeRows().length, 1);

  fireEvent.click(addDecode);
  await waitFor(() => assert.equal(decodeRows().length, 2));

  // The second row's remove button is a plain <button>; Tab to it and press
  // Enter, the way a keyboard-only operator would remove a row.
  const removeSecond = screen.getByRole("button", {
    name: deployLocale.pd.removeUnit
      .replace("{{role}}", deployLocale.pd.role.decode)
      .replace("{{index}}", "2"),
  });
  removeSecond.focus();
  assert.equal(document.activeElement, removeSecond);
  await userEvent.setup({ document: dom.window.document }).keyboard("{Enter}");
  await waitFor(() => assert.equal(decodeRows().length, 1));

  // The floor of one instance per role: the sole remaining row's remove
  // button is disabled.
  const soleRemove = screen.getByRole("button", {
    name: deployLocale.pd.removeUnit
      .replace("{{role}}", deployLocale.pd.role.decode)
      .replace("{{index}}", "1"),
  });
  assert.ok(soleRemove.hasAttribute("disabled"));
  void rowCount;
});

test("a field error the server attributes to placement.pd renders beside the PD section", async () => {
  const overlapParams = { node: "node-1", gpu: 0, role: "decode", other: "prefill" };
  let attempt = 0;
  stub("evaluate", async () => {
    attempt += 1;
    return {
      ...baseAnswer,
      ok: attempt > 1,
      errors:
        attempt > 1
          ? [{ field: "placement.pd", code: "PD_GPU_OVERLAP", params: overlapParams }]
          : [],
    };
  });
  await openDeployPage();
  await choosePdTopology();

  const decodeNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node$`),
  });
  await selectOption(decodeNode, "GPU Node 1 · 2 GPU");

  const message = errorText("PD_GPU_OVERLAP", overlapParams);
  const pdSection = within(screen.getByTestId("pd-section"));
  await waitFor(() => {
    assert.ok(pdSection.queryAllByRole("alert").some((alert) => alert.textContent?.includes(message)));
  });
});
