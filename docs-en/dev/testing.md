# Testing Guide

[`CLAUDE.md`](../../CLAUDE.md) is the sole authority for testing requirements. This guide explains how to select and run tests at each layer;
it does not redefine quality gates.

Real-browser console tests follow the [Browser E2E Testing Standard](../../test/e2e/browser-e2e-testing-standard.md): inventory shared components and
business domains, and build an evidence chain using real requests, persistence, agent execution, and applicable runtimes. `make test-ui` and
`npm run test:coverage` run unit/RTL tests and do not demonstrate browser coverage of business workflows. See that standard for existing Python Playwright entry points,
fixture boundaries, and browser gates that still need to be integrated.

> **Do not run L3 on a machine with an online `gpuworks-agent`.** L3 fixtures start a test agent on the same docker daemon. An agent's orphan cleanup deletes containers with the `gpuworks-*` name prefix that are outside its own desired set, destroying instances managed by the production agent (this happened on gpu3 on 2026-09-16; see T9 in `todo.md`). To run L3 on such a machine, stop the production agent first and restart it after the tests finish.

## L1–L4

| Layer | Environment | Purpose |
|---|---|---|
| L1 (simulation) | Single process, fakes/fixtures | Pure logic, protocols, errors, and edge cases; offline and reproducible by default |
| L2 (multi-process) | Multiple real local processes | Server/agent reconnection, streams, process exit, and recovery |
| L3 (container) | Local Docker/udocker/nerdctl | Container networking, mounts, runtime CLIs, and fake engines |
| L4 (cloud GPU) | Explicitly selected physical machines or cloud resources | Acceptance testing for GPUs/drivers/vendor runtimes, real downloads, and inference |

```bash
make test
make test-integration
make test-coverage-report
```

Before committing, also run `go test -race` for relevant packages, perform compile-only checks (compile without executing) for test suites with explicit build tags, and run
`make vet` and `git diff --check`. Only ordinary tests may use `t.Skip` when external capabilities are unavailable;
with `GPUWORKS_TEST_STRICT=1`, missing critical dependencies must cause a failure.

## Load Tests (Outside the Default Suite)

These tests measure elapsed time (wall-clock time) and resource consumption. They are excluded from the default test suite and explicitly enabled with `GPUWORKS_TEST_PERF=1`.
Here, fleet means a test cluster running agents in bulk; churn means repeated cycles of deployment, image pulling, entering running, and deletion.
The two multi-process tests referenced by CI have Make targets. Run the others directly with `go test`. Full commands, including `-tags`, `-timeout`, and scaling variables, are in the comments of each test function:

| Entry Point | Test | What It Measures |
|---|---|---|
| `go test` (see the `skipUnlessPerf` comment) | `TestLanes_*` in `internal/integrationtest/tunnel` | Dual-lane throughput and ctrl latency |
| `go test` (see the test comments) | `TestFleet1000NodesRedeployChurn` | Control-plane write pressure during large-scale node onboarding and repeated deployments (single process) |
| `make test-fleet-perf-processes` | `TestFleet1000NodesRedeployChurnAcrossProcesses` | The same load, with the server in a separate process and the fleet in another process |
| `go test` (see the test comments) | `TestFleetLogCenterScale` | Log center read targets under fleet load (single process) |
| `make test-fleet-log-scale-processes` | `TestFleetLogCenterScaleAcrossProcesses` | The same log center load, with the server and fleet in separate processes |
| `go test` (see the test comments) | `TestFleetGatewayLoadAcrossProcesses` | `/v1` data plane: server process + multi-GPU fleet (streaming fake engines) + streaming clients. Reports req/s, TTFT/E2E, server CPU core usage and memory, and replica distribution. Use `GPUWORKS_GATEWAY_*` to adjust the workload configuration; see the baseline in `artifacts/archive/2026-09-24-gateway-load-acceptance/gateway-load-gpu2-2026-09-24/` |

Fleet tests use 1000 agents by default, half of which repeatedly cycle through deployment → image pulling → running → deletion. The database writes to disk
to measure the real cost of writes; `:memory:` does not perform fsync (synchronizing data to disk), so it cannot measure this overhead. The tests assert that:

- Online nodes never go more than 60 s without a heartbeat update (heartbeats queued behind progress writes are the target failure of this remediation).
- No nodes remain offline when churn ends.
- Every deployment enters running.
- Each deployment has ≤ 20 event rows.

The tests also record, without assertions: the number of nodes that disconnect and reconnect during churn, p50/p99 (50th/99th percentile latency) for
`GET /deployments` and the events endpoint, a single `GET /deployments` request's latency when writes are idle, database growth,
RSS (resident process memory), and CPU usage.

### Two Configurations, One Shared Driver

All code from “all nodes online” to “churn complete” lives in `internal/integrationtest/fleet`
(REST client, node liveness observation, churn loops, reporting, and verdicts). Both configurations share this code; only the process topology differs:

- **Single-process configuration** (`internal/integrationtest/fleet`, outside the default suite): the server and 1000 agents run in
  one test process. This is the simplest setup, but its RSS/CPU/scheduler measurements combine both, so it **cannot answer “how much does the control plane itself need?”**
  Models and model files are written directly through the store as setup outside the measurement window.
