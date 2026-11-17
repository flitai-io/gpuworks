import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const zh = (await createI18n(["nodes"], "zh")).i18n;
const { cleanup, render, screen, within } = await import("@testing-library/react");
const { default: NodeDoctorReport } = await import("../src/components/NodeDoctorReport.tsx");
type ReportT = import("../src/api.ts").NodeDoctorReport;

afterEach(() => cleanup());

const wrap = (node: React.ReactNode, i18n = zh) => render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);

const GiB = 1024 ** 3;
const overlayRoot = { point: "/", fs_type: "overlay", source: "overlay", options: "rw,relatime,lowerdir=/var/lib/docker/overlay2/l/ABC", ephemeral: true };
const nfsWorkspace = { point: "/workspace", fs_type: "nfs4", source: "10.1.2.3:/volumes/abcd", options: "rw,relatime,vers=4.1", network: true };

// The RunPod pod of the field report: udocker could not be set up on the NFS
// volume, and the models root of the default install sits on the pod's layer.
const runpod: ReportT = {
  report: "== gpuworks agent doctor ==\n",
  host: {
    hostname: "pod", os: "linux", arch: "amd64", kernel: "5.15", cpu_model: "EPYC", cpu_cores: 8,
    mem_total_bytes: 64 * GiB, mem_avail_bytes: 40 * GiB,
    disks: [
      { kind: "root", path: "/", total_bytes: 100 * GiB, avail_bytes: 80 * GiB, mount: overlayRoot },
      { kind: "udocker", path: "/workspace/gpuworks/udocker", total_bytes: 500 * GiB, avail_bytes: 450 * GiB, mount: nfsWorkspace },
      { kind: "models", path: "/opt/gpuworks/models", total_bytes: 100 * GiB, avail_bytes: 80 * GiB, mount: overlayRoot },
    ],
  },
  runtimes: [{ name: "udocker", status: "ok", selected: true, command: "" }],
  findings: [
    {
      code: "RUNTIME_PROVISION_FAILED", status: "fail",
      probe: "set up the runveil runtime: run user, /workspace/gpuworks/udocker and /workspace/gpuworks/udocker-state, patched PRoot",
      evidence: [
        { name: "error", value: "prepare udocker ownership: chown -R udocker:udocker /workspace/gpuworks/udocker: exit status 1: Operation not permitted" },
        { name: "failed_at", value: "2026-10-10T08:30:05Z" },
      ],
      recheck: "findmnt -T /workspace/gpuworks/udocker; ls -ld /workspace/gpuworks/udocker /workspace/gpuworks/udocker-state",
    },
    {
      code: "PATH_EPHEMERAL", status: "warn",
      disk: { kind: "models", path: "/opt/gpuworks/models", total_bytes: 100 * GiB, avail_bytes: 80 * GiB, mount: overlayRoot },
      probe: "/proc/self/mountinfo",
      evidence: [{ name: "TARGET", value: "/" }, { name: "FSTYPE", value: "overlay" }, { name: "container", value: "/.dockerenv" }],
      recheck: "findmnt -T /opt/gpuworks/models",
    },
  ],
  facts: [
    {
      code: "runtime_identity",
      value: "the agent's own user (uid 0): the dedicated runtime user is not used because /workspace/gpuworks/udocker refuses to change file ownership",
      identity: { runtime: "runveil", uid: 0, fallback: "/workspace/gpuworks/udocker refuses to change file ownership" },
    },
    {
      code: "network_filesystem",
      value: "/workspace is nfs4 (10.1.2.3:/volumes/abcd); holds runveil /workspace/gpuworks/udocker",
      disks: [{ kind: "udocker", path: "/workspace/gpuworks/udocker", total_bytes: 500 * GiB, avail_bytes: 450 * GiB, mount: nfsWorkspace }],
    },
  ],
};

