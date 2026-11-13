import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { APIError, apiErrorText } from "../apiError";
import { outboundPolicyApi, type OutboundHostCheck, type OutboundPolicy } from "../outboundPolicyApi";
import { Checkbox } from "./ui/checkbox";
import { useIsAdmin } from "./useIsAdmin";

export function outboundHostName(raw: string): string {
  const value = raw.trim();
  if (!value) return "";
  try {
    const address = value.includes("://") ? value
      : value.includes(":") && !value.includes("[") && value.split(":").length > 2
        ? `http://[${value}]` : `http://${value}`;
    const url = new URL(address);
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password) return "";
    const host = url.hostname.replace(/^\[|\]$/g, "").toLowerCase().replace(/\.$/, "");
    const mapped = /^::ffff:([\da-f]{1,4}):([\da-f]{1,4})$/.exec(host);
    if (!mapped) return host;
    const high = Number.parseInt(mapped[1], 16), low = Number.parseInt(mapped[2], 16);
    return `${high >> 8}.${high & 255}.${low >> 8}.${low & 255}`;
  } catch { return ""; }
}

export interface OutboundHostAccessState {
  enabled: boolean;
  admin: boolean;
  hosts: string[];
  internationalHosts: string[];
  policy: OutboundPolicy | null;
  checks: OutboundHostCheck[];
  selected: string[];
  pending: boolean;
  error: unknown;
  toggle: (host: string, checked: boolean) => void;
  retry: () => void;
  authorize: () => Promise<void>;
}

