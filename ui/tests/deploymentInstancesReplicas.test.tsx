// Deployment instances, part three: give-up notices and Retry, the overview, an unplaced deployment and per-instance metrics.
// Split from deploymentInstances.test.tsx so each file stays well inside the
// per-file test timeout on loaded runners. The shared fixture installs an
// isolated DOM and the original cleanup hooks in each test process.
import { assert, test, screen, waitFor, within, fireEvent, userEvent, list, listPage, dom, i18n, locale, Events, APIError, selectInstance, stub, instance, deployment, openDetail, drawer } from "./helpers/deploymentLifecycle.tsx";

test("an ordinary replica that gave up is named with its failure, and Retry targets that replica", async () => {
  const exhausted = deployment({ instances: [instance("inst-a", { replica: { index: 0 } })], retry_exhausted: { index: 1, failed_attempts: 3, last_failure: "engine exited" } });
  stub("deploymentsPage", async () => listPage([exhausted]));
  stub("deployment", async () => exhausted);
  const calls: Array<{ id: number; unit: unknown }> = [];
  let fail = true;
  stub("retryDeploymentUnit", async (id: number, unit: unknown) => {
    calls.push({ id, unit });
    if (fail) throw new APIError("UNIT_NOT_FAILED", { role: "", index: 1 }, "the unit has no failed attempts to retry", 409);
    return deployment();
  });
  await openDetail();
  const overview = screen.getByRole("heading", { name: locale.detail.serviceOverview }).closest("section")!;
  const status = within(overview).getByText(locale.detail.state).closest("tr")! as HTMLElement;
  const alert = within(status).getByRole("alert");
  assert.ok(within(alert).getByText("A replica stopped being rebuilt after 3 failed attempts: engine exited"));
  fireEvent.click(within(alert).getByRole("button", { name: locale.detail.retry.action }));
  await within(alert).findByText(i18n.t("detail.retry.failed", { detail: i18n.t("common:apiError.UNIT_NOT_FAILED") }));
  fail = false;
  // antd leaves the loading icon's leave animation in the button's name under
  // JSDOM, so find the second click's button by its text.
  fireEvent.click(within(alert).getByText(locale.detail.retry.action).closest("button")!);
  await waitFor(() => assert.equal(calls.length, 2));
  assert.deepEqual(calls[1], { id: 1, unit: { role: "", index: 1 } });
});

test("a deployment without a give-up shows no retry notice", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail();
  assert.equal(screen.queryByText(/stopped being rebuilt/), null);
});

test("the overview copies the model, public name and configured image independently of the model link and instance image", async () => {
  const image = "registry.example:5000/engines/runner:v2";
  const dep = deployment({ model_name: "org/model", served_name: "public-model", image });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  await openDetail();
  const model = within(within(drawer()).getByRole("heading", { name: locale.detail.serviceOverview }).closest("section")!);
  const user = userEvent.setup();
  for (const [label, value] of [
    [locale.detail.copyModel, dep.model_name],
    [locale.detail.copyServedName, dep.served_name],
    [locale.detail.copyConfiguredImage, image],
  ]) {
    await user.click(model.getByRole("button", { name: label }));
    assert.equal(await navigator.clipboard.readText(), value);
  }
  assert.equal(model.getByRole("link", { name: dep.model_name }).getAttribute("href"), `/models?detail=${dep.model_id}`);
});

test("an unplaced deployment shows its engine and automatic image selection before any instance exists", async () => {
  const dep = deployment({ runtime_state: "pending", instances: [] });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  await openDetail();
  const model = within(drawer()).getByRole("group", { name: locale.detail.modelAndEngine });
  const field = within(model).getByText(locale.detail.configuredRuntime).closest("tr")! as HTMLElement;
  assert.ok(within(field).getByText(dep.engine));
  assert.ok(within(field).getByText(locale.page.autoImage));
  assert.equal(within(field).queryByRole("button", { name: locale.detail.copyConfiguredImage }), null);
  assert.equal(within(drawer()).queryByRole("heading", { name: locale.detail.instanceLaunch }), null);
});

test("instance metrics follow selection and the remaining record after removal", async () => {
  const { installCanvas } = await import("./helpers/analyticsFixtures.ts");
  installCanvas(dom);
  let rows = [instance("third", { state: "failed" }), instance("second"), instance("first")];
  const queried: string[] = [];
  stub("metricsQuery", async (_metric: string, params: { instance?: string }) => {
    if (params.instance) queried.push(params.instance);
    return list([]);
  });
  stub("deploymentsPage", async () => listPage([deployment({ instances: rows })]));
  stub("deployment", async () => deployment({ instances: rows }));
  await openDetail();
  selectInstance("first");
  assert.ok(screen.getByRole("heading", { name: locale.instance.tab.metrics }));
  selectInstance("second");
  await waitFor(() => assert.ok(queried.includes("second")));
  rows = [rows[0], rows[2]];
  for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  await waitFor(() => assert.ok(document.getElementById("instance-first")));
  assert.equal(document.getElementById("instance-second"), null);
  assert.ok(screen.getByRole("heading", { name: locale.instance.tab.metrics }));
});