- **Multi-process configuration** (`test/l2`, `-tags=multiproc`): the real `gpuworks` binary runs in a separate process, while another
  `test/l2/cmd/fleet-host` process hosts N agents (real agentapp + the same fake prober/driver).
  Nodes must obtain tokens through REST (`POST /api/v1/nodes`). Models use the product's own “register an existing directory on a node”
  path (`POST /api/v1/models/local` and `/models/{id}/register-path`), without downloading a single byte.
  RSS and CPU usage for the two processes are read separately from `/proc`.

**The multi-process configuration is the authoritative scale test**: only it separates the server's own overhead from that of 1000 agents,
and its measurements are more stable. The single-process configuration remains available for quick, small-scale trial runs. Neither is part of default CI. In GitLab,
the `l2-fleet-redeploy-churn` job runs the multi-process configuration and appears only in schedule / web / api
pipelines with `FLEET_REDEPLOY_CHURN=1` (the nightly schedule includes this variable). The job runs with `GPUWORKS_TEST_STRICT=1`, so an insufficient file descriptor limit
causes a failure rather than a skip. Logs from both processes are compressed into a single tar.gz (the 40 separate per-process log files were once rejected by the gateway with 413 on upload) and retained as artifacts for 14 days. Disconnect/reconnect counts and endpoint p99 are currently reported without assertions;
thresholds will be set after enough nightly runs have accumulated to establish trends.

The multi-process configuration can run with prebuilt binaries. **This is the only way to run it on machines without the Go toolchain**:

```bash
GPUWORKS_FLEET_SERVER_BIN=/path/gpuworks GPUWORKS_FLEET_HOST_BIN=/path/fleet-host \
GPUWORKS_FLEET_WORK_DIR=/path/work make test-fleet-perf-processes
```

`GPUWORKS_FLEET_WORK_DIR` places the run's database, agent state, and logs in the specified directory (so you can investigate the preserved state
after a failure). `GPUWORKS_FLEET_PROCESS_LOGS=1` separately saves stdout/stderr from both processes. On a machine already running production services,
use a private network namespace to isolate ports (engine port allocation always starts at 30000).
The following is a command skeleton; replace `...` with the actual test command selected in this section:

```bash
unshare -n bash -c 'ip link set lo up; ulimit -n 65536; ...'
```

### Disconnects and Reconnects

Disconnect/reconnect counts are reported without assertions. The diagnostic and fix history for this metric is retained below to explain the current verdict criteria.
Disconnects were previously attributed to “insufficient CPU when one process runs the server and 1000 agents,”
but measurements disproved this explanation: instability persisted on an idle 20-core machine. A race condition existed in the transport layer. `internal/wsconn`
fixed one trigger path, and the keepalive interval adjustment described below fixed the other. The original sequence was:

1. The go-yamux send loop updates the write deadline only after half of the connection write timeout period has elapsed.
2. coder/websocket's `net.Conn` implements write timeouts with a sticky “expired” flag. `SetWriteDeadline`
   clears the flag and reschedules the timer, but cannot revoke a callback already released to run. If the old callback arrives late, it marks the flag expired again after renewal.
3. The next frame fails immediately with `failed to write: context deadline exceeded`, before anything is written to the socket.
   yamux treats a send-loop error as fatal to the entire session. The server then runs `deregister`, and the node becomes offline.

On both ends, `laneKeepAliveInterval` was exactly
half of `laneConnWriteTimeout`, so idle-session ping/pong messages repeatedly coincided with the write-timeout instant, making the race highly likely.

The `internal/wsconn` regression test (`TestLiveWebSocketSurvivesAWriteAtTheDeadlineInstant`) reproduces this with real
WebSockets: without the wrapper, 16/16 connections fail, all in <5 ms (without writing to the socket at all); with the wrapper, none fail.

However, `wsconn` can handle only the branch where no write is in progress when the callback arrives. If the same callback overlaps an active write, it directly
cancels the adapter's write context, which is never replaced. When coder/websocket sees the canceled write context, it closes the entire connection,
and all subsequent writes fail. This branch cannot be repaired by a wrapper. In production (mirror1), each node still disconnected 40–100 times per day,
with disconnection times strictly aligned to a 15 s cycle. Packet captures showed the closing side sending FIN less than 1 ms after sending its own keepalive ping/pong.
This was the same path; it also reproduced on local loopback (7 times in 17 minutes).

The root fix reduces `laneKeepAliveInterval` on both ends from 15 s
to 10 s, ensuring keepalive writes always renew at least 5 s before the midpoint of the write-timeout period and never hit the expiration instant.
`TestLaneYamuxConfig_MatchesPlan` (server) and `TestLaneYamuxConfig_KeepsKeepAliveClearOfTheWriteDeadline`
(agent) enforce this margin. After the change, local loopback ran for 25 minutes with zero disconnects, and 100 concurrent web terminals streaming output at full speed also had zero disconnects.

The remaining instability occurs when the host is genuinely saturated, causing sessions to drop and reconnect immediately, so the count remains report-only. **A node that disconnects and does not return** is a separate
condition and is asserted. For diagnostics, use `GPUWORKS_TESTENV_LOG=<path>` to enable server/agent logs and
`GPUWORKS_FLEET_DUMP_DIR=<dir>` to capture goroutine/mutex/block dumps as soon as a heartbeat becomes stale or a node disconnects
(adjust the threshold with `GPUWORKS_FLEET_DUMP_TRIGGER_MS`).

