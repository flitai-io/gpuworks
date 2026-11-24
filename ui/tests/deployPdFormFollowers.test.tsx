import { assert, test, fireEvent, screen, waitFor, within, userEvent, dom, deployLocale, errorText, stub, baseAnswer, pdRank, pdEvaluate, openDeployPage, choosePdTopology, selectOption, pickPdNode } from "./helpers/deployPdForm.tsx";
// The PD deploy form, part two: a role's PP and a unit's follower nodes, in the placement.pd payload
// and in evaluate's preview. The shared fixture is in helpers/deployPdForm.tsx.

test("setting a role's PP and adding a node to a unit sends both in placement.pd", async () => {
  const payloads: Array<Record<string, unknown>> = [];
  stub("evaluate", async (payload: Record<string, unknown>) => {
    payloads.push(payload);
    return { ...baseAnswer };
  });
  await openDeployPage();
  await choosePdTopology();

  const decodeNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node$`),
  });
  await selectOption(decodeNode, "GPU Node 1 · 2 GPU");

  const ppInput = screen.getByRole("spinbutton", {
    name: deployLocale.pd.ppAria.replace("{{role}}", deployLocale.pd.role.decode),
  });
  await userEvent.setup({ document: dom.window.document }).type(ppInput, "2");

  const addNodeText = deployLocale.pd.addNode
    .replace("{{role}}", deployLocale.pd.role.decode)
    .replace("{{index}}", "1");
  fireEvent.click((await screen.findByText(addNodeText)).closest("button") as HTMLElement);
  await screen.findByText(
    deployLocale.pd.multiNodeTpHint.replace("{{role}}", deployLocale.pd.role.decode),
  );
  const followerNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node 1$`),
  });
  await selectOption(followerNode, "GPU Node 2 · 2 GPU");

  await waitFor(() => {
    const last = payloads.at(-1) as { placement?: { pd?: { roles: Record<string, { pp?: number; units: Array<{ followers?: unknown[] }> }> } } };
    const decode = last.placement?.pd?.roles.decode;
    assert.ok(decode, "the draft has a decode role once a node is picked");
    assert.equal(decode!.pp, 2);
    assert.deepEqual(decode!.units[0].followers, [{ node_uuid: "node-2" }]);
  });
});

test("evaluate's PD preview shows a role's PP and lists a unit's followers, each with its own command", async () => {
  const follower = pdRank("node-2", "GPU Node 2", ["/model", "--role", "decode", "--headless"]);
  stub("evaluate", pdEvaluate([
    { role: "decode", units: [pdRank("node-1", "GPU Node 1", ["/model", "--role", "decode"], { followers: [follower] })] },
    { role: "prefill", units: [pdRank("node-2", "GPU Node 2", ["/model", "--role", "prefill"], { gpus: [1] })] },
  ]));
  await openDeployPage();
  await choosePdTopology();
  await pickPdNode("decode", "GPU Node 1 · 2 GPU");
  await userEvent.setup({ document: dom.window.document }).type(
    screen.getByRole("spinbutton", { name: deployLocale.pd.ppAria.replace("{{role}}", deployLocale.pd.role.decode) }),
    "2",
  );
  const addNodeText = deployLocale.pd.addNode
    .replace("{{role}}", deployLocale.pd.role.decode)
    .replace("{{index}}", "1");
  fireEvent.click((await screen.findByText(addNodeText)).closest("button") as HTMLElement);
  await screen.findByText(deployLocale.pd.multiNodeTpHint.replace("{{role}}", deployLocale.pd.role.decode));
  await pickPdNode("decode", "GPU Node 2 · 2 GPU", 1);
  await pickPdNode("prefill", "GPU Node 2 · 2 GPU");

  const preview = within(await screen.findByTestId("pd-preview"));
  await preview.findByText(`${deployLocale.pd.role.decode} · TP 1 · PP 2`);
  assert.ok(preview.getByText(`${deployLocale.pd.role.prefill} · TP 1`));
  assert.ok(preview.getByText(deployLocale.pd.followersLabel));
  assert.ok(preview.getByText("GPU Node 1"));
  assert.equal(preview.getAllByText("GPU Node 2").length, 2, "the follower rank and the prefill instance both run on GPU Node 2");
  assert.ok(preview.getByText("vllm serve /model --role decode"));
  assert.ok(preview.getByText("vllm serve /model --role decode --headless"), "a follower previews its own headless command");
});
