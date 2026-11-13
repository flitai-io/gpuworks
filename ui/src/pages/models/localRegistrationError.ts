import type { APIErrorTranslator } from "../../apiError";
import english from "../../i18n/locales/en/models.json";

// This endpoint returns plain text. Translate complete known messages only;
// keep paths and unknown diagnostics verbatim instead of inferring a cause.
export function localRegistrationErrorText(t: APIErrorTranslator, error: unknown): string {
  const raw = error instanceof Error ? error.message : String(error ?? "");
  const source = english.register.directoryError;
  const entries = Object.entries(source);
  const exact = (text: string): string => {
    const entry = entries.find(([, message]) => message === text);
    return entry ? t(`register.directoryError.${entry[0]}`, { defaultValue: text }) : text;
  };
  const wrapper = `${source.validate}: `;
  const prefix = raw.startsWith(wrapper) ? `${exact(source.validate)}: ` : "";
  const body = prefix ? raw.slice(wrapper.length) : raw;
  return prefix + body.split("\n").map((line) => {
    if (line.startsWith("- ")) {
      const detail = line.slice(2);
      const missing = `${source.missingFile}: `;
      if (detail.startsWith(missing)) return `- ${exact(source.missingFile)}: ${detail.slice(missing.length)}`;
      const entry = entries.find(([, message]) => detail.endsWith(`: ${message}`));
      return entry ? `- ${detail.slice(0, -entry[1].length)}${exact(entry[1])}` : `- ${exact(detail)}`;
    }
    const unexplained = `${source.noEntry}: ${source.noReason}`;
    if (line === unexplained) return `${exact(source.noEntry)}: ${exact(source.noReason)}`;
    return line.endsWith(":") ? `${exact(line.slice(0, -1))}:` : exact(line);
  }).join("\n");
}