### Single-Process Teardown

The single-process configuration must stop its agents and close client connections when the test ends; otherwise, teardown fails for reasons unrelated to the behavior under test:

- `env.StopNodes(t)` cancels all agents and waits for the hub's session table to empty. `AddNode` cleanup only cancels
  the context without waiting, so server shutdown races with hundreds of sessions being torn down and exceeds testenv's 5 s shutdown budget.
- `client.CloseIdleConnections()` closes keep-alive connections left by churn. When the test itself holds hundreds of idle
  connections open, `http.Server.Shutdown` has to wait for them. Measurements show this is why the 1000-node disk-backed test encounters
  `server shutdown timeout` roughly half the time.

The multi-process configuration does not have this problem: the server is a separate process, and teardown simply sends it a signal.

### List Responses During Onboarding

The multi-process fleet test first onboards half the nodes and observes the steady state, then onboards the rest: 500 → 1000 by default.
During each onboarding phase and steady-state phase, it concurrently reads the full node list, paginated node list, deployment list, and instance list, reporting the sample count,
p95, maximum latency, and failure count. Each endpoint must have at least 1 successful sample during onboarding; the steady-state phase waits within a bounded period for at least 3 successful samples. For short onboarding phases, only requests already issued
are allowed to finish; new requests after onboarding do not count as onboarding samples. Requests must return HTTP 200, read the complete response body,
and take no more than 10 seconds each. Timeouts, error responses, read failures, and insufficient samples cannot be marked as passing.
Deployment/event list requests during churn also check for errors, sufficient samples, and the 10-second limit.

The deployment list already uses batched relation queries; the historical N+1 issue must no longer be described as the current performance root cause. Node onboarding also triggers image recovery and
storage scope resolution: these reads must be limited to the current node and its associated transfer/tag targets, to avoid each new node scanning the entire image catalog or
storage coverage of all nodes. `GPUWORKS_FLEET_READS=0` disables only concurrent reads during churn to isolate churn costs;
capacity checks during multi-process onboarding always run.

### Scaling and Prerequisites

Scale to the machine's capacity. These three environment variables apply to both configurations:

```bash
GPUWORKS_FLEET_NODES=200 GPUWORKS_FLEET_CHURN_NODES=100 GPUWORKS_FLEET_ROUNDS=1 \
GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 60m ./internal/integrationtest/fleet/ -run '^TestFleet1000NodesRedeployChurn$'
```

File descriptor (fd) requirements are approximately `6 × node count`. The test attempts to raise the `RLIMIT_NOFILE` soft limit to the hard limit. If it cannot,
it fails with `GPUWORKS_TEST_STRICT=1`; otherwise, it explicitly skips (it never silently runs with an insufficient limit). Non-Linux hosts
skip immediately: their fd limits and RSS measurement methods differ, so their results cannot be compared with CI.

## Public Access (expose)

All tests in `internal/server/expose` are offline. A fake cloudflared is provided through `PATH` (both `--version` and registration
logs use real output formats). An injectable `HealthChecker` performs the network health check used to determine whether an address is ready.
Tests replace this check to verify readiness decisions without relying on a real network. Process-related tests run real child processes: the test binary acts as the `connector-host`
helper, covering Stop cleanup, no orphans after the parent receives SIGKILL (including startup races, over multiple rounds), exit-code
propagation, and “no hosting when the server cannot acquire the hosting lock.” Upstream wire formats (including cloudflared `/config`,
deb indexes, and rpm repomirror metadata) are all covered by fixtures built from captured real traffic.

**Do not add permanent tests that require network access** (confirmed by the user): all download, extraction, and installation regressions use synthetic packages and local HTTP
fixtures. They do not access real package repositories, invoke package managers, or add Make targets or CI jobs. There are no “special cases”
enabled by environment variables either: those would still be network-dependent tests that deteriorate over time. Perform manual online verification during development as needed, recording versions,
environments, and results, but do not turn it into permanent test cases:

- **Package repository drift** (whether fixtures still match upstream): use `curl -s https://pkg.cloudflare.com/cloudflared/dists/any/main/binary-amd64/Packages | grep -E '^(Version|Filename|SHA256):'`
  and `curl -sI https://pkg.cloudflare.com/cloudflared/rpm/x86_64/cloudflared-linux-x86_64.rpm`
  to inspect deb index fields and check whether the rpm path is still unversioned. Fixture tests are the authoritative regressions for the parser and SHA256 validation;
  this step answers only “do the fixtures need to be captured again?”
- **End-to-end acquisition**: run `gpuworks expose up` on a host without cloudflared in PATH, and confirm
  that download, extraction, `--version` self-validation, and subsequent registration all succeed.
- **quick / named / agent**: registration and new-address availability, public `/healthz`, named token-only startup and route
  discovery, the first SSE chunk and cancellation, real agent onboarding, and recovery after network interruption are all manual development-time checks, not permanent test cases.

