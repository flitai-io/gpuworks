import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type ArchiveImageMetadata, type NodeImageSelection, type RuntimeImage, type RuntimeImageList } from "../../api";
import { Alert, AlertDescription } from "../../components/ui/alert";
import { Button } from "../../components/ui/button";
import NodePicker from "../../nodes/NodePicker";
import { OptionSelect } from "../../ui-kit/OptionSelect";

type Props = {
  disabled: boolean;
  onClear: () => void;
  onSelect: (selection: NodeImageSelection, metadata: ArchiveImageMetadata) => void;
};

export default function RuntimeImagePicker({ disabled, onClear, onSelect }: Props) {
  const { t } = useTranslation("images");
  const [node, setNode] = useState("");
  const [list, setList] = useState<RuntimeImageList>();
  const [picked, setPicked] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const pending = useRef<AbortController | null>(null);
  useEffect(() => () => pending.current?.abort(), []);

  function begin() {
    pending.current?.abort();
    const request = new AbortController();
    pending.current = request;
    onClear();
    setPicked("");
    setError("");
    setBusy(true);
    return request;
  }
  async function scan(nextNode: string, ns: string) {
    const request = begin();
    if (node !== nextNode) setList(undefined);
    else setList(last => last && { ...last, namespace: ns, items: [] });
    setNode(nextNode);
    try {
      if (!nextNode) return;
      const images = await api.runtimeImages(nextNode, ns, request.signal);
      if (!request.signal.aborted) setList(images);
    } catch (e) {
      if (!request.signal.aborted) setError(e instanceof Error ? e.message : t("import.runtime.failed"));
    } finally {
      if (!request.signal.aborted) setBusy(false);
    }
  }
  async function pick(row: RuntimeImage) {
    if (!list) return;
    const request = begin();
    setPicked(row.ref);
    const selection: NodeImageSelection = {
      node_uuid: node, ref: row.ref, identity: row.identity,
      namespace: list.namespace, target_namespace: list.target_namespace, runtime: list.runtime,
    };
    try {
      const { metadata } = await api.inspectRuntimeImage(selection, request.signal);
      if (!request.signal.aborted) onSelect(selection, metadata);
    } catch (e) {
      if (!request.signal.aborted) {
        setPicked("");
        setError(e instanceof Error ? e.message : t("import.runtime.failed"));
      }
    } finally {
      if (!request.signal.aborted) setBusy(false);
    }
  }
  return <div className="grid gap-3">
    <label htmlFor="local-image-node">{t("nodeTable.column.node")}</label>
    <NodePicker id="local-image-node" value={node} disabled={disabled} filter={{ state: ["online"] }}
      onChange={value => void scan(String(value || ""), "")} />
    {list?.target_namespace && <div className="grid gap-1">
      <label htmlFor="local-image-namespace">{t("import.runtime.namespace")}</label>
      <OptionSelect id="local-image-namespace" value={list.namespace} disabled={disabled}
        options={(list.namespaces ?? []).map(ns => ({ value: ns, label: ns }))}
        onChange={ns => void scan(node, ns)} />
      <small>{t("import.runtime.target", { ns: list.target_namespace })}</small>
    </div>}
    {node && <section aria-busy={busy} className="grid gap-2">
      <div className="flex items-center justify-between">
        <strong>{t("import.runtime.list", { runtime: list?.runtime || "" })}</strong>
        <Button type="button" size="sm" disabled={disabled} onClick={() => void scan(node, list?.namespace || "")}>{t("page.refresh")}</Button>
      </div>
      {!!list?.items.length && <div className="max-h-64 overflow-auto"><table className="w-full text-left text-sm">
        <thead><tr><th>{t("import.runtime.name")}</th><th>{t("import.runtime.tag")}</th><th>{t("import.runtime.size")}</th></tr></thead>
        <tbody>{list.items.map(row => {
          const colon = row.ref.lastIndexOf(":");
          return <tr key={row.ref}><td><label className="flex items-center gap-2 py-1"><input type="radio" name="runtime-image" aria-label={row.ref}
            checked={picked === row.ref} disabled={disabled} onChange={() => void pick(row)} />{row.ref.slice(0, colon)}</label></td>
            <td>{row.ref.slice(colon + 1)}</td><td>{row.size}</td></tr>;
        })}</tbody>
      </table></div>}
      {busy && <span role="status">{t(picked ? "import.runtime.reading" : "import.runtime.scanning")}</span>}
      {error && <Alert status="danger"><AlertDescription>{error}</AlertDescription></Alert>}
      {!busy && !error && list?.items.length === 0 && <span role="status">{t("import.runtime.empty")}</span>}
      {!busy && !error && picked && <span role="status">{t("import.runtime.ready")}</span>}
    </section>}
  </div>;
}
