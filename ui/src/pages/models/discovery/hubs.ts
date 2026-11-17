import i18n from "../../../i18n";
import { api } from "../../../api";
import { usableBases } from "../../../modelmeta";
import type { OperationType } from "../../../inferenceApi";
export const providers = { modelscope: "ModelScope", huggingface: "Hugging Face" };
export type Provider = keyof typeof providers;
export const purposes = [
  ["", "models:catalog.allTasks"],
  ["text-generation", "models:catalog.text"],
  ["image-text-to-text", "models:catalog.multimodal"],
  ["text-to-image", "models:catalog.image"],
  ["sentence-similarity", "models:catalog.similarity"],
  ["automatic-speech-recognition", "models:catalog.asr"],
];
const modelScopeTasks: Record<string, string> = {
  "text-to-image": "text-to-image-synthesis",
  "automatic-speech-recognition": "auto-speech-recognition",
  "text-to-video": "text-to-video-synthesis",
};
export function sourceTask(source: Provider, task: string) {
  return source === "modelscope" ? modelScopeTasks[task] ?? task : task;
}
export function purposeLabel(task: string) {
  const key = purposes.find(([value]) => value === task || sourceTask("modelscope", value) === task)?.[1];
  return key ? i18n.t(key) : task;
}
export const capabilityTasks: Record<OperationType, string | null> = {
  "openai.chat": "conversational",
  "openai.completions": "text-generation",
  "openai.embeddings": "sentence-similarity",
  "openai.rerank": "text-ranking",
  "openai.audio.transcribe": "automatic-speech-recognition",
  "openai.audio.speech": "text-to-speech",
  "openai.image.generate": "text-to-image",
  "openai.image.edit": "image-to-image",
  "openai.video.generate": "text-to-video",
  "openai.realtime": null,
  "systemone.decide": null,
};
export const pageSize = 10;
export interface HubModel {
  id: string;
  revision: string;
  tasks: string[];
  likes?: number;
  downloads?: number;
  updated?: string;
  license?: string;
}
export function repoUrl(source: Provider, id: string) {
  const encoded = id.split("/").map(encodeURIComponent).join("/");
  return source === "modelscope"
    ? "https://modelscope.cn/models/" + encoded
    : "https://huggingface.co/" + encoded;
}
export function catalogUrl(
  source: Provider,
  query: string,
  capability: OperationType | "all",
  sort: string,
  page: number,
) {
  const ms = source === "modelscope";
  const url = new URL(
    ms
      ? "https://modelscope.cn/openapi/v1/models"
      : "https://huggingface.co/api/models",
  );
  if (query.trim()) url.searchParams.set("search", query.trim());
  if (capability !== "all") {
    const task = capabilityTasks[capability];
    if (!task) throw new Error(i18n.t("models:catalog.unsupported"));
    if (capability === "openai.chat") {
      url.searchParams.set(ms ? "filter.custom_tag" : "filter", ms ? "chat" : task);
    } else {
      url.searchParams.set(ms ? "filter.task" : "pipeline_tag", sourceTask(source, task));
    }
  }
  if (ms) {
    url.searchParams.set("page_number", String(page));
    url.searchParams.set("page_size", String(pageSize));
    if (sort !== "trending") url.searchParams.set("sort", sort);
  } else {
    url.searchParams.set("limit", String(pageSize));
    url.searchParams.set("full", "true");
    url.searchParams.set("sort", sort === "trending" ? "trendingScore" : sort);
  }
  return url.href;
}
export function problem(error: unknown) {
  if (error instanceof DOMException && error.name === "TimeoutError")
    return i18n.t("models:catalog.timeout");
  return error instanceof Error && !(error instanceof TypeError)
    ? error.message
    : i18n.t("models:catalog.unreachable");
}
export async function readPage(
  source: Provider,
  url: string,
  signal: AbortSignal,
) {
  const r = await readHub(source, url, signal);
  if (!r.ok)
    throw new Error(
      r.status === 429
        ? i18n.t("models:catalog.limited")
        : [401, 403].includes(r.status)
          ? i18n.t("models:catalog.denied")
          : i18n.t("models:catalog.catalogError"),
    );
  const data = await r.json();
  const ms = source === "modelscope";
  const list = ms ? data.data?.models : data;
  if (!Array.isArray(list)) throw new Error(i18n.t("models:catalog.badCatalog"));
  const items: HubModel[] = list
    .filter((m) => typeof m.id === "string")
    .map((m) => ({
      id: m.id,
      revision: m.sha || "",
      tasks: ms ? m.tasks || [] : m.pipeline_tag ? [m.pipeline_tag] : [],
      downloads: m.downloads,
      likes: m.likes,
      updated: m.last_modified || m.lastModified,
      license:
        m.license ||
        m.tags?.find((tag: string) => tag.startsWith("license:"))?.slice(8),
    }));
  const next = (r.headers.get("x-catalog-link") || r.headers.get("link"))?.match(/<([^>]+)>;\s*rel="next"/)?.[1];
  const cursor = next ? new URL(next, url).searchParams.get("cursor") : null;
  const nextUrl = new URL(url);
  if (cursor) nextUrl.searchParams.set("cursor", cursor);
  return {
    items,
    total: ms ? Number(data.data.total_count) : undefined,
    next: cursor ? nextUrl.href : undefined,
  };
}

// Chrome 121 supports timeout signals but not AbortSignal.any.
export function catalogSignal(parent: AbortSignal, milliseconds: number) {
  const controller = new AbortController();
  const timeout = AbortSignal.timeout(milliseconds);
  const cancel = () => controller.abort(parent.reason);
  if (parent.aborted) cancel();
  else parent.addEventListener("abort", cancel, {once:true, signal:controller.signal});
  timeout.addEventListener("abort", () => controller.abort(timeout.reason), {once:true, signal:controller.signal});
  return controller.signal;
}

export async function readHub(source: Provider, address: string, signal: AbortSignal) {
  const url = new URL(address);
  const params = new URLSearchParams(url.search);
  params.set("source", source);
  params.set("path", url.pathname);
  const relay = fetch("/api/v1/models/catalog?" + params, {signal});
  // A browser on a different network can read approved sites that the server
  // cannot. Start the second path only if the relay fails or is slow.
  let timer: number;
  const slow = new Promise<undefined>(resolve => { timer = window.setTimeout(resolve, 650); });
  const first = await Promise.race([relay.catch(() => undefined), slow]);
  clearTimeout(timer!);
  if (first?.ok) return first;
  signal.throwIfAborted();
  const browser = async () => {
    const {bases} = await api.metadataSources(source, signal);
    for (const base of usableBases(bases)) {
      try {
        const res = await fetch(base + url.pathname + url.search, {signal, credentials:"omit", referrerPolicy:"no-referrer"});
        if (res.ok) return res;
      } catch { signal.throwIfAborted(); }
    }
    throw new Error();
  };
  try {
    const direct = browser();
    const server = relay.then(res => { if (!res.ok) throw new Error(); return res; });
    return await Promise.race([direct.catch(() => server), server.catch(() => direct)]);
  } catch {
    signal.throwIfAborted();
    return relay;
  }
}
