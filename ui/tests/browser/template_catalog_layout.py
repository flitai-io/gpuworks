#!/usr/bin/env python3
"""Real component geometry with offline catalog fixtures; no management server.

Build UI first, then run make test-ui-template-catalog-layout. Fixture counts
and hardware names are synthetic stress data, not product support claims.
"""
import os
from itertools import product
from pathlib import Path
import subprocess
import unittest
from urllib.parse import urlparse

from playwright.sync_api import expect, sync_playwright
from reduced_motion import BUILD, UI

FIXTURE = r"""
const React = require('react');
const {createRoot} = require('react-dom/client');
const {MemoryRouter} = require('react-router-dom');
const {I18nextProvider} = require('react-i18next');
const {api} = require('./src/api');
const Page = require('./src/pages/InferenceTemplatesPage').default;
const {SideDrawer} = require('./src/ui-kit/SideDrawer');
const MonitorPage = require('./src/pages/monitor/MonitorPage').default;
const {ConfigProvider} = require('antd');
const {themeConfig} = require('./src/theme/tokens');
api.monitorOverview = async () => require('./tests/fixtures/monitor-overview.json');
api.monitorDistributions = async () => ({from_ms:0,to_ms:0,token_buckets:[0,2,4],ttft_bounds_ms:[100],input_output:[],uncached_ttft:[],coverage:{total:0,with_usage:0,with_cache:0}});
api.monitorMetrics = async () => ({items:[]});
const i18n = require('i18next').createInstance();
const h = React.createElement;
const vendors = ['nvidia', 'ascend', 'amd', 'hygon', 'ppu', 'kunlunxin'];
const chips = vendors.flatMap(vendor => Array.from({length:8}, (_,i) => ({vendor, name:`MOCK-${vendor}-${i}`})));
const models = ['black-forest-labs', 'thinkingmachines', 'deepseek-ai', 'baseten', 'fishaudio', 'tinyllama'].flatMap(org => Array.from({length:12}, (_,i) => ({
 id:`${org}-${i}`, model_id:`${org}/MOCK-long-model-name-${i}`, source:'upstream',
 specs:vendors.map((vendor,j)=>({id:j,name:`MOCK-spec-${j}`,engine:'vllm',quantization:'fp8',
 gpus:{vendor,name:`MOCK-${vendor}-0`},weight_sources:[{provider:'huggingface',repo_id:`${org}/MOCK-weights-${i}`}],
 api:{operations:[{type:'openai.chat'}]}}))
})));
api.inferenceTemplates = async () => ({items:models,total:models.length,page:1,page_size:200});
api.detectedInferenceTemplateChips = async () => ({items:chips});
api.inferenceTemplateChips = async () => ({items:chips});
const drawer = new URLSearchParams(location.search).has('drawer');
const monitor = new URLSearchParams(location.search).has('monitor');
const locale = new URLSearchParams(location.search).get('locale') || 'en';
i18n.init({lng:locale,defaultNS:'inferenceTemplates',interpolation:{escapeValue:false},resources:{en:{
 monitoring:require('./src/i18n/locales/en/monitoring.json'),inferenceTemplates:require('./src/i18n/locales/en/inferenceTemplates.json'),common:require('./src/i18n/locales/en/common.json')
},zh:{monitoring:require('./src/i18n/locales/zh/monitoring.json'),inferenceTemplates:require('./src/i18n/locales/zh/inferenceTemplates.json'),common:require('./src/i18n/locales/zh/common.json')}}}).then(()=>createRoot(document.getElementById('root')).render(h(I18nextProvider,{i18n},
 h(MemoryRouter,{initialEntries:[monitor ? '/monitor' : '/inference-templates']},h('div',{style:{marginLeft:208,padding:24,minWidth:0}},
 drawer ? h(SideDrawer,{open:true,onClose(){},title:'Geometry drawer'},'Drawer body') : monitor ? h(ConfigProvider,{theme:themeConfig},h(MonitorPage)) : h(Page))))));
"""


