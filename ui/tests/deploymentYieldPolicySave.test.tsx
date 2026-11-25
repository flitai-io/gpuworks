import assert from "node:assert/strict";
import { mock, test } from "node:test";
import * as shapes from "./helpers/deploymentCapabilities.ts";
import {
  APIError, api, yieldPolicyApi, message, choosePolicy, cleanup, defaults, deployment, f, fireEvent, i18n, mount, openEditor, p, saveButton, screen, serverAnswer, spin, state, stub, summaryOf, summaryText, userEvent, waitFor, withPolicy, within, y,
  type Editor, type YieldPolicyRequest,
} from "./helpers/yieldPolicyHarness.tsx";

// What the yield policy editor saves and how it answers: the request as literal
// JSON, success, the idle-neighbours warning, a save in flight, the bounds, the
// server's refusals, the deployments that cannot yield or sleep, a read-only
// user and the keyboard.

const radioOf = (editor: Editor, name: string) => editor.getByRole("radio", { name }) as HTMLInputElement;

test("saving shows success, reloads the deployment, restarts the editor from what is saved and updates the summary", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  await choosePolicy(user, editor, "timeslice");
  await user.click(saveButton(editor));
  await waitFor(() => assert.equal(state.sent.length, 1));
  await waitFor(() => assert.equal(state.changed, 1));
  assert.ok((await screen.findAllByText(y.saved)).length > 0);
  await waitFor(() => assert.ok((screen.getByRole("region", { name: p.title }).textContent ?? "").includes(summaryOf("Yield on a turn · idle 30 s / up to 2 min per turn"))));
  const again = within(await screen.findByRole("region", { name: y.title }));
  await again.findByRole("combobox", { name: y.policyLabel });
  assert.equal(spin(again, f.max_serve_sec)?.value, "120");
  assert.equal(saveButton(again).disabled, true, "what is saved is not a change");
});

test("what is saved is the policy with the parameters that apply, as literal JSON", async () => {
  stub();
  const cases: Array<[string, (user: ReturnType<typeof userEvent.setup>, editor: Editor) => Promise<void>, YieldPolicyRequest]> = [
    ["idle with its defaults", async (user, editor) => { await choosePolicy(user, editor, "idle"); },
      { policy: "idle", idle_sec: 60, sleep_level: "auto", hold_max_sec: 300 }],
    ["timeslice with its defaults", async (user, editor) => { await choosePolicy(user, editor, "timeslice"); },
      { policy: "timeslice", idle_sec: 30, sleep_level: "auto", max_serve_sec: 120, unfinished: "wait", unfinished_wait_sec: 30, hold_max_sec: 600 }],
    ["anytime with its defaults carries no wait time", async (user, editor) => { await choosePolicy(user, editor, "anytime"); },
      { policy: "anytime", idle_sec: 30, sleep_level: "auto", unfinished: "abort", hold_max_sec: 3600 }],
    ["timeslice with edited values", async (user, editor) => {
      await choosePolicy(user, editor, "timeslice");
      fireEvent.change(spin(editor, f.idle_sec) as HTMLInputElement, { target: { value: "45" } });
      fireEvent.change(spin(editor, f.max_serve_sec) as HTMLInputElement, { target: { value: "30" } });
      fireEvent.change(spin(editor, f.unfinished_wait_sec) as HTMLInputElement, { target: { value: "600" } });
      fireEvent.change(spin(editor, f.hold_max_sec) as HTMLInputElement, { target: { value: "10" } });
      await user.click(editor.getByRole("combobox", { name: f.sleep_level }));
      await user.click(await screen.findByText(y.sleepLevel.l1));
    }, { policy: "timeslice", idle_sec: 30, sleep_level: "l1", max_serve_sec: 30, unfinished: "wait", unfinished_wait_sec: 600, hold_max_sec: 10 }],
    ["abort leaves the wait time out", async (user, editor) => {
      await choosePolicy(user, editor, "timeslice");
      await user.click(editor.getByRole("radio", { name: y.unfinished.abort }));
    }, { policy: "timeslice", idle_sec: 30, sleep_level: "auto", max_serve_sec: 120, unfinished: "abort", hold_max_sec: 600 }],
    ["parameters that stop applying are left out", async (user, editor) => {
      await choosePolicy(user, editor, "timeslice");
      fireEvent.change(spin(editor, f.max_serve_sec) as HTMLInputElement, { target: { value: "500" } });
      await choosePolicy(user, editor, "anytime");
    }, { policy: "anytime", idle_sec: 30, sleep_level: "auto", unfinished: "abort", hold_max_sec: 3600 }],
  ];
  for (const [name, act, want] of cases) {
    stub();
    const user = mount();
    const editor = await openEditor(user);
    await act(user, editor);
    // idle_sec 45 is above the cap of the 30 s max_serve case: the form would refuse it.
    if (name === "timeslice with edited values") fireEvent.change(spin(editor, f.idle_sec) as HTMLInputElement, { target: { value: "30" } });
    await user.click(saveButton(editor));
    await waitFor(() => assert.equal(state.sent.length, 1));
    assert.deepEqual(state.sent[0][1], want, name);
    assert.equal(state.sent[0][0], 1);
    for (const gone of ["preset", "auto_sleep", "min_serve_sec"]) assert.equal(gone in state.sent[0][1], false, `${name}: ${gone}`);
    cleanup();
  }
  // Going back to never sends the policy alone.
  stub();
  const user = mount(withPolicy(defaults.idle));
  const editor = await openEditor(user);
  await choosePolicy(user, editor, "never");
  await user.click(saveButton(editor));
  await waitFor(() => assert.deepEqual(state.sent, [[1, { policy: "never" }]]));
});

