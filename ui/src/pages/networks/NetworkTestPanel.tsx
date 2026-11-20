import { useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { Button } from "../../components/ui/button";
import { Combobox, ComboboxChip, ComboboxChips, ComboboxChipsInput, ComboboxContent, ComboboxItem, ComboboxList, useComboboxAnchor } from "../../components/ui/combobox";
import { Input } from "../../components/ui/input";
import { Switch } from "../../components/ui/switch";
import { FormField } from "../../ui-kit/form/FormField";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Notice } from "../../ui-kit/Notice";
import { NumberInput } from "../../ui-kit/NumberInput";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { message } from "../../ui-kit/feedback";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { NetworkDetail, NetworkTestEstimate, NetworkTestPattern, NetworkTestRequest } from "../../networkTypes";
import { isAcceleratorRDMA, isHostRDMA, usesRails } from "./networkDisplay";
import { isCardKind } from "./cardTest";
import NetworkTestResult from "./NetworkTestResult";

/** Above this many nodes a test defaults to a ring: a mesh grows as N². */
export const ringDefaultAbove = 16;

interface FormValues {
  kind: NonNullable<NetworkTestRequest["kind"]>;
  pattern: NetworkTestPattern;
  scope: NetworkTestRequest["scope"];
  node_uuids?: string[];
  label_key?: string;
  label_value?: string;
  bandwidth: boolean;
  seconds: number | null;
}

/** Seconds as the viewer reads a duration: seconds below a minute, minutes above. */
function duration(t: (key: string, options?: Record<string, unknown>) => string, seconds: number) {
  return seconds < 60 ? t("test.durationSeconds", { count: seconds }) : t("test.durationMinutes", { value: (seconds / 60).toFixed(1).replace(/\.0$/, "") });
}

/** The request a form's values stand for. Ethernet networks have no kind choice; they only run TCP tests. */
function testBody(v: FormValues): NetworkTestRequest {
  const kind = v.kind ?? "tcp";
  const body: NetworkTestRequest = isCardKind(kind)
    ? { kind, pattern: v.pattern, scope: v.scope }
    : { kind, pattern: v.pattern, scope: v.scope, bandwidth: kind === "tcp" ? v.bandwidth : true, seconds: v.seconds ?? undefined };
  if (v.scope === "nodes") body.node_uuids = v.node_uuids;
  if (v.scope === "label") { body.label_key = v.label_key?.trim(); body.label_value = v.label_value?.trim() ?? ""; }
  return body;
}

/** Keep a fleet-wide prerequisite warning readable while retaining all affected names. */
function NodeWarning({ nodes, messageKey }: { nodes: string[]; messageKey: string }) {
  const { t } = useTranslation("networks");
  if (!nodes.length) return null;
  if (nodes.length <= 3) return <Notice tone="warning" title={t(messageKey, { nodes: nodes.join(", ") })} />;
  return <Notice tone="warning" title={t(`${messageKey}Group`, { count: nodes.length })}>
    <details><summary className="cursor-pointer text-fg-default">{t("test.viewNodes", { count: nodes.length })}</summary>
      <ul className="mt-1 max-h-32 overflow-y-auto break-all">{nodes.map((name, i) => <li key={i}>{name}</li>)}</ul>
    </details>
  </Notice>;
}

