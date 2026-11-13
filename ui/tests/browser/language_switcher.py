#!/usr/bin/env python3
"""Real Chromium regression for language-menu pointer and keyboard access.

Run: python3 -B ui/tests/browser/language_switcher.py
Renders the shared component and app CSS without management API fixtures.
"""

import os
from pathlib import Path
import shutil
import subprocess
import unittest

from playwright.sync_api import expect, sync_playwright
from reduced_motion import BUILD, UI

FIXTURE = """
const React = require('react');
const {createRoot} = require('react-dom/client');
const {ConfigProvider, Layout} = require('antd');
const {I18nextProvider, useTranslation} = require('react-i18next');
const Detector = require('i18next-browser-languagedetector').default;
const i18n = require('i18next').createInstance();
const Switcher = require('./src/components/LanguageSwitcher').default;
const {themeConfig} = require('./src/theme/tokens');
const h = React.createElement;
const header = new URL(location.href).searchParams.has('header');
function App() {
  const {t} = useTranslation();
  const button = h(Switcher, {light: header});
  return h(ConfigProvider, {theme: themeConfig},
    header ? h(Layout.Header, {className: 'app-header'}, 'GPUWorks', button)
      : h('main', {className: 'auth-page'}, h('div', {className: 'auth-language-switcher'}, button)),
    h('output', {'data-testid': 'translated'}, t('language.switch')));
}
i18n.use(Detector).init({
  resources: {en: {common: require('./src/i18n/locales/en/common.json')}, zh: {common: require('./src/i18n/locales/zh/common.json')}},
  supportedLngs: ['en', 'zh'], fallbackLng: 'en', defaultNS: 'common', load: 'languageOnly',
  nonExplicitSupportedLngs: true, interpolation: {escapeValue: false},
  detection: {order: ['localStorage', 'navigator'], lookupLocalStorage: 'gpuworks.locale', caches: ['localStorage']},
}).then(() => createRoot(document.getElementById('root')).render(h(I18nextProvider, {i18n}, h(App))));
"""


class LanguageMenu(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bundle = subprocess.run(
            ["node", "-e", BUILD, str(UI / "node_modules/esbuild"), FIXTURE, str(UI)],
            check=True, capture_output=True, text=True,
        ).stdout
        cls.css = (UI / "src/styles.css").read_text()
        cls.playwright = sync_playwright().start()
        cls.browser = cls.playwright.chromium.launch(executable_path=shutil.which("google-chrome"))

    @classmethod
    def tearDownClass(cls):
        cls.browser.close()
        cls.playwright.stop()

    def exercise(self, header=False, keyboard=False):
        page = self.browser.new_page(viewport={"width": 1280, "height": 720})
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        try:
            page.route("**/*", lambda route: route.fulfill(content_type="text/html", body='<!doctype html><div id="root"></div>'))
            page.goto("http://language.test/" + ("?header=1" if header else ""))
            page.evaluate("localStorage.setItem('gpuworks.locale', 'zh-CN')")
            page.add_style_tag(content=self.css)
            page.add_script_tag(content=self.bundle)
            for label, option, language in [("切换语言", "English", "en"), ("Switch language", "简体中文", "zh")]:
                trigger = page.get_by_role("button", name=label, exact=True)
                if keyboard:
                    trigger.focus()
                    trigger.press("Enter")
                else:
                    trigger.hover()
                    # Deliberately hold the pointer through Ant Design's
                    # hover-enter delay and animation before opening. This
                    # reproduces the user gesture, independent of tooltip type.
                    page.wait_for_timeout(400)
                    trigger.click()
                item = page.get_by_role("menuitemradio", name=option, exact=True)
                expect(item).to_be_visible()
                if directory := os.environ.get("LANGUAGE_SCREENSHOTS"):
                    Path(directory).mkdir(parents=True, exist_ok=True)
                    page.screenshot(path=str(Path(directory) / f"{'header' if header else 'login'}-{'keyboard' if keyboard else 'pointer'}-{language}.png"))
                if keyboard:
                    # Opening from the keyboard moves focus straight onto the
                    # first item (Zeron/Base UI menu button; antd needed a Tab).
                    expect(page.get_by_role("menuitemradio", name="English", exact=True)).to_be_focused()
                    if option == "简体中文":
                        page.keyboard.press("ArrowDown")
                    expect(item).to_be_focused()
                    page.keyboard.press("Enter")
                else:
                    self.assertTrue(item.evaluate("el => { const r = el.getBoundingClientRect(); return el.contains(document.elementFromPoint(r.x+r.width/2, r.y+r.height/2)); }"), "language menu item is covered by another popup")
                    item.click()
                expect(page.get_by_test_id("translated")).to_have_text("Switch language" if language == "en" else "切换语言")
                self.assertEqual(page.evaluate("localStorage.getItem('gpuworks.locale')"), language)
                expect(page.get_by_role("menu")).not_to_be_visible()
            self.assertEqual(errors, [])
        finally:
            page.close()

    def test_login_pointer(self):
        self.exercise()

    def test_header_pointer(self):
        self.exercise(header=True)

    def test_login_keyboard(self):
        self.exercise(keyboard=True)

    def test_header_keyboard(self):
        self.exercise(header=True, keyboard=True)


if __name__ == "__main__":
    unittest.main()