test("the answer's idle neighbours are warned about after the save, in the user's language, and a clean answer is not", async () => {
  const warn = mock.method(message, "warning");
  try {
    stub();
    state.neighbours = ["chat-b", "chat-c"];
    const user = mount();
    const editor = await openEditor(user);
    await choosePolicy(user, editor, "idle");
    await user.click(saveButton(editor));
    await waitFor(() => assert.equal(state.changed, 1));
    const wording = "It shares GPUs with chat-b, chat-c, and all of them yield when idle: while both are busy neither gives way, and the other's requests are held until they time out. To take turns, switch to yield on a turn.";
    assert.equal(i18n.t("deployments:yieldPolicy.idleNeighbours", { names: "chat-b, chat-c" }), wording);
    await screen.findByText(wording);
    assert.equal(warn.mock.callCount(), 1);
    assert.ok((await screen.findAllByText(y.saved)).length > 0, "it is saved all the same");
    // The save itself was not refused: the server allows it.
    assert.deepEqual(state.current.yield_policy, defaults.idle);
    cleanup();

    warn.mock.resetCalls();
    stub();
    state.neighbours = [];
    const user2 = mount();
    const editor2 = await openEditor(user2);
    await choosePolicy(user2, editor2, "idle");
    await user2.click(saveButton(editor2));
    await waitFor(() => assert.equal(state.changed, 1));
    assert.equal(warn.mock.callCount(), 0, "no neighbours, no warning");
  } finally {
    warn.mock.restore();
  }
});

test("a save in flight locks the form and shows it is working", async () => {
  stub();
  let finish: () => void = () => {};
  yieldPolicyApi.set = (id, body) => new Promise((resolve) => {
    state.sent.push([id, body]);
    finish = () => { const answer = serverAnswer(body); state.current = { ...state.current, yield_policy: answer }; resolve(answer); };
  });
  const user = mount();
  const editor = await openEditor(user);
  await choosePolicy(user, editor, "idle");
  await user.click(saveButton(editor));
  await waitFor(() => assert.equal(state.sent.length, 1));
  assert.ok(saveButton(editor).className.includes("ant-btn-loading") || saveButton(editor).disabled);
  assert.equal(spin(editor, f.idle_sec)?.disabled, true);
  assert.equal((editor.getByRole("button", { name: y.restoreDefaults }) as HTMLButtonElement).disabled, true);
  assert.equal(state.changed, 0, "nothing is saved yet");
  finish();
  await waitFor(() => assert.equal(state.changed, 1));
});