test("a failed runtime setup and a path on the container layer are explained in Chinese with evidence and recheck", () => {
  wrap(<NodeDoctorReport report={runpod} />);
  const findings = screen.getByRole("region", { name: "需要处理" });
  assert.ok(within(findings).getByText("运行时初始化失败"));
  assert.ok(within(findings).getByText(/最近一次尝试失败/));
  assert.ok(within(findings).getByText("prepare udocker ownership: chown -R udocker:udocker /workspace/gpuworks/udocker: exit status 1: Operation not permitted"));
  assert.ok(within(findings).getByText("2026-10-10T08:30:05Z"));
  assert.ok(within(findings).getByText("findmnt -T /workspace/gpuworks/udocker; ls -ld /workspace/gpuworks/udocker /workspace/gpuworks/udocker-state"));
  assert.ok(within(findings).getByText("数据位于容器层"));
  assert.ok(within(findings).getByText("/opt/gpuworks/models 位于本容器自身的文件系统层（/ 上的 overlay），而不是数据卷。"));
  assert.ok(within(findings).getByText(/本地文件系统的持久卷/));
  assert.ok(within(findings).getByText("findmnt -T /opt/gpuworks/models"));
  // The "udocker" driver is shown as the runveil runtime operators know.
  const runtimes = screen.getByRole("region", { name: "运行时" });
  assert.ok(within(runtimes).getByText("runveil"));
  assert.equal(within(runtimes).queryByText("udocker"), null);
  // The network filesystem is information, not a finding.
  assert.equal(within(findings).queryByText(/nfs4/), null);
  const facts = screen.getByRole("region", { name: "信息" });
  assert.ok(within(facts).getByText("运行身份"));
  // Worded in the reader's language from the typed statement, not the
  // agent's English sentence.
  assert.ok(within(facts).getByText("agent 自身用户（uid 0）；未使用专用运行时用户，原因：/workspace/gpuworks/udocker refuses to change file ownership"));
  assert.equal(within(facts).queryByText(/the agent's own user/), null);
  assert.ok(within(facts).getByText("网络文件系统"));
  assert.ok(within(facts).getByText("/workspace 是 nfs4（10.1.2.3:/volumes/abcd），承载 runveil /workspace/gpuworks/udocker"));
});

test("each disk line names its filesystem and the flags that matter", () => {
  wrap(<NodeDoctorReport report={runpod} />);
  const overview = screen.getByRole("region", { name: "概况" });
  assert.ok(within(overview).getByText("nfs4"));
  assert.ok(within(overview).getByText("网络"));
  assert.equal(within(overview).getAllByText("overlay").length, 2);
  assert.equal(within(overview).getAllByText("容器层").length, 2);
});

test("the English report names the same findings", async () => {
  const en = (await createI18n(["nodes"], "en")).i18n;
  wrap(<NodeDoctorReport report={runpod} />, en);
  const findings = screen.getByRole("region", { name: "Action required" });
  assert.ok(within(findings).getByText("Runtime setup failed"));
  assert.ok(within(findings).getByText("Data on the container layer"));
  assert.ok(within(findings).getByText("/opt/gpuworks/models is on this container's own filesystem layer (overlay at /), not on a volume."));
  assert.ok(within(screen.getByRole("region", { name: "Information" })).getByText("Runtime identity"));
});

test("the mount findings read their path, mount point and filesystem", () => {
  const disk = { kind: "udocker", path: "/data/udocker", total_bytes: 100 * GiB, avail_bytes: 90 * GiB };
  wrap(<NodeDoctorReport report={{
    ...runpod,
    findings: [
      { code: "PATH_NOEXEC", status: "fail", disk: { ...disk, mount: { point: "/data", fs_type: "xfs", noexec: true } }, probe: "/proc/self/mountinfo", recheck: "findmnt -T /data/udocker" },
      { code: "PATH_READ_ONLY", status: "fail", disk: { ...disk, mount: { point: "/archive", fs_type: "ext4", read_only: true } } },
    ],
  }} />);
  const findings = screen.getByRole("region", { name: "需要处理" });
  assert.ok(within(findings).getByText("路径挂载为 noexec"));
  assert.ok(within(findings).getByText("/data/udocker 位于 /data（xfs），该挂载带有 noexec。"));
  assert.ok(within(findings).getByText("路径挂载为只读"));
  assert.ok(within(findings).getByText("/data/udocker 位于 /archive（ext4），该挂载为只读。"));
});

// An agent that predates these items sends disks without a mount and no
// identity fact; nothing about it may break.
test("a report from an agent without mounts renders disks as before", () => {
  const old: ReportT = {
    ...runpod,
    host: { ...runpod.host!, disks: runpod.host!.disks.map(({ kind, path, total_bytes, avail_bytes }) => ({ kind, path, total_bytes, avail_bytes })) },
    findings: [],
    facts: [{ code: "runtime_plugin", value: "none" }],
  };
  wrap(<NodeDoctorReport report={old} />);
  const overview = screen.getByRole("region", { name: "概况" });
  assert.ok(within(overview).getByText("/workspace/gpuworks/udocker：共 500.0 GiB，可用 450.0 GiB（90%）"));
  assert.equal(within(overview).queryByText("nfs4"), null);
  assert.ok(screen.getByText("未发现需要处理的问题。"));
});

// A MacBook on battery whose settings let it sleep when idle (the agent's
// report for the M1 Max test machine).
const macbook: ReportT = {
  report: "== gpuworks agent doctor ==\n",
  host: {
    hostname: "mbp", os: "darwin", arch: "arm64", kernel: "24.4.0", cpu_model: "Apple M1 Max", cpu_cores: 10,
    mem_total_bytes: 64 * GiB, mem_avail_bytes: 30 * GiB,
    disks: [{ kind: "root", path: "/", total_bytes: 926 * GiB, avail_bytes: 300 * GiB }],
  },
  runtimes: [{ name: "host", status: "ok", selected: true, command: "" }],
  findings: [{
    code: "MACOS_IDLE_SLEEP", status: "warn", probe: "pmset -g",
    evidence: [{ name: "sleep", value: "1 (sleep prevented by powerd, UURemote)" }, { name: "power source", value: "Battery Power" }],
    recheck: "pmset -g",
  }],
  facts: [
    { code: "metal_working_set", value: "GPU may keep 48.0 GiB of 64.0 GiB memory resident (iogpu.wired_limit_mb=0, the macOS default)\nto raise it until the next reboot: sudo sysctl iogpu.wired_limit_mb=<MiB>" },
    { code: "power", value: "Battery Power; battery 63%; discharging; 3:55 remaining; idle sleep after 1 min (sleep prevented by powerd, UURemote)" },
    { code: "macos_firewall", value: "Firewall is disabled. (State = 0); Firewall stealth mode is off" },
  ],
};

test("a Mac that sleeps when idle is explained with the command that keeps it awake", async () => {
  wrap(<NodeDoctorReport report={macbook} />);
  const findings = screen.getByRole("region", { name: "需要处理" });
  assert.ok(within(findings).getByText("空闲时会睡眠"));
  assert.ok(within(findings).getByText(/有实例运行时代理会阻止空闲睡眠/));
  assert.ok(within(findings).getByText(/sudo pmset -c sleep 0/));
  assert.ok(within(findings).getByText("Battery Power"));
  const facts = screen.getByRole("region", { name: "信息" });
  for (const label of ["Metal 可用内存", "电源与睡眠", "macOS 应用防火墙"]) assert.ok(within(facts).getByText(label));
  assert.ok(within(facts).getByText(/sudo sysctl iogpu\.wired_limit_mb=<MiB>/));
  cleanup();

  const en = (await createI18n(["nodes"], "en")).i18n;
  wrap(<NodeDoctorReport report={{ ...macbook, findings: [{ code: "MACOS_UNSUPPORTED", status: "fail", probe: "sysctl kern.osproductversion",
    evidence: [{ name: "macOS", value: "14.7.2" }, { name: "required", value: "15" }], recheck: "sw_vers" }] }} />, en);
  const enFindings = screen.getByRole("region", { name: "Action required" });
  assert.ok(within(enFindings).getByText("macOS too old"));
  assert.ok(within(enFindings).getByText("Mac nodes need macOS 15 or later; the evidence shows this Mac's version."));
  assert.ok(within(enFindings).getByText("14.7.2"));
  assert.ok(within(enFindings).getByText("sw_vers"));
  assert.ok(within(screen.getByRole("region", { name: "Information" })).getByText("Power and sleep"));
});

// A root agent whose state directory sits under /root: the runtime user
// cannot search /root, so every launch fails with permission denied.
const untraversable: ReportT = {
  ...runpod,
  findings: [{
    code: "RUNTIME_STATE_UNTRAVERSABLE", status: "fail",
    probe: "search permission on every directory above /root/gpuworks for udocker (uid 1001, gid 1001)",
    evidence: [
      { name: "directory", value: "/root" }, { name: "mode", value: "0700" }, { name: "owner", value: "0:0" },
      { name: "user", value: "udocker" }, { name: "state_dir", value: "/root/gpuworks" }, { name: "fix", value: "chmod o+x /root" },
    ],
    recheck: "namei -m /root/gpuworks",
  }],
};

test("an untraversable ancestor of the agent directory names the blocking directory and the fix in both languages", async () => {
  wrap(<NodeDoctorReport report={untraversable} />);
  const findings = screen.getByRole("region", { name: "需要处理" });
  assert.ok(within(findings).getByText("运行时用户无法访问代理目录"));
  assert.ok(within(findings).getByText("/root 不允许工作负载的运行用户 udocker 进入，因此它无法访问代理目录 /root/gpuworks。"));
  assert.ok(within(findings).getByText(/请执行 `chmod o\+x \/root` 开放该目录/));
  assert.ok(within(findings).getByText("0700"));
  assert.ok(within(findings).getByText("namei -m /root/gpuworks"));
  cleanup();

  const en = (await createI18n(["nodes"], "en")).i18n;
  wrap(<NodeDoctorReport report={untraversable} />, en);
  const enFindings = screen.getByRole("region", { name: "Action required" });
  assert.ok(within(enFindings).getByText("Runtime user cannot reach the agent directory"));
  assert.ok(within(enFindings).getByText("/root is not searchable by udocker, the user workloads run as, so it cannot reach the agent directory /root/gpuworks."));
  assert.ok(within(enFindings).getByText(/Open the directory with `chmod o\+x \/root`/));
});
