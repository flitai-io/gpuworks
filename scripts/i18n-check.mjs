import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import { missingProtectedTerms } from "./i18n-terms.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const glossaryPath = path.join(root, "ui/i18n/glossary.json");
const contextPath = path.join(root, "CONTEXT.md");
const localesRoot = path.join(root, "ui/src/i18n/locales");
const errors = [];

function readJSON(file) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch (error) {
    errors.push(`${path.relative(root, file)}: invalid JSON: ${error.message}`);
    return null;
  }
}

function flatten(value, prefix = "", output = new Map()) {
  for (const [key, child] of Object.entries(value ?? {})) {
    const full = prefix ? `${prefix}.${key}` : key;
    if (typeof child === "string") output.set(full, child);
    else if (child && typeof child === "object" && !Array.isArray(child)) flatten(child, full, output);
    else errors.push(`locale key ${full} must contain a string or nested object`);
  }
  return output;
}

function placeholders(value) {
  return [...value.matchAll(/{{\s*([^},\s]+)[^}]*}}/g)].map((match) => match[1]).sort();
}

function filesUnder(directory, suffix) {
  const output = [];
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const file = path.join(directory, entry.name);
    if (entry.isDirectory()) output.push(...filesUnder(file, suffix));
    else if (entry.isFile() && entry.name.endsWith(suffix)) output.push(file);
  }
  return output;
}

const glossary = readJSON(glossaryPath);
const terms = new Map();
for (const term of glossary?.terms ?? []) {
  if (!term.term_en || !term.translations?.zh) {
    errors.push("every glossary term needs term_en and translations.zh");
    continue;
  }
  if (terms.has(term.term_en)) errors.push(`duplicate glossary term: ${term.term_en}`);
  terms.set(term.term_en, term);
}

const context = fs.readFileSync(contextPath, "utf8");
const contextTerms = [...context.matchAll(/^\*\*(.+?) — (.+?)\*\*:/gm)];
const boldTerms = [...context.matchAll(/^\*\*(.+?)\*\*:/gm)];
if (contextTerms.length !== boldTerms.length) {
  errors.push(`CONTEXT.md has ${boldTerms.length - contextTerms.length} term(s) without a canonical English name`);
}
for (const [, zh, en] of contextTerms) {
  const term = terms.get(en);
  if (!term) errors.push(`glossary is missing CONTEXT.md term: ${zh} — ${en}`);
  else if (term.translations.zh !== zh) errors.push(`${en}: glossary zh is ${JSON.stringify(term.translations.zh)}, want ${JSON.stringify(zh)}`);
}

const languages = fs.readdirSync(localesRoot).filter((entry) => fs.statSync(path.join(localesRoot, entry)).isDirectory()).sort();
if (!languages.includes("en")) errors.push("English source locale is missing");
const namespaces = fs.readdirSync(path.join(localesRoot, "en")).filter((name) => name.endsWith(".json")).sort();
const localeValues = new Map();
const localeMaps = new Map();
const localePairs = [];
for (const language of languages) {
  const dir = path.join(localesRoot, language);
  const actualNamespaces = fs.readdirSync(dir).filter((name) => name.endsWith(".json")).sort();
  for (const namespace of new Set([...namespaces, ...actualNamespaces])) {
    const sourceFile = path.join(localesRoot, "en", namespace);
    const targetFile = path.join(dir, namespace);
    if (!fs.existsSync(sourceFile)) {
      errors.push(`${language}/${namespace}: orphan namespace not present in en`);
      continue;
    }
    if (!fs.existsSync(targetFile)) {
      errors.push(`${language}/${namespace}: missing namespace`);
      continue;
    }
    const source = flatten(readJSON(sourceFile));
    const target = flatten(readJSON(targetFile));
    localeMaps.set(`${language}/${namespace}`, target);
    localeValues.set(language, [...(localeValues.get(language) ?? []), ...target.values()]);
    for (const [key, sourceValue] of source) {
      if (!target.has(key)) errors.push(`${language}/${namespace}:${key}: missing key`);
      else {
        const targetValue = target.get(key);
        localePairs.push({ language, source: sourceValue, target: targetValue, location: `${language}/${namespace}:${key}` });
        if (placeholders(sourceValue).join("\0") !== placeholders(targetValue).join("\0")) {
          errors.push(`${language}/${namespace}:${key}: placeholders differ from en`);
        }
      }
    }
    for (const key of target.keys()) {
      if (!source.has(key)) errors.push(`${language}/${namespace}:${key}: orphan key`);
    }
    if (source.size === target.size && [...source.keys()].every((key) => target.has(key)) &&
        [...source.keys()].some((key, index) => [...target.keys()][index] !== key)) {
      errors.push(`${language}/${namespace}: key order differs from en`);
    }
  }
}

