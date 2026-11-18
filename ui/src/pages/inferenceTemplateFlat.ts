import type { DetectedTemplateChip, GPUPredicate, InferenceSpec, InferenceTemplate } from "../api";
import type { OperationType } from "../inferenceApi";
import { operationTypesOf } from "../modelInference";

export function predicateLabel(spec: { gpus: GPUPredicate }) {
  const parts = [spec.gpus.vendor.toUpperCase()];
  if (spec.gpus.cc) parts.push(`CC ${spec.gpus.cc}`);
  if (spec.gpus.name) parts.push(spec.gpus.name);
  if (spec.gpus.arch_id) parts.push(`arch ${spec.gpus.arch_id}`);
  return parts.join(" · ");
}

/** The spec's human-written name in the page's language, falling back to the
 * other language and then to the spec id. The names are catalog data written
 * per locale; nothing here derives a name. */
export function specDisplayName(spec: Pick<InferenceSpec, "name" | "display_name_zh_cn" | "display_name_en">, language: string) {
  const zh = spec.display_name_zh_cn?.trim();
  const en = spec.display_name_en?.trim();
  return (language.toLowerCase().startsWith("zh") ? zh || en : en || zh) || spec.name;
}

export function templateModelOperations(model: InferenceTemplate): OperationType[] {
  return operationTypesOf(model.specs.flatMap((spec) => spec.api?.operations ?? []));
}

/** @deprecated kept for consumers of the pre-operation catalog helper. */

function compactParameterCount(value?: string) {
  if (!value) return "";
  const scale = value.match(/(?:^|\s)(\d+(?:\.\d+)?\s*[BMKT])(?=\s|$|[·(),])/i)?.[1];
  if (scale) return scale.replace(/\s+/g, "").toUpperCase();
  return value.length <= 24 ? value : "";
}

function compactArchitecture(value?: string) {
  if (!value) return "";
  if (/mixture.of.experts|\bmoe\b|routed experts?/i.test(value)) return "moe";
  if (value.length <= 24 && !value.includes(";")) return value;
  if (/conditionalgeneration|vision|multimodal/i.test(value)) return "multimodal";
  if (/causallm/i.test(value)) return "causal LM";
  return "";
}

export function compactModelMetadata(model: InferenceTemplate) {
  return [compactParameterCount(model.parameter_count), compactArchitecture(model.architecture), model.context_length ? `${model.context_length.toLocaleString()} ctx` : ""].filter(Boolean).join(" · ");
}

export function chipKey(chip: DetectedTemplateChip): string {
  const vendor = chip.vendor.trim().toLowerCase();
  const name = chip.name.trim();
  const model = vendor === "amd" ? name.replace(/^AMD\s+/i, "") : name;
  return [vendor, model, chip.cc || "", chip.arch_id?.trim().toLowerCase() || ""].join("\u0000");
}

export function chipDisplayLabel(chip: Pick<DetectedTemplateChip, "vendor" | "name" | "cc" | "arch_id">): string {
  const rawName = chip.name.trim();
  const shortName = rawName
    .replace(/^NVIDIA\s+/i, "")
    .replace(/^(?:GeForce|Quadro|Tesla)\s+/i, "")
    .replace(/^Ascend\s+/i, "")
    .replace(/^Hygon\s+/i, "")
    .replace(/^Apple\s+/i, "")
    .replace(/^vGPU[-_ ]*/i, "vGPU ")
    .replace(/\s+/g, " ")
    .trim();
  const fallbackName = shortName || chip.vendor.trim().toUpperCase() || "GPU";
  const hasConcreteModel = !/^(?:v?gpu(?:\s*[-_]?[0-9]+\s*(?:g|gb|b)?)?|graphics device|3d controller|unknown|generic)$/i.test(fallbackName);
  const label = hasConcreteModel || !chip.cc ? fallbackName : `${fallbackName} · CC ${chip.cc}`;
  return chip.arch_id?.trim() ? `${label} · ${chip.arch_id.trim()}` : label;
}

export interface EditError { message: string; line?: number }

// Server YAML errors carry a line number and Go decoding noise; keep the
// reason and the line, drop the internals.
export function editErrorOf(raw: string): EditError {
  const line = /line (\d+)/i.exec(raw)?.[1];
  const cleaned = raw
    .replace(/^parse inference catalog YAML:\s*/i, "")
    .replace(/^yaml:\s*/i, "")
    .replace(/^unmarshal errors:\s*/i, "")
    .replace(/\s*in type [A-Za-z0-9_.]+\s*$/i, "")
    .trim();
  return { message: cleaned || raw, line: line ? Number(line) : undefined };
}

// The edited document's target identity: the first top-level model entry.
// Saving retargets the import onto this id, so a change from the loaded
// model_id is what the overwrite confirmation keys on. The server writes the
// models list indented, a hand-written document often does not, so the entry
// is found either right under the "models:" key at any indentation or as an
// unindented list item; whichever comes first in the text wins.
const yamlModelIDValue = String.raw`model_id:\s*(?:"([^"]+)"|'([^']+)'|(\S+))`;
const yamlModelIDPatterns = [
  new RegExp(String.raw`^models:[ \t]*\r?\n(?:[ \t]*(?:#.*)?\r?\n)*[ \t]*-[ \t]+${yamlModelIDValue}`, "m"),
  new RegExp(String.raw`^-[ \t]+${yamlModelIDValue}`, "m"),
];
export function yamlModelID(source: string): string | undefined {
  const matches = yamlModelIDPatterns.map((pattern) => pattern.exec(source)).filter((match): match is RegExpExecArray => match !== null);
  matches.sort((left, right) => left.index - right.index);
  return matches[0]?.slice(1).find(Boolean);
}
