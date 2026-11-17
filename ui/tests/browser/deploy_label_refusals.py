#!/usr/bin/env python3
"""Real Chromium component regression with faithful node endpoint fixtures.

Run with Python Playwright + Chromium installed. Uses real NodeSection/CSS,
not a live control plane. GWKS117_SCREENSHOT optionally saves the initial UI.
"""
import os
import subprocess
import unittest
from pathlib import Path
from playwright.sync_api import expect, sync_playwright
from reduced_motion import BUILD

UI = Path(__file__).resolve().parents[2]
FIXTURE = """
const React = require('react');
const {createRoot} = require('react-dom/client');
const {ConfigProvider, Form} = require('antd');
const {I18nextProvider} = require('react-i18next');
const i18n = require('./src/i18n').default;
const {themeConfig} = require('./src/theme/tokens');
const NodeSection = require('./src/pages/deploy/DeployNodeSection').default;
const {nodes, evaluation} = require('./tests/helpers/refusedLabels');
const {installFleet} = require('./tests/helpers/fakeFleet');
const {api} = require('./src/api');
if (globalThis.GWKS117_SCENARIO === 'resource') {
 evaluation.eligible_nodes[13].eligible = true;
 evaluation.eligible_nodes[13].reasons = [];
 evaluation.candidates[13].reason = {code:'no_fit',params:{gpus:1,tp:1,pp:1,gpu_count:1,required_gib:20,available_gib:11}};
}
const fake = installFleet(api, nodes);
const h = React.createElement;
i18n.addResourceBundle('zh', 'deploy', require('./src/i18n/locales/zh/deploy.json'), true, true);
i18n.addResourceBundle('zh', 'nodes', require('./src/i18n/locales/zh/nodes.json'), true, true);
function App({fleet}) {
 const [form] = Form.useForm();
 return h(I18nextProvider,{i18n},h(ConfigProvider,{theme:themeConfig},
 h('main',{className:'deploy-workbench',style:{margin:32,maxWidth:1100}},
 h(Form,{form,initialValues:{node_selection_mode:'auto',target_vendor:'nvidia',node_labels:{runtime_type:'sandbox'}}},
 h(NodeSection,{fleet,knownNodes:new Map(),eligibleNodes:evaluation.eligible_nodes,modelReady:true,result:evaluation,loading:false,onNodeMode:()=>{}})))));
}
Promise.all([i18n.changeLanguage('zh'),fake.nodeFacets({state:['online']})]).then(([,fleet])=>createRoot(document.getElementById('root')).render(h(App,{fleet})));
"""

class LabelRefusals(unittest.TestCase):
    def test_selected_node_visible_and_keyboard_expands_all_evidence(self):
        self.exercise('hardware')

    def test_planning_failure_exposes_zero_after_eligibility(self):
        self.exercise('resource')

    def exercise(self, scenario):
        bundle = subprocess.run(['node','-e',BUILD,str(UI/'node_modules/esbuild'),FIXTURE,str(UI)], check=True,capture_output=True,text=True).stdout
        with sync_playwright() as p:
            browser = p.chromium.launch()
            page = browser.new_page(viewport={'width':1440,'height':1080})
            errors=[]
            page.on('pageerror',lambda error: errors.append(str(error)))
            page.set_content('<!doctype html><div id="root"></div>')
            page.add_style_tag(content=(UI/'src/styles.css').read_text())
            for css in ['deployWizard.css','deployRuntimePreview.css','deployWizardResponsive.css','deployLayout.css']:
                page.add_style_tag(content=(UI/'src/pages/deploy'/css).read_text())
            page.evaluate('(scenario) => { globalThis.GWKS117_SCENARIO = scenario; }', scenario)
            page.add_script_tag(content=bundle)
            expect(page.locator('.deploy-node-refusals li').first).to_be_visible()
            if os.environ.get('GWKS117_SCREENSHOT'):
                page.screenshot(path=os.environ['GWKS117_SCREENSHOT'].replace('.png', '-resource.png') if scenario == 'resource' else os.environ['GWKS117_SCREENSHOT'],full_page=True)
            expect(page.locator('.deploy-node-refusals li').first).to_contain_text('selected-turing')
            if scenario == 'hardware':
                expect(page.locator('.deploy-node-refusals').first).to_contain_text('7.5')
                expect(page.locator('.deploy-node-refusals').first).to_contain_text('>=8.0')
                expect(page.get_by_text('集群 14 台 → 节点条件匹配 1 台 → 硬件及调度条件通过 0 台',exact=True)).to_be_visible()
            else:
                expect(page.get_by_text('集群 14 台 → 节点条件匹配 1 台 → 硬件及调度条件通过 1 台 · 资源规划：通过 0 台 / 已评估 1 台；未评估 0 台',exact=True)).to_be_visible()
            more=page.get_by_role('button',name='展开其他 13 个被拒绝节点')
            more.focus()
            page.keyboard.press('Enter')
            expect(page.locator('.deploy-node-refusals li')).to_have_count(14)
            expect(page.get_by_text('other-12',exact=True)).to_be_visible()
            self.assertFalse(errors)
            browser.close()

if __name__=='__main__': unittest.main()