test("a value out of its bounds or a cleared one is flagged and cannot be saved", async () => {
  stub();
  const user = mount(withPolicy(defaults.timeslice));
  const editor = await openEditor(user);
  const bounds: Array<[string, string, number, number]> = [
    [f.idle_sec, "14", 15, 120],
    [f.max_serve_sec, "29", 30, 86400],
    [f.max_serve_sec, "86401", 30, 86400],
    [f.unfinished_wait_sec, "0", 1, 600],
    [f.hold_max_sec, "3601", 10, 3600],
  ];
  for (const [label, bad, minimum, maximum] of bounds) {
    const input = spin(editor, label) as HTMLInputElement;
    const good = input.value;
    fireEvent.change(input, { target: { value: bad } });
    editor.getByText(i18n.t("deployments:yieldPolicy.error.out_of_range", { field: label, minimum, maximum }));
    assert.equal(input.getAttribute("aria-invalid"), "true", label);
    assert.equal(saveButton(editor).disabled, true, label);
    fireEvent.change(input, { target: { value: good } });
    assert.equal(editor.queryByRole("alert"), null, label);
  }
  const idle = spin(editor, f.idle_sec) as HTMLInputElement;
  fireEvent.change(idle, { target: { value: "" } });
  editor.getByText(i18n.t("deployments:yieldPolicy.error.required", { field: f.idle_sec }));
  assert.equal(saveButton(editor).disabled, true);
  await user.click(saveButton(editor));
  assert.deepEqual(state.sent, []);
  // The boundaries are accepted.
  fireEvent.change(idle, { target: { value: "15" } });
  assert.equal(saveButton(editor).disabled, false);
  fireEvent.change(idle, { target: { value: "120" } });
  assert.equal(saveButton(editor).disabled, false);
  // A field that does not apply is not checked.
  await choosePolicy(user, editor, "idle");
  assert.equal(spin(editor, f.max_serve_sec), null);
  assert.equal(saveButton(editor).disabled, false);
});

test("the idle time may not exceed the longest serving time, and the range says what the cap is now", async () => {
  stub();
  const user = mount(withPolicy(defaults.timeslice));
  const editor = await openEditor(user);
  const idle = spin(editor, f.idle_sec) as HTMLInputElement;
  fireEvent.change(idle, { target: { value: "121" } });
  editor.getByText(i18n.t("deployments:yieldPolicy.error.out_of_range", { field: f.idle_sec, minimum: 15, maximum: 120 }));
  assert.equal(saveButton(editor).disabled, true);
  // Raising the longest serving time lifts the cap.
  fireEvent.change(spin(editor, f.max_serve_sec) as HTMLInputElement, { target: { value: "300" } });
  assert.equal(editor.queryByText(i18n.t("deployments:yieldPolicy.error.out_of_range", { field: f.idle_sec, minimum: 15, maximum: 120 })), null);
  assert.equal(saveButton(editor).disabled, false);
  fireEvent.change(idle, { target: { value: "301" } });
  editor.getByText(i18n.t("deployments:yieldPolicy.error.out_of_range", { field: f.idle_sec, minimum: 15, maximum: 300 }));
  // Without the longest serving time in play (the idle policy) only the plain maximum holds.
  await choosePolicy(user, editor, "idle");
  fireEvent.change(spin(editor, f.idle_sec) as HTMLInputElement, { target: { value: "604800" } });
  assert.equal(saveButton(editor).disabled, false);
  fireEvent.change(spin(editor, f.idle_sec) as HTMLInputElement, { target: { value: "604801" } });
  editor.getByText(i18n.t("deployments:yieldPolicy.error.out_of_range", { field: f.idle_sec, minimum: 15, maximum: 604800 }));
});

test("the stub server refuses what the real one does: the old preset body and an idle time above the longest serving time", () => {
  stub();
  assert.throws(() => serverAnswer({ preset: "online" } as never), (error: unknown) => error instanceof APIError && error.status === 400);
  assert.throws(() => serverAnswer({ policy: "timeslice", idle_sec: 200, max_serve_sec: 120 }), (error: unknown) => {
    const refusal = error as APIError;
    return refusal.code === "YIELD_POLICY_INVALID" && refusal.params?.field === "idle_sec" && refusal.params?.reason === "out_of_range" && refusal.params?.maximum === 120;
  });
  assert.deepEqual(serverAnswer({ policy: "anytime" }), defaults.anytime && { policy: "anytime", idle_sec: 30, sleep_level: "auto", unfinished: "abort", hold_max_sec: 3600 });
});

