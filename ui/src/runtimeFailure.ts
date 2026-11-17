import { productMessageText, type APIErrorTranslator, type ProductMessage } from "./apiError";

/**
 * runtimeFailureText renders an instance's runtime failure (the server's
 * proto.RuntimeFailure as a product message) in the reader's language: the
 * sentence for its code, the known failures the output showed, the engine's
 * root-cause line when the output had one, and the rank it happened on when
 * it ended a multi-node generation. t must resolve the common namespace and
 * the ai namespace's finding titles ("ai:finding.<CODE>.title").
 */
export function runtimeFailureText(t: APIErrorTranslator, failure: ProductMessage): string {
  const params = failure.params ?? {};
  let text = productMessageText(t, failure);
  const causes = (Array.isArray(params.causes) ? params.causes : []) as ProductMessage[];
  const titles = causes
    .filter((cause) => cause && typeof cause.code === "string")
    .map((cause) => t(`ai:finding.${cause.code}.title`, { ...cause.params, defaultValue: cause.message || cause.code }));
  if (titles.length > 0) text = t("runtimeFailure.causes", { text, causes: titles, defaultValue: `${text} (${titles.join(", ")})` });
  const evidence = Array.isArray(params.evidence) ? String(params.evidence[0] ?? "") : "";
  if (evidence) text = t("runtimeFailure.evidence", { text, evidence, defaultValue: `${text}: ${evidence}` });
  if (typeof params.rank === "number" && !failure.code.startsWith("UNIT_RANK_")) {
    text = t("runtimeFailure.rank", { rank: params.rank, node: params.node ?? "", text, defaultValue: text });
  }
  return text;
}

/** isProductMessage says a wire value (an event param) is a product message. */
export function isProductMessage(value: unknown): value is ProductMessage {
  return typeof value === "object" && value !== null && typeof (value as ProductMessage).code === "string" && typeof (value as ProductMessage).message === "string";
}
