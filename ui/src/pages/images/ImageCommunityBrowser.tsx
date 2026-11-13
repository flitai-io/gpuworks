import { Alert, Button, Card, Empty, Input, Space, Spin, Table, Tag, Tooltip, Typography } from "antd";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type CommunityRepo, type CommunityRepoBrowse, type CommunityTag } from "../../api";
import { outboundPolicyApi } from "../../outboundPolicyApi";
import CommunityBrowseApproval from "./CommunityBrowseApproval";
import { formatTime } from "../../i18n/datetime";
import { searchCommunityTags } from "./tagFilter";
import { OutboundHostAccess, outboundHostName, useOutboundHostAccess } from "../../components/OutboundHostAccess";

const { Text } = Typography;

/** What choosing an upstream tag fills into the image form. */
export type CommunityPick = {
  imageRef: string;
  engine: string;
  framework: string;
  engineVersion: string;
  frameworkVersion: string;
  arch: string;
  rollingTag: boolean;
};

type Props = {
  onPick: (pick: CommunityPick) => void;
};

type Browse =
  | { state: "idle" }
  | { state: "loading"; repo: string }
  | { state: "error"; repo: string; message: string }
  | { state: "done"; repo: string; result: CommunityRepoBrowse };

// Match the community API's default tag-list route, not a node's image pull:
// unqualified Docker references are read from hub.docker.com.
function repositoryAPIAddress(repo: string): string {
  const value = repo.trim();
  if (!value || value.includes("://") || /[\s?#@\\]/.test(value)) return "";
  const head = value.split("/", 1)[0];
  const explicit = value.includes("/") && (head.includes(".") || head.includes(":") || head === "localhost");
  const host = !explicit || head === "index.docker.io" || head === "registry-1.docker.io" ? "docker.io" : head.toLowerCase();
  return `https://${host === "docker.io" ? "hub.docker.com" : host}`;
}

/**
 * ImageCommunityBrowser lists the built-in community repositories as cards
 * and every tag upstream holds for the one chosen — release, nightly and
 * model-specific tags alike — with a name search instead of a hidden filter.
 * Any other repository can be typed in and browsed the same way.
 */
export default function ImageCommunityBrowser({ onPick }: Props) {
  const { t } = useTranslation("images");
  const { t: tCommon } = useTranslation("common");
  const [repos, setRepos] = useState<CommunityRepo[] | null>(null);
  const [reposError, setReposError] = useState("");
  const [manualRepo, setManualRepo] = useState("");
  const [browse, setBrowse] = useState<Browse>({ state: "idle" });
  const [repoBrowses, setRepoBrowses] = useState<Record<string, Browse>>({});
  const [query, setQuery] = useState("");
  const [picked, setPicked] = useState("");
  const [approvalRepo, setApprovalRepo] = useState("");
  const requestVersion = useRef(0);
  const configured = repos?.find((repo) => repo.repo.trim() === manualRepo.trim());
  const reposReady = repos !== null && !reposError;
  const accessUnknown = Boolean(manualRepo.trim()) && (!reposReady || Boolean(configured && !configured.access_host));
  const accessAddress = reposReady ? (configured ? configured.access_host || "" : repositoryAPIAddress(manualRepo)) : "";
  const access = useOutboundHostAccess([accessAddress], !accessUnknown);

  useEffect(() => {
    let alive = true;
    api
      .communityRepos()
      .then(async (rows) => {
        if (!alive) return;
        setRepos(rows.items);
        // Mixed repositories have no single default. Their badges come from
        // actual tags, with at most two background metadata requests at once.
        const pending = rows.items.filter((card) => !card.engine || !card.framework);
        let next = 0;
        const load = async () => {
          while (alive && next < pending.length) {
            const card = pending[next++];
            setRepoBrowses((current) => ({ ...current, [card.repo]: { state: "loading", repo: card.repo } }));
            try {
              const result = await api.browseCommunity(card.repo);
              if (alive) setRepoBrowses((current) => ({ ...current, [card.repo]: { state: "done", repo: card.repo, result } }));
            } catch (error) {
              if (alive) setRepoBrowses((current) => ({ ...current, [card.repo]: {
                state: "error", repo: card.repo, message: error instanceof Error ? error.message : String(error),
              } }));
            }
          }
        };
        await Promise.all([load(), load()]);
      })
      .catch((e) => {
        if (!alive) return;
        setRepos([]);
        setReposError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      alive = false;
    };
  }, []);

  const open = async (repo: string, card?: CommunityRepo, authorize = false) => {
    const target = repo.trim();
    if (!target) return false;
    const version = ++requestVersion.current;
    setBrowse({ state: "loading", repo: target });
    setQuery("");
    try {
      if (authorize) await access.authorize();
      const result = await api.browseCommunity(target);
      if (version !== requestVersion.current) return false;
      setRepoBrowses((current) => ({ ...current, [target]: { state: "done", repo: target, result } }));
      // A sibling path lists its own tags but belongs to its card, so the
      // card's engine and framework stand in when the answer leaves them out.
      setBrowse({
        state: "done",
        repo: target,
        result: {
          ...result,
          engine: result.engine || card?.engine || "",
          framework: result.framework || card?.framework || "",
        },
      });
      return true;
    } catch (e) {
      if (version === requestVersion.current) setBrowse({ state: "error", repo: target, message: e instanceof Error ? e.message : String(e) });
      return false;
    }
  };

  const addressFor = (repo: string) => {
    const exact = repos?.find((row) => row.repo.trim() === repo.trim());
    return exact ? exact.access_host || "" : repositoryAPIAddress(repo);
  };
  const cardFor = (repo: string) => repos?.find((row) => row.repo === repo)
    ?? repos?.find((row) => row.siblings?.includes(repo));
  const openCard = async (repo: string) => {
    const version = ++requestVersion.current;
    setApprovalRepo("");
    try {
      const policy = await outboundPolicyApi.get();
      if (version !== requestVersion.current) return;
      if (policy.restrict_private_hosts) {
        const address = addressFor(repo);
        if (!address) { setApprovalRepo(repo); return; }
        const host = outboundHostName(address);
        const checks = await outboundPolicyApi.checkHosts([host]);
        if (version !== requestVersion.current) return;
        if (checks.find((item) => item.host === host)?.status !== "allowed") { setApprovalRepo(repo); return; }
      }
    } catch {
      // Listing tags remains available to members who cannot read admin policy.
      // No permission is written when the optional approval preflight fails.
    }
    if (version === requestVersion.current) await open(repo, cardFor(repo));
  };

  const visible = useMemo(
    () => (browse.state === "done" ? searchCommunityTags(browse.result.tags ?? [], query) : []),
    [browse, query],
  );

  const pick = (result: CommunityRepoBrowse, tag: CommunityTag) => {
    const imageRef = `${result.repo || (browse.state === "done" ? browse.repo : "")}:${tag.tag}`;
    setPicked(imageRef);
    onPick({
      imageRef,
      engine: tag.engine || result.engine,
      framework: tag.framework || result.framework,
      engineVersion: tag.engine_version || "",
      frameworkVersion: tag.framework_version || "",
      arch: tag.arch || "",
      rollingTag: Boolean(tag.rolling_tag),
    });
  };

  const activeRepo = browse.state === "idle" ? "" : browse.repo;

  return (
    <Card size="small" title={t("import.community.title")}>
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        {repos === null ? (
          <Space>
            <Spin size="small" />
            <Text type="secondary">{t("import.community.reposLoading")}</Text>
          </Space>
        ) : reposError ? (
          <Alert type="error" showIcon message={t("import.community.reposFailed")} description={reposError} />
        ) : (
          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(220px, 1fr))", gap: 8 }}>
            {repos.map((card) => {
              const details = repoBrowses[card.repo];
              const tags = details?.state === "done" ? (details.result.tags ?? []).filter((tag) => !tag.upstream_gone) : [];
              const engines = [...new Set([card.engine, ...tags.map((tag) => tag.engine)].filter((value): value is string => Boolean(value)))];
              const frameworks = [...new Set([card.framework, ...tags.map((tag) => tag.framework)].filter((value): value is string => Boolean(value)))];
              return (
                <Card key={card.repo} size="small" styles={{ body: { padding: 8 } }}>
                  <Space direction="vertical" size={2} style={{ width: "100%" }}>
                    {[card.repo, ...(card.siblings ?? [])].map((path) => (
                      <Button
                        key={path}
                        type={activeRepo === path ? "primary" : "link"}
                        size="small"
                        style={{ padding: activeRepo === path ? undefined : 0, maxWidth: "100%", whiteSpace: "normal", textAlign: "left", height: "auto" }}
                        onClick={() => void openCard(path)}
                      >
                        {path}
                      </Button>
                    ))}
                    <Space size={[4, 4]} wrap>
                      {engines.map((engine) => <Tag key={engine} color="blue">{engine}</Tag>)}
                      {frameworks.map((framework) => <Tag key={framework}>{framework}</Tag>)}
                      {details?.state === "loading" && <Spin size="small" />}
                    </Space>
                    {details?.state === "error" && (
                      <Text type="secondary" title={details.message}>{t("import.community.tagsFailed", { repo: card.repo })}</Text>
                    )}
                  </Space>
                </Card>
              );
            })}
          </div>
        )}
        <Space.Compact style={{ width: "100%" }}>
          <Input
            id="community-repository"
            name="community_repository"
            aria-label={t("import.community.otherRepo")}
            placeholder={t("import.community.otherRepoPlaceholder")}
            value={manualRepo}
            onChange={(event) => setManualRepo(event.target.value)}
            onPressEnter={() => void open(manualRepo, undefined, true)}
          />
          <Button onClick={() => void open(manualRepo, undefined, true)} disabled={!manualRepo.trim()}>
            {t("import.community.browse")}
          </Button>
        </Space.Compact>
        {accessUnknown && <p className="text-sm text-fg-muted">
          {t("import.community.accessUnknown")}{" "}
          <a className="underline" href="/settings#outbound-policy">{tCommon("outboundAccess.settings")}</a>
        </p>}
        <OutboundHostAccess access={access} />
        {browse.state === "loading" && (
          <Space>
            <Spin size="small" />
            <Text type="secondary">{t("import.community.tagsLoading", { repo: browse.repo })}</Text>
          </Space>
        )}
        {browse.state === "error" && (
          <Alert
            type="error"
            showIcon
            message={t("import.community.tagsFailed", { repo: browse.repo })}
            description={browse.message}
          />
        )}
        {browse.state === "done" && (
          <Space direction="vertical" size={8} style={{ width: "100%" }}>
            <Text strong>{t("import.community.tagCount", { repo: browse.repo, total: browse.result.tags?.length ?? 0 })}</Text>
            <Input
              allowClear
              aria-label={t("import.community.search")}
              placeholder={t("import.community.searchPlaceholder")}
              value={query}
              onChange={(event) => setQuery(event.target.value)}
            />
            <Table
              rowKey="tag"
              size="small"
              tableLayout="fixed"
              scroll={{ x: 560 }}
              dataSource={visible}
              pagination={{ pageSize: 10, hideOnSinglePage: true, showSizeChanger: false }}
              locale={{
                emptyText: (
                  <Empty
                    image={Empty.PRESENTED_IMAGE_SIMPLE}
                    description={
                      (browse.result.tags?.length ?? 0) === 0
                        ? t("import.community.noTags")
                        : t("import.community.noMatch")
                    }
                  />
                ),
              }}
              columns={[
                {
                  title: t("import.community.column.tag"),
                  key: "tag",
                  width: 240,
                  render: (_: unknown, tag: CommunityTag) => (
                    <Tooltip title={tag.tag} trigger={["hover", "focus"]}>
                      <Button
                        type={picked === `${browse.result.repo || browse.repo}:${tag.tag}` ? "primary" : "link"}
                        size="small"
                        style={{ paddingInline: 4, maxWidth: "100%" }}
                        onClick={() => pick(browse.result, tag)}
                      >
                        <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{tag.tag}</span>
                      </Button>
                    </Tooltip>
                  ),
                },
                {
                  title: t("import.community.column.labels"),
                  key: "labels",
                  width: 152,
                  render: (_: unknown, tag: CommunityTag) => (
                    <>
                      {tag.rolling_tag && <Tag color="gold">{t("import.rolling")}</Tag>}
                      {tag.upstream_gone && <Tag color="red">{t("tag.upstreamGone")}</Tag>}
                      {tag.engine_version && <Text type="secondary">{tag.engine_version}</Text>}
                    </>
                  ),
                },
                {
                  title: t("import.community.column.lastUpdated"),
                  key: "last_updated",
                  width: 168,
                  render: (_: unknown, tag: CommunityTag) => (
                    <Text type="secondary" style={{ whiteSpace: "nowrap" }}>{formatTime(tag.last_updated)}</Text>
                  ),
                },
              ]}
            />
          </Space>
        )}
      </Space>
      {approvalRepo && <CommunityBrowseApproval key={approvalRepo} repo={approvalRepo} address={addressFor(approvalRepo)}
        onBrowse={() => open(approvalRepo, cardFor(approvalRepo))} onClose={() => setApprovalRepo("")} />}
    </Card>
  );
}
