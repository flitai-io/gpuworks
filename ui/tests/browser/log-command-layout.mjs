// Render real product components with deterministic API data in a real drawer.
// This verifies layout, not live SSH or backend integration.
import assert from 'node:assert/strict';
import { mkdir, writeFile, unlink } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { createServer } from 'vite';
import { chromium } from 'playwright-core';
const root = fileURLToPath(new URL('../../', import.meta.url));
const out = resolve(process.argv[2] ?? 'log-command-layout-report');
await mkdir(out, { recursive: true });
const fixture = resolve(root, '.__log-command-layout.html');
const html = `<!doctype html><html><head><meta charset="utf-8"><script>
const p=new URLSearchParams(location.search);document.documentElement.dataset.theme=p.get('theme');
document.documentElement.classList.toggle('dark',p.get('theme')==='dark');
</script></head><body><div id="root"></div><script type="module">
import '/src/app.css';import '/src/theme/layers.css';
import '/src/pages/deploy/deployRuntimePreview.css';
import React from 'react';import {createRoot} from 'react-dom/client';
import {ConfigProvider,Form,theme as antdTheme} from 'antd';import {StyleProvider} from '@ant-design/cssinjs';import {MemoryRouter} from 'react-router-dom';
import {createInstance} from 'i18next';import {I18nextProvider,initReactI18next} from 'react-i18next';
import {themeConfig,darkPreview} from '/src/theme/tokens.ts';
import {FindingCards} from '/src/components/AIAssistantParts.tsx';
import {SideDrawer} from '/src/ui-kit/SideDrawer.tsx';
import ApplyCommand from '/src/pages/kubernetes/ApplyCommand.tsx';
import KubeCommandList from '/src/pages/kubernetes/KubeCommandList.tsx';
import DeployRuntimeCommand from '/src/pages/deploy/DeployRuntimeCommand.tsx';
import {LogStream} from '/src/pages/InstanceLogStream.tsx';
import ServiceLogStream from '/src/pages/monitor/ServiceLogStream.tsx';
import {api} from '/src/api.ts';
const bundles=import.meta.glob('/src/i18n/locales/*/*.json',{eager:true,import:'default'});
const resources={};for(const [path,data] of Object.entries(bundles)) {const parts=path.split('/');const lang=parts.at(-2),ns=parts.at(-1).replace('.json','');(resources[lang]??={})[ns]=data;}
const locale=new URLSearchParams(location.search).get('locale');
const i18n=createInstance();await i18n.use(initReactI18next).init({lng:locale,resources,defaultNS:'common',interpolation:{escapeValue:false}});
const e=React.createElement;const view=createRoot(document.getElementById('root'));
const uuid='aaaaaaaa-1111-2222-3333-444444444444';
const long='GPUWorks日志与命令验证-'+ 'long-model-directory-'.repeat(24);
const command='kubectl apply -f https://example.invalid/manifests/'+long+'.yaml';
const events=[{id:1,kind:'state',code:'INSTANCE_STATE_CHANGED',params:{state:'running'},created_at:new Date(2000).toISOString()}];
api.logsSearch=async()=>({items:[{id:1,instance:uuid,node:'node-1',level:'info',source:'instance',time_ms:1000,line:'LONG_LOG '+long},{id:2,instance:uuid,node:'node-1',level:'warn',source:'instance',time_ms:3000,line:'WARN_LOG queue pressure'}]});
api.instanceEvents=async()=>({items:events,has_more:false});
window.renderCase=(kind)=>{
 let child;
 if(kind==='kubernetes') child=e(React.Fragment,null,e(ApplyCommand,{command}),e(KubeCommandList,{commands:[command,'kubectl get pods --all-namespaces']}));
 if(kind==='deployment'||kind==='deployment-edit') child=e(Form,{key:kind,initialValues:{command_mode:kind==='deployment-edit'?'manual':'engine',container_cmd:command}},e(DeployRuntimeCommand,{result:{command:['python','-m','vllm.entrypoints.openai.api_server','--model',long],env:{MODEL_PATH:long},mounts:[],warnings:[]},loading:false,error:null}));
 if(kind==='evidence') child=e(FindingCards,{findings:[{code:'TEST_EVIDENCE',evidence:[{line:long}]}]});
 if(kind==='instance') child=e(LogStream,{uuid,live:false,follow:false,timeline:{events,loading:false,error:null,hasMore:false,reload:()=>{}}});
 if(kind==='service') child=e(ServiceLogStream,{logInstances:[uuid],range:'1h'});
 view.render(e(StyleProvider,{layer:true},e(I18nextProvider,{i18n},e(ConfigProvider,{theme:{algorithm:darkPreview?antdTheme.darkAlgorithm:antdTheme.defaultAlgorithm,...themeConfig}},e(MemoryRouter,null,e(SideDrawer,{open:true,onClose:()=>{},title:'GPUWorks '+kind,width:720},e('div',{'data-case':kind,key:kind,style:{minWidth:0}},child)))))));
};window.ready=true;
</script></body></html>`;
let server, browser;
try {
  await writeFile(fixture, html);
  server = await createServer({ root, server: { host: '127.0.0.1', port: 0 } });
  await server.listen();
  browser = await chromium.launch({ channel: 'chrome', headless: true });
  const results = [];
  for (const theme of ['light','dark']) for (const locale of ['zh','en']) for (const width of [1440,768]) {
    const page = await browser.newPage({ viewport: { width, height: 1000 } });
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    await page.goto(`${server.resolvedUrls.local[0]}.__log-command-layout.html?theme=${theme}&locale=${locale}`);
    await page.waitForFunction(()=>window.ready);
    for (const kind of ['kubernetes','deployment','deployment-edit','instance','service','evidence']) {
      await page.evaluate(kind=>window.renderCase(kind),kind);
      await page.locator(`[data-case="${kind}"] pre, [data-case="${kind}"] textarea`).first().waitFor();
      if (kind==='instance'||kind==='service') await page.getByText(/LONG_LOG/).waitFor();
      await page.waitForFunction(()=>[...document.getAnimations()].every(a=>a.playState!=='running'));
      const measured=await page.evaluate(()=>{
        const dialog=document.querySelector('[role="dialog"]');
        const host=dialog.querySelector('[data-case]');
        return {dialogWidth:dialog.clientWidth,dialogScrollWidth:dialog.scrollWidth,hostWidth:host.clientWidth,hostScrollWidth:host.scrollWidth,
          code:[...host.querySelectorAll('pre, textarea')].map(el=>({font:getComputedStyle(el).fontSize,content:el.textContent.slice(0,45),client:el.clientWidth,scroll:el.scrollWidth})),
          eventColors:[...host.querySelectorAll('pre span')].filter(el=>el.style.background).map(el=>getComputedStyle(el).color)};
      });
      assert.deepEqual(errors,[]);assert.ok(measured.code.length>0);
      for(const code of measured.code) assert.equal(code.font,'14px',`${kind}: ${code.content}`);
      assert.ok(measured.dialogScrollWidth<=measured.dialogWidth+1,JSON.stringify({kind,...measured}));
      assert.ok(measured.hostScrollWidth<=measured.hostWidth+1,JSON.stringify({kind,...measured}));
      if(kind==='instance'||kind==='service') assert.ok(measured.eventColors.includes('rgb(161, 210, 254)'));
      await page.screenshot({path:resolve(out,`${kind}-${theme}-${locale}-${width}.png`),fullPage:true});
      results.push({kind,theme,locale,width,...measured});
    }
    await page.close();
  }
  await writeFile(resolve(out,'results.json'),JSON.stringify(results,null,2));
  console.log(`Passed ${results.length} real-component layout cases; ${out}`);
} finally { await browser?.close();await server?.close();await unlink(fixture); }