**Real-machine acceptance testing for named was only half complete on 2026-09-22** (the user subsequently decided not to expose this provider for now; see below). Real-machine verification confirmed
automatic connector acquisition (download from `pkg.cloudflare.com` + launch from an immutable version path), startup with only a token, successful registration
(`Registered tunnel connection`, with edge/protocol recorded as observations), `awaiting_route` and guidance when no route is published remotely,
and correct rejection of a tunnel with only the default catch-all (`version:-1`, `http_status:503`) as an available address.

**Unverified**:
Initial public reachability (promotion to `ready`), the first SSE chunk/cancellation, agent onboarding, and domain persistence after restart all require the user to first
publish a route in their own Cloudflare account. Because named is currently unavailable to users (`--provider cloudflare-named` is
explicitly rejected), these items cannot be accepted before it is enabled and must not be reported as passed.

## Architecture Gates

`make check-architecture` (included in `make test-unit`, in the same CI job as `check_time_policy.sh`)
runs two types of checks:

- **Architecture constraints**: each line in `scripts/arch-constraints.txt` is `key<TAB>limit<TAB>direction`. `max` means the measured value
  must be ≤ the limit; `min` means it must be ≥ the limit. The script prints `key measured/limit (direction)` for each entry and fails when a limit is violated.
- **Dependency direction**: leaf packages do not depend on store; `api` and `controller` do not depend on each other; `pkg` does not depend on `internal`;
  agent does not depend on server; and `proto` has no dependencies within this repository. These are structural rules, not budgets, and violations fail the check.

Ratchet rule: limits for `max` keys **only decrease**, and limits for `min` keys **only increase**. When a measured value is better than its limit, the script prints
a WARNING (without failing), prompting you to tighten the limit to the new measured value in the same MR. Any change that raises a `max` limit or lowers a `min`
limit must be explained in the MR description and receive explicit approval.

See [`scripts/arch-constraints.md`](../../scripts/arch-constraints.md) for each budget's intent, measurement command, and exclusions.
Fix matching patterns for false positives; do not adjust budgets.

Process/milestone labels (phase identifiers such as `M1`, `S0`, `G2`, and `P3`) **are prohibited in production code**, including identifiers,
comments, and filenames. The `process_name_refs` budget enforces this rule. Once a plan becomes obsolete, these labels become misleading
comments that readers cannot verify; replace them with descriptions of behavior. Test files are exempt and may freely reference specific phases.

Real-machine tests consistently use `l4-<provider-or-resource-pool>-<behavior>-<topology-or-external-dependency>`, with a one-to-one mapping between Go cases, Make targets,
and CI jobs. Verifiable behaviors use `node-onboarding`, `public-download`,
`deploy-chat`, `provisioning-matrix`, `one-vm-two-logical-nodes`, or
`two-vm-docker-distributed`; `nightly` and `manual` describe only trigger policies.

The repository currently provides explicit `make test-l4-*` targets for RunPod, AutoDL, Vast, CompShare, Alibaba Cloud ECS, and Ascend resource pools.
Confirm the targets with `make help`/Makefile first, then run only the required test lanes.

Trigger policy for the schedules (`CLOUD_FULL=1`, twice a day at 03:00 and 15:00 Asia/Shanghai):

- The CompShare VM cases (`l4-compshare-provisioning-matrix`, `single-vm-tp2-dgpu`,
  `one-vm-two-logical-nodes`, `two-vm-docker-distributed`, `pd-deploy-chat-two-vm-docker`, and
  `modelscope-public-download`) are **not scheduled**: that pool almost never has capacity, and the
  Aliyun cases replace them. Start them on demand from a web/api pipeline with `COMPSHARE_VM=1`, or as an
  MR manual action. The CompShare container case (`l4-compshare-server-dial-node-onboarding-container`)
  and the offline contracts (`core-compshare-offline-contracts`) run as before.

The cloud L4 CI jobs all carry `environment: {name: cloud-test/<provider>/<lane>, action: prepare}`:
environment-scoped variables (the credentials under `cloud-test/<provider>/*`) are injected as usual, but
`action: prepare` creates no deployment. The project has "Skip outdated deployment jobs"
(`ci_forward_deployment_enabled`) on; once a job creates a deployment, the job that starts later in the same
environment fails the earlier one that is still running (truncated trace, `403 Job is not running`), so the
scheduled master pipeline and an MR manual run of the same job would kill each other. A new cloud L4 job must
use `action: prepare` too; mutual exclusion comes from `resource_group`, not from the environment.

## GitLab MR Pipeline Splitting

An MR pipeline takes as long as the slowest job in the `core` stage, and the runners differ widely in core
count (the same suite runs about 4 times slower on a 20-core machine than on the 192-core one), with a job
landing wherever a slot is free. The two long suites therefore run as parallel shards, so a shard landing on
a slow machine no longer sets the critical path:

- `core-ui-test` uses `parallel: 3` and passes `TEST_SHARD=$CI_NODE_INDEX/$CI_NODE_TOTAL` to the same
  `--test-shard` as on GitHub; the two non-UI checks, the host-env adapters and the strata queue assets, run
  only in shard 1.