// Server-owned dictionaries use the same English-source contract as UI
// locales. Validate them in the secret-free CI check so a Go source change
// cannot silently ship a missing translation or backfill display text into
// semantic dictionary definitions.
const serverDictionaries = [
  {
    name: "deploy parameters",
    directory: path.join(root, "internal/server/deploy/locales"),
    sourceFiles: [path.join(root, "internal/server/deploy/param_specs.go")],
  },
  {
    name: "playground parameters",
    directory: path.join(root, "internal/server/api/locales/playground"),
    sourceFiles: [
      path.join(root, "internal/server/api/playground.go"),
      path.join(root, "internal/server/api/playground_dictionary.go"),
      path.join(root, "internal/server/api/playground_thinking.go"),
    ],
  },
  {
    name: "benchmark metadata",
    directory: path.join(root, "pkg/bench/locales"),
    sourceFiles: [
      path.join(root, "pkg/bench/meta.go"),
      path.join(root, "pkg/bench/meta_locales.go"),
    ],
  },
  {
    name: "provider metadata",
    directory: path.join(root, "internal/server/provider/locales"),
    sourceFiles: [
      path.join(root, "internal/server/provider/provider.go"),
      path.join(root, "internal/server/provider/dictionary.go"),
      path.join(root, "internal/server/provider/ssh_connections.go"),
    ],
  },
];
for (const dictionary of serverDictionaries) {
  const sourceFile = path.join(dictionary.directory, "en.json");
  const source = flatten(readJSON(sourceFile));
  for (const language of languages) {
    const targetFile = path.join(dictionary.directory, `${language}.json`);
    if (!fs.existsSync(targetFile)) {
      errors.push(`${dictionary.name}: missing ${language}.json`);
      continue;
    }
    const target = flatten(readJSON(targetFile));
    for (const [key, sourceValue] of source) {
      if (!target.has(key)) errors.push(`${dictionary.name}/${language}.json:${key}: missing key`);
      else {
        const targetValue = target.get(key);
        localePairs.push({
          language,
          source: sourceValue,
          target: targetValue,
          location: `${dictionary.name}/${language}.json:${key}`,
        });
        if (placeholders(sourceValue).join("\0") !== placeholders(targetValue).join("\0")) {
          errors.push(`${dictionary.name}/${language}.json:${key}: placeholders differ from en`);
        }
      }
    }
    for (const key of target.keys()) {
      if (!source.has(key)) errors.push(`${dictionary.name}/${language}.json:${key}: orphan key`);
    }
    if (source.size === target.size && [...source.keys()].every((key) => target.has(key)) &&
        [...source.keys()].some((key, index) => [...target.keys()][index] !== key)) {
      errors.push(`${dictionary.name}/${language}.json: key order differs from en`);
    }
  }
  for (const sourcePath of dictionary.sourceFiles) {
    const sourceText = fs.readFileSync(sourcePath, "utf8");
    const match = sourceText.match(/[\u3400-\u9fff]/u);
    if (match) errors.push(`${path.relative(root, sourcePath)} contains product CJK text outside locale JSON`);
  }
}

// Deployment warnings are a code-only wire contract. Keep their locale
// resources in lockstep with both named backend constants and W1-W10 emitted
// directly by the performance validator.
const warningSource = ["internal/server/messages/warning.go", "internal/server/messages/pd.go"]
  .map((file) => fs.readFileSync(path.join(root, file), "utf8"))
  .join("\n");
