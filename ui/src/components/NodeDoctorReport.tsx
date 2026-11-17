import { Alert, Collapse, Descriptions, Space, Table, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { DoctorDisk, DoctorFact, DoctorFinding, DoctorMount, DoctorRunIdentity, DoctorTarget, NodeDoctorReport as Report } from "../api";
import { GPUHealthDetail, GPUHealthSummary, HealthCommand, HealthEvidence, gpuHealthLabel } from "./GPUHealth";
import NodeDoctorChecks from "./NodeDoctorChecks";
import { Badge } from "./ui/badge";
import { runtimeLabel } from "../nodeDisplay";

const rawStyle = {
  background: "#111827",
  color: "#f9fafb",
  padding: 12,
  borderRadius: 2,
  maxHeight: 320,
  overflow: "auto",
  whiteSpace: "pre-wrap",
  margin: 0,
} as const;

const healthColor: Record<string, string> = { OK: "green", Warning: "orange", Error: "red" };

function gib(bytes: number): string {
  return `${(bytes / 1024 ** 3).toFixed(1)} GiB`;
}

function percent(part: number, total: number): number {
  return total > 0 ? Math.floor((part * 100) / total) : 0;
}

function FindingTarget({ finding, target, status }: { finding: DoctorFinding; target: DoctorTarget; status: "Warning" | "Error" }) {
  const { t } = useTranslation("nodes");
  const resettable = !!target.actions?.length;
  return (
    <div className="node-doctor-target">
      <Typography.Text strong>{t("doctor.card", { index: target.index, bdf: target.pcie_bdf || "—" })}</Typography.Text>
      <GPUHealthDetail
        status={status}
        code={finding.code}
        evidence={target.evidence}
        guide={target.probe ? { probe: target.probe, recheck: target.recheck, actions: target.actions } : undefined}
        commandsOnly
        extraAction={resettable && (
          <Space direction="vertical" size={2} style={{ display: "flex" }}>
            {target.processes?.map((proc) => (
              <Typography.Text key={proc.pid} type="danger">{t("doctor.runningProcess", { pid: proc.pid, name: proc.name })}</Typography.Text>
            ))}
          </Space>
        )}
      />
    </div>
  );
}

function AcceleratorFinding({ finding }: { finding: DoctorFinding }) {
  const { t } = useTranslation("nodes");
  const { t: tc } = useTranslation("common");
  const status = finding.status === "fail" ? "Error" : "Warning";
  const targets = finding.targets ?? [];
  return (
    <div className="node-doctor-finding">
      <Space direction="vertical" size={8} style={{ display: "flex" }}>
        <Space wrap>
          <Tag color={status === "Error" ? "red" : "orange"}>{gpuHealthLabel(tc, status, finding.code)}</Tag>
          <Typography.Text>{t("doctor.cards", { count: targets.length, indexes: targets.map((target) => target.index).join(", ") })}</Typography.Text>
        </Space>
        <GPUHealthSummary status={status} code={finding.code} withAction />
        {targets.map((target) => <FindingTarget key={target.index} finding={finding} target={target} status={status} />)}
      </Space>
    </div>
  );
}

const nodeFindingCodes = new Set([
  "DISK_LOW", "RUNTIME_UNAVAILABLE", "RUNTIME_PROVISION_FAILED", "RUNTIME_STATE_UNTRAVERSABLE", "PATH_NOEXEC", "PATH_READ_ONLY", "PATH_EPHEMERAL",
  "ACCELERATOR_PROBE_FAILED", "ACCELERATOR_UNRECOGNIZED", "CUDA_INIT_FAILED", "MULTI_GPU_CHECK_FAILED", "MACOS_UNSUPPORTED", "MACOS_IDLE_SLEEP",
  "PEER_MEMORY_MISSING", "UCX_NETLINK_HANG",
  // Why a check did not run.
  "MOUNT_TABLE_UNREADABLE", "CUDA_CHECK_NOT_PASSED", "CHECK_PREREQUISITE_MISSING", "CHECK_TOOL_FAILED", "CHECK_CANCELLED", "CHECK_BUDGET_EXHAUSTED",
  "CARD_UNADDRESSABLE",
]);

const findingColor: Record<DoctorFinding["status"], string> = { fail: "red", warn: "orange", skip: "default" };

/** A finding's name in the reader's language: the node finding, a card check's skip reason, or an accelerator health code. */
export function doctorFindingLabel(t: TFunction<"nodes">, tc: TFunction<"common">, finding: DoctorFinding): string {
  if (nodeFindingCodes.has(finding.code)) return t(`doctor.finding.${finding.code}.label`);
  if (finding.status === "skip") return t("doctor.notChecked", { reason: t(`cardHealth.skip.${finding.code}`, { defaultValue: finding.code }) });
  return gpuHealthLabel(tc, finding.status === "fail" ? "Error" : "Warning", finding.code);
}

/** A check that did not run on some cards: why, and which cards, never a verdict about them. */
function SkippedCardsFinding({ finding }: { finding: DoctorFinding }) {
  const { t } = useTranslation("nodes");
  const { t: tc } = useTranslation("common");
  const targets = finding.targets ?? [];
  return (
    <div className="node-doctor-finding flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <Badge status="neutral">{doctorFindingLabel(t, tc, finding)}</Badge>
        <span>{t("doctor.cards", { count: targets.length, indexes: targets.map((target) => target.index).join(", ") })}</span>
      </div>
      {nodeFindingCodes.has(finding.code) && <p className="m-0">{t(`doctor.finding.${finding.code}.summary`)}</p>}
      <HealthEvidence fields={finding.evidence} />
    </div>
  );
}

/** Renders a finding by what it is: a check that did not run, a card health finding, or a node finding. */
export function DoctorFindingView({ finding }: { finding: DoctorFinding }) {
  if (finding.status === "skip" && finding.targets?.length) return <SkippedCardsFinding finding={finding} />;
  if (finding.targets?.length) return <AcceleratorFinding finding={finding} />;
  return <NodeFinding finding={finding} />;
}

/** Filesystem type and the mount flags that matter to the reader. */
function MountTags({ mount }: { mount?: DoctorMount }) {
  const { t } = useTranslation("nodes");
  if (!mount) return null;
  const flags = [
    mount.network && "network", mount.noexec && "noexec", mount.read_only && "readOnly", mount.ephemeral && "ephemeral",
  ].filter((flag): flag is string => !!flag);
  return (
    <>
      <Typography.Text type="secondary">{mount.fs_type}</Typography.Text>
      {flags.map((flag) => <Tag key={flag} color={flag === "network" ? "blue" : "orange"}>{t(`doctor.mountFlag.${flag}`)}</Tag>)}
    </>
  );
}

function evidenceValue(finding: DoctorFinding, name: string): string {
  return finding.evidence?.find((field) => field.name === name)?.value ?? "";
}

/** Values a finding's sentence interpolates from its evidence rather than a disk. */
function evidenceParams(finding: DoctorFinding) {
  return {
    required: evidenceValue(finding, "required"),
    directory: evidenceValue(finding, "directory"),
    user: evidenceValue(finding, "user"),
    stateDir: evidenceValue(finding, "state_dir"),
    module: evidenceValue(finding, "module"),
    fix: evidenceValue(finding, "fix"),
  };
}

function NodeFinding({ finding }: { finding: DoctorFinding }) {
  const { t } = useTranslation("nodes");
  const key = nodeFindingCodes.has(finding.code) ? `doctor.finding.${finding.code}` : "";
  const impact = key ? t(`${key}.impact`, { ...evidenceParams(finding), defaultValue: "" }) : "";
  const disk = finding.disk;
  return (
    <div className="node-doctor-finding">
      <Space direction="vertical" size={8} style={{ display: "flex" }}>
        <Tag color={findingColor[finding.status] ?? "red"} style={{ alignSelf: "flex-start" }}>
          {key ? t(`${key}.label`) : finding.code}
        </Tag>
        {key && (
          <Typography.Text>
            {t(`${key}.summary`, disk ? {
              path: disk.path, avail: gib(disk.avail_bytes), total: gib(disk.total_bytes), percent: percent(disk.avail_bytes, disk.total_bytes),
              fstype: disk.mount?.fs_type ?? "", point: disk.mount?.point ?? "",
            } : evidenceParams(finding))}
          </Typography.Text>
        )}
        {impact && (
          <div className="gpu-health-section">
            <Typography.Text type="secondary" strong>{t("doctor.impact")}</Typography.Text>
            <Typography.Text>{impact}</Typography.Text>
          </div>
        )}
        {(finding.probe || !!finding.evidence?.length) && (
          <div className="gpu-health-section">
            <Typography.Text type="secondary" strong>{t("doctor.evidence")}</Typography.Text>
            {finding.probe && <HealthCommand command={finding.probe} />}
            <HealthEvidence fields={finding.evidence} />
          </div>
        )}
        <div className="gpu-health-section">
          <Typography.Text type="secondary" strong>{t("doctor.recheck")}</Typography.Text>
          {finding.recheck ? <HealthCommand command={finding.recheck} /> : <Typography.Text>{t("doctor.rerun")}</Typography.Text>}
        </div>
      </Space>
    </div>
  );
}

function factValue(t: TFunction<"nodes">, fact: DoctorFact): string {
  const { code, value } = fact;
  if (code === "runtime_plugin" && value === "none") return t("doctor.pluginNone");
  if (code === "device_injection" && value === "explicit") return t("doctor.injectionExplicit");
  if (code === "runtime_identity" && fact.identity) return identityText(t, fact.identity);
  if (code === "network_filesystem" && fact.disks?.length) return networkFilesystemText(t, fact.disks);
  return value;
}

function identityText(t: TFunction<"nodes">, identity: DoctorRunIdentity): string {
  if (identity.pending) return t("doctor.identity.pending", { runtime: identity.runtime });
  if (identity.user) return t("doctor.identity.dedicated", { user: identity.user, uid: identity.uid });
  if (identity.fallback) return t("doctor.identity.fallback", { uid: identity.uid, reason: identity.fallback });
  return t("doctor.identity.own", { uid: identity.uid });
}

function networkFilesystemText(t: TFunction<"nodes">, disks: DoctorDisk[]): string {
  const mount = disks[0].mount;
  const paths = disks.map((disk) => `${t(`doctor.pathKind.${disk.kind}`, { defaultValue: disk.kind })} ${disk.path}`);
  return t(mount?.source ? "doctor.networkFilesystemFrom" : "doctor.networkFilesystem", {
    point: mount?.point ?? "", fstype: mount?.fs_type ?? "", source: mount?.source ?? "", paths: paths.join(t("doctor.listSeparator")),
  });
}

/**
 * NodeDoctorReport renders the unified check list and hardware details.
 * Reports from older Servers still render their original findings or text.
 */
export default function NodeDoctorReport({ report }: { report: Report }) {
  const { t } = useTranslation("nodes");
  const { t: tc } = useTranslation("common");
  if (!report.host && !report.checks) {
    return (
      <Space direction="vertical" style={{ display: "flex" }}>
        <Alert type="info" showIcon message={t("doctor.legacy")} />
        <pre style={rawStyle}>{report.report}</pre>
      </Space>
    );
  }
  const host = report.host;
  const accel = report.accelerators;
  const findings = report.findings ?? [];
  const counts: Record<string, number> = { OK: 0, Warning: 0, Error: 0 };
  for (const gpu of accel?.gpus ?? []) counts[gpu.health_status] = (counts[gpu.health_status] ?? 0) + 1;
  return (
    <Space direction="vertical" size={16} style={{ display: "flex" }}>
      {report.checks && <NodeDoctorChecks report={report} renderFinding={(finding) => <DoctorFindingView finding={finding} />}
        findingLabel={(finding) => doctorFindingLabel(t, tc, finding)} />}
      {host && <section aria-label={t("doctor.overview")}>
        <Typography.Title level={5}>{t("doctor.overview")}</Typography.Title>
        <Descriptions size="small" column={1} bordered>
          <Descriptions.Item label={t("doctor.host")}>{`${host.hostname} · ${host.os}/${host.arch} · kernel ${host.kernel}`}</Descriptions.Item>
          <Descriptions.Item label={t("doctor.cpu")}>{`${host.cpu_model} × ${host.cpu_cores}`}</Descriptions.Item>
          <Descriptions.Item label={t("doctor.memory")}>{t("doctor.memoryValue", { total: gib(host.mem_total_bytes), avail: gib(host.mem_avail_bytes) })}</Descriptions.Item>
          <Descriptions.Item label={t("doctor.disk")}>
            <Space direction="vertical" size={0}>
              {host.disks.map((disk) => (
                <Space key={`${disk.kind}-${disk.path}`} size={4} wrap>
                  <Typography.Text>
                    {t("doctor.diskValue", { path: disk.path, total: gib(disk.total_bytes), avail: gib(disk.avail_bytes), percent: percent(disk.avail_bytes, disk.total_bytes) })}
                  </Typography.Text>
                  <MountTags mount={disk.mount} />
                </Space>
              ))}
            </Space>
          </Descriptions.Item>
        </Descriptions>
      </section>}

      {(host || report.runtimes?.length) && <section aria-label={t("doctor.runtimes")}>
        <Typography.Title level={5}>{t("doctor.runtimes")}</Typography.Title>
        <Descriptions size="small" column={1} bordered>
          {(report.runtimes ?? []).map((runtime) => (
            <Descriptions.Item key={runtime.name} label={runtimeLabel(runtime.name)}>
              <Space>
                <Typography.Text type={runtime.status === "ok" ? undefined : "secondary"}>
                  {runtime.status === "not_found" ? t("doctor.runtimeNotFound") : runtime.status === "query_failed" ? t("doctor.runtimeQueryFailed") : runtime.version || "—"}
                </Typography.Text>
                {runtime.selected && <Tag color="blue">{t("doctor.runtimeSelected")}</Tag>}
              </Space>
            </Descriptions.Item>
          ))}
        </Descriptions>
      </section>}

      {(host || accel) && <section aria-label={t("doctor.accelerators")}>
        <Typography.Title level={5}>{t("doctor.accelerators")}</Typography.Title>
        {!accel ? (
          <Typography.Text type="secondary">{t("doctor.noAccelerator")}</Typography.Text>
        ) : (
          <Space direction="vertical" style={{ display: "flex" }}>
            <Typography.Text>
              {t("doctor.acceleratorSummary", { vendor: accel.vendor, driver: accel.driver || "—", total: accel.gpus.length, ok: counts.OK, warning: counts.Warning, error: counts.Error })}
            </Typography.Text>
            <Table
              size="small"
              pagination={false}
              rowKey="index"
              dataSource={accel.gpus}
              columns={[
                { title: t("doctor.column.index"), dataIndex: "index", width: 48 },
                { title: t("doctor.column.name"), dataIndex: "name" },
                { title: t("doctor.column.memory"), dataIndex: "memory_bytes", render: (bytes: number) => gib(bytes) },
                { title: t("doctor.column.bdf"), dataIndex: "pcie_bdf", render: (bdf?: string) => bdf || "—" },
                { title: t("doctor.column.numa"), dataIndex: "numa_node" },
                {
                  title: t("doctor.column.health"),
                  key: "health",
                  render: (_: unknown, gpu: NonNullable<Report["accelerators"]>["gpus"][number]) => (
                    <Tag color={healthColor[gpu.health_status] ?? "red"}>{gpuHealthLabel(tc, gpu.health_status in healthColor ? gpu.health_status : "Error", gpu.health_code)}</Tag>
                  ),
                },
              ]}
            />
          </Space>
        )}
      </section>}

      {!report.checks && <section aria-label={t("doctor.findings")}>
        <Typography.Title level={5}>{t("doctor.findings")}</Typography.Title>
        {findings.length === 0 ? (
          <Typography.Text type="secondary">{t("doctor.noFindings")}</Typography.Text>
        ) : (
          <Space direction="vertical" size={12} style={{ display: "flex" }}>
            {findings.map((finding, i) => <DoctorFindingView key={`${finding.code}-${i}`} finding={finding} />)}
          </Space>
        )}
      </section>}

      {!!report.facts?.length && (
        <section aria-label={t("doctor.facts")}>
          <Typography.Title level={5}>{t("doctor.facts")}</Typography.Title>
          <Descriptions size="small" column={1} bordered>
            {report.facts.map((fact, i) => (
              <Descriptions.Item key={`${fact.code}-${i}`} label={t(`doctor.fact.${fact.code}`, { defaultValue: fact.code })}>
                <Typography.Text style={{ whiteSpace: "pre-wrap" }}>{factValue(t, fact)}</Typography.Text>
              </Descriptions.Item>
            ))}
          </Descriptions>
        </section>
      )}

      <Collapse size="small" items={[{ key: "raw", label: t("doctor.rawToggle"), children: <pre style={rawStyle}>{report.report}</pre> }]} />
    </Space>
  );
}