- `core-l2` uses `parallel: 3`. `make test-l2 L2_SHARD=<i>/<n>` has `scripts/go_test_shard.sh` list the
  suite's top-level tests, sort them by name and deal them round-robin (the 1st, 4th, 7th... to shard 1), then
  passes the result to `go test` as `-run`; every test lands in exactly one shard. CI runs it with `-v`, so
  the trace carries each test's duration for checking that the shards stay balanced.

A cost independent of the runner is opening a database: when `store.Open` opens an empty in-memory database,
each test process runs the full migration chain for real only once, and later empty in-memory databases
replay its result (see `internal/server/store/migration_snapshot.go`). File databases, and in-memory ones
that already hold tables, always run the real migrations, so upgrade paths are unaffected.

## GitHub Actions Mirror Gates

`.github/workflows/ci.yml` runs the same checks as GitLab's core stage on GitHub-hosted runners: the UI
build and tests, the untagged Go suite with the check scripts, `go vet`, `make lint`, the CompShare offline
contracts, and strict L2/L3. GitLab remains the authoritative pipeline; the GitHub mirror only receives master,
so this workflow runs on pull requests, on manual dispatch, and nightly at 03:07 (Asia/Shanghai, just after
GitLab's nightly) on master, for comparison with GitLab's result for the same commit. It uses no secret or
self-hosted runner. Three more workflows cover GitLab's other non-L4 jobs: `docs.yml` (the docs site build,
triggered by pull requests that touch the docs; it fetches the OINK theme from the public `gpuworks_website` repository),
`ui-check.yml` (the UI walk, triggered by pull requests that touch the UI and nightly, with the report
uploaded as an artifact) and `release.yml` (release packages, see the
[release guide](release.md#github-actions-releases)). L4 still runs only in GitLab.

The scripts read CI-neutral variable names: `GPUWORKS_BUILD_COMMIT` (the commit built), `GPUWORKS_BUILT_AT`
(one build time shared by every job of a run) and `GPUWORKS_ARCH_BASELINE` (the merge request's base commit,
against which `check_architecture.sh` compares the budget file). `.gitlab-ci.yml` and the GitHub workflows
each set them from their own built-in variables; unset, the scripts fall back to local `git` and the current
time. `check_ci_yaml.sh` runs the folded-scalar comment check over both CI definitions; the stage-order check
applies to `.gitlab-ci.yml` only.

- L2/L3 take the shape of GitLab's `.docker-core`: one DinD daemon per job, exposed on a dedicated
  `gpuworks-e2e` socket, with the suite in a golang container. The runner VM's own Docker does not work: the
  VM has systemd, which the console upgrade cases take for a service manager, and the real terminal case
  accepts only a dedicated daemon. The fixture base images are pulled into DinD before the suite.
- On a 2-core runner the UI suite runs one test file at a time and takes over an hour unsplit; it runs as 4
  parallel shards, with `npm run test:coverage` passing an optional `TEST_SHARD` (such as `2/4`) to
  `--test-shard`. Unset, the command is the original one.
- The untagged Go suite runs in 3 shards of whole packages: `scripts/go_package_shard.sh <i>/<n> ./...` sorts
  the packages `go list` prints by import path and deals them round-robin, which puts the heaviest ones,
  `server/api` and two integrationtest packages, in different shards. L2 runs in 3 shards by `L2_SHARD`, as
  GitLab's `core-l2` does. The check scripts, `go vet`, `make lint`, the CompShare offline contracts and
  `check-tagged-build` share one `checks` job: the organization's hosted runners have a concurrent-job limit,
  and each of these takes seconds to a few minutes, so a job apiece would only hold more runners.
- The full-disk L2 case needs a tmpfs of its own: the suite's container cannot create a user namespace, so,
  as on GitLab, `docker run --tmpfs` mounts one and passes it in through `GPUWORKS_TEST_FULL_DISK_TMPFS`.

## Cost and Caching

- Regular provider tests verify only machine rental, SSH, agent connectivity, and GPU inventory. API fields, regional constraints, and cleanup behavior for RunPod, CompShare, AutoDL,
  Vast, and ZStack are determined by each provider's implementation. Write tests against those implementations;
  do not assume uniform behavior.
- Public downloads are verified only by explicit, low-frequency `public-download` cases such as
  `test-l4-runpod-huggingface-public-download` and `test-l4-compshare-modelscope-public-download`,
  to avoid downloading images and models again for every rental.
- AutoDL Pro, Gitee Ascend and CompShare containers have limited stock and are often sold out at the nightly peak (15:00
  Asia/Shanghai). All three wait and retry only when every candidate reports no stock: for AutoDL every candidate spec
  returns "暂无库存", for Gitee every profile's create returns "没有找到符合条件的可用主机" (it has no stock API; only a
  create reveals supply), and for CompShare containers every card type (4090 / 4090_48G / 5090, ...) is refused in every
  container zone with a supply error (capacity unavailable, `RetCode 226604` out of resources, `8433` service blip,
  `230` machine type not configured for the zone, `226641` / `226603` image not supported). Retries re-walk the whole
  candidate list every 5 minutes with a default total wait of 30 minutes, overridden by `AUTODL_STOCK_WAIT` /
  `GITEE_STOCK_WAIT` / `COMPSHARE_STOCK_WAIT` (a Go duration such as `45m`; `0` disables). Nothing is created while
  waiting, and other errors (insufficient balance, authentication, price cap exceeded, a card type skipped for a
  non-supply reason such as a missing SSH endpoint, and so on) are not retried; when AutoDL is asked for a specific
  card it neither swaps nor waits. The wait is added on top of the case's setup budget (a CompShare container is rented
  before setup, so it adds the wait too), so the `go test -timeout` and CI `timeout` of `test-l4-autodl-node-onboarding`,
  `test-l4-gitee-ascend-partition-capability` and `test-l4-compshare-server-dial-node-onboarding-container` each grew by
  30 minutes. The AutoDL hourly price cap defaults to 10 CNY (`AUTODL_MAX_HOURLY_PRICE`).
