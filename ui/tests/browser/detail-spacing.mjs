// Component layout regression with synthetic data; this is not a live network/SSH test.
import assert from 'node:assert/strict';
import { mkdir, readFile, writeFile, unlink } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { createServer } from 'vite';
import { chromium } from 'playwright-core';
const root = fileURLToPath(new URL('../../', import.meta.url));
const out = resolve(process.argv[2] ?? '/tmp/gpuworks-detail-spacing');
await mkdir(out, { recursive: true });
const fixture = resolve(root, '.__detail-spacing.html');
const network = JSON.parse(await readFile(resolve(root, 'tests/fixtures/network-detail.json'), 'utf8'));
// Exercise a resolved Bootstrap address and a missing one in the same drawer.
network.nodes[1].ip = '';
const html = `<!doctype html><html><head><meta charset="utf-8"><script>
const p=new URLSearchParams(location.search);document.documentElement.dataset.theme=p.get('theme');document.documentElement.classList.toggle('dark',p.get('theme')==='dark');
</script></head><body><div id="root"></div><script type="module">
import '/src/app.css';import '/src/theme/layers.css';import '/src/components/detail/detailDrawer.css';
import React from 'react';import {createRoot} from 'react-dom/client';import {ConfigProvider,theme as antdTheme} from 'antd';import {StyleProvider} from '@ant-design/cssinjs';import {MemoryRouter} from 'react-router-dom';
import {createInstance} from 'i18next';import {I18nextProvider,initReactI18next} from 'react-i18next';import {themeConfig,darkPreview} from '/src/theme/tokens.ts';
import DetailDrawer from '/src/components/detail/DetailDrawer.tsx';import {NodeRecoveryStatus} from '/src/components/NodeRecoveryControls.tsx';
import NetworkDetailDrawer from '/src/pages/networks/NetworkDetailDrawer.tsx';import {api} from '/src/api.ts';
const bundles=import.meta.glob('/src/i18n/locales/*/*.json',{eager:true,import:'default'});const resources={};for(const [path,data] of Object.entries(bundles)){const parts=path.split('/');(resources[parts.at(-2)]??={})[parts.at(-1).replace('.json','')]=data;}
const locale=new URLSearchParams(location.search).get('locale');const i18n=createInstance();await i18n.use(initReactI18next).init({lng:locale,resources,defaultNS:'common',interpolation:{escapeValue:false}});
const e=React.createElement,view=createRoot(document.getElementById('root'));const detail=${JSON.stringify(network)};
api.me=async()=>({username:'layout-admin',role:'admin'});api.network=async(id)=>id===0?{...detail,id:0,builtin:true,type:'host_ethernet'}:detail;
const node={uuid:'layout-node',recovery_phase:'recovered',recovery_attempts:1,last_probe_at:'2026-11-14T10:00:00Z'};
window.renderCase=(kind)=>{const child=kind.startsWith('node')?e(DetailDrawer,{key:kind,open:true,onClose:()=>{},title:'GPU server',subtitle:'gpu-server-01',banner:e(NodeRecoveryStatus,{node:kind==='node-empty'?{...node,recovery_phase:''}:node,onUpdated:()=>{}}),tabs:[{key:'basic',label:i18n.t('detail.tab.basic',{ns:'nodes'}),children:e('div',{className:'text-body'},'Node details')}]}):e(NetworkDetailDrawer,{key:kind,id:kind==='management'?0:detail.id,onTab:()=>{},onClose:()=>{},onChanged:()=>{}});view.render(e(StyleProvider,{layer:true},e(I18nextProvider,{i18n},e(ConfigProvider,{theme:{algorithm:darkPreview?antdTheme.darkAlgorithm:antdTheme.defaultAlgorithm,...themeConfig}},e(MemoryRouter,null,child)))));};window.ready=true;
</script></body></html>`;
async function checkExpandedNodes(page, locale) {
  const strings = JSON.parse(await readFile(resolve(root, `src/i18n/locales/${locale}/networks.json`), 'utf8'));
  const results = [];
  for (const node of network.nodes) {
    const row = page.getByRole('row').filter({has: page.getByRole('link', {name: node.node_name, exact: true})});
    await row.locator('td:first-child button[aria-expanded="false"]').click();
    const panel = row.locator('xpath=following-sibling::tr[1]').locator('dl');
    const value = panel.getByText(node.ip ? [node.interface, node.ip].filter(Boolean).join(' · ') : strings.rail.noBootstrap, {exact: true});
    const change = panel.getByRole('button', {name: strings.rail.changeBootstrap, exact: true});
    await value.waitFor();
    await change.waitFor();
    const fields = await panel.locator('dt').evaluateAll(labels => labels.map(label => {
      const value = label.nextElementSibling;
      const a = label.getBoundingClientRect(), b = value.getBoundingClientRect();
      return {label: label.textContent, tag: value.tagName, left: b.left - a.right, top: b.top - a.top};
    }));
    assert.ok(fields.length >= 2, 'Bootstrap and HCA details must both render');
    for (const field of fields) {
      assert.equal(field.tag, 'DD');
      assert.ok(field.left >= 0 && Math.abs(field.top) < 2, 'label and value must share a row: '+JSON.stringify(field));
    }
    const bodySize = await panel.evaluate(el => {
      const probe = document.createElement('span');
      probe.style.fontSize = 'var(--font-size-body)';
      el.append(probe);
      const size = getComputedStyle(probe).fontSize;
      const token = getComputedStyle(probe).getPropertyValue('--font-size-body').trim();
      probe.remove();
      return {size, token};
    });
    assert.ok(bodySize.token, 'the body font token must be loaded');
    for (const target of [value, change]) assert.equal(await target.evaluate(el => getComputedStyle(el).fontSize), bodySize.size);
    results.push({node: node.node_name, fields, bodySize});
    await row.locator('td:first-child button[aria-expanded="true"]').click();
  }
  return results;
}
let server, browser;
try {
  await writeFile(fixture, html);
  server=await createServer({root,server:{host:'127.0.0.1',port:0}});await server.listen();
  browser=await chromium.launch(process.env.CHROME_PATH ? {executablePath:process.env.CHROME_PATH,headless:true} : {channel:'chrome',headless:true});
  const results=[];
  for(const theme of ['light','dark']) for(const locale of ['zh','en']) for(const width of [1440,1024,768]) {
    const page=await browser.newPage({viewport:{width,height:1000}});
    // The overview subscribes to server events and checks the session if that stream closes.
    await page.route('**/api/v1/events', route => route.fulfill({status: 204}));
    await page.route('**/api/v1/auth/me', route => route.fulfill({json: {username: 'layout-admin', role: 'admin'}}));
    await page.goto(server.resolvedUrls.local[0]+'.__detail-spacing.html?theme='+theme+'&locale='+locale);
    await page.waitForFunction(()=>window.ready);
    for(const kind of ['node','node-empty','network','management']) {
      await page.evaluate(kind=>window.renderCase(kind),kind);
      await page.getByRole('tablist').waitFor();
      if(kind==='management') await page.getByRole('checkbox').first().waitFor();
      await page.waitForFunction(()=>Array.from(document.getAnimations()).every(animation=>animation.playState!=='running'));
      const metrics=await page.evaluate(()=>{
        const body=document.querySelector('.detail-body'),tabs=body.querySelector('[role=tablist]'),banner=body.querySelector('section'),heading=body.querySelector('h5');
        return {overflow:body.scrollWidth-body.clientWidth,tabMargin:getComputedStyle(tabs).marginTop,bannerGap:banner?tabs.getBoundingClientRect().top-banner.getBoundingClientRect().bottom:null,headingOffset:heading&&banner?heading.getBoundingClientRect().top-body.getBoundingClientRect().top:null,tableCount:body.querySelectorAll('[data-slot=table]').length,tableOverflow:Math.max(0,...Array.from(body.querySelectorAll('[data-slot="table-container"]')).map(el=>el.scrollWidth-el.clientWidth)),
          // Body text is 14px; a bare span that falls back to the browser's 16px is the usual portal mistake.
          largeText:Array.from(body.querySelectorAll('*')).filter(el=>[...el.childNodes].some(n=>n.nodeType===3&&n.textContent.trim())&&!/^H[1-6]$/.test(el.tagName)&&parseFloat(getComputedStyle(el).fontSize)>14).map(el=>el.textContent.trim().slice(0,40)),
          // Header titles and single-line marks must be read whole, not cut by their column.
          clipped:Array.from(body.querySelectorAll('th *, td *')).filter(el=>{const cs=getComputedStyle(el);return [...el.childNodes].some(n=>n.nodeType===3&&n.textContent.trim())&&(cs.textOverflow==='ellipsis'||cs.overflowX!=='visible')&&el.scrollWidth>el.clientWidth+1&&el.getBoundingClientRect().width>1&&!el.closest('.detail-name-cap')&&!el.classList.contains('detail-name-cap');}).map(el=>el.textContent.trim().slice(0,40))};
      });
      assert.ok(metrics.overflow<=1,JSON.stringify({theme,locale,width,kind,metrics}));
      assert.equal(metrics.tabMargin,'0px');
      assert.deepEqual(metrics.largeText,[],'text larger than the 14px body: '+JSON.stringify({theme,locale,width,kind}));
      assert.deepEqual(metrics.clipped,[],'clipped header or mark: '+JSON.stringify({theme,locale,width,kind}));
      if(kind==='node'){assert.equal(metrics.bannerGap,16);assert.equal(metrics.headingOffset,0);}
      if(kind==='node-empty') assert.equal(metrics.bannerGap,null);
      if(kind==='management') assert.ok(metrics.tableOverflow<=1,'management role switches must fit: '+JSON.stringify(metrics));
      if (kind === 'network') metrics.expandedNodes = await checkExpandedNodes(page, locale);
      results.push({theme,locale,width,kind,...metrics});
      await page.screenshot({path:resolve(out,`${kind}-${theme}-${locale}-${width}.png`)});
    }
    await page.close();
  }
  await writeFile(resolve(out,'measurements.json'),JSON.stringify(results,null,2));
} finally {await browser?.close();await server?.close();await unlink(fixture).catch(()=>{});}