/** Starts a network's internal test and shows the latest result. */
export default function NetworkTestPanel({ network, onStarted, onChanged }: { network: NetworkDetail; onStarted: () => void; onChanged: (detail: NetworkDetail) => void }) {
  const { t } = useTranslation("networks");
  const form = useForm<FormValues>({ defaultValues: {
    kind: isAcceleratorRDMA(network.type) ? "card_ping" : "tcp",
    pattern: network.nodes.length > ringDefaultAbove ? "ring" : "mesh",
    scope: "all", bandwidth: true, seconds: 3, node_uuids: [],
  } });
  const { kind, pattern, scope, node_uuids: chosen, label_key: labelKey, label_value: labelValue } = form.watch();
  const nodeAnchor = useComboboxAnchor();
  const nodeNames = new Map(network.nodes.map(n => [n.node_uuid, n.node_name]));
  // Nodes that cannot run ib_write_bw, named before the test starts.
  const kindOptions = [
    { value: "tcp", label: t("test.kindTcp") },
    ...(isHostRDMA(network.type) ? [{ value: "rdma_tool", label: t("test.kindRdmaTool") }] : []),
    ...(isAcceleratorRDMA(network.type) ? [{ value: "card_ping", label: t("test.kindCardPing") }, { value: "card_matrix", label: t("test.kindCardMatrix") }] : []),
  ];
  const card = isCardKind(kind);
  // How large a card test is, and whether it may start, is the server's to
  // say: it resolves the scope to nodes and lays the rounds out as the run
  // will. Each change of the form asks again.
  const [estimate, setEstimate] = useState<{ value?: NetworkTestEstimate; error?: string }>({});

  useEffect(() => {
    setEstimate({});
    if (!card || !scope) return;
    if (scope === "nodes" && (chosen ?? []).length < 2) return;
    if (scope === "label" && !labelKey?.trim()) return;
    let stale = false;
    const timer = setTimeout(() => {
      api.estimateNetworkTest(network.id, testBody({ kind, pattern, scope, node_uuids: chosen, label_key: labelKey, label_value: labelValue, bandwidth: false, seconds: 0 }))
        .then((value) => { if (!stale) setEstimate({ value }); })
        .catch((e: Error) => { if (!stale) setEstimate({ error: e.message }); });
    }, 200);
    return () => { stale = true; clearTimeout(timer); };
  }, [card, kind, pattern, scope, chosen, labelKey, labelValue, network.id]);
  const missingTool = network.nodes.filter((n) => !n.tools?.includes("ib_write_bw")).map((n) => n.node_name);
  // Nodes whose pairs fail before any traffic: said before the test starts.
  const noAddress = network.nodes.filter((n) => !n.ip).map((n) => n.node_name);
  const noRails = usesRails(network.type) ? network.nodes.filter((n) => n.hcas.length === 0).map((n) => n.node_name) : [];
  const [starting, setStarting] = useState(false);
  const run = network.latest_test;
  const running = run?.status === "running";

  if (!network.test_supported) return <Notice title={t("test.unsupported")} />;

  const start = form.handleSubmit(async (v) => {
    if (running || starting || (card && estimate.error)) return;
    const body = testBody(v);
    setStarting(true);
    try {
      await api.startNetworkTest(network.id, body);
      message.success(t("test.started"));
      onStarted();
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    } finally {
      setStarting(false);
    }
  });

  return (
    <div className="isolate flex min-w-0 flex-col gap-6 text-body">
      <form onSubmit={start} className="@container flex min-w-0 flex-col gap-3 rounded-xl border border-border bg-muted p-4 [&_[data-slot=select-trigger]]:bg-surface-floating [&_input[data-slot=input]]:bg-surface-floating [&_[data-slot=combobox-chips]]:bg-surface-floating">
        <h3 className="flex items-center gap-1 text-body font-semibold">
          {t("test.configuration")}
          <HelpTip content={t(card ? `test.cardNote.${kind}` : kind === "rdma_tool" ? "test.rdmaHow" : "test.what")} />
        </h3>
        <div className="grid grid-cols-1 items-start gap-x-4 gap-y-3 @min-[420px]:grid-cols-2">
          {kindOptions.length > 1 && <FormField control={form.control} name="kind" label={t("test.kind")}>
            {field => <OptionSelect id={field.id} aria-label={t("test.kind")} value={field.value} onChange={field.onChange} options={kindOptions} />}
          </FormField>}
          <FormField control={form.control} name="pattern" label={<>{t("test.pattern")} <HelpTip label={t("test.pattern")} content={t(card ? `test.cardPatternHint.${pattern}` : `test.patternHint.${pattern}`)} /></>}>
            {field => <OptionSelect id={field.id} aria-label={t("test.pattern")} value={field.value} onChange={field.onChange} options={["mesh", "ring"].map(value => ({ value, label: t(`test.patternName.${value}`) }))} />}
          </FormField>
          <FormField control={form.control} name="scope" label={t("test.scope")}>
            {field => <OptionSelect id={field.id} aria-label={t("test.scope")} value={field.value} onChange={field.onChange} options={[
              { value: "all", label: t("test.scopeAll") }, { value: "nodes", label: t("test.scopeNodes") }, { value: "label", label: t("test.scopeLabel") },
            ]} />}
          </FormField>
          {!card && <FormField control={form.control} name="seconds" label={t("test.seconds")}>
            {field => <NumberInput {...field} min={1} max={10} precision={0} />}
          </FormField>}
          {scope === "nodes" && <div className="col-span-full">
            <FormField control={form.control} name="node_uuids" label={t("test.nodes")} required rules={{ validate: value => (value?.length ?? 0) >= 2 || t("test.nodesRequired") }}>
              {field => <Combobox multiple items={[...nodeNames.keys()]} value={field.value ?? []} onValueChange={field.onChange} itemToStringLabel={id => nodeNames.get(id) ?? id}>
                <ComboboxChips ref={nodeAnchor}>{(field.value ?? []).map(id => <ComboboxChip key={id} removeAriaLabel={`${t("common:actions.remove")} ${nodeNames.get(id) ?? id}`}>{nodeNames.get(id) ?? id}</ComboboxChip>)}
                  <ComboboxChipsInput ref={field.ref} id={field.id} aria-label={t("test.nodes")} aria-required aria-invalid={field["aria-invalid"]} aria-describedby={field["aria-describedby"]} />
                </ComboboxChips>
                <ComboboxContent anchor={nodeAnchor}><ComboboxList>{(id: string) => <ComboboxItem key={id} value={id}>{nodeNames.get(id) ?? id}</ComboboxItem>}</ComboboxList></ComboboxContent>
              </Combobox>}
            </FormField>
          </div>}
          {scope === "label" && <>
            <FormField control={form.control} name="label_key" label={t("test.labelKey")} required rules={{ validate: value => !!value?.trim() || t("attach.labelKeyRequired") }}>
              {field => <Input {...field} value={field.value ?? ""} placeholder="rack" />}
            </FormField>
            <FormField control={form.control} name="label_value" label={t("test.labelValue")}>
              {field => <Input {...field} value={field.value ?? ""} placeholder="r3" />}
            </FormField>
          </>}
        </div>
        {kind === "tcp" && <Switch label={t("test.bandwidth")} checked={form.watch("bandwidth")} onCheckedChange={value => form.setValue("bandwidth", value)} />}
        <NodeWarning nodes={noAddress} messageKey={usesRails(network.type) ? "test.noBootstrap" : "test.noAddress"} />
        {(kind === "rdma_tool" || card) && <NodeWarning nodes={noRails} messageKey="test.noRails" />}
        {usesRails(network.type) && kind === "tcp" && <p className="text-label text-fg-muted">{t(isHostRDMA(network.type) ? "test.rdmaNote" : "test.acceleratorNote")}</p>}
        {kind === "rdma_tool" && (missingTool.length
          ? <NodeWarning nodes={missingTool} messageKey="test.toolMissing" />
          : <p className="text-label text-fg-muted">{t("test.toolReady")}</p>)}
        {card && (estimate.error
          ? <Notice tone="danger" title={t("test.cardEstimateFailed", { detail: estimate.error })} />
          : <p className="text-label text-fg-muted" aria-live="polite">{estimate.value?.card
            ? t("test.cardEstimate", { nodes: estimate.value.node_count, pairs: estimate.value.pair_count, probes: estimate.value.card.probes, rounds: estimate.value.card.rounds, duration: duration(t, estimate.value.card.seconds) })
            : t("test.cardEstimating")}</p>)}
        <div><Button type="submit" loading={starting} disabled={running || starting || (card && !!estimate.error)}>{t("test.start")}</Button></div>
      </form>
      <NetworkTestResult network={network} run={run} onChanged={onChanged} />
    </div>
  );
}