class TemplateCatalogLayout(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bundle = subprocess.run(
            ["node", "-e", BUILD, str(UI / "node_modules/esbuild"), FIXTURE, str(UI)],
            capture_output=True, text=True, check=True,
        ).stdout
        css_files = list((UI.parent / "internal/server/webui/dist/assets").glob("*.css"))
        if not css_files:
            raise RuntimeError("Run npm run build in ui first")
        cls.css = "\n".join(path.read_text() for path in css_files)
        cls.playwright = sync_playwright().start()
        cls.browser = cls.playwright.chromium.launch(headless=True)

    @classmethod
    def tearDownClass(cls):
        cls.browser.close()
        cls.playwright.stop()

    def render_page(self, width, dark=False, drawer=False, monitor=False, locale="en"):
        page = self.browser.new_page(viewport={"width": width, "height": 1000})
        self.addCleanup(page.close)

        def route(request):
            path = urlparse(request.request.url).path
            asset = UI / "public" / path.lstrip("/")
            if path.startswith(("/assets/", "/model-org-icons/", "/vendor-logos/")) and asset.is_file():
                request.fulfill(path=str(asset))
            elif path == "/":
                request.fulfill(content_type="text/html", body='<!doctype html><div id="root"></div>')
            else:
                request.abort()

        page.route("**/*", route)
        page.goto("http://template-layout.test/?locale=" + locale + ("&drawer" if drawer else "&monitor" if monitor else ""))
        if dark:
            page.evaluate("document.documentElement.classList.add('dark')")
        page.add_style_tag(content=self.css)
        page.add_script_tag(content=self.bundle)
        return page

    def capture(self, page, name):
        output = os.environ.get("GPUWORKS_LAYOUT_SCREENSHOT_DIR")
        if output:
            target = Path(output)
            target.mkdir(parents=True, exist_ok=True)
            page.screenshot(path=str(target / name), full_page=True)

    def test_dense_catalog_owns_scroll_and_keeps_names_and_logos(self):
        for width in (1280, 1920):
            for dark in (False, True):
                with self.subTest(width=width, dark=dark):
                    page = self.render_page(width, dark)
                    expect(page.locator('[data-slot="table"]')).to_be_visible()
                    expect(page.locator('.template-hardware-group > .vendor-logo')).to_have_count(6)
                    for name in ('Black Forest Labs', 'Thinking Machines'):
                        label = page.locator('.template-organization-sidebar nav strong').filter(has_text=name)
                        expect(label).to_be_visible()
                        self.assertTrue(label.evaluate('el => el.scrollWidth <= el.clientWidth + 1'))
                    chip_label = page.locator('.template-hardware-filters > .template-filter-label').bounding_box()
                    first_brand = page.locator('.template-hardware-group').first.bounding_box()
                    self.assertLess(abs(chip_label['y'] - first_brand['y']), 8)
                    self.assertGreater(first_brand['x'], chip_label['x'] + chip_label['width'])
                    expect(page.get_by_text('Model type', exact=True)).to_be_visible()
                    heading = page.locator('.template-filter-heading')
                    surface = page.locator('.template-filter-panel [role="region"] .bg-surface-floating')
                    self.assertNotEqual(heading.evaluate('el=>getComputedStyle(el).backgroundColor'), surface.evaluate('el=>getComputedStyle(el).backgroundColor'))
                    expect(page.get_by_text('Chip models', exact=True)).to_be_visible()
                    expect(page.get_by_text('Detected in this environment', exact=True)).to_have_count(0)
                    heights = page.locator('[data-slot="table-head"]').evaluate_all('els=>els.map(el=>el.getBoundingClientRect().height)')
                    shortest_row = min(page.locator('[data-slot="table-body"] tr').evaluate_all('els=>els.map(el=>el.getBoundingClientRect().height)'))
                    self.assertTrue(all(abs(height-shortest_row) <= 1 for height in heights), (heights, shortest_row))
                    self.assertTrue(page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1'))
                    container = page.locator('[data-slot="table-container"]')
                    if width == 1280:
                        self.assertTrue(container.evaluate('el=>el.scrollWidth > el.clientWidth'))
                        container.evaluate('el=>el.scrollLeft=200')
                        self.assertGreater(container.evaluate('el=>el.scrollLeft'), 0)
                    name_cell = page.locator('.resource-table-name').first
                    self.assertGreaterEqual(name_cell.bounding_box()['x'], container.bounding_box()['x'] - 1)
                    self.capture(page, f'templates-{width}-{"dark" if dark else "light"}.png')

    def test_collapsing_filters_returns_space_to_the_catalog(self):
        page = self.render_page(1280)
        table = page.locator('[data-slot="table"]')
        expect(table).to_be_visible()
        before = table.bounding_box()['y']
        toggle = page.get_by_role('button', name='Filters', exact=True)
        expect(toggle).to_have_attribute('aria-expanded', 'true')
        toggle.click()
        expect(toggle).to_have_attribute('aria-expanded', 'false')
        expect(page.locator('.template-hardware-filters')).not_to_be_visible()
        self.assertLess(table.bounding_box()['y'], before - 80)
        toggle.click()
        expect(page.locator('.template-hardware-filters')).to_be_visible()

    def test_header_hit_area_excludes_clear_and_chevron_is_last(self):
        page = self.render_page(1280)
        expect(page.locator('[data-slot="table"]')).to_be_visible()
        header = page.locator('.template-filter-heading')
        toggle = header.locator('button[aria-expanded]')
        clear = header.get_by_role('button', name='Clear filters', exact=True)
        arrow = toggle.locator(':scope > span').last
        self.assertGreater(arrow.bounding_box()['x'], clear.bounding_box()['x'] + clear.bounding_box()['width'])
        # Even while disabled, the clear button must not leak its click to the
        # full-width collapse trigger beneath it.
        box = clear.bounding_box()
        page.mouse.click(box['x'] + box['width']/2, box['y'] + box['height']/2)
        expect(toggle).to_have_attribute('aria-expanded', 'true')
        box = header.bounding_box()
        page.mouse.click(box['x'] + 3, box['y'] + box['height']/2)
        expect(toggle).to_have_attribute('aria-expanded', 'false')
        page.mouse.click(box['x'] + box['width'] - 4, box['y'] + box['height']/2)
        expect(toggle).to_have_attribute('aria-expanded', 'true')
        expect(page.locator('.template-hardware-filters')).to_be_visible()
        page.locator('.template-hardware-filters button').first.click()
        clear.click()
        expect(toggle).to_have_attribute('aria-expanded', 'true')

    def test_plain_dialog_prose_inherits_global_body_size_in_both_themes(self):
        for locale, width, dark in product(("en", "zh"), (820, 1280, 1920), (False, True)):
            title = "Edit chips" if locale == "en" else "编辑芯片"
            page = self.render_page(width, dark=dark, locale=locale)
            expect(page.locator('[data-slot="table"]')).to_be_visible()
            page.get_by_role('button', name=title, exact=True).click()
            dialog = page.get_by_role('dialog', name=title, exact=True)
            expect(dialog).to_be_visible()
            label = dialog.locator('.template-chip-options label > span').first
            expect(label).to_be_visible()
            self.assertEqual(label.evaluate('el=>getComputedStyle(el).fontSize'), '14px')
            self.assertEqual(page.locator('body').evaluate('el=>getComputedStyle(el).fontSize'), '14px')
            self.assertEqual(dialog.get_by_role('heading', name=title).evaluate('el=>getComputedStyle(el).fontSize'), '18px')
            self.capture(page, f'chip-editor-{locale}-{width}-{"dark" if dark else "light"}.png')

    def test_monitor_services_have_spacing_after_signal_cards_and_a_body_sized_title(self):
        for locale, width, dark in product(("en", "zh"), (820, 1280, 1920), (False, True)):
            page = self.render_page(width, monitor=True, dark=dark, locale=locale)
            heading = page.get_by_role("heading", name="Inference Services" if locale == "en" else "推理服务", exact=True)
            expect(heading).to_be_visible()
            link = page.locator('.monitor-services-section td a[href]').first
            expect(link).to_be_visible()
            self.assertTrue(link.evaluate("el => {const probe=document.createElement('span');probe.style.color='var(--fg-brand)';el.append(probe);const matches=getComputedStyle(el).color===getComputedStyle(probe).color;probe.remove();return matches}"))
            link.hover()
            self.assertIn('underline', link.evaluate('el=>getComputedStyle(el).textDecorationLine'))
            cards = page.locator('.monitor-signal-grid').bounding_box()
            toolbar = page.locator('.monitor-services-section .resource-table-toolbar').bounding_box()
            self.assertAlmostEqual(toolbar['y'] - cards['y'] - cards['height'], 16, delta=1)
            self.assertEqual(heading.evaluate('el=>getComputedStyle(el).fontSize'), '14px')
            self.assertTrue(page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1'))
            self.capture(page, f'monitor-spacing-{locale}-{width}-{dark}.png')

    def test_shared_table_text_links_keep_button_and_disabled_variants(self):
        for dark in (False, True):
            page = self.render_page(1280, monitor=True, dark=dark)
            expect(page.locator('.monitor-services-section td a[href]').first).to_be_visible()
            result = page.evaluate("""() => {
                const host = document.createElement('table');
                host.innerHTML = `<tbody><tr>
                  <td data-slot="table-cell"><a href="/nodes">Node</a><a href="/nodes" aria-disabled="true" class="text-fg-muted">Disabled</a><a href="/nodes" data-slot="button" class="text-fg-on-primary-action">Button</a><span>Plain</span></td>
                  <td class="ant-table-cell"><a href="/gpus">GPU</a><a class="ant-btn text-fg-danger" href="/delete">Action</a></td>
                </tr></tbody>`;
                document.body.append(host);
                const probe = document.createElement('span'); host.append(probe);
                const token = name => {probe.style.color=`var(--${name})`;return getComputedStyle(probe).color};
                const colors = [...host.querySelectorAll('a')].map(el=>getComputedStyle(el).color);
                const expected = ['fg-brand','fg-muted','fg-on-primary-action','fg-brand','fg-danger'].map(token);
                const plain = getComputedStyle(host.querySelector('td span')).color;
                const cell = getComputedStyle(host.querySelector('td')).color;
                host.remove();return {colors,expected,plain,cell};
            }""")
            self.assertEqual(result['colors'], result['expected'])
            self.assertEqual(result['plain'], result['cell'])

    def test_missing_organization_logos_use_stable_varied_colors(self):
        for dark in (False, True):
            page = self.render_page(1280, dark=dark)
            expect(page.locator('[data-slot="table"]')).to_be_visible()
            fallbacks = page.locator('.template-organization-fallback')
            expect(fallbacks.first).to_be_visible()
            self.assertTrue(fallbacks.evaluate_all("""els => els.every(el => {
                const probe = document.createElement('span');
                probe.style.backgroundColor = el.dataset.color;
                el.append(probe);
                const same = getComputedStyle(el).backgroundColor === getComputedStyle(probe).backgroundColor;
                probe.remove();return same;
            })"""))
            self.assertTrue(fallbacks.evaluate_all("els=>els.every(el=>getComputedStyle(el).color === 'rgb(255, 255, 255)')"))
            colors = fallbacks.evaluate_all('els=>els.map(el=>getComputedStyle(el).backgroundColor)')
            self.assertGreaterEqual(len(set(colors)), 3)
            before = fallbacks.evaluate_all('els=>els.map(el=>[el.textContent,getComputedStyle(el).backgroundColor])')
            fresh = self.render_page(1280, dark=dark)
            expect(fresh.locator('[data-slot="table"]')).to_be_visible()
            self.assertEqual(before, fresh.locator('.template-organization-fallback').evaluate_all('els=>els.map(el=>[el.textContent,getComputedStyle(el).backgroundColor])'))
            official = page.locator('img.template-organization-logo[src="/model-org-icons/deepseek-ai.webp"]')
            expect(official).to_be_visible()
            page.wait_for_function("() => document.querySelector('img.template-organization-logo[src=\"/model-org-icons/deepseek-ai.webp\"]')?.naturalWidth === 96")
            self.capture(page, f'organization-colors-{dark}.png')

    def test_drawer_has_square_corners(self):
        page = self.render_page(1280, drawer=True)
        dialog = page.get_by_role('dialog', name='Geometry drawer')
        expect(dialog).to_be_visible()
        corners = dialog.evaluate('el=>{const s=getComputedStyle(el);return [s.borderTopLeftRadius,s.borderTopRightRadius,s.borderBottomLeftRadius,s.borderBottomRightRadius]}')
        self.assertEqual(corners, ['0px'] * 4)


if __name__ == '__main__':
    unittest.main()