test("the server's refusals are shown in the user's language and nothing is marked saved", async () => {
  stub();
  const refusals: Array<[APIError, string]> = [
    [
      new APIError("YIELD_POLICY_INVALID", { field: "hold_max_sec", reason: "out_of_range", minimum: 10, maximum: 3600 }, "out of range", 400),
      i18n.t("deployments:yieldPolicy.error.out_of_range", { field: f.hold_max_sec, minimum: 10, maximum: 3600 }),
    ],
    [
      // The cap of the idle time is the longest serving time.
      new APIError("YIELD_POLICY_INVALID", { field: "idle_sec", reason: "out_of_range", minimum: 15, maximum: 120 }, "out of range", 400),
      i18n.t("deployments:yieldPolicy.error.out_of_range", { field: f.idle_sec, minimum: 15, maximum: 120 }),
    ],
    [
      new APIError("YIELD_POLICY_INVALID", { field: "max_serve_sec", reason: "required" }, "required", 400),
      i18n.t("deployments:yieldPolicy.error.required", { field: f.max_serve_sec }),
    ],
    [
      new APIError("YIELD_POLICY_INVALID", { field: "unfinished", reason: "invalid_value" }, "bad value", 400),
      i18n.t("deployments:yieldPolicy.error.invalid_value", { field: f.unfinished }),
    ],
    [new APIError("YIELD_POLICY_SHAPE", {}, "single-node only", 400), i18n.t("common:apiError.YIELD_POLICY_SHAPE")],
    [new APIError("SLEEP_NOT_ENABLED", {}, "sleep mode was not enabled", 400), i18n.t("common:apiError.SLEEP_NOT_ENABLED")],
  ];
  for (const [error, detail] of refusals) {
    yieldPolicyApi.set = async () => { throw error; };
    const user = mount();
    const editor = await openEditor(user);
    await choosePolicy(user, editor, "idle");
    await user.click(saveButton(editor));
    const alert = await screen.findByText(i18n.t("deployments:yieldPolicy.saveFailed", { detail }));
    assert.ok(alert, error.code);
    assert.equal(state.changed, 0, error.code);
    // The form stays as it was, ready to be corrected.
    assert.equal(spin(editor, f.idle_sec)?.value, "60");
    assert.ok((await summaryText()).includes(summaryOf("Never yield automatically")), "the summary still shows what is saved");
    // Changing anything clears the refusal.
    fireEvent.change(spin(editor, f.hold_max_sec) as HTMLInputElement, { target: { value: "999" } });
    assert.ok(screen.queryByText(i18n.t("deployments:yieldPolicy.saveFailed", { detail })) === null, error.code);
    cleanup();
  }
});

test("a refusal of the server that the editor did not expect still reads in the user's language (YIELD_POLICY_INVALID, unknown field)", async () => {
  stub();
  yieldPolicyApi.set = async () => { throw new APIError("YIELD_POLICY_INVALID", { field: "something_new", reason: "unheard_of" }, "bad", 400); };
  const user = mount();
  const editor = await openEditor(user);
  await choosePolicy(user, editor, "idle");
  await user.click(saveButton(editor));
  await screen.findByText(i18n.t("deployments:yieldPolicy.saveFailed", { detail: i18n.t("common:apiError.YIELD_POLICY_INVALID", { field: "something_new", reason: "unheard_of" }) }));
});

test("a deployment that cannot sleep says so and offers only the way back to never", async () => {
  stub();
  const user = mount(deployment({ sleepable: false, yield_policy: defaults.timeslice, ...shapes.notSleepableSingleNode }));
  const editor = await openEditor(user);
  editor.getByText(y.notSleepable);
  assert.equal(spin(editor, f.max_serve_sec)?.value, "120", "what it kept is shown");
  await user.click(editor.getByRole("combobox", { name: y.policyLabel }));
  const option = (policy: keyof typeof y.policy.name) => screen.getByText(`${y.policy.name[policy]} - ${y.policy.description[policy]}`).closest(".ant-select-item") as HTMLElement;
  for (const policy of ["idle", "timeslice", "anytime"] as const) assert.ok(option(policy).className.includes("ant-select-item-option-disabled"), policy);
  assert.ok(!option("never").className.includes("ant-select-item-option-disabled"));
  await user.click(option("never"));
  await user.click(saveButton(editor));
  await waitFor(() => assert.deepEqual(state.sent, [[1, { policy: "never" }]]));
});

test("a multi-node or PD deployment can only never yield automatically", async () => {
  stub();
  for (const shape of [{ nnodes: 2, ...shapes.sleepableMultiNodeManual }, { pd: { mode: "ep", roles: [] }, ...shapes.sleepablePDManual }]) {
    const user = mount(deployment(shape));
    const editor = await openEditor(user);
    editor.getByText(y.singleNodeOnly);
    await user.click(editor.getByRole("combobox", { name: y.policyLabel }));
    for (const policy of ["idle", "timeslice", "anytime"] as const) {
      const option = screen.getByText(`${y.policy.name[policy]} - ${y.policy.description[policy]}`).closest(".ant-select-item") as HTMLElement;
      assert.ok(option.className.includes("ant-select-item-option-disabled"), policy);
    }
    cleanup();
  }
  // A single-node deployment that can sleep has no such notice.
  const user = mount();
  const editor = await openEditor(user);
  assert.equal(editor.queryByText(y.singleNodeOnly), null);
  assert.equal(editor.queryByText(y.notSleepable), null);
});

