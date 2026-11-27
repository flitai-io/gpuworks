#!/usr/bin/env python3
"""Chromium geometry regression for Agent upgrade result columns (no server)."""

import os
from pathlib import Path
import subprocess
import unittest

from playwright.sync_api import expect, sync_playwright
from reduced_motion import BUILD, UI

FIXTURE = r"""
const React = require('react');
const {createRoot} = require('react-dom/client');
const {ConfigProvider} = require('antd');
const {I18nextProvider} = require('react-i18next');
const i18n = require('i18next').createInstance();
const {themeConfig} = require('./src/theme/tokens');
const {api} = require('./src/api');
const Drawer = require('./src/components/agentUpgrades/UpgradeRunDrawer').default;
const h = React.createElement;
window.EventSource = class { addEventListener() {} removeEventListener() {} close() {} };
const params = new URLSearchParams(location.search);
const actions = params.has('actions');
const rows = [
 {node_uuid:'mock-1234567890abcdef1234567890abcdef', name:'MOCK-long-node-name-for-upgrade-layout-review-linux-amd64',
 from_version:'eb676e940f5cb7bd2f874f57b18277b012345678', current_version:'b4b6e7de559d0ec50df0782bed1f5ddae989d5ab',
 phase:'failed', reason:'transfer_failed', error:'failed to transfer upgrade artifact: connection reset by peer; request_id=mock-layout-1234567890abcdef1234567890abcdef',
 started_at:'2026-10-30T03:20:01Z', finished_at:'2026-10-30T03:20:59Z'},
 {node_uuid:'mock-success',name:'MOCK-success',from_version:'dev-old',current_version:'dev-target',phase:'succeeded',reason:'target_registered'},
 {node_uuid:'mock-offline',name:'MOCK-offline',from_version:'dev-old',phase:'skipped',reason:'offline',finished_at:'2026-10-30T03:20:01Z'},
];
if (actions) rows.push({node_uuid:'mock-unconfirmed',name:'MOCK-unconfirmed',from_version:'dev-old',phase:'unconfirmed',reason:'confirmation_timeout'});
const counts = {total:rows.length,processed:rows.length,succeeded:1,failed:1,skipped:1,cancelled:0,interrupted:0,unconfirmed:actions?1:0,queued:0,active:0};
api.agentUpgradeRun = async () => ({id:9001,target_version:'dev-target',state:'finished',counts});
api.agentUpgradeItems = async (_id,page=1,pageSize=20,phase='') => {
 const filtered=rows.filter(x=>!phase||x.phase===phase);
 return {items:filtered.slice((page-1)*pageSize,page*pageSize),total:filtered.length,page,page_size:pageSize};
};
i18n.init({lng:'zh',fallbackLng:'zh',defaultNS:'nodes',interpolation:{escapeValue:false},resources:{zh:{
 nodes:require('./src/i18n/locales/zh/nodes.json'),common:require('./src/i18n/locales/zh/common.json')
}}}).then(()=>createRoot(document.getElementById('root')).render(h(ConfigProvider,{theme:themeConfig},
 h(I18nextProvider,{i18n},h(Drawer,{runID:9001,isAdmin:!params.has('viewer'),onClose(){},onContinue(){},onChanged(){}})))));
"""


class AgentUpgradeResultsLayout(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bundle = subprocess.run(
            ["node", "-e", BUILD, str(UI / "node_modules/esbuild"), FIXTURE, str(UI)],
            capture_output=True, text=True, check=True,
        ).stdout
        cls.css = (UI / "src/styles.css").read_text()
        cls.playwright = sync_playwright().start()
        cls.browser = cls.playwright.chromium.launch(headless=True)

    @classmethod
    def tearDownClass(cls):
        cls.browser.close()
        cls.playwright.stop()

    def render_results(self, width, query=""):
        page = self.browser.new_page(viewport={"width": width, "height": 1000}, timezone_id="Asia/Shanghai")
        self.addCleanup(page.close)
        page.route("**/*", lambda route: route.fulfill(content_type="text/html", body='<!doctype html><div id="root"></div>'))
        page.goto("http://upgrade-layout.test/" + query)
        page.add_style_tag(content=self.css)
        page.add_script_tag(content=self.bundle)
        drawer = page.get_by_role("dialog", name="Agent 升级批次 9001")
        expect(drawer.get_by_text("MOCK-success", exact=True)).to_be_visible()
        page.wait_for_function("""() => [...document.querySelectorAll('.ant-drawer-content-wrapper')]
            .every(el => el.getAnimations({subtree: true}).every(a => a.playState !== 'running'))""")
        return page, drawer

    def capture(self, page, name):
        output = os.environ.get("GPUWORKS_LAYOUT_SCREENSHOT_DIR")
        if output:
            target = Path(output)
            target.mkdir(parents=True, exist_ok=True)
            page.screenshot(path=str(target / name), full_page=True)

    def test_long_versions_preserve_readable_columns_at_desktop_and_narrow_widths(self):
        for width in [1440, 1280, 1024]:
            with self.subTest(width=width):
                page, drawer = self.render_results(width)
                expect(drawer.get_by_role("columnheader", name="操作", exact=True)).to_have_count(0)
                widths = drawer.locator("thead th").evaluate_all("cells=>cells.map(x=>x.getBoundingClientRect().width)")
                self.assertEqual(len(widths), 6)
                self.assertGreaterEqual(min(widths), 140)
                time_cell = drawer.locator("tbody tr.ant-table-row").first.locator("td").nth(5)
                expect(time_cell).to_contain_text("开始：2026-10-30 11:20:01")
                expect(time_cell).to_contain_text("结束：2026-10-30 11:20:59")
                lines = time_cell.locator(".ant-typography").evaluate_all("xs=>xs.map(x=>({height:x.getBoundingClientRect().height,lineHeight:parseFloat(getComputedStyle(x).lineHeight)}))")
                self.assertTrue(all(x["height"] <= x["lineHeight"] + 1 for x in lines), lines)
                self.capture(page, f"results-{width}.png")

    def test_an_unknown_outcome_offers_no_action_and_stays_in_view(self):
        page, drawer = self.render_results(1024, "?actions")
        # No operation column exists any more: an outcome the platform settles
        # by itself is a result, not a chore for the administrator.
        expect(drawer.get_by_role("columnheader", name="操作", exact=True)).to_have_count(0)
        expect(drawer.get_by_role("button", name="核实旧操作已结束", exact=True)).to_have_count(0)
        row = drawer.get_by_role("row").filter(has_text="MOCK-unconfirmed")
        expect(row).to_contain_text("结果未知")
        expect(row).to_contain_text("没有在时限内确认升级结果")
        self.capture(page, "results-unknown-1024.png")
        # The table scrolls as a whole now that nothing is pinned to the right;
        # what matters is that the last column is reachable.
        drawer.locator(".ant-table-content, .ant-table-body").first.evaluate("el=>{el.scrollLeft=el.scrollWidth}")
        time_cell = row.locator("td").last
        expect(time_cell).to_be_visible()
        self.assertEqual(len(drawer.locator("thead th").evaluate_all("cells=>cells.map(x=>x.getBoundingClientRect().width)")), 6)

if __name__ == "__main__":
    unittest.main()