- CompShare can reuse a prewarmed VM template through `COMPSHARE_GOLDEN_IMAGE_ID`, specify an image archive visible inside the VM through
  `COMPSHARE_DOCKER_IMAGE_TAR`, and register a complete model directory already present on each node through
  `COMPSHARE_PRELOADED_MODEL_PATH`.
- CompShare V100 / V100S (Volta, sm_70) cannot use the generic vLLM image: upstream's prebuilt kernels do not include sm_70,
  and the engine exits with `no kernel image is available for execution on the device`. When `l4-compshare-single-vm-tp2-dgpu`
  gets a V100S it uses `cuda12.8-v100-vllm1cat1.5.1`, built from 1Cat-vLLM (`build/runner-images`, the matrix item is marked
  `catalog: false` and stays out of the system image catalog), and asserts that the instances really started from that image.
  `COMPSHARE_V100_TEST_IMAGE` swaps the image, and `COMPSHARE_V100_DOCKER_IMAGE_TAR` names an image archive inside the VM (without one the platform
  pulls the image during the deployment startup window). Pascal (P40, sm_61) has no image that can run on it, so this case no longer rents it.
  The image must be built and published first (`build.sh`, then `publish.py`).
  `COMPSHARE_GPU_MODELS` (comma-separated, e.g. `V100S`) replaces the candidate card order the case rents from, to aim a run at one card type.
- `prepare-compshare-golden-image` creates and validates a new template, but does not delete old templates or change CI variables.
  The template contains both open-source and proprietary kernel modules for the same NVIDIA driver version (currently 570.153.02). At boot,
  `gpuworks-nvidia-kmod.service` loads the open-source modules first (Blackwell GPUs such as the RTX 5090 require them). If no GPU is initialized,
  it switches to the proprietary modules (GPUs without GSP, such as V100S and P40, require these). The selected variant is recorded in `/run/gpuworks-nvidia-kmod`.

  After creating the template, the tool boots and validates each GPU model specified by `-validate-gpus`. You can also use `-validate-image <id>` to validate only an existing template.
  GPU models that are out of stock or rejected by CompShare are reported without causing a failure. If the image lacks nvidia-container-toolkit, the tool installs the same version as the North China template and configures the Docker runtime
  (the Shanghai template line originally lacked it, preventing containers from accessing GPUs).

  Custom templates are isolated by region, and this account's API does not support cross-region copying.
  Create each region's template from an existing template in that region (`-region`/`-zone`/`-source-image`; when only 2080Ti×4 is available in Shanghai,
  use `-gpu-model 2080Ti -gpu-count 4`). Write the results to `COMPSHARE_GOLDEN_IMAGE_ID` and
  `COMPSHARE_ZONE_IMAGE_CN_SH2`.

  CompShare does not allow custom templates for A800 or H20
  (`RetCode 226603`), regardless of the driver. Template names longer than 42 characters are rejected during creation (`RetCode 230`).

When a test fails, first preserve the failure state and inspect process/container logs: retain the failed instance, record the run-id and SSH/agent logs,
and perform scoped cleanup limited to this run only after completing the investigation. Where supported by the implementation, cloud tests can set
`GPUWORKS_CLOUD_KEEP_ON_FAILURE=1`. Afterward, invoke
`test-l4-cleanup-leaked` or the corresponding cleanup entry point using only this run's scoped run-id. Account-wide cleanup without a scope is prohibited. After cloud L4 tests, use the run-id
to confirm that no instances, forwarding resources, or temporary files remain. For the product's rental workflow, see [Rent Cloud GPUs](../how-to/rent-cloud-gpus.md).

### Alibaba Cloud ECS

The Aliyun cases cover every CompShare VM scenario: `l4-aliyun-provisioning-matrix` (agent_dial onboarding, direct server_dial onboarding,
capacity shapes, two-GPU shape), `l4-aliyun-single-vm-tp2-dgpu`, `l4-aliyun-one-vm-two-logical-nodes`,
`l4-aliyun-two-vm-docker-distributed`, `l4-aliyun-pd-deploy-chat-two-vm-docker`,
`l4-aliyun-two-vm-udocker-distributed`, and the low-frequency `l4-aliyun-modelscope-public-download` (runs only with `CLOUD_DOWNLOAD=1`).
They rent ECS GPU instances
and share their scenario implementations with CompShare (`vmBackend`: the provider supplies renting, card-type selection, Docker and image, and where the model comes from).