const performanceSource = fs.readFileSync(path.join(root, "internal/server/deploy/params.go"), "utf8");
const backendWarningCodes = new Set([
  ...[...warningSource.matchAll(/Warning\w+\s*=\s*"([A-Z0-9_]+)"/g)].map((match) => match[1]),
  ...[...performanceSource.matchAll(/NewWarning\("(W\d+)"/g)].map((match) => match[1]),
]);
for (const language of languages) {
  const deployLocale = localeMaps.get(`${language}/deploy.json`) ?? new Map();
  for (const code of backendWarningCodes) {
    if (!deployLocale.has(`warning.${code}`)) errors.push(`${language}/deploy.json:warning.${code}: missing backend warning translation`);
  }
}
const translatedWarningCodes = new Set(
  [...(localeMaps.get("en/deploy.json") ?? new Map()).keys()]
    .filter((key) => key.startsWith("warning."))
    .map((key) => key.slice("warning.".length)),
);
for (const code of translatedWarningCodes) {
  if (!backendWarningCodes.has(code)) errors.push(`en/deploy.json:warning.${code}: no matching backend warning code`);
}

// Scheduler explanations are persisted and sent over the API as stable codes
// plus interpolation params. Every declared backend code must have a matching
// UI translation, and locale resources must not retain orphan scheduler codes.
const schedulerSourceFiles = [
  path.join(root, "internal/server/scheduler/scheduler.go"),
  path.join(root, "internal/server/scheduler/filter.go"),
  path.join(root, "internal/server/scheduler/gpuplan.go"),
  path.join(root, "internal/server/scheduler/scorer.go"),
];
const schedulerSources = schedulerSourceFiles.map((file) => fs.readFileSync(file, "utf8"));
const schedulerCodes = new Set([
  ...schedulerSources.flatMap((source) => [...source.matchAll(/\bReason\w+\s*=\s*"([a-z0-9_]+)"/g)].map((match) => match[1])),
  ...schedulerSources.flatMap((source) => [...source.matchAll(/\bCode:\s*"([a-z0-9_]+)"/g)].map((match) => match[1])),
]);
if (schedulerCodes.size === 0) errors.push("no scheduler reason codes found");
for (const language of languages) {
  const deployLocale = localeMaps.get(`${language}/deploy.json`) ?? new Map();
  for (const code of schedulerCodes) {
    if (!deployLocale.has(`schedule.${code}`)) errors.push(`${language}/deploy.json:schedule.${code}: missing scheduler reason translation`);
  }
}
const translatedSchedulerCodes = new Set(
  [...(localeMaps.get("en/deploy.json") ?? new Map()).keys()]
    .filter((key) => key.startsWith("schedule."))
    .map((key) => key.slice("schedule.".length)),
);
for (const code of translatedSchedulerCodes) {
  if (!schedulerCodes.has(code)) errors.push(`en/deploy.json:schedule.${code}: no matching backend scheduler reason code`);
}
for (const [index, source] of schedulerSources.entries()) {
  if (/[㐀-鿿]/u.test(source)) {
    errors.push(`${path.relative(root, schedulerSourceFiles[index])} contains product CJK text outside locale JSON`);
  }
}

// Structured API product errors keep an English fallback on the wire, while
// the UI resolves stable codes from the common namespace. Package messages
// declares them, one file per area; api/errors.go adds its own and aliases
// the rest. Agent runtime failures travel through the same ProductMessage API,
// with their wire codes owned by proto.
const messagesDir = path.join(root, "internal/server/messages");
const productErrorSourceFiles = [
  ...fs.readdirSync(messagesDir).filter((name) => name.endsWith(".go") && !name.endsWith("_test.go")).sort().map((name) => path.join(messagesDir, name)),
  path.join(root, "internal/server/api/errors.go"),
  path.join(root, "internal/proto/runtime_failure.go"),
];
const productErrorSources = productErrorSourceFiles.map((file) => fs.readFileSync(file, "utf8"));
const productErrorCodes = new Set(
  productErrorSources.flatMap((source) => [...source.matchAll(/\bError\w+\s*=\s*"([A-Z0-9_]+)"/g)].map((match) => match[1])),
);
if (productErrorCodes.size === 0) errors.push("no product API error codes found");
for (const language of languages) {
  const commonLocale = localeMaps.get(`${language}/common.json`) ?? new Map();
  for (const code of productErrorCodes) {
    if (!commonLocale.has(`apiError.${code}`)) errors.push(`${language}/common.json:apiError.${code}: missing product API error translation`);
  }
}
const translatedProductErrorCodes = new Set(
  [...(localeMaps.get("en/common.json") ?? new Map()).keys()]
    .filter((key) => key.startsWith("apiError."))
    .map((key) => key.slice("apiError.".length)),
);
for (const code of translatedProductErrorCodes) {
  if (!productErrorCodes.has(code)) errors.push(`en/common.json:apiError.${code}: no matching product API error code`);
}
for (const [index, source] of productErrorSources.entries()) {
  if (/[㐀-鿿]/u.test(source)) {
    errors.push(`${path.relative(root, productErrorSourceFiles[index])} contains product CJK text outside locale JSON`);
  }
}

// Product-facing API errors must enter through the stable-code helpers. Keep
// raw lower-level errors on writeErr, but reject new hand-authored product
// messages, CJK fallbacks, and unregistered raw string codes.
const productionAPIFiles = filesUnder(path.join(root, "internal/server/api"), ".go")
  .filter((file) => !file.endsWith("_test.go"));
// The deployment use cases build the same product errors without HTTP; the
// CJK and raw-code rules hold for them too (writeErr does not occur there).
const productionUseCaseFiles = filesUnder(path.join(root, "internal/server/deployment"), ".go")
  .filter((file) => !file.endsWith("_test.go"));
for (const file of [...productionAPIFiles, ...productionUseCaseFiles]) {
  const source = fs.readFileSync(file, "utf8");
  const relative = path.relative(root, file);
  for (const [lineIndex, line] of source.split("\n").entries()) {
    if (/writeErr\([^\n]*(?:errors\.New|staticErr|fmt\.Errorf)\(/.test(line)) {
      errors.push(`${relative}:${lineIndex + 1}: hand-authored product error must use a stable error code`);
    }
    if (/(?:writeProductError|productError|wrapProductError)\(/.test(line) && /[㐀-鿿]/u.test(line)) {
      errors.push(`${relative}:${lineIndex + 1}: structured product error fallback contains CJK text`);
    }
    if (/writeProductError\([^,]+,[^,]+,\s*"[A-Z][A-Z0-9_]+"/.test(line) || /(?:productError|wrapProductError)\(\s*"[A-Z][A-Z0-9_]+"/.test(line)) {
      errors.push(`${relative}:${lineIndex + 1}: structured product error uses a raw string code instead of an Error constant`);
    }
  }
}

// Instance timeline rows use a code column plus JSON params for new writes.
// Historical Message/Data rows remain readable but production code must not
// append new free-text events.
const instanceEventBackendPath = path.join(root, "internal/server/store/deploy.go");
const instanceEventFrontendPath = path.join(root, "ui/src/pages/instanceEvents.ts");
const instanceEventBackendSource = fs.readFileSync(instanceEventBackendPath, "utf8");
const instanceEventFrontendSource = fs.readFileSync(instanceEventFrontendPath, "utf8");
const instanceEventCodes = new Set(
  [...instanceEventBackendSource.matchAll(/\binstanceEvent\w+\s*=\s*"([A-Z0-9_]+)"/g)].map((match) => match[1]),
);
const frontendInstanceEventCodes = new Set(
  [...instanceEventFrontendSource.matchAll(/^\s*"(INSTANCE_[A-Z0-9_]+)",?$/gm)].map((match) => match[1]),
);
if (instanceEventCodes.size === 0) errors.push("no instance event codes found");
for (const code of instanceEventCodes) {
  if (!frontendInstanceEventCodes.has(code)) errors.push(`instanceEvents.ts: missing backend instance event code ${code}`);
  for (const language of languages) {
    const locale = localeMaps.get(`${language}/deployments.json`) ?? new Map();
    if (!locale.has(`instance.event.${code}`)) errors.push(`${language}/deployments.json:instance.event.${code}: missing instance event translation`);
  }
}
for (const code of frontendInstanceEventCodes) {
  if (!instanceEventCodes.has(code)) errors.push(`instanceEvents.ts: instance event code ${code} has no backend declaration`);
}
const translatedInstanceEventCodes = new Set(
  [...(localeMaps.get("en/deployments.json") ?? new Map()).keys()]
    .filter((key) => key.startsWith("instance.event.INSTANCE_"))
    .map((key) => key.slice("instance.event.".length)),
);
for (const code of translatedInstanceEventCodes) {
  if (!instanceEventCodes.has(code)) errors.push(`en/deployments.json:instance.event.${code}: no matching backend instance event code`);
}
if (/\.AddInstanceEvent\(/.test(instanceEventBackendSource) || /[㐀-鿿]/u.test(instanceEventBackendSource)) {
  errors.push(`${path.relative(root, instanceEventBackendPath)} appends free-text instance events or contains product CJK text`);
}

const benchmarkBackendPath = path.join(root, "internal/server/store/benchmark.go");
const benchmarkFrontendPath = path.join(root, "ui/src/pages/evaluations/evaluationUtils.ts");
const benchmarkBackendSource = fs.readFileSync(benchmarkBackendPath, "utf8");
const benchmarkFrontendSource = fs.readFileSync(benchmarkFrontendPath, "utf8");
const benchmarkErrorCodes = new Set(
  [...benchmarkBackendSource.matchAll(/\bBenchmarkError\w+\s*=\s*"([A-Z0-9_]+)"/g)].map((match) => match[1]),
);
const frontendBenchmarkErrorCodes = new Set(
  [...benchmarkFrontendSource.matchAll(/"(BENCHMARK_[A-Z0-9_]+)"/g)].map((match) => match[1]),
);
for (const code of benchmarkErrorCodes) {
  if (!frontendBenchmarkErrorCodes.has(code)) errors.push(`evaluationUtils.ts: missing backend benchmark error code ${code}`);
  for (const language of languages) {
    const locale = localeMaps.get(`${language}/evaluations.json`) ?? new Map();
    if (!locale.has(`benchmarkError.${code}`)) errors.push(`${language}/evaluations.json:benchmarkError.${code}: missing benchmark error translation`);
  }
}
for (const code of frontendBenchmarkErrorCodes) {
  if (!benchmarkErrorCodes.has(code)) errors.push(`evaluationUtils.ts: benchmark error code ${code} has no backend declaration`);
}
const storageBackendPath = path.join(root, "internal/server/store/storage.go");
const storageBackendSource = fs.readFileSync(storageBackendPath, "utf8");
for (const [file, source] of [[storageBackendPath, storageBackendSource], [benchmarkBackendPath, benchmarkBackendSource]]) {
  if (/[㐀-鿿]/u.test(source)) errors.push(`${path.relative(root, file)} contains persisted product CJK text`);
}

// Every non-streaming REST fetch in the central API client must decode the
// structured error envelope. This catches custom upload/import paths that do
// not use getJSON/sendJSON.
const apiClientPath = path.join(root, "ui/src/api.ts");
const apiClientLines = fs.readFileSync(apiClientPath, "utf8").split("\n");
for (const [index, line] of apiClientLines.entries()) {
  if (!line.includes("if (!res.ok)")) continue;
  const nearby = apiClientLines.slice(index, index + 4).join("\n");
  if (!nearby.includes("localizedResponseError")) {
    errors.push(`ui/src/api.ts:${index + 1}: non-streaming REST error bypasses localizedResponseError`);
  }
}

for (const term of glossary?.terms ?? []) {
  for (const [language, forbidden] of Object.entries(term.forbidden ?? {})) {
    const canonicalWords = term.term_en.split(" / ").map((value) => value.toLocaleLowerCase("en"));
    for (const pair of localePairs.filter((value) => value.language === language)) {
      if (!canonicalWords.some((canonical) => pair.source.toLocaleLowerCase("en").includes(canonical))) continue;
      for (const phrase of forbidden) {
        if (phrase && pair.target.toLocaleLowerCase(language).includes(phrase.toLocaleLowerCase(language))) {
          errors.push(`${language} locale uses forbidden term ${JSON.stringify(phrase)} for ${term.term_en}`);
        }
      }
    }
  }
}

const protectedTerms = glossary?.do_not_translate;
if (!Array.isArray(protectedTerms)) {
  errors.push("glossary do_not_translate must be an array");
} else {
  const seenProtectedTerms = new Set();
  for (const term of protectedTerms) {
    if (typeof term !== "string" || !term) {
      errors.push("every do_not_translate entry must be a non-empty string");
      continue;
    }
    if (seenProtectedTerms.has(term)) errors.push(`duplicate do_not_translate term: ${term}`);
    seenProtectedTerms.add(term);
  }
  for (const pair of localePairs) {
    for (const term of missingProtectedTerms(pair.source, pair.target, [...seenProtectedTerms])) {
      errors.push(`${pair.location}: must preserve do_not_translate term ${JSON.stringify(term)}`);
    }
  }
}

if (errors.length) {
  console.error(`i18n check failed (${errors.length}):`);
  for (const error of errors) console.error(`- ${error}`);
  process.exit(1);
}
console.log(`i18n check passed: ${terms.size} terms, ${languages.length} languages, ${namespaces.length} namespaces, ${serverDictionaries.length} server dictionaries, ${schedulerCodes.size} scheduler reasons, ${productErrorCodes.size} product API errors, ${instanceEventCodes.size} instance event codes, ${benchmarkErrorCodes.size} benchmark error codes`);
