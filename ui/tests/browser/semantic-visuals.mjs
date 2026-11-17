// Real Chromium regression check of shared CSS and production chart/terminal
// options. Uses synthetic data; does not claim server or SSH integration coverage.
// node tests/browser/semantic-visuals.mjs /absolute/output/directory
import assert from 'node:assert/strict';
import { mkdir, writeFile, unlink } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { createServer } from 'vite';
import { chromium } from 'playwright-core';
const root = fileURLToPath(new URL('../../', import.meta.url));
const out = resolve(process.argv[2] ?? 'semantic-visuals-report');
await mkdir(out, { recursive: true });
const fixture = resolve(root, '.__semantic-check.html');
const html = `<!doctype html><html><head><meta charset="utf-8"><script>
const p = new URLSearchParams(location.search);document.documentElement.dataset.theme=p.get('theme');
document.documentElement.classList.toggle('dark',p.get('theme')==='dark');
</script></head><body><main style="padding:24px;max-width:100%;font-size:16px">
<h1 style="font-size:18px">GPUWorks</h1><p id="body" style="font-size:var(--font-size-body)"></p>
<pre id="log"><span id="event">[system] Replica started / 副本已启动</span>\nINFO ready / 服务已就绪</pre>
<pre class="code-block text-label">curl http://localhost:8080/v1/models</pre>
<div class="ant-typography"><pre class="code-block">kubectl get pods</pre></div>
<div class="deploy-command-preview"><pre>docker run gpuworks</pre></div>
<div class="deploy-runtime-command"><pre>python -m vllm.entrypoints.openai.api_server</pre></div>
<div class="markdown-content"><pre><code>echo GPUWorks</code></pre></div>
<pre class="playground-preview-source">console.log('GPUWorks')</pre>
<pre class="playground-realtime-log">[INFO] connected</pre>
<pre class="template-command-preview">export MODEL_PATH=/models</pre>
<div class="template-operation-usage"><pre>curl localhost:8080/health</pre></div>
<pre style="background:#111827;color:#f9fafb">GPU health: OK</pre>
<div class="node-terminal" style="height:130px"><div id="terminal" class="node-terminal-surface"></div></div></main>
<script type="module">
import '/src/app.css';import '/src/theme/layers.css';
import '/src/pages/playground/playground.css';import '/src/pages/deploy/deployRuntimePreview.css';
import '/src/pages/inferenceTemplateFlat.css';import '/node_modules/@xterm/xterm/css/xterm.css';
import {Terminal} from '@xterm/xterm';
import {terminalOptions,logSurfaceStyle,logEventStyle} from '/src/theme/terminalTheme.ts';
const zh=new URLSearchParams(location.search).get('locale')==='zh';
document.getElementById('body').textContent=zh?'正文 14px；命令、终端 14px':'Body 14px; commands and terminal 14px';
document.querySelector('.node-terminal').style.background=terminalOptions.theme.background;
Object.assign(document.querySelector('#log').style,logSurfaceStyle);
Object.assign(document.querySelector('#event').style,logEventStyle);
const terminal=new Terminal({...terminalOptions,rows:5,cols:60});terminal.open(document.getElementById('terminal'));
terminal.write('\\x1b[35mMagenta / 紫色\\x1b[0m  normal\\r\\n\\x1b[31mRed\\x1b[32m Green\\x1b[33m Yellow\\x1b[34m Blue\\x1b[36m Cyan\\x1b[0m');
window.result={expectedBackground:terminal.options.theme.background,font:terminal.options.fontSize,contrast:terminal.options.minimumContrastRatio};
</script></body></html>`;
let server, browser;
try {
  await writeFile(fixture, html);
  server = await createServer({ root, server: { host: '127.0.0.1', port: 0 } });
  await server.listen();
  browser = await chromium.launch({ channel: 'chrome', headless: true });
  const results = [];
  for (const theme of ['light', 'dark']) for (const locale of ['zh', 'en']) for (const width of [1440, 768]) {
    const page = await browser.newPage({ viewport: { width, height: 1400 } });
    const errors = []; page.on('pageerror', (e) => errors.push(e.message));
    await page.goto(`${server.resolvedUrls.local[0]}.__semantic-check.html?theme=${theme}&locale=${locale}`);
    await page.waitForFunction(() => window.result, null, { timeout: 30000 }).catch((error) => { throw new Error(errors.join('\n') || error.message); });
    const result = await page.evaluate(() => ({...window.result,
      eventColor: getComputedStyle(document.querySelector('#event')).color,
      bodyFont: getComputedStyle(document.querySelector('#body')).fontSize,
      code: [...document.querySelectorAll('pre, pre code')].map(el => ({class:el.className,font:getComputedStyle(el).fontSize, family:getComputedStyle(el).fontFamily})),
      terminalBackground: getComputedStyle(document.querySelector('.node-terminal')).backgroundColor,
      xtermBackground: getComputedStyle(document.querySelector('.xterm-viewport')).backgroundColor,
      overflow: document.documentElement.scrollWidth > innerWidth,
    }));
    assert.deepEqual(errors, []); assert.equal(result.bodyFont, '14px'); assert.equal(result.eventColor, 'rgb(161, 210, 254)');
    for (const code of result.code) { assert.equal(code.font, '14px', code.class); assert.match(code.family, /monospace/); }
    const expectedBackground = `rgb(${result.expectedBackground.slice(1).match(/../g).map(v => parseInt(v,16)).join(', ')})`;
    assert.equal(result.terminalBackground, expectedBackground);
    assert.equal(result.xtermBackground, 'rgba(0, 0, 0, 0)', 'transparent viewport inherits the same terminal surface');
    assert.equal(result.font, 14); assert.equal(result.contrast, 4.5); assert.equal(result.overflow, false);
    await page.screenshot({ path: resolve(out, `${theme}-${locale}-${width}.png`), fullPage:true });
    results.push({theme,locale,width,...result}); await page.close();
  }
  await writeFile(resolve(out, 'results.json'), JSON.stringify(results,null,2));
  console.log(`Passed ${results.length} theme/locale/width combinations; ${out}`);
} finally {
  await browser?.close(); await server?.close(); await unlink(fixture);
}
