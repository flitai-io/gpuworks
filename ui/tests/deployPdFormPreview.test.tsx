import { assert, test, fireEvent, screen, waitFor, within, userEvent, dom, deployLocale, errorText, stub, baseAnswer, pdRank, pdEvaluate, openDeployPage, choosePdTopology, selectOption, pickPdNode } from "./helpers/deployPdForm.tsx";
// The PD deploy form, part three: evaluate's per-unit preview (a fitting and a non-fitting unit) and
// a re-evaluation withholding the previous preview. The shared fixture is in helpers/deployPdForm.tsx.

test("evaluate's PD preview shows a fitting and a non-fitting unit, each with its command", async () => {
  const decode = pdRank("node-1", "GPU Node 1", ["/model", "--role", "decode"]);
  const prefill = pdRank("node-2", "GPU Node 2", ["/model", "--role", "prefill"], {
    fits: false,
    gpus: undefined,
    reason: "not enough VRAM on GPU Node 2",
  });
  stub("evaluate", pdEvaluate([
    { role: "decode", units: [decode] },
    { role: "prefill", units: [prefill] },
  ]));
  await openDeployPage();
  await choosePdTopology();
  await pickPdNode("decode", "GPU Node 1 · 2 GPU");
  await pickPdNode("prefill", "GPU Node 2 · 2 GPU");

  const preview = within(await screen.findByTestId("pd-preview"));
  await preview.findByText(deployLocale.pd.unitFits);
  assert.ok(preview.getByText(deployLocale.pd.unitDoesNotFit));
  assert.ok(preview.getByText("not enough VRAM on GPU Node 2"));
  assert.ok(preview.getByText("GPU Node 1"));
  assert.ok(preview.getByText("GPU Node 2"));
  assert.ok(preview.getByText("vllm serve /model --role decode"));
  assert.ok(preview.getByText("vllm serve /model --role prefill"), "a unit that does not fit still previews the command it would run");
  // The runtime command section answers the same evaluation, rank by rank.
  for (const label of ["Decode #1 · GPU Node 1", "Prefill #1 · GPU Node 2"]) {
    assert.ok(screen.getByRole("button", { name: deployLocale.catalog.copyRank.replace("{{rank}}", label) }));
  }
});

test("a re-evaluation withholds the previous PD preview until its own answer lands", async () => {
  let gate: Promise<void> | undefined;
  let release = () => {};
  stub("evaluate", pdEvaluate(
    [
      { role: "decode", units: [pdRank("node-1", "GPU Node 1", ["/model", "--role", "decode"])] },
      { role: "prefill", units: [pdRank("node-2", "GPU Node 2", ["/model", "--role", "prefill"])] },
    ],
    () => gate ?? Promise.resolve(),
  ));
  await openDeployPage();
  await choosePdTopology();
  await pickPdNode("decode", "GPU Node 1 · 2 GPU");
  await pickPdNode("prefill", "GPU Node 2 · 2 GPU");
  const preview = within(await screen.findByTestId("pd-preview"));
  await preview.findByText("vllm serve /model --role decode");

  gate = new Promise((resolve) => { release = resolve; });
  await userEvent.setup({ document: dom.window.document }).type(
    screen.getByRole("spinbutton", { name: deployLocale.pd.ppAria.replace("{{role}}", deployLocale.pd.role.decode) }),
    "2",
  );
  await preview.findByText(deployLocale.catalog.evaluationPending);
  assert.equal(preview.queryByText(deployLocale.pd.unitFits), null, "no fit verdict from the previous draft");
  assert.equal(preview.queryByText("vllm serve /model --role decode"), null, "no command from the previous draft");
  assert.ok(screen.getByText(deployLocale.runtimeCommand.generating));

  release();
  await preview.findByText(/PP 2/);
  assert.ok(preview.getByText("vllm serve /model --role decode"));
  assert.equal(preview.queryByText(deployLocale.catalog.evaluationPending), null);
});
