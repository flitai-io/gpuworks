import { readFileSync, writeFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import tokens from '../ui/src/theme/zeron/index.mjs';

const root = new URL('../', import.meta.url);
const source = JSON.parse(readFileSync(new URL('ui/src/theme/zeron/source.json', root), 'utf8'));
for (const [file, expected] of [['index.mjs', source.indexSha256], ['shadow-support.json', source.supportSha256]]) {
  const actual = createHash('sha256').update(readFileSync(new URL(`ui/src/theme/zeron/${file}`, root))).digest('hex');
  if (actual !== expected) throw new Error(`Pinned Zeron source changed: ${file}`);
}
const support = JSON.parse(readFileSync(new URL('ui/src/theme/zeron/shadow-support.json', root), 'utf8'));
const light = { ...support, 'font-family-sans': tokens.fonts.family };
const dark = { ...light };
for (const [items, prefix] of [
  [[...tokens.colors, ...tokens.supportColors, ...tokens.componentColors], ''],
  [tokens.surfaces, 'surface-'],
  [tokens.shadows, 'shadow-'],
]) {
  for (const token of items) {
    light[prefix + token.name] = token.light;
    dark[prefix + token.name] = token.dark;
  }
}
for (const token of tokens.typography) {
  for (const values of [light, dark]) {
    values[`font-size-${token.name}`] = token.size;
    values[`line-height-${token.name}`] = token.lineHeight;
  }
}
for (const [items, prefix] of [
  [tokens.motionDurations, 'motion-duration-'],
  [tokens.borderWidths, 'border-width-'],
  [tokens.controlHeights, 'control-height-'],
  [tokens.badgeHeights, 'badge-height-'],
  [tokens.layers, 'layer-'],
]) for (const token of items) {
  light[prefix + token.name] = dark[prefix + token.name] = String(token.value);
}
// One spelling per colour: Zeron mixes #0060D2 and #0060d2, and code that
// compares or mixes colours must not see the same colour two ways.
const lowerHex = value => value.replace(/#[0-9a-fA-F]{3,8}\b/g, hex => hex.toLowerCase());
for (const values of [light, dark]) for (const key of Object.keys(values)) values[key] = lowerHex(values[key]);
for (const values of [light, dark]) for (const value of Object.values(values)) {
  for (const match of value.matchAll(/var\(--([\w-]+)\)/g)) if (!(match[1] in values)) throw new Error(`Unresolved token: ${match[1]}`);
}
const ts = `// Generated from pinned Zeron tokens. Do not edit. The CSS variables are\n// written by the Zeron CLI into src/app.css; tests/zeronTheme.test.ts keeps them equal.\nexport const zeronTokens = ${JSON.stringify({light,dark},null,2)} as const;\n`;
for (const [file, content] of [['ui/src/theme/zeron.generated.ts', ts]]) {
  const path = fileURLToPath(new URL(file, root));
  if (process.argv.includes('--check')) {
    if (readFileSync(path, 'utf8') !== content) throw new Error(`Stale generated tokens: ${file}`);
  } else writeFileSync(path, content);
}
console.log(`Zeron ${source.commit}: ${Object.keys(light).length} tokens per mode for antd and charts`);