- **Spot first, on-demand when there is no stock**: a spot instance (`SpotAsPriceGo`) is requested first. If the stock snapshot goes stale before the create,
  or the request is refused (`OperationDenied.NoStock`, `QuotaExceed.*`, `InvalidResourceType.NotSupported`, and so on), the same case
  requests a pay-as-you-go (PostPaid) instance instead, and then moves on to the next zone or card type if there is still no stock. Account or
  fixture problems such as an insufficient balance, an invalid AccessKey, or a missing image are not swallowed and fail the case at once. Every attempt and its reason are written to the case log (`rental attempt:`),
  as are the billing mode, zone, and instance type of the instance. `ALIYUN_NO_SPOT=1` / `ALIYUN_NO_ON_DEMAND=1` remove one billing mode.
- **Candidates come from live stock**: `DescribeAvailableResource` is queried per region for spot and for on-demand stock; card types go from cheapest to most expensive (T4, A10, L20)
  and the GPU count must match exactly (`internal/testenv/cloudprovider/aliyun_shapes.go`); spot comes first across card types too: cards whose best placement is spot are tried first,
  and cards with only on-demand stock (such as 2xA10) come after. V100 is excluded: the PyTorch in the cuda12.8 runner
  has no sm_70 kernels. 2xT4 (gn6i) is not used for tensor-parallel cases: vLLM 0.17.1 measurably fails TP=2 there (CUDA graph capture reports `CUDA error: out of memory`,
  and with the agent's `CUDA_VISIBLE_DEVICES=0,1` the workers cannot initialise NVML); the shape remains usable for onboarding and provisioning cases.
- **Two-VM cases stay in one zone**: once the first VM is bought, later rentals of the same card type and GPU count stay in the same region, zone, and instance type (the same billing mode is tried first, then the other if it is refused),
  so both VMs reach each other over the private network of one VPC. Instances in one normal security group reach each other by default, so no self-referencing rule is needed (the API rejects one).
- **The network is long-lived**: each region gets one VPC `gpuworks-test-infra` (172.30.0.0/16), one vSwitch per zone, and one security group,
  each created only if missing and reused afterward; the security group opens 22 and the agent port 18800 (source `ALIYUN_INGRESS_CIDR`, anywhere by default).
  This network belongs to no single run, run-scoped cleanup never deletes it, and VPCs that already exist in the account are never touched.
- **Product path**: the agent is installed through the product's own Aliyun provider driver. `DirectServerDialOnboarding` uses `server_dial` (an ECS instance has a public IP,
  so the server dials the agent directly); the product dials an ECS instance's private address by default, so the case passes the public `host_address` explicitly); the other three cases use `agent_dial` over an SSH reverse forward,
  where Cloud Assistant is online and installs the agent as root without opening an SSH session, so no platform SSH key is written
  (the cases do not require a `Platform key` on that path). If the runner's direct path to the cloud addresses is tampered with by a network device (locally some public IPs answered any port with
  `HTTP 403`), set `GPUWORKS_SSH_PROXY=socks5://host:port` and the cases' SSH goes through that proxy (the TCP of `server_dial` does not). The product driver still does not create or release instances;
  creating and releasing exist only in the test ECS client (`aliyun.RPC` reuses the driver's V3 signing, retries, and STS).
- **Cleanup**: instances are named `gpuworks-test-<run-id>` and tagged `gpuworks-test-run`; `DeleteInstance` uses `Force`,
  and the instance counts as released only once it disappears from the listing. The system disk goes with the instance, and `cleanup-leaked --run-id` also finds and deletes leftover disks by tag.
  The leak scan lists instances region by region: over `ALIYUN_REGIONS`, or if unset over the regions that have a golden image (`ALIYUN_IMAGE_<REGION>`),
  and a failure to list one of those regions is an error; with neither it scans every region the account can see and skips the ones the key cannot list (stating so on stderr).
  When a spot instance is reclaimed, the log of the failed case contains `SPOT INTERRUPTION`, which says the failure is most likely a consequence and the case can simply be rerun.

- **First container on a fresh VM, and retries**: on a newly booted A10 VM the first vLLM container sometimes fails to start (`torch._C._cuda_init()` reports
  `No CUDA GPUs are available`; `NVMLError_Unknown` was seen once), and the platform's retry of the identical container then succeeds; the failed row stays in the deployment's instance list.
  The multi-VM cases therefore warm up in `EnsureDocker` first: they run a CUDA check on every card the way the agent does (`--gpus "device=N"`), with retries,
  and log `GPUs ready for containers after N warm-up tries`. While waiting for a deployment to run they count live ranks only (failed, stopped and removing rows do not count),
  and every failed attempt writes its engine log to the case log at once. Before this, the PD case counted the failed row as a rank and kept waiting until the timeout even though the deployment was already running 2/2.

- **udocker case**: it needs three things. Agent state must not live under `/tmp` (so the Aliyun backend's `HostedRoot` is `/opt`): udocker binds a private directory over the guest's `/tmp`,
  which hides the loopback link `ensureHostRootLoopback` puts under `ROOT/tmp/...`, so an engine that dlopens `libcudart` by the host path `/proc/self/maps` shows cannot find it (measured: the file is in ROOT, unresolvable in the guest).
  A root login's agent runs workloads as the dedicated `udocker` user, so `$root` is `chmod 711`; without it PRoot fails with `fork/exec .../proot-x86_64: permission denied`.
  udocker has its own image store and cannot use the runner image the golden image keeps in Docker. The case does not pull it from a registry (a pull takes 25 to 40 minutes per rank and now and then restarts from another source near the end).
  It goes through the platform's "import from URL" instead: each VM runs `docker save` to an uncompressed archive (about 14 GiB, about 4 minutes) and publishes it with a small file server that answers HEAD and Range (a `python3` script bound only to the private address, port 18881, serving that one file);
  ordinary private destinations are accessible by default; the fixture's preconfigured `outbound_allow_private_hosts` authorizes both VMs when strict mode (`outbound_restrict_private_hosts: true`) is enabled. The case then calls the management API the way an operator does: `POST /images/preview-archive-url` to read the archive,
  record both archive URLs on the runner image's fetch addresses (a custom image is created from the console's form when the catalog lacks it), and `POST /images/{id}/nodes/{uuid}/sync` (`source=url`) per node so the node downloads directly from **its own VM** and loads it.
  The deployment is created only once the node image row is `present`, and after it the case checks the row is still `present` from `url`. The archives and file servers are deleted/stopped as soon as the import finishes.
  The case log records each rank's `save`, `download`, `load` and total times (the download/load boundary is read from the image row's message, so it is as fine as the polling interval); measured on 2026-10-27: save about 4 minutes, download about 1 minute, load about 15 seconds, the whole case about 11 minutes.
  The system disk stays 300 GiB: during the import the same disk holds the Docker store, the archive being served, the archive the node downloaded and udocker's layers (both archives are deleted after the load), and the container still has to be unpacked afterwards.
  The case rents on-demand instead of spot (`GPUSpec.OnDemand`) so a reclaim cannot end it.
  On failure `GPUWORKS_CLOUD_KEEP_ON_FAILURE=1` now works for the multi-VM cases too: the failed VM is kept and a credentials file written.

