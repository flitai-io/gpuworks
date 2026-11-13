import { useEffect, useState } from "react";
import { Alert, Badge, Button, Popconfirm, Space, Table, Tag, Typography, message } from "antd";
import { Badge as ZeronBadge } from "./ui/badge";
import { Tooltip } from "./ui/tooltip";
import type { ColumnsType } from "antd/es/table";
import { localizedText } from "../i18n/localizedText";
import { isCommandChannel } from "../providerConnections";
import { api, type ProviderInstance, type ProviderKindInfo } from "../api";
import ProviderInstallModal from "./ProviderInstallModal";
import { useTranslation } from "react-i18next";

const powerBadge: Record<string, { status: "success" | "default" | "warning"; labelKey: string }> = {
  running: { status: "success", labelKey: "instances.power.running" },
  stopped: { status: "default", labelKey: "instances.power.stopped" },
  unknown: { status: "warning", labelKey: "instances.power.unknown" },
};

const INSTANCE_PAGE_SIZE = 10;

export default function ProviderInstancesTable({
  providerId,
  kindInfo,
  refreshKey,
  showRegion = true,
}: {
  providerId: number;
  kindInfo?: ProviderKindInfo;
  refreshKey: string;
  showRegion?: boolean;
}) {
  const { t, i18n } = useTranslation("providers");
  const { t: tCommon } = useTranslation("common");
  const language = i18n.resolvedLanguage || i18n.language;
  const [rows, setRows] = useState<ProviderInstance[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [installFor, setInstallFor] = useState<ProviderInstance | null>(null);
  const [powering, setPowering] = useState<string | null>(null);

  const load = () => {
    setLoadError(null);
    api
      .providerInstances(providerId)
      .then((list) => setRows(list.items))
      .catch((error: Error) => {
        setRows([]);
        setLoadError(error.message);
      });
  };
  useEffect(load, [providerId, refreshKey]);

  const doPower = async (inst: ProviderInstance, action: "on" | "off") => {
    setPowering(`${inst.external_id}:${action}`);
    try {
      await api.powerProviderInstance(providerId, inst.external_id, action);
      message.success(t(action === "on" ? "instances.message.powerOnSent" : "instances.message.powerOffSent"));
      load();
    } catch (e) {
      message.error(t("instances.message.actionFailed", { detail: (e as Error).message }));
    } finally {
      setPowering(null);
    }
  };

  const columns: ColumnsType<ProviderInstance> = [
    {
      title: t("instances.column.name"),
      dataIndex: "name",
      width: 200,
      fixed: "left",
      ellipsis: { showTitle: true },
      render: (name: string, inst: ProviderInstance) => name || inst.external_id || "—",
    },
    {
      title: t("instances.column.id"),
      dataIndex: "external_id",
      width: 180,
      ellipsis: true,
      render: (id: string) => id || "—",
    },
    { title: t("instances.column.gpuModel"), dataIndex: "gpu_model", width: 150, ellipsis: true, render: (m: string) => m || t("instances.notReturned") },
    { title: t("instances.column.gpuCount"), dataIndex: "gpu_count", width: 90, render: (count: number) => count || "—" },
    {
      title: t("instances.column.sshConnection"),
      key: "ssh_endpoint",
      width: 280,
      render: (_: unknown, inst: ProviderInstance) => {
        const connections = inst.ssh_connections?.length
          ? inst.ssh_connections
          : inst.public_ip && inst.ssh_port
            ? [{ id: "legacy", type: "direct", label: "SSH", host: inst.public_ip, port: inst.ssh_port, user: inst.ssh_user || "root", install_supported: true }]
            : [];
        if (!connections.length) return t("instances.notReturned");
        return (
          <Space direction="vertical" size={4} style={{ width: "100%" }}>
            {connections.map((connection) => {
              if (isCommandChannel(connection)) {
                return (
                  <Space key={connection.id} size={6} wrap>
                    <Tag color="blue">{localizedText(connection.label, language) || connection.type}</Tag>
                    <Typography.Text type="secondary">{t("instances.connection.noSSH")}</Typography.Text>
                  </Space>
                );
              }
              const user = connection.user || "root";
              const command = `ssh ${user}@${connection.host} -p ${connection.port}`;
              return (
                <Space key={connection.id} size={6} wrap>
                  <Tag>{localizedText(connection.label, language) || t(connection.type === "direct" ? "instances.connection.direct" : "instances.connection.proxy")}</Tag>
                  <Typography.Text copyable={{ text: command }} ellipsis={{ tooltip: command }} style={{ maxWidth: 170 }}>
                    {user}@{connection.host}:{connection.port}
                  </Typography.Text>
                  {!connection.install_supported && <Typography.Text type="secondary">{t("instances.connection.terminalOnly")}</Typography.Text>}
                </Space>
              );
            })}
          </Space>
        );
      },
    },
    {
      title: t("instances.column.status"),
      dataIndex: "power",
      width: 110,
      render: (s: string) => {
        const b = powerBadge[s];
        return <Badge status={b?.status ?? "warning"} text={b ? t(b.labelKey) : s || t("instances.power.unknown")} />;
      },
    },
    ...(kindInfo?.capabilities.has_pricing === true ? [{
      title: t("instances.column.price"),
      dataIndex: "price_per_hr",
      width: 110,
      render: (p: number) => t("instances.pricePerHour", { price: p.toFixed(2) }),
    }] : []),
    ...(showRegion ? [{ title: t("instances.column.region"), dataIndex: "region", width: 120, ellipsis: true, render: (r: string) => r || "—" }] : []),
    {
      title: t("instances.column.type"),
      dataIndex: "kind",
      width: 100,
      render: (k: string) => (k === "container" ? t("instances.type.container") : k === "vm" ? t("instances.type.vm") : k || "—"),
    },
    {
      title: tCommon("resourceTable.actions"),
      key: "action",
      width: 200,
      fixed: "right",
      render: (_: unknown, inst: ProviderInstance) => {
        // The server says what the instance allows; nothing here decides it.
        const actions = inst.actions ?? [];
        const powerAction = actions.includes("power_off") ? "off" : actions.includes("power_on") ? "on" : null;
        const powerButton = powerAction ? (
          powerAction === "off" ? (
            <Popconfirm
              title={t("instances.powerOffConfirm")}
              okText={t("instances.powerOff")}
              cancelText={t("instances.cancel")}
              onConfirm={() => doPower(inst, "off")}
            >
              <Button
                type="link"
                size="small"
                danger
                loading={powering === `${inst.external_id}:off`}
              >
                {t("instances.powerOff")}
              </Button>
            </Popconfirm>
          ) : (
            <Button
              type="link"
              size="small"
              loading={powering === `${inst.external_id}:on`}
              onClick={() => doPower(inst, "on")}
            >
              {t("instances.powerOn")}
            </Button>
          )
        ) : null;
        return (
          <>
            {powerButton}
            {inst.managed_cluster ? (
              <Tooltip content={t("instances.clusterNodeHint", { cluster: inst.managed_cluster })}>
                <ZeronBadge color="purple">{t("instances.clusterNode")}</ZeronBadge>
              </Tooltip>
            ) : actions.includes("install") ? (
              <Button type="link" size="small" onClick={() => setInstallFor(inst)}>
                {t("instances.installAgent")}
              </Button>
            ) : (
              <Tag color="green">{t("instances.onboarded")}</Tag>
            )}
          </>
        );
      },
    },
  ];

  return (
    <>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 12 }}
        message={t("instances.containerRuntimeNote")}
      />
      {loadError && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 12 }}
          message={t("instances.loadFailed")}
          description={loadError}
          action={<Button size="small" onClick={load}>{t("instances.retry")}</Button>}
        />
      )}
      <Table<ProviderInstance>
        rowKey="external_id"
        size="small"
        columns={columns}
        dataSource={rows ?? []}
        loading={rows === null}
        // Every column has a width and the table scrolls sideways rather
        // than squeezing the instance name into a character-wide strip.
        tableLayout="fixed"
        scroll={{ x: columns.reduce((width, column) => width + Number(column.width ?? 0), 0), y: 420 }}
        // A provider can hold hundreds of instances: the expanded row pages
        // instead of stretching the provider list.
        pagination={(rows?.length ?? 0) > INSTANCE_PAGE_SIZE ? { defaultPageSize: INSTANCE_PAGE_SIZE, size: "small", showSizeChanger: true, pageSizeOptions: [10, 20, 50] } : false}
        locale={{ emptyText: t("instances.empty") }}
      />
      <ProviderInstallModal
        providerId={providerId}
        providerKind={kindInfo?.kind}
        defaultServerDialListen={kindInfo?.default_server_dial_listen}
        serverDialSupported={kindInfo?.server_dial_supported ?? true}
        serverDialAutoPort={kindInfo?.server_dial_auto_port ?? false}
        inst={installFor}
        onClose={() => setInstallFor(null)}
        onDone={() => {
          setInstallFor(null);
          load();
        }}
      />
    </>
  );
}