test("the server's eligibility decides, whatever the deployment's own fields say: a refusal disables the policies with its reason", async () => {
  stub();
  // Sleepable and single-node, yet the server refuses: its engine cannot
  // sleep. It kept a policy, so the editor shows, to go back to never.
  const refused = { ok: false, reason: "SLEEP_ENGINE_UNSUPPORTED" };
  const user = mount(deployment({
    engine: "tgi",
    yield_policy: defaults.idle,
    yield_policy_eligible: refused,
    schedule_capabilities: { ...shapes.sleepableSingleNode.schedule_capabilities, sleep: refused, wake: refused },
  }));
  const editor = await openEditor(user);
  editor.getByText(i18n.t("common:apiError.SLEEP_ENGINE_UNSUPPORTED", { engine: "tgi" }));
  await user.click(editor.getByRole("combobox", { name: y.policyLabel }));
  for (const policy of ["idle", "timeslice", "anytime"] as const) {
    const option = screen.getByText(`${y.policy.name[policy]} - ${y.policy.description[policy]}`).closest(".ant-select-item") as HTMLElement;
    assert.ok(option.className.includes("ant-select-item-option-disabled"), policy);
  }
  cleanup();
  // Eligible by the server's word: every policy is offered, no notice.
  const user2 = mount(deployment({ yield_policy_eligible: { ok: true } }));
  const editor2 = await openEditor(user2);
  assert.equal(editor2.queryByText(y.notSleepable), null);
  await user2.click(editor2.getByRole("combobox", { name: y.policyLabel }));
  const idle = screen.getByText(`${y.policy.name.idle} - ${y.policy.description.idle}`).closest(".ant-select-item") as HTMLElement;
  assert.ok(!idle.className.includes("ant-select-item-option-disabled"));
});

test("a read-only user sees the yield policy but cannot edit it", async () => {
  stub();
  state.role = "viewer";
  mount(withPolicy(defaults.idle));
  assert.ok((await summaryText()).includes(summaryOf("Yield when idle · yields after 1 min idle")));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: p.edit }), null));
});

test("the editor is operable from the keyboard: the select, the radios, restore defaults, then save", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  const policy = editor.getByRole("combobox", { name: y.policyLabel });
  policy.focus();
  assert.equal(document.activeElement, policy);
  // The select reads key codes (which jsdom does not fill in for typed keys), so its keys are sent as events.
  const press = (name: "ArrowDown" | "Enter") => fireEvent.keyDown(policy, { key: name, code: name, keyCode: name === "Enter" ? 13 : 40, which: name === "Enter" ? 13 : 40 });
  // ArrowDown opens the list on the current policy, each further one moves on, Enter picks.
  const pick = async (steps: number) => {
    press("ArrowDown");
    await screen.findAllByRole("option", { hidden: true });
    for (let i = 0; i < steps; i += 1) press("ArrowDown");
    press("Enter");
  };
  await pick(1);
  await waitFor(() => assert.equal(spin(editor, f.idle_sec)?.value, "60"));
  await choosePolicy(user, editor, "timeslice");
  assert.equal(spin(editor, f.max_serve_sec)?.value, "120");

  // Arrow keys walk the unfinished-request choices.
  radioOf(editor, y.unfinished.wait).focus();
  await user.keyboard("{ArrowUp}");
  assert.equal(radioOf(editor, y.unfinished.abort).checked, true);
  assert.equal(spin(editor, f.unfinished_wait_sec), null);
  await user.keyboard("{ArrowDown}");
  assert.equal(radioOf(editor, y.unfinished.wait).checked, true);

  // The number field takes typed digits; restore defaults and save are reached and pressed with the keyboard.
  const hold = spin(editor, f.hold_max_sec) as HTMLInputElement;
  hold.focus();
  await user.keyboard("{Control>}a{/Control}200");
  within(editor.getByRole("group", { name: f.hold_max_sec })).getByText(y.modified);
  const restore = editor.getByRole("button", { name: y.restoreDefaults });
  restore.focus();
  await user.keyboard("{Enter}");
  assert.equal(spin(editor, f.hold_max_sec)?.value, "600");
  assert.equal(editor.queryAllByText(y.modified).length, 0);
  hold.focus();
  await user.keyboard("{Control>}a{/Control}200");
  const save = saveButton(editor);
  save.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(state.sent, [[1, { policy: "timeslice", idle_sec: 30, sleep_level: "auto", max_serve_sec: 120, unfinished: "wait", unfinished_wait_sec: 30, hold_max_sec: 200 }]]));
  await waitFor(() => assert.equal(state.changed, 1));
});