/** Permission belongs to the current host, and is saved only by a form action. */
export function useOutboundHostAccess(addresses: readonly string[], enabled = true): OutboundHostAccessState {
  const { t } = useTranslation("common");
  const key = [...new Set(addresses.map(outboundHostName).filter(Boolean))].join("\n");
  const hosts = useMemo(() => key ? key.split("\n") : [], [key]);
  const internationalKey = [...new Set(addresses.filter((raw) => {
    const authority = raw.trim().replace(/^[a-z][a-z\d+.-]*:\/\//i, "").split(/[/?#]/, 1)[0];
    return [...authority].some((character) => character.charCodeAt(0) > 127);
  }).map(outboundHostName).filter(Boolean))].join("\n");
  const internationalHosts = internationalKey ? internationalKey.split("\n") : [];
  const canCheck = enabled && !internationalKey;
  const admin = useIsAdmin(canCheck && hosts.length > 0);
  const [policy, setPolicy] = useState<OutboundPolicy | null>(null);
  const [checks, setChecks] = useState<OutboundHostCheck[]>([]);
  const [selected, setSelected] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [reload, setReload] = useState(0);
  const current = useRef({ key, enabled: canCheck, selected });
  current.current = { key, enabled: canCheck, selected };

  useEffect(() => {
    setSelected([]);
    setChecks([]);
    setPolicy(null);
    setError(null);
    setLoading(false);
    if (!canCheck || !admin || hosts.length === 0) return;
    const controller = new AbortController();
    let active = true;
    setLoading(true);
    const timer = setTimeout(() => {
      void (async () => {
        try {
          const next = await outboundPolicyApi.get(controller.signal);
          const states = next.restrict_private_hosts ? await outboundPolicyApi.checkHosts(hosts, controller.signal) : [];
          if (active) { setPolicy(next); setChecks(states); }
        } catch (failure) {
          if (active) setError(failure);
        } finally { if (active) setLoading(false); }
      })();
    }, 250);
    return () => { active = false; clearTimeout(timer); controller.abort(); };
  }, [canCheck, admin, hosts, reload]);

  const authorize = async () => {
    const requested = hosts.filter((host) => selected.includes(host));
    if (!canCheck || !admin || hosts.length === 0) return;
    const stillSelected = () => current.current.enabled && current.current.key === key
      && requested.every((host) => current.current.selected.includes(host));
    setSaving(true);
    setError(null);
    try {
      const latest = await outboundPolicyApi.get();
      if (!stillSelected()) {
        if (requested.length > 0) throw new Error(t("outboundAccess.changed"));
        return;
      }
      setPolicy(latest);
      if (!latest.restrict_private_hosts) { setPolicy(latest); setSelected([]); return; }
      const inspected = requested.length ? requested : hosts;
      const fresh = await outboundPolicyApi.checkHosts(inspected);
      if (!stillSelected()) {
        if (requested.length > 0) throw new Error(t("outboundAccess.changed"));
        return;
      }
      setChecks((previous) => [...previous.filter((item) => !inspected.includes(item.host)), ...fresh]);
      if (requested.length === 0) return;
      const states = requested.map((host) => fresh.find((item) => item.host === host)?.status ?? "unresolved");
      if (states.some((status) => status === "blocked" || status === "unresolved")) {
        setChecks((previous) => [...previous.filter((item) => !requested.includes(item.host)), ...fresh]);
        setSelected([]);
        throw new Error(t(states.includes("blocked") ? "outboundAccess.cannotAllow" : "outboundAccess.checkFailed"));
      }
      const merged = [...latest.allow_private_hosts];
      for (const [index, host] of requested.entries()) {
        if (states[index] === "needs_allow" && !merged.some((entry) => entry.toLowerCase().replace(/\.$/, "") === host)) merged.push(host);
      }
      if (merged.length !== latest.allow_private_hosts.length) {
        if (!latest.editable) throw new APIError("SERVER_OUTBOUND_POLICY_READ_ONLY", undefined, t("outboundAccess.readOnly"), 409);
        const updated = await outboundPolicyApi.put({
          restrict_private_hosts: latest.restrict_private_hosts, allow_private_hosts: merged, revision: latest.revision,
        });
        setPolicy(updated);
      }
      setSelected([]);
      setReload((value) => value + 1);
    } catch (failure) {
      setError(failure);
      if (requested.length > 0) throw failure;
    } finally { setSaving(false); }
  };

  return {
    enabled, admin, hosts, internationalHosts, policy, checks, selected, pending: loading || saving, error,
    toggle: (host, checked) => setSelected((previous) => checked ? [...new Set([...previous, host])] : previous.filter((item) => item !== host)),
    retry: () => setReload((value) => value + 1), authorize,
  };
}

export function OutboundHostAccess({ access }: { access: OutboundHostAccessState }) {
  const { t } = useTranslation("common");
  if (!access.enabled || access.hosts.length === 0) return null;
  const settings = <a className="underline" href="/settings#outbound-policy">{t("outboundAccess.settings")}</a>;
  if (access.internationalHosts.length > 0) return <p role="alert" className="text-sm text-fg-muted">
    {t("outboundAccess.internationalHost", { hosts: access.internationalHosts.join(", ") })}
  </p>;
  if (!access.admin) return <p className="text-sm text-fg-muted">{t("outboundAccess.adminOnly")} {settings}</p>;
  if (access.pending && !access.policy) return <p role="status" className="text-sm text-fg-muted">{t("outboundAccess.checking")}</p>;
  if (!access.policy && !access.error) return null;
  if (access.policy && !access.policy.restrict_private_hosts && !access.error) return null;
  const retry = <button type="button" className="underline" disabled={access.pending} onClick={access.retry}>{t("outboundAccess.retry")}</button>;
  return <div className="space-y-2 text-sm text-fg-muted">
    <p>{t("outboundAccess.scope")} {settings}</p>
    {access.policy && !access.policy.editable && <p>{t("outboundAccess.readOnly")}</p>}
    {access.hosts.map((host) => {
      const status = access.checks.find((item) => item.host === host)?.status ?? "unresolved";
      if (status === "needs_allow") return <label key={host} className="flex items-center gap-2">
        <Checkbox checked={access.selected.includes(host)} disabled={!access.policy?.editable || access.pending}
          onCheckedChange={(checked) => access.toggle(host, checked === true)} />
        {t("outboundAccess.allow", { host })}
      </label>;
      return <p key={host}>{t(`outboundAccess.${status}`, { host })}</p>;
    })}
    {access.checks.some((item) => item.status === "needs_allow") && <p>{t("outboundAccess.appliedOnAction")}</p>}
    {access.error != null && <p role="alert">{apiErrorText((name, options) => t(name, options), access.error)}</p>}
    {(access.error != null || access.checks.some((item) => item.status === "unresolved")) && retry}
  </div>;
}
