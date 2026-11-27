// Deployment instances, part one: switching between instances, rank tags and the monitor's instance link.
// Split from deploymentInstances.test.tsx so each file stays well inside the
// per-file test timeout on loaded runners. The shared fixture installs an
// isolated DOM and the original cleanup hooks in each test process.
import { assert, test, screen, waitFor, listPage, locale, scrolled, selectInstance, stub, instance, deployment, openDetail, drawer } from "./helpers/deploymentLifecycle.tsx";

test("instance switching starts with the first-created instance and retains the leader and rank of each follower", async () => {
  const group = (uuid: string, rank: number, group: string, index: number) => instance(uuid, {
    nnodes: 2, node_rank: rank, headless: rank > 0, dist_role: rank > 0 ? "follower" : "leader", dist_group: group, dist_attempt: 1, replica: { index },
  });
  // Listed follower-first and interleaved; a legacy follower row carries no
  // group, and the server still names it a follower (it derives dist_role
  // from headless and the rank for rows from before it was recorded).
  const multi = deployment({ instances: [
    group("follow-a1", 1, "7-a1f3", 0), group("follow-b1", 1, "7-c9e0", 1), group("leader-a0", 0, "7-a1f3", 0), group("leader-b0", 0, "7-c9e0", 1),
    instance("legacy-f1", { nnodes: 2, node_rank: 1, headless: true, dist_role: "follower" }),
  ] });
  stub("deploymentsPage", async () => listPage([multi]));
  stub("deployment", async () => multi);
  await openDetail();
  selectInstance("legacy-f1");
  for (const [uuid, rank] of [
    ["legacy-f1", "follower · rank 1"],
    ["leader-b0", "leader · rank 0"],
    ["leader-a0", "leader · rank 0"],
    ["follow-b1", "follower · rank 1 · leader leader-b"],
    ["follow-a1", "follower · rank 1 · leader leader-a"],
  ]) {
    selectInstance(uuid);
    assert.deepEqual([...document.querySelectorAll("[id^='instance-']")].map((el) => el.id), [`instance-${uuid}`]);
    assert.ok(screen.getByText(rank));
  }
});

test("single-rank replicas carry no rank tag and keep the plain count", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail();
  await screen.findByText(locale.detail.instances.replace("{{count}}", "2"));
  assert.equal(screen.queryByText(/rank \d/), null);
});

test("the monitor's instance link scrolls the drawer to that instance", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail("?deployment=1&instance=inst-a");
  await waitFor(() => assert.deepEqual(scrolled, ["deployment-instances"]));
  assert.ok(document.getElementById("instance-inst-a"));
  selectInstance("inst-b");
  assert.ok(document.getElementById("instance-inst-b"));
  selectInstance("inst-a");
  assert.ok(document.getElementById("instance-inst-a"));
  assert.deepEqual(scrolled, ["deployment-instances"]);
});
