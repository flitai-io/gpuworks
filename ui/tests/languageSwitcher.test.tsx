import React from "react";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import en from "../src/i18n/locales/en/common.json";

const { cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: LanguageSwitcher } = await import("../src/components/LanguageSwitcher.tsx");

afterEach(cleanup);

for (const light of [false, true]) {
  test(`language menu switches both directions with translated button labels (light=${light})`, async () => {
    const { i18n, locale } = await createI18n([], "zh");
    i18n.addResourceBundle("en", "common", en);
    const user = userEvent.setup();
    render(<I18nextProvider i18n={i18n}><LanguageSwitcher light={light} /></I18nextProvider>);
    await user.click(screen.getByRole("button", { name: locale.common.language.switch }));
    await user.click(await screen.findByRole("menuitemradio", { name: "English" }));
    const english = await screen.findByRole("button", { name: en.language.switch });
    assert.equal(english.textContent, "EN");
    assert.equal(i18n.resolvedLanguage, "en");
    await user.click(english);
    await user.click(await screen.findByRole("menuitemradio", { name: "简体中文" }));
    const chinese = await screen.findByRole("button", { name: locale.common.language.switch });
    assert.equal(chinese.textContent, "中文");
    assert.equal(i18n.resolvedLanguage, "zh");
  });
}
