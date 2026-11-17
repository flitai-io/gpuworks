import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  modelOrganizationColor,
  modelOrganizationIcon,
  modelOrganizationIconOrganizations,
  modelOrganizationGroups,
  modelOrganizationKey,
  modelOrganizationName,
} from "../src/pages/inferenceTemplatePresentation.ts";

const messages: Record<string, string> = {
  "organization.other": "其他",
};
const t = ((key: string, options: Record<string, unknown> = {}) => Object.entries(options).reduce(
  (value, [name, replacement]) => value.replaceAll(`{{${name}}}`, String(replacement)), messages[key] || key,
)) as never;

test("model organizations use original model IDs rather than variant repositories", () => {
  assert.equal(modelOrganizationKey("baidu/ernie-4.5"), "baidu");
  assert.equal(modelOrganizationKey("qwen3-14b-q4_k_m.gguf"), "other");
  assert.equal(modelOrganizationName("baidu", t), "Baidu");
  assert.equal(modelOrganizationName("bytedance-seed", t), "ByteDance");
  assert.deepEqual(modelOrganizationGroups([
    { model_id: "bytedance-seed/seed-oss" },
    { model_id: "baidu/ernie-4.5" },
    { model_id: "baidu/ernie-4.5" },
    { model_id: "baidu/unlimited-ocr" },
  ], t), [
    { key: "baidu", label: "Baidu", modelCount: 2 },
    { key: "bytedance-seed", label: "ByteDance", modelCount: 1 },
  ]);
});

test("model organization icons use only reviewed local assets", () => {
  assert.equal(modelOrganizationIconOrganizations.length, 82);
  assert.equal(modelOrganizationIcon("lmsys/vicuna-7b-v1.5"), "/model-org-icons/lmsys.webp");
  assert.equal(modelOrganizationIcon("deepseek-ai/deepseek-r1"), "/model-org-icons/deepseek-ai.webp");
  assert.equal(modelOrganizationIcon("eco-tech/deepseek-v3.1-w8a8"), undefined);
  assert.equal(modelOrganizationIcon("zhipuai/glm-5"), undefined);
  assert.equal(modelOrganizationIcon("qwen3-14b-q4_k_m.gguf"), undefined);
});

test("every reviewed organization icon is a 96 by 96 WebP", async () => {
  for (const organization of modelOrganizationIconOrganizations) {
    const data = await readFile(new URL(`../public/model-org-icons/${organization}.webp`, import.meta.url));
    assert.equal(data.subarray(0, 4).toString("ascii"), "RIFF", organization);
    assert.equal(data.subarray(8, 12).toString("ascii"), "WEBP", organization);
    const chunk = data.subarray(12, 16).toString("ascii");
    let width = 0;
    let height = 0;
    if (chunk === "VP8 ") {
      width = data.readUInt16LE(26) & 0x3fff;
      height = data.readUInt16LE(28) & 0x3fff;
    } else if (chunk === "VP8L") {
      width = 1 + ((data[21] | (data[22] << 8)) & 0x3fff);
      height = 1 + (((data[22] >> 6) | (data[23] << 2) | (data[24] << 10)) & 0x3fff);
    } else if (chunk === "VP8X") {
      width = 1 + data.readUIntLE(24, 3);
      height = 1 + data.readUIntLE(27, 3);
    }
    assert.deepEqual([width, height], [96, 96], organization);
  }
});

test("Hygon compute vendor uses the reviewed horizontal WebP logo", async () => {
  const data = await readFile(new URL("../public/vendor-logos/hygon.webp", import.meta.url));
  assert.equal(data.subarray(0, 4).toString("ascii"), "RIFF");
  assert.equal(data.subarray(8, 12).toString("ascii"), "WEBP");
});

test("organization fallback colors are stable and varied across missing-logo organizations", () => {
  assert.equal(modelOrganizationColor("my-templates"), "var(--organization-avatar-5)");
  assert.equal(modelOrganizationColor(" MY-TEMPLATES "), "var(--organization-avatar-5)");
  const organizations = ["baseten", "fishaudio", "tinyllama", "lmsys", "opendatalab", "my-templates"];
  const colors = organizations.map(modelOrganizationColor);
  assert.ok(new Set(colors).size >= 3);
  organizations.reverse().forEach(org => {
    assert.equal(modelOrganizationColor(org.toUpperCase()), modelOrganizationColor(org));
    assert.match(modelOrganizationColor(org), /^var\(--organization-avatar-[1-5]\)$/);
  });
});

test("all organization artwork shades have at least 4.5:1 contrast with white initials", async () => {
  const css = await readFile(new URL("../src/app.css", import.meta.url), "utf8");
  const colors = [...css.matchAll(/--organization-avatar-([1-5]): #([0-9A-F]{6});/g)];
  assert.equal(colors.length, 5);
  for (const [, index, hex] of colors) {
    const linear = [0, 2, 4].map(offset => {
      const channel = parseInt(hex.slice(offset, offset + 2), 16) / 255;
      return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
    });
    const luminance = linear[0] * 0.2126 + linear[1] * 0.7152 + linear[2] * 0.0722;
    assert.ok(1.05 / (luminance + 0.05) >= 4.5, `avatar shade ${index}`);
  }
});
