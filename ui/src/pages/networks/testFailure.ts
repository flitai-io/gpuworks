// A test failure in the viewer's language.
type T = (key: string, options?: Record<string, unknown>) => string;

/**
 * The text of a failure code under keyPrefix, with the node it is about and
 * the tool's own detail. A code the client does not know shows the server's
 * text.
 */
export function failureText(t: T, keyPrefix: string, known: Set<string>, f: { code?: string; side?: string; error?: string }, node?: string) {
  if (!f.code || !known.has(f.code)) return f.error ?? "";
  const detail = f.error?.trim() ?? "";
  const message = t(`${keyPrefix}.${f.code}`, { node: node ?? t(`test.side.${f.side ?? "node"}`), detail });
  return detail ? message : message.replace(/[:：]\s*$/, "");
}
