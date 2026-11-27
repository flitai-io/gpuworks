// Deployment instances, part two: status filters and the record cascade across replicas, refreshes and searches.
// Split from deploymentInstances.test.tsx so each file stays well inside the
// per-file test timeout on loaded runners. The shared fixture installs an
// isolated DOM and the original cleanup hooks in each test process.
import { assert, test, screen, within, fireEvent, listPage, dom, locale, Events, selectInstance, stub, instance, deployment, openDetail, drawer } from "./helpers/deploymentLifecycle.tsx";

test("status filters count the whole deployment and follow matching records across replicas and refreshes", async () => {
  const generation = { nnodes: 2, dist_group: "multi-node", dist_attempt: 1 };
  let rows = [instance("ready", { ...generation, replica: { index: 0 } }), instance("launch", { replica: { index: 1 }, state: "starting" }),
    instance("retry", { ...generation, dist_role: "follower", node_rank: 1, state: "failed" }), instance("unassigned", { state: "failed" })];
  stub("deployment", async () => deployment({ instances: rows }));
  await openDetail();
  selectInstance("ready");
  const all = () => screen.getByRole("button", { name: "All 4" });
  const replica = () => screen.getByRole("combobox", { name: locale.detailLayout.switchReplica });
  fireEvent.click(screen.getByRole("button", { name: "Starting 1" }));
  assert.ok(document.getElementById("instance-launch"));
  assert.ok(all());
  fireEvent.click(replica());
  assert.equal(screen.getAllByRole("option").length, 1);
  fireEvent.click(screen.getByRole("option"));

  fireEvent.click(screen.getByRole("button", { name: "Failed attempt 2" }));
  assert.ok(document.getElementById("instance-retry"));
  fireEvent.click(replica());
  const options = screen.getAllByRole("option");
  assert.equal(options.length, 2);
  fireEvent.click(options[1]);
  assert.ok(document.getElementById("instance-unassigned"));
  assert.ok(screen.getByRole("button", { name: "Failed attempt 2", pressed: true }));
  fireEvent.click(all());
  assert.ok(document.getElementById("instance-unassigned"));

  fireEvent.click(screen.getByRole("button", { name: "Starting 1" }));
  rows = rows.filter((_, i) => i !== 1);
  for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  await screen.findByRole("button", { name: "All 3", pressed: true });
  assert.ok(document.getElementById("instance-ready"));
});

test("the cascade keeps the inspected attempt across refreshes and searches all 32 records by status and ID", async () => {
  const record = (uuid: string, state: string) => instance(uuid, {
    state, replica: state === "running" ? { index: 0 } : undefined, schedule: { unit: { key: "|[]" } },
  });
  let rows = [record("ready", "running"), record("first-failure", "failed")];
  stub("deployment", async () => deployment({ instances: rows }));
  stub("deploymentsPage", async () => listPage([deployment({ instances: rows })]));
  const refresh = () => {
    for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  };
  await openDetail();
  selectInstance("first-failure");
  rows = [...Array.from({ length: 30 }, (_, n) => record(`attempt-${n}`, "failed")), ...rows];
  refresh();
  const failures = await screen.findByRole("button", { name: "Failed attempt 31" });
  assert.ok(document.getElementById("instance-first-failure"));
  fireEvent.click(failures);
  const switcher = within(screen.getByRole("group", { name: locale.detail.switchInstance }));
  fireEvent.click(switcher.getByRole("combobox"));
  fireEvent.change(screen.getByPlaceholderText(locale.detail.switchInstance), { target: { value: "ATTEMPT-29" } });
  const options = within(screen.getByRole("listbox")).getAllByRole("option");
  assert.deepEqual(options.map(el => el.getAttribute("aria-label")), ["attempt-29 Failed attempt"]);
  fireEvent.click(options[0]);
  assert.ok(document.getElementById("instance-attempt-29"));
  fireEvent.click(switcher.getByRole("button", { name: locale.detail.previousInstance }));
  assert.ok(document.getElementById("instance-attempt-28"));
  fireEvent.click(switcher.getByRole("button", { name: locale.detail.nextInstance }));
  assert.ok(document.getElementById("instance-attempt-29"));
  rows = [];
  refresh();
  await within(document.getElementById("deployment-instances")!).findByText(locale.detail.noInstances);
  assert.ok(screen.getByRole("tab", { name: locale.detail.instanceDetails, exact: true }));
  assert.equal(drawer().querySelector(".replica-cascade"), null);
  assert.equal(screen.queryByRole("group", { name: locale.detail.switchInstance }), null);
});
