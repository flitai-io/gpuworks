import { specDisplayName } from "../../inferenceTemplateFlat";
import DetailDrawer from "../../../components/detail/DetailDrawer";
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { InferenceTemplate } from "../../../api";
import EmptyState from "../../../components/EmptyState";
import { Button } from "../../../components/ui/button";
import { HugeiconsIcon } from "@hugeicons/react";
import LinkSquare02Icon from "@hugeicons/core-free-icons/LinkSquare02Icon";
import GitBranchIcon from "@hugeicons/core-free-icons/GitBranchIcon";
import Calendar03Icon from "@hugeicons/core-free-icons/Calendar03Icon";
import Download04Icon from "@hugeicons/core-free-icons/Download04Icon";
import FavouriteIcon from "@hugeicons/core-free-icons/FavouriteIcon";
import LicenseIcon from "@hugeicons/core-free-icons/LicenseIcon";
import MarkdownContent from "../../../components/MarkdownContent";
import { CatalogAvatar } from "./CatalogAvatar";
import {catalogSignal, readHub, providers, purposeLabel, problem, repoUrl, type Provider, type HubModel} from "./hubs";

export default function CatalogDetails({
  source,
  model,
  version,
  templates,
  closeDrawer,
  retryMatches,
}: {
  source: Provider;
  model: HubModel;
  version: string;
  templates: InferenceTemplate[];
  closeDrawer?: () => void;
  retryMatches?: () => void;
}) {
  const { t, i18n } = useTranslation("models");
  const navigate = useNavigate();
  const fmt = new Intl.NumberFormat(i18n.language, {notation:"compact", maximumFractionDigits:1});
  const url = repoUrl(source, model.id);
  const base =
    url +
    "/resolve/" +
    encodeURIComponent(
      version || (source === "modelscope" ? "master" : "main"),
    ) +
    "/";
  const [readme, setReadme] = useState(""),
    [error, setError] = useState(""),
    [retry, setRetry] = useState(0);


  useEffect(() => {
    const controller = new AbortController();
    const signal = catalogSignal(controller.signal, 5000);
    setReadme("");
    setError("");
    readHub(source, base + "README.md", signal)
      .then(async (r) => {
        if (!r.ok)
          throw new Error(
            r.status === 404 ? t("catalog.missingReadme") : t("catalog.readmeError"),
          );
        const md = (await r.text())
          .replace(/^---\r?\n[\s\S]*?\r?\n---(?:\r?\n|$)/, "")
          .trim();
        if (!md) throw new Error(t("catalog.missingReadme"));
        if (!controller.signal.aborted) setReadme(md);
      })
      .catch((e) => {
        if (!controller.signal.aborted) setError(problem(e));
      });
    return () => controller.abort();
  }, [base, retry, source]);
  const specs = templates.flatMap(template => template.specs.map(spec => ({template, spec})));
  const name = model.id.split("/").pop();
  const owner = model.id.includes("/") ? model.id.split("/")[0] : "";
  const metadata = [
    { icon: GitBranchIcon, label: t("catalog.version"), value: version || t("catalog.defaultVersion"), short: version ? version.slice(0, 12) : t("catalog.defaultVersion") },
    { icon: Calendar03Icon, label: t("catalog.updated"), value: model.updated?.slice(0, 10) },
    { icon: Download04Icon, label: t("catalog.downloads"), value: model.downloads?.toLocaleString(i18n.language), short: model.downloads != null ? fmt.format(model.downloads) : undefined },
    { icon: FavouriteIcon, label: t("catalog.like"), value: model.likes?.toLocaleString(i18n.language) },
    { icon: LicenseIcon, label: t("catalog.license"), value: model.license },
  ];
  const identity = (
    <span className="model-profile-identity">
      <CatalogAvatar modelID={model.id} />
      <span className="min-w-0">
        <a className="model-profile-name" href={url} target="_blank" rel="noreferrer">
          {name}<HugeiconsIcon icon={LinkSquare02Icon} size={14} aria-hidden />
        </a>
        <span className="model-profile-source">
          {owner && <span>{owner}</span>}<span>{providers[source]}</span>
        </span>
      </span>
    </span>
  );
  const body = (
      <section className="live-readme" aria-label={t("catalog.detail")}>
        <header className="model-profile">
          {!closeDrawer && <h3 className="m-0">{identity}</h3>}
          <ul className="live-meta" aria-label={t("catalog.metadata")}>
            {model.tasks.map(task => <li className="model-task" key={task}>{purposeLabel(task)}</li>)}
            {metadata.filter(item => item.value).map(({ icon, label, value, short }) => (
              <li key={label} title={label + " · " + value}>
                <HugeiconsIcon icon={icon} size={12} aria-hidden />
                <span className="sr-only">{label} </span>{short || value}
              </li>
            ))}
          </ul>
        </header>
        {retryMatches && (
          <aside aria-label={t("catalog.templates")}>
            <EmptyState compact description={<><strong>{t("catalog.templatesError")}</strong><span>{t("catalog.templatesHelp")}</span></>}
              action={<Button size="sm" variant="link" onClick={retryMatches}>{t("catalog.reload")}</Button>} />
          </aside>
        )}
        {!retryMatches && specs.length > 0 && (
          <aside className="live-matches" aria-label={t("catalog.templates")}>
            <h4>{t("catalog.templates")}</h4>
            {specs.map(({ template, spec }) => (
              <div
                className="live-spec"
                key={[template.id, spec.source, spec.name].join("/")}
              >
                <div>
                  <strong>{specDisplayName(spec, i18n.language)}</strong>
                  <small>{spec.engine}</small>
                </div>
                <Button
                  size="sm"
                  variant="link"
                  onClick={() => {
                    navigate(
                      "/deploy?" +
                        new URLSearchParams({
                          template: String(template.id),
                          spec_source: spec.source,
                          spec: spec.name,
                          provider: source,
                          repo: model.id,
                          revision: version,
                          subfolder: "",
                        }),
                    );
                  }}
                >
                  {t("catalog.deploy")}
                </Button>
              </div>
            ))}
          </aside>
        )}
        <section className="model-readme-body" aria-label="README">
          <h4>{t("catalog.readme")}</h4>
          {error ? (
            <EmptyState compact description={<><strong>{error}</strong><span>{t("catalog.readmeHelp")}</span></>}
              action={error !== t("catalog.missingReadme") && <Button size="sm" variant="link" onClick={() => setRetry((v) => v + 1)}>{t("catalog.retry")}</Button>} />
          ) : readme ? (
            <MarkdownContent content={readme} allowHtml assetBase={base} />
          ) : (
            <p role="status">{t("catalog.loadingReadme")}</p>
          )}
        </section>
      </section>
  );
  if (closeDrawer) return (
    <DetailDrawer open onClose={closeDrawer} title={identity} className="online-model-drawer">
      {body}
    </DetailDrawer>
  );
  return <div className="live-detail-container">{body}</div>;
}