The golden image is built by `make prepare-aliyun-golden-image`: it starts a T4 instance from Alibaba Cloud's public image
`ubuntu_22_04_x64_100G_with_gpu_driver_and_cuda_alibase_*` (driver and CUDA preinstalled),
installs Docker 29 and the NVIDIA container toolkit (the Docker CE repository is tried at `mirrors.aliyun.com`, Tsinghua, USTC, and Huawei Cloud in turn,
moving on when a mirror is mid-sync and fails the index check; a resumed build first drops the Docker repository the earlier attempt wrote), pulls the vLLM runner image (the Huawei Cloud SWR mirror first,
then quay.io and Docker Hub; the final tag is `gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4`), downloads Qwen3-0.6B to
`/opt/gpuworks/test-fixtures/models/qwen3-0.6b` with checksums, stops Docker, syncs, and calls `CreateImage`, then `CopyImage`
to every region in `-regions` (by default East China 1, North China 2, South China 1, and South China 3) and boots a new instance in the source region to validate it (validation comes before copying, so a broken image is not copied to four regions).
The image is about 50 GB (the runner image dominates); the first full snapshot of the 150 GiB system disk takes about 90 minutes and the cross-region copies about another hour, so plan for two to three hours.
Custom images are isolated by region, so there is one ID per region; the printed `ALIYUN_IMAGE_CN_HANGZHOU=` and similar lines go into `secrets/cloud-providers.env`
or the CI environment variables, and cases rent only in regions that have an image. The tool deletes no older image and changes no CI variable;
with `GPUWORKS_CLOUD_KEEP_ON_FAILURE=1` a failed source instance is kept and a 0600 credentials file is written,
and `-resume-credentials` continues from it.
Both the model and the runner can be changed: `-model-repo`/`-model-rev` pick the ModelScope model, `-model-path` its path in the image,
and `-docker-image`/`-pull-sources` the runner; the default image name follows the model (`gpuworks-golden-<model>-<UTC time>`).
`-source-regions` limits where the source and validation instances are rented (by default the same as `-regions`); when copying to overseas and similar regions, use it to keep the source instance in a mainland region with stock.
The L4 cases register the default Qwen3-0.6B; single-VM cases deploy `gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4` on A10, L20 and similar cards and `gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2` on T4 (baked in with `-extra-image`), and multi-node cases use the latter on every card. An image with another model or runner cannot serve them as is. The image already carries Docker and the model, so the cases only verify them and neither install nor download
(a missing piece means the image is broken and the case fails at once).

Variables CI needs (scoped to `cloud-test/aliyun/*`): `ALIYUN_ACCESS_KEY_ID`, `ALIYUN_ACCESS_KEY_SECRET`
(preferably a RAM user limited to ECS and VPC), and one `ALIYUN_IMAGE_<REGION>` per region; optionally `ALIYUN_REGIONS`,
`ALIYUN_INGRESS_CIDR`, `ALIYUN_NO_SPOT`, and `ALIYUN_NO_ON_DEMAND`.

## Evidence

This guide documents testing methods. Put one-off run logs, screenshots, image digests, and environment facts in
`artifacts/work/<YYYY-MM-DD>-<topic>/<topic>-<YYYY-MM-DD>.md` (move them with the topic into
`artifacts/archive/` when the work is closed out). Current documentation only links to this evidence.
